package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/meta"
)

func TestPushReadDropCollect(t *testing.T) {
	r := newRig(t)
	data := []byte("a layer")
	d := digestOf(data)
	if err := r.push("asset:1", data); err != nil {
		t.Fatal(err)
	}
	if got, err := r.read(d); err != nil || string(got) != "a layer" {
		t.Fatalf("read: %q %v", got, err)
	}
	rc, info, err := r.l.Open(context.Background(), d, 2, 6)
	if err != nil || info.Size != 7 {
		t.Fatalf("open a range: %v %+v", err, info)
	}
	part, _ := io.ReadAll(rc)
	if string(part) != "layer" {
		t.Fatalf("range: %q", part)
	}
	r.sound()
	r.only()

	// The same bytes for a second owner are kept once.
	if err := r.push("asset:2", data); err != nil {
		t.Fatal(err)
	}
	r.only()
	if n := len(r.store.all()); n != 1 {
		t.Fatalf("%d keys for one blob", n)
	}

	// One owner lets go: the other still needs the blob, however long ago that was.
	r.drop("asset:1")
	r.settle()
	if _, err := r.read(d); err != nil {
		t.Fatalf("the blob went while an owner held it: %v", err)
	}

	// The last owner lets go: the blob stays for the grace time and then goes.
	r.drop("asset:2")
	r.pass(30 * time.Minute)
	if _, err := r.l.Collect(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := r.read(d); err != nil {
		t.Fatalf("the blob went before its grace time: %v", err)
	}
	r.settle()
	if _, err := r.read(d); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the blob is still read after its grace time: %v", err)
	}
	r.empty()
}

func TestOtherBytesThanNamedAreNotKept(t *testing.T) {
	r := newRig(t)
	_, err := r.l.Ingest(context.Background(), bytes.NewReader([]byte("these")), digestOf([]byte("those")))
	if !errors.Is(err, blob.ErrDigest) {
		t.Fatalf("got %v", err)
	}
	r.empty()
	if _, err := r.l.Ingest(context.Background(), bytes.NewReader(nil), "not-a-digest"); !errors.Is(err, blob.ErrDigest) {
		t.Fatalf("got %v", err)
	}
	r.empty()
}

func TestHoldOfABlobThatIsNotThereIsRefusedAndChangesNothing(t *testing.T) {
	r := newRig(t)
	if err := r.push("asset:1", []byte("here")); err != nil {
		t.Fatal(err)
	}
	// One list: a blob that is there and one that is not. Nothing of the list may stay.
	list := r.l.Hold("asset:2", digestOf([]byte("here")), digestOf([]byte("nowhere")))
	_, err := r.db.Exec(context.Background(), list...)
	if why, _ := meta.RefusedBy(err); why != blob.WhyBlobNotLive {
		t.Fatalf("got %v", err)
	}
	if n := r.count(`SELECT COUNT(*) FROM refs WHERE owner = 'asset:2'`); n != 0 {
		t.Fatalf("%d references stayed of a list that was refused", n)
	}
	r.sound()
}

// A process that dies at any step of taking bytes in leaves nothing that nobody knows of: a second try works, and after the times run out nothing is left.
func TestCrashAtEveryStepOfIngest(t *testing.T) {
	for _, point := range []string{"ingest:row", "ingest:put", "ingest:claim", "ingest:link", "ingest:live", "dig:deleted", "ingest:unstaged"} {
		for _, again := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/again=%v", point, again), func(t *testing.T) {
				r := newRig(t)
				data := []byte("bytes of " + point)
				if !r.dieAt(point, func() { _ = r.push("asset:1", data) }) {
					t.Fatalf("the push did not reach %s", point)
				}
				r.sound()
				if again {
					// The client tries again at once, while what the dead process left is still there.
					if err := r.push("asset:1", data); err != nil {
						t.Fatal(err)
					}
					r.sound()
					r.settle()
					if got, err := r.read(digestOf(data)); err != nil || !bytes.Equal(got, data) {
						t.Fatalf("after the second try: %v", err)
					}
					r.only()
					r.drop("asset:1")
				}
				r.settle()
				r.empty()
			})
		}
	}
}

