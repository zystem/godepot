// Package settings reads the settings file: the repositories and who may do what in them (docs/spec.md, O1). The file is JSON, checked as a whole (every problem at once, an unknown word is a problem),
// read at start and on SIGHUP. Accounts and tokens are not in it: they are made at run time and live in the database.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/policy"
)

// Formats that exist. A format is added here when its door is built.
var formats = map[string]bool{"raw": true, "oci": true, "go": true, "pypi": true, "npm": true, "maven": true}

// Repo is one repository.
type Repo struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	// Kind is "hosted" (written by you), "proxy" (a copy, made on demand, of one upstream: Upstream) or "group" (an ordered list of repositories of the same format read as one: Members).
	Kind string `json:"kind"`
	// Upstream is the registry a proxy repository copies from (docs/spec.md, P1 to P5, P8, P9). Only for kind proxy.
	Upstream *Upstream `json:"upstream,omitempty"`
	// Members are the repositories of a group, in the order in which they answer (R2): names of repositories of the same format, groups included. Only for kind group.
	Members []string `json:"members,omitempty"`
	// WriteTo is the hosted member that a write to the group goes to (T20). Default none: a write to the group is refused.
	WriteTo string `json:"write_to,omitempty"`
	// Claims are prefixes of names that belong to the hosted members of the group before anything is published under them (R3): a name under a claim that no hosted member has is a 404, not a fetch.
	Claims []string `json:"claims,omitempty"`
	// Merge would make a group merge the versions of one name from all its members (R2). Not built: true is refused.
	Merge bool `json:"merge,omitempty"`
	// AnonymousRead lets a request with no token read. Default false.
	AnonymousRead bool `json:"anonymous_read,omitempty"`
	// OverwritePaths are the patterns of the paths that may be written again with other bytes. Default none: a path, once written, is immutable (docs/spec.md, R4).
	OverwritePaths []string `json:"overwrite_paths,omitempty"`
	// ImmutableTags are the patterns of the tags of an OCI repository that, once they name a manifest, never name another (R4). Default none: a tag may move. Only for the format oci.
	ImmutableTags []string `json:"immutable_tags,omitempty"`
	// QuotaBytes and QuotaComponents are the limits of the repository (R7); 0 is no limit.
	QuotaBytes      int64 `json:"quota_bytes,omitempty"`
	QuotaComponents int64 `json:"quota_components,omitempty"`
	// Cleanup is the policy that removes what is old (docs/spec.md, C1, C2): a report every time it runs, and no removal until Enforce is true. Not for a group.
	Cleanup *Cleanup `json:"cleanup,omitempty"`
}

// Cleanup is the cleanup policy of a repository. A version (a module version, a file of a version, a package version; for the format raw, a file) is removed when RemoveOlderDays is set and it is older and
// none of the keep rules holds. A keep rule always wins (C1).
type Cleanup struct {
	// Enforce makes the policy remove what its report names. Default false: the policy only writes the report (C2).
	Enforce bool `json:"enforce,omitempty"`
	// RemoveOlderDays removes the versions written more than this many days ago. 0 means every version that is not kept. Default: not set, and then nothing is removed.
	RemoveOlderDays *int `json:"remove_older_days,omitempty"`
	// KeepNewest keeps the newest N versions of each name (by when they were written). Default 0: none kept by this rule.
	KeepNewest int `json:"keep_newest,omitempty"`
	// KeepReadDays keeps what was read in the last D days. Default 0: none kept by this rule.
	KeepReadDays int `json:"keep_read_days,omitempty"`
	// Keep are patterns of the names (for the format raw, the paths) that are kept. Default none.
	Keep []string `json:"keep,omitempty"`
	// KeepProps keeps what has all of these properties (key and value). Default none.
	KeepProps map[string]string `json:"keep_props,omitempty"`
}

