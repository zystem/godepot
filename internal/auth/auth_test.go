package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/testutil"
)

func store(t *testing.T) (*auth.Store, *atomic.Int64) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(1_800_000_000)
	return &auth.Store{DB: testutil.DB(t), Now: func() time.Time { return time.Unix(clock.Load(), 0) }}, &clock
}

var all = []auth.Grant{{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write}}}

func TestTokenLife(t *testing.T) {
	ctx := context.Background()
	s, clock := store(t)
	if err := s.CreateAccount(ctx, "ci-build", "robot", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(ctx, "ci-build", "robot", false); !errors.Is(err, auth.ErrExists) {
		t.Fatalf("a second account of the name: %v", err)
	}
	tok, info, err := s.CreateToken(ctx, "ci-build", all, time.Hour, "for the build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "gd_"+info.ID+"_") {
		t.Fatalf("the token %q does not carry its id %s", tok, info.ID)
	}
	p, err := s.Authenticate(ctx, tok)
	if err != nil || p.Account != "ci-build" || p.Token != info.ID || p.Admin {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if !p.ScopeAllows("files", "a/b", auth.Write) || p.ScopeAllows("files", "a/b", auth.Delete) {
		t.Fatal("the scope of the token is not what it was made with")
	}

	// the secret is not kept: the database holds the digest only
	rows, err := s.DB.Query(ctx, mustStmt(`SELECT digest FROM tokens`))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	secret := strings.SplitN(tok, "_", 3)[2]
	if strings.Contains(rows[0][0].(string), secret) || rows[0][0].(string) == secret {
		t.Fatal("the secret is in the database")
	}

	// a wrong secret, a wrong id, text that is not a token
	for _, bad := range []string{tok + "x", "gd_" + info.ID + "_" + strings.Repeat("a", 43), "gd_000000000000_" + secret, "nonsense", "", "gd_", "gd_" + info.ID} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}

	// it ends
	clock.Add(3599)
	if _, err := s.Authenticate(ctx, tok); err != nil {
		t.Fatalf("one second before the end: %v", err)
	}
	clock.Add(1)
	if _, err := s.Authenticate(ctx, tok); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("at the end: %v", err)
	}
}

func TestTrapT31TokenNeedsAnEndAndAScope(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	s.MaxLife = 24 * time.Hour
	if err := s.CreateAccount(ctx, "robot", "robot", false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		scope []auth.Grant
		life  time.Duration
	}{
		{"no end date", all, 0},
		{"longer than the longest", all, 25 * time.Hour},
		{"no scope", nil, time.Hour},
		{"a bad scope", []auth.Grant{{Repositories: []string{"x"}}}, time.Hour},
	} {
		if _, _, err := s.CreateToken(ctx, "robot", c.scope, c.life, ""); err == nil {
			t.Errorf("%s: a token was made", c.name)
		}
	}
	if _, _, err := s.CreateToken(ctx, "nobody", all, time.Hour, ""); !errors.Is(err, auth.ErrNoAccount) {
		t.Errorf("a token for an account that is not there: %v", err)
	}
	if err := s.CreateAccount(ctx, "robot$ci", "robot", false); err == nil {
		t.Error("a name with $")
	}
	if err := s.CreateAccount(ctx, "x", "wizard", false); err == nil {
		t.Error("a kind that is not")
	}
}

func TestRevokeAndDisable(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	if err := s.CreateAccount(ctx, "ann", "user", true); err != nil {
		t.Fatal(err)
	}
	t1, i1, _ := s.CreateToken(ctx, "ann", all, time.Hour, "")
	t2, _, _ := s.CreateToken(ctx, "ann", all, time.Hour, "")
	p, err := s.Authenticate(ctx, t1)
	if err != nil || !p.Admin {
		t.Fatalf("%+v %v", p, err)
	}
	if err := s.Revoke(ctx, i1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, i1.ID); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("revoked twice: %v", err)
	}
	if _, err := s.Authenticate(ctx, t1); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("a revoked token: %v", err)
	}
	if _, err := s.Authenticate(ctx, t2); err != nil {
		t.Fatalf("the other token of the account: %v", err)
	}
	if err := s.SetDisabled(ctx, "ann", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, t2); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("a token of a disabled account: %v", err)
	}
	if _, _, err := s.CreateToken(ctx, "ann", all, time.Hour, ""); !errors.Is(err, auth.ErrNoAccount) {
		t.Fatalf("a token for a disabled account: %v", err)
	}
	if err := s.SetDisabled(ctx, "ann", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, t2); err != nil {
		t.Fatalf("enabled again: %v", err)
	}
	if err := s.SetDisabled(ctx, "nobody", true); !errors.Is(err, auth.ErrNoAccount) {
		t.Fatalf("%v", err)
	}
	list, err := s.Tokens(ctx, "ann")
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	revoked := 0
	for _, x := range list {
		if x.Revoked {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("revoked: %d", revoked)
	}
}
