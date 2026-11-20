package format

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

// The tee is one byte behind: until Finish the last byte has not gone, so a body that fails the check is one byte short.
func TestTeeHoldsTheLastByteUntilFinish(t *testing.T) {
	w := httptest.NewRecorder()
	started := 0
	tee := NewTee(w, 0, func() { started++; w.WriteHeader(200) })
	if n, err := tee.Write([]byte("a")); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if tee.Started() || w.Body.Len() != 0 {
		t.Fatal("a single byte went out")
	}
	_, _ = tee.Write([]byte("bcd"))
	_, _ = tee.Write(nil)
	_, _ = tee.Write([]byte("e"))
	if got := w.Body.String(); got != "abcd" || started != 1 || !tee.Started() {
		t.Fatalf("before Finish: %q, started %d", got, started)
	}
	tee.Finish()
	tee.Finish()
	if w.Body.String() != "abcde" || started != 1 {
		t.Fatalf("after Finish: %q", w.Body.String())
	}
}

func TestTeeOfNothing(t *testing.T) {
	w := httptest.NewRecorder()
	tee := NewTee(w, 0, func() { w.WriteHeader(204) })
	tee.Finish()
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatal("an empty body has a status")
	}
}

type brokenWriter struct {
	*httptest.ResponseRecorder
	fail bool
}

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.fail {
		return 0, bytes.ErrTooLarge
	}
	return b.ResponseRecorder.Write(p)
}

// A client that goes away stops being written to, and the reading from the upstream is not stopped by it.
func TestTeeSurvivesAGoneClient(t *testing.T) {
	w := &brokenWriter{ResponseRecorder: httptest.NewRecorder(), fail: true}
	tee := NewTee(w, 100, func() {})
	for i := 0; i < 5; i++ {
		if _, err := tee.Write([]byte("xyz")); err != nil {
			t.Fatal("a failed write stopped the reading")
		}
	}
	tee.Finish()
}
