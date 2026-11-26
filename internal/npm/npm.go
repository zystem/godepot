package npm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/settings"
)

const (
	mimeAbbrev = "application/vnd.npm.install-v1+json"
	mimeJSON   = "application/json"
)

// NPM is the handler of the repositories of the format npm.
type NPM struct {
	*raw.Raw
}

// New makes the handler from a raw one (which it copies and sets up).
func New(base *raw.Raw) *NPM {
	r := *base
	p := &NPM{Raw: &r}
	r.Self = p
	r.Layout = &raw.Layout{
		Unit: func(path string) (string, string, bool) {
			t, ok := parse(path)
			switch {
			case !ok:
			case t.kind == kManifest:
				return t.pkg, strings.TrimSuffix(t.file, ".json"), true
			case t.kind == kTarball:
				if v, ok := version(t.pkg, t.file); ok {
					return t.pkg, v, true
				}
			}
			return "", "", false
		},
		Name:        nameOf,
		AssetPrefix: func(name string) string { return name + "/-/" },
		Also:        func(name string) []string { return []string{"-/index/" + name + ".json"} },
		Probe: func(path string) string {
			if t, ok := parse(path); ok && t.pkg != "" {
				return t.pkg
			}
			return path
		},
		// A packument can change; a tarball cannot.
		TTL: func(path string, u *settings.Upstream) time.Duration {
			if t, ok := parse(path); ok && t.kind == kCached {
				return u.TagTTL()
			}
			return 0
		},
		Source: p.source,
	}
	return p
}

// fail answers in the words of npm: {"error": "..."}.
func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", mimeJSON)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func ok(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", mimeJSON)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *NPM) unavailable(w http.ResponseWriter) {
	fail(w, http.StatusServiceUnavailable, "the database or the store is not available: try again")
}

// Serve answers one request.
func (p *NPM) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	t, found := parse(q.Path)
	if !found || t.kind == kManifest || t.kind == kCached {
		fail(w, http.StatusNotFound, "not an address of this repository: <package>, <package>/-/<file>.tgz, -/package/<package>/dist-tags")
		return
	}
	switch t.kind {
	case kPing:
		ok(w, http.StatusOK, map[string]string{})
		return
	case kWhoami:
		if q.Principal.Anonymous() {
			q.Authorize(w, auth.Write, "-/whoami") // 401
			return
		}
		ok(w, http.StatusOK, map[string]string{"username": q.Principal.Account})
		return
	case kUser:
		p.login(w, r, q)
		return
	case kOther:
		fail(w, http.StatusNotFound, "this registry does not have that: audit and search are not built")
		return
	}
	switch q.Repo.Kind {
	case "hosted":
		p.hosted(w, r, q, t)
	case "proxy":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "a proxy repository is read-only: publish to a hosted repository, or to a group that has write_to")
			return
		}
		switch t.kind {
		case kPackument, kTags:
			p.proxyDoc(w, r, q, t)
		case kTarball:
			if p.tarballAllowed(w, r, q, t) {
				p.Raw.Serve(w, r, q)
			}
		default:
			fail(w, http.StatusNotFound, "not an address of a proxy repository")
		}
	case "group":
		p.group(w, r, q, t)
	}
}

func (p *NPM) hosted(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case t.kind == kPackument && read:
		p.hostedDoc(w, r, q, t)
	case t.kind == kPackument && r.Method == http.MethodPut:
		p.publish(w, r, q, t.pkg, "")
	case t.kind == kRev && r.Method == http.MethodPut:
		p.publish(w, r, q, t.pkg, t.rev)
	case t.kind == kRev && r.Method == http.MethodDelete:
		p.unpublish(w, r, q, t)
	case t.kind == kTarRev && r.Method == http.MethodDelete:
		p.unpublish(w, r, q, t)
	case t.kind == kTarball && read:
		p.Raw.Serve(w, r, q)
	case t.kind == kTags && read:
		p.hostedDoc(w, r, q, t)
	case t.kind == kTags && (r.Method == http.MethodPut || r.Method == http.MethodDelete) && t.file != "":
		p.tag(w, r, q, t)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed here")
	}
}

