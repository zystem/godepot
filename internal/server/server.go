// Package server is the HTTP door: it finds the repository an address names, checks the token, and hands the request to the format of the repository; and it serves the API of accounts, tokens and the
// audit log. Nothing here writes a secret to a log: a token is read from the header Authorization only, never from an address, and only its id is ever named.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/settings"
)

// Server is the door.
type Server struct {
	Settings atomic.Pointer[settings.Settings] // read at start and replaced on SIGHUP
	Auth     *auth.Store
	Audit    *audit.Log
	Formats  map[string]format.Format
	// Refused counts the refusals by reason (unauthenticated, forbidden); nil: not counted.
	Refused *metrics.Counter
}

const (
	repoPrefix = "/repository/"
	apiPrefix  = "/api/v1/"
	v2Prefix   = "/v2/"
)

// Handles says whether the address is one of this door's (the rest is for the caller's own mux).
func Handles(path string) bool {
	return strings.HasPrefix(path, repoPrefix) || strings.HasPrefix(path, apiPrefix) || path == "/v2" || strings.HasPrefix(path, v2Prefix)
}

// ServeHTTP answers a request whose address Handles.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, repoPrefix):
		s.repository(w, r)
	case strings.HasPrefix(r.URL.Path, apiPrefix):
		s.api(w, r)
	case r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, v2Prefix):
		s.v2(w, r)
	default:
		http.NotFound(w, r)
	}
}

type problem struct {
	Error string `json:"error"`
	Info  any    `json:"info,omitempty"`
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { reply(w, code, problem{Error: msg}) }

// refuser writes an answer that refuses a request, in the words of the door's own protocol (JSON with "error", or the "errors" list of the OCI distribution API).
type refuser func(w http.ResponseWriter, code int, msg string)

func remote(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// token reads the token of a request: "Authorization: Bearer <token>", or Basic with the token as the password (any user name; the account is the token's own). ok is false when the header is there
// but is not one of the two.
func token(r *http.Request) (tok string, present, ok bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false, true
	}
	scheme, rest, _ := strings.Cut(h, " ")
	switch strings.ToLower(scheme) {
	case "bearer":
		return strings.TrimSpace(rest), true, true
	case "basic":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", true, false
		}
		_, pass, found := strings.Cut(string(b), ":")
		if !found {
			return "", true, false
		}
		return pass, true, true
	}
	return "", true, false
}

func unauthorized(w http.ResponseWriter, refuse refuser) {
	w.Header().Set("WWW-Authenticate", `Basic realm="godepot", charset="UTF-8"`)
	refuse(w, http.StatusUnauthorized, "a token is needed: send it as the password of basic authentication or as a bearer token")
}

// principal finds who the request is from. It answers 401 itself (and audits it) when a token is sent and is not good; the principal is then nil.
func (s *Server) principal(w http.ResponseWriter, r *http.Request, refuse refuser) *auth.Principal {
	tok, present, ok := token(r)
	if !present {
		return &auth.Principal{}
	}
	var (
		p   *auth.Principal
		err error
	)
	if ok {
		p, err = s.Auth.Authenticate(r.Context(), tok)
	} else {
		err = errors.New("the Authorization header is not understood")
	}
	if err != nil {
		var inv = errors.Is(err, auth.ErrInvalid) || !ok
		if !inv {
			refuse(w, http.StatusServiceUnavailable, "the database is not available: try again")
			return nil
		}
		s.count("unauthenticated")
		s.Audit.Record(r.Context(), audit.Event{Remote: remote(r), Action: "auth", Result: audit.Refused, Detail: err.Error()})
		unauthorized(w, refuse)
		return nil
	}
	return p
}

func (s *Server) count(reason string) {
	if s.Refused != nil {
		s.Refused.Add(reason, 1)
	}
}

func (s *Server) repository(w http.ResponseWriter, r *http.Request) {
	name, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, repoPrefix), "/")
	set := s.Settings.Load()
	var repo *settings.Repo
	if set != nil {
		repo = set.Repo(name)
	}
	if repo == nil {
		fail(w, http.StatusNotFound, "no such repository")
		return
	}
	f := s.Formats[repo.Format]
	if f == nil {
		fail(w, http.StatusNotImplemented, "this format has no door yet")
		return
	}
	p := s.principal(w, r, fail)
	if p == nil {
		return
	}
	rem := remote(r)
	f.Serve(w, r, format.Request{
		Repo: repo, Path: path, Principal: p, Remote: rem,
		Authorize: func(w http.ResponseWriter, action, path string) bool {
			return s.authorize(w, r.Context(), set, p, rem, repo.Name, path, action, fail)
		},
		Allowed: func(action, path string) bool { return set.Allows(p, repo.Name, path, action) },
	})
}

// authorize checks one action and, when it is refused, answers (401 for a request with no token, 403 for one with a token) and audits it.
func (s *Server) authorize(w http.ResponseWriter, ctx context.Context, set *settings.Settings, p *auth.Principal, rem, repo, path, action string, refuse refuser) bool {
	if set.Allows(p, repo, path, action) {
		return true
	}
	actor, tok := "anonymous", ""
	if !p.Anonymous() {
		actor, tok = p.Account, p.Token
	}
	reason := "forbidden"
	if p.Anonymous() {
		reason = "unauthenticated"
	}
	s.count(reason)
	s.Audit.Record(ctx, audit.Event{Actor: actor, Token: tok, Remote: rem, Action: action, Repo: repo, Path: path, Result: audit.Refused, Detail: reason})
	if p.Anonymous() {
		unauthorized(w, refuse)
	} else {
		refuse(w, http.StatusForbidden, "this token may not "+action+" here")
	}
	return false
}
