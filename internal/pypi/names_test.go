package pypi

import "testing"

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"Foo_Bar": "foo-bar", "foo.bar": "foo-bar", "FOO--bar__baz.qux": "foo-bar-baz-qux", "zope.interface": "zope-interface", "a": "a"} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFilename(t *testing.T) {
	for in, want := range map[string]file{
		"requests-2.31.0-py3-none-any.whl":               {"requests", "2.31.0", true},
		"My_Pkg-1.0-1-cp311-cp311-manylinux1_x86_64.whl": {"my-pkg", "1.0", true},
		"my-pkg-1.0.tar.gz":                              {"my-pkg", "1.0", false},
		"Zope.Interface-6.0.zip":                         {"zope-interface", "6.0", false},
		"a-1!2.0+local.tar.gz":                           {"a", "1!2.0+local", false},
	} {
		got, ok := parseFilename(in)
		if !ok || got != want {
			t.Errorf("parseFilename(%q) = %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x.whl", "a-1-py3.whl", "a-1.txt", "-1.0.tar.gz", "a-.tar.gz", "../a-1.0.tar.gz", "a/b-1.0.tar.gz", "a b-1.0.tar.gz", ".hidden-1.0.zip", "a-1.0.egg"} {
		if f, ok := parseFilename(bad); ok {
			t.Errorf("parseFilename(%q) accepted: %+v", bad, f)
		}
	}
}

func TestParsePaths(t *testing.T) {
	for path, want := range map[string]target{
		"simple/":                  {kRoot, "", ""},
		"simple":                   {kRoot, "", ""},
		"simple/Foo_Bar/":          {kIndex, "Foo_Bar", ""},
		"simple/foo":               {kIndex, "foo", ""},
		"legacy/":                  {kLegacy, "", ""},
		"files/foo/foo-1.0.tar.gz": {kFile, "foo", "foo-1.0.tar.gz"},
		"files/foo/foo-1.0-py3-none-any.whl.metadata": {kFile, "foo", "foo-1.0-py3-none-any.whl.metadata"},
		"yank/foo/foo-1.0.tar.gz":                     {kYank, "foo", "foo-1.0.tar.gz"},
		"index/foo.json":                              {kCached, "foo", ""},
	} {
		got, ok := parse(path)
		if !ok || got != want {
			t.Errorf("parse(%q) = %+v %v, want %+v", path, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "simple/a/b/", "simple/-a/", "files/Foo/foo-1.0.tar.gz", "files/foo/", "files/foo/x.txt", "files/foo/foo-1.0.tar.gz/x", "yank/foo/foo-1.0.tar.gz.metadata", "index/foo", "legacy/x", "../simple/"} {
		if got, ok := parse(bad); ok {
			t.Errorf("parse(%q) accepted: %+v", bad, got)
		}
	}
}

func TestWantsJSON(t *testing.T) {
	for h, want := range map[string]bool{
		"": false, "text/html": false, "*/*": false,
		"application/vnd.pypi.simple.v1+json": true,
		"application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1, text/html; q=0.01": true,
		"application/vnd.pypi.simple.v1+html":                                             false,
		"application/vnd.pypi.simple.v1+html, application/vnd.pypi.simple.v1+json; q=0.5": false,
		"application/json; q=0.9, text/html; q=0.1":                                       true,
	} {
		if got := wantsJSON(h); got != want {
			t.Errorf("wantsJSON(%q) = %v", h, got)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	h := parseHeaders([]byte("Metadata-Version: 2.1\nName: Foo_Bar\nVersion: 1.0\nRequires-Python: >=3.8\nSummary: x\n continued\n\nName: later\n"))
	if h["name"] != "Foo_Bar" || h["version"] != "1.0" || h["requires-python"] != ">=3.8" || h["summary"] != "x" {
		t.Fatalf("%v", h)
	}
	if !sameVersion("1.0_1", "1.0-1") || sameVersion("1.0", "1.1") || sameVersion("", "") {
		t.Fatal("sameVersion")
	}
}
