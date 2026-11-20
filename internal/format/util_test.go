package format_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/format"
)

func TestParseRange(t *testing.T) {
	for _, c := range []struct {
		h              string
		size           int64
		from, to       int64
		partial, valid bool
	}{
		{"", 10, 0, 9, false, true},
		{"bytes=2-4", 10, 2, 4, true, true},
		{"bytes=2-", 10, 2, 9, true, true},
		{"bytes=-3", 10, 7, 9, true, true},
		{"bytes=5-99", 10, 5, 9, true, true},
		{"bytes=10-", 10, 0, 0, false, false},
		{"bytes=4-2", 10, 0, 0, false, false},
		{"bytes=0-1,3-4", 10, 0, 9, false, true}, // several ranges: the whole body
		{"items=1-2", 10, 0, 9, false, true},
		{"", 0, 0, -1, false, true},
	} {
		from, to, partial, ok := format.ParseRange(c.h, c.size)
		if ok != c.valid || (ok && (from != c.from || to != c.to || partial != c.partial)) {
			t.Errorf("ParseRange(%q, %d) = %d %d %v %v, want %d %d %v %v", c.h, c.size, from, to, partial, ok, c.from, c.to, c.partial, c.valid)
		}
	}
}

func TestGate(t *testing.T) {
	var none *format.Gate
	if rel, ok := none.Acquire(context.Background()); !ok {
		t.Fatal("a nil gate lets everything through")
	} else {
		rel()
	}
	g := format.NewGate(2, 30*time.Millisecond)
	r1, ok1 := g.Acquire(context.Background())
	r2, ok2 := g.Acquire(context.Background())
	if !ok1 || !ok2 || g.InUse() != 2 {
		t.Fatalf("two places: %v %v %d", ok1, ok2, g.InUse())
	}
	start := time.Now()
	if _, ok := g.Acquire(context.Background()); ok || time.Since(start) < 25*time.Millisecond {
		t.Fatal("the third came through, or did not wait")
	}
	r1()
	if r3, ok := g.Acquire(context.Background()); !ok {
		t.Fatal("a place was given back and is not free")
	} else {
		r3()
	}
	r2()
	if g.InUse() != 0 {
		t.Fatalf("in use: %d", g.InUse())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, _ := g.Acquire(ctx)
	a2, _ := g.Acquire(ctx)
	_ = a2
	if a != nil {
		a()
	}
}

// A client that takes nothing is let go of after the grace time, and the copy ends with an error: the place it held is free.
func TestCopyPacedEndsAStalledClient(t *testing.T) {
	old := format.PaceGrace
	format.PaceGrace = 300 * time.Millisecond
	defer func() { format.PaceGrace = old }()
	done := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := format.CopyPaced(w, endless{}, 1<<20)
		done <- err
	}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The client reads nothing. The kernel takes some megabytes; then the writes block, and the deadline ends them.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the copy of an endless stream ended without an error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a client that takes nothing was not ended")
	}
}

// A client that reads at once is not ended.
func TestCopyPacedKeepsAGoodClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = format.CopyPaced(w, io.LimitReader(endless{}, 16<<20), 1<<20)
	}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != 16<<20 {
		t.Fatalf("read %d, %v", n, err)
	}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }
