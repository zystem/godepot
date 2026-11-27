package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// Passwords of local accounts (docs/spec.md, A1). A password is kept as PBKDF2-HMAC-SHA256 (the standard library has it; the program has no other dependency for this) with a salt of its own and 600 000
// rounds, as "pbkdf2-sha256$<rounds>$<salt>$<key>"; the rounds are in the string, so a later change of the number checks the old passwords as they were made. A user logs in with a password and gets a token; the
// password is also taken as the password of basic authentication, which is checked at every request, so a check that was made lately is remembered for a short time.
const (
	// PasswordRounds is the number of rounds of a new hash.
	PasswordRounds = 600_000
	// MinPassword and MaxPassword bound the length of a password in bytes.
	MinPassword = 12
	MaxPassword = 128
	// LockAfter is how many failed logins in a row lock an account, and the first lock lasts LockFor; each failure after it doubles that, up to MaxLock.
	LockAfter = 5
	LockFor   = time.Minute
	MaxLock   = 15 * time.Minute
	// verifiedFor is how long a password that was checked is not checked again.
	verifiedFor = 5 * time.Minute
)

// Rounds is the number of rounds that a new hash gets (a test lowers it).
var Rounds = PasswordRounds

var (
	// ErrLocked: too many failed logins; the account may be tried again after the time in LockedError.
	ErrLocked = errors.New("too many failed logins: the account is locked for a time")
	// ErrNoPassword: the account has no password (a robot, or a user who gets in with tokens).
	ErrNoPassword = errors.New("this account has no password")
	// ErrBadPassword is for a password that is too short or too long to be set.
	ErrBadPassword = fmt.Errorf("a password is %d to %d bytes", MinPassword, MaxPassword)
)

// LockedError says when an account can be tried again.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string { return ErrLocked.Error() }
func (e *LockedError) Unwrap() error { return ErrLocked }

// HashPassword makes the hash that is kept.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, Rounds, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", Rounds, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword says whether the password is the one the hash was made of.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 1 || rounds > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, rounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// CheckNewPassword says what is wrong with a password that is to be set.
func CheckNewPassword(pw string) error {
	if len(pw) < MinPassword || len(pw) > MaxPassword {
		return ErrBadPassword
	}
	return nil
}

// SetPassword sets the password of a user account. A robot has none: it gets in with tokens.
func (s *Store) SetPassword(ctx context.Context, name, pw string) error {
	if err := CheckNewPassword(pw); err != nil {
		return err
	}
	a, err := s.Account(ctx, name)
	if err != nil {
		return err
	}
	if a.Source != "" {
		return fmt.Errorf("%w: the password of %s is the directory's: change it there", ErrBadPassword, name)
	}
	if a.Kind != "user" {
		return errors.New("a robot has no password: it gets in with tokens")
	}
	h, err := HashPassword(pw)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx,
		meta.S(`UPDATE accounts SET password = ?, password_at = ? WHERE name = ?`, h, s.now().Unix(), name),
		meta.S(`DELETE FROM login_state WHERE account = ?`, name))
	return err
}

// ClearPassword removes the password of an account: it gets in with tokens only.
func (s *Store) ClearPassword(ctx context.Context, name string) error {
	_, err := s.DB.Exec(ctx, meta.S(`UPDATE accounts SET password = '', password_at = 0 WHERE name = ?`, name))
	return err
}

// The hashing is the heavy part of a login: only a few at a time, so that a flood of logins cannot take the whole machine (a login that finds them all busy is turned away).
var (
	gateOnce sync.Once
	gate     chan struct{}
)

// ErrBusy is returned when too many logins are being checked at once.
var ErrBusy = errors.New("too many logins are being checked at once: try again")

func acquire() (func(), error) {
	gateOnce.Do(func() { gate = make(chan struct{}, 4) })
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-time.After(2 * time.Second):
		return nil, ErrBusy
	}
}

// Memory keeps the checks of passwords that were good lately: a password that was checked is remembered for a short time by its SHA-256 (with the account), so that each request of a client that sends it
// with basic authentication does not cost the check again. The remembered value is what it was checked against (the hash that was matched, or the address of a directory): a changed password is another
// hash and is not matched by the memory.
type Memory struct {
	mu   sync.Mutex
	seen map[[32]byte]memo
}

