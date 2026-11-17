// Package blobgate is the blob.Store of godepot over blobgate (github.com/zystem/blobgate): the bytes of every blob are objects of one store of one tenant.
//
// The store must be set up with dedup "tenant" (blobgate's docs/deduplication.md): the layer makes the key of a blob from the bytes of an upload with Link, and a store that cannot link makes every
// Ingest end in blob.ErrNotLinked. Check does that test before anything is written.
package blobgate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	bg "github.com/zystem/blobgate/client"

	"github.com/zystem/godepot/internal/blob"
)

// Store implements blob.Store.
type Store struct {
	C     *bg.Client
	Store string // the name of the blobgate store ("_default" is the store marked default)
}

// New makes a Store for one tenant of one blobgate, in one of its stores.
func New(base, tenant, secret, store string) *Store {
	return &Store{C: bg.New(base, tenant, secret), Store: store}
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Put writes the bytes as they come: blobgate cuts them into blocks (the length is not asked for).
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	c := &counter{r: r}
	if err := s.C.Put(ctx, s.Store, key, c, -1, nil); err != nil {
		return 0, err
	}
	return c.n, nil
}

// Link makes key hold bytes that the tenant's store keeps already.
func (s *Store) Link(ctx context.Context, key, sha256hex string, size int64) (bool, error) {
	return s.C.Link(ctx, s.Store, key, sha256hex, size, nil)
}

// Get reads from..to (both included; to < 0: to the end).
func (s *Store) Get(ctx context.Context, key string, from, to int64) (io.ReadCloser, error) {
	rc, _, err := s.C.Get(ctx, s.Store, key, from, to)
	if bg.IsNotFound(err) {
		return nil, blob.ErrNotFound
	}
	return rc, err
}

// Delete removes the key; a key that is not there is no error.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.C.Delete(ctx, s.Store, key)
}

// List calls f for every key under the prefix.
func (s *Store) List(ctx context.Context, prefix string, f func(blob.Entry) error) error {
	return s.C.ListAll(ctx, s.Store, prefix, func(o bg.Listed) error {
		return f(blob.Entry{Key: o.Key, Size: o.Size, Modified: time.Unix(o.Modified, 0)})
	})
}

// Check tries what the layer needs, with a key of its own that it removes: a write of bytes of no known length, a read of a range, a link of the same bytes to a second key, a listing, a delete.
// An error says what to set up in blobgate.
func (s *Store) Check(ctx context.Context) error {
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	// The bytes differ every time: a store that has the bytes already would link without the write being tried.
	data := []byte("godepot check " + hex.EncodeToString(rnd[:]))
	sum := sha256.Sum256(data)
	a, b := "checks/"+hex.EncodeToString(rnd[:])+"/a", "checks/"+hex.EncodeToString(rnd[:])+"/b"
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = s.Delete(c, a)
		_ = s.Delete(c, b)
	}()
	if n, err := s.Put(ctx, a, bytesReader(data)); err != nil || n != int64(len(data)) {
		return fmt.Errorf("writing a test object to the store %q: %v (wrote %d of %d bytes)", s.Store, err, n, len(data))
	}
	ok, err := s.Link(ctx, b, hex.EncodeToString(sum[:]), int64(len(data)))
	if err != nil {
		return fmt.Errorf("linking: %w: set the store %q to dedup \"tenant\" (blobgate docs/deduplication.md)", err, s.Store)
	}
	if !ok {
		return blob.ErrNotLinked
	}
	rc, err := s.Get(ctx, b, 5, 12)
	if err != nil {
		return fmt.Errorf("reading a range of the linked object: %w", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != string(data[5:13]) {
		return fmt.Errorf("a range of the linked object is %q (%v), want %q", got, err, data[5:13])
	}
	found := false
	if err := s.List(ctx, "checks/"+hex.EncodeToString(rnd[:])+"/", func(e blob.Entry) error { found = found || e.Key == b; return nil }); err != nil || !found {
		return fmt.Errorf("the linked object is not in the listing of the store (%v)", err)
	}
	return nil
}
