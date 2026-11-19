package auth

import (
	"errors"
	"fmt"
	"strings"
)

// The actions a grant or a token can allow (docs/spec.md, A2).
const (
	Read    = "read"
	Write   = "write"
	Delete  = "delete"
	Promote = "promote"
	Admin   = "admin"
)

// A pattern with many wildcards costs a time that grows with the product of their reach; patterns come from users (the scope of a token), so they are short and have few.
const (
	maxPattern   = 256
	maxWildcards = 4
)

// wildcards counts the runs of "*" in the pattern.
func wildcards(p string) int {
	n := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '*' && (i == 0 || p[i-1] != '*') {
			n++
		}
	}
	return n
}

var actions = map[string]bool{Read: true, Write: true, Delete: true, Promote: true, Admin: true}

// Grant allows some actions on the paths of some repositories. It is the form of a token's scope and of the grants in the settings file.
// A pattern has two wildcards: "*" is any run of characters but "/", "**" is any run of characters. Paths empty means every path.
type Grant struct {
	Repositories []string `json:"repositories"`
	Paths        []string `json:"paths,omitempty"`
	Actions      []string `json:"actions"`
}

// Check says what is wrong with the grant, or nil.
func (g Grant) Check() error {
	if len(g.Repositories) == 0 {
		return errors.New("repositories is empty: name at least one repository or pattern (\"*\" is all)")
	}
	if len(g.Actions) == 0 {
		return errors.New("actions is empty")
	}
	for _, a := range g.Actions {
		if !actions[a] {
			return fmt.Errorf("action %q is unknown: use read, write, delete, promote or admin", a)
		}
	}
	for _, p := range append(append([]string{}, g.Repositories...), g.Paths...) {
		if err := CheckPattern(p); err != nil {
			return err
		}
	}
	return nil
}

// CheckPattern says what is wrong with a pattern.
func CheckPattern(p string) error {
	switch {
	case p == "":
		return errors.New("an empty pattern")
	case len(p) > maxPattern:
		return fmt.Errorf("the pattern %.20q... is longer than %d bytes", p, maxPattern)
	case wildcards(p) > maxWildcards:
		return fmt.Errorf("the pattern %q has more than %d wildcards: matching it would take too long", p, maxWildcards)
	}
	return nil
}

// Allows says whether the grant allows the action on the path of the repository.
func (g Grant) Allows(repo, path, action string) bool {
	if !anyMatch(g.Repositories, repo) {
		return false
	}
	if len(g.Paths) > 0 && !anyMatch(g.Paths, path) {
		return false
	}
	for _, a := range g.Actions {
		if a == action || a == Admin { // admin on a place is every action there
			return true
		}
	}
	return false
}

func anyMatch(patterns []string, s string) bool {
	for _, p := range patterns {
		if Match(p, s) {
			return true
		}
	}
	return false
}

// Match says whether s fits the pattern.
func Match(pattern, s string) bool {
	for len(pattern) > 0 {
		if strings.HasPrefix(pattern, "**") {
			rest := strings.TrimLeft(pattern, "*")
			if rest == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if Match(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		if pattern[0] == '*' {
			rest := pattern[1:]
			for i := 0; ; i++ {
				if Match(rest, s[i:]) {
					return true
				}
				if i >= len(s) || s[i] == '/' {
					return false
				}
			}
		}
		if s == "" || pattern[0] != s[0] {
			return false
		}
		pattern, s = pattern[1:], s[1:]
	}
	return s == ""
}
