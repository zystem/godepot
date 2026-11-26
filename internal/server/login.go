package server

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
)

// errThrottled: too many failed logins came from one address lately.
var errThrottled = errors.New("too many failed logins from this address")

// ipFails counts the failed logins of an address in the last minute: a flood of wrong passwords from one place is turned away before the rounds of the hash are spent (the lock of an account is in the
// database, and works against a guess for one account from many places; this works against a guess for many accounts from one).
type ipFails struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

const (
	ipFailsMax    = 30
	ipFailsWindow = time.Minute
)

func (f *ipFails) blocked(ip string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recent(ip, now)) >= ipFailsMax
}

func (f *ipFails) recent(ip string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range f.seen[ip] {
		if now.Sub(t) < ipFailsWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(f.seen, ip)
	} else if f.seen != nil {
		f.seen[ip] = keep
	}
	return keep
}

func (f *ipFails) add(ip string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string][]time.Time{}
	}
	if len(f.seen) > 10000 {
		f.seen = map[string][]time.Time{}
	}
	f.seen[ip] = append(f.recent(ip, now), now)
}

// password checks the password of an account for a request that sent it. A failure is audited here.
func (s *Server) password(r *http.Request, user, pass string) (*auth.Principal, error) {
	ip := remote(r)
	if s.fails.blocked(ip, time.Now()) {
		return nil, errThrottled
	}
	p, err := s.Auth.Login(r.Context(), user, pass)
	if err != nil {
		if errors.Is(err, auth.ErrInvalid) {
			s.fails.add(ip, time.Now())
			s.Audit.Record(r.Context(), audit.Event{Actor: user, Remote: ip, Action: "login", Result: audit.Refused, Detail: err.Error()})
		}
		return nil, err
	}
	return p, nil
}

// login is POST /api/v1/login: the password of an account for a token (docs/spec.md, A1, A5). It needs no token. The body names the account and the password, and may narrow the token: life, repositories,
// paths and actions (the default is the actions of data on every repository, for 12 hours). A token never has more than its account, so it is safe to let it be wider than the account has.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	var body struct {
		Account      string   `json:"account"`
		Password     string   `json:"password"`
		LifeSeconds  int64    `json:"life_seconds"`
		Repositories []string `json:"repositories"`
		Paths        []string `json:"paths"`
		Actions      []string `json:"actions"`
		Note         string   `json:"note"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p, err := s.password(r, body.Account, body.Password)
	if err != nil {
		s.loginRefused(w, err)
		return
	}
	life := time.Duration(body.LifeSeconds) * time.Second
	if life == 0 {
		life = s.LoginLife
		if life == 0 {
			life = 12 * time.Hour
		}
	}
	scope := []auth.Grant{{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete, auth.Promote}}}
	if len(body.Repositories) > 0 || len(body.Actions) > 0 || len(body.Paths) > 0 {
		scope = []auth.Grant{{Repositories: body.Repositories, Paths: body.Paths, Actions: body.Actions}}
	}
	note := strings.TrimSpace(body.Note)
	if note == "" {
		note = "login"
	}
	tok, info, err := s.Auth.CreateToken(r.Context(), p.Account, scope, life, note)
	switch {
	case err != nil && !isUnavailable(err) && !errors.Is(err, auth.ErrNoAccount):
		fail(w, http.StatusBadRequest, err.Error())
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
	default:
		s.Audit.Record(r.Context(), audit.Event{Actor: p.Account, Remote: remote(r), Action: "login", Result: audit.OK, Detail: "token " + info.ID})
		w.Header().Set("Cache-Control", "no-store")
		reply(w, http.StatusCreated, map[string]any{"token": tok, "account": p.Account, "expires_at": info.Expires, "note": "this is the only time the token is shown"})
	}
}

// loginRefused answers a login that was not good, in the same words for every reason (what is wrong is in the audit log).
func (s *Server) loginRefused(w http.ResponseWriter, err error) {
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", itoa(int64(max(1, time.Until(locked.Until).Seconds()))))
		fail(w, http.StatusTooManyRequests, "too many failed logins: try again later")
	case errors.Is(err, errThrottled), errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusTooManyRequests, "too many logins: try again later")
	case errors.Is(err, auth.ErrInvalid):
		w.Header().Set("WWW-Authenticate", `Basic realm="godepot", charset="UTF-8"`)
		fail(w, http.StatusUnauthorized, "the account or the password is not right")
	default:
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// setPassword is PUT and DELETE of /api/v1/accounts/<name>/password for an admin (set, with {"password": ...}; clear), and POST /api/v1/password for a user who changes their own: {"account", "password",
// "new_password"} (it needs the old password and no token).
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request, p *auth.Principal, name string) {
	if !s.admin(w, r, p, "password.set") {
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Password string `json:"password"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		switch err := s.Auth.SetPassword(r.Context(), name, body.Password); {
		case errors.Is(err, auth.ErrNoAccount):
			fail(w, http.StatusNotFound, "no such account (or it is disabled)")
		case errors.Is(err, auth.ErrBadPassword):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil && !isUnavailable(err):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		default:
			s.Audit.Record(r.Context(), s.event(r, p, "password.set", audit.OK, name))
			w.WriteHeader(http.StatusNoContent)
		}
	case http.MethodDelete:
		if err := s.Auth.ClearPassword(r.Context(), name); err != nil {
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
			return
		}
		s.Audit.Record(r.Context(), s.event(r, p, "password.clear", audit.OK, name))
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusMethodNotAllowed, "PUT or DELETE")
	}
}

// changePassword is POST /api/v1/password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	var body struct {
		Account     string `json:"account"`
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := auth.CheckNewPassword(body.NewPassword); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.password(r, body.Account, body.Password); err != nil {
		s.loginRefused(w, err)
		return
	}
	if err := s.Auth.SetPassword(r.Context(), body.Account, body.NewPassword); err != nil {
		fail(w, http.StatusServiceUnavailable, "the password could not be changed: "+err.Error())
		return
	}
	s.Audit.Record(r.Context(), audit.Event{Actor: body.Account, Remote: remote(r), Action: "password.change", Result: audit.OK, Detail: body.Account})
	w.WriteHeader(http.StatusNoContent)
}

// loginToken is what a format's login (npm's) does: check the password, make a token of the actions of data for LoginLife.
func (s *Server) loginToken(r *http.Request, account, password, note string) (string, error) {
	p, err := s.password(r, account, password)
	if err != nil {
		return "", err
	}
	life := s.LoginLife
	if life == 0 {
		life = 12 * time.Hour
	}
	tok, info, err := s.Auth.CreateToken(r.Context(), p.Account, []auth.Grant{{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete, auth.Promote}}}, life, note)
	if err != nil {
		return "", err
	}
	s.Audit.Record(r.Context(), audit.Event{Actor: p.Account, Remote: remote(r), Action: "login", Result: audit.OK, Detail: "token " + info.ID + " (" + note + ")"})
	return tok, nil
}
