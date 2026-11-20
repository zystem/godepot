package server

import (
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/settings"
)

// v2 is the door of the OCI distribution API: "/v2/" is the check that clients make first, and "/v2/<repository>/<image>/..." is a request for the repository named by the first part of the name
// (docs/spec.md, F1: one port and one host name; the repository is the first part of the name of an image). The format sees the rest of the address as the path.
func (s *Server) v2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := s.principal(w, r, format.RefuseOCI)
	if p == nil {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		// The check of the client. A token that is good gets 200; a request with none gets the challenge, as a registry does, so that "docker login" and the clients that learn from the first
		// answer send their credentials.
		if p.Anonymous() {
			s.count("unauthenticated")
			unauthorized(w, format.RefuseOCI)
			return
		}
		reply(w, http.StatusOK, map[string]any{})
		return
	}
	name, path, _ := strings.Cut(rest, "/")
	set := s.Settings.Load()
	var repo *settings.Repo
	if set != nil {
		repo = set.Repo(name)
	}
	if repo == nil || repo.Format != "oci" {
		format.WriteOCI(w, http.StatusNotFound, format.OCIError{Code: "NAME_UNKNOWN", Message: "the first part of the name is the repository, and no repository of the format oci has this name"})
		return
	}
	f := s.Formats[repo.Format]
	if f == nil {
		format.WriteOCI(w, http.StatusNotImplemented, format.OCIError{Code: "UNSUPPORTED", Message: "this format has no door yet"})
		return
	}
	rem := remote(r)
	f.Serve(w, r, format.Request{
		Repo: repo, Path: path, Principal: p, Remote: rem, Set: set, Address: repo.Name,
		Authorize: func(w http.ResponseWriter, action, path string) bool {
			return s.authorize(w, r.Context(), set, p, rem, repo.Name, path, action, format.RefuseOCI)
		},
		Allowed: func(action, path string) bool { return set.Allows(p, repo.Name, path, action) },
	})
}
