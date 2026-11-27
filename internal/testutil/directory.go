package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

// Directory is a false LDAP server: simple bind, search, StartTLS and unbind, over TLS or after StartTLS. It does not understand filters: a test says which entries answer which filter, to the letter, so
// that the test also sees how the filter was made.
type Directory struct {
	CAFile   string // the certificate to trust
	Addr     string // 127.0.0.1:port
	Down     atomic.Bool
	Searches atomic.Int64
	Binds    atomic.Int64

	mu        sync.Mutex
	passwords map[string]string              // by DN
	answers   map[string][]map[string]string // by filter: entries, each with "dn" and attributes
	Seen      []string                       // the filters asked
	Clear     []string                       // what was received outside TLS, other than StartTLS
	tls       *tls.Config
	startTLS  bool
}

// NewDirectory starts one: with startTLS it listens in the clear and expects StartTLS first, otherwise TLS from the start.
func NewDirectory(t testing.TB, startTLS bool) *Directory {
	t.Helper()
	return newDirectory(t, startTLS, true)
}

// NewSelfSigned starts one whose certificate is its own: not that of an authority, as `openssl req -x509` with CA:FALSE or Windows' New-SelfSignedCertificate makes it.
func NewSelfSigned(t testing.TB) *Directory {
	t.Helper()
	return newDirectory(t, false, false)
}

func newDirectory(t testing.TB, startTLS, authority bool) *Directory {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dc.test"}, DNSNames: []string{"dc.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: authority, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if authority {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	d := &Directory{CAFile: filepath.Join(t.TempDir(), "ca.pem"), passwords: map[string]string{}, answers: map[string][]map[string]string{}, startTLS: startTLS,
		tls: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	if err := os.WriteFile(d.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.Addr = l.Addr().String()
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go d.serve(c)
		}
	}()
	return d
}

// URL is the address as the settings name it.
func (d *Directory) URL() string {
	if d.startTLS {
		return "ldap://" + d.Addr
	}
	return "ldaps://" + d.Addr
}

// Password sets (or, with "", removes) the password of an entry.
func (d *Directory) Password(dn, pw string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if pw == "" {
		delete(d.passwords, dn)
		return
	}
	d.passwords[dn] = pw
}

// Answer sets the entries that answer a filter.
func (d *Directory) Answer(filter string, entries ...map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[filter] = entries
}

// Filters are the filters that were asked.
func (d *Directory) Filters() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.Seen, "\n")
}

// Cleartext is what arrived in the clear apart from StartTLS.
func (d *Directory) Cleartext() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.Clear...)
}

func str(s, what string) *ber.Packet {
	return ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, s, what)
}

func result(id int64, app ber.Tag, code uint16) []byte {
	m := ber.NewSequence("message")
	m.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "id"))
	r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, app, nil, "result")
	r.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(code), "code"))
	r.AppendChild(str("", "matched"))
	r.AppendChild(str("", "message"))
	m.AppendChild(r)
	return m.Bytes()
}

func (d *Directory) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	secure := !d.startTLS
	if secure {
		c = tls.Server(c, d.tls)
	}
	bound := ""
	for {
		p, err := ber.ReadPacket(c)
		if err != nil || len(p.Children) < 2 || d.Down.Load() {
			return
		}
		id, _ := p.Children[0].Value.(int64)
		op := p.Children[1]
		if !secure && op.Tag != ldap.ApplicationExtendedRequest {
			d.mu.Lock()
			d.Clear = append(d.Clear, ldap.ApplicationMap[uint8(op.Tag)]) // #nosec G115 -- an application tag of LDAP is below 32
			d.mu.Unlock()
		}
		switch op.Tag {
		case ldap.ApplicationExtendedRequest:
			if _, err := c.Write(result(id, ldap.ApplicationExtendedResponse, ldap.LDAPResultSuccess)); err != nil {
				return
			}
			c, secure = tls.Server(c, d.tls), true
		case ldap.ApplicationBindRequest:
			d.Binds.Add(1)
			dn := op.Children[1].Value.(string)
			pw := op.Children[2].Data.String()
			d.mu.Lock()
			want, ok := d.passwords[dn]
			d.mu.Unlock()
			code := uint16(ldap.LDAPResultSuccess)
			switch {
			case pw == "": // as many servers do: a bind with no password "succeeds" as nobody
				bound = ""
			case !ok || want != pw:
				code, bound = ldap.LDAPResultInvalidCredentials, ""
			default:
				bound = dn
			}
			if _, err := c.Write(result(id, ldap.ApplicationBindResponse, code)); err != nil {
				return
			}
		case ldap.ApplicationSearchRequest:
			d.Searches.Add(1)
			filter, err := ldap.DecompileFilter(op.Children[6])
			if err != nil {
				return
			}
			d.mu.Lock()
			d.Seen = append(d.Seen, filter)
			entries := d.answers[filter]
			d.mu.Unlock()
			if bound == "" {
				_, _ = c.Write(result(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultInsufficientAccessRights))
				continue
			}
			for _, e := range entries {
				m := ber.NewSequence("message")
				m.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "id"))
				r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchResultEntry, nil, "entry")
				r.AppendChild(str(e["dn"], "dn"))
				attrs := ber.NewSequence("attributes")
				for k, v := range e {
					if k == "dn" {
						continue
					}
					a := ber.NewSequence("attribute")
					a.AppendChild(str(k, "type"))
					set := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "values")
					set.AppendChild(str(v, "value"))
					a.AppendChild(set)
					attrs.AppendChild(a)
				}
				r.AppendChild(attrs)
				m.AppendChild(r)
				if _, err := c.Write(m.Bytes()); err != nil {
					return
				}
			}
			if _, err := c.Write(result(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess)); err != nil {
				return
			}
		default:
			return
		}
	}
}
