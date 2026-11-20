package format

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/zystem/godepot/internal/blob"
)

// FetchTimeout is the longest a fetch from an upstream may take, from the request to the last byte. A fetch is not tied to the client that started it: if that client goes away the fetch goes on, so
// that the others who wait for it, and the cache, get the bytes.
const FetchTimeout = 2 * time.Hour

// Tee sends to a client the bytes that are being read from an upstream (docs/spec.md, P1: the client does not wait for the whole file). It is one byte behind, and sends the last byte only when the
// bytes have been checked: if they are not the ones that were named, the response is one byte short and the connection is cut by the caller, so a client that counts the length of the body sees the
// failure. A client that goes away or is too slow stops being written to; the reading goes on, because the bytes are wanted for the cache.
type Tee struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	rate    int64
	start   func()
	held    byte
	have    bool
	started bool
	dead    bool
}

// NewTee makes a Tee. start writes the status line and the headers; it is called once, just before the first byte goes to the client (or at Finish, for a body of no bytes). rate is the least speed of a
// client in bytes a second (0: none).
func NewTee(w http.ResponseWriter, rate int64, start func()) *Tee {
	return &Tee{w: w, rc: http.NewResponseController(w), rate: rate, start: start}
}

func (t *Tee) emit(b []byte) {
	if t.dead || len(b) == 0 {
		return
	}
	if !t.started {
		t.started = true
		t.start()
	}
	if t.rate > 0 {
		_ = t.rc.SetWriteDeadline(time.Now().Add(time.Duration(len(b))*time.Second/time.Duration(t.rate) + PaceGrace))
	}
	if _, err := t.w.Write(b); err != nil {
		t.dead = true
	}
}

// Write never fails: an error here would stop the reading from the upstream.
func (t *Tee) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if t.have {
		t.emit([]byte{t.held})
	}
	t.emit(p[:n-1])
	t.held, t.have = p[n-1], true
	return n, nil
}

// Started says whether anything has gone to the client: after that the status cannot change, and a failure is told by cutting the connection.
func (t *Tee) Started() bool { return t.started }

// Finish sends the last byte. Call it when the bytes are known to be right.
func (t *Tee) Finish() {
	if t.have {
		t.emit([]byte{t.held})
		t.have = false
	}
	if !t.started && !t.dead {
		t.started = true
		t.start()
	}
}

// Through keeps what is read from body as a blob, and sends it to the client as it comes. want is the digest the upstream named ("" if it named none). The upload that comes back is held for the
// caller to commit, as Ingest says.
func Through(ctx context.Context, l *blob.Layer, body io.Reader, want string, t *Tee) (*blob.Upload, error) {
	up, err := l.Ingest(ctx, io.TeeReader(body, t), want)
	if err == nil {
		t.Finish()
	}
	return up, err
}

// ReadErr remembers the error of a reader that is not the end of it: what tells a body that was cut by the upstream from a failure of the store.
type ReadErr struct {
	R   io.Reader
	Err error
}

func (e *ReadErr) Read(p []byte) (int, error) {
	n, err := e.R.Read(p)
	if err != nil && err != io.EOF {
		e.Err = err
	}
	return n, err
}
