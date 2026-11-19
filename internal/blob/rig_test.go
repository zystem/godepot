package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/testutil"
)

// memStore is a Store in memory that links as blobgate does with dedup "tenant": a key can be made from bytes that another key holds.
type memStore struct {
	mu   sync.Mutex
	keys map[string]memObj
	now  func() time.Time
}

type memObj struct {
	data []byte
	mod  time.Time
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = memObj{data, m.now()}
	return int64(len(data)), nil
}

func (m *memStore) Link(_ context.Context, key, sum string, size int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.keys {
		if int64(len(o.data)) == size && digestOf(o.data) == sum {
			m.keys[key] = memObj{o.data, m.now()}
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) Get(_ context.Context, key string, from, to int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.keys[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	if to < 0 || to >= int64(len(o.data)) {
		to = int64(len(o.data)) - 1
	}
	return io.NopCloser(bytes.NewReader(o.data[from : to+1])), nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
	return nil
}

func (m *memStore) List(_ context.Context, prefix string, f func(blob.Entry) error) error {
	m.mu.Lock()
	var out []blob.Entry
	for k, o := range m.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blob.Entry{Key: k, Size: int64(len(o.data)), Modified: o.mod})
		}
	}
	m.mu.Unlock()
	for _, e := range out {
		if err := f(e); err != nil {
			return err
		}
	}
	return nil
}

func (m *memStore) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// testStore is a blob.Store that can also say every key it holds.
type testStore interface {
	blob.Store
	all() []string
}

// bgStore is the Store over a real blobgate (GODEPOT_BLOBGATE_URL, tools/test/blobgate-stand.sh).
type bgStore struct {
	*blobgate.Store
	t *testing.T
}

