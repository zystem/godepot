package server

import (
	"errors"
	"net/http"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/trash"
)

// cleanup is GET (the report of the policy as it would be now, changing nothing; ?report=last for the last report that was made) and POST (run the policy now: it removes only when the repository says
// enforce) of /api/v1/repositories/<name>/cleanup (docs/spec.md, C1, C2).
func (s *Server) cleanup(w http.ResponseWriter, r *http.Request, p *auth.Principal, repo string) {
	if !s.admin(w, r, p, "cleanup") {
		return
	}
	set := s.Settings.Load()
	if s.Cleanup == nil || set == nil || set.Repo(repo) == nil || set.Repo(repo).Cleanup == nil {
		fail(w, http.StatusNotFound, "no such repository, or it has no cleanup policy")
		return
	}
	l := set.Repo(repo)
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("report") == "last":
		rep, err := s.Cleanup.Last(r.Context(), repo)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
			return
		}
		if rep == nil {
			fail(w, http.StatusNotFound, "the policy has not run yet")
			return
		}
		reply(w, http.StatusOK, rep)
	case r.Method == http.MethodGet:
		rep, err := s.Cleanup.Plan(r.Context(), l)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "the report could not be made: "+err.Error())
			return
		}
		reply(w, http.StatusOK, rep)
	default:
		rep, err := s.Cleanup.Run(r.Context(), l, l.Cleanup.Enforce, p.Account)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "the policy could not run: "+err.Error())
			return
		}
		reply(w, http.StatusOK, rep)
	}
}

// trashList is GET /api/v1/trash[?repository=name].
func (s *Server) trashList(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !s.admin(w, r, p, "trash.list") {
		return
	}
	if s.Trash == nil {
		fail(w, http.StatusNotFound, "there is no trash")
		return
	}
	items, err := s.Trash.List(r.Context(), r.URL.Query().Get("repository"))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the database is not available: try again")
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items, "days": s.Trash.Days})
}

// trashRestore is POST /api/v1/trash/<id>/restore: the item comes back whole, or (409) a path of it has been written since.
func (s *Server) trashRestore(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string) {
	if !s.admin(w, r, p, "trash.restore") {
		return
	}
	if s.Trash == nil {
		fail(w, http.StatusNotFound, "there is no trash")
		return
	}
	it, err := s.Trash.Restore(r.Context(), id, p.Account)
	switch {
	case errors.Is(err, trash.ErrGone):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, trash.ErrExists):
		fail(w, http.StatusConflict, err.Error())
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "it could not be restored: "+err.Error())
	default:
		reply(w, http.StatusOK, it)
	}
}

// trashPurge is DELETE /api/v1/trash/<id>: empty the item now.
func (s *Server) trashPurge(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string) {
	if !s.admin(w, r, p, "trash.purge") {
		return
	}
	if s.Trash == nil {
		fail(w, http.StatusNotFound, "there is no trash")
		return
	}
	switch err := s.Trash.Purge(r.Context(), id, p.Account); {
	case errors.Is(err, trash.ErrGone):
		fail(w, http.StatusNotFound, err.Error())
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "it could not be emptied: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
