package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/upstream"
)

// The API of accounts, tokens and the audit log. Who may do what:
//   - whoami: any token.
//   - tokens: a token whose scope has "admin" on every repository ("*") makes tokens for its own account and lists and revokes its own; an admin account does so for any account.
//   - accounts, the audit log and the settings: an admin account whose token has that scope.
//
// A token never has more than its account has (the account's grants and the token's scope must both allow), so making a token with a wide scope gives nothing that the account lacks.
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	p := s.principal(w, r, fail)
	if p == nil {
		return
	}
	if p.Anonymous() {
		s.count("unauthenticated")
		unauthorized(w, fail)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, apiPrefix), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "whoami" && r.Method == http.MethodGet:
		reply(w, http.StatusOK, map[string]any{"account": p.Account, "admin": p.Admin, "token": p.Token, "scope": p.Scope})
	case len(parts) == 1 && parts[0] == "accounts":
		s.accounts(w, r, p)
	case len(parts) == 3 && parts[0] == "accounts" && r.Method == http.MethodPost && (parts[2] == "disable" || parts[2] == "enable"):
		s.disable(w, r, p, parts[1], parts[2] == "disable")
	case len(parts) == 1 && parts[0] == "tokens":
		s.tokens(w, r, p)
	case len(parts) == 2 && parts[0] == "tokens" && r.Method == http.MethodDelete:
		s.revoke(w, r, p, parts[1])
	case len(parts) == 1 && parts[0] == "audit" && r.Method == http.MethodGet:
		s.auditList(w, r, p)
	case len(parts) == 1 && parts[0] == "repositories" && r.Method == http.MethodGet:
		if !s.admin(w, r, p, "settings.read") {
			return
		}
		reply(w, http.StatusOK, s.Settings.Load())
	case len(parts) == 3 && parts[0] == "repositories" && parts[2] == "forget" && r.Method == http.MethodPost:
		s.forget(w, r, p, parts[1])
	default:
		fail(w, http.StatusNotFound, "no such call")
	}
}

// forget clears what a proxy repository remembers as missing, and ends the pause after errors of its upstream (docs/spec.md, P4): all of it, or the one name given as ?name=.
func (s *Server) forget(w http.ResponseWriter, r *http.Request, p *auth.Principal, repo string) {
	if !s.admin(w, r, p, "forget") {
		return
	}
	set := s.Settings.Load()
	if set == nil || set.Repo(repo) == nil || set.Repo(repo).Kind != "proxy" {
		fail(w, http.StatusNotFound, "no such proxy repository")
		return
	}
	n, err := upstream.Forget(r.Context(), s.Auth.DB, repo, r.URL.Query().Get("name"))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	s.Audit.Record(r.Context(), s.event(r, p, "forget", audit.OK, repo))
	reply(w, http.StatusOK, map[string]any{"repository": repo, "forgotten": n})
}

// admin checks that the principal is an admin account with a token that has admin on everything; otherwise it answers 403 and audits.
func (s *Server) admin(w http.ResponseWriter, r *http.Request, p *auth.Principal, action string) bool {
	if p.Admin && p.ScopeAllows("*", "", auth.Admin) {
		return true
	}
	s.count("forbidden")
	s.Audit.Record(r.Context(), audit.Event{Actor: p.Account, Token: p.Token, Remote: remote(r), Action: action, Result: audit.Refused, Detail: "forbidden"})
	fail(w, http.StatusForbidden, "this needs an admin account and a token with the action admin on every repository")
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "the body is not the JSON this call takes: "+err.Error())
		return false
	}
	return true
}

