// Package format is what the server asks of a format (raw, later OCI, Maven, ...): to answer the requests of the repositories that have it. The server finds the repository, checks the token and
// hands over a Request; the format knows which action each of its requests is, and asks Authorize for it. The interface is as small as the one format that uses it needs; the next steps grow it.
package format

import (
	"net/http"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/settings"
)

// Request is a request for a repository.
type Request struct {
	Repo *settings.Repo
	// Path is what follows the name of the repository in the address, without the leading "/"; it ends with "/" for a directory.
	Path      string
	Principal *auth.Principal
	Remote    string
	// Authorize says whether the principal may do the action on the path. When it may not, the answer to the client (401 or 403) has been written and audited, and the format returns.
	Authorize func(w http.ResponseWriter, action, path string) bool
	// Set is the settings that the request was served under (the members of a group are found in it); Address is the repository as the client named it in the address, which is the group when the
	// request was sent to a group and Repo has been replaced by the member that answers.
	Set     *settings.Settings
	Address string
	// Allowed says the same as Authorize without answering the client, for a format that asks about another path than the one of the request (the source of an OCI mount).
	Allowed func(action, path string) bool
}

// Format answers the requests of the repositories of one format.
type Format interface {
	Serve(w http.ResponseWriter, r *http.Request, q Request)
}

// Actor is the account the request is from, "anonymous" when it had no token.
func (q Request) Actor() string {
	if q.Principal.Anonymous() {
		return "anonymous"
	}
	return q.Principal.Account
}
