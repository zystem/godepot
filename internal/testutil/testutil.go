// Package testutil is what the tests of several packages share: a database to start from nothing, and a store in memory. Only tests import it.
package testutil

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/meta"
)

// DB connects to the rqlite of GODEPOT_RQLITE_URL, drops every table in it and migrates. The test is skipped when the variable is not set. The packages of the tests share one database, so
// they run one at a time (go test -p 1).
func DB(t testing.TB) meta.DB {
	t.Helper()
	url := os.Getenv("GODEPOT_RQLITE_URL")
	if url == "" {
		t.Skip("GODEPOT_RQLITE_URL is not set: this is tested against a real rqlite")
	}
	ctx := context.Background()
	db := meta.NewRqlite(url)
	if err := Reset(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := meta.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

// Reset drops every table of the database.
func Reset(ctx context.Context, db meta.DB) error {
	rows, err := db.Query(ctx, meta.S(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := db.Exec(ctx, meta.S(`DROP TABLE IF EXISTS "`+meta.Str(r[0])+`"`)); err != nil {
			return err
		}
	}
	return nil
}

// MemStore is a blob.Store in memory that links as blobgate does with dedup "tenant".
type MemStore struct {
	mu   sync.Mutex
	keys map[string][]byte
}

// NewMemStore makes an empty one.
func NewMemStore() *MemStore { return &MemStore{keys: map[string][]byte{}} }

func (m *MemStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = data
	return int64(len(data)), nil
}

func (m *MemStore) Link(_ context.Context, key, sum string, size int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.keys {
		if int64(len(d)) == size && blob.SumHex(d) == sum {
			m.keys[key] = d
			return true, nil
		}
	}
	return false, nil
}

func (m *MemStore) Get(_ context.Context, key string, from, to int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.keys[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	if to < 0 || to >= int64(len(d)) {
		to = int64(len(d)) - 1
	}
	return io.NopCloser(strings.NewReader(string(d[from : to+1]))), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
	return nil
}

func (m *MemStore) List(_ context.Context, prefix string, f func(blob.Entry) error) error {
	m.mu.Lock()
	var out []blob.Entry
	for k, d := range m.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blob.Entry{Key: k, Size: int64(len(d)), Modified: time.Now()})
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

// Keys says how many keys it holds.
func (m *MemStore) Keys() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}
