// Package upstream is how godepot talks to the registry that a proxy repository copies from (docs/proxy.md). It holds what is the same for every format: the address is the operator's alone and
// private addresses are refused after the name is resolved (P9), credentials go to the upstream and to nowhere else (P8), the requests are limited, an error stops the requests for a growing pause
// and is never taken for "not found" (P4), a "not found" is remembered for a short time, and requests for one thing are joined (P2).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/settings"
)

// The pause after an error of the upstream: it starts at MinPause and doubles with each error in a row, up to MaxPause (P4).
const (
	MinPause = 5 * time.Second
	MaxPause = 5 * time.Minute
)

// Errors of a request to the upstream.
var (
	// ErrPrivate: the name of the upstream resolved to an address that godepot does not fetch from (P9).
	ErrPrivate = errors.New("the address of the upstream is private, loopback or link-local, and the upstream does not say allow_private")
	// ErrCredentials: the upstream refused the credentials of the repository, or wants some and has none.
	ErrCredentials = errors.New("the upstream refused the credentials of this repository")
)

// PausedError says that requests to the upstream are stopped for a while because of its errors.
type PausedError struct {
	Until time.Time
	Last  string
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("the upstream is paused until %s after its errors (%s)", e.Until.UTC().Format(time.RFC3339), e.Last)
}

// FailError says that the upstream answered with an error or did not answer. It has started (or lengthened) the pause.
type FailError struct {
	Status     int // 0: no answer
	Err        error
	RetryAfter time.Duration
}

func (e *FailError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("the upstream answered %d", e.Status)
	}
	return "the upstream did not answer: " + e.Err.Error()
}

func (e *FailError) Unwrap() error { return e.Err }

// Metrics are what a client counts; nil counters are not counted.
type Metrics struct {
	// Requests counts the requests sent to upstreams, by repository; Errors counts those that ended in an error or a 429 or 5xx, and those that were not sent because the upstream was paused.
	Requests, Errors *metrics.Counter
	// Stale counts the answers that were served from the cache because the upstream could not be asked, by repository (P3).
	Stale *metrics.Counter
	// Changed counts the times an upstream gave other bytes for what cannot change, by repository (P5).
	Changed *metrics.Counter
}

// Client is the way to the upstream of one repository.
type Client struct {
	Repo string
	DB   meta.DB
	M    *Metrics
	Now  func() time.Time

	cfg   settings.Upstream
	base  *url.URL
	user  string
	pass  string
	token string
	http  *http.Client
	slots chan struct{}
	tick  *time.Ticker // nil: no limit of the rate
	mu    sync.Mutex
	toks  map[string]bearer // bearer tokens of the upstream's token service, by scope
}

type bearer struct {
	token   string
	expires time.Time
}

// New makes the client of a proxy repository. The credentials are read from the environment variables the settings name.
func New(repo string, u settings.Upstream, db meta.DB, m *Metrics) (*Client, error) {
	base, err := url.Parse(u.URL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/")
	c := &Client{Repo: repo, DB: db, M: m, cfg: u, base: base, toks: map[string]bearer{}}
	if u.PasswordEnv != "" {
		if c.pass = os.Getenv(u.PasswordEnv); c.pass == "" {
			return nil, fmt.Errorf("the environment variable %s (upstream.password_env of %s) is not set", u.PasswordEnv, repo)
		}
		c.user = u.Username
	}
	if u.TokenEnv != "" {
		if c.token = os.Getenv(u.TokenEnv); c.token == "" {
			return nil, fmt.Errorf("the environment variable %s (upstream.token_env of %s) is not set", u.TokenEnv, repo)
		}
	}
	n := u.MaxConcurrent
	if n <= 0 {
		n = settings.DefaultMaxConcurrent
	}
	c.slots = make(chan struct{}, n)
	if u.MaxPerSecond > 0 {
		c.tick = time.NewTicker(time.Duration(float64(time.Second) / u.MaxPerSecond))
	}
	dial := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !u.AllowPrivate {
		dial.Control = refusePrivate
	}
	c.http = &http.Client{
		Transport: &http.Transport{
			// No proxy from the environment: the check of the address must see the address that is dialled.
			Proxy:                 nil,
			DialContext:           dial.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   n,
			// The bytes are kept as the upstream sent them: a layer is digested as it is.
			DisableCompression: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("more than 5 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization") // P8: credentials are never sent after a redirect to another host
			}
			return nil
		},
	}
	return c, nil
}

