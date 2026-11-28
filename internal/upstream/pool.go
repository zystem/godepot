package upstream

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/secret"
	"github.com/zystem/godepot/internal/settings"
)

// storedFor is how long a credential that was read from the database is used before it is read again: a new one given with the API is used within this time.
const storedFor = 5 * time.Second

// Secrets are the secrets kept sealed in the database (internal/seal).
type Secrets interface {
	Get(ctx context.Context, name string) (value string, found bool, err error)
}

// SecretName is the name under which the credential of a repository's upstream is kept.
func SecretName(repo string) string { return "upstream/" + repo }

type kept struct {
	value string
	found bool
	at    time.Time
}

// Pool keeps the client of each proxy repository, and makes a new one when the settings of the repository change (the settings are read again on SIGHUP).
type Pool struct {
	DB      meta.DB
	Secrets Secrets // nil: no credential is kept in the database
	M       *Metrics
	Now     func() time.Time // the clock of the clients (default time.Now)
	Flights Flights
	mu      sync.Mutex
	clients map[string]*Client
	stored  map[string]kept
}

func (p *Pool) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// fromDB is the secret of a repository in the database, remembered for storedFor. It is read outside the lock of the clients: a slow database must not stop the other repositories.
func (p *Pool) fromDB(repo string) (string, bool, error) {
	if p.Secrets == nil {
		return "", false, nil
	}
	p.mu.Lock()
	k, ok := p.stored[repo]
	p.mu.Unlock()
	if ok && p.now().Sub(k.at) < storedFor {
		return k.value, k.found, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value, found, err := p.Secrets.Get(ctx, SecretName(repo))
	if err != nil {
		return "", false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stored == nil {
		p.stored = map[string]kept{}
	}
	p.stored[repo] = kept{value, found, p.now()}
	return value, found, nil
}

// credentials finds the password and the token of an upstream. When the settings name a variable (password_env, token_env) it is that variable, or the file of NAME_FILE, read at each use.
// Else it is what was given with the API and is sealed in the database: the password of upstream.username, or with no user name a bearer token; with neither, the upstream is asked with none.
func (p *Pool) credentials(repo string, u settings.Upstream) (pass, token string, err error) {
	if u.PasswordEnv != "" || u.TokenEnv != "" {
		for _, c := range []struct {
			setting, name string
			to            *string
		}{{"password_env", u.PasswordEnv, &pass}, {"token_env", u.TokenEnv, &token}} {
			if c.name == "" {
				continue
			}
			if *c.to, err = secret.Read(os.Getenv, c.name); err != nil {
				return "", "", fmt.Errorf("upstream.%s of %s: %w", c.setting, repo, err)
			}
			if *c.to == "" {
				return "", "", fmt.Errorf("the environment variable %s (upstream.%s of %s) is not set, and %s_FILE names no file with it", c.name, c.setting, repo, c.name)
			}
		}
		return pass, token, nil
	}
	value, found, err := p.fromDB(repo)
	if err != nil {
		return "", "", fmt.Errorf("the credential of %s: %w", repo, err)
	}
	if u.Username == "" {
		return "", value, nil
	}
	if !found {
		return "", "", fmt.Errorf("no password is kept for %s, which has upstream.username: give it with PUT /api/v1/credentials/%s, or name it with password_env", repo, SecretName(repo))
	}
	return value, "", nil
}

// Forgot makes the pool read the credential of a repository again at the next request.
func (p *Pool) Forgot(repo string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.stored, repo)
}

// Client returns the client of the proxy repository.
func (p *Pool) Client(r *settings.Repo) (*Client, error) {
	pass, token, err := p.credentials(r.Name, *r.Upstream)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[r.Name]; c != nil && c.same(*r.Upstream, pass, token) {
		return c, nil
	}
	c, err := New(r.Name, *r.Upstream, p.DB, p.M, pass, token)
	if err != nil {
		return nil, err
	}
	c.Now = p.Now
	if p.clients == nil {
		p.clients = map[string]*Client{}
	}
	if old := p.clients[r.Name]; old != nil {
		old.Close()
	}
	p.clients[r.Name] = c
	return c, nil
}

// Stale counts an answer that was served from the cache because the upstream could not be asked; Changed counts other bytes for what cannot change.
func (p *Pool) Stale(repo string) {
	if p.M != nil && p.M.Stale != nil {
		p.M.Stale.Add(repo, 1)
	}
}

// Changed counts other bytes for what cannot change (P5).
func (p *Pool) Changed(repo string) {
	if p.M != nil && p.M.Changed != nil {
		p.M.Changed.Add(repo, 1)
	}
}