// Duration is a length of time written in the file as a string of Go ("90s", "5m", "24h"); "0" is none.
type Duration time.Duration

// UnmarshalJSON reads "5m".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a time is a string like \"90s\", \"5m\" or \"24h\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("%q is not a time (like \"90s\", \"5m\", \"24h\"; not negative)", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes "5m".
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Upstream is the registry that a proxy repository copies from.
type Upstream struct {
	// URL is the address of the upstream: its origin, and a path when it has one ("https://registry-1.docker.io", "https://repo.example.com/raw"). Required.
	URL string `json:"url"`
	// Username and PasswordEnv are sent as basic authentication (for a registry that has a token service: to the service of the hosts in TokenHosts). TokenEnv is sent as a bearer token. The secrets
	// are never in the file: PasswordEnv and TokenEnv name environment variables.
	Username    string `json:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
	TokenEnv    string `json:"token_env,omitempty"`
	// TokenHosts are the hosts, besides the upstream's own, that may be sent the credentials when the upstream answers 401 and names them as its token service (Docker Hub: "auth.docker.io"). Default
	// none: the token service is asked with no credentials, and credentials are never sent to another host (P8).
	TokenHosts []string `json:"token_hosts,omitempty"`
	// FileHosts are the hosts, besides the upstream's own, that an index of the upstream may name for its files (PyPI: files.pythonhosted.org). Default none. They are never sent credentials.
	FileHosts []string `json:"file_hosts,omitempty"`
	// AllowPrivate lets godepot fetch from loopback, private and link-local addresses (P9). Default false: such an address is refused after the name is resolved.
	AllowPrivate bool `json:"allow_private,omitempty"`
	// MaxPerSecond limits the requests to the upstream (0: no limit). MaxConcurrent limits those at once (default 16).
	MaxPerSecond  float64 `json:"max_per_second,omitempty"`
	MaxConcurrent int     `json:"max_concurrent,omitempty"`
	// TTL is how long what can change is trusted before it is asked again: an OCI tag (default 5m), a raw file (default none: a file is fetched once and never asked again, P3). NegativeTTL is how long
	// a "not found" is remembered (default 10m, P4).
	TTL         *Duration `json:"ttl,omitempty"`
	NegativeTTL *Duration `json:"negative_ttl,omitempty"`
	// ImmutablePaths are the patterns of the raw files that cannot change upstream: other bytes under such a path are kept out, and are an alarm (P5). Only for the format raw.
	ImmutablePaths []string `json:"immutable_paths,omitempty"`
	// Cooldown holds back a version that the upstream published less than this long ago (docs/spec.md, P6): it is left out of the indexes this repository serves and is refused by name. Default none. For
	// the formats npm, pypi and go. Allow lifts it for the versions it names; Block names versions and digests that are never served (P7), for every format but oci.
	Cooldown *Duration `json:"cooldown,omitempty"`
	Allow    []string  `json:"allow,omitempty"`
	Block    []string  `json:"block,omitempty"`
}

// Policy is the cooldown and the lists of the upstream.
func (u *Upstream) Policy() *policy.Policy {
	var c time.Duration
	if u.Cooldown != nil {
		c = time.Duration(*u.Cooldown)
	}
	return policy.New(c, u.Allow, u.Block)
}

// Default values of an upstream.
const (
	DefaultMaxConcurrent = 16
	DefaultTagTTL        = 5 * time.Minute
	DefaultNegativeTTL   = 10 * time.Minute
)

// FileTTL is how long a raw file is trusted (0: for ever).
func (u *Upstream) FileTTL() time.Duration {
	if u.TTL == nil {
		return 0
	}
	return time.Duration(*u.TTL)
}

// TagTTL is how long an OCI tag is trusted.
func (u *Upstream) TagTTL() time.Duration {
	if u.TTL == nil {
		return DefaultTagTTL
	}
	return time.Duration(*u.TTL)
}

// MissTTL is how long a "not found" is remembered.
func (u *Upstream) MissTTL() time.Duration {
	if u.NegativeTTL == nil {
		return DefaultNegativeTTL
	}
	return time.Duration(*u.NegativeTTL)
}

// Immutable says whether the raw file may not change upstream.
func (u *Upstream) Immutable(path string) bool {
	for _, p := range u.ImmutablePaths {
		if auth.Match(p, path) {
			return true
		}
	}
	return false
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ImmutableTag says whether the tag may not be moved.
func (r *Repo) ImmutableTag(tag string) bool {
	for _, p := range r.ImmutableTags {
		if auth.Match(p, tag) {
			return true
		}
	}
	return false
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

// Rule gives an account, or the people of a group of the directory, a grant: one of Account and Group.
type Rule struct {
	Account string `json:"account,omitempty"`
	Group   string `json:"group,omitempty"`
	auth.Grant
}

// Settings is the file.
type Settings struct {
	Repositories []Repo `json:"repositories"`
	Grants       []Rule `json:"grants"`
	// Issuers are the issuers of signed tokens that are trusted, and Trusts say which of their tokens may be exchanged for a token of godepot, and for what (docs/spec.md, A3).
	Issuers []Issuer `json:"issuers,omitempty"`
	Trusts  []Trust  `json:"trusts,omitempty"`
	// LDAP is the directory that people log in with (docs/spec.md, A1, A4). Default none.
	LDAP *LDAP `json:"ldap,omitempty"`
}

// LDAP is where the directory is and how people and groups are found in it.
type LDAP struct {
	URL         string    `json:"url"`
	StartTLS    bool      `json:"start_tls,omitempty"`
	CAFile      string    `json:"ca_file,omitempty"`
	ServerName  string    `json:"server_name,omitempty"`
	BindDN      string    `json:"bind_dn"`
	PasswordEnv string    `json:"password_env"`
	UserBase    string    `json:"user_base"`
	UserFilter  string    `json:"user_filter"`
	GroupBase   string    `json:"group_base,omitempty"`
	GroupFilter string    `json:"group_filter,omitempty"`
	GroupName   string    `json:"group_name,omitempty"`
	Refresh     *Duration `json:"refresh,omitempty"`
}

// Defaults of the directory.
const (
	DefaultGroupFilter = "(member={dn})"
	DefaultGroupName   = "cn"
	DefaultRefresh     = 5 * time.Minute
)

// RefreshEvery is how long what the directory said of a person is trusted.
func (l *LDAP) RefreshEvery() time.Duration {
	if l.Refresh == nil {
		return DefaultRefresh
	}
	return time.Duration(*l.Refresh)
}

func (s *Settings) checkLDAP(add func(string, ...any)) {
	l := s.LDAP
	if l == nil {
		return
	}
	p, err := url.Parse(l.URL)
	switch {
	case err != nil || p.Host == "" || (p.Scheme != "ldaps" && p.Scheme != "ldap"):
		add("ldap: url is %q: it must be an address like ldaps://dc.example.com:636", l.URL)
	case p.Scheme == "ldap" && !l.StartTLS:
		add("ldap: url is ldap and start_tls is not set: a password is never sent in the clear")
	case p.Scheme == "ldaps" && l.StartTLS:
		add("ldap: start_tls is for an ldap url; ldaps is encrypted from the start")
	case p.User != nil || p.RawQuery != "" || strings.Trim(p.Path, "/") != "":
		add("ldap: url has a user name, a path or a query")
	}
	for _, f := range []struct{ name, v string }{{"bind_dn", l.BindDN}, {"user_base", l.UserBase}, {"user_filter", l.UserFilter}} {
		if f.v == "" {
			add("ldap: %s is required", f.name)
		}
	}
	if !envNameRE.MatchString(l.PasswordEnv) {
		add("ldap: password_env is required: the name of the environment variable that holds the password of bind_dn")
	}
	if l.UserFilter != "" && !strings.Contains(l.UserFilter, "{user}") {
		add("ldap: user_filter has no {user}: it would find the same entry for every name")
	}
	if l.GroupFilter != "" && !strings.Contains(l.GroupFilter, "{dn}") && !strings.Contains(l.GroupFilter, "{user}") {
		add("ldap: group_filter has neither {dn} nor {user}: everybody would be in the same groups")
	}
	if l.GroupBase == "" && (l.GroupFilter != "" || l.GroupName != "") {
		add("ldap: group_filter and group_name need group_base")
	}
	if l.Refresh != nil && time.Duration(*l.Refresh) <= 0 {
		add("ldap: refresh must be more than 0")
	}
}

// Issuer is who signs the tokens of CI jobs: GitHub Actions, GitLab, Kubernetes, anything that publishes its keys as OpenID Connect does.
type Issuer struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Audience     string `json:"audience"`
	JWKSURL      string `json:"jwks_url,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
}

