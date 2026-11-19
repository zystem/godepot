// Package blob keeps bytes by their SHA-256, once, and knows who needs them. It is the only package that writes to the store (blobgate) and the only one that writes the tables blobs, refs, uploads
// and graves. docs/blob-layer.md says why each step is where it is, and what a process that dies at each step leaves.
//
// The rules, short:
//
//   - A blob is a row of blobs and an object of the store under the row's key. The row is the commit.
//   - A reference (a row of refs) says that an owner needs a blob. unref_since of a blob is NULL exactly while a reference names it.
//   - A reference is added only to a live blob, in one transaction with the check (Hold). So a reference never names bytes that are not there.
//   - A blob is removed only when no reference has named it for the grace time, in one transaction with the check (Collect), which also writes down the key to delete (a grave).
//   - The key of a blob is new for every life of its digest, so a delete that comes late cannot remove a blob that has come back.
package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// Store is what the layer asks of the place the bytes are kept. internal/blobgate implements it over blobgate; the tests have one in memory.
type Store interface {
	// Put writes the bytes under the key, replacing what was there, and returns how many it wrote.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Link makes the key hold bytes that the store already keeps under another key, named by their SHA-256 (hex) and size, without sending them. false: the store keeps no such bytes.
	Link(ctx context.Context, key, sha256hex string, size int64) (bool, error)
	// Get reads the bytes from..to (to < 0: to the end). A key that is not there is ErrNotFound.
	Get(ctx context.Context, key string, from, to int64) (io.ReadCloser, error)
	// Delete removes the key. A key that is not there is no error.
	Delete(ctx context.Context, key string) error
	// List calls f for every key that begins with the prefix, in any order.
	List(ctx context.Context, prefix string, f func(Entry) error) error
}

// Entry is one key of a listing.
type Entry struct {
	Key      string
	Size     int64
	Modified time.Time
}

var (
	// ErrNotFound: no live blob has this digest.
	ErrNotFound = errors.New("no such blob")
	// ErrDigest: the bytes that came are not the bytes that were named.
	ErrDigest = errors.New("the bytes do not have the digest that was named")
	// ErrExpired: the upload took longer than the time an upload may take and was swept.
	ErrExpired = errors.New("the upload was swept: it took too long")
	// ErrNotLinked: the store wrote the bytes of an upload and then said it does not keep them. The store is not set up for linking (blobgate: dedup "tenant").
	ErrNotLinked = errors.New("the store cannot link the bytes it has just written: is deduplication per tenant on?")
)

// The words of the guards of this package (meta.RefusedBy).
const (
	WhyBlobNotLive = "blob_not_live" // Hold: the blob is not there, or its bytes are not there yet
	whyUploadGone  = "upload_gone"
	whySizeDiffers = "size_differs"
)

// Layer is the blob layer. The zero value of the times means the defaults.
type Layer struct {
	DB    meta.DB
	Store Store
	// Grace is how long a blob that nothing names is kept (default 24 hours). It must be longer than the longest time between a client learning that a blob exists and committing what names it.
	Grace time.Duration
	// UploadTTL is how long an upload may take from its first byte to its commit (default 24 hours).
	UploadTTL time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// CrashAt is called at every named point; a test makes it panic to stop the work there, as a process that is killed would. nil in the program.
	CrashAt func(point string)
}

func (l *Layer) now() int64 {
	if l.Now != nil {
		return l.Now().Unix()
	}
	return time.Now().Unix()
}

func (l *Layer) grace() int64 {
	if l.Grace > 0 {
		return int64(l.Grace / time.Second)
	}
	return 24 * 3600
}

func (l *Layer) uploadTTL() int64 {
	if l.UploadTTL > 0 {
		return int64(l.UploadTTL / time.Second)
	}
	return 24 * 3600
}

func (l *Layer) at(point string) {
	if l.CrashAt != nil {
		l.CrashAt(point)
	}
}

func token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness: nothing can be done safely
	}
	return hex.EncodeToString(b)
}

// ValidDigest says whether s is a SHA-256 in lower-case hex.
func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// blobKey is the key of one life of a digest.
func blobKey(digest string) string {
	return "blobs/sha256/" + digest[:2] + "/" + digest + "/" + token(8)
}

// Upload is bytes that have been taken in and are held for their owner-to-be until Done is committed or the sweep finds the upload too old.
type Upload struct {
	ID     string
	Digest string // SHA-256, hex
	Size   int64
	key    string
	gone   bool // the copy under key has been deleted: the blob has the bytes
}

