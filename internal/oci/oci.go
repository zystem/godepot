// Package oci is the format "oci": the OCI distribution API 1.1, for images, indexes, Helm charts, signatures and any other artifact (docs/oci.md). A name is "<repository>/<image>": the first part
// is the repository of the settings file (docs/spec.md, F1), the rest is the image. Blobs, manifests and tags belong to a name; the bytes of a blob are kept once for all names by the blob layer.
//
// What is atomic: a blob is linked to a name, a manifest is stored with its tag and its links, a manifest is deleted with its tags, each in one transaction of the database (meta.Exec), with its
// line of the audit log. What a manifest names (its config, its layers, the manifests of an index) is checked in the same transaction.
package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
)

// OCI is the handler of the repositories of the format oci.
type OCI struct {
	Blobs *blob.Layer
	DB    meta.DB
	Audit *audit.Log
	// Reads, Writes and Deletes count the requests per repository; Refused counts by reason. nil: not counted.
	Reads, Writes, Deletes, Refused *metrics.Counter
	// Downloads limits the blobs being sent at once (nil: no limit). Rate is the least speed of a client that is being sent a blob, in bytes a second (0: no least).
	Downloads *format.Gate
	Rate      int64
	// Active is set to the number of downloads going on (nil: not shown).
	Active *metrics.Gauge
	// MaxManifest is the largest manifest taken in, in bytes (default 4 MiB, the figure of the specification).
	MaxManifest int64
	// UploadTTL is how long an upload in parts may take from its start to its end (default 24 hours).
	UploadTTL time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

const (
	defaultMaxManifest = 4 << 20
	inlineManifest     = 256 << 10 // a manifest up to this size is a row of the database (docs/spec.md, S5); a larger one is a blob
	maxEntries         = 4096      // layers of a manifest, children of an index
	maxName            = 255
)

var (
	componentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagRE       = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	uploadRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func (h *OCI) now() int64 {
	if h.Now != nil {
		return h.Now().Unix()
	}
	return time.Now().Unix()
}

func (h *OCI) active() {
	if h.Active != nil {
		h.Active.Set(int64(h.Downloads.InUse()))
	}
}

func (h *OCI) maxManifest() int64 {
	if h.MaxManifest > 0 {
		return h.MaxManifest
	}
	return defaultMaxManifest
}

func (h *OCI) ttl() time.Duration {
	if h.UploadTTL > 0 {
		return h.UploadTTL
	}
	return 24 * time.Hour
}

// The kinds of request.
const (
	kBlob      = "blob"
	kUploads   = "uploads" // POST: begins an upload
	kUpload    = "upload"  // one upload
	kManifest  = "manifest"
	kTags      = "tags"
	kReferrers = "referrers"
)

type route struct {
	image string
	kind  string
	ref   string // a digest, a tag, an upload id
}

// parse reads the path that follows the repository: "<image>/blobs/<digest>", "<image>/blobs/uploads/", "<image>/blobs/uploads/<id>", "<image>/manifests/<reference>", "<image>/tags/list",
// "<image>/referrers/<digest>". The image is read from the left: the words of the API are looked for from the right, so an image may be called "blobs" or "tags".
func parse(path string) (route, bool) {
	p := strings.Split(path, "/")
	n := len(p)
	img := func(k int) string { return strings.Join(p[:k], "/") }
	switch {
	case n >= 2 && p[n-2] == "blobs" && p[n-1] == "uploads": // the address without the closing "/"
		return route{image: img(n - 2), kind: kUploads}, true
	case n >= 3 && p[n-3] == "blobs" && p[n-2] == "uploads" && p[n-1] == "":
		return route{image: img(n - 3), kind: kUploads}, true
	case n >= 3 && p[n-3] == "blobs" && p[n-2] == "uploads":
		return route{image: img(n - 3), kind: kUpload, ref: p[n-1]}, true
	case n >= 2 && p[n-2] == "blobs":
		return route{image: img(n - 2), kind: kBlob, ref: p[n-1]}, true
	case n >= 2 && p[n-2] == "manifests":
		return route{image: img(n - 2), kind: kManifest, ref: p[n-1]}, true
	case n >= 2 && p[n-2] == "tags" && p[n-1] == "list":
		return route{image: img(n - 2), kind: kTags}, true
	case n >= 2 && p[n-2] == "referrers":
		return route{image: img(n - 2), kind: kReferrers, ref: p[n-1]}, true
	}
	return route{}, false
}

// CheckImage says what is wrong with the name of an image under the repository.
func CheckImage(repo, image string) error {
	if image == "" {
		return errors.New("a name is <repository>/<image>: the image is missing")
	}
	if len(repo)+1+len(image) > maxName {
		return fmt.Errorf("the name is longer than %d bytes", maxName)
	}
	for _, c := range strings.Split(image, "/") {
		if !componentRE.MatchString(c) {
			return fmt.Errorf("%q is not a part of a name: lower-case letters and digits, separated by '.', '_', '__' or '-'", c)
		}
	}
	return nil
}

// ValidDigest says whether s is "sha256:" and 64 hex digits, and returns the hex.
func ValidDigest(s string) (string, bool) {
	hex, ok := strings.CutPrefix(s, "sha256:")
	return hex, ok && blob.ValidDigest(hex)
}

// Serve answers one request.
func (h *OCI) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	rt, ok := parse(q.Path)
	if !ok {
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "this address is not one of the OCI distribution API", nil)
		return
	}
	if err := CheckImage(q.Repo.Name, rt.image); err != nil {
		h.fail(w, http.StatusBadRequest, "NAME_INVALID", err.Error(), nil)
		return
	}
	switch rt.kind {
	case kBlob:
		h.blob(w, r, q, rt)
	case kUploads:
		h.startUpload(w, r, q, rt)
	case kUpload:
		h.upload(w, r, q, rt)
	case kManifest:
		h.manifest(w, r, q, rt)
	case kTags:
		h.tags(w, r, q, rt)
	case kReferrers:
		h.referrers(w, r, q, rt)
	}
}

