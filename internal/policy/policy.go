// Package policy is what a proxy repository decides about a version before it serves it (docs/spec.md, P6, P7): a cooldown (a version that the upstream published less than a given time ago is left
// out of the indexes and refused by name), a block list and an allow list. A block list names versions or digests that are never served; an allow list names the versions that the cooldown does not hold.
// A block always wins: an allow lifts the cooldown and nothing else.
package policy

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Rule is one entry of a list: "name" (every version), "name@version" (the version matches the pattern) or "sha256:<hex>" (a digest). A name and a version are patterns (auth.Match: "*" is any run of
// characters but "/", "**" any run); a scoped name is "@scope/name", and the version, if there is one, follows the last "@".
type Rule struct {
	Name, Version, Digest string
}

// ParseRule reads an entry of a list.
func ParseRule(s string) (Rule, error) {
	if strings.HasPrefix(s, "sha256:") {
		if !digestRE.MatchString(s) {
			return Rule{}, fmt.Errorf("%q is not a digest: sha256: and 64 hex digits in lower case", s)
		}
		return Rule{Digest: s}, nil
	}
	name, version := s, ""
	if i := strings.LastIndex(s, "@"); i > 0 {
		name, version = s[:i], s[i+1:]
		if version == "" {
			return Rule{}, fmt.Errorf("%q: a version pattern after the @", s)
		}
	}
	for _, p := range []string{name, version} {
		if p == "" && version == "" {
			continue
		}
		if err := auth.CheckPattern(p); err != nil {
			return Rule{}, fmt.Errorf("%q: %v", s, err)
		}
	}
	if name == "" || name == "@" {
		return Rule{}, fmt.Errorf("%q: an empty name", s)
	}
	return Rule{Name: name, Version: version}, nil
}

// Matches says whether the rule names the version of the name.
func (r Rule) Matches(name, version string) bool {
	if r.Digest != "" {
		return false
	}
	if !auth.Match(r.Name, name) {
		return false
	}
	return r.Version == "" || auth.Match(r.Version, version)
}

// Policy is the policy of one proxy repository. The zero value holds nothing back.
type Policy struct {
	Cooldown     time.Duration
	Allow, Block []Rule
}

// New reads the words of the settings; an entry that does not parse is left out (the settings check them when they are read, so there is none).
func New(cooldown time.Duration, allow, block []string) *Policy {
	p := &Policy{Cooldown: cooldown}
	for _, s := range allow {
		if r, err := ParseRule(s); err == nil {
			p.Allow = append(p.Allow, r)
		}
	}
	for _, s := range block {
		if r, err := ParseRule(s); err == nil {
			p.Block = append(p.Block, r)
		}
	}
	return p
}

// Active says whether the policy holds anything back.
func (p *Policy) Active() bool { return p != nil && (p.Cooldown > 0 || len(p.Block) > 0) }

// Verdict is the answer for a version.
type Verdict struct {
	OK bool
	// Blocked: the block list names it. Until is when a version that the cooldown holds will be served.
	Blocked bool
	Until   time.Time
	Why     string
}

// Blocked says whether the block list names the version, which needs no time: what a request can be refused by before anything is asked of the upstream.
func (p *Policy) Blocked(name, version string) (Verdict, bool) {
	if p == nil {
		return Verdict{OK: true}, false
	}
	for _, r := range p.Block {
		if r.Matches(name, version) {
			return Verdict{Blocked: true, Why: fmt.Sprintf("%s@%s is on the block list of this repository", name, version)}, true
		}
	}
	return Verdict{OK: true}, false
}

// Version decides about a version of a name that the upstream published at the time (the zero time: the upstream did not say).
func (p *Policy) Version(name, version string, published, now time.Time) Verdict {
	if p == nil {
		return Verdict{OK: true}
	}
	if v, blocked := p.Blocked(name, version); blocked {
		return v
	}
	if p.Cooldown <= 0 {
		return Verdict{OK: true}
	}
	for _, r := range p.Allow {
		if r.Matches(name, version) {
			return Verdict{OK: true}
		}
	}
	if published.IsZero() {
		return Verdict{Why: fmt.Sprintf("%s@%s: the upstream names no time when it was published, so the cooldown of %s cannot be checked (put it on the allow list to serve it)", name, version, p.Cooldown)}
	}
	if until := published.Add(p.Cooldown); now.Before(until) {
		return Verdict{Until: until, Why: fmt.Sprintf("%s@%s was published at %s, less than the cooldown of %s ago: it will be served from %s", name, version, published.UTC().Format(time.RFC3339), p.Cooldown, until.UTC().Format(time.RFC3339))}
	}
	return Verdict{OK: true}
}

// BlockedDigest says whether the block list names the digest ("sha256:<hex>").
func (p *Policy) BlockedDigest(digest string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Block {
		if r.Digest != "" && r.Digest == digest {
			return true
		}
	}
	return false
}
