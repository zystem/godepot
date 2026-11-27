package server_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/testutil"
)

func anotherCertificate(t *testing.T, until time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "another"}, NotBefore: until.Add(-2 * time.Hour), NotAfter: until, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// An admin gives godepot the certificate to trust: no file on the machine, and a server's own self-signed one is taken.
func TestATrustedCertificateGivenThroughTheAPI(t *testing.T) {
	r := newRig(t)
	d := r.directoryOf(testutil.NewSelfSigned(t), "5m")
	set := *r.srv.Settings.Load()
	l := *set.LDAP
	l.CAFile = ""
	set.LDAP = &l
	r.srv.Settings.Store(&set)
	text, err := os.ReadFile(d.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(text)
	sum := sha256.Sum256(block.Bytes)
	fingerprint := hex.EncodeToString(sum[:])
	login := func(pw string, want int) {
		t.Helper()
		r.want(want, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": "alice", "password": %q}`, pw))
	}
	const pw = "alice's directory password"
	login(pw, 503) // nothing trusts the certificate yet
	why := func() string {
		_, b := r.want(200, "GET", "/api/v1/audit?action=directory&limit=100", r.admin, "")
		return string(b)
	}
	if got := why(); !strings.Contains(got, `"actor":"alice"`) || !strings.Contains(got, "certificate") {
		t.Fatalf("the reason is not in the audit log: %s", got)
	}

	if _, b := r.want(200, "GET", "/api/v1/ldap/ca", r.admin, ""); !strings.Contains(string(b), `"certificates":[]`) {
		t.Fatalf("%s", b)
	}
	reader := r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Admin}})
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		r.want(403, method, "/api/v1/ldap/ca", reader, string(text))
		r.want(401, method, "/api/v1/ldap/ca", "", string(text))
	}
	r.want(405, "POST", "/api/v1/ldap/ca", r.admin, string(text))
	key := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	for name, body := range map[string]string{
		"nothing":            "",
		"not a certificate":  "hello",
		"a key":              key,
		"a certificate, key": string(text) + key,
		"a cut second block": string(text) + "-----BEGIN CERTIFICATE-----\nAAAA",
		"broken bytes":       "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"ended":              anotherCertificate(t, time.Unix(r.srv.Auth.Clock().Unix()-60, 0)),
		"too long":           string(text) + strings.Repeat(" ", 64<<10),
	} {
		if code, _, _ := r.do("PUT", "/api/v1/ldap/ca", r.admin, strings.NewReader(body)); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	login(pw, 503)

	_, b := r.want(200, "PUT", "/api/v1/ldap/ca", r.admin, "subject=what openssl writes\n"+string(text))
	if !strings.Contains(string(b), fingerprint) || !strings.Contains(string(b), "CN=dc.test") {
		t.Fatalf("%s", b)
	}
	login(pw, 201)
	_, b = r.want(200, "GET", "/api/v1/ldap/ca", r.admin, "")
	if !strings.Contains(string(b), fingerprint) || !strings.Contains(string(b), `"set_by":"root"`) || strings.Contains(string(b), "openssl writes") {
		t.Fatalf("%s", b)
	}

	// replaced, not added to: another certificate does not verify the server
	r.want(200, "PUT", "/api/v1/ldap/ca", r.admin, anotherCertificate(t, r.srv.Auth.Clock().Add(time.Hour)))
	login("a password not checked before", 503)
	r.want(200, "PUT", "/api/v1/ldap/ca", r.admin, string(text))
	login("a password not checked before", 401)
	r.want(204, "DELETE", "/api/v1/ldap/ca", r.admin, "")
	login("yet another password", 503)

	_, b = r.want(200, "GET", "/api/v1/audit?action=ldap.ca.set&limit=100", r.admin, "")
	if !strings.Contains(string(b), fingerprint) || !strings.Contains(string(b), "removed") || !strings.Contains(string(b), "forbidden") {
		t.Fatalf("%s", b)
	}
}
