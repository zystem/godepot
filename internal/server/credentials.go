package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/seal"
	"github.com/zystem/godepot/internal/upstream"
)

const (
	// ldapSecret is the name under which the password of the directory's service account is kept.
	ldapSecret = "ldap"
	// maxCredential bounds a password or token in bytes.
	maxCredential = 8 << 10
)

// wanted says whether the settings have a place for the credential of a name, and which variable of theirs would be used instead of what is stored.
func (s *Server) wanted(name string) (known bool, instead string) {
	set := s.Settings.Load()
	if set == nil {
		return false, ""
	}
	if name == ldapSecret {
		if set.LDAP == nil {
			return false, ""
		}
		return true, set.LDAP.PasswordEnv
	}
	repo, ok := strings.CutPrefix(name, upstream.SecretName(""))
	if !ok {
		return false, ""
	}
	if r := set.Repo(repo); r != nil && r.Kind == "proxy" && r.Upstream != nil {
		return true, r.Upstream.PasswordEnv + r.Upstream.TokenEnv
	}
	return false, ""
}

// credentials is /api/v1/credentials and /api/v1/credentials/<name>: the passwords that godepot shows to others, "ldap" and "upstream/<repository>". The body of a PUT is the secret itself
// (curl --data-binary @file); the line breaks at its end are taken off. A secret is never told back (docs/secrets.md).
func (s *Server) credentials(w http.ResponseWriter, r *http.Request, p *auth.Principal, name string) {
	if s.Secrets == nil {
		fail(w, http.StatusNotFound, "no such call")
		return
	}
	allow := "PUT, DELETE"
	action := "credential.set"
	if name == "" {
		allow, action = "GET", "credential.list"
	} else if r.Method == http.MethodDelete {
		action = "credential.delete"
	}
	if !strings.Contains(", "+allow+",", ", "+r.Method+",") {
		w.Header().Set("Allow", allow)
		fail(w, http.StatusMethodNotAllowed, "this call takes "+allow)
		return
	}
	if !s.admin(w, r, p, action) {
		return
	}
	down := func() { fail(w, http.StatusServiceUnavailable, "the database is not available: try again") }
	changed := func(detail string) {
		if repo, ok := strings.CutPrefix(name, upstream.SecretName("")); ok && s.CredentialChanged != nil {
			s.CredentialChanged(repo)
		}
		s.Audit.Record(r.Context(), s.event(r, p, action, audit.OK, detail))
		w.WriteHeader(http.StatusNoContent)
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.Secrets.List(r.Context())
		if err != nil {
			down()
			return
		}
		reply(w, http.StatusOK, map[string]any{"items": list})
	case http.MethodDelete:
		if err := s.Secrets.Delete(r.Context(), name); err != nil {
			down()
			return
		}
		changed(clip(name, 200))
	case http.MethodPut:
		known, instead := s.wanted(name)
		if !known {
			fail(w, http.StatusNotFound, "the settings have no place for this credential: it is ldap (with an ldap section) or upstream/<a proxy repository>")
			return
		}
		if instead != "" {
			fail(w, http.StatusConflict, "the settings name the variable "+instead+" for this credential, and it would be used instead: take password_env or token_env out of the settings")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCredential+1))
		if err != nil {
			fail(w, http.StatusBadRequest, "the body cannot be read")
			return
		}
		value := strings.TrimRight(string(body), "\r\n")
		if value == "" || len(body) > maxCredential || strings.ContainsAny(value, "\r\n\x00") {
			fail(w, http.StatusBadRequest, "the body is the password or token itself: one line of at most 8 KiB")
			return
		}
		if err := s.Secrets.Set(r.Context(), name, value, p.Account); err != nil {
			if errors.Is(err, seal.ErrWrongKey) {
				fail(w, http.StatusServiceUnavailable, "this replica has an old seal key: restart it")
				return
			}
			down()
			return
		}
		changed(name)
	}
}