func (b bgStore) all() []string {
	var out []string
	if err := b.List(context.Background(), "", func(e blob.Entry) error { out = append(out, e.Key); return nil }); err != nil {
		b.t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// rig is a layer over a real rqlite (GODEPOT_RQLITE_URL) and a store with a clock the test moves. The store is in memory, or a real blobgate when GODEPOT_BLOBGATE_URL is set (with the clock then
// starting at the real time, since the store gives the real time of a write).
type rig struct {
	t     *testing.T
	l     *blob.Layer
	db    meta.DB
	store testStore
	clock atomic.Int64
	mu    sync.Mutex
	point string // the point at which the next call of the layer dies
}

type died struct{ at string }

func newRig(t *testing.T) *rig {
	t.Helper()
	url := os.Getenv("GODEPOT_RQLITE_URL")
	if url == "" {
		t.Skip("GODEPOT_RQLITE_URL is not set: the blob layer is tested against a real rqlite")
	}
	ctx := context.Background()
	db := meta.NewRqlite(url)
	if err := testutil.Reset(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := meta.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, db: db}
	r.clock.Store(1_800_000_000)
	if os.Getenv("GODEPOT_BLOBGATE_URL") != "" {
		r.clock.Store(time.Now().Unix())
	}
	now := func() time.Time { return time.Unix(r.clock.Load(), 0) }
	if u := os.Getenv("GODEPOT_BLOBGATE_URL"); u != "" {
		s := blobgate.New(u, os.Getenv("GODEPOT_BLOBGATE_TENANT"), os.Getenv("GODEPOT_BLOBGATE_SECRET"), os.Getenv("GODEPOT_BLOBGATE_STORE"))
		if err := s.Check(ctx); err != nil {
			t.Fatalf("the blobgate of the test cannot be used: %v", err)
		}
		bs := bgStore{s, t}
		for _, k := range bs.all() { // what an earlier test left
			if err := s.Delete(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		r.store = bs
	} else {
		r.store = &memStore{keys: map[string]memObj{}, now: now}
	}
	r.l = &blob.Layer{DB: db, Store: r.store, Now: now, Grace: time.Hour, UploadTTL: time.Hour}
	r.l.CrashAt = func(p string) {
		r.mu.Lock()
		hit := r.point == p
		r.mu.Unlock()
		if hit {
			panic(died{p})
		}
	}
	return r
}

// dieAt runs f, which is expected to die at the point as a killed process would: nothing after the point runs, the deferred clean-up included. It says whether f died there.
func (r *rig) dieAt(point string, f func()) (dead bool) {
	r.mu.Lock()
	r.point = point
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.point = ""
		r.mu.Unlock()
		if p := recover(); p != nil {
			if _, ok := p.(died); !ok {
				panic(p)
			}
			dead = true
		}
	}()
	f()
	return false
}

func (r *rig) pass(d time.Duration) { r.clock.Add(int64(d / time.Second)) }

// push takes bytes in and commits them for the owner, as a format does.
func (r *rig) push(owner string, data []byte) error {
	ctx := context.Background()
	up, err := r.l.Ingest(ctx, bytes.NewReader(data), digestOf(data))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, append(r.l.Hold(owner, up.Digest), r.l.Done(up)...)...)
	return err
}

func (r *rig) drop(owner string) {
	r.t.Helper()
	if _, err := r.db.Exec(context.Background(), r.l.Drop(owner)...); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(digest string) ([]byte, error) {
	rc, _, err := r.l.Open(context.Background(), digest, 0, -1)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (r *rig) count(sql string, args ...any) int64 {
	r.t.Helper()
	rows, err := r.db.Query(context.Background(), meta.S(sql, args...))
	if err != nil {
		r.t.Fatal(err)
	}
	return meta.Int(rows[0][0])
}

// sound checks what must hold at every moment, whatever died:
// a blob is unnamed-since exactly when no reference names it; a reference names a blob; only an upload names a blob that is not live; a live blob has its bytes.
func (r *rig) sound() {
	r.t.Helper()
	if n := r.count(`SELECT COUNT(*) FROM blobs b WHERE (b.unref_since IS NULL) <> EXISTS (SELECT 1 FROM refs WHERE refs.digest = b.digest)`); n != 0 {
		r.t.Errorf("%d blobs whose unref_since does not say whether a reference names them", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM refs WHERE NOT EXISTS (SELECT 1 FROM blobs WHERE blobs.digest = refs.digest)`); n != 0 {
		r.t.Errorf("%d references that name no blob", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM refs JOIN blobs USING (digest) WHERE blobs.state <> 'live' AND refs.owner NOT LIKE 'upload:%'`); n != 0 {
		r.t.Errorf("%d references of an owner that name a blob that is not live", n)
	}
	rows, err := r.db.Query(context.Background(), meta.S(`SELECT digest FROM blobs WHERE state = 'live'`))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, row := range rows {
		d := meta.Str(row[0])
		if got, err := r.read(d); err != nil || digestOf(got) != d {
			r.t.Errorf("the live blob %s cannot be read whole: %v", d, err)
		}
	}
}

// settle is what the background replica does in the days after: two passes, each after the times have run out, and a reconcile. Then nothing may be left that nobody needs.
func (r *rig) settle() {
	r.t.Helper()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		r.pass(3 * time.Hour)
		if _, err := r.l.Collect(ctx, 1000); err != nil {
			r.t.Fatal(err)
		}
	}
	rep, err := r.l.Reconcile(ctx, false)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(rep.Missing) != 0 {
		r.t.Errorf("reconcile: live blobs without bytes: %v", rep.Missing)
	}
	if _, err := r.l.Collect(ctx, 1000); err != nil {
		r.t.Fatal(err)
	}
	r.sound()
}

// empty checks that the database and the store hold nothing at all.
func (r *rig) empty() {
	r.t.Helper()
	for _, table := range []string{"blobs", "refs", "uploads", "graves", "guard"} {
		if n := r.count(`SELECT COUNT(*) FROM ` + table); n != 0 {
			r.t.Errorf("%d rows left in %s", n, table)
		}
	}
	if keys := r.store.all(); len(keys) != 0 {
		r.t.Errorf("keys left in the store: %v", keys)
	}
}

// only checks that the store holds exactly the keys of the blobs of the database.
func (r *rig) only() {
	r.t.Helper()
	rows, err := r.db.Query(context.Background(), meta.S(`SELECT key FROM blobs ORDER BY key`))
	if err != nil {
		r.t.Fatal(err)
	}
	var want []string
	for _, row := range rows {
		want = append(want, meta.Str(row[0]))
	}
	if got := r.store.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		r.t.Errorf("the store holds\n%v\nand the database knows\n%v", got, want)
	}
}
