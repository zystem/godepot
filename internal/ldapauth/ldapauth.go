// Package ldapauth asks a directory (LDAP, Active Directory) who a person is and which groups they are in (docs/spec.md, A1, A4). It never talks in the clear: ldaps, or ldap with StartTLS.
//
// A login is two binds: the service account searches for the person, then the password is tried by binding as the entry that was found. An empty password is refused before anything is sent: a simple
// bind with a name and no password "succeeds" on many servers as an anonymous one.
package ldapauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

var (
	// ErrInvalid: no such person, or not the password. The text says which, for the audit log.
	ErrInvalid = errors.New("the login is not valid")
	// ErrPassword: the directory has the person and refuses the password. It is an ErrInvalid.
	ErrPassword = fmt.Errorf("%w: the directory refuses the password", ErrInvalid)
	// ErrUnavailable: the directory cannot be asked now (it does not answer, its certificate is not trusted, the service account is refused).
	ErrUnavailable = errors.New("the directory cannot be asked")
)

// Config is where the directory is and how to find people in it. See the settings file.
type Config struct {
	URL          string
	StartTLS     bool
	CAFile       string
	CA           []byte // PEM of certificates trusted besides those of CAFile; with either, those of the system are not
	ServerName   string
	BindDN       string
	BindPassword string
	// BindError says why the password of the service account could not be read; the directory is then not asked.
	BindError   error
	UserBase    string
	UserFilter  string // "{user}" is the name that logs in, escaped
	GroupBase   string // "": no groups are read
	GroupFilter string // "{dn}" is the entry of the person, "{user}" the name, both escaped
	GroupName   string // the attribute of a group that is its name
}

const (
	dialTimeout    = 5 * time.Second
	requestTimeout = 10 * time.Second
	maxGroups      = 1000
)

func unavailable(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUnavailable}, a...)...)
}

func (c Config) tlsConfig() (*tls.Config, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if t.ServerName == "" {
		t.ServerName = u.Hostname()
	}
	var given []*x509.Certificate
	if c.CAFile != "" {
		data, err := os.ReadFile(c.CAFile) // #nosec G304 -- the path is the operator's own setting
		if err != nil {
			return nil, err
		}
		if given = certificates(data); len(given) == 0 {
			return nil, fmt.Errorf("%s holds no certificate", c.CAFile)
		}
	}
	if len(c.CA) > 0 {
		stored := certificates(c.CA)
		if len(stored) == 0 {
			return nil, errors.New("the stored CA holds no certificate")
		}
		given = append(given, stored...)
	}
	if len(given) > 0 {
		t.RootCAs = x509.NewCertPool()
		for _, g := range given {
			t.RootCAs.AddCert(g)
		}
		t.InsecureSkipVerify = true // #nosec G402 -- VerifyConnection checks the chain against the given certificates, and the name except for a server's certificate that was given itself
		t.VerifyConnection = verify(t.RootCAs, given, t.ServerName, c.ServerName != "")
	}
	return t, nil
}

func certificates(data []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for b, rest := pem.Decode(data); b != nil; b, rest = pem.Decode(rest) {
		if c, err := x509.ParseCertificate(b.Bytes); err == nil && b.Type == "CERTIFICATE" {
			out = append(out, c)
		}
	}
	return out
}

// verify checks the server's certificate against the given ones: it is one of them or is signed by one, is in its time and is for a server. The name: a server's own certificate that was given is believed
// for what it is, whatever address reached it (who added it believes it and not the DNS), unless askName; one that is only signed by a given authority must have the name, since the authority signs
// for many machines.
func verify(roots *x509.CertPool, given []*x509.Certificate, name string, askName bool) func(tls.ConnectionState) error {
	return func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) == 0 {
			return errors.New("tls: the server sent no certificate")
		}
		leaf := s.PeerCertificates[0]
		between := x509.NewCertPool()
		for _, c := range s.PeerCertificates[1:] {
			between.AddCert(c)
		}
		o := x509.VerifyOptions{Roots: roots, Intermediates: between, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSName: name}
		if !askName && slices.ContainsFunc(given, leaf.Equal) {
			o.DNSName = ""
		}
		if _, err := leaf.Verify(o); err != nil {
			return fmt.Errorf("tls: failed to verify certificate: %w", err)
		}
		return nil
	}
}