func (s *Server) event(r *http.Request, p *auth.Principal, action, result, detail string) audit.Event {
	return audit.Event{Actor: p.Account, Token: p.Token, Remote: remote(r), Action: action, Result: result, Detail: detail}
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	switch r.Method {
	case http.MethodGet:
		if !s.admin(w, r, p, "account.list") {
			return
		}
		list, err := s.Auth.Accounts(r.Context())
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
			return
		}
		reply(w, http.StatusOK, map[string]any{"items": list})
	case http.MethodPost:
		if !s.admin(w, r, p, "account.create") {
			return
		}
		var body struct {
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Admin bool   `json:"admin"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Kind == "" {
			body.Kind = "user"
		}
		err := s.Auth.CreateAccount(r.Context(), body.Name, body.Kind, body.Admin)
		switch {
		case errors.Is(err, auth.ErrExists):
			fail(w, http.StatusConflict, err.Error())
		case err != nil && strings.Contains(err.Error(), "not allowed"):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		default:
			s.Audit.Record(r.Context(), s.event(r, p, "account.create", audit.OK, body.Name+" "+body.Kind))
			reply(w, http.StatusCreated, map[string]any{"name": body.Name, "kind": body.Kind, "admin": body.Admin})
		}
	default:
		fail(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) disable(w http.ResponseWriter, r *http.Request, p *auth.Principal, name string, off bool) {
	action := "account.enable"
	if off {
		action = "account.disable"
	}
	if !s.admin(w, r, p, action) {
		return
	}
	if err := s.Auth.SetDisabled(r.Context(), name, off); err != nil {
		if errors.Is(err, auth.ErrNoAccount) {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	s.Audit.Record(r.Context(), s.event(r, p, action, audit.OK, name))
	w.WriteHeader(http.StatusNoContent)
}

// mayMakeTokens says whether the token may make, list and revoke tokens of its own account.
func mayMakeTokens(p *auth.Principal) bool { return p.ScopeAllows("*", "", auth.Admin) }

func (s *Server) tokens(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !mayMakeTokens(p) {
		s.count("forbidden")
		s.Audit.Record(r.Context(), s.event(r, p, "token.call", audit.Refused, "the token has no admin scope"))
		fail(w, http.StatusForbidden, "making and listing tokens needs a token with the action admin on every repository")
		return
	}
	switch r.Method {
	case http.MethodGet:
		account := p.Account
		if q := r.URL.Query().Get("account"); q != "" && q != p.Account {
			if !s.admin(w, r, p, "token.list") {
				return
			}
			account = q
		} else if r.URL.Query().Get("all") == "1" {
			if !s.admin(w, r, p, "token.list") {
				return
			}
			account = ""
		}
		list, err := s.Auth.Tokens(r.Context(), account)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
			return
		}
		reply(w, http.StatusOK, map[string]any{"items": list})
	case http.MethodPost:
		var body struct {
			Account     string       `json:"account"`
			Scope       []auth.Grant `json:"scope"`
			LifeSeconds int64        `json:"life_seconds"`
			Note        string       `json:"note"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Account == "" {
			body.Account = p.Account
		}
		if body.Account != p.Account && !s.admin(w, r, p, "token.create") {
			return
		}
		tok, info, err := s.Auth.CreateToken(r.Context(), body.Account, body.Scope, time.Duration(body.LifeSeconds)*time.Second, body.Note)
		switch {
		case errors.Is(err, auth.ErrNoAccount):
			fail(w, http.StatusNotFound, "no such account (or it is disabled)")
		case err != nil && !isUnavailable(err):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		default:
			s.Audit.Record(r.Context(), s.event(r, p, "token.create", audit.OK, info.ID+" for "+info.Account+" until "+time.Unix(info.Expires, 0).UTC().Format(time.RFC3339)))
			w.Header().Set("Cache-Control", "no-store")
			reply(w, http.StatusCreated, map[string]any{"token": tok, "info": info, "note": "this is the only time the token is shown"})
		}
	default:
		fail(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func isUnavailable(err error) bool {
	var u *meta.Unavailable
	return errors.As(err, &u)
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string) {
	if !mayMakeTokens(p) {
		s.count("forbidden")
		s.Audit.Record(r.Context(), s.event(r, p, "token.revoke", audit.Refused, "the token has no admin scope"))
		fail(w, http.StatusForbidden, "revoking tokens needs a token with the action admin on every repository")
		return
	}
	all, err := s.Auth.Tokens(r.Context(), "")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	var owner string
	for _, t := range all {
		if t.ID == id {
			owner = t.Account
		}
	}
	if owner == "" {
		fail(w, http.StatusNotFound, "no such token")
		return
	}
	if owner != p.Account && !s.admin(w, r, p, "token.revoke") {
		return
	}
	if err := s.Auth.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, auth.ErrNoToken) {
			fail(w, http.StatusNotFound, "no such token, or it is revoked already")
			return
		}
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	s.Audit.Record(r.Context(), s.event(r, p, "token.revoke", audit.OK, id+" of "+owner))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !s.admin(w, r, p, "audit.read") {
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := s.Audit.List(r.Context(), before, limit, q.Get("action"), q.Get("actor"))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	next := int64(0)
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	reply(w, http.StatusOK, map[string]any{"items": rows, "next": next})
}
