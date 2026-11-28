package server_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/testutil"
)

const (
	svcDN   = "cn=svc,dc=test"
	aliceDN = "cn=alice,dc=test"
	rootDN  = "cn=root,dc=test"
)

// directory gives the rig a false directory with alice (in the group Devs) and an entry that has the name of the local admin.
func (r *rig) directory(refresh string) *testutil.Directory {
	r.t.Helper()
	return r.directoryOf(testutil.NewDirectory(r.t, false), refresh)
}

// directoryOf fills a false directory and points the settings at it.
func (r *rig) directoryOf(d *testutil.Directory, refresh string) *testutil.Directory {
	r.t.Helper()
	d.Password(svcDN, "service secret")
	d.Password(aliceDN, "alice's directory password")
	d.Password(rootDN, "the directory's root password")
	d.Answer("(uid=alice)", map[string]string{"dn": aliceDN})
	d.Answer("(uid=root)", map[string]string{"dn": rootDN})
	d.Answer("(uid=ci-build)", map[string]string{"dn": rootDN})
	d.Answer("(member="+aliceDN+")", map[string]string{"dn": "cn=devs,dc=test", "cn": "Devs"})
	r.t.Setenv("TEST_LDAP_PASSWORD", "service secret")
	text := fmt.Sprintf(`{"repositories": [{"name": "files", "format": "raw", "kind": "hosted"}],
	 "grants": [{"group": "devs", "repositories": ["files"], "paths": ["dev/**"], "actions": ["read", "write"]},
	            {"group": "ops", "repositories": ["files"], "paths": ["ops/**"], "actions": ["read", "write"]}],
	 "ldap": {"url": %q, "ca_file": %q, "bind_dn": %q, "password_env": "TEST_LDAP_PASSWORD", "user_base": "dc=test", "user_filter": "(uid={user})", "group_base": "dc=test", "refresh": %q}}`,
		d.URL(), d.CAFile, svcDN, refresh)
	set, err := settings.Parse(strings.NewReader(text))
	if err != nil {
		r.t.Fatal(err)
	}
	r.srv.Settings.Store(set)
	return d
}

