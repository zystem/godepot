package pypi

import (
	"encoding/json"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	mimeJSON = "application/vnd.pypi.simple.v1+json"
	mimeHTML = "application/vnd.pypi.simple.v1+html"
)

// entry is one file of a project, as the index shows it.
type entry struct {
	Filename       string
	SHA256         string
	Size           int64
	RequiresPython string
	Yanked         bool
	YankedReason   string
	Metadata       bool   // there is a .metadata file (PEP 658)
	MetadataSHA256 string // its hash, when it is known
	UploadTime     string
}

// wantsJSON reads the Accept header: the JSON of PEP 691 when it is asked for at least as much as any form of HTML; HTML (PEP 503) when there is no header or nothing is asked for.
func wantsJSON(h string) bool {
	best := map[string]float64{}
	for _, part := range strings.Split(h, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(f), "q="); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					q = n
				}
			}
		}
		switch mt {
		case mimeJSON, "application/json":
			best["json"] = max(best["json"], q)
		case mimeHTML, "text/html", "*/*", "text/*", "application/*":
			best["html"] = max(best["html"], q)
		}
	}
	return best["json"] > 0 && best["json"] >= best["html"]
}

// render writes the index of a project. Each file's address is relative to the index, so the client stays on the address it used (a group's, for instance).
func render(w http.ResponseWriter, r *http.Request, project string, files []entry) {
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	w.Header().Set("Vary", "Accept")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rel := func(f entry) string { return "../../files/" + project + "/" + f.Filename }
	if wantsJSON(r.Header.Get("Accept")) {
		type jfile struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython string            `json:"requires-python,omitempty"`
			Yanked         any               `json:"yanked"`
			CoreMetadata   any               `json:"core-metadata,omitempty"`
			Size           int64             `json:"size,omitempty"`
			UploadTime     string            `json:"upload-time,omitempty"`
		}
		out := struct {
			Meta     map[string]string `json:"meta"`
			Name     string            `json:"name"`
			Files    []jfile           `json:"files"`
			Versions []string          `json:"versions,omitempty"`
		}{Meta: map[string]string{"api-version": "1.1"}, Name: project, Files: []jfile{}}
		seen := map[string]bool{}
		for _, f := range files {
			j := jfile{Filename: f.Filename, URL: rel(f), Hashes: map[string]string{"sha256": f.SHA256}, RequiresPython: f.RequiresPython, Yanked: false, Size: f.Size, UploadTime: f.UploadTime}
			if f.Yanked {
				j.Yanked = f.YankedReason
				if f.YankedReason == "" {
					j.Yanked = true
				}
			}
			if f.Metadata {
				if f.MetadataSHA256 != "" {
					j.CoreMetadata = map[string]string{"sha256": f.MetadataSHA256}
				} else {
					j.CoreMetadata = true
				}
			}
			out.Files = append(out.Files, j)
			if pf, ok := parseFilename(f.Filename); ok && !seen[pf.version] {
				seen[pf.version] = true
				out.Versions = append(out.Versions, pf.version)
			}
		}
		w.Header().Set("Content-Type", mimeJSON)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta name=\"pypi:repository-version\" content=\"1.1\">\n<title>Links for " + html.EscapeString(project) + "</title>\n</head>\n<body>\n<h1>Links for " + html.EscapeString(project) + "</h1>\n")
	for _, f := range files {
		sb.WriteString("<a href=\"" + html.EscapeString(rel(f)) + "#sha256=" + f.SHA256 + "\"")
		if f.RequiresPython != "" {
			sb.WriteString(" data-requires-python=\"" + html.EscapeString(f.RequiresPython) + "\"")
		}
		if f.Yanked {
			sb.WriteString(" data-yanked=\"" + html.EscapeString(f.YankedReason) + "\"")
		}
		if f.Metadata {
			v := "true"
			if f.MetadataSHA256 != "" {
				v = "sha256=" + f.MetadataSHA256
			}
			sb.WriteString(" data-core-metadata=\"" + v + "\" data-dist-info-metadata=\"" + v + "\"")
		}
		sb.WriteString(">" + html.EscapeString(f.Filename) + "</a><br/>\n")
	}
	sb.WriteString("</body>\n</html>\n")
	w.Header().Set("Content-Type", mimeHTML)
	_, _ = w.Write([]byte(sb.String()))
}
