package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
)

// The limits of a publish (docs/spec.md, F5). npm sends the tarball inside the JSON, in base64, so the whole request is read into memory: the limit is what keeps that safe.
const (
	MaxPublish     = 128 << 20
	maxEntries     = 100000
	maxUnpacked    = 2 << 30
	maxPackageJSON = 4 << 20
)

type attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int64  `json:"length"`
}

type doc struct {
	Rev         string                     `json:"_rev"`
	Name        string                     `json:"name"`
	DistTags    map[string]string          `json:"dist-tags"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]attachment      `json:"_attachments"`
}

// inspected is what is read of a tarball.
type inspected struct {
	name, version string
	installScript bool
	shrinkwrap    bool
}

// inspect reads a tarball of npm: a gzip of a tar whose files are under one directory ("package/"), with a package.json in it. The names in it are looked at (no "..", no absolute path): the client that
// unpacks it is not trusted to look.
func inspect(data []byte) (inspected, error) {
	var in inspected
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return in, errors.New("the tarball is not a gzip")
	}
	defer zr.Close()
	tr := tar.NewReader(io.LimitReader(zr, maxUnpacked+1))
	top := ""
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return in, errors.New("the tarball is not a tar")
		}
		if n >= maxEntries {
			return in, fmt.Errorf("the tarball has more than %d entries", maxEntries)
		}
		name := h.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "\x00") {
			return in, fmt.Errorf("the name %q is not allowed in a tarball", name)
		}
		for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
			if part == ".." {
				return in, fmt.Errorf("the name %q is not allowed in a tarball", name)
			}
		}
		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
			if strings.HasPrefix(h.Linkname, "/") || strings.Contains(h.Linkname, "..") {
				return in, fmt.Errorf("the link %q is not allowed in a tarball", name)
			}
		}
		first, rest, _ := strings.Cut(strings.TrimPrefix(name, "./"), "/")
		if top == "" {
			top = first
		}
		total += h.Size
		if total > maxUnpacked {
			return in, errors.New("the tarball is too large unpacked")
		}
		switch {
		case first == top && rest == "package.json" && h.Typeflag == tar.TypeReg:
			if h.Size > maxPackageJSON {
				return in, errors.New("the package.json is too large")
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxPackageJSON+1))
			if err != nil {
				return in, errors.New("the package.json cannot be read")
			}
			var pj struct {
				Name    string            `json:"name"`
				Version string            `json:"version"`
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(b, &pj) != nil {
				return in, errors.New("the package.json is not JSON")
			}
			in.name, in.version = pj.Name, pj.Version
			for _, s := range []string{"preinstall", "install", "postinstall"} {
				if pj.Scripts[s] != "" {
					in.installScript = true
				}
			}
		case first == top && (rest == "npm-shrinkwrap.json"):
			in.shrinkwrap = true
		}
	}
	if in.name == "" {
		return in, errors.New("the tarball has no package/package.json")
	}
	return in, nil
}

// sums are the two hashes npm names of a tarball.
func sums(data []byte) (shasum, integrity string) {
	b := sha512.Sum512(data)
	return sha1Hex(data), "sha512-" + base64.StdEncoding.EncodeToString(b[:])
}

// publish answers a PUT of a packument: `npm publish` (a new version, with its tarball in _attachments), and `npm deprecate` and the changes of tags and of versions that `npm unpublish` makes (no
// tarball). A write that has a revision is checked against the revision there is.
func (p *NPM) publish(w http.ResponseWriter, r *http.Request, q format.Request, pkg, pathRev string) {
	if q.Principal.Anonymous() {
		q.Authorize(w, auth.Write, pkg) // 401, before the body is read
		return
	}
	if !q.Authorize(w, auth.Write, pkg) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPublish))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a publish is at most %d bytes; npm sends the tarball inside the request", MaxPublish))
			return
		}
		fail(w, http.StatusBadRequest, "the request could not be read")
		return
	}
	var d doc
	if err := json.Unmarshal(body, &d); err != nil {
		fail(w, http.StatusBadRequest, "the body is not a packument: JSON")
		return
	}
	if d.Name != pkg {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the name %q is not the package of the address (%s)", d.Name, pkg))
		return
	}
	if len(d.Attachments) == 0 {
		p.change(w, r, q, pkg, pathRev, d)
		return
	}
	if pathRev != "" {
		fail(w, http.StatusBadRequest, "a version comes with its tarball to the address of the package, not to a revision")
		return
	}
	p.add(w, r, q, pkg, d)
}

func (p *NPM) add(w http.ResponseWriter, r *http.Request, q format.Request, pkg string, d doc) {
	ctx := r.Context()
	if !validName(pkg, true) {
		fail(w, http.StatusBadRequest, "a name to publish is lower case, with a-z 0-9 . _ ~ - (and a scope: @scope/name)")
		return
	}
	if len(d.Attachments) != 1 {
		fail(w, http.StatusBadRequest, "one tarball to a publish (a provenance attachment is not taken)")
		return
	}
	var file string
	var at attachment
	for file, at = range d.Attachments {
	}
	// npm names the attachment with the whole name, the scope too ("@corp/demo-1.0.0.tgz"); the file of the tarball has the part after the scope.
	v, found := version(pkg, strings.TrimPrefix(file, strings.TrimSuffix(pkg, unscoped(pkg))))
	if !found || file != pkg+"-"+v+".tgz" {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the attachment %q is not <name>-<version>.tgz of this package", file))
		return
	}
	file = tarball(pkg, v)
	raw, found := d.Versions[v]
	if !found {
		fail(w, http.StatusBadRequest, "the packument has no manifest of the version "+v)
		return
	}
	if len(raw) > maxManifest {
		fail(w, http.StatusRequestEntityTooLarge, "the manifest of a version is at most 4 MiB")
		return
	}
	if len(d.DistTags) != 1 {
		fail(w, http.StatusBadRequest, "name one tag for the version: npm publish --tag <tag> (the default is latest)")
		return
	}
	var tag, tagged string
	for tag, tagged = range d.DistTags {
	}
	if !validTag(tag) || tagged != v {
		fail(w, http.StatusBadRequest, "the tag must be a word that is not a version, and must name the version that is published")
		return
	}
	data, err := base64.StdEncoding.DecodeString(at.Data)
	if err != nil || len(data) == 0 {
		fail(w, http.StatusBadRequest, "the data of the attachment is not base64")
		return
	}
	if at.Length != 0 && at.Length != int64(len(data)) {
		fail(w, http.StatusBadRequest, "the length of the attachment is not that of its data")
		return
	}
	in, err := inspect(data)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.name != pkg || in.version != v {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the package.json in the tarball names %s@%s, the publish names %s@%s", in.name, in.version, pkg, v))
		return
	}
	man := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &man) != nil {
		fail(w, http.StatusBadRequest, "the manifest of the version is not a JSON object")
		return
	}
	var nm, vv string
	_ = json.Unmarshal(man["name"], &nm)
	_ = json.Unmarshal(man["version"], &vv)
	if nm != pkg || vv != v {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the manifest names %s@%s, the publish names %s@%s", nm, vv, pkg, v))
		return
	}
	shasum, integrity := sums(data)
	var dist struct {
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
	}
	_ = json.Unmarshal(man["dist"], &dist)
	if (dist.Shasum != "" && !strings.EqualFold(dist.Shasum, shasum)) || (dist.Integrity != "" && dist.Integrity != integrity) {
		fail(w, http.StatusBadRequest, "the hash in dist is not that of the tarball")
		return
	}
	for k := range man {
		if strings.HasPrefix(k, "_") || k == "deprecated" || k == "readme" {
			delete(man, k) // what the registry names itself, and what is kept apart
		}
	}
	d2, _ := json.Marshal(map[string]string{"shasum": shasum, "integrity": integrity})
	man["dist"] = d2
	if in.installScript {
		man["hasInstallScript"] = json.RawMessage("true")
	}
	if in.shrinkwrap {
		man["_hasShrinkwrap"] = json.RawMessage("true")
	}
	manifest, _ := json.Marshal(man)
	if len(manifest) > maxManifest {
		fail(w, http.StatusRequestEntityTooLarge, "the manifest of a version is at most 4 MiB")
		return
	}

	// A version is never overwritten (docs/spec.md, R4): a registry whose lock files hold hashes must not change what a version is. The same bytes again are fine.
	tpath := pkg + "/-/" + file
	mpath := pkg + "/-/" + v + ".json"
	have, exists, err := p.Lookup(ctx, q.Repo.Name, mpath)
	if err != nil {
		p.unavailable(w)
		return
	}
	if exists {
		tdig, _, err := p.Lookup(ctx, q.Repo.Name, tpath)
		if err != nil {
			p.unavailable(w)
			return
		}
		_ = have
		if want := hex.EncodeToString(sum256(data)); tdig == want {
			ok(w, http.StatusOK, map[string]any{"ok": true, "id": pkg, "note": "the version is there with these bytes"})
		} else {
			fail(w, http.StatusConflict, fmt.Sprintf("you cannot publish over the previously published version %s@%s", pkg, v))
		}
		return
	}
	up, err := p.Blobs.Ingest(ctx, bytes.NewReader(data), "")
	if err != nil {
		p.unavailable(w)
		return
	}
	rec := &quiet{h: http.Header{}, code: 200}
	p.Raw.Commit(rec, ctx, q, tpath, up, "application/octet-stream", "{}")
	if rec.code >= 300 && rec.code != http.StatusConflict {
		if rec.code == http.StatusRequestEntityTooLarge || rec.code == http.StatusInsufficientStorage {
			fail(w, rec.code, "over the quota of the repository")
			return
		}
		p.unavailable(w)
		return
	}
	if rec.code == http.StatusConflict {
		fail(w, http.StatusConflict, fmt.Sprintf("you cannot publish over the previously published version %s@%s", pkg, v))
		return
	}
	mup, err := p.Blobs.Ingest(ctx, bytes.NewReader(manifest), "")
	if err != nil {
		p.unavailable(w)
		return
	}
	props, _ := json.Marshal(map[string]string{"tag." + tag: "1"})
	rec = &quiet{h: http.Header{}, code: 200}
	p.Raw.Commit(rec, ctx, q, mpath, mup, mimeJSON, string(props))
	if rec.code >= 300 {
		p.unavailable(w)
		return
	}
	// The tag names this version only: it is taken off the others.
	prefix := pkg + "/-/"
	_, _ = p.DB.Exec(ctx, meta.S(`UPDATE components SET props = json_remove(props, ?) WHERE repo = ? AND substr(name, 1, ?) = ? AND name <> ? AND name LIKE '%.json'`,
		"$.\"tag."+tag+"\"", q.Repo.Name, len(prefix), prefix, mpath))
	ok(w, http.StatusCreated, map[string]any{"ok": true, "id": pkg})
}

func sum256(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// change takes a packument without a tarball: the deprecation of versions, the tags, and (with a revision, from `npm unpublish`) versions that are gone.
func (p *NPM) change(w http.ResponseWriter, r *http.Request, q format.Request, pkg, pathRev string, d doc) {
	ctx := r.Context()
	vs, err := p.load(ctx, q.Repo.Name, pkg)
	if err != nil {
		p.unavailable(w)
		return
	}
	if len(vs) == 0 {
		fail(w, http.StatusNotFound, "no such package")
		return
	}
	if d.Rev != "" && d.Rev != rev(vs) || pathRev != "" && pathRev != rev(vs) {
		fail(w, http.StatusConflict, "the package changed since it was read: read it again")
		return
	}
	have := map[string]*ver{}
	for _, e := range vs {
		have[e.v] = e
	}
	var gone []string
	for v := range have {
		if _, kept := d.Versions[v]; !kept {
			gone = append(gone, v)
		}
	}
	for v := range d.Versions {
		if have[v] == nil {
			fail(w, http.StatusBadRequest, "a new version comes with its tarball: "+v)
			return
		}
	}
	if len(gone) > 0 {
		if pathRev == "" {
			fail(w, http.StatusBadRequest, "versions are taken away by npm unpublish, which writes to a revision")
			return
		}
		if !q.Authorize(w, auth.Delete, pkg) {
			return
		}
	}
	for t, v := range d.DistTags {
		if !validTag(t) || have[v] == nil || contains(gone, v) {
			fail(w, http.StatusBadRequest, fmt.Sprintf("the tag %q names a version that is not there", t))
			return
		}
	}
	var stmts []meta.Stmt
	cur := tagMap(vs)
	for t := range cur {
		if _, kept := d.DistTags[t]; !kept && !contains(gone, cur[t]) {
			stmts = append(stmts, meta.S(`UPDATE components SET props = json_remove(props, ?) WHERE repo = ? AND name = ?`, "$.\"tag."+t+"\"", q.Repo.Name, pkg+"/-/"+cur[t]+".json"))
		}
	}
	for t, v := range d.DistTags {
		if cur[t] != v {
			stmts = append(stmts, p.moveTag(q.Repo.Name, pkg, t, v)...)
		}
	}
	for v, raw := range d.Versions {
		var m struct {
			Deprecated *string `json:"deprecated"`
		}
		_ = json.Unmarshal(raw, &m)
		msg := ""
		if m.Deprecated != nil {
			msg = *m.Deprecated
		}
		if msg == have[v].deprecated {
			continue
		}
		path := pkg + "/-/" + v + ".json"
		if msg == "" {
			stmts = append(stmts, meta.S(`UPDATE components SET props = json_remove(props, '$.deprecated') WHERE repo = ? AND name = ?`, q.Repo.Name, path))
		} else {
			if len(msg) > 4096 {
				fail(w, http.StatusBadRequest, "a deprecation message is at most 4096 bytes")
				return
			}
			stmts = append(stmts, meta.S(`UPDATE components SET props = json_set(props, '$.deprecated', ?) WHERE repo = ? AND name = ?`, msg, q.Repo.Name, path))
		}
	}
	if len(stmts) > 0 {
		ev := audit.Event{Actor: q.Actor(), Token: q.Principal.Token, Remote: q.Remote, Action: "npm_change", Repo: q.Repo.Name, Path: pkg, Result: audit.OK, Detail: fmt.Sprintf("%d changes, %d versions gone", len(stmts), len(gone))}
		if _, err := p.DB.Exec(ctx, append(stmts, p.Audit.Stmt(ev))...); err != nil {
			p.unavailable(w)
			return
		}
		p.Audit.Emit(ev)
	}
	for _, v := range gone {
		if !p.remove(ctx, r, q, pkg, v) {
			p.unavailable(w)
			return
		}
	}
	ok(w, http.StatusOK, map[string]any{"ok": true, "id": pkg})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// moveTag makes the statements that give a tag to a version and take it off the others.
func (p *NPM) moveTag(repo, pkg, tag, v string) []meta.Stmt {
	prefix := pkg + "/-/"
	key := "$.\"tag." + tag + "\""
	return []meta.Stmt{
		meta.S(`UPDATE components SET props = json_remove(props, ?) WHERE repo = ? AND substr(name, 1, ?) = ? AND name LIKE '%.json'`, key, repo, len(prefix), prefix),
		meta.S(`UPDATE components SET props = json_set(props, ?, '1') WHERE repo = ? AND name = ?`, key, repo, prefix+v+".json"),
	}
}

// tag is PUT and DELETE of -/package/<pkg>/dist-tags/<tag>: `npm dist-tag add` and `rm`. The body of a PUT is the version, a JSON string.
func (p *NPM) tag(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Write, t.pkg) {
		return
	}
	ctx := r.Context()
	vs, err := p.load(ctx, q.Repo.Name, t.pkg)
	if err != nil {
		p.unavailable(w)
		return
	}
	if len(vs) == 0 {
		fail(w, http.StatusNotFound, "no such package")
		return
	}
	var stmts []meta.Stmt
	if r.Method == http.MethodPut {
		var v string
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&v); err != nil || !validVersion(v) {
			fail(w, http.StatusBadRequest, "the body is the version, a JSON string")
			return
		}
		found := false
		for _, e := range vs {
			found = found || e.v == v
		}
		if !found {
			fail(w, http.StatusNotFound, "no such version")
			return
		}
		stmts = p.moveTag(q.Repo.Name, t.pkg, t.file, v)
	} else {
		if t.file == "latest" {
			fail(w, http.StatusBadRequest, "the tag latest is not removed: move it")
			return
		}
		if _, there := tagMap(vs)[t.file]; !there {
			fail(w, http.StatusNotFound, "no such tag")
			return
		}
		prefix := t.pkg + "/-/"
		stmts = []meta.Stmt{meta.S(`UPDATE components SET props = json_remove(props, ?) WHERE repo = ? AND substr(name, 1, ?) = ? AND name LIKE '%.json'`, "$.\"tag."+t.file+"\"", q.Repo.Name, len(prefix), prefix)}
	}
	ev := audit.Event{Actor: q.Actor(), Token: q.Principal.Token, Remote: q.Remote, Action: "npm_tag", Repo: q.Repo.Name, Path: t.pkg, Result: audit.OK, Detail: r.Method + " " + t.file}
	if _, err := p.DB.Exec(ctx, append(stmts, p.Audit.Stmt(ev))...); err != nil {
		p.unavailable(w)
		return
	}
	p.Audit.Emit(ev)
	ok(w, http.StatusOK, map[string]any{"ok": true, "id": t.pkg})
}

// unpublish is DELETE of <pkg>/-rev/<rev> (the whole package) and of <pkg>/-/<file>/-rev/<rev> (the tarball of a version, which npm unpublish sends after the packument without it). The claim of the
// name stays (docs/proxy.md): the name is not given to the upstream.
func (p *NPM) unpublish(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Delete, t.pkg) {
		return
	}
	ctx := r.Context()
	vs, err := p.load(ctx, q.Repo.Name, t.pkg)
	if err != nil {
		p.unavailable(w)
		return
	}
	if t.kind == kTarRev {
		v, _ := version(t.pkg, t.file)
		if !p.remove(ctx, r, q, t.pkg, v) {
			p.unavailable(w)
			return
		}
		ok(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(vs) == 0 {
		fail(w, http.StatusNotFound, "no such package")
		return
	}
	if t.rev != rev(vs) {
		fail(w, http.StatusConflict, "the package changed since it was read: read it again")
		return
	}
	for _, e := range vs {
		if !p.remove(ctx, r, q, t.pkg, e.v) {
			p.unavailable(w)
			return
		}
	}
	ok(w, http.StatusOK, map[string]any{"ok": true})
}

// remove deletes a version: its manifest and its tarball. It is true when both are gone (or were).
func (p *NPM) remove(ctx context.Context, r *http.Request, q format.Request, pkg, v string) bool {
	for _, path := range []string{pkg + "/-/" + v + ".json", pkg + "/-/" + tarball(pkg, v)} {
		qq := q
		qq.Path = path
		qq.Authorize = func(http.ResponseWriter, string, string) bool { return true } // authorized for the package
		rr := r.Clone(ctx)
		rr.Method = http.MethodDelete
		rec := &quiet{h: http.Header{}, code: 200}
		p.Raw.Serve(rec, rr, qq)
		if rec.code != http.StatusNoContent && rec.code != http.StatusNotFound {
			return false
		}
	}
	return true
}

// quiet takes an answer that is not for a client.
type quiet struct {
	h    http.Header
	code int
}

func (q *quiet) Header() http.Header         { return q.h }
func (q *quiet) WriteHeader(c int)           { q.code = c }
func (q *quiet) Write(b []byte) (int, error) { return len(b), nil }
