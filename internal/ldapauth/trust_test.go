package ldapauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func certificate(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, change func(*x509.Certificate)) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	change(tmpl)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func authority(c *x509.Certificate) {
	c.IsCA, c.KeyUsage, c.ExtKeyUsage = true, x509.KeyUsageCertSign, nil
}

// A given server certificate decides by itself; one signed by a given authority must also have the name. The chain, the time and the use are always checked.
func TestWhatAGivenCertificateTrusts(t *testing.T) {
	ca, caKey := certificate(t, "ca", nil, nil, authority)
	mid, midKey := certificate(t, "intermediate", ca, caKey, authority)
	stranger, strangerKey := certificate(t, "ca", nil, nil, authority) // the same name, another key
	named := func(c *x509.Certificate) { c.DNSNames = []string{"dc.test"} }
	other := func(c *x509.Certificate) { c.DNSNames = []string{"other.test"} }
	leaf, _ := certificate(t, "leaf", ca, caKey, named)
	deep, _ := certificate(t, "deep", mid, midKey, named)
	misnamed, _ := certificate(t, "misnamed", ca, caKey, other)
	foreign, _ := certificate(t, "foreign", stranger, strangerKey, named)
	ended, _ := certificate(t, "ended", ca, caKey, func(c *x509.Certificate) { named(c); c.NotAfter = time.Now().Add(-time.Hour) })
	client, _ := certificate(t, "client", ca, caKey, func(c *x509.Certificate) { named(c); c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} })
	// servers' own certificates, given themselves: one with another name, one with none, one that has ended
	own, _ := certificate(t, "own", nil, nil, other)
	bare, _ := certificate(t, "bare", nil, nil, func(*x509.Certificate) {})
	old, _ := certificate(t, "old", nil, nil, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Hour) })

	config := func(serverName string, given ...*x509.Certificate) *tls.Config {
		t.Helper()
		var data []byte
		for _, g := range given {
			data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.Raw})...)
		}
		got, err := Config{URL: "ldaps://dc.test:636", CA: data, ServerName: serverName}.tlsConfig()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for name, x := range map[string]struct {
		serverName string
		given      *x509.Certificate
		sent       []*x509.Certificate
		ok         bool
	}{
		"signed by it":                      {"", ca, []*x509.Certificate{leaf}, true},
		"through another":                   {"", ca, []*x509.Certificate{deep, mid}, true},
		"the other not sent":                {"", ca, []*x509.Certificate{deep}, false},
		"signed, another name":              {"", ca, []*x509.Certificate{misnamed}, false},
		"signed, server_name says the name": {"other.test", ca, []*x509.Certificate{misnamed}, true},
		"signed by a stranger":              {"", ca, []*x509.Certificate{foreign, stranger}, false},
		"ended":                             {"", ca, []*x509.Certificate{ended}, false},
		"not for a server":                  {"", ca, []*x509.Certificate{client}, false},
		"no certificate at all":             {"", ca, nil, false},
		"given itself, another name":        {"", own, []*x509.Certificate{own}, true},
		"given itself, no name":             {"", bare, []*x509.Certificate{bare}, true},
		"given itself, server_name asks":    {"dc.test", own, []*x509.Certificate{own}, false},
		"given itself, server_name it has":  {"other.test", own, []*x509.Certificate{own}, true},
		"given itself, ended":               {"", old, []*x509.Certificate{old}, false},
		"another than the one given":        {"", own, []*x509.Certificate{bare}, false},
		"a server certificate of an authority, given itself, misnamed": {"", misnamed, []*x509.Certificate{misnamed}, true},
	} {
		check := config(x.serverName, x.given).VerifyConnection
		if err := check(tls.ConnectionState{PeerCertificates: x.sent}); (err == nil) != x.ok {
			t.Errorf("%s: %v", name, err)
		}
	}

	// with nothing given it is the standard check of Go, against the system's authorities, name included
	if got := config(""); got.InsecureSkipVerify || got.VerifyConnection != nil || got.RootCAs != nil || got.ServerName != "dc.test" {
		t.Errorf("nothing given: %+v", got)
	}
}