// Owner is the name under which the upload holds its blob.
func (u *Upload) Owner() string { return "upload:" + u.ID }

// Ingest reads r to its end, keeps the bytes as a blob and holds the blob for the upload. want is the digest the sender named ("" when it named none); other bytes are ErrDigest and are not kept.
// The caller then commits what the bytes are for, in one Exec: its own rows, Hold(its owner, digest) and Done(upload).
func (l *Layer) Ingest(ctx context.Context, r io.Reader, want string) (*Upload, error) {
	if want != "" && !ValidDigest(want) {
		return nil, ErrDigest
	}
	up := &Upload{ID: token(16)}
	up.key = "uploads/" + up.ID
	// The row comes before the first byte, so that the sweep knows of the key whatever happens next.
	if _, err := l.DB.Exec(ctx, meta.S(`INSERT INTO uploads (id, key, started) VALUES (?, ?, ?)`, up.ID, up.key, l.now())); err != nil {
		return nil, err
	}
	l.at("ingest:row")
	ok := false
	defer func() {
		if r := recover(); r != nil {
			panic(r) // a process that dies cleans nothing up, and neither does a test that plays one
		}
		if !ok {
			// Best effort, with a context of its own: the caller's may be the reason we are here. What this does not finish, the sweep does.
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = l.Abandon(c, up)
		}
	}()

	h := sha256.New()
	n, err := l.Store.Put(ctx, up.key, io.TeeReader(r, h))
	if err != nil {
		return nil, fmt.Errorf("writing the upload: %w", err)
	}
	l.at("ingest:put")
	up.Digest, up.Size = hex.EncodeToString(h.Sum(nil)), n
	if want != "" && want != up.Digest {
		return nil, ErrDigest
	}

	// Claim: the upload holds the digest. If no blob has it, a row is made whose bytes are not there yet ('incoming'); nothing else can hold or read such a row.
	// Sent twice, the list changes nothing the second time.
	_, err = l.DB.Exec(ctx,
		meta.Guard(whyUploadGone, `SELECT 1 FROM uploads WHERE id = ?`, up.ID),
		meta.S(`INSERT INTO blobs (digest, size, key, state, unref_since) VALUES (?, ?, ?, 'incoming', NULL) ON CONFLICT (digest) DO NOTHING`, up.Digest, up.Size, blobKey(up.Digest)),
		meta.Guard(whySizeDiffers, `SELECT 1 FROM blobs WHERE digest = ? AND size = ?`, up.Digest, up.Size),
		meta.S(`INSERT OR IGNORE INTO refs (owner, digest) VALUES (?, ?)`, up.Owner(), up.Digest),
		meta.S(`UPDATE blobs SET unref_since = NULL WHERE digest = ?`, up.Digest),
	)
	switch why, _ := meta.RefusedBy(err); {
	case why == whyUploadGone:
		// The sweep took the upload while its bytes were still coming. The key is ours alone (its name is random), and the sweep's delete may have come before our last byte: delete it again.
		_ = l.Store.Delete(ctx, up.key)
		ok = true // nothing is left to abandon
		return nil, ErrExpired
	case why == whySizeDiffers:
		return nil, fmt.Errorf("a blob with the digest %s has another size than these %d bytes: the store or the database is damaged", up.Digest, up.Size)
	case err != nil:
		return nil, err
	}
	l.at("ingest:claim")

	// The row cannot go while the upload holds it, so what is read here stays true.
	rows, err := l.DB.Query(ctx, meta.S(`SELECT key, state FROM blobs WHERE digest = ?`, up.Digest))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("the blob %s that the upload holds is not in the database", up.Digest)
	}
	if key := meta.Str(rows[0][0]); meta.Str(rows[0][1]) == "incoming" {
		// Whoever made the row may have died before the bytes were linked, so every upload that finds the row incoming links its own bytes; doing it twice is the same as once.
		linked, err := l.Store.Link(ctx, key, up.Digest, up.Size)
		if err != nil {
			return nil, fmt.Errorf("linking the blob: %w", err)
		}
		if !linked {
			return nil, ErrNotLinked
		}
		l.at("ingest:link")
		if _, err := l.DB.Exec(ctx, meta.S(`UPDATE blobs SET state = 'live' WHERE digest = ? AND key = ? AND state = 'incoming'`, up.Digest, key)); err != nil {
			return nil, err
		}
		l.at("ingest:live")
	}

	// The blob has the bytes; the copy under the upload's key is not needed. The row of the upload stays: it is what holds the blob.
	if err := l.bury(ctx, up.key); err != nil {
		return nil, err
	}
	up.gone = true
	l.at("ingest:unstaged")
	ok = true
	return up, nil
}