// The same for the collector: it dies after the row went and before the bytes went, or after the bytes went and before the grave went.
func TestCrashAtEveryStepOfCollect(t *testing.T) {
	for _, point := range []string{"collect:grave", "dig:deleted"} {
		t.Run(point, func(t *testing.T) {
			r := newRig(t)
			data := []byte("bytes to collect at " + point)
			if err := r.push("asset:1", data); err != nil {
				t.Fatal(err)
			}
			r.drop("asset:1")
			r.pass(2 * time.Hour)
			if !r.dieAt(point, func() { _, _ = r.l.Collect(context.Background(), 100) }) {
				t.Fatalf("the pass did not reach %s", point)
			}
			r.sound()
			// The blob is gone for readers from the moment its row went; a push brings it back as a new life, which the unfinished delete of the old one must not touch.
			if _, err := r.read(digestOf(data)); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("a blob whose row went is read: %v", err)
			}
			if err := r.push("asset:2", data); err != nil {
				t.Fatal(err)
			}
			r.settle()
			if got, err := r.read(digestOf(data)); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("the new life of the blob was harmed by the delete of the old: %v", err)
			}
			r.only()
			r.drop("asset:2")
			r.settle()
			r.empty()
		})
	}
}

// A collector that stalls between removing the row and deleting the bytes (a pause of the process, a slow store) while a push brings the same digest back: its late delete must hit the old key only.
func TestALateDeleteDoesNotRemoveABlobThatCameBack(t *testing.T) {
	r := newRig(t)
	data := []byte("comes back")
	if err := r.push("asset:1", data); err != nil {
		t.Fatal(err)
	}
	r.drop("asset:1")
	r.pass(2 * time.Hour)

	stalled, resume := make(chan struct{}), make(chan struct{})
	slow := *r.l
	slow.CrashAt = func(p string) {
		if p == "collect:grave" {
			close(stalled)
			<-resume
		}
	}
	finished := make(chan error, 1)
	go func() { _, err := slow.Collect(context.Background(), 100); finished <- err }()
	<-stalled
	if err := r.push("asset:2", data); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got, err := r.read(digestOf(data)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the blob that came back was removed by the late delete: %v", err)
	}
	r.sound()
	r.only()
}

// An upload that is slower than uploads may be: the sweep takes it while its bytes still come. The upload ends with ErrExpired and leaves nothing.
func TestSweepOfAnUploadThatIsStillComing(t *testing.T) {
	r := newRig(t)
	pr, pw := io.Pipe()
	result := make(chan error, 1)
	go func() {
		_, err := r.l.Ingest(context.Background(), pr, "")
		result <- err
	}()
	if _, err := pw.Write([]byte("the first half, ")); err != nil {
		t.Fatal(err)
	}
	for r.count(`SELECT COUNT(*) FROM uploads`) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	r.pass(2 * time.Hour)
	if c, err := r.l.Collect(context.Background(), 100); err != nil || c.Uploads != 1 {
		t.Fatalf("sweep: %+v %v", c, err)
	}
	_, _ = pw.Write([]byte("the second half"))
	pw.Close()
	if err := <-result; !errors.Is(err, blob.ErrExpired) {
		t.Fatalf("got %v", err)
	}
	r.sound()
	r.empty()
}

func raceN(def int) int {
	if n, err := strconv.Atoi(os.Getenv("GODEPOT_RACE_N")); err == nil && n > 0 {
		return n
	}
	return def
}

// A push of bytes and the collector that is removing the same bytes, at the same moment, many times: whenever the push is told it succeeded, the bytes are there.
func TestRaceOfPushAndCollect(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < raceN(150); i++ {
		data := []byte("raced " + strconv.Itoa(i))
		d := digestOf(data)
		if err := r.push("asset:old", data); err != nil {
			t.Fatal(err)
		}
		r.drop("asset:old")
		r.pass(2 * time.Hour)

		var wg sync.WaitGroup
		var pushErr, collectErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, collectErr = r.l.Collect(ctx, 100) }()
		go func() { defer wg.Done(); pushErr = r.push("asset:new", data) }()
		wg.Wait()
		if collectErr != nil {
			t.Fatal(collectErr)
		}
		if pushErr != nil {
			t.Fatalf("round %d: the push failed: %v", i, pushErr)
		}
		if got, err := r.read(d); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round %d: the push succeeded and the blob cannot be read: %v", i, err)
		}
		r.drop("asset:new")
	}
	r.sound()
	r.settle()
	r.empty()
}

