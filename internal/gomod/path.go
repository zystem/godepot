// Package gomod is the format "go": the module proxy protocol of the go command (GOPROXY): "<module>/@v/list", "<module>/@v/<version>.info|.mod|.zip", "<module>/@latest" (docs/gomod.md). It is built on
// the format raw: a module file is a file of the repository at the path of the address, and what raw does for a proxy and for a group (fetching, the cache, claims by name) it does here with the
// module as the name. What is added is what a Go repository must know: the shape of the paths, the list and the latest version made from what is stored, and the check of a module that is published.
package gomod

import (
	"regexp"
	"strconv"
	"strings"
)

// The kinds of file of the protocol.
const (
	kList   = "list"
	kLatest = "latest"
	kInfo   = "info"
	kMod    = "mod"
	kZip    = "zip"
)

// target is what a path names.
type target struct {
	module  string // escaped, as in the address
	kind    string
	version string // escaped, for info, mod and zip
}

const (
	maxModule  = 512
	maxVersion = 128
)

var (
	moduleRE  = regexp.MustCompile(`^[a-z0-9!._~+-]+(/[a-z0-9!._~+-]+)*$`)
	versionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+incompatible)?$`)
)

// parse reads the path that follows the name of the repository.
func parse(path string) (target, bool) {
	if mod, ok := strings.CutSuffix(path, "/@latest"); ok {
		if !validModule(mod) {
			return target{}, false
		}
		return target{module: mod, kind: kLatest}, true
	}
	mod, rest, ok := strings.Cut(path, "/@v/")
	if !ok || !validModule(mod) {
		return target{}, false
	}
	if rest == "list" {
		return target{module: mod, kind: kList}, true
	}
	for _, k := range []string{kInfo, kMod, kZip} {
		if v, ok := strings.CutSuffix(rest, "."+k); ok {
			if _, ok := unescape(v); !ok {
				return target{}, false
			}
			if u, _ := unescape(v); len(u) > maxVersion || !versionRE.MatchString(u) {
				return target{}, false
			}
			return target{module: mod, kind: k, version: v}, true
		}
	}
	return target{}, false
}

func validModule(m string) bool {
	if m == "" || len(m) > maxModule || !moduleRE.MatchString(m) {
		return false
	}
	for _, e := range strings.Split(m, "/") {
		if e == "." || e == ".." || strings.HasPrefix(e, "-") || strings.Contains(e, "..") {
			return false
		}
	}
	_, ok := unescape(m)
	return ok
}

// unescape reads the escaping of the protocol: an upper-case letter is written "!" and the lower-case letter. A bare upper-case letter, or a "!" that is not followed by a lower-case letter, is not valid.
func unescape(s string) (string, bool) {
	var sb strings.Builder
	bang := false
	for _, c := range s {
		switch {
		case bang:
			if c < 'a' || c > 'z' {
				return "", false
			}
			sb.WriteRune(c - 'a' + 'A')
			bang = false
		case c == '!':
			bang = true
		case c >= 'A' && c <= 'Z':
			return "", false
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String(), !bang
}

// escape is the reverse of unescape.
func escape(s string) string {
	var sb strings.Builder
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			sb.WriteByte('!')
			sb.WriteRune(c - 'A' + 'a')
		} else {
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

// semver is a version split for comparing.
type semver struct {
	nums [3]int
	pre  []string // empty: a release
}

func parseSemver(v string) (semver, bool) {
	if !versionRE.MatchString(v) {
		return semver{}, false
	}
	v = strings.TrimSuffix(v[1:], "+incompatible")
	core, pre, _ := strings.Cut(v, "-")
	var s semver
	for i, p := range strings.Split(core, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return semver{}, false
		}
		s.nums[i] = n
	}
	if pre != "" {
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

// less says whether a comes before b by the rules of semantic versioning: a release comes after its prereleases.
func (a semver) less(b semver) bool {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			return a.nums[i] < b.nums[i]
		}
	}
	switch {
	case len(a.pre) == 0:
		return false
	case len(b.pre) == 0:
		return true
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if a.pre[i] == b.pre[i] {
			continue
		}
		an, aerr := strconv.Atoi(a.pre[i])
		bn, berr := strconv.Atoi(b.pre[i])
		switch {
		case aerr == nil && berr == nil:
			return an < bn
		case aerr == nil:
			return true
		case berr == nil:
			return false
		}
		return a.pre[i] < b.pre[i]
	}
	return len(a.pre) < len(b.pre)
}
