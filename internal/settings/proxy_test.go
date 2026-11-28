package settings_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/settings"
)

const withProxy = `{"repositories": [
 {"name": "mine", "format": "oci", "kind": "hosted"},
 {"name": "hub", "format": "oci", "kind": "proxy", "upstream": {"url": "https://registry-1.docker.io", "username": "bot", "password_env": "HUB_PASSWORD", "token_hosts": ["auth.docker.io"], "ttl": "10m", "negative_ttl": "1m", "max_per_second": 2}},
 {"name": "files", "format": "raw", "kind": "proxy", "upstream": {"url": "https://example.com/raw", "immutable_paths": ["rel/**"]}},
 {"name": "inner", "format": "oci", "kind": "group", "members": ["hub"], "claims": ["lib/"]},
 {"name": "all", "format": "oci", "kind": "group", "members": ["hub", "mine", "inner"], "write_to": "mine", "claims": ["corp/"]}
], "grants": []}`

func TestProxyAndGroupSettings(t *testing.T) {
	s, err := settings.Parse(strings.NewReader(withProxy))
	if err != nil {
		t.Fatal(err)
	}
	hub := s.Repo("hub").Upstream
	if hub.TagTTL() != 10*time.Minute || hub.MissTTL() != time.Minute || hub.FileTTL() != 10*time.Minute {
		t.Fatalf("times: %v %v %v", hub.TagTTL(), hub.MissTTL(), hub.FileTTL())
	}
	// The defaults: an OCI tag 5 minutes, a not-found 10 minutes, a raw file for ever.
	files := s.Repo("files").Upstream
	if files.FileTTL() != 0 || files.TagTTL() != 5*time.Minute || files.MissTTL() != 10*time.Minute || !files.Immutable("rel/1/x") || files.Immutable("other") {
		t.Fatalf("defaults: %v %v %v", files.FileTTL(), files.TagTTL(), files.MissTTL())
	}
	// The members that answer, in order, a group in a group opened, each once.
	var names []string
	for _, l := range s.Leaves("all") {
		names = append(names, l.Name)
	}
	if strings.Join(names, ",") != "hub,mine" {
		t.Fatalf("leaves: %v", names)
	}
	if g := strings.Join(s.GroupsOf("hub"), ","); g != "inner,all" {
		t.Fatalf("groups of hub: %s", g)
	}
	if g := strings.Join(s.GroupsOf("mine"), ","); g != "all" {
		t.Fatalf("groups of mine: %s", g)
	}
	if c := strings.Join(s.ClaimPrefixes("all"), ","); c != "corp/,lib/" {
		t.Fatalf("claims: %s", c)
	}
}

func TestProxyAndGroupRefuse(t *testing.T) {
	up := `"upstream": {"url": "https://x.example"}`
	for name, text := range map[string]string{
		"a proxy with no upstream":      `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy"}]}`,
		"an upstream that is no url":    `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "registry"}}]}`,
		"an upstream with a password":   `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://u:p@x.example"}}]}`,
		"a password with no user":       `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "password_env": "P"}}]}`,
		"both a password and a token":   `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "username": "u", "password_env": "P", "token_env": "T"}}]}`,
		"an env name that is a secret":  `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "token_env": "has space"}}]}`,
		"a token host with a path":      `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "token_hosts": ["a.com/x"]}}]}`,
		"a time that is not a time":     `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "ttl": "soon"}}]}`,
		"a time that is a number":       `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", "upstream": {"url": "https://x.example", "ttl": 5}}]}`,
		"immutable paths of oci":        `{"repositories": [{"name": "a", "format": "oci", "kind": "proxy", "upstream": {"url": "https://x.example", "immutable_paths": ["a"]}}]}`,
		"a quota on a proxy":            `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", ` + up + `, "quota_bytes": 5}]}`,
		"an upstream on a hosted":       `{"repositories": [{"name": "a", "format": "raw", "kind": "hosted", ` + up + `}]}`,
		"members on a proxy":            `{"repositories": [{"name": "a", "format": "raw", "kind": "proxy", ` + up + `, "members": ["a"]}]}`,
		"a group with no members":       `{"repositories": [{"name": "g", "format": "raw", "kind": "group"}]}`,
		"a member that is not there":    `{"repositories": [{"name": "g", "format": "raw", "kind": "group", "members": ["nope"]}]}`,
		"a member of another format":    `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "oci", "kind": "group", "members": ["h"]}]}`,
		"a member twice":                `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "raw", "kind": "group", "members": ["h", "h"]}]}`,
		"a group of itself":             `{"repositories": [{"name": "g", "format": "raw", "kind": "group", "members": ["g"]}]}`,
		"a cycle":                       `{"repositories": [{"name": "a", "format": "raw", "kind": "group", "members": ["b"]}, {"name": "b", "format": "raw", "kind": "group", "members": ["a"]}]}`,
		"write_to that is no member":    `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "o", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "raw", "kind": "group", "members": ["h"], "write_to": "o"}]}`,
		"write_to that is a proxy":      `{"repositories": [{"name": "p", "format": "raw", "kind": "proxy", ` + up + `}, {"name": "g", "format": "raw", "kind": "group", "members": ["p"], "write_to": "p"}]}`,
		"a claim with a wildcard":       `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "raw", "kind": "group", "members": ["h"], "claims": ["a*"]}]}`,
		"merge, which is not built":     `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "raw", "kind": "group", "members": ["h"], "merge": true}]}`,
		"a group with an upstream":      `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted"}, {"name": "g", "format": "raw", "kind": "group", "members": ["h"], ` + up + `}]}`,
		"claims on a hosted repository": `{"repositories": [{"name": "h", "format": "raw", "kind": "hosted", "claims": ["a"]}]}`,
	} {
		if _, err := settings.Parse(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
