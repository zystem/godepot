package auth

import "testing"

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "files", true},
		{"*", "a/b", false},
		{"**", "a/b/c", true},
		{"**", "", true},
		{"releases/**", "releases/1.0/app.tgz", true},
		{"releases/**", "releases/", true},
		{"releases/**", "other/x", false},
		{"releases/*", "releases/x", true},
		{"releases/*", "releases/x/y", false},
		{"*.tgz", "app.tgz", true},
		{"*.tgz", "dir/app.tgz", false},
		{"**.tgz", "dir/app.tgz", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abd", false},
		{"", "", true},
		{"", "x", false},
		{"x", "", false},
	} {
		if got := Match(c.pattern, c.s); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestGrant(t *testing.T) {
	g := Grant{Repositories: []string{"files"}, Paths: []string{"releases/**"}, Actions: []string{Read, Write}}
	if err := g.Check(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		repo, path, action string
		want               bool
	}{
		{"files", "releases/a", Read, true},
		{"files", "releases/a", Write, true},
		{"files", "releases/a", Delete, false},
		{"files", "other/a", Read, false},
		{"docs", "releases/a", Read, false},
	} {
		if got := g.Allows(c.repo, c.path, c.action); got != c.want {
			t.Errorf("Allows(%q, %q, %q) = %v, want %v", c.repo, c.path, c.action, got, c.want)
		}
	}
	// admin on a place is every action there; no paths is every path
	a := Grant{Repositories: []string{"*"}, Actions: []string{Admin}}
	if !a.Allows("anything", "any/path", Delete) {
		t.Error("admin on * allows delete")
	}
	for _, bad := range []Grant{
		{Actions: []string{Read}},
		{Repositories: []string{"x"}},
		{Repositories: []string{"x"}, Actions: []string{"fly"}},
		{Repositories: []string{""}, Actions: []string{Read}},
		{Repositories: []string{"x"}, Paths: []string{"*a*a*a*a*a*"}, Actions: []string{Read}},
		{Repositories: []string{"x"}, Paths: []string{string(make([]byte, 300))}, Actions: []string{Read}},
	} {
		if bad.Check() == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"ci-build", "a", "robot.1", "x_y"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "robot$ci", "Ci", " a", "a b", "-a", "a/b", string(make([]byte, 70))} {
		if CheckName(bad) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func FuzzMatch(f *testing.F) {
	f.Add("releases/**", "releases/a/b")
	f.Add("*.tgz", "a.tgz")
	f.Fuzz(func(t *testing.T, pattern, s string) {
		if len(pattern) > 40 || len(s) > 80 {
			return // the matcher is exponential in the worst case on long inputs; patterns are short
		}
		_ = Match(pattern, s)
		if !Match("**", s) {
			t.Fatal("** matches everything")
		}
		if !Match(s, s) && !containsStar(s) {
			t.Fatalf("a pattern without wildcards matches itself: %q", s)
		}
	})
}

func containsStar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			return true
		}
	}
	return false
}
