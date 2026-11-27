package ldapauth_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zystem/godepot/internal/ldapauth"
	"github.com/zystem/godepot/internal/testutil"
)

const (
	svc   = "cn=svc,dc=test"
	alice = "cn=Alice,ou=people,dc=test"
)

func directory(t *testing.T, startTLS bool) (*testutil.Directory, ldapauth.Config) {
	d := testutil.NewDirectory(t, startTLS)
	d.Password(svc, "service secret")
	d.Password(alice, "alice secret")
	d.Answer("(uid=alice)", map[string]string{"dn": alice})
	d.Answer("(member="+alice+")", map[string]string{"dn": "cn=Devs,dc=test", "cn": "Devs"}, map[string]string{"dn": "cn=all,dc=test", "CN": "all"}, map[string]string{"dn": "cn=nameless,dc=test"})
	return d, ldapauth.Config{URL: d.URL(), StartTLS: startTLS, CAFile: d.CAFile, BindDN: svc, BindPassword: "service secret",
		UserBase: "dc=test", UserFilter: "(uid={user})", GroupBase: "dc=test", GroupFilter: "(member={dn})", GroupName: "cn"}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	for _, startTLS := range []bool{false, true} {
		d, c := directory(t, startTLS)
		p, err := c.Authenticate(ctx, "alice", "alice secret")
		if err != nil || p.DN != alice || strings.Join(p.Groups, ",") != "Devs,all" {
			t.Fatalf("startTLS=%v: %+v %v", startTLS, p, err)
		}
		if seen := d.Cleartext(); len(seen) != 0 {
			t.Fatalf("sent in the clear: %v", seen)
		}
		if _, err := c.Authenticate(ctx, "alice", "wrong"); !errors.Is(err, ldapauth.ErrPassword) || !errors.Is(err, ldapauth.ErrInvalid) {
			t.Fatalf("a wrong password: %v", err)
		}
		if _, err := c.Authenticate(ctx, "nobody", "alice secret"); !errors.Is(err, ldapauth.ErrInvalid) || errors.Is(err, ldapauth.ErrPassword) {
			t.Fatalf("an unknown name: %v", err)
		}
		if p, err := c.Lookup(ctx, "alice"); err != nil || len(p.Groups) != 2 {
			t.Fatalf("lookup: %+v %v", p, err)
		}
		if _, err := c.Lookup(ctx, "nobody"); !errors.Is(err, ldapauth.ErrInvalid) {
			t.Fatalf("lookup of nobody: %v", err)
		}
		noGroups := c
		noGroups.GroupBase = ""
		if p, err := noGroups.Authenticate(ctx, "alice", "alice secret"); err != nil || p.Groups != nil {
			t.Fatalf("no groups asked: %+v %v", p, err)
		}
	}
}

