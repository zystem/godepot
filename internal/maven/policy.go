package maven

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/policy"
)

// The block list of a Maven proxy (docs/spec.md, P7). A rule names "groupId:artifactId" (patterns) and, after "@", a version; a digest rule is checked by raw as the bytes come in. A cooldown is not built for this
// format: the upstream gives no time of publication in its listings (docs/maven.md).
func coords(t target) string { return t.group + ":" + t.artifact }

var versionEl = regexp.MustCompile(`(?m)^[ \t]*<version>([^<]*)</version>[ \t]*\n`)

// policed answers a request to a proxy that has a policy, when the policy has a word to say: the files of a blocked version are refused, and the metadata is made without the blocked versions. false: raw answers.
func (m *Maven) policed(w http.ResponseWriter, r *http.Request, q format.Request, t target) bool {
	pol := q.Repo.Upstream.Policy()
	if !pol.Active() {
		return false
	}
	in := t
	if t.kind == kSum {
		in = *t.inner
	}
	switch in.kind {
	case kFile, kSnapMeta:
		if v, blocked := pol.Blocked(coords(in), in.version); blocked {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(v.Why + "\n"))
			return true
		}
	case kMeta:
		if len(pol.Block) == 0 {
			return false
		}
		if !q.Authorize(w, auth.Read, q.Path) {
			return true
		}
		body, c := m.filteredMeta(r, q, in, pol)
		if body == nil {
			fail(w, c.code, "the metadata is not available")
			return true
		}
		if t.kind == kSum {
			h := newHash(t.algo)
			_, _ = h.Write(body)
			write(w, r, []byte(hexOf(h.Sum(nil))), "text/plain; charset=utf-8")
			return true
		}
		write(w, r, body, "application/xml")
		return true
	}
	return false
}

// filteredMeta reads the metadata of an artifact through the cache and leaves out the versions that the block list names.
func (m *Maven) filteredMeta(r *http.Request, q format.Request, t target, pol *policy.Policy) ([]byte, *capture) {
	path := t.name + "/maven-metadata.xml"
	c := m.fetch(r.Context(), r, q, path)
	if c.code != http.StatusOK || c.full {
		if c.code == 0 || c.full {
			c.code = http.StatusBadGateway
		}
		return nil, c
	}
	text := c.buf.String()
	var kept []string
	text = versionEl.ReplaceAllStringFunc(text, func(el string) string {
		v := versionEl.FindStringSubmatch(el)[1]
		if _, blocked := pol.Blocked(coords(t), strings.TrimSpace(v)); blocked {
			return ""
		}
		kept = append(kept, strings.TrimSpace(v))
		return el
	})
	// latest and release must not name a version that is gone: the highest that is left takes the place.
	for _, tag := range []string{"latest", "release"} {
		re := regexp.MustCompile(`<` + tag + `>([^<]*)</` + tag + `>`)
		text = re.ReplaceAllStringFunc(text, func(el string) string {
			v := strings.TrimSpace(re.FindStringSubmatch(el)[1])
			if _, blocked := pol.Blocked(coords(t), v); !blocked {
				return el
			}
			best := ""
			for _, k := range kept {
				if tag == "release" && strings.HasSuffix(k, "-SNAPSHOT") {
					continue
				}
				if best == "" || compare(best, k) < 0 {
					best = k
				}
			}
			if best == "" {
				return ""
			}
			return "<" + tag + ">" + best + "</" + tag + ">"
		})
	}
	return []byte(text), c
}