// Trust lets a token of the issuer whose claims fit become a token of the robot account.
type Trust struct {
	Issuer  string            `json:"issuer"`
	Account string            `json:"account"`
	Claims  map[string]string `json:"claims"`
	// Repositories, Paths and Actions narrow the token (default: every repository, every path, read and write). The grants of the account narrow it too.
	Repositories []string `json:"repositories,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Actions      []string `json:"actions,omitempty"`
}

// Scope is the scope of the token that the trust gives.
func (t *Trust) Scope() []auth.Grant {
	g := auth.Grant{Repositories: t.Repositories, Paths: t.Paths, Actions: t.Actions}
	if len(g.Repositories) == 0 {
		g.Repositories = []string{"*"}
	}
	if len(g.Actions) == 0 {
		g.Actions = []string{auth.Read, auth.Write}
	}
	return []auth.Grant{g}
}

// IssuerByURL finds the issuer whose tokens name this address, or nil.
func (s *Settings) IssuerByURL(address string) *Issuer {
	for i := range s.Issuers {
		if s.Issuers[i].URL == address {
			return &s.Issuers[i]
		}
	}
	return nil
}

// identifying are the claims that say whose job a token is. A trust must pin one: an issuer such as GitHub signs tokens for everybody's jobs.
var identifying = []string{"sub", "repository", "repository_id", "repository_owner", "repository_owner_id", "project_path", "project_id", "namespace_path", "namespace_id"}

func (s *Settings) checkTrust(add func(string, ...any)) {
	names, urls := map[string]bool{}, map[string]bool{}
	for i, is := range s.Issuers {
		who := fmt.Sprintf("issuer %d (%q)", i+1, is.Name)
		if err := auth.CheckName(is.Name); err != nil {
			add("%s: %v", who, err)
		}
		if names[is.Name] {
			add("%s: the name is used twice", who)
		}
		if urls[is.URL] {
			add("%s: the url is used twice", who)
		}
		names[is.Name], urls[is.URL] = true, true
		for _, a := range []struct{ name, v string }{{"url", is.URL}, {"jwks_url", is.JWKSURL}} {
			if a.v == "" && a.name == "jwks_url" {
				continue
			}
			p, err := url.Parse(a.v)
			switch {
			case err != nil || p.Host == "" || (p.Scheme != "https" && p.Scheme != "http"):
				add("%s: %s is %q: it must be an address like https://issuer.example.com", who, a.name, a.v)
			case p.Scheme == "http" && !is.AllowPrivate:
				add("%s: %s is not https (only an issuer with allow_private may be http)", who, a.name)
			case p.User != nil || p.RawQuery != "" || p.Fragment != "":
				add("%s: %s has a user name, a query or a fragment", who, a.name)
			}
		}
		if is.Audience == "" {
			add("%s: audience is required: without it a token meant for another service would be taken", who)
		}
	}
	for i, t := range s.Trusts {
		who := fmt.Sprintf("trust %d (account %q)", i+1, t.Account)
		if !names[t.Issuer] {
			add("%s: issuer %q is not one of the issuers", who, t.Issuer)
		}
		if err := auth.CheckName(t.Account); err != nil {
			add("%s: %v", who, err)
		}
		pinned := false
		for name, pattern := range t.Claims {
			if err := auth.CheckPattern(pattern); err != nil {
				add("%s: claims %q: %v", who, name, err)
			}
			if slices.Contains(identifying, name) && !strings.HasPrefix(pattern, "*") {
				pinned = true
			}
		}
		if !pinned {
			add("%s: claims must pin who the job is: one of %s, with a value that does not begin with \"*\"", who, strings.Join(identifying, ", "))
		}
		if slices.Contains(t.Actions, auth.Admin) {
			add("%s: actions: a job is never given admin", who)
		}
		for _, g := range t.Scope() {
			if err := g.Check(); err != nil {
				add("%s: %v", who, err)
			}
		}
	}
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
			add("%s: the format %q is not known (known: raw, oci, go, pypi, npm, maven)", who, r.Format)
		}
		switch r.Kind {
		case "hosted":
			if r.Upstream != nil || len(r.Members) > 0 || r.WriteTo != "" || len(r.Claims) > 0 || r.Merge {
				add("%s: upstream is for a proxy; members, write_to, claims and merge are for a group", who)
			}
		case "proxy":
			s.checkProxy(who, &r, add)
		case "group":
			s.checkGroup(who, &r, add)
		default:
			add("%s: kind %q is not known: use hosted, proxy or group", who, r.Kind)
		}
		if r.Kind != "hosted" && (len(r.OverwritePaths) > 0 || len(r.ImmutableTags) > 0 || r.QuotaBytes != 0 || r.QuotaComponents != 0) {
			add("%s: overwrite_paths, immutable_tags and quotas are for a hosted repository", who)
		} else if r.Format == "oci" {
			if len(r.OverwritePaths) > 0 {
				add("%s: overwrite_paths is for the format raw; an OCI repository has immutable_tags", who)
			}
			if r.QuotaBytes != 0 || r.QuotaComponents != 0 {
				add("%s: a quota is not built for the format oci yet", who)
			}
			for _, p := range r.ImmutableTags {
				if err := auth.CheckPattern(p); err != nil {
					add("%s: immutable_tags: %v", who, err)
				}
			}
		} else if len(r.ImmutableTags) > 0 {
			add("%s: immutable_tags is for the format oci", who)
		} else if (r.Format == "go" || r.Format == "pypi" || r.Format == "npm" || r.Format == "maven") && len(r.OverwritePaths) > 0 {
			add("%s: overwrite_paths is for the format raw; a published file of this format is never overwritten", who)
		}
		if r.QuotaBytes < 0 || r.QuotaComponents < 0 {
			add("%s: a quota is negative", who)
		}
		if c := r.Cleanup; c != nil {
			switch {
			case r.Kind == "group":
				add("%s: cleanup is for a hosted or a proxy repository, not a group (clean the members)", who)
			case r.Format == "oci":
				add("%s: cleanup is not built for the format oci yet", who)
			}
			if (c.RemoveOlderDays != nil && *c.RemoveOlderDays < 0) || c.KeepNewest < 0 || c.KeepReadDays < 0 {
				add("%s: cleanup: a number of days or versions is negative", who)
			}
			if c.Enforce && c.RemoveOlderDays == nil {
				add("%s: cleanup: enforce is true and no remove_older_days is set: nothing would ever be removed", who)
			}
			for _, p := range c.Keep {
				if err := auth.CheckPattern(p); err != nil {
					add("%s: cleanup keep: %v", who, err)
				}
			}
		}
		for _, p := range r.OverwritePaths {
			if err := auth.CheckPattern(p); err != nil {
				add("%s: overwrite_paths: %v", who, err)
			}
		}
	}
	for i, g := range s.Grants {
		who := fmt.Sprintf("grant %d (account %q)", i+1, g.Account)
		switch {
		case g.Group != "":
			who = fmt.Sprintf("grant %d (group %q)", i+1, g.Group)
			if g.Account != "" {
				add("%s: a grant is for an account or for a group, not both", who)
			}
			if s.LDAP == nil || s.LDAP.GroupBase == "" {
				add("%s: groups need ldap with a group_base", who)
			}
		default:
			if err := auth.CheckName(g.Account); err != nil {
				add("%s: %v", who, err)
			}
		}
		if err := g.Grant.Check(); err != nil {
			add("%s: %v", who, err)
		}
	}
	s.checkTrust(add)
	s.checkLDAP(add)
	return errors.Join(errs...)
}

func (s *Settings) checkProxy(who string, r *Repo, add func(string, ...any)) {
	if len(r.Members) > 0 || r.WriteTo != "" || len(r.Claims) > 0 || r.Merge {
		add("%s: members, write_to, claims and merge are for a group", who)
	}
	u := r.Upstream
	if u == nil {
		add("%s: a proxy has an upstream: {\"url\": ...}", who)
		return
	}
	p, err := url.Parse(u.URL)
	switch {
	case err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "":
		add("%s: upstream.url is %q: it must be an address like https://registry.example.com", who, u.URL)
	case p.User != nil || p.RawQuery != "" || p.Fragment != "":
		add("%s: upstream.url has a user name, a query or a fragment: credentials go in password_env or token_env", who)
	}
	for _, e := range []struct{ name, v string }{{"password_env", u.PasswordEnv}, {"token_env", u.TokenEnv}} {
		if e.v != "" && !envNameRE.MatchString(e.v) {
			add("%s: upstream.%s names an environment variable (letters, digits, '_')", who, e.name)
		}
	}
	if u.PasswordEnv != "" && u.Username == "" {
		add("%s: upstream.password_env needs upstream.username", who)
	}
	if u.PasswordEnv != "" && u.TokenEnv != "" {
		add("%s: upstream has both password_env and token_env: use one", who)
	}
	if (u.Username != "" || u.TokenEnv != "") && u.PasswordEnv == "" && u.TokenEnv == "" {
		add("%s: upstream.username needs password_env", who)
	}
	for _, h := range append(append([]string{}, u.TokenHosts...), u.FileHosts...) {
		if h == "" || strings.ContainsAny(h, "/: ") {
			add("%s: upstream.token_hosts and file_hosts have %q: a host name with no port or path", who, h)
		}
	}
	if u.MaxPerSecond < 0 || u.MaxConcurrent < 0 {
		add("%s: upstream.max_per_second and max_concurrent are not negative", who)
	}
	if u.Cooldown != nil && r.Format != "npm" && r.Format != "pypi" && r.Format != "go" {
		add("%s: upstream.cooldown is for the formats npm, pypi and go (the others give no time of publication that can be trusted)", who)
	}
	if (u.Cooldown != nil || len(u.Allow) > 0 || len(u.Block) > 0) && r.Format == "oci" {
		add("%s: upstream.cooldown, allow and block are not built for the format oci yet", who)
	}
	for _, l := range []struct {
		name string
		v    []string
	}{{"allow", u.Allow}, {"block", u.Block}} {
		for _, e := range l.v {
			if _, err := policy.ParseRule(e); err != nil {
				add("%s: upstream.%s: %v", who, l.name, err)
			}
		}
	}
	if len(u.Allow) > 0 && u.Cooldown == nil {
		add("%s: upstream.allow lifts the cooldown and there is none: set upstream.cooldown, or remove allow", who)
	}
	if len(u.ImmutablePaths) > 0 && r.Format != "raw" {
		add("%s: upstream.immutable_paths is for the format raw", who)
	}
	for _, pat := range u.ImmutablePaths {
		if err := auth.CheckPattern(pat); err != nil {
			add("%s: upstream.immutable_paths: %v", who, err)
		}
	}
}

func (s *Settings) checkGroup(who string, r *Repo, add func(string, ...any)) {
	if r.Upstream != nil {
		add("%s: upstream is for a proxy", who)
	}
	if r.Merge {
		add("%s: merge is not built: a group stops at the first member that has the name", who)
	}
	if len(r.Members) == 0 {
		add("%s: a group has members", who)
	}
	seen := map[string]bool{}
	for _, m := range r.Members {
		mr := s.Repo(m)
		switch {
		case m == r.Name:
			add("%s: the group is a member of itself", who)
		case mr == nil:
			add("%s: member %q is not a repository of the file", who, m)
		case mr.Format != r.Format:
			add("%s: member %q has the format %s, the group has %s", who, m, mr.Format, r.Format)
		case seen[m]:
			add("%s: member %q is named twice", who, m)
		}
		seen[m] = true
	}
	if r.WriteTo != "" {
		w := s.Repo(r.WriteTo)
		switch {
		case !seen[r.WriteTo]:
			add("%s: write_to %q is not a member", who, r.WriteTo)
		case w != nil && w.Kind != "hosted":
			add("%s: write_to %q is not hosted: only a hosted repository takes a write", who, r.WriteTo)
		}
	}
	for _, c := range r.Claims {
		if c == "" || strings.ContainsAny(c, "*? \t\n") {
			add("%s: claims has %q: a prefix of names, with no wildcard", who, c)
		}
	}
	if s.cycle(r.Name, map[string]bool{}) {
		add("%s: the group holds itself through its members", who)
	}
}

// cycle says whether a group reaches itself through its members.
func (s *Settings) cycle(name string, path map[string]bool) bool {
	r := s.Repo(name)
	if r == nil || r.Kind != "group" {
		return false
	}
	path[name] = true
	defer delete(path, name)
	for _, m := range r.Members {
		if path[m] || s.cycle(m, path) {
			return true
		}
	}
	return false
}

// Leaves are the repositories that answer for a group, in order: its members, a member that is a group replaced by its own leaves, each once.
func (s *Settings) Leaves(name string) []*Repo {
	var out []*Repo
	seen := map[string]bool{}
	var walk func(n string, depth int)
	walk = func(n string, depth int) {
		r := s.Repo(n)
		if r == nil || seen[n] || depth > 16 {
			return
		}
		seen[n] = true
		if r.Kind != "group" {
			out = append(out, r)
			return
		}
		for _, m := range r.Members {
			walk(m, depth+1)
		}
	}
	walk(name, 0)
	return out
}

// GroupsOf are the groups that hold the repository, directly or through other groups.
func (s *Settings) GroupsOf(name string) []string {
	var out []string
	for i := range s.Repositories {
		g := &s.Repositories[i]
		if g.Kind != "group" {
			continue
		}
		for _, l := range s.Leaves(g.Name) {
			if l.Name == name {
				out = append(out, g.Name)
				break
			}
		}
	}
	return out
}

// ClaimPrefixes are the prefixes that the group and the groups inside it claim.
func (s *Settings) ClaimPrefixes(name string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(n string, depth int)
	walk = func(n string, depth int) {
		r := s.Repo(n)
		if r == nil || r.Kind != "group" || seen[n] || depth > 16 {
			return
		}
		seen[n] = true
		out = append(out, r.Claims...)
		for _, m := range r.Members {
			walk(m, depth+1)
		}
	}
	walk(name, 0)
	return out
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
		if ((g.Account != "" && g.Account == p.Account) || (g.Group != "" && p.InGroup(g.Group))) && g.Grant.Allows(repo, path, action) {
			return true
		}
	}
	return false
}
