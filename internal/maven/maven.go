package maven

import (
	"context"
	"crypto/md5"  // #nosec G501 -- Maven names an md5 of every file; it is a name, not a defence
	"crypto/sha1" // #nosec G505 -- the same: the sha1 of Maven
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/raw"
)

// Maven is the handler of the repositories of the format maven.
type Maven struct {
	*raw.Raw
}

// New makes the handler from a raw one (which it copies and sets up).
func New(base *raw.Raw) *Maven {
	r := *base
	m := &Maven{Raw: &r}
	r.Self = m
	r.Layout = &raw.Layout{
		Unit: func(path string) (string, string, bool) {
			if t := parse(path); t.kind == kFile {
				return t.name, t.version, true
			}
			return "", "", false
		},
		Name: nameOf,
		// A repository has an artifact when it has a file below its directory; a proxy is asked for its maven-metadata.xml.
		Probe: func(path string) string {
			if t := parse(path); t.name != "" {
				return t.name + "/maven-metadata.xml"
			}
			return path
		},
		TTL:    ttlOf,
		Source: m.source,
	}
	return m
}

func newHash(algo string) hash.Hash {
	switch algo {
	case "md5":
		return md5.New() // #nosec G401 -- see the import
	case "sha1":
		return sha1.New() // #nosec G401 -- see the import
	case "sha256":
		return sha256.New()
	}
	return sha512.New()
}

func sizeOf(algo string) int { return newHash(algo).Size() * 2 }

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

func (m *Maven) unavailable(w http.ResponseWriter) {
	fail(w, http.StatusServiceUnavailable, "the database or the store is not available: try again")
}

// Serve answers one request.
func (m *Maven) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	if q.Path == "" || strings.HasSuffix(q.Path, "/") {
		m.Raw.Serve(w, r, q) // a listing: raw's
		return
	}
	t := parse(q.Path)
	switch q.Repo.Kind {
	case "group":
		m.Raw.Serve(w, r, q) // raw finds the member that has the name, or sends a write to write_to, and comes back here
	case "proxy":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "a proxy repository is read-only: deploy to a hosted repository, or to a group that has write_to")
			return
		}
		if t.kind == kSum && (t.inner.kind == kMeta || t.inner.kind == kSnapMeta) {
			m.proxySum(w, r, q, t)
			return
		}
		m.Raw.Serve(w, r, q)
	default:
		m.hosted(w, r, q, t)
	}
}

func (m *Maven) hosted(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case read && (t.kind == kMeta || t.kind == kSnapMeta):
		m.metadata(w, r, q, t)
	case read && t.kind == kSum:
		m.sum(w, r, q, t)
	case read || (r.Method == http.MethodDelete && t.kind == kFile):
		m.Raw.Serve(w, r, q)
	case r.Method == http.MethodPut:
		m.put(w, r, q, t)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed here")
	}
}

// props is what is kept of a file besides its SHA-256 (which is its digest): the other checksums that Maven names.
type props struct {
	SHA1   string `json:"sha1"`
	MD5    string `json:"md5"`
	SHA512 string `json:"sha512"`
}

// stored finds a file: its digest, and its props.
func (m *Maven) stored(ctx context.Context, repo, path string) (digest string, p props, ok bool, err error) {
	rows, err := m.DB.Query(ctx, meta.S(`SELECT a.digest, c.props FROM assets a JOIN components c ON c.id = a.component_id WHERE a.repo = ? AND a.path = ?`, repo, path))
	if err != nil || len(rows) == 0 {
		return "", p, false, err
	}
	_ = json.Unmarshal([]byte(meta.Str(rows[0][1])), &p)
	return meta.Str(rows[0][0]), p, true, nil
}

// sum answers a request for a checksum file of a hosted repository: the checksum of the file that is kept (made when it was uploaded), or of the metadata that the program makes.
func (m *Maven) sum(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	ctx := r.Context()
	var hexsum string
	switch t.inner.kind {
	case kMeta, kSnapMeta:
		body, _, err := m.generate(ctx, q.Repo.Name, *t.inner)
		if err != nil {
			m.unavailable(w)
			return
		}
		if body == nil {
			fail(w, http.StatusNotFound, "no such artifact")
			return
		}
		h := newHash(t.algo)
		_, _ = h.Write(body)
		hexsum = hex.EncodeToString(h.Sum(nil))
	default:
		digest, p, found, err := m.stored(ctx, q.Repo.Name, strings.TrimSuffix(q.Path, "."+t.algo))
		if err != nil {
			m.unavailable(w)
			return
		}
		if !found {
			fail(w, http.StatusNotFound, "no such file")
			return
		}
		switch t.algo {
		case "sha256":
			hexsum = digest
		case "sha1":
			hexsum = p.SHA1
		case "md5":
			hexsum = p.MD5
		default:
			hexsum = p.SHA512
		}
		if hexsum == "" { // a file that was kept before its checksums were: read it
			var err error
			if hexsum, err = m.compute(ctx, digest, t.algo); err != nil {
				m.unavailable(w)
				return
			}
		}
	}
	write(w, r, []byte(hexsum), "text/plain; charset=utf-8")
}

func (m *Maven) compute(ctx context.Context, digest, algo string) (string, error) {
	rc, _, err := m.Blobs.Open(ctx, digest, 0, -1)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := newHash(algo)
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// write sends a made document, with an ETag.
func write(w http.ResponseWriter, r *http.Request, body []byte, ct string) {
	s := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(s[:16]) + `"`
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102150405") }