// Hold makes the statements with which an owner takes the blobs, for the caller's own Exec. The list is refused with WhyBlobNotLive when one of the blobs is not there (it was never pushed, or it was
// collected between the client's look and this commit): the client is told to push it.
func (l *Layer) Hold(owner string, digests ...string) []meta.Stmt {
	var out []meta.Stmt
	for _, d := range digests {
		out = append(out,
			meta.Guard(WhyBlobNotLive, `SELECT 1 FROM blobs WHERE digest = ? AND state = 'live'`, d),
			meta.S(`INSERT OR IGNORE INTO refs (owner, digest) VALUES (?, ?)`, owner, d),
			meta.S(`UPDATE blobs SET unref_since = NULL WHERE digest = ?`, d),
		)
	}
	return out
}

// Drop makes the statements with which an owner lets go of all it holds, for the caller's own Exec. A blob that nobody else holds starts its grace time. In a list that also has Hold, Hold comes first.
func (l *Layer) Drop(owner string) []meta.Stmt {
	return []meta.Stmt{
		meta.S(`UPDATE blobs SET unref_since = ? WHERE unref_since IS NULL
			AND digest IN (SELECT digest FROM refs WHERE owner = ?)
			AND NOT EXISTS (SELECT 1 FROM refs r WHERE r.digest = blobs.digest AND r.owner <> ?)`, l.now(), owner, owner),
		meta.S(`DELETE FROM refs WHERE owner = ?`, owner),
	}
}

// Done makes the statements that end an upload, for the Exec in which the caller commits what the upload was for (after its Hold).
func (l *Layer) Done(up *Upload) []meta.Stmt {
	out := append(l.Drop(up.Owner()), meta.S(`DELETE FROM uploads WHERE id = ?`, up.ID))
	if !up.gone {
		out = append(out, meta.S(`INSERT OR IGNORE INTO graves (key, since) VALUES (?, ?)`, up.key, l.now()))
	}
	return out
}

// Abandon ends an upload whose bytes are not wanted after all.
func (l *Layer) Abandon(ctx context.Context, up *Upload) error {
	if _, err := l.DB.Exec(ctx, l.Done(up)...); err != nil {
		return err
	}
	if up.gone {
		return nil
	}
	return l.dig(ctx, up.key)
}

// Info is what is known of a blob.
type Info struct {
	Digest string
	Size   int64
	key    string
}

// Stat finds a live blob. ErrNotFound when there is none.
func (l *Layer) Stat(ctx context.Context, digest string) (*Info, error) {
	if !ValidDigest(digest) {
		return nil, ErrNotFound
	}
	rows, err := l.DB.Query(ctx, meta.S(`SELECT size, key FROM blobs WHERE digest = ? AND state = 'live'`, digest))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &Info{Digest: digest, Size: meta.Int(rows[0][0]), key: meta.Str(rows[0][1])}, nil
}

// Open reads the bytes from..to of a live blob (to < 0: to the end).
func (l *Layer) Open(ctx context.Context, digest string, from, to int64) (io.ReadCloser, *Info, error) {
	info, err := l.Stat(ctx, digest)
	if err != nil {
		return nil, nil, err
	}
	rc, err := l.Store.Get(ctx, info.key, from, to)
	if err != nil {
		return nil, nil, err
	}
	return rc, info, nil
}

// bury writes down that a key is to be deleted, and deletes it.
func (l *Layer) bury(ctx context.Context, key string) error {
	if _, err := l.DB.Exec(ctx, meta.S(`INSERT OR IGNORE INTO graves (key, since) VALUES (?, ?)`, key, l.now())); err != nil {
		return err
	}
	return l.dig(ctx, key)
}

// dig deletes a key that has a grave, and then the grave. A key has one life (its name is random), so deleting it late or twice harms nothing.
func (l *Layer) dig(ctx context.Context, key string) error {
	if err := l.Store.Delete(ctx, key); err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	l.at("dig:deleted")
	_, err := l.DB.Exec(ctx, meta.S(`DELETE FROM graves WHERE key = ?`, key))
	return err
}

// SumHex is the SHA-256 of the bytes, in hex: the digest of a blob.
func SumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
