package format

import (
	"encoding/json"
	"net/http"
)

// OCIError is one error of the OCI distribution API (https://github.com/opencontainers/distribution-spec, "Error Codes").
type OCIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// WriteOCI answers with the "errors" list of the OCI distribution API.
func WriteOCI(w http.ResponseWriter, status int, errs ...OCIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
}

// RefuseOCI is the way the door refuses a request of the OCI API: 401 is UNAUTHORIZED, 403 is DENIED, anything else is UNAVAILABLE.
func RefuseOCI(w http.ResponseWriter, code int, msg string) {
	c := "UNAVAILABLE"
	switch code {
	case http.StatusUnauthorized:
		c = "UNAUTHORIZED"
	case http.StatusForbidden:
		c = "DENIED"
	}
	WriteOCI(w, code, OCIError{Code: c, Message: msg})
}