func (r *rig) loginToken(account, password string) string {
	r.t.Helper()
	_, b := r.want(201, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": %q, "password": %q}`, account, password))
	var got struct{ Token string }
	jsonOf(r.t, b, &got)
	return got.Token
}

func TestPeopleOfADirectory(t *testing.T) {
	r := newRig(t)
	d := r.directory("5m")
	const pw = "alice's directory password"

	tok := r.loginToken("Alice", pw)
	_, b := r.want(200, "GET", "/api/v1/whoami", tok, "")
	if !strings.Contains(string(b), `"account":"alice"`) || strings.Contains(string(b), `"admin":true`) {
		t.Fatalf("%s", b)
	}
	// the grants of her group, and no others
	r.want(201, "PUT", "/repository/files/dev/a", tok, "x")
	r.want(403, "PUT", "/repository/files/ops/a", tok, "x")
	// basic authentication with the directory's password; the directory is not asked again at once
	if code, _, _ := r.basic("GET", "/repository/files/dev/a", "alice", pw, ""); code != 200 {
		t.Fatalf("basic: %d", code)
	}
	binds := d.Binds.Load()
	for range 3 {
		r.basic("GET", "/repository/files/dev/a", "ALICE", pw, "")
	}
	r.want(200, "GET", "/repository/files/dev/a", tok, "")
	if d.Binds.Load() != binds {
		t.Fatal("the directory was asked again for what it had just said")
	}
	_, b = r.want(200, "GET", "/api/v1/accounts", r.admin, "")
	var list struct {
		Items []struct{ Name, Kind, Source string }
	}
	_ = json.Unmarshal(b, &list)
	if !strings.Contains(string(b), `"source":"ldap"`) {
		t.Fatalf("%s", b)
	}
	// her password is not one that godepot sets or changes
	r.want(400, "PUT", "/api/v1/accounts/alice/password", r.admin, `{"password": "another password here"}`)
	r.want(400, "POST", "/api/v1/password", "", fmt.Sprintf(`{"account": "alice", "password": %q, "new_password": "another password here"}`, pw))
	r.want(409, "POST", "/api/v1/accounts", r.admin, `{"name": "alice"}`)

	// the directory moves her from devs to ops: within the time of refresh the token made before has the new rights
	d.Answer("(member="+aliceDN+")", map[string]string{"dn": "cn=ops,dc=test", "cn": "ops"})
	r.want(201, "PUT", "/repository/files/dev/b", tok, "x")
	r.clock.Add(5*60 + 1)
	r.want(403, "PUT", "/repository/files/dev/c", tok, "x")
	r.want(201, "PUT", "/repository/files/ops/a", tok, "x")

	// the directory cannot be asked: what is kept is used for an hour, then nothing
	d.Down.Store(true)
	r.clock.Add(6 * 60)
	r.want(200, "GET", "/repository/files/ops/a", tok, "")
	r.clock.Add(60 * 60)
	h, _ := r.want(503, "GET", "/repository/files/ops/a", tok, "")
	if h.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	r.want(503, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": "alice", "password": %q}`, pw))
	if code, _, _ := r.basic("GET", "/repository/files/ops/a", "alice", "some other password", ""); code != 503 {
		t.Fatalf("basic with the directory down: %d", code)
	}
	// local accounts do not need the directory (the admin's token of the rig has ended with the hour that passed)
	r.admin = r.token("root", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Admin}})
	r.want(204, "PUT", "/api/v1/accounts/reader/password", r.admin, `{"password": "reader has a password"}`)
	r.loginToken("reader", "reader has a password")
	d.Down.Store(false)
	r.want(200, "GET", "/repository/files/ops/a", tok, "")

	// she is removed from the directory (or disabled and the filter no longer takes her): the token dies
	d.Answer("(uid=alice)")
	r.clock.Add(5*60 + 1)
	r.want(401, "GET", "/repository/files/ops/a", tok, "")
	d.Answer("(uid=alice)", map[string]string{"dn": aliceDN})
	r.want(200, "GET", "/repository/files/ops/a", tok, "")

	// disabled in godepot: neither her token nor her password
	r.want(204, "POST", "/api/v1/accounts/alice/disable", r.admin, "")
	r.want(401, "GET", "/repository/files/ops/a", tok, "")
	binds = d.Binds.Load()
	r.want(401, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": "alice", "password": %q}`, pw))
	if d.Binds.Load() != binds {
		t.Fatal("the password of a disabled account was sent to the directory")
	}
	r.want(204, "POST", "/api/v1/accounts/alice/enable", r.admin, "")

	// the ldap section is taken away: accounts of the directory are nobody
	tok = r.loginToken("alice", pw)
	set := *r.srv.Settings.Load()
	set.LDAP = nil
	r.srv.Settings.Store(&set)
	r.want(401, "GET", "/api/v1/whoami", tok, "")
	r.want(401, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": "alice", "password": %q}`, pw))

	for _, secret := range []string{pw, "service secret"} {
		if strings.Contains(r.log.String(), secret) {
			t.Fatalf("a password is in the log")
		}
	}
}

func TestTheDirectoryDoesNotTakeOverLocalAccounts(t *testing.T) {
	r := newRig(t)
	d := r.directory("5m")
	// root (a local admin) and ci-build (a robot) are also names in the directory, with a password
	for _, who := range []string{"root", "Root", "ROOT", "ci-build"} {
		r.want(401, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": %q, "password": "the directory's root password"}`, who))
	}
	if d.Binds.Load() != 0 {
		t.Fatal("the directory was asked about a local account")
	}
	// a name the directory lacks, or that cannot be an account's, is refused like a wrong password, and makes no account
	for _, who := range []string{"mallory", "alice@test.local", "TEST\\\\alice"} {
		r.want(401, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": "%s", "password": "the directory's root password"}`, who))
	}
	rows, err := r.db.Query(t.Context(), metaQuery(`SELECT COUNT(*) FROM accounts WHERE source = 'ldap'`))
	if err != nil || metaInt(rows[0][0]) != 0 {
		t.Fatal(rows, err)
	}
}

func TestGuessesAreStoppedBeforeTheDirectory(t *testing.T) {
	r := newRig(t)
	d := r.directory("5m")
	login := func(who, pw string, want int) {
		t.Helper()
		r.want(want, "POST", "/api/v1/login", "", fmt.Sprintf(`{"account": %q, "password": %q}`, who, pw))
	}
	for range 5 {
		login("alice", "a wrong guess", 401)
	}
	binds := d.Binds.Load()
	login("alice", "alice's directory password", 429)
	login("alice", "a wrong guess", 429)
	if d.Binds.Load() != binds {
		t.Fatal("a locked name still reaches the directory")
	}
	r.clock.Add(int64(time.Minute/time.Second) + 1)
	login("alice", "alice's directory password", 201)
	// invented names are not counted: the table of the locks does not fill with them
	for range 6 {
		login("mallory", "a wrong guess", 401)
	}
	rows, err := r.db.Query(t.Context(), metaQuery(`SELECT COUNT(*) FROM login_state`))
	if err != nil || metaInt(rows[0][0]) != 0 {
		t.Fatal(rows, err)
	}
}

func metaQuery(sql string) meta.Stmt { return meta.S(sql) }
func metaInt(v any) int64            { return meta.Int(v) }

// A password that was checked lately is not checked again, but the person is: removed from the directory, the password stops within "refresh".
func TestARememberedPasswordDoesNotKeepARemovedPersonIn(t *testing.T) {
	r := newRig(t)
	d := r.directory("1m")
	r.loginToken("alice", "alice's directory password")
	if code, _, _ := r.basic("GET", "/repository/files/dev/none", "alice", "alice's directory password", ""); code != 404 {
		t.Fatalf("%d", code)
	}
	d.Answer("(uid=alice)")
	r.clock.Add(61)
	if code, _, _ := r.basic("GET", "/repository/files/dev/none", "alice", "alice's directory password", ""); code != 401 {
		t.Fatalf("a removed person with a remembered password: %d", code)
	}
}

// The password of the service account may be in a file: it is read at each use, so a new one needs no restart, and one that cannot be read is a directory that cannot be asked.
func TestTheServicePasswordFromAFile(t *testing.T) {
	r := newRig(t)
	d := r.directory("5m")
	file := filepath.Join(t.TempDir(), "ldap-password")
	t.Setenv("TEST_LDAP_PASSWORD", "")
	t.Setenv("TEST_LDAP_PASSWORD_FILE", file)
	write := func(s string) {
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	login := func(want int) {
		t.Helper()
		r.want(want, "POST", "/api/v1/login", "", `{"account": "alice", "password": "alice's directory password"}`)
	}
	login(503) // no file yet
	if !strings.Contains(r.log.String(), "TEST_LDAP_PASSWORD_FILE") {
		t.Fatal("the log does not say which file cannot be read")
	}
	write("service secret\n")
	login(201)
	d.Password(svcDN, "the new service secret") // changed in the directory
	r.clock.Add(11 * 60)                        // past the time a checked password is remembered
	login(503)
	write("the new service secret\n")
	login(201)
	if strings.Contains(r.log.String(), "service secret") {
		t.Fatal("a password is in the log")
	}
}