// connect dials, secures the connection and binds the service account.
func (c Config) connect(ctx context.Context) (*ldap.Conn, error) {
	t, err := c.tlsConfig()
	if err != nil {
		return nil, unavailable("%v", err)
	}
	secure := strings.HasPrefix(c.URL, "ldaps://")
	if !secure && !(c.StartTLS && strings.HasPrefix(c.URL, "ldap://")) {
		return nil, unavailable("the address is not ldaps, and not ldap with start_tls: nothing is sent in the clear")
	}
	d := &net.Dialer{Timeout: dialTimeout}
	if deadline, ok := ctx.Deadline(); ok {
		d.Deadline = deadline
	}
	conn, err := ldap.DialURL(c.URL, ldap.DialWithDialer(d), ldap.DialWithTLSConfig(t))
	if err != nil {
		return nil, unavailable("%v", err)
	}
	conn.SetTimeout(requestTimeout)
	if !secure {
		if err := conn.StartTLS(t); err != nil {
			_ = conn.Close()
			return nil, unavailable("StartTLS: %v", err)
		}
	}
	if c.BindError != nil {
		_ = conn.Close()
		return nil, unavailable("the password of the service account cannot be read: %v", c.BindError)
	}
	if c.BindPassword == "" {
		_ = conn.Close()
		return nil, unavailable("the service account has no password (the environment variable or its file is empty)")
	}
	if err := conn.Bind(c.BindDN, c.BindPassword); err != nil {
		_ = conn.Close()
		return nil, unavailable("the bind of the service account: %v", err)
	}
	return conn, nil
}

// find searches the one entry of a name.
func (c Config) find(conn *ldap.Conn, user string) (string, error) {
	filter := strings.ReplaceAll(c.UserFilter, "{user}", ldap.EscapeFilter(user))
	res, err := conn.Search(ldap.NewSearchRequest(c.UserBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(requestTimeout/time.Second), false, filter, []string{"1.1"}, nil))
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return "", unavailable("the search of the user: %v", err)
	}
	switch n := len(res.Entries); {
	case err != nil || n > 1:
		return "", fmt.Errorf("%w: more than one entry fits the name %q", ErrInvalid, user)
	case n == 0:
		return "", fmt.Errorf("%w: no entry for the name %q", ErrInvalid, user)
	}
	return res.Entries[0].DN, nil
}

func (c Config) groups(conn *ldap.Conn, dn, user string) ([]string, error) {
	if c.GroupBase == "" {
		return nil, nil
	}
	filter := strings.NewReplacer("{dn}", ldap.EscapeFilter(dn), "{user}", ldap.EscapeFilter(user)).Replace(c.GroupFilter)
	res, err := conn.SearchWithPaging(ldap.NewSearchRequest(c.GroupBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, int(requestTimeout/time.Second), false, filter, []string{c.GroupName}, nil), 500)
	if err != nil {
		return nil, unavailable("the search of the groups: %v", err)
	}
	var out []string
	for _, e := range res.Entries {
		if name := e.GetEqualFoldAttributeValue(c.GroupName); name != "" {
			out = append(out, name)
		}
	}
	if len(out) > maxGroups {
		return nil, unavailable("the user is in more than %d groups", maxGroups)
	}
	slices.Sort(out)
	return out, nil
}

// Person is what the directory says of one.
type Person struct {
	DN     string
	Groups []string
}

// Lookup finds a person and their groups without their password: ErrInvalid when there is none (or the user filter no longer takes them, for one that is disabled).
func (c Config) Lookup(ctx context.Context, user string) (*Person, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return c.person(conn, user)
}

func (c Config) person(conn *ldap.Conn, user string) (*Person, error) {
	dn, err := c.find(conn, user)
	if err != nil {
		return nil, err
	}
	g, err := c.groups(conn, dn, user)
	if err != nil {
		return nil, err
	}
	return &Person{DN: dn, Groups: g}, nil
}

// Authenticate checks the password of a person and says who they are.
func (c Config) Authenticate(ctx context.Context, user, password string) (*Person, error) {
	if user == "" || password == "" {
		return nil, fmt.Errorf("%w: an empty name or password", ErrInvalid)
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// the groups are read as the service account, before the connection is bound as the person
	p, err := c.person(conn, user)
	if err != nil {
		return nil, err
	}
	if err := conn.Bind(p.DN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return nil, fmt.Errorf("%w of %q", ErrPassword, user)
		}
		return nil, unavailable("the bind of the user: %v", err)
	}
	return p, nil
}
