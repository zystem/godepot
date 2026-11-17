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
	"errors"
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
	// Pause is the first pause before a call is tried again (default 100 ms; it doubles, up to 5 tries again). Calls are tried again when blobgate says it cannot reach its database
	// (502, 503, 504) or the connection fails: blobgate answers 503 when a connection to its database was reset (seen in the probes of 2026-11-17 when that database was reached through docker's port forwarding, and not on the host network: docs/step0-probes.md; a real network can reset a connection too). Every call of the
	// layer can be sent again; a write is tried again only when its source can be read from the start (an io.Seeker), since a stream that has been read is gone.
	Pause time.Duration
}

// New makes a Store for one tenant of one blobgate, in one of its stores.
func New(base, tenant, secret, store string) *Store {
	return &Store{C: bg.New(base, tenant, secret), Store: store}
}

const tries = 6

// transient says whether a failure is worth trying again: blobgate could not reach its database, or the connection to blobgate failed.
func transient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var e *bg.Error
	if errors.As(err, &e) {
		return e.Status == 502 || e.Status == 503 || e.Status == 504
	}
	return true // the call did not get an answer
}

// retry calls f until it succeeds, fails for good, or the tries are spent.
func (s *Store) retry(ctx context.Context, f func() error) error {
	pause := s.Pause
	if pause <= 0 {
		pause = 100 * time.Millisecond
	}
	var err error
	for i := 0; i < tries; i++ {
		if err = f(); !transient(ctx, err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		pause *= 2
	}
	return err
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
	seeker, canSeek := r.(io.Seeker)
	var start int64
	if canSeek {
		var err error
		if start, err = seeker.Seek(0, io.SeekCurrent); err != nil {
			canSeek = false
		}
	}
	var n int64
	try := func() error {
		if canSeek {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				return err
			}
		}
		c := &counter{r: r}
		err := s.C.Put(ctx, s.Store, key, c, -1, nil)
		n = c.n
		return err
	}
	if !canSeek {
		if err := try(); err != nil {
			return 0, err
		}
		return n, nil
	}
	if err := s.retry(ctx, try); err != nil {
		return 0, err
	}
	return n, nil
}

// Link makes key hold bytes that the tenant's store keeps already.
func (s *Store) Link(ctx context.Context, key, sha256hex string, size int64) (bool, error) {
	var ok bool
	err := s.retry(ctx, func() (err error) { ok, err = s.C.Link(ctx, s.Store, key, sha256hex, size, nil); return })
	return ok, err
}

// Get reads from..to (both included; to < 0: to the end).
func (s *Store) Get(ctx context.Context, key string, from, to int64) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := s.retry(ctx, func() (err error) { rc, _, err = s.C.Get(ctx, s.Store, key, from, to); return })
	if bg.IsNotFound(err) {
		return nil, blob.ErrNotFound
	}
	return rc, err
}

// Delete removes the key; a key that is not there is no error.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.retry(ctx, func() error { return s.C.Delete(ctx, s.Store, key) })
}

// List calls f for every key under the prefix.
func (s *Store) List(ctx context.Context, prefix string, f func(blob.Entry) error) error {
	after := ""
	for {
		var page *bg.Listing
		if err := s.retry(ctx, func() (err error) {
			page, err = s.C.List(ctx, s.Store, bg.ListOptions{Prefix: prefix, After: after, Limit: 1000})
			return
		}); err != nil {
			return err
		}
		for _, o := range page.Objects {
			if err := f(blob.Entry{Key: o.Key, Size: o.Size, Modified: time.Unix(o.Modified, 0)}); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
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
