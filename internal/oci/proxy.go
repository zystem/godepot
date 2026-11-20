package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/upstream"
)

// errMissing is what the leader of a fetch tells the ones that wait: the upstream has no such thing.
var errMissing = errors.New("the upstream has none")

// errChanged: the upstream gave other bytes than the digest it was asked for names (docs/spec.md, P5).
var errChanged = errors.New("the upstream gave other bytes than the digest names")

const acceptManifests = mtOCIManifest + ", " + mtOCIIndex + ", " + mtDockerManifest + ", " + mtDockerList

func (h *OCI) client(l *settings.Repo) (*upstream.Client, error) {
	if h.Up == nil || l.Upstream == nil {
		return nil, errors.New("this repository has no upstream")
	}
	return h.Up.Client(l)
}

// group answers a request sent to a group: a write goes to write_to, a read to the member that has the image (docs/spec.md, R1, R2, R3, T20).
func (h *OCI) group(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		m := q.Set.Repo(q.Repo.WriteTo)
		if m == nil {
			h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "this group has no write_to: push to a hosted repository, or name one as write_to of the group", nil)
			return
		}
		q.Repo = m
		h.Serve(w, r, q)
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	m, err := format.Resolve(ctx, h.DB, q.Set, q.Repo, rt.image, func(l *settings.Repo) (bool, error) { return h.hasName(ctx, l, rt.image) })
	if err != nil {
		h.upstreamFailed(w, err)
		return
	}
	if m == nil {
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "no member of this group has this name", nil)
		return
	}
	q.Repo = m
	h.Serve(w, r, q)
}

// hasName says whether a member has the image: a hosted repository when it holds anything of it, a proxy when its cache does or, if not, when the upstream lists it.
func (h *OCI) hasName(ctx context.Context, l *settings.Repo, image string) (bool, error) {
	ok, err := h.exists(ctx, l.Name, image)
	if err != nil || ok || l.Kind != "proxy" {
		return ok, err
	}
	cl, err := h.client(l)
	if err != nil {
		return false, err
	}
	if m, err := cl.Missed(ctx, "name:"+image); err != nil || m {
		return false, err
	}
	resp, err := cl.Do(ctx, http.MethodGet, "/v2/"+image+"/tags/list?n=1", http.Header{"Accept": {"application/json"}})
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if upstream.Missing(resp.StatusCode) {
		cl.Miss(ctx, "name:"+image)
	}
	return false, nil
}

// upstreamFailed answers a request that the upstream could not serve.
func (h *OCI) upstreamFailed(w http.ResponseWriter, err error) {
	var paused *upstream.PausedError
	var failed *upstream.FailError
	switch {
	case errors.As(err, &paused):
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(paused.Until).Seconds())+1))
		h.count(h.Refused, "upstream")
		h.fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the upstream cannot be asked now: try again", map[string]string{"detail": err.Error()})
	case errors.As(err, &failed):
		ra := int(failed.RetryAfter.Seconds())
		if ra == 0 {
			ra = int(upstream.MinPause.Seconds())
		}
		w.Header().Set("Retry-After", strconv.Itoa(ra))
		h.count(h.Refused, "upstream")
		h.fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the upstream failed: try again", map[string]string{"detail": err.Error()})
	case errors.Is(err, upstream.ErrCredentials), errors.Is(err, upstream.ErrPrivate), errors.Is(err, errChanged):
		h.count(h.Refused, "upstream")
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", "the upstream cannot be used: "+err.Error(), nil)
	default:
		h.unavailable(w, err)
	}
}

func (h *OCI) stale(w http.ResponseWriter, repo string) {
	w.Header().Set("Warning", `110 godepot "Response is stale"`)
	if h.Up != nil {
		h.Up.Stale(repo)
	}
}

