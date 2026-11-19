package server_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/meta"
)

func TestAPIAccountsAndTokens(t *testing.T) {
	r := newRig(t)
	a := r.admin
	// who am I
	_, b := r.want(200, "GET", "/api/v1/whoami", a, "")
	var who struct {
		Account string
		Admin   bool
	}
	jsonOf(t, b, &who)
	if who.Account != "root" || !who.Admin {
		t.Fatalf("%s", b)
	}
	r.want(401, "GET", "/api/v1/whoami", "", "")

	// accounts: made, listed, refused twice, refused with a name a shell would eat
	r.want(201, "POST", "/api/v1/accounts", a, `{"name":"deploy","kind":"robot"}`)
	r.want(409, "POST", "/api/v1/accounts", a, `{"name":"deploy","kind":"robot"}`)
	r.want(400, "POST", "/api/v1/accounts", a, `{"name":"deploy$1","kind":"robot"}`)
	r.want(400, "POST", "/api/v1/accounts", a, `{"name":"x","colour":"red"}`)
	_, b = r.want(200, "GET", "/api/v1/accounts", a, "")
	if !strings.Contains(string(b), `"deploy"`) || !strings.Contains(string(b), `"ci-build"`) {
		t.Fatalf("%s", b)
	}

	// a token for another account: the secret is shown once and is not in a listing
	_, b = r.want(201, "POST", "/api/v1/tokens", a, `{"account":"deploy","scope":[{"repositories":["files"],"actions":["read"]}],"life_seconds":3600,"note":"first"}`)
	var made struct {
		Token string
		Info  struct{ ID string }
	}
	jsonOf(t, b, &made)
	if !strings.HasPrefix(made.Token, "gd_") {
		t.Fatalf("%s", b)
	}
	_, b = r.want(200, "GET", "/api/v1/tokens?account=deploy", a, "")
	if strings.Contains(string(b), strings.SplitN(made.Token, "_", 3)[2]) || !strings.Contains(string(b), made.Info.ID) {
		t.Fatalf("a listing must name the token and never show it: %s", b)
	}
	// the new token works within its scope: the account has no grant, so it can read nothing
	r.want(403, "GET", "/repository/files/releases/x", made.Token, "")

	// rules: a life is required and has a longest, a scope is required
	r.want(400, "POST", "/api/v1/tokens", a, `{"account":"deploy","scope":[{"repositories":["*"],"actions":["read"]}]}`)
	r.want(400, "POST", "/api/v1/tokens", a, `{"account":"deploy","scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":999999999}`)
	r.want(400, "POST", "/api/v1/tokens", a, `{"account":"deploy","life_seconds":60}`)
	r.want(404, "POST", "/api/v1/tokens", a, `{"account":"nobody","scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":60}`)

	// a token without admin scope cannot make tokens; a non-admin account with an admin-scope token can make its own, and only its own
	r.want(403, "POST", "/api/v1/tokens", made.Token, `{"scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":60}`)
	self := r.token("ci-build", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Admin}})
	r.want(201, "POST", "/api/v1/tokens", self, `{"scope":[{"repositories":["files"],"actions":["write"]}],"life_seconds":60}`)
	r.want(403, "POST", "/api/v1/tokens", self, `{"account":"deploy","scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":60}`)
	r.want(403, "GET", "/api/v1/accounts", self, "")
	r.want(403, "GET", "/api/v1/audit", self, "")
	r.want(403, "GET", "/api/v1/repositories", self, "")
	_, b = r.want(200, "GET", "/api/v1/tokens", self, "")
	if strings.Contains(string(b), made.Info.ID) {
		t.Fatalf("a token sees the tokens of its own account only: %s", b)
	}
	r.want(403, "GET", "/api/v1/tokens?account=deploy", self, "")
	r.want(403, "DELETE", "/api/v1/tokens/"+made.Info.ID, self, "")

	// revoke: the owner and an admin may; the token is dead at once
	r.want(204, "DELETE", "/api/v1/tokens/"+made.Info.ID, a, "")
	r.want(404, "DELETE", "/api/v1/tokens/"+made.Info.ID, a, "")
	r.want(401, "GET", "/api/v1/whoami", made.Token, "")

	// disable and enable an account
	r.want(204, "POST", "/api/v1/accounts/ci-build/disable", a, "")
	r.want(401, "GET", "/api/v1/whoami", self, "")
	r.want(204, "POST", "/api/v1/accounts/ci-build/enable", a, "")
	r.want(200, "GET", "/api/v1/whoami", self, "")
	r.want(404, "POST", "/api/v1/accounts/nobody/disable", a, "")

	// the settings can be read by API
	_, b = r.want(200, "GET", "/api/v1/repositories", a, "")
	if !strings.Contains(string(b), `"files"`) || !strings.Contains(string(b), `"ci-build"`) {
		t.Fatalf("%s", b)
	}
	r.want(404, "GET", "/api/v1/nothing", a, "")

	// everything above that changed something, or was refused, is in the audit log, and the log can be read by API
	for _, action := range []string{"account.create", "token.create", "token.revoke", "account.disable", "account.enable"} {
		if len(r.auditRows(action)) == 0 {
			t.Errorf("no audit line for %s", action)
		}
	}
	_, b = r.want(200, "GET", "/api/v1/audit?action=token.create&limit=1", a, "")
	if !strings.Contains(string(b), `"token.create"`) || !strings.Contains(string(b), `"next"`) {
		t.Fatalf("%s", b)
	}
	for _, row := range r.auditRows("token.create") {
		if strings.Contains(row.Detail, strings.SplitN(made.Token, "_", 3)[2]) {
			t.Fatal("the secret is in the audit log")
		}
	}
	// a token's disabling of its own creator does not matter here; the audit rows are in order
	rows := r.auditRows("")
	for i := 1; i < len(rows); i++ {
		if rows[i].ID >= rows[i-1].ID {
			t.Fatal("the audit rows are not newest first")
		}
	}
	_ = audit.OK
}

