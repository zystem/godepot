// Package secret reads a secret that the operator gives to the program: from the environment variable NAME, or from the file that NAME_FILE names.
package secret

import (
	"fmt"
	"os"
	"strings"
)

// Read returns the value of NAME, or the contents of the file that NAME_FILE names without the line breaks at its end (a secret kept out of the environment of the process).
// "" with no error: neither is set. Setting both is an error. A file is read at each call, so a new secret in it is seen by the next reader.
func Read(env func(string) string, name string) (string, error) {
	v, f := env(name), env(name+"_FILE")
	if v != "" && f != "" {
		return "", fmt.Errorf("%s and %s_FILE are both set: set one", name, name)
	}
	if f != "" {
		b, err := os.ReadFile(f) // #nosec G304 G703 -- the path is the operator's own setting (NAME_FILE): reading the file it names is the point
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return v, nil
}
