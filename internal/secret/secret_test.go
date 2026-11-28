package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	write := func(s string) {
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{}
	get := func(n string) string { return env[n] }
	want := func(value, errPart string) {
		t.Helper()
		v, err := Read(get, "PW")
		if v != value || (errPart == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), errPart)) {
			t.Fatalf("Read = %q, %v; want %q, error with %q", v, err, value, errPart)
		}
	}
	want("", "")
	env["PW"] = "from-env"
	want("from-env", "")
	env["PW_FILE"] = file
	want("", "both set")
	delete(env, "PW")
	want("", "PW_FILE: ")
	write("first \r\n")
	want("first ", "") // only the line breaks at the end are taken off
	write("second\n")
	want("second", "") // read again at each call
}
