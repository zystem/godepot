package server_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
)

func init() { auth.Rounds = 1000 }

func (r *rig) basic(method, path, user, pass string, body string) (int, http.Header, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header, []byte(b.String())
}

func TestPasswords(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	// An admin sets the password of a user; a robot has none.
	r.want(204, "PUT", "/api/v1/accounts/reader/password", r.admin, `{"password": "reader has a password"}`)
	r.want(400, "PUT", "/api/v1/accounts/ci-build/password", r.admin, `{"password": "a robot has none ok"}`)
	r.want(400, "PUT", "/api/v1/accounts/reader/password", r.admin, `{"password": "short"}`)
	r.want(403, "PUT", "/api/v1/accounts/reader/password", r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read}}), `{"password": "reader sets it for self"}`)
	// The password is the password of basic authentication: the data actions of the account, not the admin API.
	r.want(201, "PUT", "/repository/files/p/a.txt", r.admin, "x")
	if code, _, b := r.basic("GET", "/repository/files/p/a.txt", "reader", "reader has a password", ""); code != 200 || string(b) != "x" {
		t.Fatalf("a password in basic authentication: %d %s", code, b)
	}
	if code, _, _ := r.basic("GET", "/api/v1/accounts", "reader", "reader has a password", ""); code != 403 && code != 401 {
		t.Fatalf("the admin API with a password: %d", code)
	}
	root := "root"
	r.want(204, "PUT", "/api/v1/accounts/"+root+"/password", r.admin, `{"password": "root has a password!"}`)
	if code, _, _ := r.basic("GET", "/api/v1/accounts", root, "root has a password!", ""); code != 403 {
		t.Fatalf("an admin account's password does not open the admin API: %d", code)
	}
	// A wrong password is 401, with the same words whoever is asked about; a token in the password place still works with any name.
	for _, c := range [][2]string{{"reader", "not the password at all"}, {"nobody", "not the password at all"}} {
		if code, h, b := r.basic("GET", "/repository/files/p/a.txt", c[0], c[1], ""); code != 401 || h.Get("WWW-Authenticate") == "" || strings.Contains(string(b), c[0]) {
			t.Fatalf("%v: %d %s", c, code, b)
		}
	}
	tok := r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read}})
	if code, _, _ := r.basic("GET", "/repository/files/p/a.txt", "whatever", tok, ""); code != 200 {
		t.Fatalf("a token as the password: %d", code)
	}

	// login: the password for a token, which can be narrowed and always ends.
	_, b := r.want(201, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a password", "actions": ["read"], "repositories": ["files"], "life_seconds": 600}`)
	var lg struct {
		Token     string
		Account   string
		ExpiresAt int64 `json:"expires_at"`
	}
	if err := json.Unmarshal(b, &lg); err != nil || !strings.HasPrefix(lg.Token, "gd_") || lg.Account != "reader" || lg.ExpiresAt-time.Now().Unix() > 700 {
		t.Fatalf("login: %s %v", b, err)
	}
	r.want(200, "GET", "/repository/files/p/a.txt", lg.Token, "")
	r.want(403, "PUT", "/repository/files/p/b.txt", lg.Token, "y")
	_, b = r.want(201, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a password"}`)
	_ = json.Unmarshal(b, &lg)
	if lg.ExpiresAt-time.Now().Unix() < 11*3600 { // the default is 12 hours
		t.Fatalf("the default life: %d", lg.ExpiresAt-time.Now().Unix())
	}
	r.want(401, "POST", "/api/v1/login", "", `{"account": "reader", "password": "wrong password here"}`)
	r.want(400, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a password", "life_seconds": 99999999}`)
	r.want(405, "GET", "/api/v1/login", "", "")
	// Five wrong passwords lock the account: 429 with Retry-After, also for the right password, and for the same account over basic authentication.
	for i := 0; i < auth.LockAfter; i++ {
		r.want(401, "POST", "/api/v1/login", "", `{"account": "reader", "password": "wrong password here"}`)
	}
	h, _ := r.want(429, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a password"}`)
	if h.Get("Retry-After") == "" {
		t.Fatal("no Retry-After on a lock")
	}
	if code, h, _ := r.basic("GET", "/repository/files/p/a.txt", "reader", "reader has a password", ""); code != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("basic authentication of a locked account: %d", code)
	}
	// A token of the account is not touched by the lock of its password.
	r.want(200, "GET", "/repository/files/p/a.txt", tok, "")
	// A user changes their own password with the old one, no token.
	r.clock.Add(int64(auth.LockFor/time.Second) + 1)
	r.want(204, "POST", "/api/v1/password", "", `{"account": "reader", "password": "reader has a password", "new_password": "reader has a new one"}`)
	r.want(400, "POST", "/api/v1/password", "", `{"account": "reader", "password": "reader has a new one", "new_password": "short"}`)
	r.want(401, "POST", "/api/v1/password", "", `{"account": "reader", "password": "reader has a password", "new_password": "never gets set ok"}`)
	r.want(201, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a new one"}`)
	// An admin clears it.
	r.want(204, "DELETE", "/api/v1/accounts/reader/password", r.admin, "")
	r.want(401, "POST", "/api/v1/login", "", `{"account": "reader", "password": "reader has a new one"}`)
	// A flood of wrong passwords for many accounts from one address is turned away before the hash is spent.
	for i := 0; i < 40; i++ {
		r.basic("GET", "/repository/files/p/a.txt", "nobody"+itoaT(i), "not the password at all", "")
	}
	if code, h, _ := r.basic("GET", "/repository/files/p/a.txt", "nobody-else", "not the password at all", ""); code != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("the address was not turned away: %d", code)
	}
	// The audit log has the logins, the refusals and the changes, and no password.
	_, ab := r.want(200, "GET", "/api/v1/audit?limit=200", r.admin, "")
	for _, w := range []string{`"login"`, `"password.set"`, `"password.change"`, `"password.clear"`} {
		if !strings.Contains(string(ab), w) {
			t.Errorf("the audit log has no %s", w)
		}
	}
	for _, secret := range []string{"reader has a password", "reader has a new one", "wrong password here", "root has a password!"} {
		if strings.Contains(string(ab), secret) || strings.Contains(r.log.String(), secret) {
			t.Errorf("the password %q is in the audit log or the log", secret)
		}
	}
	_ = ctx
}

func itoaT(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
