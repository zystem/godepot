package ldapauth

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxCA is the longest PEM of trusted certificates that is taken.
const MaxCA = 64 << 10

// Certificate is what is told of one trusted certificate, so that an admin can compare it with the one they meant.
type Certificate struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	SHA256    string    `json:"sha256"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// ParseCA reads the certificates of a PEM: that of an authority (ca.crt), or a server's own self-signed one. Anything else in it is refused, a private key above all: it was pasted by mistake and must
// not be kept.
func ParseCA(data []byte) ([]*x509.Certificate, error) {
	if len(data) > MaxCA {
		return nil, fmt.Errorf("more than %d bytes", MaxCA)
	}
	var out []*x509.Certificate
	for rest := data; len(bytes.TrimSpace(rest)) > 0; {
		next, after := pem.Decode(rest)
		if next == nil {
			if bytes.Contains(rest, []byte("-----BEGIN")) {
				return nil, errors.New("a PEM block is cut or broken")
			}
			break // text around the blocks, as openssl writes it, is not kept
		}
		rest = after
		if next.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the PEM has a block %q: only certificates are taken (never send a private key)", next.Type)
		}
		c, err := x509.ParseCertificate(next.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a certificate cannot be read: %v", err)
		}
		out = append(out, c)
	}
	if strings.Contains(string(data), "PRIVATE KEY") {
		return nil, errors.New("the text has a private key: only certificates are taken")
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate in PEM form (-----BEGIN CERTIFICATE-----)")
	}
	return out, nil
}

// Describe tells of the certificates.
func Describe(certs []*x509.Certificate) []Certificate {
	out := make([]Certificate, 0, len(certs))
	for _, c := range certs {
		sum := sha256.Sum256(c.Raw)
		out = append(out, Certificate{Subject: c.Subject.String(), Issuer: c.Issuer.String(), SHA256: hex.EncodeToString(sum[:]), NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC()})
	}
	return out
}