func TestWhatIsRefusedBeforeAndByTheDirectory(t *testing.T) {
	ctx := context.Background()
	d, c := directory(t, false)
	// an empty password would be an anonymous bind that "succeeds"
	for _, pair := range [][2]string{{"alice", ""}, {"", "alice secret"}} {
		if _, err := c.Authenticate(ctx, pair[0], pair[1]); !errors.Is(err, ldapauth.ErrInvalid) {
			t.Fatalf("%q: %v", pair, err)
		}
	}
	if d.Binds.Load() != 0 {
		t.Fatal("something was sent for an empty name or password")
	}
	// a name cannot change the filter
	_, _ = c.Authenticate(ctx, "*)(uid=*", "x")
	if want := `(uid=\2a\29\28uid=\2a)`; !strings.Contains(d.Filters(), want) {
		t.Fatalf("the name was not escaped: %s", d.Filters())
	}
	// two entries for one name: nobody is chosen
	d.Answer("(uid=twin)", map[string]string{"dn": alice}, map[string]string{"dn": "cn=other,dc=test"})
	if _, err := c.Authenticate(ctx, "twin", "alice secret"); !errors.Is(err, ldapauth.ErrInvalid) || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("two entries: %v", err)
	}
	unavailable := func(name string, change func(*ldapauth.Config), want string) {
		t.Helper()
		x := c
		change(&x)
		if _, err := x.Authenticate(ctx, "alice", "alice secret"); !errors.Is(err, ldapauth.ErrUnavailable) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	before := d.Binds.Load()
	unavailable("a certificate that is not trusted", func(x *ldapauth.Config) { x.CAFile = "" }, "certificate")
	unavailable("another server name", func(x *ldapauth.Config) { x.ServerName = "other.test" }, "certificate")
	unavailable("in the clear", func(x *ldapauth.Config) { x.URL = "ldap://" + d.Addr }, "in the clear")
	unavailable("no password of the service account", func(x *ldapauth.Config) { x.BindPassword = "" }, "empty")
	unavailable("no CA file", func(x *ldapauth.Config) { x.CAFile = "/nonexistent" }, "nonexistent")
	if d.Binds.Load() != before {
		t.Fatal("a password was sent to a directory that could not be trusted, or in the clear")
	}
	unavailable("a wrong password of the service account", func(x *ldapauth.Config) { x.BindPassword = "nope" }, "service account")
	named := c
	named.ServerName = "dc.test"
	if _, err := named.Authenticate(ctx, "alice", "alice secret"); err != nil {
		t.Fatalf("server_name of the certificate: %v", err)
	}
	d.Down.Store(true)
	unavailable("down", func(*ldapauth.Config) {}, "")
}

// A server's own certificate (not an authority's) is trusted when it is given, from a file or as bytes, and only then.
func TestASelfSignedCertificate(t *testing.T) {
	ctx := context.Background()
	d := testutil.NewSelfSigned(t)
	d.Password(svc, "service secret")
	d.Password(alice, "alice secret")
	d.Answer("(uid=alice)", map[string]string{"dn": alice})
	c := ldapauth.Config{URL: d.URL(), BindDN: svc, BindPassword: "service secret", UserBase: "dc=test", UserFilter: "(uid={user})"}
	if _, err := c.Authenticate(ctx, "alice", "alice secret"); !errors.Is(err, ldapauth.ErrUnavailable) || d.Binds.Load() != 0 {
		t.Fatalf("not trusted: %v", err)
	}
	pem, err := os.ReadFile(d.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	byFile, byBytes := c, c
	byFile.CAFile, byBytes.CA = d.CAFile, pem
	for name, x := range map[string]ldapauth.Config{"file": byFile, "bytes": byBytes} {
		if _, err := x.Authenticate(ctx, "alice", "alice secret"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the file and the bytes are both trusted: the bytes of another authority do not take the file's away
	_, other := directory(t, false)
	both := byFile
	if both.CA, err = os.ReadFile(other.CAFile); err != nil {
		t.Fatal(err)
	}
	if _, err := both.Authenticate(ctx, "alice", "alice secret"); err != nil {
		t.Errorf("file and bytes: %v", err)
	}
	// the certificate is believed, not the name: the address is one that it does not have
	elsewhere := byBytes
	elsewhere.URL = strings.Replace(d.URL(), "127.0.0.1", "localhost", 1)
	if _, err := elsewhere.Authenticate(ctx, "alice", "alice secret"); err != nil {
		t.Errorf("another address: %v", err)
	}
	stranger := elsewhere
	if stranger.CA, err = os.ReadFile(other.CAFile); err != nil {
		t.Fatal(err)
	}
	binds := d.Binds.Load()
	if _, err := stranger.Authenticate(ctx, "alice", "alice secret"); !errors.Is(err, ldapauth.ErrUnavailable) || d.Binds.Load() != binds {
		t.Errorf("another certificate of the same name: %v", err)
	}
	broken := c
	broken.CA = []byte("nothing")
	if _, err := broken.Authenticate(ctx, "alice", "alice secret"); !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Errorf("broken: %v", err)
	}
}