type memo struct {
	hash  string
	until time.Time
}

var verified = &Memory{}

func memoKey(name, pw string) [32]byte { return sha256.Sum256([]byte(name + "\x00" + pw)) }

// OK says whether this name and password were checked lately against the same thing (a hash, or the address of a directory).
func (m *Memory) OK(name, pw, hash string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, found := m.seen[memoKey(name, pw)]
	return found && v.hash == hash && now.Before(v.until)
}

// Add remembers a check that was good.
func (m *Memory) Add(name, pw, hash string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[[32]byte]memo{}
	}
	if len(m.seen) > 10_000 {
		for k, v := range m.seen {
			if now.After(v.until) {
				delete(m.seen, k)
			}
		}
		if len(m.seen) > 10_000 {
			m.seen = map[[32]byte]memo{}
		}
	}
	m.seen[memoKey(name, pw)] = memo{hash, now.Add(verifiedFor)}
}

// Login checks the password of a user account and says who it is: the rights of its account for the actions of data, not the admin API (an admin makes a token for that). ErrInvalid for any login that is
// not good (the reason is in the text, for the audit log, and is not told to the client), a *LockedError for an account that is locked, ErrBusy when the machine is busy with other logins.
func (s *Store) Login(ctx context.Context, name, pw string) (*Principal, error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT a.admin, a.disabled, a.password, a.kind, COALESCE(l.fails, 0), COALESCE(l.locked_until, 0) FROM accounts a LEFT JOIN login_state l ON l.account = a.name WHERE a.name = ?`, name))
	if err != nil {
		return nil, err
	}
	now := s.now()
	if len(rows) == 0 || len(pw) > MaxPassword {
		// The same work as for a real account, so that the time does not tell which names exist.
		if release, err := acquire(); err == nil {
			_ = CheckPassword("pbkdf2-sha256$"+strconv.Itoa(Rounds)+"$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", pw[:min(len(pw), MaxPassword)])
			release()
		}
		return nil, fmt.Errorf("%w: no account %q or a password too long", ErrInvalid, name)
	}
	r := rows[0]
	hash := meta.Str(r[2])
	if until := meta.Int(r[5]); until > now.Unix() {
		return nil, &LockedError{Until: time.Unix(until, 0)}
	}
	if hash == "" || meta.Int(r[1]) != 0 {
		// An account with no password, or a disabled one, fails like a wrong password (and counts as one).
		if release, err := acquire(); err == nil {
			_ = CheckPassword("pbkdf2-sha256$"+strconv.Itoa(Rounds)+"$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", pw)
			release()
		}
		return nil, s.failed(ctx, name, int(meta.Int(r[4])), fmt.Errorf("%w: the account %s has no password or is disabled", ErrInvalid, name))
	}
	good := verified.OK(name, pw, hash, now)
	if !good {
		release, err := acquire()
		if err != nil {
			return nil, err
		}
		good = CheckPassword(hash, pw)
		release()
	}
	if !good {
		return nil, s.failed(ctx, name, int(meta.Int(r[4])), fmt.Errorf("%w: wrong password for %s", ErrInvalid, name))
	}
	verified.Add(name, pw, hash, now)
	if meta.Int(r[4]) > 0 {
		_, _ = s.DB.Exec(ctx, meta.S(`DELETE FROM login_state WHERE account = ?`, name))
	}
	return &Principal{Account: name, Admin: meta.Int(r[0]) != 0, Scope: dataScope()}, nil
}

// failed counts a failed login, and locks the account after LockAfter in a row: for LockFor, and twice as long for each failure after, up to MaxLock.
func (s *Store) failed(ctx context.Context, name string, before int, err error) error {
	fails := before + 1
	var until int64
	if fails >= LockAfter {
		d := LockFor << min(fails-LockAfter, 10)
		until = s.now().Add(min(d, MaxLock)).Unix()
	}
	if _, derr := s.DB.Exec(ctx, meta.S(`INSERT INTO login_state (account, fails, locked_until) VALUES (?, ?, ?) ON CONFLICT (account) DO UPDATE SET fails = excluded.fails, locked_until = excluded.locked_until`, name, fails, until)); derr != nil {
		return derr
	}
	return err
}