// proxy answers a request for a proxy repository: a read is the cache or a fetch from the upstream; nothing else is allowed.
func (h *OCI) proxy(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "a proxy repository is read-only: push to a hosted repository, or to a group that has write_to", nil)
		return
	}
	switch rt.kind {
	case kManifest:
		h.proxyManifest(w, r, q, rt)
	case kBlob:
		h.proxyBlob(w, r, q, rt)
	case kTags:
		h.proxyTags(w, r, q, rt)
	case kReferrers:
		h.referrers(w, r, q, rt) // from the cache: the referrers API of the upstream is not asked
	default:
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "a proxy repository is read-only", nil)
	}
}

func (h *OCI) proxyManifest(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	digest, tag, ok := reference(w, h, rt.ref, false)
	if !ok {
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	cl, err := h.client(q.Repo)
	if err != nil {
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", err.Error(), nil)
		return
	}
	repo := q.Repo.Name
	// What is in the cache and is still trusted is served with no word to the upstream: a manifest by its digest for ever, a tag for the time of the settings (P3).
	cached, fresh := "", false
	if tag == "" {
		rows, err := h.DB.Query(ctx, meta.S(`SELECT 1 FROM oci_manifests WHERE repo = ? AND image = ? AND digest = ?`, repo, rt.image, digest))
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if len(rows) > 0 {
			h.getManifest(w, r, q, rt)
			return
		}
	} else {
		rows, err := h.DB.Query(ctx, meta.S(`SELECT digest, updated_at FROM oci_tags WHERE repo = ? AND image = ? AND tag = ?`, repo, rt.image, tag))
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if len(rows) > 0 {
			cached = meta.Str(rows[0][0])
			fresh = h.now()-meta.Int(rows[0][1]) < int64(q.Repo.Upstream.TagTTL()/time.Second)
		}
		if fresh {
			h.getManifest(w, r, q, rt)
			return
		}
	}
	name := "manifest:" + rt.image + "@" + rt.ref
	if cached == "" {
		if m, err := cl.Missed(ctx, name); err != nil {
			h.unavailable(w, err)
			return
		} else if m {
			h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "the upstream has no such manifest (it said so lately)", nil)
			return
		}
	}
	leader, end, wait := h.Up.Flights.Join("m:" + repo + "/" + rt.image + "/" + rt.ref)
	var ferr error
	if leader {
		ferr = h.pullManifest(ctx, q, cl, rt.image, rt.ref, tag, cached)
		end(ferr)
	} else {
		ferr = wait(ctx)
		if ctx.Err() != nil {
			return
		}
	}
	switch {
	case ferr == nil:
		h.getManifest(w, r, q, rt)
	case errors.Is(ferr, errMissing):
		h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "the upstream has no such manifest", nil)
	case cached != "" && !errors.Is(ferr, errChanged):
		h.stale(w, repo) // P3: the upstream cannot answer, and the tag is served as it was
		h.getManifest(w, r, q, rt)
	default:
		h.upstreamFailed(w, ferr)
	}
}

