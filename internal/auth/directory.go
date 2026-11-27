package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// Accounts of a directory (docs/spec.md, A1, A4). The password is checked by the directory, not here; what is kept is that the account exists, its groups as the directory last said them, and the count
// of failed logins. An account of godepot's own is never one of the directory's: the same name cannot be both, and the local one wins.

// SourceLDAP marks an account that a login at the directory made.
const SourceLDAP = "ldap"

// ErrLocalAccount is returned when the name belongs to an account of godepot's own.
var ErrLocalAccount = fmt.Errorf("%w: the name is that of a local account", ErrInvalid)

func (p *Principal) directory(source, groups string, at int64) {
	if source != SourceLDAP {
		return
	}
	p.Directory, p.GroupsAt = true, time.Unix(at, 0)
	_ = json.Unmarshal([]byte(groups), &p.Groups)
}

// DirectoryState is what is known of a name before the directory is asked about it.
type DirectoryState struct {
	Exists, Local, Disabled bool
	Fails                   int
	LockedUntil             time.Time
}

// DirectoryState reads it.
func (s *Store) DirectoryState(ctx context.Context, name string) (*DirectoryState, error) {
	rows, err := s.DB.Query(ctx,
		meta.S(`SELECT a.source, a.disabled, COALESCE(l.fails, 0), COALESCE(l.locked_until, 0) FROM (SELECT ? AS name) n LEFT JOIN accounts a ON a.name = n.name LEFT JOIN login_state l ON l.account = n.name`, name))
	if err != nil {
		return nil, err
	}
	r := rows[0]
	st := &DirectoryState{Exists: !meta.Null(r[0]), Fails: int(meta.Int(r[2]))}
	st.Local = st.Exists && meta.Str(r[0]) != SourceLDAP
	st.Disabled = st.Exists && meta.Int(r[1]) != 0
	if until := meta.Int(r[3]); until > s.now().Unix() {
		st.LockedUntil = time.Unix(until, 0)
	}
	return st, nil
}

// DirectoryFailed counts a password that the directory refused for a name it has, and locks the name as a local account is locked: so that guesses through godepot do not use up the tries of the
// directory's own lock.
func (s *Store) DirectoryFailed(ctx context.Context, name string, before int, why error) error {
	return s.failed(ctx, name, before, why)
}

// DirectoryLogin writes down a good login at the directory: the account is made the first time (a user, never an admin), its groups are kept, and the count of failures is forgotten. ErrLocalAccount when
// the name is a local one, ErrInvalid when the account has been disabled here.
func (s *Store) DirectoryLogin(ctx context.Context, name string, groups []string) (*Principal, error) {
	if err := CheckName(name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	now := s.now()
	gj := groupsJSON(groups)
	_, err := s.DB.Exec(ctx,
		meta.Unless("local_account", `SELECT 1 FROM accounts WHERE name = ? AND source <> ?`, name, SourceLDAP),
		meta.Unless("disabled", `SELECT 1 FROM accounts WHERE name = ? AND disabled <> 0`, name),
		meta.S(`INSERT INTO accounts (name, kind, admin, created_at, source, dir_groups, dir_at) VALUES (?, 'user', 0, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET dir_groups = excluded.dir_groups, dir_at = excluded.dir_at`, name, now.Unix(), SourceLDAP, gj, now.Unix()),
		meta.S(`DELETE FROM login_state WHERE account = ?`, name))
	if why, refused := meta.RefusedBy(err); refused {
		if why == "local_account" {
			return nil, ErrLocalAccount
		}
		return nil, fmt.Errorf("%w: the account %s is disabled", ErrInvalid, name)
	}
	if err != nil {
		return nil, err
	}
	return &Principal{Account: name, Directory: true, Groups: groups, GroupsAt: time.Unix(now.Unix(), 0), Scope: dataScope()}, nil
}

// DirectoryRefreshed keeps what the directory says now of the groups of an account.
func (s *Store) DirectoryRefreshed(ctx context.Context, name string, groups []string) error {
	_, err := s.DB.Exec(ctx, meta.S(`UPDATE accounts SET dir_groups = ?, dir_at = ? WHERE name = ? AND source = ?`, groupsJSON(groups), s.now().Unix(), name, SourceLDAP))
	return err
}

// Directory reads the principal of an account of the directory with what is kept of it (for a password that was checked lately); ErrInvalid when there is none or it is disabled.
func (s *Store) Directory(ctx context.Context, name string) (*Principal, error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT source, dir_groups, dir_at FROM accounts WHERE name = ? AND source = ? AND disabled = 0`, name, SourceLDAP))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no enabled account of the directory %q", ErrInvalid, name)
	}
	p := &Principal{Account: name, Scope: dataScope()}
	p.directory(meta.Str(rows[0][0]), meta.Str(rows[0][1]), meta.Int(rows[0][2]))
	return p, nil
}

func groupsJSON(groups []string) string {
	if groups == nil {
		groups = []string{}
	}
	b, _ := json.Marshal(groups)
	return string(b)
}

// dataScope is what a password gives: the actions of data everywhere, never the admin API.
func dataScope() []Grant {
	return []Grant{{Repositories: []string{"*"}, Actions: []string{Read, Write, Delete, Promote}}}
}

// DirectoryCA is the PEM that an admin gave to be trusted for the directory, who gave it and when; nil when there is none.
func (s *Store) DirectoryCA(ctx context.Context) (pem []byte, by string, at time.Time, err error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT pem, set_by, set_at FROM ldap_ca WHERE id = 1`))
	if err != nil || len(rows) == 0 {
		return nil, "", time.Time{}, err
	}
	return []byte(meta.Str(rows[0][0])), meta.Str(rows[0][1]), time.Unix(meta.Int(rows[0][2]), 0), nil
}

// SetDirectoryCA keeps the PEM in the place of the one there was; a nil one forgets it. The caller has checked what is in it.
func (s *Store) SetDirectoryCA(ctx context.Context, pem []byte, by string) error {
	st := meta.S(`DELETE FROM ldap_ca`)
	if pem != nil {
		st = meta.S(`INSERT INTO ldap_ca (id, pem, set_by, set_at) VALUES (1, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET pem = excluded.pem, set_by = excluded.set_by, set_at = excluded.set_at`,
			string(pem), by, s.now().Unix())
	}
	_, err := s.DB.Exec(ctx, st)
	return err
}

// Clock is the time of the store (for those that compare it with what the store wrote).
func (s *Store) Clock() time.Time { return s.now() }