// Close stops what the client runs.
func (c *Client) Close() {
	if c.tick != nil {
		c.tick.Stop()
	}
	c.http.CloseIdleConnections()
}

// Same says whether the client was made for these settings.
func (c *Client) Same(u settings.Upstream) bool {
	return fmt.Sprintf("%+v", c.cfg) == fmt.Sprintf("%+v", u)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// refusePrivate is the Control of the dialer: it sees the address after the name is resolved, for the first request and for every redirect and token service after it.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if Private(ip) {
		return ErrPrivate
	}
	return nil
}

// Private says whether an address is one that a fetch for a client must not reach: loopback, private, link-local (the address of a cloud's metadata service is one), unspecified, multicast, and the
// shared address space of carriers (100.64.0.0/10).
func Private(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// Paused says whether requests are stopped now.
func (c *Client) Paused(ctx context.Context) (*PausedError, error) {
	rows, err := c.DB.Query(ctx, meta.S(`SELECT pause_until, last_error FROM upstream_state WHERE repo = ?`, c.Repo))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	until := meta.Int(rows[0][0])
	if until <= c.now().Unix() {
		return nil, nil
	}
	return &PausedError{Until: time.Unix(until, 0), Last: meta.Str(rows[0][1])}, nil
}

// fail writes down an error of the upstream and lengthens the pause: 5 seconds, then twice as long each time in a row, at most 5 minutes; a Retry-After of the upstream, when longer, is used (at most
// 5 minutes).
func (c *Client) fail(ctx context.Context, why string, retry time.Duration) {
	ctx = context.WithoutCancel(ctx)
	fails := int64(0)
	if rows, err := c.DB.Query(ctx, meta.S(`SELECT fails FROM upstream_state WHERE repo = ?`, c.Repo)); err == nil && len(rows) > 0 {
		fails = meta.Int(rows[0][0])
	}
	pause := MinPause << min(fails, 6)
	if retry > pause {
		pause = retry
	}
	if pause > MaxPause {
		pause = MaxPause
	}
	if len(why) > 200 {
		why = why[:200]
	}
	until := c.now().Add(pause).Unix()
	_, _ = c.DB.Exec(ctx, meta.S(`INSERT INTO upstream_state (repo, fails, pause_until, last_error) VALUES (?, ?, ?, ?)
		ON CONFLICT (repo) DO UPDATE SET fails = excluded.fails, pause_until = excluded.pause_until, last_error = excluded.last_error`, c.Repo, fails+1, until, why))
}

// Failed writes down that a body was cut or an answer was not good after Do had returned it: it starts the pause as an error of the upstream does.
func (c *Client) Failed(ctx context.Context, err error) {
	c.fail(ctx, err.Error(), 0)
	c.record("error")
}

// worked ends the run of errors.
func (c *Client) worked(ctx context.Context) {
	_, _ = c.DB.Exec(context.WithoutCancel(ctx), meta.S(`DELETE FROM upstream_state WHERE repo = ?`, c.Repo))
}

// Miss remembers that the upstream has no such name, for the time of the settings (P4). Only an answer of "not found" is remembered; an error never is.
func (c *Client) Miss(ctx context.Context, name string) {
	ttl := c.cfg.MissTTL()
	if ttl <= 0 {
		return
	}
	_, _ = c.DB.Exec(context.WithoutCancel(ctx), meta.S(`INSERT INTO upstream_misses (repo, name, until) VALUES (?, ?, ?)
		ON CONFLICT (repo, name) DO UPDATE SET until = excluded.until`, c.Repo, name, c.now().Add(ttl).Unix()))
}

// Missed says whether the upstream was lately found not to have the name.
func (c *Client) Missed(ctx context.Context, name string) (bool, error) {
	rows, err := c.DB.Query(ctx, meta.S(`SELECT 1 FROM upstream_misses WHERE repo = ? AND name = ? AND until > ?`, c.Repo, name, c.now().Unix()))
	return len(rows) > 0, err
}

// Forget clears what is remembered as missing for the repository (all of it, or one name), and ends a pause. It is what the API calls.
func Forget(ctx context.Context, db meta.DB, repo, name string) (int64, error) {
	var stmts []meta.Stmt
	if name == "" {
		stmts = []meta.Stmt{meta.S(`DELETE FROM upstream_misses WHERE repo = ?`, repo), meta.S(`DELETE FROM upstream_state WHERE repo = ?`, repo)}
	} else {
		stmts = []meta.Stmt{meta.S(`DELETE FROM upstream_misses WHERE repo = ? AND name = ?`, repo, name)}
	}
	res, err := db.Exec(ctx, stmts...)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range res {
		n += r
	}
	return n, nil
}

type body struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *body) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// Missing says whether an answer means that the upstream does not give this name: a 404 (a 410, which the Go proxies use for a version they removed), or a 401 or 403 that the credentials (or the anonymous token) could not turn into an answer. Docker Hub and
// GitHub's registry answer 401 for a name that is not there, so a refusal for one name is not an error of the upstream and does not pause it.
func Missing(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone || status == http.StatusUnauthorized || status == http.StatusForbidden
}

