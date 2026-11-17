// Package metrics is a small Prometheus text exposition: counters (with one label at most) and gauges, and nothing else. It keeps the program free of a large dependency for what a handful of numbers need.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds the metrics of the program.
type Registry struct {
	mu    sync.Mutex
	order []*family
	names map[string]bool
}

type family struct {
	name, help, typ, label string
	mu                     sync.Mutex
	vals                   map[string]*atomic.Int64
}

// New makes an empty registry.
func New() *Registry { return &Registry{names: map[string]bool{}} }

func (r *Registry) add(name, help, typ, label string) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic("metric registered twice: " + name)
	}
	r.names[name] = true
	f := &family{name: name, help: help, typ: typ, label: label, vals: map[string]*atomic.Int64{}}
	r.order = append(r.order, f)
	return f
}

func (f *family) get(v string) *atomic.Int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.vals[v]
	if a == nil {
		a = new(atomic.Int64)
		f.vals[v] = a
	}
	return a
}

// Counter is a number that only goes up. label is the name of its one label ("" for none).
type Counter struct{ f *family }

// Counter registers a counter.
func (r *Registry) Counter(name, help, label string) *Counter {
	return &Counter{r.add(name, help, "counter", label)}
}

// Add adds n to the counter of the label value (use "" when the counter has no label).
func (c *Counter) Add(labelValue string, n int64) { c.f.get(labelValue).Add(n) }

// Value reads it.
func (c *Counter) Value(labelValue string) int64 { return c.f.get(labelValue).Load() }

// Gauge is a number that goes both ways.
type Gauge struct{ f *family }

// Gauge registers a gauge.
func (r *Registry) Gauge(name, help string) *Gauge { return &Gauge{r.add(name, help, "gauge", "")} }

// Set sets it.
func (g *Gauge) Set(n int64) { g.f.get("").Store(n) }

// Value reads it.
func (g *Gauge) Value() int64 { return g.f.get("").Load() }

// Render writes every metric in the text format of Prometheus, in the order they were registered.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	fams := append([]*family(nil), r.order...)
	r.mu.Unlock()
	for _, f := range fams {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		f.mu.Lock()
		keys := make([]string, 0, len(f.vals))
		for k := range f.vals {
			keys = append(keys, k)
		}
		f.mu.Unlock()
		sort.Strings(keys)
		if len(keys) == 0 && f.label == "" {
			keys = []string{""}
		}
		for _, k := range keys {
			if f.label == "" {
				fmt.Fprintf(w, "%s %d\n", f.name, f.get(k).Load())
			} else {
				fmt.Fprintf(w, "%s{%s=%q} %d\n", f.name, f.label, strings.ReplaceAll(k, `"`, ""), f.get(k).Load())
			}
		}
	}
}
