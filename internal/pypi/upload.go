package pypi

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
)

// The limits of an upload (docs/spec.md, F5).
const (
	MaxUpload   = 1 << 30 // the whole request
	maxField    = 64 << 10
	maxFields   = 128
	maxMetadata = 10 << 20
	maxEntries  = 50000
)

// upload takes a distribution as `twine upload` sends it: a POST of a form (multipart/form-data) whose fields name the project and the version and whose part "content" is the file. The file is
// ingested as it comes; the fields are read before it by every client that exists, and after it too, since the whole form is read before anything is committed.
func (p *PyPI) upload(w http.ResponseWriter, r *http.Request, q format.Request) {
	if q.Principal.Anonymous() {
		q.Authorize(w, auth.Write, "legacy/") // 401, before the form is looked at
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		reply(w, http.StatusBadRequest, "an upload is a form: multipart/form-data")
		return
	}
	ctx := r.Context()
	body := http.MaxBytesReader(w, r.Body, MaxUpload)
	mr := newMultipart(body, params["boundary"])
	fields := map[string]string{}
	var (
		filename string
		fi       file
		path     string
		up       = (*upload)(nil)
	)
	abandon := func() {
		if up != nil {
			_ = p.Blobs.Abandon(ctx, up.u)
		}
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			abandon()
			if strings.Contains(err.Error(), "too large") {
				reply(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("an upload is at most %d bytes", MaxUpload))
				return
			}
			reply(w, http.StatusBadRequest, "the form could not be read")
			return
		}
		if part.FormName() == "content" && part.FileName() != "" {
			if up != nil {
				abandon()
				reply(w, http.StatusBadRequest, "one file to an upload")
				return
			}
			filename = part.FileName()
			var ok bool
			if fi, ok = parseFilename(filename); !ok {
				reply(w, http.StatusBadRequest, "the file name is not that of a wheel (name-version-python-abi-platform.whl) or a source distribution (name-version.tar.gz or .zip)")
				return
			}
			path = "files/" + fi.project + "/" + filename
			// The file is let in only when the account may write it: this is known before the first byte is read.
			if !q.Authorize(w, auth.Write, path) {
				return
			}
			u, err := p.Blobs.Ingest(ctx, part, "")
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					reply(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("an upload is at most %d bytes", MaxUpload))
					return
				}
				p.unavailable(w)
				return
			}
			up = &upload{u: u}
			continue
		}
		if len(fields) >= maxFields {
			abandon()
			reply(w, http.StatusBadRequest, "too many fields")
			return
		}
		b, _ := io.ReadAll(io.LimitReader(part, maxField+1))
		if len(b) > maxField {
			abandon()
			reply(w, http.StatusBadRequest, "a field is too long")
			return
		}
		fields[part.FormName()] = string(b)
	}
	if up == nil {
		reply(w, http.StatusBadRequest, "no file: the part \"content\" is the file")
		return
	}
	bad := func(msg string) {
		abandon()
		reply(w, http.StatusBadRequest, msg)
	}
	if a := fields[":action"]; a != "" && a != "file_upload" {
		bad("the action " + a + " is not taken: only file_upload")
		return
	}
	if n := fields["name"]; n == "" || normalize(n) != fi.project {
		bad(fmt.Sprintf("the name %q is not the project of the file name (%s)", fields["name"], fi.project))
		return
	}
	if !sameVersion(fields["version"], fi.version) {
		bad(fmt.Sprintf("the version %q is not that of the file name (%s)", fields["version"], fi.version))
		return
	}
	if d := strings.ToLower(fields["sha256_digest"]); d != "" && d != up.u.Digest {
		bad("the sha256_digest is not that of the bytes")
		return
	}
	props := map[string]string{"name": fi.project, "version": fi.version}
	if rp := fields["requires_python"]; rp != "" && len(rp) <= 256 {
		props["requires_python"] = rp
	}
	var metadata []byte
	if fi.wheel {
		md, err := wheelMetadata(r, p, up.u.Digest, up.u.Size)
		if err != nil {
			bad(err.Error())
			return
		}
		metadata = md
		h := parseHeaders(md)
		if normalize(h["name"]) != fi.project || !sameVersion(h["version"], fi.version) {
			bad(fmt.Sprintf("the METADATA of the wheel names %q %q, the file name %q %q", h["name"], h["version"], fi.project, fi.version))
			return
		}
		if rp := h["requires-python"]; rp != "" && len(rp) <= 256 {
			props["requires_python"] = rp
		}
	}
	// The same bytes again is fine and changes nothing; other bytes under a file name are refused (docs/spec.md, R4): the hash in a lock file must stay true.
	if have, found, err := p.Lookup(ctx, q.Repo.Name, path); err != nil {
		abandon()
		p.unavailable(w)
		return
	} else if found {
		abandon()
		if have == up.u.Digest {
			reply(w, http.StatusOK, "the file is there with these bytes")
		} else {
			reply(w, http.StatusConflict, "this file name is taken by other bytes; a file is not overwritten: publish a new version")
		}
		return
	}
	if metadata != nil {
		in, err := p.Blobs.Ingest(ctx, bytes.NewReader(metadata), "")
		if err != nil {
			abandon()
			p.unavailable(w)
			return
		}
		qq := &quiet{h: http.Header{}, code: 200}
		p.Raw.Commit(qq, ctx, q, path+".metadata", in, "text/plain; charset=utf-8", "{}")
		if qq.code >= 500 {
			abandon()
			p.unavailable(w)
			return
		}
	}
	pj := "{"
	first := true
	for _, k := range []string{"name", "version", "requires_python"} {
		if v, ok := props[k]; ok {
			if !first {
				pj += ","
			}
			first = false
			pj += fmt.Sprintf("%q:%q", k, v)
		}
	}
	pj += "}"
	p.Raw.Commit(w, ctx, q, path, up.u, "application/octet-stream", pj)
}