// A client that saw the blob (a HEAD) and commits what names it without sending bytes, against the collector: the commit is either refused whole, or the bytes are there.
func TestRaceOfHoldAndCollect(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	held, refused := 0, 0
	for i := 0; i < raceN(150); i++ {
		data := []byte("seen " + strconv.Itoa(i))
		d := digestOf(data)
		if err := r.push("asset:old", data); err != nil {
			t.Fatal(err)
		}
		r.drop("asset:old")
		r.pass(2 * time.Hour)

		var wg sync.WaitGroup
		var holdErr, collectErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, collectErr = r.l.Collect(ctx, 100) }()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%8) * time.Millisecond) // the pass takes a few calls: start at different moments of it
			_, holdErr = r.db.Exec(ctx, r.l.Hold("asset:new", d)...)
		}()
		wg.Wait()
		if collectErr != nil {
			t.Fatal(collectErr)
		}
		switch why, isRefusal := meta.RefusedBy(holdErr); {
		case holdErr == nil:
			held++
			if got, err := r.read(d); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("round %d: the blob was held and cannot be read: %v", i, err)
			}
			r.drop("asset:new")
		case isRefusal && why == blob.WhyBlobNotLive:
			refused++
			if n := r.count(`SELECT COUNT(*) FROM refs WHERE owner = 'asset:new'`); n != 0 {
				t.Fatalf("round %d: a refused hold left a reference", i)
			}
		default:
			t.Fatal(holdErr)
		}
	}
	t.Logf("held %d times, refused %d times", held, refused)
	if held == 0 || refused == 0 {
		t.Errorf("the race was always won by one side (held %d, refused %d): the test proves nothing", held, refused)
	}
	r.sound()
	r.settle()
	r.empty()
}

// Many pushes of the same bytes at once: one blob, every push succeeds, the bytes are right.
func TestManyPushesOfTheSameBytesAtOnce(t *testing.T) {
	r := newRig(t)
	data := bytes.Repeat([]byte("same "), 1000)
	for round := 0; round < raceN(150)/15; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 12)
		for i := range errs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = r.push("asset:"+strconv.Itoa(i), data) }()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("push %d: %v", i, err)
			}
		}
		if got, err := r.read(digestOf(data)); err != nil || !bytes.Equal(got, data) {
			t.Fatal(err)
		}
		if _, err := r.l.Collect(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		r.only()
		r.sound()
		for i := range errs {
			r.drop("asset:" + strconv.Itoa(i))
		}
		r.settle()
		r.empty()
	}
}

func TestReconcile(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	kept, lost := []byte("kept"), []byte("lost")
	for i, data := range [][]byte{kept, lost} {
		if err := r.push("asset:"+strconv.Itoa(i), data); err != nil {
			t.Fatal(err)
		}
	}
	// The store loses a blob behind the back of the database, and holds three keys that no row knows.
	rows, err := r.db.Query(ctx, meta.S(`SELECT key FROM blobs WHERE digest = ?`, digestOf(lost)))
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	_ = r.store.Delete(ctx, meta.Str(rows[0][0]))
	_, _ = r.store.Put(ctx, "blobs/sha256/ab/"+digestOf([]byte("x"))+"/0011223344556677", bytes.NewReader([]byte("stray")))
	_, _ = r.store.Put(ctx, "blobs/sha256/cd/"+digestOf([]byte("y"))+"/0011223344556677", bytes.NewReader([]byte("another stray")))
	_, _ = r.store.Put(ctx, "uploads/00000000000000000000000000000000", bytes.NewReader([]byte("stray upload")))

	// Young: a write in progress may be about to name them, so none is removed. The lost blob is found at once.
	rep, err := r.l.Reconcile(ctx, false)
	if err != nil || rep.Strays != 0 || len(rep.Missing) != 1 || rep.Missing[0] != digestOf(lost) || rep.Checked != 2 {
		t.Fatalf("young: %+v %v", rep, err)
	}
	if n := len(r.store.all()); n != 4 {
		t.Fatalf("young keys were removed: %d keys are left of 4", n)
	}

	// Old: a dry pass counts them and changes nothing; the real pass removes them and the blob that is there stays.
	r.pass(2 * time.Hour)
	dry, err := r.l.Reconcile(ctx, true)
	if err != nil || dry.Strays != 3 || len(dry.Missing) != 1 {
		t.Fatalf("dry: %+v %v", dry, err)
	}
	if n := len(r.store.all()); n != 4 {
		t.Fatalf("a dry reconcile changed the store: %d keys", n)
	}
	rep, err = r.l.Reconcile(ctx, false)
	if err != nil || rep.Strays != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := len(r.store.all()); n != 1 {
		t.Fatalf("after reconcile: %v", r.store.all())
	}
	if got, err := r.read(digestOf(kept)); err != nil || string(got) != "kept" {
		t.Fatal(err)
	}
}