// T32: a secret is in the Authorization header only; it is in no log line and no audit row, whatever way the client sent it.
func TestTrapT32NoSecretInAnyLog(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.auth.CreateAccount(ctx, "robot", "robot", false); err != nil {
		t.Fatal(err)
	}
	good := r.token("robot", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write}})
	goodSecret := strings.SplitN(good, "_", 3)[2]
	wrong := "gd_" + strings.SplitN(good, "_", 3)[1] + "_" + strings.Repeat("Z", 43) // the id of a real token with a wrong secret
	wrongSecret := strings.Repeat("Z", 43)
	adminSecret := strings.SplitN(r.admin, "_", 3)[2]

	// the secret as a bearer token, as the password of basic authentication, as a query parameter and as the user name of an address
	r.want(201, "PUT", "/repository/public/a", r.admin, "x")
	r.want(403, "PUT", "/repository/public/b", good, "x")
	r.want(401, "GET", "/repository/files/x", wrong, "")
	r.want(401, "GET", "/repository/files/x?token="+goodSecret+"&access_token="+good, "", "")
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/files/x", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("robot:"+wrong)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("GET", "http://robot:"+good+"@"+strings.TrimPrefix(r.ts.URL, "http://")+"/repository/files/x", nil)
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	r.want(201, "POST", "/api/v1/tokens", r.admin, `{"account":"robot","scope":[{"repositories":["*"],"actions":["read"]}],"life_seconds":60}`)

	logs := r.log.String()
	if len(logs) < 200 {
		t.Fatalf("the log is nearly empty, so this test would prove nothing: %q", logs)
	}
	rows, err := r.db.Query(ctx, meta.S(`SELECT actor || token || remote || action || repo || path || result || detail FROM audit`))
	if err != nil || len(rows) < 5 {
		t.Fatalf("%v %v", rows, err)
	}
	var audit strings.Builder
	for _, row := range rows {
		audit.WriteString(meta.Str(row[0]) + "\n")
	}
	for name, secret := range map[string]string{"the secret of a good token": goodSecret, "the secret of a wrong token": wrongSecret, "the secret of the admin token": adminSecret, "a whole token": good} {
		if strings.Contains(logs, secret) {
			t.Errorf("%s is in the log", name)
		}
		if strings.Contains(audit.String(), secret) {
			t.Errorf("%s is in the audit log", name)
		}
	}
	// what the log has instead is the id of the token
	id := strings.SplitN(good, "_", 3)[1]
	if !strings.Contains(logs, id) {
		t.Error("the id of the token is not in the log: the audit line cannot say whose request it was")
	}
}
