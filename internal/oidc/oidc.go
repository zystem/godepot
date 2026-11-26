// Package oidc checks a signed token (a JWT) of an issuer that the settings trust: the token that GitHub Actions, GitLab, Kubernetes or any CI gives a job (docs/spec.md, A3). It reads the keys the issuer
// publishes, checks the signature, the issuer, the audience and the times, and returns the claims. What the claims allow is the caller's business.
//
// It takes RS256 and ES256 and nothing else: no "none", no HMAC (a public key must never be usable as a secret), no header that must be understood ("crit"). The token is decoded into a map, so a claim is
// the one that has exactly its name.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zystem/godepot/internal/upstream"
)

var (
	// ErrInvalid: the token is not one this issuer signed for us, or it is out of its time. The text says why, for the audit log.
	ErrInvalid = errors.New("the token is not accepted")
	// ErrUnavailable: the keys of the issuer cannot be had now.
	ErrUnavailable = errors.New("the keys of the issuer cannot be read")
)

const (
	maxToken = 16 << 10
	maxDoc   = 1 << 20
	// Leeway is how far the clock of the issuer and ours may be apart.
	Leeway = time.Minute
	// the keys are read again after keysFor; a key id that is not among them causes a read at once, but not more often than every retry.
	keysFor = time.Hour
	retry   = time.Minute
	// when the issuer cannot be read, keys read this long ago are still used.
	staleFor = 24 * time.Hour
	// after a read that failed and left nothing, wait this long before the next.
	failedWait = 5 * time.Second
)

// Issuer is who is trusted to sign.
type Issuer struct {
	URL      string // the "iss" of its tokens, and where its discovery document is
	Audience string // the "aud" a token must have: what makes a token meant for this server
	JWKSURL  string // the address of the keys; "": from the discovery document
	// AllowPrivate lets the keys be read from a loopback or private address, and over http.
	AllowPrivate bool
}

// Verifier checks tokens and keeps the keys of the issuers. The zero value is ready.
type Verifier struct {
	// Now is the clock (default time.Now).
	Now func() time.Time

	mu      sync.Mutex
	issuers map[Issuer]*cache
}

type cache struct {
	mu            sync.Mutex
	keys          []key
	fetched       time.Time // when keys were read
	tried, failed time.Time // when a read was last begun, and when one last failed
	last          error
}

type key struct {
	id  string
	alg string // RS256 or ES256: what the key is for
	pub crypto.PublicKey
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

type parsed struct {
	header, claims map[string]any
	signed         string // header.payload, as signed
	sig            []byte
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

func parse(token string) (*parsed, error) {
	if len(token) > maxToken {
		return nil, invalid("it is longer than %d bytes", maxToken)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, invalid("not a signed token of three parts")
	}
	p := &parsed{signed: parts[0] + "." + parts[1]}
	for i, into := range []*map[string]any{&p.header, &p.claims} {
		b, err := base64.RawURLEncoding.Strict().DecodeString(parts[i])
		if err != nil {
			return nil, invalid("part %d is not base64url", i+1)
		}
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		if err := d.Decode(into); err != nil || *into == nil || d.More() {
			return nil, invalid("part %d is not a JSON object", i+1)
		}
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return nil, invalid("the signature is not base64url")
	}
	p.sig = sig
	return p, nil
}

// Peek reads the "iss" of a token without checking anything: it is only for choosing which issuer to check it against.
func Peek(token string) (string, error) {
	p, err := parse(token)
	if err != nil {
		return "", err
	}
	iss, _ := p.claims["iss"].(string)
	if iss == "" {
		return "", invalid("it names no issuer")
	}
	return iss, nil
}

// Verify checks the token against the issuer and returns its claims.
func (v *Verifier) Verify(ctx context.Context, is Issuer, token string) (map[string]any, error) {
	p, err := parse(token)
	if err != nil {
		return nil, err
	}
	alg, _ := p.header["alg"].(string)
	if alg != "RS256" && alg != "ES256" {
		return nil, invalid("the algorithm %.20q is not taken (RS256 and ES256 are)", alg)
	}
	if _, ok := p.header["crit"]; ok {
		return nil, invalid("it has a header that must be understood (crit)")
	}
	kid, _ := p.header["kid"].(string)
	if iss, _ := p.claims["iss"].(string); iss != is.URL {
		return nil, invalid("it is of the issuer %.100q", iss)
	}
	k, err := v.key(ctx, is, kid, alg)
	if err != nil {
		return nil, err
	}
	if !signedBy(k.pub, p.signed, p.sig) {
		return nil, invalid("the signature is not right")
	}
	if !hasAudience(p.claims["aud"], is.Audience) {
		return nil, invalid("it is not for the audience %q", is.Audience)
	}
	now := v.now()
	exp, ok := unixTime(p.claims["exp"])
	if !ok {
		return nil, invalid("it has no end (exp)")
	}
	if !now.Before(exp.Add(Leeway)) {
		return nil, invalid("it has expired")
	}
	for _, name := range []string{"nbf", "iat"} {
		if raw, there := p.claims[name]; there {
			t, ok := unixTime(raw)
			if !ok {
				return nil, invalid("%s is not a time", name)
			}
			if t.After(now.Add(Leeway)) {
				return nil, invalid("%s is in the future", name)
			}
		}
	}
	return p.claims, nil
}

func unixTime(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<40 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func hasAudience(aud any, want string) bool {
	if want == "" {
		return false
	}
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, e := range a {
			if s, _ := e.(string); s == want {
				return true
			}
		}
	}
	return false
}

// signedBy checks the signature with a key; the kind of the key says the algorithm, and the caller has checked that it is the one of the token.
func signedBy(pub crypto.PublicKey, signed string, sig []byte) bool {
	sum := sha256.Sum256([]byte(signed))
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) == nil
	case *ecdsa.PublicKey:
		if len(sig) != 64 {
			return false
		}
		return ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	}
	return false
}

