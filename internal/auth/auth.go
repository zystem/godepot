// Package auth is who may do what: accounts, tokens and the scope of a token. An account has no password; it gets in with tokens, each of which has an owner, a scope and an end date
// (docs/spec.md, A1, A2). The secret part of a token is shown once, when it is made, and only its SHA-256 is kept.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

var (
	// ErrInvalid: no such token, a wrong secret, a revoked or an expired token, or a disabled account. The caller says nothing more to the client.
	ErrInvalid = errors.New("the token is not valid")
	// ErrExists: an account of this name is there.
	ErrExists = errors.New("an account of this name exists")
	// ErrNoAccount: there is no such account (or it is disabled).
	ErrNoAccount = errors.New("no such account")
	// ErrNoToken: there is no such token.
	ErrNoToken = errors.New("no such token")
)

// DefaultMaxLife is the longest life of a token when the operator sets none (docs/spec.md, A2).
const DefaultMaxLife = 90 * 24 * time.Hour

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// CheckName says what is wrong with the name of an account: plain words, no "$" and no spaces that a shell would eat (docs/plan.md, T31).
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("the name %q is not allowed: use 1 to 63 of a-z, 0-9, '.', '_' and '-', beginning with a letter or digit", name)
	}
	return nil
}

// Store keeps accounts and tokens in the database.
type Store struct {
	DB meta.DB
	// MaxLife is the longest life of a token (default DefaultMaxLife).
	MaxLife time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) maxLife() time.Duration {
	if s.MaxLife > 0 {
		return s.MaxLife
	}
	return DefaultMaxLife
}

// Account is a user or a robot.
type Account struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Admin    bool   `json:"admin"`
	Disabled bool   `json:"disabled"`
	Created  int64  `json:"created_at"`
}

// CreateAccount makes an account. kind is "user" or "robot".
func (s *Store) CreateAccount(ctx context.Context, name, kind string, admin bool) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if kind != "user" && kind != "robot" {
		return fmt.Errorf("the kind %q is not allowed: user or robot", kind)
	}
	a := 0
	if admin {
		a = 1
	}
	_, err := s.DB.Exec(ctx, meta.S(`INSERT INTO accounts (name, kind, admin, created_at) VALUES (?, ?, ?, ?)`, name, kind, a, s.now().Unix()))
	if _, refused := meta.RefusedBy(err); refused {
		return ErrExists
	}
	return err
}

// Account reads an account; ErrNoAccount when there is none.
func (s *Store) Account(ctx context.Context, name string) (*Account, error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT name, kind, admin, disabled, created_at FROM accounts WHERE name = ?`, name))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNoAccount
	}
	return accountOf(rows[0]), nil
}

func accountOf(r []any) *Account {
	return &Account{Name: meta.Str(r[0]), Kind: meta.Str(r[1]), Admin: meta.Int(r[2]) != 0, Disabled: meta.Int(r[3]) != 0, Created: meta.Int(r[4])}
}

// Accounts lists every account by name.
func (s *Store) Accounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT name, kind, admin, disabled, created_at FROM accounts ORDER BY name`))
	if err != nil {
		return nil, err
	}
	var out []*Account
	for _, r := range rows {
		out = append(out, accountOf(r))
	}
	return out, nil
}

// SetDisabled stops (or lets again) every token of an account at once.
func (s *Store) SetDisabled(ctx context.Context, name string, disabled bool) error {
	d := 0
	if disabled {
		d = 1
	}
	n, err := s.DB.Exec(ctx, meta.S(`UPDATE accounts SET disabled = ? WHERE name = ?`, d, name))
	if err != nil {
		return err
	}
	if n[0] == 0 {
		return ErrNoAccount
	}
	return nil
}

// Principal is who a request is from, after the token has been checked.
type Principal struct {
	Account string
	Admin   bool
	Token   string  // the id of the token; "" for an anonymous request
	Scope   []Grant // what the token allows; a token never allows more than its account has
}

// Anonymous says whether the request had no token.
func (p *Principal) Anonymous() bool { return p == nil || p.Account == "" }

// ScopeAllows says whether the scope of the token allows the action. An anonymous principal allows nothing here (the repository's own anonymous setting is checked by the caller).
func (p *Principal) ScopeAllows(repo, path, action string) bool {
	if p.Anonymous() {
		return false
	}
	for _, g := range p.Scope {
		if g.Allows(repo, path, action) {
			return true
		}
	}
	return false
}

// TokenInfo is what is known of a token after it was made. The secret is not among it.
type TokenInfo struct {
	ID      string  `json:"id"`
	Account string  `json:"account"`
	Scope   []Grant `json:"scope"`
	Note    string  `json:"note,omitempty"`
	Created int64   `json:"created_at"`
	Expires int64   `json:"expires_at"`
	Revoked bool    `json:"revoked"`
}

const tokenPrefix = "gd_"

