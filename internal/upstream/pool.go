package upstream

import (
	"sync"
	"time"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
)

// Pool keeps the client of each proxy repository, and makes a new one when the settings of the repository change (the settings are read again on SIGHUP).
type Pool struct {
	DB      meta.DB
	M       *Metrics
	Now     func() time.Time // the clock of the clients (default time.Now)
	Flights Flights
	mu      sync.Mutex
	clients map[string]*Client
}

// Client returns the client of the proxy repository.
func (p *Pool) Client(r *settings.Repo) (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[r.Name]; c != nil && c.Same(*r.Upstream) {
		return c, nil
	}
	c, err := New(r.Name, *r.Upstream, p.DB, p.M)
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
