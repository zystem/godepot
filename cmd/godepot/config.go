package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the program reads from its environment. docs/configuration.md says each setting; a test fails when one is missing there.
type Config struct {
	RqliteURL    string
	BlobgateURL  string
	Tenant       string
	Secret       string
	Store        string
	Port         int
	MetricsPort  int
	Grace        time.Duration
	UploadTTL    time.Duration
	CollectEvery time.Duration
	CollectBatch int
	LeaseTTL     time.Duration
	// ConfigFile is the settings file (repositories, grants); "" is none: then no repository is served.
	ConfigFile string
	// TokenMaxLife is the longest life a token may have.
	TokenMaxLife time.Duration
	// MaxDownloads is how many blobs may be sent at once (docs/spec.md, P11); MinRate is the least speed of a client in bytes a second, 0 for none.
	MaxDownloads int
	MinRate      int
	// TrashDays is how long what is removed stays in the trash (docs/spec.md, C3); 0 frees at once. CleanupEvery is how often the policies run.
	TrashDays    int
	CleanupEvery time.Duration
}

// getenv reads NAME, or the contents of the file that NAME_FILE names (a secret kept out of the environment of the process). Both set is an error.
func getenv(env func(string) string, name string) (string, error) {
	v, f := env(name), env(name+"_FILE")
	if v != "" && f != "" {
		return "", fmt.Errorf("%s and %s_FILE are both set: set one", name, name)
	}
	if f != "" {
		b, err := os.ReadFile(f) // #nosec G304 -- the path is the operator's own setting (NAME_FILE): reading the file it names is the point
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return v, nil
}

func number(env func(string) string, name string, def int, min int) (int, error) {
	s := env(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min {
		return 0, fmt.Errorf("%s is %q: it must be a whole number of at least %d", name, s, min)
	}
	return n, nil
}

// loadConfig reads and checks the settings. It returns every problem at once, not the first.
func loadConfig(env func(string) string) (*Config, error) {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	c := &Config{Store: "_default"}
	var err error
	c.RqliteURL = env("GODEPOT_RQLITE_URL")
	c.BlobgateURL = env("GODEPOT_BLOBGATE_URL")
	c.Tenant = env("GODEPOT_BLOBGATE_TENANT")
	if s := env("GODEPOT_BLOBGATE_STORE"); s != "" {
		c.Store = s
	}
	c.Secret, err = getenv(env, "GODEPOT_BLOBGATE_SECRET")
	add(err)
	for _, u := range []struct{ name, v string }{{"GODEPOT_RQLITE_URL", c.RqliteURL}, {"GODEPOT_BLOBGATE_URL", c.BlobgateURL}} {
		if u.v == "" {
			add(fmt.Errorf("%s is required", u.name))
		} else if p, perr := url.Parse(u.v); perr != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			add(fmt.Errorf("%s is %q: it must be an address like http://host:4001", u.name, u.v))
		}
	}
	if c.Tenant == "" {
		add(errors.New("GODEPOT_BLOBGATE_TENANT is required: the name of the tenant of blobgate that godepot writes as"))
	}
	if err == nil && len(c.Secret) < 16 {
		add(errors.New("GODEPOT_BLOBGATE_SECRET (or GODEPOT_BLOBGATE_SECRET_FILE) is required, at least 16 characters: the secret of that tenant"))
	}
	num := func(name string, def, min int) int {
		n, err := number(env, name, def, min)
		add(err)
		return n
	}
	c.Port = num("GODEPOT_PORT", 18100, 1)
	c.MetricsPort = num("GODEPOT_METRICS_PORT", 0, 0)
	if c.MetricsPort != 0 && c.MetricsPort == c.Port {
		add(errors.New("GODEPOT_METRICS_PORT is the same as GODEPOT_PORT: use another, or 0 for none"))
	}
	secs := func(name string, def, min int) time.Duration { return time.Duration(num(name, def, min)) * time.Second }
	c.Grace = secs("GODEPOT_GRACE_SECONDS", 86400, 60)
	c.UploadTTL = secs("GODEPOT_UPLOAD_TTL_SECONDS", 86400, 60)
	c.CollectEvery = secs("GODEPOT_COLLECT_SECONDS", 60, 1)
	c.LeaseTTL = secs("GODEPOT_LEASE_SECONDS", 120, 10)
	c.CollectBatch = num("GODEPOT_COLLECT_BATCH", 200, 1)
	c.ConfigFile = env("GODEPOT_CONFIG_FILE")
	c.TokenMaxLife = time.Duration(num("GODEPOT_TOKEN_MAX_DAYS", 90, 1)) * 24 * time.Hour
	c.MaxDownloads = num("GODEPOT_MAX_DOWNLOADS", 64, 1)
	c.MinRate = num("GODEPOT_MIN_DOWNLOAD_RATE", 8192, 0)
	c.TrashDays = num("GODEPOT_TRASH_DAYS", 7, 0)
	c.CleanupEvery = secs("GODEPOT_CLEANUP_SECONDS", 3600, 60)
	if c.LeaseTTL > 0 && c.CollectEvery > 0 && c.LeaseTTL < 2*c.CollectEvery {
		add(fmt.Errorf("GODEPOT_LEASE_SECONDS (%d) is less than twice GODEPOT_COLLECT_SECONDS (%d): the lease would run out between two renewals", c.LeaseTTL/time.Second, c.CollectEvery/time.Second))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}