// group answers a request sent to a group: a publish and the changes go to write_to; a packument and a tarball come from the member that has the package.
func (p *NPM) group(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !read {
		m := q.Set.Repo(q.Repo.WriteTo)
		if m == nil {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "this group has no write_to: publish to a hosted repository, or name one as write_to of the group")
			return
		}
		q.Repo = m
		p.Serve(w, r, q)
		return
	}
	if t.kind == kTarball {
		p.Raw.Serve(w, r, q) // raw finds the member by the package, and comes back here for the member
		return
	}
	if !q.Authorize(w, auth.Read, t.pkg) {
		return
	}
	ctx := r.Context()
	probe := "-/index/" + t.pkg + ".json"
	m, err := format.Resolve(ctx, p.DB, q.Set, q.Repo, strings.ToLower(t.pkg), func(l *settings.Repo) (bool, error) { return p.Raw.Has(ctx, l, probe) })
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "the upstream cannot be asked now: try again ("+err.Error()+")")
		return
	}
	if m == nil {
		fail(w, http.StatusNotFound, "no member of this group has this package")
		return
	}
	q.Repo = m
	p.Serve(w, r, q)
}

// base is the address of the repository as the client named it, whole: a tarball is named by an address in the packument.
func base(r *http.Request, q format.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/repository/" + q.Address + "/"
}

// A version, as it is kept.
type ver struct {
	v          string
	manifest   map[string]json.RawMessage
	created    int64
	tags       []string
	deprecated string
	digest     string
}

const maxManifest = 4 << 20

// load reads the versions of a package that a hosted repository has.
func (p *NPM) load(ctx context.Context, repo, pkg string) ([]*ver, error) {
	prefix := pkg + "/-/"
	rows, err := p.DB.Query(ctx, meta.S(`SELECT a.path, a.digest, a.created_at, c.props FROM assets a JOIN components c ON c.id = a.component_id
		WHERE a.repo = ? AND substr(a.path, 1, ?) = ? ORDER BY a.path`, repo, len(prefix), prefix))
	if err != nil {
		return nil, err
	}
	var out []*ver
	for _, row := range rows {
		file := strings.TrimPrefix(meta.Str(row[0]), prefix)
		v, isJSON := strings.CutSuffix(file, ".json")
		if !isJSON || strings.Contains(v, "/") || !validVersion(v) {
			continue
		}
		props := map[string]string{}
		_ = json.Unmarshal([]byte(meta.Str(row[3])), &props)
		e := &ver{v: v, created: meta.Int(row[2]), deprecated: props["deprecated"], digest: meta.Str(row[1])}
		for k := range props {
			if t, ok := strings.CutPrefix(k, "tag."); ok {
				e.tags = append(e.tags, t)
			}
		}
		sort.Strings(e.tags) // the order of a map is not one: the revision must not change with it
		rc, _, err := p.Blobs.Open(ctx, e.digest, 0, -1)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxManifest+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(b, &e.manifest) != nil {
			continue // not a manifest of ours: not shown
		}
		out = append(out, e)
	}
	return out, nil
}

// tagMap says which version each tag names: when two versions have the tag (a write that was cut short), the one that was written last.
func tagMap(vs []*ver) map[string]string {
	best := map[string]*ver{}
	for _, e := range vs {
		for _, t := range e.tags {
			if b := best[t]; b == nil || e.created > b.created || (e.created == b.created && e.v > b.v) {
				best[t] = e
			}
		}
	}
	out := map[string]string{}
	for t, e := range best {
		out[t] = e.v
	}
	return out
}

// rev is the revision of a package: it changes with every change to its versions, tags and deprecations, so that a write made on a document that was read before another write is refused.
func rev(vs []*ver) string {
	sort.Slice(vs, func(i, j int) bool { return vs[i].v < vs[j].v })
	h := sha256.New()
	for _, e := range vs {
		fmt.Fprintf(h, "%s %s %s %v\n", e.v, e.digest, e.deprecated, e.tags)
	}
	return fmt.Sprintf("%d-%s", len(vs), hex.EncodeToString(h.Sum(nil))[:32])
}

func iso(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05.000Z") }

// The keys of a version that the abbreviated document keeps (the "corgi" document of npm).
var abbreviated = map[string]bool{
	"name": true, "version": true, "dependencies": true, "devDependencies": true, "optionalDependencies": true, "peerDependencies": true, "peerDependenciesMeta": true,
	"bundleDependencies": true, "bundledDependencies": true, "bin": true, "directories": true, "engines": true, "cpu": true, "os": true, "libc": true, "deprecated": true,
	"_hasShrinkwrap": true, "hasInstallScript": true, "dist": true,
}

func wantsAbbrev(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), mimeAbbrev)
}