// Do asks the upstream. path begins with "/" and is added to the address of the upstream; header is added to the request. The answers that the caller must understand come back as they are, whatever
// their status (200, 206, 304, 404, 401 or 403 for one name, and other 4xx; see Missing); an error, a 429 or a 5xx is a *FailError (and starts the pause), a pause that is on is a *PausedError, and
// credentials that are refused for every name are ErrCredentials (and start the pause). The caller closes the body.
func (c *Client) Do(ctx context.Context, method, path string, header http.Header) (*http.Response, error) {
	return c.doTarget(ctx, method, c.base.String()+path, header)
}

// DoURL asks for an address that the upstream named itself (a file that an index says is on another host). The host must be the upstream's own or one of file_hosts; credentials are sent only to the
// upstream's own host (P8), and the address is checked as every other (P9). The rest is as Do.
func (c *Client) DoURL(ctx context.Context, method, target string, header http.Header) (*http.Response, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("the upstream named an address that is not taken: %q", target)
	}
	if !strings.EqualFold(u.Host, c.base.Host) && !c.fileHost(u.Hostname()) {
		return nil, fmt.Errorf("the upstream named a file on the host %q, which is not the upstream's and not in file_hosts", u.Hostname())
	}
	return c.doTarget(ctx, method, target, header)
}

