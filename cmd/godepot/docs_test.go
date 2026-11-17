package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goFiles(t *testing.T, dir string, tests bool) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") && strings.HasSuffix(p, "_test.go") == tests {
			sb.WriteString(read(t, p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// A setting, a metric or a command that the program has and its page does not mention fails the build (docs/writing-docs.md).
func TestEverySettingMetricAndCommandIsInTheDocs(t *testing.T) {
	src := goFiles(t, "../../cmd", false) + goFiles(t, "../../internal", false)
	conf, mets := read(t, "../../docs/configuration.md"), read(t, "../../docs/metrics.md")
	for _, name := range regexp.MustCompile(`GODEPOT_[A-Z_]+[A-Z]`).FindAllString(src, -1) {
		if !strings.Contains(conf, name) {
			t.Errorf("%s is read by the program and is not in docs/configuration.md", name)
		}
	}
	for _, m := range regexp.MustCompile(`"(godepot_[a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(mets, m[1]) {
			t.Errorf("metric %s is not in docs/metrics.md", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z]+) {2,}`).FindAllStringSubmatch(usage, -1) {
		if !strings.Contains(conf, "`godepot "+m[1]) {
			t.Errorf("the command %s is not in docs/configuration.md", m[1])
		}
	}
	for _, flag := range []string{"-fix"} {
		if !strings.Contains(conf, flag) {
			t.Errorf("the flag %s is not in docs/configuration.md", flag)
		}
	}
	// every exit code
	for _, c := range []string{"0", "1", "2", "3"} {
		if !strings.Contains(conf, "| "+c+" |") {
			t.Errorf("the exit code %s is not in docs/configuration.md", c)
		}
	}
	// what the tests read is in the page about the tests
	testing_ := read(t, "../../docs/testing.md")
	all := goFiles(t, "../..", true) + read(t, "../../tools/test/blobgate-stand.sh")
	for _, name := range regexp.MustCompile(`GODEPOT_[A-Z_]+[A-Z]`).FindAllString(all, -1) {
		if !strings.Contains(testing_, name) && !strings.Contains(conf, name) {
			t.Errorf("%s is read by the tests and is not in docs/testing.md", name)
		}
	}
}

// Every error word of the layer that a caller sees is named in docs/blob-layer.md.
func TestTheBlobLayerPageNamesWhatItReturns(t *testing.T) {
	page := read(t, "../../docs/blob-layer.md")
	for _, w := range []string{"ErrExpired", "ErrDigest", "ErrNotFound", "ErrNotLinked", "blob_not_live", "upload_gone"} {
		if !strings.Contains(page, w) {
			t.Errorf("%s is not in docs/blob-layer.md", w)
		}
	}
}
