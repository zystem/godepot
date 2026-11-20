package gomod

import "testing"

func TestParsePaths(t *testing.T) {
	for path, want := range map[string]target{
		"github.com/a/b/@v/list":                   {"github.com/a/b", kList, ""},
		"github.com/a/b/@latest":                   {"github.com/a/b", kLatest, ""},
		"github.com/!azure/x/@v/v1.2.3.info":       {"github.com/!azure/x", kInfo, "v1.2.3"},
		"example.com/m/@v/v0.0.0-2020.mod":         {"example.com/m", kMod, "v0.0.0-2020"},
		"example.com/m/v2/@v/v2.0.0.zip":           {"example.com/m/v2", kZip, "v2.0.0"},
		"example.com/m/@v/v2.0.0+incompatible.zip": {"example.com/m", kZip, "v2.0.0+incompatible"},
	} {
		got, ok := parse(path)
		if !ok || got != want {
			t.Errorf("parse(%q) = %+v %v, want %+v", path, got, ok, want)
		}
	}
	for _, path := range []string{
		"", "list", "a/@v/", "a/@v/v1.0.0", "a/@v/v1.0.0.txt", "a/@v/1.0.0.zip", "a/@v/v1.0.zip", "a/@v/v01.0.0.zip", "a/@v/V1.0.0.zip",
		"A/b/@v/list", "a/../b/@v/list", "a/./b/@latest", "a//b/@latest", "-a/@latest", "a!/@latest", "a!1/@latest", "a b/@latest", "@v/list", "/@latest",
		"a/@v/v1.0.0-x/../y.zip", "a/@v/list/x",
	} {
		if got, ok := parse(path); ok {
			t.Errorf("parse(%q) accepted: %+v", path, got)
		}
	}
}

func TestEscape(t *testing.T) {
	for in, out := range map[string]string{"github.com/Azure/Go": "github.com/!azure/!go", "a/b": "a/b"} {
		if escape(in) != out {
			t.Errorf("escape(%q) = %q", in, escape(in))
		}
		if back, ok := unescape(out); !ok || back != in {
			t.Errorf("unescape(%q) = %q %v", out, back, ok)
		}
	}
	for _, bad := range []string{"A", "a!", "!A", "a!!b", "!1"} {
		if _, ok := unescape(bad); ok {
			t.Errorf("unescape(%q) accepted", bad)
		}
	}
}

func TestSemverOrder(t *testing.T) {
	order := []string{"v0.0.1", "v0.1.0", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.10.0", "v2.0.0"}
	for i := range order {
		for j := range order {
			a, ok1 := parseSemver(order[i])
			b, ok2 := parseSemver(order[j])
			if !ok1 || !ok2 {
				t.Fatal("not parsed", order[i], order[j])
			}
			if a.less(b) != (i < j) {
				t.Errorf("%s < %s is %v", order[i], order[j], a.less(b))
			}
		}
	}
}

func TestModulePathAndMajor(t *testing.T) {
	for in, want := range map[string]string{
		"module example.com/m\n":                         "example.com/m",
		"// c\nmodule \"example.com/q\" // x\ngo 1.21\n": "example.com/q",
		"modulex a\n":               "",
		"go 1.20\n":                 "",
		"module\texample.com/t\r\n": "example.com/t",
	} {
		if got := modulePath([]byte(in)); got != want {
			t.Errorf("modulePath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		mod, ver string
		ok       bool
	}{{"a/b", "v1.2.3", true}, {"a/b", "v0.1.0", true}, {"a/b/v2", "v2.0.1", true}, {"a/b", "v2.0.0", false}, {"a/b/v2", "v1.0.0", false}, {"a/b/v3", "v2.0.0", false},
		{"a/b", "v2.0.0+incompatible", true}, {"a/b/v2", "v2.0.0+incompatible", false}} {
		if majorOK(c.mod, c.ver) != c.ok {
			t.Errorf("majorOK(%q, %q) = %v", c.mod, c.ver, !c.ok)
		}
	}
}
