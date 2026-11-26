package oidc_test

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/oidc"
	"github.com/zystem/godepot/internal/testutil"
)

func issuer(i *testutil.Issuer) oidc.Issuer {
	return oidc.Issuer{URL: i.URL, Audience: "godepot", AllowPrivate: true}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestVerify(t *testing.T) {
	i := testutil.NewIssuer(t)
	v := &oidc.Verifier{}
	ctx := context.Background()
	now := time.Now().Unix()
	for _, alg := range []string{"RS256", "ES256"} {
		c, err := v.Verify(ctx, issuer(i), i.Sign(t, alg, i.Claims(nil), nil))
		if err != nil || c["repository"] != "corp/app" {
			t.Fatalf("%s: %v %v", alg, c, err)
		}
	}
	good := map[string]string{
		"an audience in a list":          i.Sign(t, "RS256", i.Claims(map[string]any{"aud": []string{"other", "godepot"}}), nil),
		"expired inside the leeway":      i.Sign(t, "RS256", i.Claims(map[string]any{"exp": now - 30}), nil),
		"a start just ahead":             i.Sign(t, "RS256", i.Claims(map[string]any{"nbf": now + 30, "iat": now + 30}), nil),
		"no nbf and no iat":              i.Sign(t, "RS256", i.Claims(map[string]any{"nbf": nil, "iat": nil}), nil),
		"no key id, one key of its kind": i.Sign(t, "RS256", i.Claims(nil), map[string]any{"kid": nil}),
	}
	for name, tok := range good {
		if _, err := v.Verify(ctx, issuer(i), tok); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	ok := i.Sign(t, "RS256", i.Claims(nil), nil)
	parts := strings.Split(ok, ".")
	forged, _ := json.Marshal(i.Claims(map[string]any{"repository": "evil/app"}))
	// HMAC with the public key as the secret: the classic confusion of algorithms.
	hs := b64(`{"alg":"HS256","kid":"rsa-1"}`) + "." + parts[1]
	n, _ := base64.RawURLEncoding.DecodeString(i.Keys()[0]["n"])
	mac := hmac.New(sha256.New, n)
	mac.Write([]byte(hs))
	other := testutil.NewIssuer(t)
	bad := map[string]string{
		"no signature, alg none":     b64(`{"alg":"none"}`) + "." + parts[1] + ".",
		"HMAC with the public key":   hs + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
		"another payload":            parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2],
		"signed by another's key":    other.Sign(t, "RS256", i.Claims(nil), nil),
		"signed by another's EC key": other.Sign(t, "ES256", i.Claims(nil), nil),
		"an EC signature as RSA":     i.Sign(t, "ES256", i.Claims(nil), map[string]any{"alg": "RS256", "kid": "rsa-1"}),
		"an unknown key id":          i.Sign(t, "RS256", i.Claims(nil), map[string]any{"kid": "nope"}),
		"crit":                       i.Sign(t, "RS256", i.Claims(nil), map[string]any{"crit": []string{"exp"}}),
		"another audience":           i.Sign(t, "RS256", i.Claims(map[string]any{"aud": "somebody"}), nil),
		"no audience":                i.Sign(t, "RS256", i.Claims(map[string]any{"aud": nil}), nil),
		"another issuer":             i.Sign(t, "RS256", i.Claims(map[string]any{"iss": other.URL}), nil),
		"an ISS in capitals only":    i.Sign(t, "RS256", i.Claims(map[string]any{"iss": nil, "ISS": i.URL}), nil),
		"expired":                    i.Sign(t, "RS256", i.Claims(map[string]any{"exp": now - 90}), nil),
		"no end":                     i.Sign(t, "RS256", i.Claims(map[string]any{"exp": nil}), nil),
		"an end that is text":        i.Sign(t, "RS256", i.Claims(map[string]any{"exp": "tomorrow"}), nil),
		"not yet good":               i.Sign(t, "RS256", i.Claims(map[string]any{"nbf": now + 300}), nil),
		"issued in the future":       i.Sign(t, "RS256", i.Claims(map[string]any{"iat": now + 300}), nil),
		"two parts":                  parts[0] + "." + parts[1],
		"not base64":                 "@@." + parts[1] + "." + parts[2],
		"not an object":              b64(`[1]`) + "." + parts[1] + "." + parts[2],
		"null":                       parts[0] + "." + b64(`null`) + "." + parts[2],
		"too long":                   ok + strings.Repeat("A", 20000),
		"empty":                      "",
	}
	for name, tok := range bad {
		if c, err := v.Verify(ctx, issuer(i), tok); !errors.Is(err, oidc.ErrInvalid) || c != nil {
			t.Errorf("%s: %v %v", name, c, err)
		}
	}
	// an algorithm that is not taken is refused before anything is asked of the issuer
	before := i.Reads.Load()
	for _, tok := range []string{bad["no signature, alg none"], bad["HMAC with the public key"]} {
		_, _ = (&oidc.Verifier{}).Verify(ctx, issuer(i), tok)
	}
	if i.Reads.Load() != before {
		t.Error("the keys were read for a token of an algorithm that is not taken")
	}
	// An issuer with no audience set takes nothing.
	if _, err := v.Verify(ctx, oidc.Issuer{URL: i.URL, AllowPrivate: true}, i.Sign(t, "RS256", i.Claims(map[string]any{"aud": ""}), nil)); !errors.Is(err, oidc.ErrInvalid) {
		t.Errorf("no audience set: %v", err)
	}
	if iss, err := oidc.Peek(ok); err != nil || iss != i.URL {
		t.Fatalf("peek: %q %v", iss, err)
	}
	if _, err := oidc.Peek(i.Sign(t, "RS256", i.Claims(map[string]any{"iss": nil}), nil)); !errors.Is(err, oidc.ErrInvalid) {
		t.Fatalf("peek with no issuer: %v", err)
	}
}

func TestKeysAreReadSeldomAndFollowARotation(t *testing.T) {
	i := testutil.NewIssuer(t)
	clock := time.Now()
	v := &oidc.Verifier{Now: func() time.Time { return clock }}
	ctx := context.Background()
	verify := func(tok string) error {
		_, err := v.Verify(ctx, issuer(i), tok)
		return err
	}
	long := map[string]any{"exp": clock.Add(100 * time.Hour).Unix()}
	for range 3 {
		if err := verify(i.Sign(t, "RS256", i.Claims(long), nil)); err != nil {
			t.Fatal(err)
		}
	}
	// tokens with invented key ids do not make requests, one a minute at most
	for range 20 {
		_ = verify(i.Sign(t, "RS256", i.Claims(long), map[string]any{"kid": "invented"}))
	}
	if n := i.Reads.Load(); n != 1 {
		t.Fatalf("the keys were read %d times", n)
	}
	i.Rotate(t, "rsa-2")
	clock = clock.Add(2 * time.Minute)
	if err := verify(i.Sign(t, "RS256", i.Claims(long), nil)); err != nil {
		t.Fatalf("a new key: %v", err)
	}
	if n := i.Reads.Load(); n != 2 {
		t.Fatalf("the keys were read %d times", n)
	}
	// the issuer is down: the keys that are held still work, for a day; an unknown key is "unavailable", not "invalid"
	tok := i.Sign(t, "RS256", i.Claims(long), nil)
	unknown := i.Sign(t, "RS256", i.Claims(long), map[string]any{"kid": "rsa-3"})
	i.Down.Store(true)
	clock = clock.Add(2 * time.Hour)
	if err := verify(tok); err != nil {
		t.Fatalf("old keys while the issuer is down: %v", err)
	}
	if err := verify(unknown); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("an unknown key while the issuer is down: %v", err)
	}
	clock = clock.Add(23 * time.Hour)
	if err := verify(tok); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("keys older than a day: %v", err)
	}
}