func (h *OCI) fail(w http.ResponseWriter, status int, code, msg string, detail any) {
	format.WriteOCI(w, status, format.OCIError{Code: code, Message: msg, Detail: detail})
}

func (h *OCI) method(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed here", nil)
}

func (h *OCI) count(c *metrics.Counter, label string) {
	if c != nil {
		c.Add(label, 1)
	}
}

func (h *OCI) unavailable(w http.ResponseWriter, err error) {
	var u *meta.Unavailable
	if errors.As(err, &u) || errors.Is(err, context.Canceled) {
		h.fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the database or the store is not available: try again", nil)
		return
	}
	h.fail(w, http.StatusBadGateway, "UNAVAILABLE", "the store failed: try again", nil)
}

func (h *OCI) event(q format.Request, action, image, tail, result, detail string) audit.Event {
	return audit.Event{Actor: q.Actor(), Token: q.Principal.Token, Remote: q.Remote, Action: action, Repo: q.Repo.Name, Path: image + tail, Result: result, Detail: detail}
}

func (h *OCI) location(q format.Request, image, tail string) string {
	return "/v2/" + q.Repo.Name + "/" + image + tail
}

// The owners of the references of a name. They are fixed by the name and the digest, so that taking the same blob twice is one reference, and no owner is left without a row.
func blobOwner(repo, image, digest string) string {
	return "ociblob:" + repo + "/" + image + "@sha256:" + digest
}
func manifestOwner(repo, image, digest string) string {
	return "ocimanifest:" + repo + "/" + image + "@sha256:" + digest
}
func uploadOwner(id string) string { return "ociupload:" + id }

// inList makes "(?, ?, ?)" and its arguments for a list of digests.
func inList(digests []string) (string, []any) {
	args := make([]any, len(digests))
	for i, d := range digests {
		args[i] = d
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?, ", len(digests)), ", ") + ")", args
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// exists says whether a name has anything: a blob, a manifest, a tag or an upload.
func (h *OCI) exists(ctx context.Context, repo, image string) (bool, error) {
	rows, err := h.DB.Query(ctx, meta.S(`SELECT 1 WHERE EXISTS (SELECT 1 FROM oci_blobs WHERE repo = ? AND image = ?)
		OR EXISTS (SELECT 1 FROM oci_manifests WHERE repo = ? AND image = ?)
		OR EXISTS (SELECT 1 FROM oci_uploads WHERE repo = ? AND image = ?)`, repo, image, repo, image, repo, image))
	return len(rows) > 0, err
}

var _ = auth.Read
