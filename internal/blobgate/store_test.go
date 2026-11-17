package blobgate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
)

func stand(t *testing.T, store string) *blobgate.Store {
	t.Helper()
	u := os.Getenv("GODEPOT_BLOBGATE_URL")
	if u == "" {
		t.Skip("GODEPOT_BLOBGATE_URL is not set (tools/test/blobgate-stand.sh)")
	}
	if store == "" {
		store = os.Getenv("GODEPOT_BLOBGATE_STORE")
	}
	return blobgate.New(u, os.Getenv("GODEPOT_BLOBGATE_TENANT"), os.Getenv("GODEPOT_BLOBGATE_SECRET"), store)
}

func TestTheStoreOfTheLayer(t *testing.T) {
	s, ctx := stand(t, ""), context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	const key = "adapter-test/one"
	defer s.Delete(ctx, key)
	n, err := s.Put(ctx, key, strings.NewReader("0123456789"))
	if err != nil || n != 10 {
		t.Fatalf("put: %d %v", n, err)
	}
	if n, err := s.Put(ctx, key, strings.NewReader("")); err != nil || n != 0 {
		t.Fatalf("an empty write: %d %v", n, err)
	}
	if _, err := s.Put(ctx, key, strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, key, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); string(got) != "345" {
		t.Fatalf("range: %q", got)
	}
	rc.Close()
	var seen []string
	if err := s.List(ctx, "adapter-test/", func(e blob.Entry) error {
		seen = append(seen, e.Key)
		if e.Size != 10 || e.Modified.IsZero() {
			t.Errorf("entry: %+v", e)
		}
		return nil
	}); err != nil || len(seen) != 1 || seen[0] != key {
		t.Fatalf("list: %v %v", seen, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("deleting what is not there: %v", err)
	}
	if _, err := s.Get(ctx, key, 0, -1); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a key that is not there: %v", err)
	}
	// a link of bytes the store has not got is "no", not an error
	if ok, err := s.Link(ctx, "adapter-test/two", strings.Repeat("0", 64), 5); err != nil || ok {
		t.Fatalf("link of unknown bytes: %v %v", ok, err)
	}
	var big bytes.Buffer // more than one block of blobgate (8 MiB), of unknown length
	big.Grow(9 << 20)
	for big.Len() < 9<<20 {
		big.WriteString("0123456789abcdef")
	}
	if n, err := s.Put(ctx, "adapter-test/big", bytes.NewReader(big.Bytes())); err != nil || n != int64(big.Len()) {
		t.Fatalf("big: %d %v", n, err)
	}
	_ = s.Delete(ctx, "adapter-test/big")
}

func TestAStoreThatCannotLinkIsSaidSo(t *testing.T) {
	s := stand(t, "plain")
	err := s.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), `dedup "tenant"`) {
		t.Fatalf("got %v", err)
	}
}

func TestAStoreThatIsNotThere(t *testing.T) {
	if err := stand(t, "no-such-store").Check(context.Background()); err == nil {
		t.Fatal("accepted")
	}
}
