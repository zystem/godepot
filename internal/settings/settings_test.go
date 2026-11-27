package settings_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/settings"
)

const good = `{
 "repositories": [
  {"name": "files", "format": "raw", "kind": "hosted", "overwrite_paths": ["latest/**"], "quota_bytes": 100},
  {"name": "public", "format": "raw", "kind": "hosted", "anonymous_read": true}
 ],
 "grants": [
  {"account": "ci-build", "repositories": ["files"], "paths": ["releases/**"], "actions": ["read", "write"]},
  {"account": "reader", "repositories": ["*"], "actions": ["read"]}
 ]
}`

func TestParse(t *testing.T) {
	s, err := settings.Parse(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	files := s.Repo("files")
	if files == nil || files.QuotaBytes != 100 || !files.Overwrites("latest/x") || files.Overwrites("releases/x") {
		t.Fatalf("%+v", files)
	}
	if s.Repo("nothing") != nil {
		t.Fatal("a repository that is not there")
	}
}

func TestParseRefuses(t *testing.T) {
	for name, text := range map[string]string{
		"an unknown word":     `{"repositories": [], "colour": 1}`,
		"not json":            `repositories:`,
		"two values":          `{} {}`,
		"an unknown format":   `{"repositories": [{"name": "a", "format": "wasm", "kind": "hosted"}]}`,
		"proxy not built":     `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy"}]}`,
		"no kind":             `{"repositories": [{"name": "a", "format": "raw"}]}`,
		"a bad name":          `{"repositories": [{"name": "A$", "format": "raw", "kind": "hosted"}]}`,
		"a name twice":        `{"repositories": [{"name": "a", "format": "raw", "kind": "hosted"}, {"name": "a", "format": "raw", "kind": "hosted"}]}`,
		"a negative quota":    `{"repositories": [{"name": "a", "format": "raw", "kind": "hosted", "quota_bytes": -1}]}`,
		"a grant with no act": `{"grants": [{"account": "a", "repositories": ["x"]}]}`,
		"a grant, bad action": `{"grants": [{"account": "a", "repositories": ["x"], "actions": ["fly"]}]}`,
		"a grant, bad name":   `{"grants": [{"account": "A B", "repositories": ["x"], "actions": ["read"]}]}`,
		"a pattern too wild":  `{"repositories": [{"name": "a", "format": "raw", "kind": "hosted", "overwrite_paths": ["*a*a*a*a*a*"]}]}`,
	} {
		if _, err := settings.Parse(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// every problem at once
	_, err := settings.Parse(strings.NewReader(`{"repositories": [{"name": "A", "format": "x", "kind": "y"}]}`))
	if err == nil || strings.Count(err.Error(), "\n") < 2 {
		t.Fatalf("all the problems should be named at once: %v", err)
	}
}

func TestAllows(t *testing.T) {
	s, _ := settings.Parse(strings.NewReader(good))
	scope := func(actions ...string) []auth.Grant {
		return []auth.Grant{{Repositories: []string{"*"}, Actions: actions}}
	}
	ci := &auth.Principal{Account: "ci-build", Token: "t1", Scope: scope(auth.Read, auth.Write, auth.Delete)}
	for _, c := range []struct {
		p          *auth.Principal
		repo, path string
		action     string
		want       bool
		why        string
	}{
		{ci, "files", "releases/1", auth.Write, true, "the account has a grant"},
		{ci, "files", "releases/1", auth.Delete, false, "the token allows delete, the account does not"},
		{ci, "files", "other/1", auth.Write, false, "outside the paths of the grant"},
		{ci, "public", "x", auth.Write, false, "no grant in this repository"},
		{ci, "nothing", "x", auth.Read, false, "no such repository"},
		{&auth.Principal{Account: "ci-build", Token: "t2", Scope: scope(auth.Read)}, "files", "releases/1", auth.Write, false, "the account allows write, the token does not"},
		{&auth.Principal{Account: "reader", Token: "t3", Scope: scope(auth.Read)}, "files", "any/path", auth.Read, true, "read everywhere"},
		{&auth.Principal{Account: "root", Admin: true, Token: "t4", Scope: scope(auth.Delete)}, "files", "a", auth.Delete, true, "an admin account, the token allows"},
		{&auth.Principal{Account: "root", Admin: true, Token: "t5", Scope: scope(auth.Read)}, "files", "a", auth.Delete, false, "an admin account, the token does not"},
		{&auth.Principal{}, "public", "x", auth.Read, true, "anonymous read is set"},
		{&auth.Principal{}, "public", "x", auth.Write, false, "anonymous never writes"},
		{&auth.Principal{}, "files", "x", auth.Read, false, "anonymous read is off by default"},
		{nil, "public", "x", auth.Read, true, "a nil principal is anonymous"},
		{&auth.Principal{Account: "stranger", Token: "t6", Scope: scope(auth.Read)}, "public", "x", auth.Read, true, "a token with no grant reads what anyone reads"},
	} {
		if got := s.Allows(c.p, c.repo, c.path, c.action); got != c.want {
			t.Errorf("%s: got %v want %v", c.why, got, c.want)
		}
	}
}

// The format oci has immutable tags, and none of the settings that only a file path has; the other formats have no immutable tags.
func TestOCISettings(t *testing.T) {
	ok := `{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted", "immutable_tags": ["v*", "release-*"]}]}`
	s, err := settings.Parse(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	r := s.Repo("reg")
	if !r.ImmutableTag("v1") || !r.ImmutableTag("release-2") || r.ImmutableTag("latest") || r.ImmutableTag("dev/v1") {
		t.Fatal("ImmutableTag does not follow the patterns")
	}
	for text, word := range map[string]string{
		`{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted", "overwrite_paths": ["a"]}]}`:         "overwrite_paths is for the format raw",
		`{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted", "quota_bytes": 5}]}`:                 "quota is not built for the format oci",
		`{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted", "immutable_tags": ["*a*a*a*a*a"]}]}`: "immutable_tags",
		`{"repositories": [{"name": "f", "format": "raw", "kind": "hosted", "immutable_tags": ["v*"]}]}`:           "immutable_tags is for the format oci",
	} {
		if _, err := settings.Parse(strings.NewReader(text)); err == nil || !strings.Contains(err.Error(), word) {
			t.Errorf("%s: %v, want a problem with %q", text, err, word)
		}
	}
}

func TestCleanupSettings(t *testing.T) {
	ok := `{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"enforce": true, "remove_older_days": 30, "keep_newest": 3, "keep_read_days": 7, "keep": ["org/keep/**"], "keep_props": {"pin": "yes"}}},
		{"name": "p", "format": "npm", "kind": "proxy", "upstream": {"url": "https://registry.npmjs.org"}, "cleanup": {"remove_older_days": 0}}]}`
	s, err := settings.Parse(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Repo("m").Cleanup; c == nil || !c.Enforce || *c.RemoveOlderDays != 30 || c.KeepNewest != 3 || c.KeepProps["pin"] != "yes" {
		t.Fatalf("%+v", c)
	}
	if c := s.Repo("p").Cleanup; c == nil || c.RemoveOlderDays == nil || *c.RemoveOlderDays != 0 {
		t.Fatal("remove_older_days 0 is a value, not none")
	}
	for text, word := range map[string]string{
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"enforce": true}}]}`:                                                                             "nothing would ever be removed",
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"remove_older_days": -1}}]}`:                                                                     "negative",
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"keep_newest": -1}}]}`:                                                                           "negative",
		`{"repositories": [{"name": "o", "format": "oci", "kind": "hosted", "cleanup": {"remove_older_days": 1}}]}`:                                                                        "not built for the format oci",
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted"}, {"name": "g", "format": "maven", "kind": "group", "members": ["m"], "cleanup": {"remove_older_days": 1}}]}`: "not a group",
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"remove_older_days": 1, "keep": ["*a*a*a*a*a*"]}}]}`:                                             "keep",
		`{"repositories": [{"name": "m", "format": "maven", "kind": "hosted", "cleanup": {"colour": 1}}]}`:                                                                                 "colour",
	} {
		if _, err := settings.Parse(strings.NewReader(text)); err == nil || !strings.Contains(err.Error(), word) {
			t.Errorf("%s: %v, want a problem with %q", text, err, word)
		}
	}
}

func TestIssuersAndTrusts(t *testing.T) {
	ok := `{"repositories": [], "grants": [],
	 "issuers": [{"name": "gh", "url": "https://token.actions.githubusercontent.com", "audience": "godepot"},
	             {"name": "lab", "url": "http://10.0.0.1", "audience": "godepot", "allow_private": true, "jwks_url": "http://10.0.0.1/keys"}],
	 "trusts": [{"issuer": "gh", "account": "ci", "claims": {"repository": "corp/app"}}]}`
	s, err := settings.Parse(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	g := s.Trusts[0].Scope()
	if len(g) != 1 || strings.Join(g[0].Repositories, ",") != "*" || strings.Join(g[0].Actions, ",") != "read,write" {
		t.Fatalf("the default scope: %+v", g)
	}
	if s.IssuerByURL("https://token.actions.githubusercontent.com") == nil || s.IssuerByURL("https://token.actions.githubusercontent.com/") != nil {
		t.Fatal("an issuer is found by its exact address")
	}
	issuer := func(more string) string { return `{"repositories": [], "grants": [], "issuers": [{` + more + `}]}` }
	trust := func(more string) string {
		return `{"repositories": [], "grants": [], "issuers": [{"name": "gh", "url": "https://a.example", "audience": "g"}], "trusts": [{"account": "ci", ` + more + `}]}`
	}
	for text, want := range map[string]string{
		issuer(`"name": "Bad Name", "url": "https://a.example", "audience": "g"`):                                                      "name",
		issuer(`"name": "a", "url": "a.example", "audience": "g"`):                                                                     "must be an address",
		issuer(`"name": "a", "url": "http://a.example", "audience": "g"`):                                                              "not https",
		issuer(`"name": "a", "url": "https://a.example", "audience": "g", "jwks_url": "http://x/k"`):                                   "jwks_url is not https",
		issuer(`"name": "a", "url": "https://u@a.example", "audience": "g"`):                                                           "a user name",
		issuer(`"name": "a", "url": "https://a.example"`):                                                                              "audience is required",
		issuer(`"name": "a", "url": "https://a.example", "audience": "g"}, {"name": "a", "url": "https://a.example", "audience": "g"`): "used twice",
		trust(`"issuer": "no", "claims": {"sub": "x"}`):                                                                                "is not one of the issuers",
		trust(`"issuer": "gh"`): "claims must pin",
		trust(`"issuer": "gh", "claims": {"ref": "refs/heads/main"}`):         "claims must pin",
		trust(`"issuer": "gh", "claims": {"repository": "*/app"}`):            "claims must pin",
		trust(`"issuer": "gh", "claims": {"sub": "**"}`):                      "claims must pin",
		trust(`"issuer": "gh", "claims": {"sub": "x", "ref": ""}`):            "empty pattern",
		trust(`"issuer": "gh", "claims": {"sub": "x"}, "actions": ["admin"]`): "never given admin",
		trust(`"issuer": "gh", "claims": {"sub": "x"}, "actions": ["fly"]`):   "unknown",
	} {
		if _, err := settings.Parse(strings.NewReader(text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", text, err, want)
		}
	}
}

func TestTheDirectoryAndItsGroups(t *testing.T) {
	text := func(ldap, grants string) string {
		return `{"repositories": [], "grants": [` + grants + `], "ldap": {` + ldap + `}}`
	}
	good := `"url": "ldaps://dc.test", "bind_dn": "cn=svc", "password_env": "P", "user_base": "dc=test", "user_filter": "(uid={user})"`
	s, err := settings.Parse(strings.NewReader(text(good+`, "group_base": "dc=test"`, `{"group": "Domain Devs", "repositories": ["*"], "actions": ["read"]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if s.LDAP.RefreshEvery() != 5*time.Minute {
		t.Fatal(s.LDAP.RefreshEvery())
	}
	if _, err := settings.Parse(strings.NewReader(text(`"url": "ldap://dc.test", "start_tls": true, "bind_dn": "a", "password_env": "P", "user_base": "b", "user_filter": "{user}"`, ""))); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		text(strings.Replace(good, "ldaps:", "http:", 1), ""):                                                          "must be an address",
		text(strings.Replace(good, "ldaps:", "ldap:", 1), ""):                                                          "never sent in the clear",
		text(good+`, "start_tls": true`, ""):                                                                           "start_tls is for an ldap url",
		text(strings.Replace(good, "dc.test", "u@dc.test", 1), ""):                                                     "a user name",
		text(`"url": "ldaps://dc.test"`, ""):                                                                           "bind_dn is required",
		text(strings.Replace(good, `"P"`, `"a b"`, 1), ""):                                                             "password_env is required",
		text(strings.Replace(good, "{user}", "bob", 1), ""):                                                            "no {user}",
		text(good+`, "group_base": "x", "group_filter": "(cn=all)"`, ""):                                               "neither {dn} nor {user}",
		text(good+`, "group_name": "cn"`, ""):                                                                          "need group_base",
		text(good+`, "refresh": "0s"`, ""):                                                                             "more than 0",
		text(good, `{"group": "g", "repositories": ["*"], "actions": ["read"]}`):                                       "groups need ldap",
		text(good+`, "group_base": "x"`, `{"group": "g", "account": "a", "repositories": ["*"], "actions": ["read"]}`): "not both",
		text(good, `{"repositories": ["*"], "actions": ["read"]}`):                                                     "name",
	} {
		if _, err := settings.Parse(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
