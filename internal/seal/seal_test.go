package seal_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/seal"
	"github.com/zystem/godepot/internal/testutil"
)

func TestTheKeyOfADatabase(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	dir := t.TempDir()
	path, now := filepath.Join(dir, "keys", "seal.key"), time.Unix(1_800_000_000, 0)
	inspect := func(p, want, errPart string) {
		t.Helper()
		got, err := seal.Inspect(ctx, db, p)
		if !strings.Contains(got, want) || (errPart == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), errPart)) {
			t.Fatalf("Inspect = %q, %v", got, err)
		}
	}
	refused := func(p, errPart string) {
		t.Helper()
		if _, _, err := seal.Open(ctx, db, p, now); !errors.Is(err, seal.ErrWrongKey) || !strings.Contains(err.Error(), errPart) || !strings.Contains(err.Error(), "godepot reset-seal-key -yes") {
			t.Fatalf("Open(%s): %v", p, err)
		}
	}
	inspect(path, "not made yet", "")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Inspect made a file")
	}
	key, made, err := seal.Open(ctx, db, path, now)
	if err != nil || !made {
		t.Fatal(made, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "keys", ".seal-*")); len(left) != 0 {
		t.Fatal("a temporary file stayed", left)
	}
	if _, made, err = seal.Open(ctx, db, path, now); err != nil || made {
		t.Fatal("the second start", made, err)
	}
	inspect(path, "ok", "")

	// the database has a key: another file is refused, and a missing one is never made
	gone := filepath.Join(dir, "gone.key")
	refused(gone, "never made for a database that has one")
	inspect(gone, "", "is not there")
	if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a key was made for a database that has one")
	}
	wrong := filepath.Join(dir, "wrong.key")
	if _, _, err := seal.MakeKey(wrong); err != nil {
		t.Fatal(err)
	}
	refused(wrong, "another key")
	inspect(wrong, "", "another key")
	broken := filepath.Join(dir, "broken.key")
	if err := os.WriteFile(broken, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := seal.Open(ctx, db, broken, now); err == nil || !strings.Contains(err.Error(), "reset-seal-key -yes") || !strings.Contains(err.Error(), "remove the damaged") {
		t.Fatalf("a damaged key: %v", err)
	}
	inspect(broken, "", "reset-seal-key -yes")

	s := &seal.Store{DB: db, Key: key}
	if err := s.Set(ctx, "upstream/hub", "the hub password", "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "ldap", "first", "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "ldap", "the directory password", "root"); err != nil {
		t.Fatal(err)
	}
	get := func(s *seal.Store, name, want string) {
		t.Helper()
		v, found, err := s.Get(ctx, name)
		if err != nil || v != want || found != (want != "") {
			t.Fatalf("%s: %q %v %v", name, v, found, err)
		}
	}
	get(s, "ldap", "the directory password")
	get(s, "upstream/none", "")
	rows, err := db.Query(ctx, meta.S(`SELECT sealed FROM secrets WHERE name = 'ldap'`))
	if err != nil || strings.Contains(meta.Str(rows[0][0]), "password") || !strings.HasPrefix(meta.Str(rows[0][0]), "v1:") {
		t.Fatal(rows, err)
	}
	// what is sealed for one name does not open under another
	if _, err := db.Exec(ctx, meta.S(`UPDATE secrets SET sealed = (SELECT sealed FROM secrets WHERE name = 'ldap') WHERE name = 'upstream/hub'`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "upstream/hub"); err == nil {
		t.Fatal("a moved secret was opened")
	}
	if _, err := s.Reseal(ctx, key); err == nil {
		t.Fatal("reseal went over a damaged secret")
	}
	if err := s.Delete(ctx, "upstream/hub"); err != nil {
		t.Fatal(err)
	}

	// reseal: the new key opens, the old one writes nothing more
	oldStore := &seal.Store{DB: db, Key: key}
	nextPath := filepath.Join(dir, "next.key")
	next, _, err := seal.MakeKey(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reseal(ctx, next); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	get(s, "ldap", "the directory password")
	if err := oldStore.Set(ctx, "ldap", "with the old key", "root"); !errors.Is(err, seal.ErrWrongKey) {
		t.Fatal("written with the old key", err)
	}
	if _, _, err := oldStore.Get(ctx, "ldap"); err == nil {
		t.Fatal("opened with the old key")
	}
	refused(path, "another key")
	if _, _, err := seal.Open(ctx, db, nextPath, now); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "ldap" || list[0].SetBy != "root" {
		t.Fatal(list, err)
	}

	// the key is lost: the secrets are removed, and a new key may be made
	if names, err := s.Reset(ctx); err != nil || len(names) != 1 || names[0] != "ldap" {
		t.Fatal(names, err)
	}
	if _, made, err := seal.Open(ctx, db, gone, now); err != nil || !made {
		t.Fatal(made, err)
	}

	// without a key nothing is written
	if err := (&seal.Store{DB: db}).Set(ctx, "ldap", "x", "root"); !errors.Is(err, seal.ErrNoKey) {
		t.Fatal(err)
	}
}

// Replicas that start at once on a database with no key, each with a disk of its own: one wins, the others stop and leave no file behind.
func TestReplicasThatStartAtOnce(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	dir := t.TempDir()
	const n = 6
	errs := make([]error, n)
	paths := make([]string, n)
	done := make(chan struct{})
	for i := range n {
		paths[i] = filepath.Join(dir, string(rune('a'+i)), "seal.key")
		go func() {
			_, _, errs[i] = seal.Open(ctx, db, paths[i], time.Now())
			done <- struct{}{}
		}()
	}
	for range n {
		<-done
	}
	won := 0
	for i := range n {
		_, statErr := os.Stat(paths[i])
		switch {
		case errs[i] == nil && statErr == nil:
			won++
		case errs[i] != nil && errors.Is(errs[i], seal.ErrWrongKey) && errors.Is(statErr, fs.ErrNotExist):
		default:
			t.Fatalf("replica %d: %v, file: %v", i, errs[i], statErr)
		}
	}
	if won != 1 {
		t.Fatalf("%d replicas have a key", won)
	}
}

func TestFilesThatAreNoKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	for _, text := range []string{"", "not base64!", "c2hvcnQ=\n"} {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := seal.ReadKey(p); err == nil || !strings.Contains(err.Error(), "is not a seal key") {
			t.Fatalf("%q: %v", text, err)
		}
		if _, _, err := seal.MakeKey(p); err == nil {
			t.Fatalf("%q was taken or written over", text)
		}
	}
}