// sameVersion compares the version of a form or a METADATA with that of a file name: a file name has "_" where a version may have "-", and case does not matter.
func sameVersion(a, b string) bool {
	n := func(s string) string { return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-")) }
	return a != "" && n(a) == n(b)
}

// wheelMetadata reads the METADATA of a wheel where the wheel is kept (never unpacked): the file "<name>-<version>.dist-info/METADATA".
func wheelMetadata(r *http.Request, p *PyPI, digest string, size int64) ([]byte, error) {
	zr, err := zip.NewReader(format.BlobReaderAt{Ctx: r.Context(), L: p.Blobs, Digest: digest}, size)
	if err != nil {
		return nil, errors.New("the wheel is not a zip")
	}
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("the wheel has more than %d files", maxEntries)
	}
	var md *zip.File
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "\\") {
			return nil, fmt.Errorf("the name %q is not allowed in a wheel", f.Name)
		}
		if dir, name, ok := strings.Cut(f.Name, "/"); ok && name == "METADATA" && strings.HasSuffix(dir, ".dist-info") {
			if md != nil {
				return nil, errors.New("the wheel has more than one METADATA")
			}
			md = f
		}
	}
	if md == nil {
		return nil, errors.New("the wheel has no <name>-<version>.dist-info/METADATA")
	}
	if md.UncompressedSize64 > maxMetadata {
		return nil, errors.New("the METADATA is too large")
	}
	rc, err := md.Open()
	if err != nil {
		return nil, errors.New("the METADATA cannot be read")
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil || len(b) > maxMetadata {
		return nil, errors.New("the METADATA cannot be read")
	}
	return b, nil
}

// parseHeaders reads the headers of a METADATA (RFC 822 style) up to the first blank line: the first value of each, keys in lower case.
func parseHeaders(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			k = strings.ToLower(strings.TrimSpace(k))
			if _, seen := out[k]; !seen {
				out[k] = strings.TrimSpace(v)
			}
		}
	}
	return out
}
