// Package npm is the format "npm": the registry API that the npm command (and yarn, pnpm) speaks, for hosted, proxy and group repositories (docs/npm.md). Like go and pypi it is built on raw: a
// tarball is a file of the repository, and the package (with its scope) is the name that claims and groups work with. What is added is what an npm registry must know: the packument (the document
// of a package, full and abbreviated) is made from the version manifests that are kept, publish reads the tarball and checks it, dist-tags, deprecation and unpublish are kept, and a proxy reads the
// packument of its upstream and checks each tarball against the hash that packument names.
package npm

import (
	"regexp"
	"strings"
)

const (
	maxName    = 214
	maxVersion = 128
	maxFile    = 300
)

var (
	// A name that may be published: lower case, url-safe (the rules of npm for a new package).
	newPart = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]*$`)
	// A name that may be read: the packages of the first years have capitals and other marks; they are in every registry that mirrors npmjs.
	oldPart = regexp.MustCompile(`^[A-Za-z0-9_.~-][A-Za-z0-9_.~!'()*-]*$`)
	semver  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	tagRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// validName says whether the name is a package name: "name" or "@scope/name". strict is for a name to publish (lower case); otherwise old names are read.
func validName(n string, strict bool) bool {
	if n == "" || len(n) > maxName || n == "node_modules" || n == "favicon.ico" {
		return false
	}
	re := oldPart
	if strict {
		re = newPart
	}
	scope, name, scoped := strings.Cut(n, "/")
	if !scoped {
		return !strings.HasPrefix(n, "@") && re.MatchString(n)
	}
	if !strings.HasPrefix(scope, "@") || strings.Contains(name, "/") {
		return false
	}
	return re.MatchString(scope[1:]) && re.MatchString(name)
}

func validVersion(v string) bool { return len(v) <= maxVersion && semver.MatchString(v) }

// validTag: a tag is a word that is not a version (npm refuses a tag that is a valid range, so that a tag cannot be taken for one).
func validTag(t string) bool { return tagRE.MatchString(t) && !validVersion(t) }

// unscoped is the part of the name after the scope.
func unscoped(n string) string {
	_, name, ok := strings.Cut(n, "/")
	if !ok {
		return n
	}
	return name
}

// tarball is the file name of a version: "<name without scope>-<version>.tgz".
func tarball(pkg, version string) string { return unscoped(pkg) + "-" + version + ".tgz" }

// The kinds of address of a repository of this format.
const (
	kPackument = "packument" // <pkg>
	kTarball   = "tarball"   // <pkg>/-/<name>-<version>.tgz
	kManifest  = "manifest"  // <pkg>/-/<version>.json: what is kept of a version; not an address of a client
	kCached    = "cached"    // -/index/<pkg>.json: what a proxy keeps of a packument; not an address of a client
	kTags      = "tags"      // -/package/<pkg>/dist-tags and -/package/<pkg>/dist-tags/<tag>
	kRev       = "rev"       // <pkg>/-rev/<rev>: the packument for a write that has a revision, or the whole package for a DELETE
	kTarRev    = "tarrev"    // <pkg>/-/<file>/-rev/<rev>: DELETE of a tarball (npm unpublish)
	kPing      = "ping"      // -/ping
	kWhoami    = "whoami"    // -/whoami
	kUser      = "user"      // -/user/...: npm adduser
	kOther     = "other"     // the rest of -/..., which an npm client may ask (audit, search)
)

type target struct {
	kind string
	pkg  string
	file string // a tarball or a manifest, or a tag
	rev  string
}

// split takes the package name off the front of parts: one part, or two when the first is a scope. It returns the name and the rest.
func split(parts []string) (string, []string, bool) {
	if len(parts) == 0 || parts[0] == "" || parts[0] == "-" {
		return "", nil, false
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) < 2 {
			return "", nil, false
		}
		return parts[0] + "/" + parts[1], parts[2:], true
	}
	return parts[0], parts[1:], true
}

// parse reads the path that follows the name of the repository (the percent-encoded "/" of a scope has been decoded already).
func parse(path string) (target, bool) {
	parts := strings.Split(path, "/")
	if parts[0] == "-" {
		switch {
		case len(parts) == 2 && parts[1] == "ping":
			return target{kind: kPing}, true
		case len(parts) == 2 && parts[1] == "whoami":
			return target{kind: kWhoami}, true
		case len(parts) >= 2 && parts[1] == "user":
			return target{kind: kUser}, true
		case len(parts) >= 3 && parts[1] == "index" && strings.HasSuffix(path, ".json"):
			pkg, rest, ok := split(parts[2:])
			pkg = strings.TrimSuffix(pkg, ".json")
			if !ok || len(rest) != 0 || !validName(pkg, false) {
				// a scope is "@s/n.json": the suffix is on the last part
				return target{}, false
			}
			return target{kind: kCached, pkg: pkg}, true
		case len(parts) >= 4 && parts[1] == "package":
			pkg, rest, ok := split(parts[2:])
			if !ok || !validName(pkg, false) || len(rest) == 0 || rest[0] != "dist-tags" || len(rest) > 2 {
				return target{}, false
			}
			t := target{kind: kTags, pkg: pkg}
			if len(rest) == 2 {
				if !validTag(rest[1]) {
					return target{}, false
				}
				t.file = rest[1]
			}
			return t, true
		default:
			return target{kind: kOther}, true
		}
	}
	pkg, rest, ok := split(parts)
	if !ok || !validName(pkg, false) {
		return target{}, false
	}
	switch {
	case len(rest) == 0:
		return target{kind: kPackument, pkg: pkg}, true
	case len(rest) == 2 && rest[0] == "-rev" && rest[1] != "":
		return target{kind: kRev, pkg: pkg, rev: rest[1]}, true
	case len(rest) == 2 && rest[0] == "-" && rest[1] != "" && len(rest[1]) <= maxFile:
		if v, ok := strings.CutSuffix(rest[1], ".json"); ok && validVersion(v) {
			return target{kind: kManifest, pkg: pkg, file: rest[1]}, true
		}
		if _, ok := version(pkg, rest[1]); !ok {
			return target{}, false
		}
		return target{kind: kTarball, pkg: pkg, file: rest[1]}, true
	case len(rest) == 4 && rest[0] == "-" && rest[2] == "-rev" && rest[3] != "":
		if _, ok := version(pkg, rest[1]); !ok {
			return target{}, false
		}
		return target{kind: kTarRev, pkg: pkg, file: rest[1], rev: rest[3]}, true
	}
	return target{}, false
}

// version reads the version out of the file name of a tarball of the package.
func version(pkg, file string) (string, bool) {
	v, ok := strings.CutPrefix(file, unscoped(pkg)+"-")
	if !ok {
		return "", false
	}
	v, ok = strings.CutSuffix(v, ".tgz")
	if !ok || !validVersion(v) {
		return "", false
	}
	return v, true
}

// nameOf is the name that claims and groups work with for the path of a file of the repository: the package, lower case.
func nameOf(path string) string {
	if t, ok := parse(path); ok && t.pkg != "" {
		return strings.ToLower(t.pkg)
	}
	return path
}