func TestSchemaIsAppliedOnceAndANewerOneIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.push("asset:1", []byte("stays")); err != nil {
		t.Fatal(err)
	}
	if err := meta.Migrate(ctx, r.db); err != nil {
		t.Fatalf("a second start: %v", err)
	}
	if _, err := r.read(digestOf([]byte("stays"))); err != nil {
		t.Fatalf("a second start lost the data: %v", err)
	}
	if _, err := r.db.Exec(ctx, meta.S(`INSERT INTO schema_steps (n) VALUES (?)`, meta.Version+1)); err != nil {
		t.Fatal(err)
	}
	if err := meta.Migrate(ctx, r.db); err == nil {
		t.Fatal("a database of a newer program was accepted")
	}
}

// A large object through the whole path: taken in as a stream of unknown length (the digest made on the way), made a blob by a link, read back in ranges, let go and collected. It is a probe,
// skipped unless GODEPOT_BIG_MIB is set, and it logs the times and the memory of the test process; the memory of blobgate is not measured here.
func TestBigObject(t *testing.T) {
	mib, _ := strconv.Atoi(os.Getenv("GODEPOT_BIG_MIB"))
	if mib == 0 {
		t.Skip("GODEPOT_BIG_MIB is not set")
	}
	r := newRig(t)
	ctx := context.Background()
	want := sha256.New()
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	src := io.LimitReader(readerFunc(func(p []byte) (int, error) {
		n := copy(p, chunk)
		want.Write(p[:n])
		return n, nil
	}), int64(mib)<<20)

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0 := time.Now()
	up, err := r.l.Ingest(ctx, src, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ingest of %d MiB: %v", mib, time.Since(t0).Round(time.Millisecond))
	if up.Size != int64(mib)<<20 || up.Digest != hex.EncodeToString(want.Sum(nil)) {
		t.Fatalf("size %d digest %s", up.Size, up.Digest)
	}
	t0 = time.Now()
	if _, err := r.db.Exec(ctx, append(r.l.Hold("asset:big", up.Digest), r.l.Done(up)...)...); err != nil {
		t.Fatal(err)
	}
	t.Logf("commit: %v", time.Since(t0).Round(time.Millisecond))

	// The whole of it, and a range from the middle.
	t0 = time.Now()
	rc, info, err := r.l.Open(ctx, up.Digest, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, rc)
	rc.Close()
	if err != nil || n != info.Size || hex.EncodeToString(h.Sum(nil)) != up.Digest {
		t.Fatalf("read back: %d bytes, %v", n, err)
	}
	t.Logf("read of all: %v", time.Since(t0).Round(time.Millisecond))
	mid := int64(mib) << 19
	rc, _, err = r.l.Open(ctx, up.Digest, mid+3, mid+18)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if string(part) != string(chunk[(mid+3)%int64(len(chunk)):][:16]) {
		t.Fatalf("range: %q", part)
	}
	runtime.ReadMemStats(&m1)
	t.Logf("test process: heap in use at the end %d MiB, total allocated during the run %d MiB", m1.HeapInuse>>20, (m1.TotalAlloc-m0.TotalAlloc)>>20)
	r.drop("asset:big")
	r.settle()
	r.empty()
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A layer with no times set keeps what nothing names for a day, and sweeps nothing that is young.
func TestDefaultTimes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	l := &blob.Layer{DB: r.db, Store: r.store}
	data := []byte("default times")
	up, err := l.Ingest(ctx, bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(ctx, append(l.Hold("asset:1", up.Digest), l.Done(up)...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(ctx, l.Drop("asset:1")...); err != nil {
		t.Fatal(err)
	}
	got, err := l.Collect(ctx, 100)
	if err != nil || got != (blob.Collected{}) {
		t.Fatalf("a blob that was let go a moment ago was collected: %+v %v", got, err)
	}
	if _, err := l.Stat(ctx, up.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Stat(ctx, "not a digest"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// An upload that its owner gives up: nothing is left of it, and a blob that another owner holds is not harmed.
func TestAbandon(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	data := []byte("shared and abandoned")
	if err := r.push("asset:1", data); err != nil {
		t.Fatal(err)
	}
	up, err := r.l.Ingest(ctx, bytes.NewReader(data), digestOf(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.l.Abandon(ctx, up); err != nil {
		t.Fatal(err)
	}
	r.sound()
	r.only()
	if n := r.count(`SELECT COUNT(*) FROM uploads`); n != 0 {
		t.Fatalf("%d uploads left", n)
	}
	r.drop("asset:1")
	r.settle()
	r.empty()
}

// lossy is a database that applies a list and then loses the answer, at the exec number `at` (counting from 0): the caller gets an *Unavailable and cannot tell whether the list was applied.
type lossy struct {
	meta.DB
	at, n int
}

func (l *lossy) Exec(ctx context.Context, stmts ...meta.Stmt) ([]int64, error) {
	i := l.n
	l.n++
	res, err := l.DB.Exec(ctx, stmts...)
	if i == l.at && err == nil {
		return nil, &meta.Unavailable{Msg: "the answer was lost"}
	}
	return res, err
}

// A list of this layer sent again after its answer was lost changes nothing more, whichever list it was: the answer is lost at every Exec of a push in turn; the caller then tries again, and what
// the lost try left is cleaned up by the times running out.
func TestAnswerLostAtEveryList(t *testing.T) {
	probe := newRig(t)
	pl := &lossy{DB: probe.db, at: -1}
	l := *probe.l
	l.DB = pl
	data := []byte("the answer is lost")
	up, err := l.Ingest(context.Background(), bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.DB.Exec(context.Background(), append(l.Hold("asset:probe", up.Digest), l.Done(up)...)...); err != nil {
		t.Fatal(err)
	}
	execs := pl.n
	if execs < 5 {
		t.Fatalf("only %d lists in a push: the test counts wrong", execs)
	}
	for at := 0; at < execs; at++ {
		t.Run(fmt.Sprintf("exec%d", at), func(t *testing.T) {
			r := newRig(t)
			lossyDB := &lossy{DB: r.db, at: at}
			l := *r.l
			l.DB = lossyDB
			ctx := context.Background()
			attempt := func() error {
				up, err := l.Ingest(ctx, bytes.NewReader(data), "")
				if err != nil {
					return err
				}
				_, err = l.DB.Exec(ctx, append(l.Hold("asset:1", up.Digest), l.Done(up)...)...)
				return err
			}
			if err := attempt(); err == nil && lossyDB.n <= at {
				t.Fatalf("the push ended after %d lists and the answer to list %d was never lost", lossyDB.n, at)
			}
			r.sound()
			// The caller tries again, from the beginning, as a client does.
			if err := r.push("asset:1", data); err != nil {
				t.Fatalf("the second try: %v", err)
			}
			if got, err := r.read(digestOf(data)); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("after the second try: %v", err)
			}
			r.sound()
			r.drop("asset:1")
			r.settle()
			r.empty()
		})
	}
}

// The commit of a caller, sent again as it is: the same as once.
func TestCommitSentTwice(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	data := []byte("committed twice")
	up, err := r.l.Ingest(ctx, bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	commit := append(r.l.Hold("asset:1", up.Digest), r.l.Done(up)...)
	for i := 0; i < 3; i++ {
		if _, err := r.db.Exec(ctx, commit...); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	r.sound()
	r.only()
	if n := r.count(`SELECT COUNT(*) FROM refs`); n != 1 {
		t.Fatalf("%d references after three sends", n)
	}
	drop := r.l.Drop("asset:1")
	for i := 0; i < 3; i++ {
		if _, err := r.db.Exec(ctx, drop...); err != nil {
			t.Fatal(err)
		}
	}
	r.sound()
	r.settle()
	r.empty()
}
