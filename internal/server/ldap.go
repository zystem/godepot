package server

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/ldapauth"
	"github.com/zystem/godepot/internal/settings"
)

// directoryStale is how long what the directory said of a person is still used when it cannot be asked.
const directoryStale = time.Hour

// directory is what the server remembers about the directory.
type directory struct {
	checked  auth.Memory
	mu       sync.Mutex
	asking   map[string]*sync.Mutex // one question about one name at a time
	attempts chan struct{}
	once     sync.Once
}

func (d *directory) name(n string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.asking == nil {
		d.asking = map[string]*sync.Mutex{}
	}
	if len(d.asking) > 10_000 {
		d.asking = map[string]*sync.Mutex{}
	}
	m := d.asking[n]
	if m == nil {
		m = &sync.Mutex{}
		d.asking[n] = m
	}
	return m
}

// slot keeps the logins that talk to the directory at once to a few.
func (d *directory) slot() (func(), error) {
	d.once.Do(func() { d.attempts = make(chan struct{}, 8) })
	select {
	case d.attempts <- struct{}{}:
		return func() { <-d.attempts }, nil
	case <-time.After(2 * time.Second):
		return nil, auth.ErrBusy
	}
}

func (s *Server) ldap() *settings.LDAP {
	if set := s.Settings.Load(); set != nil {
		return set.LDAP
	}
	return nil
}

// ldapConfig is the directory of the settings, with the certificates an admin gave: read at each use, so that every replica takes a change at once.
func (s *Server) ldapConfig(ctx context.Context, l *settings.LDAP) (ldapauth.Config, error) {
	ca, _, _, err := s.Auth.DirectoryCA(ctx)
	if err != nil {
		return ldapauth.Config{}, err
	}
	c := ldapauth.Config{
		CA: ca, URL: l.URL, StartTLS: l.StartTLS, CAFile: l.CAFile, ServerName: l.ServerName, BindDN: l.BindDN, BindPassword: os.Getenv(l.PasswordEnv),
		UserBase: l.UserBase, UserFilter: l.UserFilter, GroupBase: l.GroupBase, GroupFilter: l.GroupFilter, GroupName: l.GroupName,
	}
	if c.GroupFilter == "" {
		c.GroupFilter = settings.DefaultGroupFilter
	}
	if c.GroupName == "" {
		c.GroupName = settings.DefaultGroupName
	}
	return c, nil
}

// check is the password of an account: a local one is checked here, and any other name is asked of the directory, when there is one.
func (s *Server) check(ctx context.Context, user, pass string) (*auth.Principal, error) {
	l := s.ldap()
	if l == nil {
		return s.Auth.Login(ctx, user, pass)
	}
	// A directory takes no notice of capitals, so the account of "Alice" and "alice" is one: the small letters.
	name := strings.ToLower(user)
	if auth.CheckName(name) != nil || len(pass) > auth.MaxPassword || pass == "" {
		return s.Auth.Login(ctx, user, pass)
	}
	st, err := s.Auth.DirectoryState(ctx, name)
	if err != nil {
		return nil, err
	}
	if st.Local {
		// The local account wins, and is logged in to only by its own name.
		return s.Auth.Login(ctx, user, pass)
	}
	if !st.LockedUntil.IsZero() {
		return nil, &auth.LockedError{Until: st.LockedUntil}
	}
	if st.Disabled {
		return nil, fmt.Errorf("%w: the account %s is disabled", auth.ErrInvalid, name)
	}
	if s.dir.checked.OK(name, pass, l.URL, s.Auth.Clock()) {
		p, err := s.Auth.Directory(ctx, name)
		if err != nil {
			return nil, err
		}
		return p, s.current(ctx, p)
	}
	release, err := s.dir.slot()
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := s.ldapConfig(ctx, l)
	if err != nil {
		return nil, err
	}
	person, err := cfg.Authenticate(ctx, name, pass)
	switch {
	case errors.Is(err, ldapauth.ErrInvalid):
		why := fmt.Errorf("%w: %v", auth.ErrInvalid, err)
		// Only a name the directory has is counted: invented names must not fill the table.
		if errors.Is(err, ldapauth.ErrPassword) {
			return nil, s.Auth.DirectoryFailed(ctx, name, st.Fails, why)
		}
		return nil, why
	case err != nil:
		return nil, err
	}
	p, err := s.Auth.DirectoryLogin(ctx, name, person.Groups)
	if err != nil {
		return nil, err
	}
	s.dir.checked.Add(name, pass, l.URL, s.Auth.Clock())
	return p, nil
}

