package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/auth"
)

func init() { auth.Rounds = 1000 } // the tests do not need the cost of the real number

func TestPasswordHash(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := auth.HashPassword("correct horse battery")
	if h == h2 || !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Fatalf("a hash has a salt of its own and names its rounds: %s %s", h, h2)
	}
	if !auth.CheckPassword(h, "correct horse battery") || auth.CheckPassword(h, "correct horse batterY") || auth.CheckPassword(h, "") {
		t.Fatal("CheckPassword")
	}
	for _, bad := range []string{"", "x", "pbkdf2-sha256$1000$AA$", "md5$1$AA$AA", "pbkdf2-sha256$0$AA$AA", "pbkdf2-sha256$x$AA$AA", "pbkdf2-sha256$1000$!!$AA"} {
		if auth.CheckPassword(bad, "anything") {
			t.Errorf("%q checked as a hash", bad)
		}
	}
	// A hash made with other rounds is checked with its own.
	old := auth.Rounds
	auth.Rounds = 2000
	h3, _ := auth.HashPassword("another long password")
	auth.Rounds = old
	if !auth.CheckPassword(h3, "another long password") || !strings.Contains(h3, "$2000$") {
		t.Fatal("rounds are read from the hash")
	}
	if auth.CheckNewPassword("short") == nil || auth.CheckNewPassword(strings.Repeat("x", 129)) == nil || auth.CheckNewPassword("long enough password") != nil {
		t.Fatal("the length of a password")
	}
}

func TestLoginAndLock(t *testing.T) {
	ctx := context.Background()
	s, clock := store(t)
	for _, a := range []struct {
		n, k string
		adm  bool
	}{{"alice", "user", true}, {"bob", "user", false}, {"ci-build", "robot", false}} {
		if err := s.CreateAccount(ctx, a.n, a.k, a.adm); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPassword(ctx, "alice", "alice has a password"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "ci-build", "a robot has none ok"); err == nil {
		t.Fatal("a robot got a password")
	}
	if err := s.SetPassword(ctx, "alice", "short"); !errors.Is(err, auth.ErrBadPassword) {
		t.Fatalf("short: %v", err)
	}
	if err := s.SetPassword(ctx, "nobody", "a long enough password"); !errors.Is(err, auth.ErrNoAccount) {
		t.Fatalf("no account: %v", err)
	}
	p, err := s.Login(ctx, "alice", "alice has a password")
	if err != nil || p.Account != "alice" || !p.Admin || !p.ScopeAllows("r", "x", auth.Delete) || p.ScopeAllows("r", "x", auth.Admin) {
		t.Fatalf("login: %+v %v", p, err)
	}
	// Every kind of failure is the same error for the caller.
	for _, c := range []struct{ n, pw string }{{"alice", "wrong password here"}, {"bob", "bob has no password!"}, {"nobody", "no such account at all"}, {"ci-build", "robots have no password"}, {"alice", strings.Repeat("x", 500)}} {
		if _, err := s.Login(ctx, c.n, c.pw); !errors.Is(err, auth.ErrInvalid) {
			t.Fatalf("%s: %v", c.n, err)
		}
	}
	// Five failures in a row lock the account; even the right password is then refused until the time is over, and the lock grows.
	for i := 0; i < auth.LockAfter-1; i++ { // one failure is counted above for alice; four more make five
		_, _ = s.Login(ctx, "alice", "wrong password here")
	}
	var locked *auth.LockedError
	_, err = s.Login(ctx, "alice", "alice has a password")
	if !errors.As(err, &locked) || !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("locked: %v", err)
	}
	if d := locked.Until.Sub(time.Unix(clock.Load(), 0)); d <= 0 || d > auth.LockFor {
		t.Fatalf("the first lock lasts %v", d)
	}
	clock.Add(int64(auth.LockFor/time.Second) + 1)
	if _, err := s.Login(ctx, "alice", "wrong password here"); !errors.Is(err, auth.ErrInvalid) { // one more failure: the next lock is twice as long
		t.Fatalf("after the lock: %v", err)
	}
	_, err = s.Login(ctx, "alice", "alice has a password")
	if !errors.As(err, &locked) || locked.Until.Sub(time.Unix(clock.Load(), 0)) <= auth.LockFor {
		t.Fatalf("the second lock should be longer: %v", err)
	}
	clock.Add(int64(auth.MaxLock/time.Second) + 1)
	if _, err := s.Login(ctx, "alice", "alice has a password"); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	// A new password lifts a lock at once (an admin who sets it has talked to the owner).
	for i := 0; i < auth.LockAfter; i++ {
		_, _ = s.Login(ctx, "alice", "wrong password here")
	}
	if err := s.SetPassword(ctx, "alice", "alice has a password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "alice", "alice has a password"); err != nil {
		t.Fatalf("a new password should lift the lock: %v", err)
	}
	// A success resets the count; a new password resets it too, and the old one stops at once (the memory of checked passwords does not outlive it).
	if err := s.SetPassword(ctx, "alice", "alice has another one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "alice", "alice has a password"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("the old password: %v", err)
	}
	if _, err := s.Login(ctx, "alice", "alice has another one"); err != nil {
		t.Fatal(err)
	}
	// A disabled account does not log in, whatever it was before.
	if err := s.SetDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "alice", "alice has another one"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("a disabled account: %v", err)
	}
	if err := s.SetDisabled(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPassword(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "alice", "alice has another one"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("a cleared password: %v", err)
	}
}
