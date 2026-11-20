package raw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/upstream"
)

// errMissing is what the leader of a fetch tells the ones that wait: the upstream has no such file.
var errMissing = errors.New("the upstream has no such file")

// group answers a request sent to a group: a write goes to write_to, a read to the member that has the name (docs/spec.md, R1, R2, R3, T20). The answer is the member's; the audit names the member.
func (h *Raw) group(w http.ResponseWriter, r *http.Request, q format.Request) {
	switch r.Method {
	case http.MethodPut, http.MethodDelete:
		m := q.Set.Repo(q.Repo.WriteTo)
		if m == nil {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "this group has no write_to: write to a hosted repository, or name one as write_to of the group", nil)
			return
		}
		q.Repo = m
		h.self().Serve(w, r, q)
	case http.MethodGet, http.MethodHead:
		if q.Path == "" || strings.HasSuffix(q.Path, "/") {
			h.groupList(w, r, q)
			return
		}
		if err := CheckPath(q.Path); err != nil {
			fail(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		if !q.Authorize(w, auth.Read, q.Path) {
			return
		}
		m, err := format.Resolve(r.Context(), h.DB, q.Set, q.Repo, h.name(q.Path), func(l *settings.Repo) (bool, error) { return h.has(r.Context(), l, q.Path) })
		if err != nil {
			h.upstreamError(w, r, q, nil, err)
			return
		}
		if m == nil {
			fail(w, http.StatusNotFound, "no such file", nil)
			return
		}
		q.Repo = m
		h.self().Serve(w, r, q)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

// has says whether a member has the file: a hosted repository by its records, a proxy by its cache or, when the cache lacks it, by asking the upstream.
func (h *Raw) has(ctx context.Context, l *settings.Repo, path string) (bool, error) {
	name := h.name(path)
	var a *asset
	var err error
	if name == path {
		a, err = h.find(ctx, l.Name, path)
	} else {
		// A name that is more than one file (a module, an artifact): the repository has it when it holds a file below it.
		var rows meta.Rows
		rows, err = h.DB.Query(ctx, meta.S(`SELECT 1 FROM assets WHERE repo = ? AND substr(path, 1, ?) = ? LIMIT 1`, l.Name, len(name)+1, name+"/"))
		if len(rows) > 0 {
			a = &asset{}
		}
	}
	if err != nil || a != nil || l.Kind != "proxy" {
		return a != nil, err
	}
	cl, err := h.client(l)
	if err != nil {
		return false, err
	}
	if m, err := cl.Missed(ctx, "raw:"+name); err != nil || m {
		return false, err
	}
	probe := path
	if h.Layout != nil && h.Layout.Probe != nil {
		probe = h.Layout.Probe(path)
	}
	method := http.MethodHead // for a file; a name that is more than a file is probed with a GET of a small page (a list of versions)
	if probe != path || name != path {
		method = http.MethodGet
	}
	resp, err := cl.Do(ctx, method, "/"+escape(probe), nil)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if upstream.Missing(resp.StatusCode) {
		cl.Miss(ctx, "raw:"+name)
	}
	return false, nil
}

// groupList answers a GET of a directory of a group with the listing of the first hosted member that has files under it. A proxy has no listing to give.
func (h *Raw) groupList(w http.ResponseWriter, r *http.Request, q format.Request) {
	prefix := q.Path
	var first *settings.Repo
	for _, l := range q.Set.Leaves(q.Repo.Name) {
		if l.Kind != "hosted" {
			continue
		}
		if first == nil {
			first = l
		}
		rows, err := h.DB.Query(r.Context(), meta.S(`SELECT 1 FROM assets WHERE repo = ? AND substr(path, 1, ?) = ? LIMIT 1`, l.Name, len(prefix), prefix))
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if len(rows) > 0 {
			q.Repo = l
			h.list(w, r, q)
			return
		}
	}
	if first == nil {
		if !q.Authorize(w, auth.Read, prefix) {
			return
		}
		reply(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	q.Repo = first
	h.list(w, r, q)
}

func (h *Raw) client(l *settings.Repo) (*upstream.Client, error) {
	if h.Up == nil || l.Upstream == nil {
		return nil, errors.New("this repository has no upstream")
	}
	return h.Up.Client(l)
}

func escape(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(p), "%21", "!") // "!" is how a Go module path escapes a capital: an upstream may not decode it
	}
	return strings.Join(parts, "/")
}

// proxy answers a request for a proxy repository: a read is a file of the cache or a fetch from the upstream; nothing else is allowed.
func (h *Raw) proxy(w http.ResponseWriter, r *http.Request, q format.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "a proxy repository is read-only: write to a hosted repository, or to a group that has write_to", nil)
		return
	}
	if q.Path == "" || strings.HasSuffix(q.Path, "/") {
		fail(w, http.StatusNotFound, "a proxy repository has no listing: ask for a file", nil)
		return
	}
	h.proxyGet(w, r, q)
}

func (h *Raw) proxyGet(w http.ResponseWriter, r *http.Request, q format.Request) {
	path := q.Path
	if err := CheckPath(path); err != nil {
		fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if !q.Authorize(w, auth.Read, path) {
		return
	}
	ctx := r.Context()
	cl, err := h.client(q.Repo)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error(), nil)
		return
	}
	old, err := h.find(ctx, q.Repo.Name, path)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	ttl := h.ttl(q.Repo.Upstream, path)
	if old != nil && (ttl == 0 || h.nowUnix()-old.checked < int64(ttl/time.Second)) {
		h.get(w, r, q) // what is in the cache and is still trusted: no word with the upstream (P3)
		return
	}
	if old == nil {
		if m, err := cl.Missed(ctx, "raw:"+path); err != nil {
			h.unavailable(w, err)
			return
		} else if m {
			fail(w, http.StatusNotFound, "no such file (the upstream said so lately)", nil)
			return
		}
		if r.Method == http.MethodHead {
			h.proxyHead(w, r, q, cl)
			return
		}
	}
	leader, end, wait := h.Up.Flights.Join(q.Repo.Name + "/" + path)
	if !leader {
		// P2: one fetch, many readers. This one waits for the leader and then reads what it stored.
		err := wait(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if a, _ := h.find(ctx, q.Repo.Name, path); a != nil {
				h.get(w, r, q)
				return
			}
			fail(w, http.StatusBadGateway, "the fetch of this file did not end well: try again", nil)
		case errors.Is(err, errMissing):
			fail(w, http.StatusNotFound, "no such file", nil)
		default:
			h.upstreamError(w, r, q, old, err)
		}
		return
	}
	ferr, abort := h.fetch(w, r, q, cl, old)
	end(ferr)
	if abort {
		panic(http.ErrAbortHandler) // the status has been sent: the only way to tell the client is to cut the connection
	}
}

