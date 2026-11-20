package npm

import "testing"

func TestParse(t *testing.T) {
	for path, want := range map[string]target{
		"left-pad":                          {kind: kPackument, pkg: "left-pad"},
		"@corp/lib":                         {kind: kPackument, pkg: "@corp/lib"},
		"left-pad/-/left-pad-1.3.0.tgz":     {kind: kTarball, pkg: "left-pad", file: "left-pad-1.3.0.tgz"},
		"@corp/lib/-/lib-1.0.0-rc.1.tgz":    {kind: kTarball, pkg: "@corp/lib", file: "lib-1.0.0-rc.1.tgz"},
		"@corp/lib/-/1.0.0.json":            {kind: kManifest, pkg: "@corp/lib", file: "1.0.0.json"},
		"@corp/lib/-rev/3-abc":              {kind: kRev, pkg: "@corp/lib", rev: "3-abc"},
		"lib/-/lib-1.0.0.tgz/-rev/3-abc":    {kind: kTarRev, pkg: "lib", file: "lib-1.0.0.tgz", rev: "3-abc"},
		"-/index/@corp/lib.json":            {kind: kCached, pkg: "@corp/lib"},
		"-/index/lib.json":                  {kind: kCached, pkg: "lib"},
		"-/package/@corp/lib/dist-tags":     {kind: kTags, pkg: "@corp/lib"},
		"-/package/lib/dist-tags/beta":      {kind: kTags, pkg: "lib", file: "beta"},
		"-/ping":                            {kind: kPing},
		"-/whoami":                          {kind: kWhoami},
		"-/user/org.couchdb.user:x":         {kind: kUser},
		"-/npm/v1/security/advisories/bulk": {kind: kOther},
	} {
		got, ok := parse(path)
		if !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", path, got, ok, want)
		}
	}
	for _, path := range []string{
		"@corp", "@corp/", "lib/-/other-1.0.0.tgz", "lib/-/lib-1.0.tgz", "lib/-/lib-1.0.0.zip", "lib/x", "lib/-/", "-/package/lib/versions", "-/package/lib/dist-tags/1.0.0",
		"-/package/lib/dist-tags/a/b", "node_modules", "lib/-/lib-1.0.0.tgz/-rev/", "@/x", "@a/b/c", ".hidden/x/y",
	} {
		if got, ok := parse(path); ok {
			t.Errorf("%s: taken as %+v", path, got)
		}
	}
}

func TestNames(t *testing.T) {
	for n, strict := range map[string]bool{"a": true, "@s/n": true, "a.b_c~d-e": true, "a1": true} {
		if !validName(n, strict) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "A", "_a", ".a", "-a", "@s", "@/n", "@s/n/m", "a b", "a/b", "@S/n", "node_modules", "favicon.ico"} {
		if validName(n, true) {
			t.Errorf("%q taken for a name to publish", n)
		}
	}
	if !validName("JSONStream", false) || validName("JSONStream", true) {
		t.Error("an old name is read, never published")
	}
	for _, v := range []string{"1.0.0", "0.0.1", "1.2.3-rc.1", "1.2.3+build.5", "10.20.30-alpha.1+x"} {
		if !validVersion(v) {
			t.Errorf("version %q refused", v)
		}
	}
	for _, v := range []string{"1.0", "v1.0.0", "01.0.0", "1.0.0-", "latest", "1.0.0 ", ""} {
		if validVersion(v) {
			t.Errorf("version %q taken", v)
		}
	}
	if validTag("1.2.3") || !validTag("beta") || validTag("") || validTag("-x") {
		t.Error("tags")
	}
	if nameOf("@Corp/Lib/-/Lib-1.0.0.tgz") != "@corp/lib" || nameOf("-/index/Foo.json") != "foo" || nameOf("-/ping") != "-/ping" {
		t.Error("nameOf")
	}
	if tarball("@s/n", "1.0.0") != "n-1.0.0.tgz" || tarball("n", "1.0.0") != "n-1.0.0.tgz" {
		t.Error("tarball")
	}
}
