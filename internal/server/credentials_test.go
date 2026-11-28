package server_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/seal"
	"github.com/zystem/godepot/internal/settings"
)

// sealed gives the rig a seal key, as serve does.
func (r *rig) sealed() *seal.Store {
	r.t.Helper()
	key, _, err := seal.Open(context.Background(), r.db, filepath.Join(r.t.TempDir(), "seal.key"), r.auth.Clock())
	if err != nil {
		r.t.Fatal(err)
	}
	r.srv.Secrets = &seal.Store{DB: r.db, Key: key, Now: r.auth.Now}
	return r.srv.Secrets
}

func TestCredentialsByTheAPI(t *testing.T) {
	r := newRig(t)
	r.want(404, "GET", "/api/v1/credentials", r.admin, "") // a server with no store of secrets
	store := r.sealed()
	set, err := settings.Parse(strings.NewReader(`{"repositories": [
	  {"name": "hub", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "username": "bot"}},
	  {"name": "env", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "token_env": "SOME_TOKEN"}},
	  {"name": "files", "format": "raw", "kind": "hosted"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Settings.Store(set)
	var changed []string
	r.srv.CredentialChanged = func(repo string) { changed = append(changed, repo) }

	const pw = "the password of the hub"
	reader := r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read}})
	for _, c := range []struct{ method, path string }{{"GET", "/api/v1/credentials"}, {"PUT", "/api/v1/credentials/upstream/hub"}, {"DELETE", "/api/v1/credentials/upstream/hub"}} {
		r.want(403, c.method, c.path, reader, pw)
		r.want(401, c.method, c.path, "", pw)
	}
	r.want(405, "POST", "/api/v1/credentials", r.admin, "")
	r.want(405, "GET", "/api/v1/credentials/upstream/hub", r.admin, "") // a secret is never told back
	r.want(204, "PUT", "/api/v1/credentials/upstream/hub", r.admin, pw+"\r\n")
	if v, _, err := store.Get(context.Background(), "upstream/hub"); err != nil || v != pw {
		t.Fatal(v, err)
	}
	_, b := r.want(200, "GET", "/api/v1/credentials", r.admin, "")
	if !strings.Contains(string(b), `"name":"upstream/hub","set_by":"root"`) || strings.Contains(string(b), pw) {
		t.Fatalf("%s", b)
	}
	for _, name := range []string{"upstream/files", "upstream/none", "ldap", "other", "upstream/hub/more"} {
		r.want(404, "PUT", "/api/v1/credentials/"+name, r.admin, pw)
	}
	_, b = r.want(409, "PUT", "/api/v1/credentials/upstream/env", r.admin, pw)
	if !strings.Contains(string(b), "SOME_TOKEN") {
		t.Fatalf("%s", b)
	}
	for _, body := range []string{"", "\n", "two\nlines", strings.Repeat("x", 8<<10+1)} {
		r.want(400, "PUT", "/api/v1/credentials/upstream/hub", r.admin, body)
	}
	r.want(204, "DELETE", "/api/v1/credentials/upstream/hub", r.admin, "")
	if _, found, err := store.Get(context.Background(), "upstream/hub"); err != nil || found {
		t.Fatal(found, err)
	}
	if fmt.Sprint(changed) != "[hub hub]" {
		t.Fatal(changed)
	}
	_, b = r.want(200, "GET", "/api/v1/audit?action=credential.set", r.admin, "")
	if !strings.Contains(string(b), "upstream/hub") || strings.Contains(string(b)+r.log.String(), pw) {
		t.Fatalf("%s", b)
	}

	// a replica whose key is not the one of the database any more writes nothing
	next, _, err := seal.MakeKey(filepath.Join(t.TempDir(), "next"))
	if err != nil {
		t.Fatal(err)
	}
	other := *store
	if _, err := other.Reseal(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	r.want(503, "PUT", "/api/v1/credentials/upstream/hub", r.admin, pw)
}

// The password of the directory's service account given with the API, when the settings name no variable.
func TestTheServicePasswordFromTheDatabase(t *testing.T) {
	r := newRig(t)
	r.sealed()
	d := r.directory("5m")
	set := *r.srv.Settings.Load()
	l := *set.LDAP
	l.PasswordEnv = ""
	set.LDAP = &l
	r.srv.Settings.Store(&set)
	login := func(want int) {
		t.Helper()
		r.want(want, "POST", "/api/v1/login", "", `{"account": "alice", "password": "alice's directory password"}`)
	}
	login(503) // nothing is kept yet
	r.want(204, "PUT", "/api/v1/credentials/ldap", r.admin, "service secret\n")
	login(201)
	d.Password(svcDN, "the new service secret")
	r.clock.Add(11 * 60)
	login(503)
	r.want(204, "PUT", "/api/v1/credentials/ldap", r.admin, "the new service secret")
	login(201)
	if strings.Contains(r.log.String(), "service secret") {
		t.Fatal("a password is in the log")
	}
}
