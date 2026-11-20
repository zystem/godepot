// ocibig is a probe of step 4 of the plan (docs/plan.md): a big layer pushed and pulled through the OCI door of a running godepot, with the memory of the program read from /proc, and clients that read
// slowly or not at all. It is not part of the program and not tested; docs/oci.md records what was run.
//
//	go run ./tools/ocibig -url http://127.0.0.1:18100 -token $T -name reg/big -pid $(pgrep -f 'godepot serve') -mib 10240
//	go run ./tools/ocibig -url ... -name reg/big -mib 2048 -chunk-mib 64      # the same, in parts
//	go run ./tools/ocibig -url ... -name reg/big -slow 20 -hold 30s           # 20 clients that do not read, against the limit of downloads
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	url     = flag.String("url", "http://127.0.0.1:18100", "the address of godepot")
	token   = flag.String("token", os.Getenv("TOKEN"), "a token with write and read")
	name    = flag.String("name", "reg/big", "repository/image")
	mib     = flag.Int64("mib", 1024, "the size of the layer, in MiB")
	chunk   = flag.Int64("chunk-mib", 0, "send the layer in parts of this many MiB (0: one PATCH)")
	pid     = flag.Int("pid", 0, "the process id of godepot, for its memory")
	slow    = flag.Int("slow", 0, "instead of the layer: this many clients that open a download of an existing layer (see -digest) and do not read it")
	digest  = flag.String("digest", "", "with -slow: the digest of the layer to download (sha256:...)")
	hold    = flag.Duration("hold", 30*time.Second, "with -slow: how long to watch")
	skipGet = flag.Bool("no-pull", false, "push only")
)

// gen is a stream of pseudo-random bytes of a fixed length that is hashed as it is read.
type gen struct {
	rng  *rand.ChaCha8
	left int64
}

func newGen(size int64, seed byte) (*gen, *sumW) {
	var key [32]byte
	key[0] = seed
	s := &sumW{h: sha256.New()}
	return &gen{rng: rand.NewChaCha8(key), left: size}, s
}

type sumW struct {
	h hash.Hash
	n int64
}

func (s *sumW) hex() string { return "sha256:" + hex.EncodeToString(s.h.Sum(nil)) }

// reader couples a generator and its hash.
type reader struct {
	g *gen
	s *sumW
}

func (r *reader) Read(p []byte) (int, error) {
	if r.g.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.g.left {
		p = p[:r.g.left]
	}
	n, _ := r.g.rng.Read(p)
	r.g.left -= int64(n)
	r.s.h.Write(p[:n])
	r.s.n += int64(n)
	return n, nil
}

func hwm(pid int) string {
	if pid == 0 {
		return "n/a"
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return "n/a (" + err.Error() + ")"
	}
	var hwm, rss string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			hwm = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			rss = strings.TrimSpace(v)
		}
	}
	return "peak " + hwm + ", now " + rss
}

func do(method, u string, body io.Reader, hdr ...string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return http.DefaultClient.Do(req)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func main() {
	flag.Parse()
	if *slow > 0 {
		slowClients()
		return
	}
	size := *mib << 20
	fmt.Printf("memory of godepot before: %s\n", hwm(*pid))
	start := time.Now()
	r := &reader{}
	r.g, r.s = newGen(size, 7)

	resp, err := do("POST", *url+"/v2/"+*name+"/blobs/uploads/", nil)
	if err != nil || resp.StatusCode != 202 {
		die("start: %v %v", err, resp)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if *chunk == 0 {
		resp, err = do("PATCH", *url+loc, r, "Content-Type", "application/octet-stream")
		if err != nil || resp.StatusCode != 202 {
			die("patch: %v %v", err, resp)
		}
		resp.Body.Close()
	} else {
		var off int64
		for off < size {
			n := *chunk << 20
			if off+n > size {
				n = size - off
			}
			resp, err = do("PATCH", *url+loc, io.LimitReader(r, n), "Content-Type", "application/octet-stream", "Content-Length", strconv.FormatInt(n, 10), "Content-Range", fmt.Sprintf("%d-%d", off, off+n-1))
			if err != nil || resp.StatusCode != 202 {
				die("patch at %d: %v %v", off, err, resp)
			}
			resp.Body.Close()
			off += n
		}
	}
	sent := time.Since(start)
	d := r.s.hex()
	t2 := time.Now()
	resp, err = do("PUT", *url+loc+"?digest="+d, nil)
	if err != nil || resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		die("put: %v %v %s", err, resp, b)
	}
	resp.Body.Close()
	fmt.Printf("pushed %d MiB (%s) in parts of %d MiB: bytes sent in %s, the end of the upload in %s; %s\n", *mib, d, *chunk, sent.Round(time.Millisecond), time.Since(t2).Round(time.Millisecond), hwm(*pid))
	if *skipGet {
		return
	}

	t3 := time.Now()
	resp, err = do("GET", *url+"/v2/"+*name+"/blobs/"+d, nil)
	if err != nil || resp.StatusCode != 200 {
		die("get: %v %v", err, resp)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	resp.Body.Close()
	if err != nil {
		die("read: %v", err)
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	fmt.Printf("pulled %d MiB in %s, digest equal: %v; %s\n", n>>20, time.Since(t3).Round(time.Millisecond), got == d, hwm(*pid))
	if got != d {
		os.Exit(1)
	}
	// A range from the middle.
	from := size / 2
	resp, err = do("GET", *url+"/v2/"+*name+"/blobs/"+d, nil, "Range", fmt.Sprintf("bytes=%d-%d", from, from+1023))
	if err != nil || resp.StatusCode != 206 {
		die("range: %v %v", err, resp)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The same bytes, from the generator.
	g2, _ := newGen(size, 7)
	skip := make([]byte, 1<<20)
	for left := from; left > 0; {
		k := int64(len(skip))
		if k > left {
			k = left
		}
		g2.rng.Read(skip[:k])
		left -= k
	}
	want := make([]byte, 1024)
	g2.rng.Read(want)
	fmt.Printf("range of 1 KiB from the middle equal: %v\n", bytes.Equal(b, want))
	fmt.Printf("digest: %s\n", d)
}

// slowClients opens downloads and does not read them, and says how godepot answers the ones over its limit and what it does with the ones that do not read.
func slowClients() {
	if *digest == "" {
		die("-digest is needed")
	}
	var open, busy, ended atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *slow; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := do("GET", *url+"/v2/"+*name+"/blobs/"+*digest, nil)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == 503 {
				busy.Add(1)
				return
			}
			open.Add(1)
			// Read a little, then nothing: a client that stalls.
			buf := make([]byte, 4096)
			_, _ = io.ReadFull(resp.Body, buf)
			time.Sleep(*hold)
			if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
				ended.Add(1) // godepot closed the connection
			}
		}()
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-tick.C:
			fmt.Printf("%4.0fs  downloads open %d, answered 503 %d, connections ended by godepot %d; %s\n", time.Since(start).Seconds(), open.Load(), busy.Load(), ended.Load(), hwm(*pid))
		case <-done:
			fmt.Printf("done: open %d, 503 %d, ended by godepot %d; %s\n", open.Load(), busy.Load(), ended.Load(), hwm(*pid))
			return
		}
	}
}