// pick finds the key of a token: the one with its id (and of its algorithm), or, for a token that names none, the only one of its algorithm.
func pick(keys []key, kid, alg string) (key, bool) {
	var found []key
	for _, k := range keys {
		if k.alg == alg && (kid == "" || k.id == kid) {
			found = append(found, k)
		}
	}
	if len(found) != 1 {
		return key{}, false
	}
	return found[0], true
}

func (v *Verifier) cacheOf(is Issuer) *cache {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.issuers == nil {
		v.issuers = map[Issuer]*cache{}
	}
	c := v.issuers[is]
	if c == nil {
		c = &cache{}
		v.issuers[is] = c
	}
	return c
}

func (v *Verifier) key(ctx context.Context, is Issuer, kid, alg string) (key, error) {
	c := v.cacheOf(is)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := v.now()
	k, ok := pick(c.keys, kid, alg)
	fresh := now.Sub(c.fetched) < keysFor
	if ok && fresh {
		return k, nil
	}
	// Read again: the keys are old, or the token names a key we do not have. A flood of tokens with invented key ids must not become a flood of requests to the issuer.
	wait := retry
	if c.keys == nil {
		wait = failedWait
	}
	if c.tried.IsZero() || now.Sub(c.tried) >= wait {
		c.tried = now
		keys, err := fetch(ctx, is)
		if err != nil {
			c.failed, c.last = now, err
		} else {
			c.keys, c.fetched, c.failed, c.last = keys, now, time.Time{}, nil
		}
		k, ok = pick(c.keys, kid, alg)
	}
	if ok && now.Sub(c.fetched) < staleFor {
		return k, nil
	}
	if !c.failed.IsZero() {
		return key{}, fmt.Errorf("%w: %v", ErrUnavailable, c.last)
	}
	return key{}, invalid("the issuer has no key %.100q for %s", kid, alg)
}

func client(is Issuer) *http.Client {
	dial := &net.Dialer{Timeout: 5 * time.Second}
	if !is.AllowPrivate {
		dial.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if upstream.Private(ip) {
				return upstream.ErrPrivate
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		// No proxy from the environment: the check of the address must see the address that is dialled. No redirects: the keys are where the issuer says they are.
		Transport:     &http.Transport{Proxy: nil, DialContext: dial.DialContext, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func getJSON(ctx context.Context, h *http.Client, is Issuer, address string, into any) error {
	if !strings.HasPrefix(address, "https://") && !(is.AllowPrivate && strings.HasPrefix(address, "http://")) {
		return fmt.Errorf("%.200q is not an https address", address)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", address, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDoc)).Decode(into); err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	return nil
}

func fetch(ctx context.Context, is Issuer) ([]key, error) {
	// a caller that goes away must not make the read fail for the others who wait for it
	ctx = context.WithoutCancel(ctx)
	h := client(is)
	address := is.JWKSURL
	if address == "" {
		var d struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := getJSON(ctx, h, is, strings.TrimSuffix(is.URL, "/")+"/.well-known/openid-configuration", &d); err != nil {
			return nil, err
		}
		if d.Issuer != is.URL {
			return nil, fmt.Errorf("the discovery document is of the issuer %.200q, not %q", d.Issuer, is.URL)
		}
		address = d.JWKSURI
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, h, is, address, &set); err != nil {
		return nil, err
	}
	var out []key
	for _, j := range set.Keys {
		if k, ok := j.key(); ok { // a key of a kind we do not use is no error
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no RS256 or ES256 key", address)
	}
	return out, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j jwk) key() (key, bool) {
	if j.Use != "" && j.Use != "sig" {
		return key{}, false
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
		return b
	}
	switch j.Kty {
	case "RSA":
		n, e := new(big.Int).SetBytes(dec(j.N)), new(big.Int).SetBytes(dec(j.E))
		if (j.Alg != "" && j.Alg != "RS256") || n.BitLen() < 2048 || n.BitLen() > 8192 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
			return key{}, false
		}
		return key{id: j.Kid, alg: "RS256", pub: &rsa.PublicKey{N: n, E: int(e.Int64())}}, true
	case "EC":
		x, y := dec(j.X), dec(j.Y)
		if (j.Alg != "" && j.Alg != "ES256") || j.Crv != "P-256" || len(x) != 32 || len(y) != 32 {
			return key{}, false
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...)) // checks that the point is on the curve
		if err != nil {
			return key{}, false
		}
		return key{id: j.Kid, alg: "ES256", pub: pub}, true
	}
	return key{}, false
}