// proxyHead answers a HEAD for a file that is not in the cache by asking the upstream for the head, and storing nothing.
func (h *Raw) proxyHead(w http.ResponseWriter, r *http.Request, q format.Request, cl *upstream.Client) {
	resp, err := cl.Do(r.Context(), http.MethodHead, "/"+escape(q.Path), nil)
	if err != nil {
		h.upstreamError(w, r, q, nil, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if upstream.Missing(resp.StatusCode) {
			cl.Miss(r.Context(), "raw:"+q.Path)
			fail(w, http.StatusNotFound, "no such file", nil)
			return
		}
		fail(w, http.StatusBadGateway, fmt.Sprintf("the upstream answered %d", resp.StatusCode), nil)
		return
	}
	for _, k := range []string{"Content-Type", "ETag", "Content-Length", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// upstreamError answers a request that the upstream could not serve: from the cache when there is a copy (marked as stale, P3), else with the error.
func (h *Raw) upstreamError(w http.ResponseWriter, r *http.Request, q format.Request, old *asset, err error) {
	var paused *upstream.PausedError
	var failed *upstream.FailError
	retry := 0
	switch {
	case errors.As(err, &paused):
		retry = int(time.Until(paused.Until).Seconds()) + 1
	case errors.As(err, &failed):
		retry = int(failed.RetryAfter.Seconds())
		if retry == 0 {
			retry = int(upstream.MinPause.Seconds())
		}
	case errors.Is(err, upstream.ErrCredentials), errors.Is(err, upstream.ErrPrivate):
	default:
		h.unavailable(w, err)
		return
	}
	if old != nil && q.Repo.Kind == "proxy" {
		w.Header().Set("Warning", `110 godepot "Response is stale"`)
		if h.Up != nil {
			h.Up.Stale(q.Repo.Name)
		}
		h.get(w, r, q)
		return
	}
	h.count(h.Refused, "upstream")
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		fail(w, http.StatusServiceUnavailable, "the upstream cannot be asked now: try again", map[string]any{"detail": err.Error()})
		return
	}
	fail(w, http.StatusBadGateway, "the upstream cannot be used: "+err.Error(), nil)
}

// fetch asks the upstream for the file and answers the client with what comes. It returns what the ones that wait for it are told, and whether the connection must be cut (the status was sent and the
// bytes failed).
func (h *Raw) fetch(w http.ResponseWriter, r *http.Request, q format.Request, cl *upstream.Client, old *asset) (ferr error, abort bool) {
	path := q.Path
	// The fetch is not the client's: if the client goes away it goes on, for the cache and for the ones that wait.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), format.FetchTimeout)
	defer cancel()
	hd := http.Header{}
	if old != nil && old.etag != "" {
		hd.Set("If-None-Match", old.etag)
	}
	resp, err := cl.Do(ctx, http.MethodGet, "/"+escape(path), hd)
	if err != nil {
		h.upstreamError(w, r, q, old, err)
		return err, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		h.touch(ctx, old)
		h.get(w, r, q)
		return nil, false
	case http.StatusNotFound, http.StatusGone, http.StatusUnauthorized, http.StatusForbidden:
		if old != nil { // the upstream dropped what we have: what we have is kept
			h.touch(ctx, old)
			h.get(w, r, q)
			return nil, false
		}
		cl.Miss(ctx, "raw:"+path)
		fail(w, http.StatusNotFound, "no such file", nil)
		return errMissing, false
	case http.StatusOK:
	default:
		fail(w, http.StatusBadGateway, fmt.Sprintf("the upstream answered %d", resp.StatusCode), nil)
		return fmt.Errorf("the upstream answered %d", resp.StatusCode), false
	}
	want := strings.ToLower(resp.Header.Get("X-Checksum-Sha256"))
	if !blob.ValidDigest(want) {
		want = ""
	}
	etag := resp.Header.Get("ETag")
	src := &format.ReadErr{R: resp.Body}

	if old != nil && q.Repo.Upstream.Immutable(path) {
		// P5: a file that cannot change. Its bytes are read to see whether they are the same; the cached bytes stay either way.
		up, err := h.Blobs.Ingest(ctx, src, want)
		if err != nil {
			h.fetchFailed(ctx, cl, src, err)
			h.upstreamError(w, r, q, old, &upstream.FailError{Err: err})
			return err, false
		}
		_ = h.Blobs.Abandon(ctx, up)
		if up.Digest != old.digest {
			if h.Up != nil {
				h.Up.Changed(q.Repo.Name)
			}
			h.Audit.Record(ctx, h.event(q, "upstream_changed", path, audit.Refused, fmt.Sprintf("the upstream gave %s for a file that cannot change; the cached %s stays", up.Digest, old.digest)))
		}
		h.touch(ctx, old)
		h.get(w, r, q)
		return nil, false
	}

	release, ok := h.Downloads.Acquire(ctx)
	if !ok {
		h.count(h.Refused, "busy")
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "too many downloads are going on: try again", nil)
		return errors.New("busy"), false
	}
	h.setActive()
	defer func() { release(); h.setActive() }()

	ct := resp.Header.Get("Content-Type")
	if len(ct) > 255 {
		ct = ct[:255]
	}
	hdr := w.Header()
	if ct != "" {
		hdr.Set("Content-Type", ct)
	} else {
		hdr.Set("Content-Type", "application/octet-stream")
	}
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "sandbox")
	if resp.ContentLength >= 0 {
		hdr.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	tee := format.NewTee(w, h.Rate, func() { w.WriteHeader(http.StatusOK) })
	up, err := format.Through(ctx, h.Blobs, src, want, tee)
	if err != nil {
		h.fetchFailed(ctx, cl, src, err)
		if errors.Is(err, blob.ErrDigest) {
			if h.Up != nil {
				h.Up.Changed(q.Repo.Name)
			}
			h.Audit.Record(ctx, h.event(q, "upstream_changed", path, audit.Refused, "the bytes do not have the digest the upstream named"))
		}
		if tee.Started() {
			return err, true
		}
		hdr.Del("Content-Length")
		h.count(h.Refused, "upstream")
		fail(w, http.StatusBadGateway, "the fetch from the upstream failed: "+err.Error(), nil)
		return err, false
	}
	if err := h.commitFetched(ctx, q, path, up, ct, etag, old); err != nil {
		_ = h.Blobs.Abandon(ctx, up) // not cached; the client has its bytes
	}
	return nil, false
}

// fetchFailed starts the pause when the upstream is what failed (its body was cut), not when the store or the digest was.
func (h *Raw) fetchFailed(ctx context.Context, cl *upstream.Client, src *format.ReadErr, err error) {
	if src.Err != nil {
		cl.Failed(ctx, src.Err)
	}
}

// touch says that the upstream has been asked now.
func (h *Raw) touch(ctx context.Context, a *asset) {
	if a != nil {
		_, _ = h.DB.Exec(ctx, meta.S(`UPDATE assets SET checked_at = ? WHERE id = ?`, h.nowUnix(), a.id))
	}
}

// commitFetched makes the file of the cache, in one transaction with the reference to its blob: new, or replacing the copy that was there.
func (h *Raw) commitFetched(ctx context.Context, q format.Request, path string, up *blob.Upload, ct, etag string, old *asset) error {
	id := format.Token()
	owner := "asset:" + id
	stmts := h.Blobs.Hold(owner, up.Digest)
	component := format.Token()
	if old != nil {
		component = old.component
		stmts = append(stmts, meta.Guard("changed", `SELECT 1 FROM assets WHERE id = ?`, old.id))
		stmts = append(stmts, h.Blobs.Drop("asset:"+old.id)...)
		stmts = append(stmts, meta.S(`DELETE FROM assets WHERE id = ?`, old.id))
	} else {
		stmts = append(stmts,
			meta.Unless("changed", `SELECT 1 FROM assets WHERE repo = ? AND path = ?`, q.Repo.Name, path),
			meta.S(`INSERT INTO components (id, repo, name, props, created_by, created_at) VALUES (?, ?, ?, '{}', ?, ?)`, component, q.Repo.Name, path, "proxy", h.nowUnix()))
	}
	stmts = append(stmts,
		meta.S(`INSERT INTO assets (id, component_id, repo, path, digest, size, content_type, created_at, etag, checked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, component, q.Repo.Name, path, up.Digest, up.Size, ct, h.nowUnix(), etag, h.nowUnix()))
	stmts = append(stmts, h.Blobs.Done(up)...)
	ev := h.event(q, "fetch", path, audit.OK, fmt.Sprintf("%s %d", up.Digest, up.Size))
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if why, refused := meta.RefusedBy(err); refused && why == "changed" {
			_ = h.Blobs.Abandon(ctx, up) // another request stored it first (the same file): this upload is not needed
			return nil
		}
		return err
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, q.Repo.Name)
	return nil
}