func (c *Client) fileHost(host string) bool {
	for _, h := range c.cfg.FileHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func (c *Client) doTarget(ctx context.Context, method, target string, header http.Header) (*http.Response, error) {
	if p, err := c.Paused(ctx); err != nil {
		return nil, err
	} else if p != nil {
		c.record("paused")
		return nil, p
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-c.slots
		}
	}
	if c.tick != nil {
		select {
		case <-c.tick.C:
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	}
	resp, err := c.once(ctx, method, target, header, "")
	challenged := false
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		scope, ok, cerr := c.challenge(ctx, resp)
		if cerr != nil {
			resp.Body.Close()
			release()
			if ctx.Err() == nil {
				c.fail(ctx, cerr.Error(), 0)
				c.record("error")
			}
			return nil, cerr
		}
		if ok {
			challenged = true
			resp.Body.Close()
			resp, err = c.once(ctx, method, target, header, scope)
		}
	}
	if err != nil {
		release()
		if ctx.Err() != nil {
			return nil, ctx.Err() // the caller went away: the upstream did nothing wrong
		}
		if errors.Is(err, ErrPrivate) {
			return nil, ErrPrivate // the settings are wrong, the upstream did nothing: no pause
		}
		c.fail(ctx, err.Error(), 0)
		c.record("error")
		return nil, &FailError{Err: err}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && !challenged && c.hasCredentials():
		// The credentials that every request carries were refused: it is the same for every name, so it is an error of the upstream's use, and pauses it.
		resp.Body.Close()
		release()
		c.fail(ctx, "answered 401 to the credentials", 0)
		c.record("error")
		return nil, ErrCredentials
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		ra := retryAfter(resp.Header.Get("Retry-After"), c.now())
		resp.Body.Close()
		release()
		c.fail(ctx, fmt.Sprintf("answered %d", resp.StatusCode), ra)
		c.record("error")
		return nil, &FailError{Status: resp.StatusCode, RetryAfter: ra}
	}
	c.worked(ctx)
	if Missing(resp.StatusCode) {
		c.record("not_found")
	} else {
		c.record("ok")
	}
	resp.Body = &body{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// record counts a request by its result: ok, not_found, error, paused.
func (c *Client) record(result string) {
	if c.M == nil {
		return
	}
	if result != "paused" && c.M.Requests != nil {
		c.M.Requests.Add(c.Repo, 1)
	}
	if (result == "error" || result == "paused") && c.M.Errors != nil {
		c.M.Errors.Add(c.Repo, 1)
	}
}

func (c *Client) hasCredentials() bool { return c.pass != "" || c.token != "" }

func retryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// once sends one request. scope, when it is not empty, is the key of a bearer token of the token service to send.
func (c *Client) once(ctx context.Context, method, target string, header http.Header, scope string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "godepot")
	own := strings.EqualFold(req.URL.Host, c.base.Host)
	switch {
	case !own:
		// A host that the upstream's index named: no credentials, ever (P8).
	case scope != "":
		c.mu.Lock()
		t := c.toks[scope]
		c.mu.Unlock()
		if t.token != "" && c.now().Before(t.expires) {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.pass != "":
		req.SetBasicAuth(c.user, c.pass)
	}
	return c.http.Do(req)
}

// challenge answers a 401 that names a token service ("WWW-Authenticate: Bearer realm=..., service=..., scope=..."): it asks the service for a token, and keeps it. ok is false when there is no such
// challenge or the service gave nothing. The credentials go to the service only when it is on the host of the upstream or on one of token_hosts (P8); otherwise the token is asked for with none.
func (c *Client) challenge(ctx context.Context, resp *http.Response) (string, bool, error) {
	h := resp.Header.Get("WWW-Authenticate")
	scheme, rest, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "bearer") {
		return "", false, nil
	}
	params := parseParams(rest)
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") || realm.Host == "" {
		return "", false, nil
	}
	key := params["service"] + "|" + params["scope"] + "|" + realm.Host
	q := realm.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", false, nil
	}
	req.Header.Set("User-Agent", "godepot")
	if c.trusts(realm.Hostname()) {
		switch {
		case c.pass != "":
			req.SetBasicAuth(c.user, c.pass)
		case c.token != "":
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
	}
	tr, err := c.http.Do(req)
	if err != nil {
		return "", false, nil
	}
	defer tr.Body.Close()
	if tr.StatusCode == http.StatusUnauthorized || tr.StatusCode == http.StatusForbidden {
		if c.trusts(realm.Hostname()) && c.hasCredentials() {
			return "", false, ErrCredentials // the token service refused the credentials: they are wrong for every name
		}
		return "", false, nil
	}
	if tr.StatusCode != http.StatusOK {
		return "", false, nil
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decode(io.LimitReader(tr.Body, 64<<10), &out); err != nil {
		return "", false, nil
	}
	t := out.Token
	if t == "" {
		t = out.AccessToken
	}
	if t == "" {
		return "", false, nil
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Minute
	}
	if life > 5*time.Minute {
		life = 5 * time.Minute
	}
	c.mu.Lock()
	c.toks[key] = bearer{token: t, expires: c.now().Add(life - 10*time.Second)}
	c.mu.Unlock()
	return key, true, nil
}

func (c *Client) trusts(host string) bool {
	if strings.EqualFold(host, c.base.Hostname()) {
		return true
	}
	for _, h := range c.cfg.TokenHosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

// parseParams reads `a="b", c="d"`.
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(k)
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			v, s = rest[1:1+end], rest[end+2:]
		} else {
			v, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(k)] = v
	}
	return out
}
