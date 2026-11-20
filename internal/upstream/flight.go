package upstream

import (
	"context"
	"encoding/json"
	"io"
	"sync"
)

func decode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// Flights joins the requests for the same missing thing (docs/spec.md, P2): the first one fetches, the others wait for it to end and then read what it stored. It joins within one replica; two
// replicas that are asked at once each fetch once.
type Flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

// Join says whether the caller is the one that fetches (leader). The leader calls end when it is done, with what happened. A caller that is not the leader calls wait.
func (f *Flights) Join(key string) (leader bool, end func(error), wait func(context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	if fl, ok := f.m[key]; ok {
		return false, nil, func(ctx context.Context) error {
			select {
			case <-fl.done:
				return fl.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	fl := &flight{done: make(chan struct{})}
	f.m[key] = fl
	return true, func(err error) {
		f.mu.Lock()
		delete(f.m, key)
		f.mu.Unlock()
		fl.err = err
		close(fl.done)
	}, nil
}