func randomText(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness: nothing safe can be done
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func secretDigest(secret string) string {
	d := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(d[:])
}

// CreateToken makes a token for an account and returns it. This is the only time the secret is known: it is not kept. life is at most MaxLife and is required: a token always ends.
func (s *Store) CreateToken(ctx context.Context, account string, scope []Grant, life time.Duration, note string) (string, *TokenInfo, error) {
	if life <= 0 {
		return "", nil, errors.New("a token needs a life: it always has an end date")
	}
	if life > s.maxLife() {
		return "", nil, fmt.Errorf("the life %v is longer than the longest allowed, %v", life, s.maxLife())
	}
	if len(scope) == 0 {
		return "", nil, errors.New("a token needs a scope: at least one grant")
	}
	for i, g := range scope {
		if err := g.Check(); err != nil {
			return "", nil, fmt.Errorf("scope %d: %w", i+1, err)
		}
	}
	id := strings.ToLower(hex.EncodeToString(randBytes(6)))
	secret := randomText(32)
	sj, _ := json.Marshal(scope)
	now := s.now()
	info := &TokenInfo{ID: id, Account: account, Scope: scope, Note: note, Created: now.Unix(), Expires: now.Add(life).Unix()}
	_, err := s.DB.Exec(ctx,
		meta.Guard("no_account", `SELECT 1 FROM accounts WHERE name = ? AND disabled = 0`, account),
		meta.S(`INSERT INTO tokens (id, account, digest, scope, note, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, account, secretDigest(secret), string(sj), note, info.Created, info.Expires))
	if why, refused := meta.RefusedBy(err); refused && why == "no_account" {
		return "", nil, ErrNoAccount
	}
	if err != nil {
		return "", nil, err
	}
	return tokenPrefix + id + "_" + secret, info, nil
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Authenticate checks a token and says who it is. ErrInvalid for any token that is not good; the reason is the audit log's business, not the client's, and is in the error's text.
func (s *Store) Authenticate(ctx context.Context, token string) (*Principal, error) {
	id, secret, ok := splitToken(token)
	if !ok {
		return nil, fmt.Errorf("%w: not a godepot token", ErrInvalid)
	}
	rows, err := s.DB.Query(ctx, meta.S(`SELECT t.digest, t.scope, t.expires_at, t.revoked_at, a.name, a.admin, a.disabled FROM tokens t JOIN accounts a ON a.name = t.account WHERE t.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no token %s", ErrInvalid, id)
	}
	r := rows[0]
	want := meta.Str(r[0])
	if subtle.ConstantTimeCompare([]byte(want), []byte(secretDigest(secret))) != 1 {
		return nil, fmt.Errorf("%w: wrong secret for token %s", ErrInvalid, id)
	}
	switch {
	case !meta.Null(r[3]):
		return nil, fmt.Errorf("%w: token %s is revoked", ErrInvalid, id)
	case meta.Int(r[2]) <= s.now().Unix():
		return nil, fmt.Errorf("%w: token %s has expired", ErrInvalid, id)
	case meta.Int(r[6]) != 0:
		return nil, fmt.Errorf("%w: the account of token %s is disabled", ErrInvalid, id)
	}
	var scope []Grant
	if err := json.Unmarshal([]byte(meta.Str(r[1])), &scope); err != nil {
		return nil, fmt.Errorf("%w: token %s has a scope that cannot be read", ErrInvalid, id)
	}
	return &Principal{Account: meta.Str(r[4]), Admin: meta.Int(r[5]) != 0, Token: id, Scope: scope}, nil
}

func splitToken(t string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(t, tokenPrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || len(id) != 12 || len(secret) < 20 {
		return "", "", false
	}
	return id, secret, true
}

// Revoke ends a token now.
func (s *Store) Revoke(ctx context.Context, id string) error {
	n, err := s.DB.Exec(ctx, meta.S(`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.now().Unix(), id))
	if err != nil {
		return err
	}
	if n[0] == 0 {
		return ErrNoToken
	}
	return nil
}

// Tokens lists the tokens of an account ("" for every account), newest first.
func (s *Store) Tokens(ctx context.Context, account string) ([]*TokenInfo, error) {
	q := meta.S(`SELECT id, account, scope, note, created_at, expires_at, revoked_at FROM tokens ORDER BY created_at DESC, id`)
	if account != "" {
		q = meta.S(`SELECT id, account, scope, note, created_at, expires_at, revoked_at FROM tokens WHERE account = ? ORDER BY created_at DESC, id`, account)
	}
	rows, err := s.DB.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []*TokenInfo
	for _, r := range rows {
		t := &TokenInfo{ID: meta.Str(r[0]), Account: meta.Str(r[1]), Note: meta.Str(r[3]), Created: meta.Int(r[4]), Expires: meta.Int(r[5]), Revoked: !meta.Null(r[6])}
		_ = json.Unmarshal([]byte(meta.Str(r[2])), &t.Scope)
		out = append(out, t)
	}
	return out, nil
}