// pullManifest brings a manifest into the cache: for a tag, it first asks the upstream with HEAD whether the tag still names what the cache has; it takes the manifest by its digest, checks the digest,
// and in one transaction stores it and moves the tag.
func (h *OCI) pullManifest(ctx context.Context, q format.Request, cl *upstream.Client, image, ref, tag, cached string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	repo := q.Repo.Name
	hd := http.Header{"Accept": {acceptManifests}}
	if cached != "" {
		// A tag that the cache has: HEAD says what it names now (a HEAD costs the upstream's rate limit less than a GET, T15).
		resp, err := cl.Do(ctx, http.MethodHead, "/v2/"+image+"/manifests/"+ref, hd)
		if err != nil {
			return err
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			if d, ok := ValidDigest(resp.Header.Get("Docker-Content-Digest")); ok && d == cached {
				h.touchTag(ctx, repo, image, tag)
				return nil
			}
		case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
			h.touchTag(ctx, repo, image, tag) // the upstream dropped the tag: the cache keeps what it has
			return nil
		default:
			return fmt.Errorf("the upstream answered %d", resp.StatusCode)
		}
	}
	resp, err := cl.Do(ctx, http.MethodGet, "/v2/"+image+"/manifests/"+ref, hd)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		cl.Miss(ctx, "manifest:"+image+"@"+ref)
		return errMissing
	default:
		return fmt.Errorf("the upstream answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, h.maxManifest()+1))
	if err != nil {
		cl.Failed(ctx, err)
		return &upstream.FailError{Err: err}
	}
	if int64(len(body)) > h.maxManifest() {
		return fmt.Errorf("the manifest is over %d bytes", h.maxManifest())
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if tag == "" && ref != "sha256:"+digest {
		if h.Up != nil {
			h.Up.Changed(repo)
		}
		h.Audit.Record(ctx, audit.Event{Actor: "proxy", Action: "upstream_changed", Repo: repo, Path: image + "/manifests/" + ref, Result: audit.Refused, Detail: "the upstream gave bytes of sha256:" + digest + " for this digest"})
		return errChanged
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return errors.New("the upstream sent a manifest with no media type")
	}
	return h.storeFetched(ctx, q, image, digest, mt, body, tag)
}

func (h *OCI) touchTag(ctx context.Context, repo, image, tag string) {
	if tag != "" {
		_, _ = h.DB.Exec(ctx, meta.S(`UPDATE oci_tags SET updated_at = ? WHERE repo = ? AND image = ? AND tag = ?`, h.now(), repo, image, tag))
	}
}

// storeFetched keeps a manifest that came from the upstream, and its tag. What it names is not checked to be in the cache: it is fetched when a client asks for it.
func (h *OCI) storeFetched(ctx context.Context, q format.Request, image, digest, mt string, body []byte, tag string) error {
	var m manifest
	if !utf8.Valid(body) || json.Unmarshal(body, &m) != nil {
		return errors.New("the upstream sent a manifest that is not JSON")
	}
	blobs, children, why := h.check(mt, &m)
	if why != "" {
		return errors.New("the upstream sent a manifest that is not taken: " + why)
	}
	repo := q.Repo.Name
	var up *blob.Upload
	var stored any
	if len(body) > inlineManifest {
		var err error
		if up, err = h.Blobs.Ingest(ctx, strings.NewReader(string(body)), digest); err != nil {
			return err
		}
	} else {
		stored = string(body)
	}
	art := m.ArtifactType
	if art == "" && m.Config != nil {
		art = m.Config.MediaType
	}
	subject := ""
	if m.Subject != nil {
		subject, _ = ValidDigest(m.Subject.Digest)
	}
	ann := ""
	if len(m.Annotations) > 0 {
		b, _ := json.Marshal(m.Annotations)
		ann = string(b)
	}
	stmts := []meta.Stmt{meta.S(`INSERT OR IGNORE INTO oci_manifests (repo, image, digest, media_type, size, body, subject, artifact_type, annotations, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, repo, image, digest, mt, int64(len(body)), stored, subject, art, ann, "proxy", h.now())}
	for _, d := range blobs {
		stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_links (repo, image, from_digest, to_digest, kind) VALUES (?, ?, ?, ?, 'blob')`, repo, image, digest, d))
	}
	for _, d := range children {
		stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_links (repo, image, from_digest, to_digest, kind) VALUES (?, ?, ?, ?, 'child')`, repo, image, digest, d))
	}
	if tag != "" {
		stmts = append(stmts, meta.S(`INSERT INTO oci_tags (repo, image, tag, digest, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (repo, image, tag) DO UPDATE SET digest = excluded.digest, updated_at = excluded.updated_at`, repo, image, tag, digest, h.now()))
	}
	if up != nil {
		stmts = append(stmts, h.Blobs.Hold(manifestOwner(repo, image, digest), digest)...)
		stmts = append(stmts, h.Blobs.Done(up)...)
	}
	ev := audit.Event{Actor: "proxy", Action: "fetch", Repo: repo, Path: image + "/manifests/" + digest, Result: audit.OK, Detail: fmt.Sprintf("sha256:%s %d bytes %s", digest, len(body), mt)}
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if up != nil {
			_ = h.Blobs.Abandon(ctx, up)
		}
		return err
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, repo)
	return nil
}

func (h *OCI) proxyBlob(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	hx, ok := ValidDigest(rt.ref)
	if !ok {
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the digest is not sha256: and 64 hex digits", nil)
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	repo := q.Repo.Name
	if _, found, err := h.linked(ctx, repo, rt.image, hx); err != nil {
		h.unavailable(w, err)
		return
	} else if found {
		h.getBlob(w, r, q, rt) // a blob is named by its digest: once fetched it is never asked again (P3)
		return
	}
	cl, err := h.client(q.Repo)
	if err != nil {
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", err.Error(), nil)
		return
	}
	name := "blob:" + rt.image + "@" + hx
	if m, err := cl.Missed(ctx, name); err != nil {
		h.unavailable(w, err)
		return
	} else if m {
		h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "the upstream has no such blob (it said so lately)", nil)
		return
	}
	if r.Method == http.MethodHead {
		h.proxyBlobHead(w, r, cl, rt, hx, name)
		return
	}
	leader, end, wait := h.Up.Flights.Join("b:" + repo + "/" + rt.image + "@" + hx)
	if !leader {
		// P2: one fetch, many readers: this one waits for the leader and then reads from the cache.
		err := wait(ctx)
		switch {
		case ctx.Err() != nil:
		case err == nil:
			if _, found, _ := h.linked(ctx, repo, rt.image, hx); found {
				h.getBlob(w, r, q, rt)
				return
			}
			h.fail(w, http.StatusBadGateway, "UNAVAILABLE", "the fetch of this blob did not end well: try again", nil)
		case errors.Is(err, errMissing):
			h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "the upstream has no such blob", nil)
		default:
			h.upstreamFailed(w, err)
		}
		return
	}
	ferr, abort := h.fetchBlob(w, r, q, cl, rt, hx, name)
	end(ferr)
	if abort {
		panic(http.ErrAbortHandler) // the status has been sent: the only way to tell the client is to cut the connection
	}
}

func (h *OCI) proxyBlobHead(w http.ResponseWriter, r *http.Request, cl *upstream.Client, rt route, hx, name string) {
	resp, err := cl.Do(r.Context(), http.MethodHead, "/v2/"+rt.image+"/blobs/sha256:"+hx, nil)
	if err != nil {
		h.upstreamFailed(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if upstream.Missing(resp.StatusCode) {
			cl.Miss(r.Context(), name)
		}
		h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "the upstream has no such blob", nil)
		return
	}
	hd := w.Header()
	hd.Set("Docker-Content-Digest", rt.ref)
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("X-Content-Type-Options", "nosniff")
	if resp.ContentLength >= 0 {
		hd.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// fetchBlob takes a blob from the upstream and sends it to the client as it comes, keeping it (docs/spec.md, P1). The digest is the one of the address: other bytes are not kept and are not let end
// well (P5). It returns what the ones that wait are told, and whether the connection must be cut.
func (h *OCI) fetchBlob(w http.ResponseWriter, r *http.Request, q format.Request, cl *upstream.Client, rt route, hx, name string) (ferr error, abort bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), format.FetchTimeout)
	defer cancel()
	resp, err := cl.Do(ctx, http.MethodGet, "/v2/"+rt.image+"/blobs/sha256:"+hx, nil)
	if err != nil {
		h.upstreamFailed(w, err)
		return err, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		cl.Miss(ctx, name)
		h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "the upstream has no such blob", nil)
		return errMissing, false
	default:
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", fmt.Sprintf("the upstream answered %d", resp.StatusCode), nil)
		return fmt.Errorf("the upstream answered %d", resp.StatusCode), false
	}
	release, ok := h.Downloads.Acquire(ctx)
	if !ok {
		h.count(h.Refused, "busy")
		w.Header().Set("Retry-After", "5")
		h.fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "too many downloads are going on: try again", nil)
		return errors.New("busy"), false
	}
	h.active()
	defer func() { release(); h.active() }()

	hd := w.Header()
	hd.Set("Docker-Content-Digest", rt.ref)
	hd.Set("ETag", `"`+rt.ref+`"`)
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("X-Content-Type-Options", "nosniff")
	if resp.ContentLength >= 0 {
		hd.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	src := &format.ReadErr{R: resp.Body}
	tee := format.NewTee(w, h.Rate, func() { w.WriteHeader(http.StatusOK) })
	up, err := format.Through(ctx, h.Blobs, src, hx, tee)
	if err != nil {
		if src.Err != nil {
			cl.Failed(ctx, src.Err)
		}
		if errors.Is(err, blob.ErrDigest) {
			if h.Up != nil {
				h.Up.Changed(q.Repo.Name)
			}
			h.Audit.Record(ctx, audit.Event{Actor: "proxy", Action: "upstream_changed", Repo: q.Repo.Name, Path: rt.image + "/blobs/" + rt.ref, Result: audit.Refused, Detail: "the upstream gave other bytes than the digest names"})
			err = errChanged
		}
		if tee.Started() {
			return err, true
		}
		hd.Del("Content-Length")
		h.upstreamFailed(w, err)
		return err, false
	}
	repo := q.Repo.Name
	stmts := h.Blobs.Hold(blobOwner(repo, rt.image, hx), hx)
	stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_blobs (repo, image, digest, size, created_at) VALUES (?, ?, ?, ?, ?)`, repo, rt.image, hx, up.Size, h.now()))
	stmts = append(stmts, h.Blobs.Done(up)...)
	ev := audit.Event{Actor: "proxy", Action: "fetch", Repo: repo, Path: rt.image + "/blobs/" + rt.ref, Result: audit.OK, Detail: fmt.Sprintf("%d bytes", up.Size)}
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		_ = h.Blobs.Abandon(ctx, up) // not cached; the client has its bytes
		return nil, false
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, repo)
	return nil, false
}

