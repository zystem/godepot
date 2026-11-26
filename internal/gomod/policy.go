package gomod

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/policy"
)

// The cooldown of a Go proxy (docs/spec.md, P6). The time of a version is in its .info, which is fetched (once: it cannot change) through the cache of the proxy. The list of an upstream names no times, so
// to leave out the young versions the .info of each is read, the newest probeMax of them (by version); an older version is not looked at for its time (it is looked at for the block list).
const (
	probeMax      = 300
	probeParallel = 8
	maxList       = 8 << 20
)

func (g *Go) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// fetch reads a small file of the proxy through raw (the cache, or the upstream).
func (g *Go) fetch(ctx context.Context, r *http.Request, q format.Request, path string) *format.Capture {
	qq := q
	qq.Path = path
	qq.Authorize = func(http.ResponseWriter, string, string) bool { return true } // the request was authorized
	qq.Allowed = func(string, string) bool { return true }
	rr := r.Clone(ctx)
	rr.Method = http.MethodGet
	rr.Header.Del("Range")
	rr.Header.Del("If-None-Match")
	c := format.NewCapture(maxList)
	g.Raw.Serve(c, rr, qq)
	return c
}

type info struct {
	Version string
	Time    time.Time
}

// info reads the .info of a version (module and version as in the address: escaped).
func (g *Go) info(ctx context.Context, r *http.Request, q format.Request, module, version string) (info, *format.Capture, bool) {
	c := g.fetch(ctx, r, q, module+"/@v/"+version+".info")
	var in info
	if c.Code != http.StatusOK || c.Full || json.Unmarshal(c.Buf.Bytes(), &in) != nil {
		return in, c, false
	}
	return in, c, true
}

// judge decides about a version of a module (both unescaped), reading its time when the cooldown needs it.
func (g *Go) judge(ctx context.Context, r *http.Request, q format.Request, pol *policy.Policy, module, version string) (policy.Verdict, *format.Capture) {
	if v, blocked := pol.Blocked(module, version); blocked {
		return v, nil
	}
	if pol.Cooldown <= 0 {
		return policy.Verdict{OK: true}, nil
	}
	in, c, ok := g.info(ctx, r, q, escape(module), escape(version))
	if !ok {
		return policy.Verdict{Why: module + "@" + version + ": its time of publication cannot be read from the upstream, so the cooldown cannot be checked"}, c
	}
	return pol.Version(module, version, in.Time, g.now()), c
}

func refuse(w http.ResponseWriter, v policy.Verdict) {
	if !v.Until.IsZero() {
		w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(time.Until(v.Until).Seconds())), 10))
	}
	reply(w, http.StatusForbidden, v.Why)
}

// proxy answers a request to a proxy repository that has a policy. ok is false when the policy does not apply and raw should answer.
func (g *Go) policed(w http.ResponseWriter, r *http.Request, q format.Request, t target) bool {
	pol := q.Repo.Upstream.Policy()
	if !pol.Active() {
		return false
	}
	module, _ := unescape(t.module)
	switch t.kind {
	case kInfo, kMod, kZip:
		v, _ := unescape(t.version)
		if verdict, _ := g.judge(r.Context(), r, q, pol, module, v); !verdict.OK {
			refuse(w, verdict)
			return true
		}
		return false
	case kList:
		g.list2(w, r, q, t, pol, module)
		return true
	case kLatest:
		g.latest2(w, r, q, t, pol, module)
		return true
	}
	return false
}

// visible lists the versions of the upstream's list that the policy lets be seen.
func (g *Go) visible(ctx context.Context, r *http.Request, q format.Request, pol *policy.Policy, module string, list []string) []string {
	sort.Slice(list, func(i, j int) bool {
		a, aok := parseSemver(list[i])
		b, bok := parseSemver(list[j])
		if aok && bok {
			return b.less(a) // the newest first
		}
		return list[i] > list[j]
	})
	ok := make([]bool, len(list))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeParallel)
	for i, v := range list {
		if _, b := pol.Blocked(module, v); b {
			continue
		}
		if pol.Cooldown <= 0 || i >= probeMax {
			ok[i] = true
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			verdict, _ := g.judge(ctx, r, q, pol, module, v)
			ok[i] = verdict.OK
		}()
	}
	wg.Wait()
	var out []string
	for i, v := range list {
		if ok[i] {
			out = append(out, v)
		}
	}
	return out
}

func (g *Go) list2(w http.ResponseWriter, r *http.Request, q format.Request, t target, pol *policy.Policy, module string) {
	c := g.fetch(r.Context(), r, q, q.Path)
	if c.Code != http.StatusOK || c.Full {
		copyAnswer(w, c)
		return
	}
	var list []string
	for _, l := range strings.Split(strings.TrimSpace(c.Buf.String()), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			list = append(list, l)
		}
	}
	vis := g.visible(r.Context(), r, q, pol, module, list)
	if v := c.Header_.Get("Warning"); v != "" {
		w.Header().Set("Warning", v)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	for i := len(vis) - 1; i >= 0; i-- { // visible has the newest first; the list of a module proxy has the oldest
		_, _ = w.Write([]byte(vis[i] + "\n"))
	}
}

// latest2 answers @latest: the upstream's, when the policy lets that version be seen; else the newest version that it does let be seen.
func (g *Go) latest2(w http.ResponseWriter, r *http.Request, q format.Request, t target, pol *policy.Policy, module string) {
	c := g.fetch(r.Context(), r, q, q.Path)
	if c.Code != http.StatusOK || c.Full {
		copyAnswer(w, c)
		return
	}
	var in info
	if json.Unmarshal(c.Buf.Bytes(), &in) == nil {
		if v, _ := g.judge(r.Context(), r, q, pol, module, in.Version); v.OK {
			sendJSON(w, r, c.Buf.Bytes(), c)
			return
		}
	}
	lc := g.fetch(r.Context(), r, q, t.module+"/@v/list")
	if lc.Code != http.StatusOK || lc.Full {
		reply(w, http.StatusNotFound, "no version of this module can be served")
		return
	}
	var list []string
	for _, l := range strings.Split(strings.TrimSpace(lc.Buf.String()), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if s, ok := parseSemver(l); ok && len(s.pre) == 0 {
				list = append(list, l)
			}
		}
	}
	vis := g.visible(r.Context(), r, q, pol, module, list)
	if len(vis) == 0 {
		reply(w, http.StatusNotFound, "no version of this module can be served")
		return
	}
	ic, _, ok := g.info(r.Context(), r, q, t.module, escape(vis[0]))
	if !ok {
		reply(w, http.StatusBadGateway, "the .info of the newest version that can be served is not available")
		return
	}
	sendJSON(w, r, ic.raw(), c)
}

func (i info) raw() []byte {
	b, _ := json.Marshal(map[string]any{"Version": i.Version, "Time": i.Time})
	return b
}

func sendJSON(w http.ResponseWriter, r *http.Request, b []byte, c *format.Capture) {
	if v := c.Header_.Get("Warning"); v != "" {
		w.Header().Set("Warning", v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodHead {
		_, _ = w.Write(b) // #nosec G705 -- JSON with nosniff: a browser does not run it
	}
}

func copyAnswer(w http.ResponseWriter, c *format.Capture) {
	for _, k := range []string{"Retry-After", "Content-Type"} {
		if v := c.Header_.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	code := c.Code
	if code == 0 || c.Full {
		code = http.StatusBadGateway
	}
	w.WriteHeader(code)
	_, _ = w.Write(c.Buf.Bytes())
}
