// probe is step 0 of the plan (docs/plan.md): questions about blobgate that godepot stands on, asked with the real thing. It is not part of the program and not tested; each subcommand prints what it
// measured, and docs/step0-probes.md records what was run. It needs the stand of tools/test/blobgate-stand.sh started with the PROGRAM of blobgate (BLOBGATE_BIN), not the image, because it
// reads the memory of the process and kills it:
//
//	eval "$(BLOBGATE_BIN=/path/to/blobgate GODEPOT_STAND_DIR=/some/disk/stand tools/test/blobgate-stand.sh start)"
//	GODEPOT_STAND_DIR=/some/disk/stand go run ./tools/probe big -mib 10240
//	go run ./tools/probe slow -readers 50
//	go run ./tools/probe many -n 200000
//	go run ./tools/probe kill
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
	"github.com/zystem/godepot/internal/meta"
)

type rig struct {
	db    *meta.Rqlite
	store *blobgate.Store
	layer *blob.Layer
	dir   string // the directory of the stand
}

func standDir() string {
	if d := os.Getenv("GODEPOT_STAND_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "godepot-bg-stand")
}

func newRig(ctx context.Context) *rig {
	for _, k := range []string{"GODEPOT_RQLITE_URL", "GODEPOT_BLOBGATE_URL", "GODEPOT_BLOBGATE_TENANT", "GODEPOT_BLOBGATE_SECRET"} {
		if os.Getenv(k) == "" {
			fatal("%s is not set: eval the output of tools/test/blobgate-stand.sh start", k)
		}
	}
	r := &rig{db: meta.NewRqlite(os.Getenv("GODEPOT_RQLITE_URL")), dir: standDir()}
	for _, t := range []string{"schema_steps", "guard", "blobs", "refs", "uploads", "graves", "lease"} {
		if _, err := r.db.Exec(ctx, meta.S("DROP TABLE IF EXISTS "+t)); err != nil {
			fatal("%v", err)
		}
	}
	if err := meta.Migrate(ctx, r.db); err != nil {
		fatal("%v", err)
	}
	st := os.Getenv("GODEPOT_BLOBGATE_STORE")
	if st == "" {
		st = "main"
	}
	r.store = blobgate.New(os.Getenv("GODEPOT_BLOBGATE_URL"), os.Getenv("GODEPOT_BLOBGATE_TENANT"), os.Getenv("GODEPOT_BLOBGATE_SECRET"), st)
	if err := r.store.Check(ctx); err != nil {
		fatal("the store: %v", err)
	}
	r.layer = &blob.Layer{DB: r.db, Store: r.store}
	r.wipe(ctx)
	return r
}

// wipe removes every key of the tenant and every row, so that a probe starts clean.
func (r *rig) wipe(ctx context.Context) {
	var keys []string
	if err := r.store.List(ctx, "", func(e blob.Entry) error { keys = append(keys, e.Key); return nil }); err != nil {
		fatal("%v", err)
	}
	for _, k := range keys {
		if err := r.store.Delete(ctx, k); err != nil {
			fatal("%v", err)
		}
	}
	for _, t := range []string{"blobs", "refs", "uploads", "graves"} {
		if _, err := r.db.Exec(ctx, meta.S("DELETE FROM "+t)); err != nil {
			fatal("%v", err)
		}
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "probe: "+f+"\n", a...)
	os.Exit(1)
}

func say(f string, a ...any) { fmt.Printf(time.Now().Format("15:04:05 ")+f+"\n", a...) }

// pid of the program of blobgate, from the stand.
func (r *rig) pid() int {
	b, err := os.ReadFile(filepath.Join(r.dir, "pid"))
	if err != nil {
		fatal("no pid file in %s: the stand must run the program of blobgate (BLOBGATE_BIN), not the image", r.dir)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func procKB(pid int, field string) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, field+":") {
			f := strings.Fields(l)
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return -1
}

// resetPeak starts a new peak of the resident memory of the process (the kernel's clear_refs 5), so that the peak of one phase can be read.
func (r *rig) resetPeak() {
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", r.pid()), []byte("5"), 0o200)
}
func (r *rig) peakMiB() int64 { return procKB(r.pid(), "VmHWM") >> 10 }
func (r *rig) rssMiB() int64  { return procKB(r.pid(), "VmRSS") >> 10 }

func freeGiB(dir string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(float64(len(s)-1)*p)]
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: probe big|slow|many|kill [flags]")
	}
	ctx := context.Background()
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "big":
		mib := fs.Int("mib", 10240, "size of the object in MiB")
		_ = fs.Parse(args)
		probeBig(ctx, newRig(ctx), *mib)
	case "slow":
		readers := fs.Int("readers", 50, "slow readers")
		mib := fs.Int("mib", 64, "size of the object each reads, in MiB")
		kbps := fs.Int("kbps", 1024, "the rate of each slow reader, KiB/s")
		dur := fs.Duration("dur", 30*time.Second, "how long they read")
		_ = fs.Parse(args)
		probeSlow(ctx, newRig(ctx), *readers, *mib, *kbps, *dur)
	case "many":
		n := fs.Int("n", 200000, "objects")
		c := fs.Int("c", 16, "writers at once")
		seq = fs.Bool("seq", false, "keys in order (not the hash of a number): says if the order of keys is the cause of a slowdown")
		uniq = fs.Bool("unique", false, "a body of its own for each object (else all are equal, and blobgate with dedup keeps one set and queues the removal of every copy written)")
		every = fs.Duration("every", 30*time.Second, "how often to say the rate")
		_ = fs.Parse(args)
		probeMany(ctx, newRig(ctx), *n, *c)
	case "kill":
		_ = fs.Parse(args)
		probeKill(ctx, newRig(ctx))
	default:
		fatal("no probe %q", cmd)
	}
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------------------