// proxyTags answers the list of tags with the upstream's own list, and from the cache when the upstream cannot answer.
func (h *OCI) proxyTags(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	cl, err := h.client(q.Repo)
	if err != nil {
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", err.Error(), nil)
		return
	}
	qs := url.Values{}
	for _, k := range []string{"n", "last"} {
		if v := r.URL.Query().Get(k); v != "" {
			qs.Set(k, v)
		}
	}
	path := "/v2/" + rt.image + "/tags/list"
	if len(qs) > 0 {
		path += "?" + qs.Encode()
	}
	resp, err := cl.Do(ctx, http.MethodGet, path, http.Header{"Accept": {"application/json"}})
	if err != nil {
		var u *upstream.PausedError
		var f *upstream.FailError
		if errors.As(err, &u) || errors.As(err, &f) {
			if have, _ := h.exists(ctx, q.Repo.Name, rt.image); have {
				h.stale(w, q.Repo.Name)
				h.tags(w, r, q, rt)
				return
			}
		}
		h.upstreamFailed(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "the upstream has no such name", nil)
		return
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		h.fail(w, http.StatusBadGateway, "UNAVAILABLE", "the upstream sent a list of tags that is not JSON", nil)
		return
	}
	if list.Tags == nil {
		list.Tags = []string{}
	}
	if l := resp.Header.Get("Link"); l != "" {
		w.Header().Set("Link", strings.Replace(l, "/v2/"+rt.image+"/", "/v2/"+h.address(q)+"/"+rt.image+"/", 1))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": h.address(q) + "/" + rt.image, "tags": list.Tags})
}
