// Package settings reads the settings file: the repositories and who may do what in them (docs/spec.md, O1). The file is JSON, checked as a whole (every problem at once, an unknown word is a problem),
// read at start and on SIGHUP. Accounts and tokens are not in it: they are made at run time and live in the database.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zystem/godepot/internal/auth"
)

// Formats that exist. A format is added here when its door is built.
var formats = map[string]bool{"raw": true}

// Repo is one repository.
type Repo struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	// Kind is "hosted" (written by you). "proxy" and "group" come with later steps of the plan and are refused now.
	Kind string `json:"kind"`
	// AnonymousRead lets a request with no token read. Default false.
	AnonymousRead bool `json:"anonymous_read,omitempty"`
	// OverwritePaths are the patterns of the paths that may be written again with other bytes. Default none: a path, once written, is immutable (docs/spec.md, R4).
	OverwritePaths []string `json:"overwrite_paths,omitempty"`
	// QuotaBytes and QuotaComponents are the limits of the repository (R7); 0 is no limit.
	QuotaBytes      int64 `json:"quota_bytes,omitempty"`
	QuotaComponents int64 `json:"quota_components,omitempty"`
}

// Overwrites says whether the path may be written again.
func (r *Repo) Overwrites(path string) bool {
	for _, p := range r.OverwritePaths {
		if auth.Match(p, path) {
			return true
		}
	}
	return false
}

// Rule gives an account a grant.
type Rule struct {
	Account string `json:"account"`
	auth.Grant
}

// Settings is the file.
type Settings struct {
	Repositories []Repo `json:"repositories"`
	Grants       []Rule `json:"grants"`
}

// Parse reads and checks the settings. It returns every problem at once.
func Parse(r io.Reader) (*Settings, error) {
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		return nil, err
	}
	s := &Settings{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("the settings file is not valid: %w", err)
	}
	if dec.More() {
		return nil, errors.New("the settings file has more than one value")
	}
	if err := s.Check(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load reads the file at path.
func Load(path string) (*Settings, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is the operator's own setting
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Check says everything that is wrong with the settings.
func (s *Settings) Check() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	seen := map[string]bool{}
	for i, r := range s.Repositories {
		who := fmt.Sprintf("repository %d (%q)", i+1, r.Name)
		if err := auth.CheckName(r.Name); err != nil {
			add("%s: %v", who, err)
		}
		if seen[r.Name] {
			add("%s: the name is used twice", who)
		}
		seen[r.Name] = true
		if !formats[r.Format] {
			add("%s: the format %q is not known (known: raw)", who, r.Format)
		}
		switch r.Kind {
		case "hosted":
		case "proxy", "group":
			add("%s: kind %q is not built yet (only hosted)", who, r.Kind)
		default:
			add("%s: kind %q is not known: use hosted", who, r.Kind)
		}
		if r.QuotaBytes < 0 || r.QuotaComponents < 0 {
			add("%s: a quota is negative", who)
		}
		for _, p := range r.OverwritePaths {
			if err := auth.CheckPattern(p); err != nil {
				add("%s: overwrite_paths: %v", who, err)
			}
		}
	}
	for i, g := range s.Grants {
		who := fmt.Sprintf("grant %d (account %q)", i+1, g.Account)
		if err := auth.CheckName(g.Account); err != nil {
			add("%s: %v", who, err)
		}
		if err := g.Grant.Check(); err != nil {
			add("%s: %v", who, err)
		}
	}
	return errors.Join(errs...)
}

// Repo finds a repository by name.
func (s *Settings) Repo(name string) *Repo {
	for i := range s.Repositories {
		if s.Repositories[i].Name == name {
			return &s.Repositories[i]
		}
	}
	return nil
}

// Allows says whether the principal may do the action on the path of the repository. An unknown repository allows nothing. A request with no token may read what the repository lets it.
// With a token, both must say yes: the account (an admin account, or a grant for it in the file) and the scope of the token. A token never allows more than its account has.
func (s *Settings) Allows(p *auth.Principal, repo, path, action string) bool {
	r := s.Repo(repo)
	if r == nil {
		return false
	}
	if action == auth.Read && r.AnonymousRead {
		return true // what anyone may read, a token that is too narrow may read too
	}
	if p.Anonymous() {
		return false
	}
	if !p.ScopeAllows(repo, path, action) {
		return false
	}
	if p.Admin {
		return true
	}
	for _, g := range s.Grants {
		if g.Account == p.Account && g.Grant.Allows(repo, path, action) {
			return true
		}
	}
	return false
}
