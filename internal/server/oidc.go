package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/oidc"
	"github.com/zystem/godepot/internal/settings"
)

// jobLife is how long the token that a CI job gets lives (docs/spec.md, A3). It is not a setting: a job that runs longer exchanges again.
const jobLife = 15 * time.Minute

// exchange is POST /api/v1/oidc/token: the signed token of a CI job for a token of godepot. It needs no token of godepot: the signature of a trusted issuer is the credential. The body is
// {"token": "<the job's token>", "account": "<the account of a trust, when more than one fits>"}.
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	var body struct {
		Token   string `json:"token"`
		Account string `json:"account"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ip := remote(r)
	if s.fails.blocked(ip, time.Now()) {
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusTooManyRequests, "too many failed logins: try again later")
		return
	}
	refused := func(code int, reason, detail, msg string) {
		s.count(reason)
		s.Audit.Record(r.Context(), audit.Event{Actor: "oidc", Remote: ip, Action: "oidc.exchange", Result: audit.Refused, Detail: clip(detail, 500)})
		fail(w, code, msg)
	}
	notAccepted := func(detail string) {
		s.fails.add(ip, time.Now())
		refused(http.StatusUnauthorized, "unauthenticated", detail, "the token is not accepted")
	}
	set := s.Settings.Load()
	iss, err := oidc.Peek(body.Token)
	if err != nil {
		notAccepted(err.Error())
		return
	}
	var issuer *settings.Issuer
	if set != nil {
		issuer = set.IssuerByURL(iss)
	}
	if issuer == nil {
		notAccepted(fmt.Sprintf("no issuer of the settings is %q", iss))
		return
	}
	claims, err := s.OIDC.Verify(r.Context(), oidc.Issuer{URL: issuer.URL, Audience: issuer.Audience, JWKSURL: issuer.JWKSURL, AllowPrivate: issuer.AllowPrivate}, body.Token)
	if errors.Is(err, oidc.ErrUnavailable) {
		w.Header().Set("Retry-After", "5")
		refused(http.StatusServiceUnavailable, "upstream", err.Error(), "the keys of the issuer cannot be read: try again")
		return
	}
	if err != nil {
		notAccepted(err.Error())
		return
	}
	// From here the claims are the issuer's own words.
	who := fmt.Sprintf("issuer %s, sub %s", issuer.Name, claim(claims["sub"]))
	var trust *settings.Trust
	for i := range set.Trusts {
		t := &set.Trusts[i]
		if t.Issuer == issuer.Name && (body.Account == "" || body.Account == t.Account) && fits(t.Claims, claims) {
			trust = t
			break
		}
	}
	if trust == nil {
		refused(http.StatusForbidden, "forbidden", "no trust fits: "+who, "no trust of this server fits the token: the audit log says what the token is")
		return
	}
	a, err := s.Auth.Account(r.Context(), trust.Account)
	switch {
	case errors.Is(err, auth.ErrNoAccount):
		refused(http.StatusForbidden, "forbidden", "the account "+trust.Account+" does not exist: "+who, "the account of the trust cannot be used")
		return
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	case a.Kind != "robot" || a.Admin || a.Disabled:
		refused(http.StatusForbidden, "forbidden", "the account "+trust.Account+" is not an enabled robot that is not an admin: "+who, "the account of the trust cannot be used")
		return
	}
	tok, info, err := s.Auth.CreateToken(r.Context(), a.Name, trust.Scope(), jobLife, clip("oidc "+who, 200))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	s.Audit.Record(r.Context(), audit.Event{Actor: a.Name, Token: info.ID, Remote: ip, Action: "oidc.exchange", Result: audit.OK, Detail: clip("token "+info.ID+", "+who, 500)})
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusCreated, map[string]any{"token": tok, "account": a.Name, "expires_at": info.Expires})
}

// claim is the text of a claim that a condition may be put on: a string, a number or a boolean. "" for a list or an object, which no condition fits.
func claim(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case json.Number:
		return c.String()
	case bool:
		return strconv.FormatBool(c)
	}
	return ""
}

// fits says whether every condition of a trust holds: the claim is there and its text fits the pattern.
func fits(conditions map[string]string, claims map[string]any) bool {
	names := make([]string, 0, len(conditions))
	for n := range conditions {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		v := claim(claims[n])
		if v == "" || !auth.Match(conditions[n], v) {
			return false
		}
	}
	return len(conditions) > 0 // a trust with no condition fits nobody
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
