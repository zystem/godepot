// Package maven is the format "maven": the layout of a Maven 2 repository (what `mvn`, Gradle, sbt and Ivy read and write) for hosted, proxy and group repositories (docs/maven.md). Like go, pypi and
// npm it is built on raw: an artifact is a file of the repository at its Maven path, and the artifact (its group directories and its id, "com/acme/app") is the name that claims and groups work with.
// What is added is what a Maven repository must know: maven-metadata.xml and the checksum files are made by the program, an uploaded pom is read and checked against its path, versions are ordered
// as Maven orders them, a snapshot is a set of timestamped files, and a proxy checks each file against the checksum file the upstream has for it.
package maven

import (
	"regexp"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/settings"
)

var (
	segRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+~@-]*$`)
	tsRE  = `(\d{8}\.\d{6})-(\d+)`
)

// The kinds of address of a repository of this format.
const (
	kFile     = "file"     // <group>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<ext>: an artifact, a pom, a signature
	kMeta     = "meta"     // <group>/<artifact>/maven-metadata.xml: the versions of an artifact
	kSnapMeta = "snapmeta" // <group>/<artifact>/<version>-SNAPSHOT/maven-metadata.xml: the latest files of a snapshot
	kSum      = "sum"      // any of the above and ".sha1", ".md5", ".sha256" or ".sha512"
	kOther    = "other"    // anything else: a directory, the metadata of a group of plugins
)

var sumAlgos = []string{"sha512", "sha256", "sha1", "md5"} // the strongest first

// target is what a path says.
type target struct {
	kind     string
	name     string // the group directories and the artifact id: "com/acme/app"
	group    string // "com.acme"
	artifact string
	version  string // the name of the version directory: "1.0", "1.0-SNAPSHOT"
	file     string
	// for a snapshot file: the timestamp and the build number; classifier and extension of a file
	stamp, build, classifier, ext string
	algo                          string  // for kSum
	inner                         *target // for kSum: what the checksum is of
}

func (t target) snapshot() bool { return strings.HasSuffix(t.version, "-SNAPSHOT") }

// parse reads a path of the repository. A path that is not of a known shape is kOther (a proxy hands it on; a hosted repository refuses to write it).
func parse(path string) target {
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if !segRE.MatchString(s) {
			return target{kind: kOther}
		}
	}
	last := segs[len(segs)-1]
	for _, a := range sumAlgos {
		if base, ok := strings.CutSuffix(last, "."+a); ok && base != "" {
			in := parse(strings.TrimSuffix(path, "."+a))
			if in.kind == kSum || in.kind == kOther {
				return target{kind: kOther}
			}
			return target{kind: kSum, algo: a, inner: &in, name: in.name, group: in.group, artifact: in.artifact, version: in.version, file: last}
		}
	}
	n := len(segs)
	if last == "maven-metadata.xml" {
		dir := segs[:n-1]
		if len(dir) >= 3 && strings.HasSuffix(dir[len(dir)-1], "-SNAPSHOT") {
			art := dir[len(dir)-2]
			name := strings.Join(dir[:len(dir)-1], "/")
			return target{kind: kSnapMeta, name: name, group: strings.Join(dir[:len(dir)-2], "."), artifact: art, version: dir[len(dir)-1], file: last}
		}
		if len(dir) >= 2 {
			return target{kind: kMeta, name: strings.Join(dir, "/"), group: strings.Join(dir[:len(dir)-1], "."), artifact: dir[len(dir)-1], file: last}
		}
		return target{kind: kOther}
	}
	if n < 4 {
		return target{kind: kOther}
	}
	t := target{kind: kFile, file: last, version: segs[n-2], artifact: segs[n-3], name: strings.Join(segs[:n-2], "/"), group: strings.Join(segs[:n-3], ".")}
	prefix := t.artifact + "-"
	if !strings.HasPrefix(last, prefix) {
		return target{kind: kOther}
	}
	rest := last[len(prefix):]
	if t.snapshot() {
		// Unique snapshots only: <artifact>-<base>-<yyyyMMdd.HHmmss>-<n>[-classifier].ext
		base := strings.TrimSuffix(t.version, "-SNAPSHOT")
		m := regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `-` + tsRE + `(.*)$`).FindStringSubmatch(rest)
		if m == nil {
			return target{kind: kOther}
		}
		t.stamp, t.build, rest = m[1], m[2], m[3]
		if _, err := time.Parse("20060102.150405", t.stamp); err != nil {
			return target{kind: kOther}
		}
	} else {
		r, ok := strings.CutPrefix(rest, t.version)
		if !ok {
			return target{kind: kOther}
		}
		rest = r
	}
	switch {
	case strings.HasPrefix(rest, "."):
		t.ext = rest[1:]
	case strings.HasPrefix(rest, "-"):
		c, e, ok := strings.Cut(rest[1:], ".")
		if !ok || c == "" {
			return target{kind: kOther}
		}
		t.classifier, t.ext = c, e
	default:
		return target{kind: kOther}
	}
	if t.ext == "" {
		return target{kind: kOther}
	}
	return t
}

// nameOf is the name that claims and groups work with for a path.
func nameOf(path string) string {
	if t := parse(path); t.name != "" {
		return t.name
	}
	return path
}

// ttlOf is how long a proxied file is trusted: the metadata and what is not a file of a version can change; a file of a version cannot, except a snapshot that has no timestamp.
func ttlOf(path string, u *settings.Upstream) time.Duration {
	t := parse(path)
	if t.kind == kSum && t.inner != nil {
		t = *t.inner
	}
	switch t.kind {
	case kMeta, kSnapMeta, kOther:
		return u.TagTTL()
	}
	return 0
}
