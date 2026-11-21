package maven

import (
	"sort"
	"testing"
)

func TestParse(t *testing.T) {
	type c struct {
		path                   string
		kind, name, ver, class string
		ext, stamp, algo       string
	}
	for _, x := range []c{
		{"com/acme/app/1.0/app-1.0.jar", kFile, "com/acme/app", "1.0", "", "jar", "", ""},
		{"com/acme/app/1.0/app-1.0-sources.jar", kFile, "com/acme/app", "1.0", "sources", "jar", "", ""},
		{"com/acme/app/1.0/app-1.0.tar.gz", kFile, "com/acme/app", "1.0", "", "tar.gz", "", ""},
		{"com/acme/app/1.0/app-1.0.jar.asc", kFile, "com/acme/app", "1.0", "", "jar.asc", "", ""},
		{"com/acme/app/1.0-SNAPSHOT/app-1.0-20240102.030405-7-tests.jar", kFile, "com/acme/app", "1.0-SNAPSHOT", "tests", "jar", "20240102.030405", ""},
		{"com/acme/app/maven-metadata.xml", kMeta, "com/acme/app", "", "", "", "", ""},
		{"com/acme/app/1.0-SNAPSHOT/maven-metadata.xml", kSnapMeta, "com/acme/app", "1.0-SNAPSHOT", "", "", "", ""},
		{"com/acme/app/1.0/app-1.0.jar.sha1", kSum, "com/acme/app", "1.0", "", "", "", "sha1"},
		{"com/acme/app/maven-metadata.xml.sha512", kSum, "com/acme/app", "", "", "", "", "sha512"},
		{"com/acme/app/", kOther, "", "", "", "", "", ""},
		{"app-1.0.jar", kOther, "", "", "", "", "", ""},
		{"com/acme/app/1.0/other-1.0.jar", kOther, "", "", "", "", "", ""},
		{"com/acme/app/1.0/app-2.0.jar", kOther, "", "", "", "", "", ""},
		{"com/acme/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar", kOther, "", "", "", "", "", ""}, // not unique
		{"com/acme/app/1.0/app-1.0.jar.sha1.sha1", kOther, "", "", "", "", "", ""},
		{"com/../app/1.0/app-1.0.jar", kOther, "", "", "", "", "", ""},
		{"plugins/maven-metadata.xml", kOther, "", "", "", "", "", ""},
	} {
		got := parse(x.path)
		if got.kind != x.kind || got.name != x.name || got.version != x.ver || got.classifier != x.class || got.ext != x.ext || got.stamp != x.stamp || got.algo != x.algo {
			t.Errorf("%s: %+v", x.path, got)
		}
	}
	if g := parse("com/acme/app/1.0/app-1.0.jar"); g.group != "com.acme" || g.artifact != "app" {
		t.Errorf("group: %+v", g)
	}
}

func TestVersionOrder(t *testing.T) {
	want := []string{"1.0-alpha-1", "1.0-beta-2", "1.0-rc1", "1.0-SNAPSHOT", "1.0", "1.0-sp1", "1.0.1", "1.1", "1.2", "1.10", "2.0-SNAPSHOT", "2.0"}
	got := append([]string{}, want...)
	for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
		got[i], got[j] = got[j], got[i]
	}
	sort.Slice(got, func(i, j int) bool { return compare(got[i], got[j]) < 0 })
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: %v, want %v", got, want)
		}
	}
	if compare("1.0", "1.0.0") != 0 || compare("1.0-ga", "1.0") != 0 || compare("1.0", "1.0.0.1") >= 0 {
		t.Error("equal and padded versions")
	}
}