// current makes sure that what is kept of an account of the directory is new enough, asking the directory again when it is not: a person who was removed or disabled there, or moved between groups, is
// so here too within the time of "refresh", also for tokens made before. ErrInvalid: the person is gone; ErrUnavailable: the directory cannot be asked and what is kept is too old.
func (s *Server) current(ctx context.Context, p *auth.Principal) error {
	if !p.Directory {
		return nil
	}
	l := s.ldap()
	if l == nil {
		return fmt.Errorf("%w: the account %s is one of a directory, and the settings name none", auth.ErrInvalid, p.Account)
	}
	if s.Auth.Clock().Sub(p.GroupsAt) < l.RefreshEvery() {
		return nil
	}
	m := s.dir.name(p.Account)
	m.Lock()
	defer m.Unlock()
	kept, err := s.Auth.Directory(ctx, p.Account) // another request may have asked while we waited
	if err != nil {
		return err
	}
	p.Groups, p.GroupsAt = kept.Groups, kept.GroupsAt
	if s.Auth.Clock().Sub(p.GroupsAt) < l.RefreshEvery() {
		return nil
	}
	cfg, err := s.ldapConfig(ctx, l)
	if err != nil {
		return err
	}
	person, err := cfg.Lookup(context.WithoutCancel(ctx), p.Account)
	switch {
	case errors.Is(err, ldapauth.ErrInvalid):
		return fmt.Errorf("%w: the directory no longer has %s", auth.ErrInvalid, p.Account)
	case err != nil:
		if s.Auth.Clock().Sub(p.GroupsAt) < directoryStale {
			s.Audit.Record(ctx, audit.Event{Actor: p.Account, Action: "directory", Result: audit.Refused, Detail: clip("what is kept is used: "+err.Error(), 500)})
			return nil
		}
		return err
	}
	if err := s.Auth.DirectoryRefreshed(ctx, p.Account, person.Groups); err != nil {
		return err
	}
	p.Groups, p.GroupsAt = person.Groups, s.Auth.Clock()
	return nil
}

// directoryCA is /api/v1/ldap/ca: the certificates trusted for the directory besides ca_file. The body of a PUT is the PEM itself (curl --data-binary @ca.crt).
func (s *Server) directoryCA(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	action := "ldap.ca.read"
	if r.Method != http.MethodGet {
		action = "ldap.ca.set"
	}
	switch r.Method {
	case http.MethodGet, http.MethodPut, http.MethodDelete:
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "this call takes GET, PUT and DELETE")
		return
	}
	if !s.admin(w, r, p, action) {
		return
	}
	down := func() { fail(w, http.StatusServiceUnavailable, "the database is not available: try again") }
	switch r.Method {
	case http.MethodGet:
		stored, by, at, err := s.Auth.DirectoryCA(r.Context())
		if err != nil {
			down()
			return
		}
		out := map[string]any{"certificates": []ldapauth.Certificate{}}
		if stored != nil {
			certs, err := ldapauth.ParseCA(stored)
			if err != nil {
				fail(w, http.StatusInternalServerError, "what is stored cannot be read: set it again")
				return
			}
			out = map[string]any{"certificates": ldapauth.Describe(certs), "pem": string(stored), "set_by": by, "set_at": at.Unix()}
		}
		reply(w, http.StatusOK, out)
	case http.MethodDelete:
		if err := s.Auth.SetDirectoryCA(r.Context(), nil, p.Account); err != nil {
			down()
			return
		}
		s.Audit.Record(r.Context(), s.event(r, p, action, audit.OK, "removed"))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, ldapauth.MaxCA+1))
		if err != nil {
			fail(w, http.StatusBadRequest, "the body cannot be read")
			return
		}
		certs, err := ldapauth.ParseCA(body)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, c := range certs {
			if s.Auth.Clock().After(c.NotAfter) {
				fail(w, http.StatusBadRequest, fmt.Sprintf("the certificate %q ended on %s", c.Subject.String(), c.NotAfter.UTC().Format(time.DateOnly)))
				return
			}
		}
		// only the certificates are kept, without any text around them
		var kept []byte
		var sums []string
		for _, c := range certs {
			kept = append(kept, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
		}
		told := ldapauth.Describe(certs)
		for _, c := range told {
			sums = append(sums, c.SHA256)
		}
		if err := s.Auth.SetDirectoryCA(r.Context(), kept, p.Account); err != nil {
			down()
			return
		}
		s.Audit.Record(r.Context(), s.event(r, p, action, audit.OK, clip("sha256 "+strings.Join(sums, " "), 500)))
		reply(w, http.StatusOK, map[string]any{"certificates": told})
	}
}

// directoryDown answers a request that needed the directory and could not have it.
func directoryDown(w http.ResponseWriter, refuse refuser) {
	w.Header().Set("Retry-After", "5")
	refuse(w, http.StatusServiceUnavailable, "the directory cannot be asked: try again")
}
