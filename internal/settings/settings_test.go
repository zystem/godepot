package settings_test

import (
	"strings"
	"testing"

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