// document makes the packument of a package from its versions.
func document(pkg, root string, vs []*ver, abbrev bool) ([]byte, error) {
	versions := map[string]map[string]json.RawMessage{}
	times := map[string]string{}
	var created, modified int64
	for _, e := range vs {
		m := map[string]json.RawMessage{}
		for k, v := range e.manifest {
			if !abbrev || abbreviated[k] {
				m[k] = v
			}
		}
		dist := map[string]json.RawMessage{}
		if raw, found := e.manifest["dist"]; found {
			_ = json.Unmarshal(raw, &dist)
		}
		file := tarball(pkg, e.v)
		u, _ := json.Marshal(root + pkg + "/-/" + file)
		dist["tarball"] = u
		d, _ := json.Marshal(dist)
		m["dist"] = d
		if e.deprecated != "" {
			d, _ := json.Marshal(e.deprecated)
			m["deprecated"] = d
		}
		versions[e.v] = m
		times[e.v] = iso(e.created)
		if created == 0 || e.created < created {
			created = e.created
		}
		if e.created > modified {
			modified = e.created
		}
	}
	doc := map[string]any{"name": pkg, "dist-tags": tagMap(vs), "versions": versions, "modified": iso(modified)}
	if !abbrev {
		times["created"], times["modified"] = iso(created), iso(modified)
		doc["_id"] = pkg
		doc["_rev"] = rev(vs)
		doc["time"] = times
	}
	return json.Marshal(doc)
}

// send writes a document with its ETag, and answers an If-None-Match that has it.
func send(w http.ResponseWriter, r *http.Request, body []byte, abbrev bool, extra http.Header) {
	h := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(h[:]) + `"`
	ct := mimeJSON
	if abbrev {
		ct = mimeAbbrev
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept")
	for k, v := range extra {
		w.Header()[k] = v
	}
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

func (p *NPM) hostedDoc(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, t.pkg) {
		return
	}
	vs, err := p.load(r.Context(), q.Repo.Name, t.pkg)
	if err != nil {
		p.unavailable(w)
		return
	}
	if len(vs) == 0 {
		fail(w, http.StatusNotFound, "no such package")
		return
	}
	if t.kind == kTags {
		b, _ := json.Marshal(tagMap(vs))
		send(w, r, b, false, nil)
		return
	}
	abbrev := wantsAbbrev(r)
	b, err := document(t.pkg, base(r, q), vs, abbrev)
	if err != nil {
		p.unavailable(w)
		return
	}
	send(w, r, b, abbrev, nil)
}

// login is `npm login` (and `npm adduser`): PUT -/user/org.couchdb.user:<name> with {"name", "password"}. The password is checked, and the answer has a token, which npm writes to .npmrc. There is no sign
// up: an account is made by an admin (docs/spec.md, A5).
func (p *NPM) login(w http.ResponseWriter, r *http.Request, q format.Request) {
	user, ok := strings.CutPrefix(q.Path, "-/user/org.couchdb.user:")
	if r.Method != http.MethodPut || !ok || user == "" || q.Login == nil {
		fail(w, http.StatusNotImplemented, "this registry takes npm login as PUT -/user/org.couchdb.user:<name>; an account is made by an admin")
		return
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.Name != user {
		fail(w, http.StatusBadRequest, "the body is {\"name\": ..., \"password\": ...} and the name is that of the address")
		return
	}
	tok, err := q.Login(r.Context(), body.Name, body.Password, "npm login")
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		ok201 := map[string]any{"ok": true, "id": "org.couchdb.user:" + user, "token": tok}
		w.Header().Set("Content-Type", mimeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ok201)
	case errors.Is(err, auth.ErrInvalid):
		fail(w, http.StatusUnauthorized, "the account or the password is not right")
	case errors.Is(err, auth.ErrLocked), errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "30")
		fail(w, http.StatusTooManyRequests, "too many logins: try again later")
	default:
		if strings.Contains(err.Error(), "too many failed logins") {
			w.Header().Set("Retry-After", "30")
			fail(w, http.StatusTooManyRequests, "too many logins: try again later")
			return
		}
		p.unavailable(w)
	}
}
