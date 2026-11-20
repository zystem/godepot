// Package pypi is the format "pypi": the simple repository API of Python packaging (PEP 503, with the JSON of PEP 691, the yanked marks of PEP 592 and the metadata files of PEP 658), upload as `twine`
// does it (the legacy upload API), for hosted, proxy and group repositories (docs/pypi.md). Like the format go it is built on raw: a file is a file of the repository, and the project is the name that
// claims and groups work with. What is added is what a Python index must know: names are normalised, the index is generated, a wheel's metadata is read, and a proxy reads the index of its upstream.
package pypi

import (
	"regexp"
	"strings"
)

var (
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	filenameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!-]*$`)
	runRE      = regexp.MustCompile(`[-_.]+`)
)

const (
	maxName     = 256
	maxFilename = 255
)

// normalize is the normalisation of PEP 503: lower case, and a run of "-", "_" and "." is one "-".
func normalize(name string) string {
	return runRE.ReplaceAllString(strings.ToLower(name), "-")
}

func validName(n string) bool { return len(n) <= maxName && nameRE.MatchString(n) }

// file is what a file name says.
type file struct {
	project string // normalised
	version string
	wheel   bool
}

// parseFilename reads the name of a distribution file: a wheel "{name}-{version}(-{build})?-{python}-{abi}-{platform}.whl", or a source distribution "{name}-{version}.tar.gz" or ".zip".
func parseFilename(f string) (file, bool) {
	if len(f) > maxFilename || !filenameRE.MatchString(f) {
		return file{}, false
	}
	if base, ok := strings.CutSuffix(f, ".whl"); ok {
		parts := strings.Split(base, "-")
		if len(parts) != 5 && len(parts) != 6 {
			return file{}, false
		}
		if !validName(strings.ReplaceAll(parts[0], "_", "-")) || parts[1] == "" {
			return file{}, false
		}
		return file{project: normalize(parts[0]), version: parts[1], wheel: true}, true
	}
	for _, ext := range []string{".tar.gz", ".zip"} {
		if base, ok := strings.CutSuffix(f, ext); ok {
			i := strings.LastIndex(base, "-")
			if i <= 0 || i == len(base)-1 || !validName(base[:i]) {
				return file{}, false
			}
			return file{project: normalize(base[:i]), version: base[i+1:]}, true
		}
	}
	return file{}, false
}

// The kinds of address of a repository of this format.
const (
	kRoot   = "root"   // simple/
	kIndex  = "index"  // simple/<project>/
	kFile   = "file"   // files/<project>/<filename>, or <filename>.metadata
	kLegacy = "legacy" // legacy/: the upload
	kYank   = "yank"   // yank/<project>/<filename>
	kCached = "cached" // index/<project>.json: what a proxy keeps of an index; not an address of a client
)

type target struct {
	kind     string
	project  string // as written in the address
	filename string
}

// parse reads the path that follows the name of the repository.
func parse(path string) (target, bool) {
	parts := strings.Split(path, "/")
	switch {
	case path == "simple" || path == "simple/":
		return target{kind: kRoot}, true
	case parts[0] == "simple" && (len(parts) == 2 || (len(parts) == 3 && parts[2] == "")) && validName(parts[1]):
		return target{kind: kIndex, project: parts[1]}, true
	case parts[0] == "legacy" && (len(parts) == 1 || (len(parts) == 2 && parts[1] == "")):
		return target{kind: kLegacy}, true
	case (parts[0] == "files" || parts[0] == "yank") && len(parts) == 3 && validName(parts[1]) && parts[1] == normalize(parts[1]):
		fn := strings.TrimSuffix(parts[2], ".metadata")
		if _, ok := parseFilename(fn); !ok {
			return target{}, false
		}
		if parts[0] == "yank" && fn != parts[2] {
			return target{}, false
		}
		k := kFile
		if parts[0] == "yank" {
			k = kYank
		}
		return target{kind: k, project: parts[1], filename: parts[2]}, true
	case parts[0] == "index" && len(parts) == 2 && strings.HasSuffix(parts[1], ".json") && validName(strings.TrimSuffix(parts[1], ".json")):
		return target{kind: kCached, project: strings.TrimSuffix(parts[1], ".json")}, true
	}
	return target{}, false
}