type prng struct{ c *rand.ChaCha8 }

func newPRNG(seed byte) *prng {
	var s [32]byte
	s[0] = seed
	return &prng{rand.NewChaCha8(s)}
}
func (p *prng) Read(b []byte) (int, error) { return p.c.Read(b) }

func probeBig(ctx context.Context, r *rig, mib int) {
	say("free on the disk of the stand: %.1f GiB; object: %d MiB", freeGiB(r.dir), mib)
	size := int64(mib) << 20
	r.resetPeak()
	t0 := time.Now()
	up, err := r.layer.Ingest(ctx, io.LimitReader(newPRNG(1), size), "")
	if err != nil {
		fatal("ingest: %v", err)
	}
	d := time.Since(t0)
	say("ingest (stream of unknown length, put + link + delete of the staged key): %v = %.0f MiB/s; blobgate peak RSS %d MiB; free %.1f GiB", d.Round(time.Millisecond), float64(mib)/d.Seconds(), r.peakMiB(), freeGiB(r.dir))
	if up.Size != size {
		fatal("size %d", up.Size)
	}
	if _, err := r.db.Exec(ctx, append(r.layer.Hold("asset:big", up.Digest), r.layer.Done(up)...)...); err != nil {
		fatal("commit: %v", err)
	}

	r.resetPeak()
	t0 = time.Now()
	rc, _, err := r.layer.Open(ctx, up.Digest, 0, -1)
	if err != nil {
		fatal("open: %v", err)
	}
	h := sha256.New()
	n, err := io.Copy(h, rc)
	rc.Close()
	d = time.Since(t0)
	if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != up.Digest {
		fatal("read back: %d bytes, %v", n, err)
	}
	say("read of all, digest equal: %v = %.0f MiB/s; blobgate peak RSS %d MiB", d.Round(time.Millisecond), float64(mib)/d.Seconds(), r.peakMiB())

	var lat []time.Duration
	for i := 0; i < 20; i++ {
		off := rand.Int64N(size - 8<<20) // #nosec G404 -- where to read in a measurement, not a secret
		t := time.Now()
		rc, _, err := r.layer.Open(ctx, up.Digest, off, off+8<<20-1)
		if err != nil {
			fatal("range: %v", err)
		}
		m, _ := io.Copy(io.Discard, rc)
		rc.Close()
		if m != 8<<20 {
			fatal("range of %d bytes", m)
		}
		lat = append(lat, time.Since(t))
	}
	say("20 reads of 8 MiB at random offsets: p50 %v, max %v", pct(lat, .5).Round(time.Millisecond), pct(lat, 1).Round(time.Millisecond))

	// A second object with the same bytes: the link must be a record, not a copy.
	free := freeGiB(r.dir)
	t0 = time.Now()
	up2, err := r.layer.Ingest(ctx, io.LimitReader(newPRNG(1), size), "")
	if err != nil {
		fatal("second ingest: %v", err)
	}
	say("the same bytes again: %v (the stream is sent again, the digest is the same: %v); free on disk changed by %.2f GiB", time.Since(t0).Round(time.Millisecond), up2.Digest == up.Digest, free-freeGiB(r.dir))
	_ = r.layer.Abandon(ctx, up2)

	// Let go and collect, with the time moved on.
	if _, err := r.db.Exec(ctx, r.layer.Drop("asset:big")...); err != nil {
		fatal("%v", err)
	}
	l2 := *r.layer
	l2.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	t0 = time.Now()
	for i := 0; i < 3; i++ {
		if _, err := l2.Collect(ctx, 100); err != nil {
			fatal("collect: %v", err)
		}
	}
	say("collected: %v; free on the disk %.1f GiB (it was %.1f at the start of the stand)", time.Since(t0).Round(time.Millisecond), freeGiB(r.dir), free)
	var left int
	_ = r.store.List(ctx, "", func(blob.Entry) error { left++; return nil })
	say("keys left in the store: %d", left)
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------------------

func probeSlow(ctx context.Context, r *rig, readers, mib, kbps int, dur time.Duration) {
	size := int64(mib) << 20
	push := func(owner string, rd io.Reader) string {
		up, err := r.layer.Ingest(ctx, rd, "")
		if err != nil {
			fatal("%v", err)
		}
		if _, err := r.db.Exec(ctx, append(r.layer.Hold(owner, up.Digest), r.layer.Done(up)...)...); err != nil {
			fatal("%v", err)
		}
		return up.Digest
	}
	big := push("asset:big", io.LimitReader(newPRNG(2), size))
	small := push("asset:small", bytes.NewReader([]byte("a small blob that a fast client reads")))

	fast := func() time.Duration {
		t := time.Now()
		rc, _, err := r.layer.Open(ctx, small, 0, -1)
		if err != nil {
			return -1
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
		return time.Since(t)
	}
	var base []time.Duration
	for i := 0; i < 30; i++ {
		base = append(base, fast())
	}
	t0 := time.Now()
	rc, _, _ := r.layer.Open(ctx, big, 0, -1)
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	say("no slow readers: a fast read of a small blob p50 %v max %v; a fast read of the %d MiB object %v", pct(base, .5).Round(time.Microsecond), pct(base, 1).Round(time.Microsecond), mib, time.Since(t0).Round(time.Millisecond))

	r.resetPeak()
	cctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var wg sync.WaitGroup
	var started, bytesRead atomic.Int64
	firstByte := make([]time.Duration, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			rc, _, err := r.layer.Open(cctx, big, 0, -1)
			if err != nil {
				return
			}
			defer rc.Close()
			buf := make([]byte, 32<<10)
			per := time.Duration(float64(time.Second) * float64(len(buf)) / float64(kbps<<10))
			for first := true; ; first = false {
				n, err := rc.Read(buf)
				if first && n > 0 {
					firstByte[i] = time.Since(t)
					started.Add(1)
				}
				bytesRead.Add(int64(n))
				if err != nil {
					return
				}
				select {
				case <-cctx.Done():
					return
				case <-time.After(per):
				}
			}
		}()
	}
	var during, over []time.Duration
	var failed int
	end := time.Now().Add(dur)
	for time.Now().Before(end) {
		d := fast()
		if d < 0 {
			failed++
		} else {
			during = append(during, d)
			if d > time.Second {
				over = append(over, d)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	var fb []time.Duration
	for _, d := range firstByte {
		if d > 0 {
			fb = append(fb, d)
		}
	}
	say("%d slow readers of %d KiB/s for %v: %d got their first byte (time to it p50 %v, max %v); %.0f MiB went through; blobgate peak RSS %d MiB (now %d)",
		readers, kbps, dur, started.Load(), pct(fb, .5).Round(time.Millisecond), pct(fb, 1).Round(time.Millisecond), float64(bytesRead.Load())/(1<<20), r.peakMiB(), r.rssMiB())
	say("a fast read of a small blob meanwhile: %d done, %d failed; p50 %v p95 %v max %v; %d took more than a second", len(during), failed, pct(during, .5).Round(time.Millisecond), pct(during, .95).Round(time.Millisecond), pct(during, 1).Round(time.Millisecond), len(over))
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------------------

var (
	seq   *bool
	uniq  *bool
	every *time.Duration
)

func manyKey(i int) string {
	if seq != nil && *seq {
		return fmt.Sprintf("blobs/sha256/00/%064d/0000000000000000", i)
	}
	s := sha256.Sum256([]byte(strconv.Itoa(i)))
	d := hex.EncodeToString(s[:])
	return "blobs/sha256/" + d[:2] + "/" + d + "/0000000000000000"
}

func probeMany(ctx context.Context, r *rig, n, c int) {
	body := bytes.Repeat([]byte("x"), 100)
	r.resetPeak()
	t0 := time.Now()
	var next, done atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		last, lt := int64(0), time.Now()
		for {
			select {
			case <-stop:
				return
			case <-time.After(*every):
				d := done.Load()
				say("%d of %d written; %.0f/s over the last 30 s; blobgate RSS %d MiB", d, n, float64(d-last)/time.Since(lt).Seconds(), r.rssMiB())
				last, lt = d, time.Now()
			}
		}
	}()
	for range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				b := body
				if uniq != nil && *uniq {
					b = []byte(fmt.Sprintf("%-100d", i))
				}
				if _, err := r.store.Put(ctx, manyKey(i), bytes.NewReader(b)); err != nil {
					fatal("put %d: %v", i, err)
				}
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	d := time.Since(t0)
	say("%d objects of %d bytes written by %d writers: %v = %.0f/s; blobgate peak RSS %d MiB", n, len(body), c, d.Round(time.Second), float64(n)/d.Seconds(), r.peakMiB())

	t0 = time.Now()
	var count int
	if err := r.store.List(ctx, "blobs/sha256/", func(blob.Entry) error { count++; return nil }); err != nil {
		fatal("list: %v", err)
	}
	say("a listing of all: %d keys in %v (%.0f keys/s)", count, time.Since(t0).Round(time.Millisecond), float64(count)/time.Since(t0).Seconds())

	var shard []time.Duration
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("blobs/sha256/%02x/", rand.IntN(256)) // #nosec G404 -- which shard to list in a measurement, not a secret
		t := time.Now()
		k := 0
		_ = r.store.List(ctx, p, func(blob.Entry) error { k++; return nil })
		shard = append(shard, time.Since(t))
		_ = k
	}
	say("20 listings of one shard (%d keys each on average), the way Reconcile does: p50 %v max %v; a full Reconcile would take about %v", n/256, pct(shard, .5).Round(time.Millisecond), pct(shard, 1).Round(time.Millisecond), (pct(shard, .5) * 256).Round(time.Second))

	var gets []time.Duration
	for i := 0; i < 500; i++ {
		t := time.Now()
		rc, err := r.store.Get(ctx, manyKey(rand.IntN(n)), 0, -1) // #nosec G404 -- which object to read in a measurement, not a secret
		if err != nil {
			fatal("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
		gets = append(gets, time.Since(t))
	}
	say("500 reads of random objects while all are there: p50 %v p99 %v", pct(gets, .5).Round(time.Microsecond), pct(gets, .99).Round(time.Microsecond))
	if out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}", "godepot-stand-rq").Output(); err == nil {
		say("the rqlite of blobgate: %s", strings.TrimSpace(string(out)))
	}

	t0 = time.Now()
	next.Store(0)
	for range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if err := r.store.Delete(ctx, manyKey(i)); err != nil {
					fatal("delete %d: %v", i, err)
				}
			}
		}()
	}
	wg.Wait()
	say("deleted all: %v = %.0f/s", time.Since(t0).Round(time.Second), float64(n)/time.Since(t0).Seconds())
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------------------

func standCmd(sub string) {
	out, err := exec.Command(os.Getenv("GODEPOT_STAND_SCRIPT"), sub).CombinedOutput() // #nosec G204 G702 -- the operator's own script, named by the operator, in a tool for a test stand
	if err != nil {
		fatal("stand script %s: %v: %s", sub, err, out)
	}
}

// probeKill kills the program of blobgate (kill -9) at a chosen step of an upload, starts it again, and looks at what is left: the layer, with short times, must bring the database and the store
// back to a state where nothing is held that nobody needs, and a second try must succeed.
func probeKill(ctx context.Context, r *rig) {
	if os.Getenv("GODEPOT_STAND_SCRIPT") == "" {
		os.Setenv("GODEPOT_STAND_SCRIPT", "tools/test/blobgate-stand.sh")
	}
	short := &blob.Layer{DB: r.db, Store: r.store, Grace: 2 * time.Second, UploadTTL: 2 * time.Second}
	data := func(seed byte, mib int) []byte {
		b := make([]byte, mib<<20)
		_, _ = newPRNG(seed).Read(b)
		return b
	}
	for _, sc := range []struct {
		name, point string
		mib         int
	}{
		{"during the write of the bytes", "", 64},
		{"after the write, before the claim", "ingest:put", 8},
		{"after the claim, before the link", "ingest:claim", 8},
		{"after the link, before the row is live", "ingest:link", 8},
		{"after the row is live, before the staged copy is deleted", "ingest:live", 8},
	} {
		say("--- kill -9 of blobgate %s", sc.name)
		r.wipe(ctx)
		b := data(7, sc.mib)
		l := *short
		killed := false
		kill := func() {
			if !killed {
				killed = true
				standCmd("kill-blobgate")
			}
		}
		var body io.Reader = bytes.NewReader(b)
		if sc.point == "" {
			body = &killAfter{r: bytes.NewReader(b), after: int64(len(b)) / 2, kill: kill}
		} else {
			l.CrashAt = func(p string) {
				if p == sc.point {
					kill()
				}
			}
		}
		up, err := l.Ingest(ctx, body, "")
		say("the first try ended: %v (killed: %v)", errStr(err), killed)
		if !killed {
			fatal("blobgate was not killed: the point %q was not reached", sc.point)
		}
		standCmd("start-blobgate")
		say("blobgate is up again")
		if err == nil {
			// The kill came too late to matter (the last point): commit as the caller would.
			_, cerr := r.db.Exec(ctx, append(l.Hold("asset:1", up.Digest), l.Done(up)...)...)
			say("the commit: %v", errStr(cerr))
			_, _ = r.db.Exec(ctx, l.Drop("asset:1")...)
		}
		// A second try, from the beginning, must succeed and read back right.
		up2, err2 := l.Ingest(ctx, bytes.NewReader(b), "")
		if err2 != nil {
			say("FAIL: the second try: %v", err2)
			continue
		}
		rc, _, oerr := l.Open(ctx, up2.Digest, 0, -1)
		ok := false
		if oerr == nil {
			got, _ := io.ReadAll(rc)
			rc.Close()
			ok = bytes.Equal(got, b)
		}
		say("the second try: ok; the blob reads back equal: %v", ok)
		if _, err := r.db.Exec(ctx, append(l.Hold("asset:2", up2.Digest), l.Done(up2)...)...); err != nil {
			say("FAIL: the commit of the second try: %v", err)
			continue
		}
		if _, err := r.db.Exec(ctx, l.Drop("asset:2")...); err != nil {
			say("FAIL: %v", err)
		}
		// Let the times run out and collect, as the background replica does, then reconcile from outside.
		time.Sleep(3 * time.Second)
		for i := 0; i < 3; i++ {
			if _, err := l.Collect(ctx, 100); err != nil {
				say("FAIL: collect: %v", err)
			}
			time.Sleep(3 * time.Second)
		}
		rep, err := l.Reconcile(ctx, false)
		say("reconcile: %+v %v", rep, errStr(err))
		var keys []string
		_ = r.store.List(ctx, "", func(e blob.Entry) error { keys = append(keys, e.Key); return nil })
		var rows int64
		for _, t := range []string{"blobs", "refs", "uploads", "graves"} {
			rs, _ := r.db.Query(ctx, meta.S("SELECT COUNT(*) FROM "+t))
			rows += meta.Int(rs[0][0])
		}
		if len(keys) == 0 && rows == 0 {
			say("RESULT: nothing is left, in the store or in the database")
		} else {
			say("RESULT: LEFT %d keys %v, %d rows", len(keys), keys, rows)
		}
	}
}

func errStr(err error) string {
	if err == nil {
		return "no error"
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// killAfter kills at the byte `after`, and goes on: the write is then in the middle of a block when the program dies.
type killAfter struct {
	r     io.Reader
	after int64
	n     int64
	kill  func()
}

func (k *killAfter) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	k.n += int64(n)
	if k.n >= k.after {
		k.kill()
		time.Sleep(200 * time.Millisecond)
	}
	return n, err
}