func TestTheIssuerThatCannotBeRead(t *testing.T) {
	i := testutil.NewIssuer(t)
	ctx := context.Background()
	tok := i.Sign(t, "RS256", i.Claims(nil), nil)
	unavailable := func(name string, is oidc.Issuer) {
		t.Helper()
		if _, err := (&oidc.Verifier{}).Verify(ctx, is, tok); !errors.Is(err, oidc.ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// without allow_private neither a loopback address nor http is read
	unavailable("private", oidc.Issuer{URL: i.URL, Audience: "godepot"})
	if i.Reads.Load() != 0 {
		t.Fatal("a private address was read")
	}
	// https to a loopback address is refused for being private, before anything is sent
	if _, err := (&oidc.Verifier{}).Verify(ctx, oidc.Issuer{URL: i.URL, Audience: "godepot", JWKSURL: "https://127.0.0.1:1/keys"}, tok); !errors.Is(err, oidc.ErrUnavailable) || !strings.Contains(err.Error(), "private") {
		t.Errorf("a private https address: %v", err)
	}
	unavailable("a redirect", oidc.Issuer{URL: i.URL, Audience: "godepot", AllowPrivate: true, JWKSURL: i.URL + "/moved"})
	unavailable("no such address", oidc.Issuer{URL: i.URL, Audience: "godepot", AllowPrivate: true, JWKSURL: i.URL + "/nothing"})
	i.SaysItIs = "https://somebody.else"
	unavailable("a discovery document of another issuer", issuer(i))
	i.SaysItIs = ""
	// the address of the keys in the settings is used instead of the discovery document
	i.SaysItIs = "https://somebody.else"
	if _, err := (&oidc.Verifier{}).Verify(ctx, oidc.Issuer{URL: i.URL, Audience: "godepot", AllowPrivate: true, JWKSURL: i.URL + "/keys"}, tok); err != nil {
		t.Fatalf("jwks_url: %v", err)
	}
	// a failed first read is not repeated at once, and is repeated after a pause
	i.SaysItIs = ""
	i.Down.Store(true)
	clock := time.Now()
	v := &oidc.Verifier{Now: func() time.Time { return clock }}
	if _, err := v.Verify(ctx, issuer(i), tok); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatal(err)
	}
	i.Down.Store(false)
	if _, err := v.Verify(ctx, issuer(i), tok); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("read again at once: %v", err)
	}
	clock = clock.Add(6 * time.Second)
	if _, err := v.Verify(ctx, issuer(i), tok); err != nil {
		t.Fatal(err)
	}
}

