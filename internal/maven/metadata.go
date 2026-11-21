package maven

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"sort"
	"strings"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
)

type fileRow struct {
	path    string
	created int64
}

// files lists the files of an artifact that a hosted repository has: <name>/<version>/<file>.
func (m *Maven) files(ctx context.Context, repo, name string) ([]fileRow, error) {
	prefix := name + "/"
	rows, err := m.DB.Query(ctx, meta.S(`SELECT path, created_at FROM assets WHERE repo = ? AND substr(path, 1, ?) = ? ORDER BY path`, repo, len(prefix), prefix))
	if err != nil {
		return nil, err
	}
	var out []fileRow
	for _, r := range rows {
		out = append(out, fileRow{meta.Str(r[0]), meta.Int(r[1])})
	}
	return out, nil
}

// generate makes maven-metadata.xml of an artifact (the versions) or of a snapshot (its latest files) from the files that are kept. nil: there is nothing of that name.
func (m *Maven) generate(ctx context.Context, repo string, t target) ([]byte, int64, error) {
	rows, err := m.files(ctx, repo, t.name)
	if err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var last int64
	if t.kind == kMeta {
		seen := map[string]bool{}
		for _, r := range rows {
			parts := strings.Split(strings.TrimPrefix(r.path, t.name+"/"), "/")
			if len(parts) != 2 || parts[1] == "maven-metadata.xml" {
				continue // not a file of a version (the files of another artifact below this directory are not counted)
			}
			if p := parse(r.path); p.kind != kFile || p.name != t.name {
				continue
			}
			seen[parts[0]] = true
			last = max(last, r.created)
		}
		if len(seen) == 0 {
			return nil, 0, nil
		}
		vs := make([]string, 0, len(seen))
		for v := range seen {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool { return compare(vs[i], vs[j]) < 0 })
		release := ""
		for _, v := range vs {
			if !strings.HasSuffix(v, "-SNAPSHOT") {
				release = v
			}
		}
		buf.WriteString("<metadata>\n  <groupId>" + esc(t.group) + "</groupId>\n  <artifactId>" + esc(t.artifact) + "</artifactId>\n  <versioning>\n")
		buf.WriteString("    <latest>" + esc(vs[len(vs)-1]) + "</latest>\n")
		if release != "" {
			buf.WriteString("    <release>" + esc(release) + "</release>\n")
		}
		buf.WriteString("    <versions>\n")
		for _, v := range vs {
			buf.WriteString("      <version>" + esc(v) + "</version>\n")
		}
		buf.WriteString("    </versions>\n    <lastUpdated>" + stamp(last) + "</lastUpdated>\n  </versioning>\n</metadata>\n")
		return buf.Bytes(), last, nil
	}
	// A snapshot: for each classifier and extension, the file with the latest timestamp and build number.
	type key struct{ classifier, ext string }
	type best struct {
		value   string
		stamp   string
		build   int
		created int64
	}
	latest := map[key]best{}
	var top best
	for _, r := range rows {
		p := parse(r.path)
		if p.kind != kFile || p.name != t.name || p.version != t.version {
			continue
		}
		n := 0
		for _, c := range p.build {
			n = n*10 + int(c-'0')
		}
		b := best{value: strings.TrimSuffix(t.version, "-SNAPSHOT") + "-" + p.stamp + "-" + p.build, stamp: p.stamp, build: n, created: r.created}
		k := key{p.classifier, p.ext}
		if o, ok := latest[k]; !ok || b.stamp > o.stamp || (b.stamp == o.stamp && b.build > o.build) {
			latest[k] = b
		}
		if b.stamp > top.stamp || (b.stamp == top.stamp && b.build > top.build) {
			top = b
		}
		last = max(last, r.created)
	}
	if len(latest) == 0 {
		return nil, 0, nil
	}
	keys := make([]key, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].classifier != keys[j].classifier {
			return keys[i].classifier < keys[j].classifier
		}
		return keys[i].ext < keys[j].ext
	})
	buf.WriteString("<metadata modelVersion=\"1.1.0\">\n  <groupId>" + esc(t.group) + "</groupId>\n  <artifactId>" + esc(t.artifact) + "</artifactId>\n  <version>" + esc(t.version) + "</version>\n  <versioning>\n")
	buf.WriteString("    <snapshot>\n      <timestamp>" + top.stamp + "</timestamp>\n      <buildNumber>" + itoa(top.build) + "</buildNumber>\n    </snapshot>\n")
	buf.WriteString("    <lastUpdated>" + stamp(last) + "</lastUpdated>\n    <snapshotVersions>\n")
	for _, k := range keys {
		b := latest[k]
		buf.WriteString("      <snapshotVersion>\n")
		if k.classifier != "" {
			buf.WriteString("        <classifier>" + esc(k.classifier) + "</classifier>\n")
		}
		buf.WriteString("        <extension>" + esc(k.ext) + "</extension>\n        <value>" + esc(b.value) + "</value>\n        <updated>" + stamp(b.created) + "</updated>\n      </snapshotVersion>\n")
	}
	buf.WriteString("    </snapshotVersions>\n  </versioning>\n</metadata>\n")
	return buf.Bytes(), last, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// metadata answers a GET of maven-metadata.xml of a hosted repository: it is made at each request from the files that are kept, so it is never out of step with them. An uploaded one is not kept.
func (m *Maven) metadata(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	body, _, err := m.generate(r.Context(), q.Repo.Name, t)
	if err != nil {
		m.unavailable(w)
		return
	}
	if body == nil {
		fail(w, http.StatusNotFound, "no such artifact")
		return
	}
	write(w, r, body, "application/xml")
}
