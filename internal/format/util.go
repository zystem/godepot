// Helpers that the formats share: ranges, back-off, a gate on downloads.

package format

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRange reads one range of a Range header ("bytes=a-b", "bytes=a-", "bytes=-n") over a body of size bytes. Several ranges, or a header that is not bytes, are answered with the whole body.
// ok is false for a range that lies outside the body.
func ParseRange(h string, size int64) (from, to int64, partial, ok bool) {
	if size == 0 {
		return 0, -1, false, h == "" || !strings.HasPrefix(h, "bytes=")
	}
	whole := func() (int64, int64, bool, bool) { return 0, size - 1, false, true }
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return whole()
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return whole()
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	default:
		f, err := strconv.ParseInt(a, 10, 64)
		if err != nil || f < 0 || f >= size {
			return 0, 0, false, false
		}
		t := size - 1
		if b != "" {
			t, err = strconv.ParseInt(b, 10, 64)
			if err != nil || t < f {
				return 0, 0, false, false
			}
			if t > size-1 {
				t = size - 1
			}
		}
		return f, t, true, true
	}
}

// Pause waits a little, longer after each try and never the same for two writers, so that writers of one path do not keep colliding.
func Pause(ctx context.Context, try int) {
	n := 2 + try
	if n > 20 {
		n = 20
	}
	d := time.Duration(mrand.IntN(n)+1) * time.Millisecond // #nosec G404 -- a back-off, not a secret
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func Token() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no randomness: nothing safe can be done
	}
	return hex.EncodeToString(b)
}

// Gate limits how many things go on at once (the downloads of blobs: docs/spec.md, P11). A nil Gate lets everything through.
type Gate struct {
	slots chan struct{}
	wait  time.Duration
}

// NewGate makes a gate that lets n through at once, and makes the n+1st wait for up to wait.
func NewGate(n int, wait time.Duration) *Gate {
	return &Gate{slots: make(chan struct{}, n), wait: wait}
}

// Acquire takes a place and returns what gives it back; ok is false when no place came free in time.
func (g *Gate) Acquire(ctx context.Context) (release func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	t := time.NewTimer(g.wait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	case <-t.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// InUse is the number of places taken.
func (g *Gate) InUse() int {
	if g == nil {
		return 0
	}
	return len(g.slots)
}

type paced struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	rate int64
}

// PaceGrace is the time a client always has to take a write, on top of the time that the least speed allows (a test makes it short).
var PaceGrace = 10 * time.Second

func (p paced) Write(b []byte) (int, error) {
	t := time.Duration(len(b))*time.Second/time.Duration(p.rate) + PaceGrace
	_ = p.rc.SetWriteDeadline(time.Now().Add(t))
	return p.w.Write(b)
}

// CopyPaced copies r to w. With a rate (bytes a second) above 0 each write must be taken by the client within ten seconds plus the time its size takes at that rate, or the copy ends with the
// error of the write: a client that is slower holds a block of blobgate's memory for nothing (docs/step0-probes.md).
func CopyPaced(w http.ResponseWriter, r io.Reader, rate int64) (int64, error) {
	if rate <= 0 {
		return io.Copy(w, r)
	}
	return io.Copy(paced{w: w, rc: http.NewResponseController(w), rate: rate}, r)
}