func TestKeysThatAreNotUsed(t *testing.T) {
	i := testutil.NewIssuer(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024) // #nosec G403 -- a key that must be refused
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	i.Extra = []map[string]string{
		{"kty": "RSA", "kid": "small", "n": enc(small.N.Bytes()), "e": enc(big.NewInt(int64(small.E)).Bytes())},
		{"kty": "RSA", "kid": "enc", "use": "enc", "n": i.Keys()[0]["n"], "e": "AQAB"},
		{"kty": "RSA", "kid": "other-alg", "alg": "RS512", "n": i.Keys()[0]["n"], "e": "AQAB"},
		{"kty": "EC", "kid": "off-curve", "crv": "P-256", "x": enc(make([]byte, 32)), "y": enc(make([]byte, 32))},
		{"kty": "oct", "kid": "secret"},
	}
	v := &oidc.Verifier{}
	// a token really signed by the short key: only its length refuses it
	head := b64(`{"alg":"RS256","kid":"small"}`)
	claims, _ := json.Marshal(i.Claims(nil))
	signed := head + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, small, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), issuer(i), signed+"."+enc(sig)); !errors.Is(err, oidc.ErrInvalid) {
		t.Errorf("a key of 1024 bits: %v", err)
	}
	for _, kid := range []string{"enc", "other-alg", "off-curve", "secret"} {
		alg := "RS256"
		if kid == "off-curve" {
			alg = "ES256"
		}
		if _, err := v.Verify(context.Background(), issuer(i), i.Sign(t, alg, i.Claims(nil), map[string]any{"kid": kid})); !errors.Is(err, oidc.ErrInvalid) {
			t.Errorf("%s: %v", kid, err)
		}
	}
	// two keys of one kind and a token that names none: which one is not guessed
	j := testutil.NewIssuer(t)
	j.Extra = []map[string]string{{"kty": "RSA", "kid": "second", "n": j.Keys()[0]["n"], "e": "AQAB"}}
	if _, err := (&oidc.Verifier{}).Verify(context.Background(), issuer(j), j.Sign(t, "RS256", j.Claims(nil), map[string]any{"kid": nil})); !errors.Is(err, oidc.ErrInvalid) {
		t.Errorf("no key id and two keys: %v", err)
	}
}
