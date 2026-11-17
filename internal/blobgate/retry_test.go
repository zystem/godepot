package blobgate_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
)

// fake is a blobgate that answers 503 `fails` times and then does what the call asks.
func fake(t *testing.T, fails int32, status int) (*blobgate.Store, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= fails {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"database_unavailable","detail":"rqlite: connection reset by peer"}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodPut || r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `{"size":3}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/objects/"):
			_, _ = io.WriteString(w, "abc")
		default:
			_, _ = io.WriteString(w, `{"objects":[{"key":"k","size":3,"modified":1}],"next":""}`)
		}
	}))
	t.Cleanup(srv.Close)
	s := blobgate.New(srv.URL, "t", "a-secret-of-16-chars", "main")
	s.Pause = time.Millisecond
	return s, &calls
}

func TestAnswerOf503IsTriedAgain(t *testing.T) {
	ctx := context.Background()
	s, calls := fake(t, 3, 503)
	if n, err := s.Put(ctx, "k", bytes.NewReader([]byte("abc"))); err != nil || n != 3 || calls.Load() != 4 {
		t.Fatalf("put: %d %v after %d calls", n, err, calls.Load())
	}
	s, calls = fake(t, 2, 503)
	if err := s.Delete(ctx, "k"); err != nil || calls.Load() != 3 {
		t.Fatalf("delete: %v after %d calls", err, calls.Load())
	}
	s, calls = fake(t, 2, 502)
	rc, err := s.Get(ctx, "k", 0, -1)
	if err != nil || calls.Load() != 3 {
		t.Fatalf("get: %v after %d calls", err, calls.Load())
	}
	if b, _ := io.ReadAll(rc); string(b) != "abc" {
		t.Fatalf("%q", b)
	}
	s, calls = fake(t, 1, 504)
	var keys []string
	if err := s.List(ctx, "", func(e blob.Entry) error { keys = append(keys, e.Key); return nil }); err != nil || len(keys) != 1 || calls.Load() != 2 {
		t.Fatalf("list: %v %v after %d calls", keys, err, calls.Load())
	}
}

func TestAStreamIsNotTriedAgain(t *testing.T) {
	// The bytes of a stream that has been read are gone: one try, and the error is the caller's to handle (the client of godepot sends the upload again).
	s, calls := fake(t, 1, 503)
	if _, err := s.Put(context.Background(), "k", io.MultiReader(strings.NewReader("abc"))); err == nil || calls.Load() != 1 {
		t.Fatalf("a stream was sent again: %v after %d calls", err, calls.Load())
	}
}

func TestAnswersThatAreFinalAreNotTriedAgain(t *testing.T) {
	for _, status := range []int{400, 404, 409, 507} {
		s, calls := fake(t, 5, status)
		if err := s.Delete(context.Background(), "k"); err == nil || calls.Load() != 1 {
			t.Fatalf("status %d: %v after %d calls", status, err, calls.Load())
		}
	}
}

func TestTriesEndAndTheContextStopsThem(t *testing.T) {
	s, calls := fake(t, 100, 503)
	if err := s.Delete(context.Background(), "k"); err == nil || calls.Load() != 6 {
		t.Fatalf("%v after %d calls", err, calls.Load())
	}
	s, calls = fake(t, 100, 503)
	s.Pause = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if err := s.Delete(ctx, "k"); err == nil || time.Since(t0) > 5*time.Second || calls.Load() != 1 {
		t.Fatalf("%v after %v and %d calls", err, time.Since(t0), calls.Load())
	}
}

func TestConnectionRefusedIsTriedAgainAndThenReported(t *testing.T) {
	s := blobgate.New("http://127.0.0.1:1", "t", "a-secret-of-16-chars", "main")
	s.Pause = time.Millisecond
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Fatal("no error")
	}
}
