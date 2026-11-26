package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/testutil"
)

// trusting replaces the settings of the rig by ones that trust the issuer.
func (r *rig) trusting(i *testutil.Issuer) {
	r.t.Helper()
	text := strings.TrimSuffix(strings.TrimSpace(settingsText), "}") + fmt.Sprintf(`,
 "issuers": [{"name": "ci", "url": %q, "audience": "godepot", "allow_private": true}],
 "trusts": [
  {"issuer": "ci", "account": "ci-build", "claims": {"repository": "corp/app", "ref": "refs/heads/**"}, "repositories": ["files"], "paths": ["releases/app/**"]},
  {"issuer": "ci", "account": "second", "claims": {"repository": "corp/app", "run_number": "7", "protected": "true"}},
  {"issuer": "ci", "account": "reader", "claims": {"repository": "corp/user"}},
  {"issuer": "ci", "account": "root", "claims": {"repository": "corp/root"}},
  {"issuer": "ci", "account": "boss", "claims": {"repository": "corp/boss"}},
  {"issuer": "ci", "account": "ghost", "claims": {"repository": "corp/ghost"}}
 ]}`, i.URL)
	set, err := settings.Parse(strings.NewReader(text))
	if err != nil {
		r.t.Fatal(err)
	}
	r.srv.Settings.Store(set)
	for _, a := range []struct {
		name  string
		admin bool
	}{{"second", false}, {"boss", true}} {
		if err := r.auth.CreateAccount(r.t.Context(), a.name, "robot", a.admin); err != nil {
			r.t.Fatal(err)
		}
	}
}

func body(tok, account string) string {
	b, _ := json.Marshal(map[string]string{"token": tok, "account": account})
	return string(b)
}

func TestACIJobExchangesItsToken(t *testing.T) {
	r := newRig(t)
	i := testutil.NewIssuer(t)
	r.trusting(i)
	job := i.Sign(t, "RS256", i.Claims(nil), nil)
	h, b := r.want(201, "POST", "/api/v1/oidc/token", "", body(job, ""))
	var got struct {
		Token     string
		Account   string
		ExpiresAt int64 `json:"expires_at"`
	}
	jsonOf(t, b, &got)
	left := got.ExpiresAt - r.clock.Load()
	if got.Account != "ci-build" || !strings.HasPrefix(got.Token, "gd_") || left < 14*60 || left > 15*60 || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %v", b, h)
	}
	// the token is narrowed by the trust (releases/app/**) and by the grants of the account (releases/**)
	r.want(201, "PUT", "/repository/files/releases/app/1.0", got.Token, "bytes")
	r.want(200, "GET", "/repository/files/releases/app/1.0", got.Token, "")
	r.want(403, "PUT", "/repository/files/releases/other/1.0", got.Token, "bytes")
	r.want(403, "DELETE", "/repository/files/releases/app/1.0", got.Token, "")
	r.want(403, "GET", "/api/v1/audit", got.Token, "")
	r.want(403, "POST", "/api/v1/tokens", got.Token, `{"scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":60}`)
	// it ends after fifteen minutes
	r.clock.Add(15*60 + 1)
	r.want(401, "GET", "/repository/files/releases/app/1.0", got.Token, "")
	r.clock.Add(-(15*60 + 1))

	// the second trust: chosen by name, and its conditions are a number and a boolean
	second := i.Sign(t, "ES256", i.Claims(map[string]any{"run_number": 7, "protected": true}), nil)
	_, b = r.want(201, "POST", "/api/v1/oidc/token", "", body(second, "second"))
	if !strings.Contains(string(b), `"second"`) {
		t.Fatalf("%s", b)
	}
	_, b = r.want(201, "POST", "/api/v1/oidc/token", "", body(second, ""))
	if !strings.Contains(string(b), `"ci-build"`) {
		t.Fatalf("the first trust that fits: %s", b)
	}
	r.want(403, "POST", "/api/v1/oidc/token", "", body(job, "second"))

	// no trust fits: another repository, a ref outside the pattern, a claim that is missing, one that is a list
	for _, more := range []map[string]any{
		{"repository": "evil/app"}, {"ref": "refs/tags/v1"}, {"ref": nil}, {"repository": []string{"corp/app"}},
		// an account that is a user, an admin user, an admin robot, one that does not exist
		{"repository": "corp/user"}, {"repository": "corp/root"}, {"repository": "corp/boss"}, {"repository": "corp/ghost"},
	} {
		_, b := r.want(403, "POST", "/api/v1/oidc/token", "", body(i.Sign(t, "RS256", i.Claims(more), nil), ""))
		if strings.Contains(string(b), `"token":`) {
			t.Fatalf("%v: %s", more, b)
		}
	}
	// a disabled robot
	r.want(204, "POST", "/api/v1/accounts/ci-build/disable", r.admin, "")
	r.want(403, "POST", "/api/v1/oidc/token", "", body(job, ""))
	r.want(401, "GET", "/repository/files/releases/app/1.0", got.Token, "")
	r.want(204, "POST", "/api/v1/accounts/ci-build/enable", r.admin, "")

	// tokens that are not accepted: the same words whatever is wrong
	other := testutil.NewIssuer(t)
	var said string
	for _, bad := range []string{
		other.Sign(t, "RS256", other.Claims(nil), nil),                                    // an issuer that is not trusted
		other.Sign(t, "RS256", i.Claims(nil), nil),                                        // another's key
		i.Sign(t, "RS256", i.Claims(map[string]any{"aud": "else"}), nil),                  // for somebody else
		i.Sign(t, "RS256", i.Claims(map[string]any{"exp": time.Now().Unix() - 600}), nil), // old
		"rubbish", "",
	} {
		_, b := r.want(401, "POST", "/api/v1/oidc/token", "", body(bad, ""))
		if said != "" && said != string(b) {
			t.Fatalf("the words differ: %s / %s", said, b)
		}
		said = string(b)
	}
	if other.Reads.Load() != 0 {
		t.Fatal("the keys of an issuer that is not trusted were read")
	}
	r.want(405, "GET", "/api/v1/oidc/token", "", "")
	r.want(400, "POST", "/api/v1/oidc/token", "", `{"jwt": "x"}`)

	// the audit log says who and why, and holds no job token and no secret
	rows := r.auditRows("oidc.exchange")
	var all strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&all, "%s %s %s\n", row.Actor, row.Result, row.Detail)
	}
	for _, want := range []string{"sub repo:corp/app:ref:refs/heads/main", "no trust fits", "is not an enabled robot", "ghost does not exist", "no issuer of the settings", "it has expired"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("no %q in the audit log:\n%s", want, all.String())
		}
	}
	for _, secret := range []string{job, strings.Split(job, ".")[2], strings.SplitN(got.Token, "_", 3)[2]} {
		if strings.Contains(all.String(), secret) || strings.Contains(r.log.String(), secret) {
			t.Fatal("a secret is in a log")
		}
	}
}

func TestExchangeWhenTheIssuerIsDownAndWhenFlooded(t *testing.T) {
	r := newRig(t)
	i := testutil.NewIssuer(t)
	r.trusting(i)
	job := i.Sign(t, "RS256", i.Claims(nil), nil)
	i.Down.Store(true)
	h, _ := r.want(503, "POST", "/api/v1/oidc/token", "", body(job, ""))
	if h.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	for range 30 {
		r.want(401, "POST", "/api/v1/oidc/token", "", body("rubbish", ""))
	}
	h, _ = r.want(429, "POST", "/api/v1/oidc/token", "", body("rubbish", ""))
	if h.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}
