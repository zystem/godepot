package lease_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/lease"
	"github.com/zystem/godepot/internal/meta"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time       { return time.Unix(c.t.Load(), 0) }
func (c *clock) pass(d time.Duration) { c.t.Add(int64(d / time.Second)) }

func setup(t *testing.T) (meta.DB, *clock) {
	t.Helper()
	url := os.Getenv("GODEPOT_RQLITE_URL")
	if url == "" {
		t.Skip("GODEPOT_RQLITE_URL is not set")
	}
	db := meta.NewRqlite(url)
	ctx := context.Background()
	for _, table := range []string{"schema_steps", "guard", "blobs", "refs", "uploads", "graves", "lease"} {
		if _, err := db.Exec(ctx, meta.S("DROP TABLE IF EXISTS "+table)); err != nil {
			t.Fatal(err)
		}
	}
	if err := meta.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	c.t.Store(1_800_000_000)
	return db, c
}

func (c *clock) lease(db meta.DB, holder string) *lease.Lease {
	return &lease.Lease{DB: db, Name: "background", Holder: holder, TTL: 2 * time.Minute, Now: c.now}
}

func must(t *testing.T, l *lease.Lease, want bool, why string) {
	t.Helper()
	got, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: %s asked and got %v, want %v", why, l.Holder, got, want)
	}
}

func TestOneHoldsAndTheOtherWaits(t *testing.T) {
	db, c := setup(t)
	a, b := c.lease(db, "a"), c.lease(db, "b")
	must(t, a, true, "the first to ask")
	must(t, b, false, "the lease is held")
	must(t, a, true, "a renews")

	c.pass(90 * time.Second)
	must(t, a, true, "a renews before the time runs out")
	c.pass(90 * time.Second) // 180 s from the start, 90 s after the renewal: still a's
	must(t, b, false, "a renewed 90 s ago and the lease is 120 s")

	c.pass(31 * time.Second) // a has not renewed for 121 s: it is dead
	must(t, b, true, "the time ran out")
	must(t, a, false, "a comes back and finds b holding it")
	must(t, b, true, "b renews")
}

func TestReleaseLetsTheNextOneIn(t *testing.T) {
	db, c := setup(t)
	a, b := c.lease(db, "a"), c.lease(db, "b")
	must(t, a, true, "a")
	if err := b.Release(context.Background()); err != nil { // b does not hold it: nothing changes
		t.Fatal(err)
	}
	must(t, b, false, "b released what it did not hold")
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	must(t, b, true, "a let go")
}

// Many replicas ask at the same moment, many times: never two hold it, and one does.
func TestOnlyOneWinsARace(t *testing.T) {
	db, c := setup(t)
	for round := 0; round < 40; round++ {
		c.pass(5 * time.Minute) // the last round's holder is dead
		var wg sync.WaitGroup
		var won atomic.Int32
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := c.lease(db, "r"+strconv.Itoa(i)).Acquire(context.Background())
				if err != nil {
					t.Error(err)
				}
				if ok {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("round %d: %d replicas hold the lease", round, won.Load())
		}
	}
}
