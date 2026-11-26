package policy

import (
	"strings"
	"testing"
	"time"
)

func TestParseRule(t *testing.T) {
	for s, want := range map[string]Rule{
		"left-pad":                          {Name: "left-pad"},
		"left-pad@1.3.*":                    {Name: "left-pad", Version: "1.3.*"},
		"@types/*":                          {Name: "@types/*"},
		"@types/node@20.*":                  {Name: "@types/node", Version: "20.*"},
		"com.acme:*@1.0":                    {Name: "com.acme:*", Version: "1.0"},
		"sha256:" + strings.Repeat("a", 64): {Digest: "sha256:" + strings.Repeat("a", 64)},
	} {
		got, err := ParseRule(s)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "@", "x@", "sha256:abc", "sha256:" + strings.Repeat("A", 64), "*a*a*a*a*a*"} {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("%q taken", s)
		}
	}
}

func TestVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	p := New(7*24*time.Hour, []string{"trusted", "lib@2.*"}, []string{"evil@1.0.*", "bad", "sha256:" + strings.Repeat("b", 64)})
	young, old := now.Add(-time.Hour), now.Add(-30*24*time.Hour)
	for _, c := range []struct {
		name, version string
		at            time.Time
		ok, blocked   bool
	}{
		{"left-pad", "1.0.0", old, true, false},
		{"left-pad", "1.1.0", young, false, false},
		{"left-pad", "1.1.0", now.Add(-7 * 24 * time.Hour), true, false}, // exactly the cooldown old: served
		{"left-pad", "1.1.0", time.Time{}, false, false},                 // no time: held back
		{"trusted", "9.9.9", young, true, false},                         // the allow list lifts the cooldown
		{"lib", "2.1.0", young, true, false},
		{"lib", "3.0.0", young, false, false},
		{"evil", "1.0.3", old, false, true},
		{"evil", "1.1.0", old, true, false},
		{"bad", "0.0.1", old, false, true},
		{"trusted", "1.0.0", time.Time{}, true, false},
	} {
		v := p.Version(c.name, c.version, c.at, now)
		if v.OK != c.ok || v.Blocked != c.blocked {
			t.Errorf("%s@%s %v: %+v", c.name, c.version, c.at, v)
		}
		if !v.OK && v.Why == "" {
			t.Errorf("%s@%s: refused with no words", c.name, c.version)
		}
	}
	if v := p.Version("left-pad", "1.1.0", young, now); !v.Until.Equal(young.Add(7*24*time.Hour)) || !strings.Contains(v.Why, "will be served from") {
		t.Fatalf("until: %+v", v)
	}
	// An allow list does not lift a block.
	q := New(time.Hour, []string{"x"}, []string{"x@1.0"})
	if v := q.Version("x", "1.0", old, now); v.OK {
		t.Fatal("an allow lifted a block")
	}
	if !p.BlockedDigest("sha256:"+strings.Repeat("b", 64)) || p.BlockedDigest("sha256:"+strings.Repeat("c", 64)) {
		t.Fatal("digests")
	}
	var nilP *Policy
	if !nilP.Version("a", "1", time.Time{}, now).OK || nilP.Active() || nilP.BlockedDigest("x") {
		t.Fatal("the nil policy holds back nothing")
	}
}
