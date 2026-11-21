package maven

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
)

const (
	// MaxUpload is the largest file that is taken (the body of a PUT).
	MaxUpload = 4 << 30
	maxPOM    = 10 << 20
	maxSum    = 1024
)

// put takes a PUT: a file of a version (an artifact, a pom, a signature), a checksum file (checked, not kept: the program makes them) or a maven-metadata.xml (not kept: the program makes it).
func (m *Maven) put(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	switch t.kind {
	case kMeta, kSnapMeta:
		if !q.Authorize(w, auth.Write, q.Path) {
			return
		}
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxPOM))
		w.WriteHeader(http.StatusCreated) // `mvn deploy` sends the metadata it merged; this program makes its own from the files
	case kSum:
		m.putSum(w, r, q, t)
	case kFile:
		m.putFile(w, r, q, t)
	default:
		fail(w, http.StatusBadRequest, "not a path of a Maven repository: <group dirs>/<artifactId>/<version>/<artifactId>-<version>[-<classifier>].<ext> (a snapshot: -<yyyyMMdd.HHmmss>-<n> in place of -SNAPSHOT in the file name)")
	}
}

func (m *Maven) putFile(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Write, q.Path) {
		return
	}
	ctx := r.Context()
	ws := []io.Writer{}
	sums := map[string]interface{ Sum([]byte) []byte }{}
	for _, a := range []string{"sha1", "md5", "sha512"} {
		h := newHash(a)
		ws = append(ws, h)
		sums[a] = h
	}
	body := io.TeeReader(http.MaxBytesReader(w, r.Body, MaxUpload), io.MultiWriter(ws...))
	up, err := m.Blobs.Ingest(ctx, body, "")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a file is at most %d bytes", int64(MaxUpload)))
			return
		}
		m.unavailable(w)
		return
	}
	if t.ext == "pom" && t.classifier == "" {
		if msg := m.checkPOM(ctx, up.Digest, t); msg != "" {
			_ = m.Blobs.Abandon(ctx, up)
			fail(w, http.StatusBadRequest, msg)
			return
		}
	}
	p, _ := json.Marshal(props{SHA1: hex.EncodeToString(sums["sha1"].Sum(nil)), MD5: hex.EncodeToString(sums["md5"].Sum(nil)), SHA512: hex.EncodeToString(sums["sha512"].Sum(nil))})
	m.Raw.Commit(w, ctx, q, q.Path, up, contentType(t), string(p))
}

func contentType(t target) string {
	switch t.ext {
	case "pom", "xml":
		return "application/xml"
	case "jar", "war", "ear", "zip":
		return "application/java-archive"
	}
	return "application/octet-stream"
}

// checkPOM reads an uploaded pom and sees that it is the pom of the path: its artifactId is the artifact, and its groupId and version (its own, or its parent's) are those of the path. ("" is fine.)
func (m *Maven) checkPOM(ctx context.Context, digest string, t target) string {
	rc, info, err := m.Blobs.Open(ctx, digest, 0, -1)
	if err != nil {
		return "the pom cannot be read back: try again"
	}
	defer rc.Close()
	if info.Size > maxPOM {
		return "a pom is at most 10 MiB"
	}
	var pom struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
		Parent     struct {
			GroupID string `xml:"groupId"`
			Version string `xml:"version"`
		} `xml:"parent"`
	}
	dec := xml.NewDecoder(io.LimitReader(rc, maxPOM))
	dec.Strict = true
	if err := dec.Decode(&pom); err != nil {
		return "the pom is not XML: " + err.Error()
	}
	g, v := strings.TrimSpace(pom.GroupID), strings.TrimSpace(pom.Version)
	if g == "" {
		g = strings.TrimSpace(pom.Parent.GroupID)
	}
	if v == "" {
		v = strings.TrimSpace(pom.Parent.Version)
	}
	if strings.TrimSpace(pom.ArtifactID) != t.artifact || (g != "" && g != t.group) || (v != "" && v != t.version) {
		return fmt.Sprintf("the pom names %s:%s:%s, the path names %s:%s:%s", g, strings.TrimSpace(pom.ArtifactID), v, t.group, t.artifact, t.version)
	}
	if g == "" || v == "" {
		return "the pom names no groupId or version (and its parent none): the path cannot be checked against it"
	}
	return ""
}

// putSum takes the checksum file that `mvn deploy` sends after a file. It is checked against the file and not kept, because the program makes the checksum files itself.
func (m *Maven) putSum(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Write, q.Path) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSum))
	if err != nil {
		fail(w, http.StatusBadRequest, "a checksum file is at most 1 KiB")
		return
	}
	if t.inner.kind == kMeta || t.inner.kind == kSnapMeta {
		w.WriteHeader(http.StatusCreated) // of a metadata that is not kept
		return
	}
	ctx := r.Context()
	base := strings.TrimSuffix(q.Path, "."+t.algo)
	digest, p, found, err := m.stored(ctx, q.Repo.Name, base)
	if err != nil {
		m.unavailable(w)
		return
	}
	if !found {
		fail(w, http.StatusConflict, "upload the file before its checksum")
		return
	}
	var want string
	switch t.algo {
	case "sha256":
		want = digest
	case "sha1":
		want = p.SHA1
	case "md5":
		want = p.MD5
	default:
		want = p.SHA512
	}
	if want == "" {
		if want, err = m.compute(ctx, digest, t.algo); err != nil {
			m.unavailable(w)
			return
		}
	}
	if got := firstHex(b, sizeOf(t.algo)); got != want {
		fail(w, http.StatusBadRequest, "the "+t.algo+" is not that of the file")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// firstHex finds the first word of b that is a hex string of n digits (a checksum file may name the file after it), in lower case. "" if there is none.
func firstHex(b []byte, n int) string {
	for _, f := range strings.Fields(string(b)) {
		f = strings.ToLower(f)
		if len(f) == n {
			if _, err := hex.DecodeString(f); err == nil {
				return f
			}
		}
	}
	return ""
}
