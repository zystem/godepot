package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/settings"
)

// The credentials of an upstream come from the variable or from the file of NAME_FILE, and a new one in the file makes a new client.
func TestCredentialsFromFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	write := func(s string) {
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_UP_PASSWORD_FILE", file)
	t.Setenv("TEST_UP_TOKEN", "a-token")
	pool := &Pool{}
	repo := &settings.Repo{Name: "hub", Upstream: &settings.Upstream{URL: "https://upstream.example", Username: "bot", PasswordEnv: "TEST_UP_PASSWORD"}}

	write("first\n")
	a, err := pool.Client(repo)
	if err != nil || a.user != "bot" || a.pass != "first" {
		t.Fatal(a, err)
	}
	if b, err := pool.Client(repo); err != nil || b != a {
		t.Fatal("a new client for the same password", err)
	}
	write("second\n")
	b, err := pool.Client(repo)
	if err != nil || b == a || b.pass != "second" {
		t.Fatal("the new password of the file was not taken", err)
	}
	// in this order: each case builds on the one before
	for _, c := range []struct {
		want   string
		change func()
	}{
		{"is not set", func() { write("\n") }},
		{"password_env", func() { _ = os.Remove(file) }},
		{"both set", func() { t.Setenv("TEST_UP_PASSWORD", "x") }},
	} {
		c.change()
		if _, err := pool.Client(repo); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("want an error with %q: %v", c.want, err)
		}
	}

	c, err := pool.Client(&settings.Repo{Name: "other", Upstream: &settings.Upstream{URL: "https://upstream.example", TokenEnv: "TEST_UP_TOKEN"}})
	if err != nil || c.token != "a-token" || c.pass != "" {
		t.Fatal(c, err)
	}
}

// A request that waits for the rate of a client goes on after the client was replaced.
func TestAReplacedClientStillTicks(t *testing.T) {
	c, err := New("hub", settings.Upstream{URL: "https://upstream.example", MaxPerSecond: 100}, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-c.tick.C:
	case <-time.After(5 * time.Second):
		t.Fatal("a request would wait for ever")
	}
}

type fakeSecrets struct {
	values map[string]string
	asked  int
	err    error
}

func (f *fakeSecrets) Get(_ context.Context, name string) (string, bool, error) {
	f.asked++
	v, ok := f.values[name]
	return v, ok, f.err
}

// With no variable in the settings the credential is the one kept in the database: read again after storedFor, or at once when it was changed here.
func TestCredentialsFromTheDatabase(t *testing.T) {
	db := &fakeSecrets{values: map[string]string{}}
	now := time.Unix(1_800_000_000, 0)
	pool := &Pool{Secrets: db, Now: func() time.Time { return now }}
	user := &settings.Repo{Name: "hub", Upstream: &settings.Upstream{URL: "https://upstream.example", Username: "bot"}}
	anon := &settings.Repo{Name: "open", Upstream: &settings.Upstream{URL: "https://upstream.example"}}

	if _, err := pool.Client(user); err == nil || !strings.Contains(err.Error(), "PUT /api/v1/credentials/upstream/hub") {
		t.Fatal("a user with no password", err)
	}
	c, err := pool.Client(anon)
	if err != nil || c.user != "" || c.pass != "" || c.token != "" {
		t.Fatal(c, err)
	}
	db.values["upstream/hub"], db.values["upstream/open"] = "first", "a-token"
	pool.Forgot("hub")
	if c, err = pool.Client(user); err != nil || c.user != "bot" || c.pass != "first" || c.token != "" {
		t.Fatal(c, err)
	}
	asked := db.asked
	if again, err := pool.Client(user); err != nil || again != c || db.asked != asked {
		t.Fatal("the database was asked again at once", err)
	}
	db.values["upstream/hub"] = "second"
	now = now.Add(storedFor)
	if c, err = pool.Client(user); err != nil || c.pass != "second" {
		t.Fatal("the new password was not taken", err)
	}
	if c, err = pool.Client(anon); err != nil || c.token != "a-token" || c.user != "" {
		t.Fatal(c, err)
	}
	db.err = errors.New("the database is gone")
	now = now.Add(storedFor)
	if _, err := pool.Client(user); err == nil || !strings.Contains(err.Error(), "the database is gone") {
		t.Fatal(err)
	}
}
