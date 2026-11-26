package testutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issuer is a false OpenID Connect issuer: it publishes a discovery document and its keys, and signs what a test asks it to sign. It listens on a loopback address over http, so the settings that trust it
// say allow_private.
type Issuer struct {
	*httptest.Server
	Reads atomic.Int64 // how many times the keys were read
	Down  atomic.Bool  // answers 500

	mu       sync.Mutex
	rsa      *rsa.PrivateKey
	ec       *ecdsa.PrivateKey
	RSAID    string
	ECID     string
	SaysItIs string // the issuer in the discovery document ("": its own address)
	Extra    []map[string]string
}

// NewIssuer starts one with an RSA key and a P-256 key.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	i := &Issuer{RSAID: "rsa-1", ECID: "ec-1"}
	var err error
	if i.rsa, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if i.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	i.Server = httptest.NewServer(http.HandlerFunc(i.serve))
	t.Cleanup(i.Close)
	return i
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Rotate replaces the RSA key by a new one with the given id.
func (i *Issuer) Rotate(t testing.TB, id string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rsa, i.RSAID = k, id
}

// Keys are the keys as the issuer publishes them.
func (i *Issuer) Keys() []map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	pt, err := i.ec.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return append([]map[string]string{
		{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": i.RSAID, "n": b64(i.rsa.N.Bytes()), "e": b64(big.NewInt(int64(i.rsa.E)).Bytes())},
		{"kty": "EC", "use": "sig", "alg": "ES256", "crv": "P-256", "kid": i.ECID, "x": b64(pt[1:33]), "y": b64(pt[33:])},
	}, i.Extra...)
}

func (i *Issuer) serve(w http.ResponseWriter, r *http.Request) {
	if i.Down.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		is := i.SaysItIs
		if is == "" {
			is = i.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is, "jwks_uri": i.URL + "/keys"})
	case "/keys":
		i.Reads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": i.Keys()})
	case "/moved":
		http.Redirect(w, r, "/keys", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

// Claims are the claims of a job that is good for five minutes, with those of more over them (a nil value removes one).
func (i *Issuer) Claims(more map[string]any) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{"iss": i.URL, "aud": "godepot", "sub": "repo:corp/app:ref:refs/heads/main", "repository": "corp/app", "ref": "refs/heads/main", "iat": now, "nbf": now, "exp": now + 300}
	maps.Copy(c, more)
	for k, v := range c {
		if v == nil {
			delete(c, k)
		}
	}
	return c
}

// Sign signs the claims with the key of the algorithm (RS256 or ES256); header replaces words of the header.
func (i *Issuer) Sign(t testing.TB, alg string, claims map[string]any, header map[string]any) string {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	h := map[string]any{"alg": alg, "typ": "JWT", "kid": i.RSAID}
	if alg == "ES256" {
		h["kid"] = i.ECID
	}
	maps.Copy(h, header)
	for k, v := range h {
		if v == nil {
			delete(h, k)
		}
	}
	hj, _ := json.Marshal(h)
	cj, _ := json.Marshal(claims)
	signed := b64(hj) + "." + b64(cj)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	if alg == "ES256" {
		r, s, err := ecdsa.Sign(rand.Reader, i.ec, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	} else {
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, i.rsa, crypto.SHA256, sum[:]); err != nil {
			t.Fatal(err)
		}
	}
	return signed + "." + b64(sig)
}
