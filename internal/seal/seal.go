// Package seal keeps the secrets that godepot has to show to others (the password of the directory, the credentials of upstreams) sealed in the database (docs/secrets.md). The key is in a file and
// never in the database; the database holds a check value of it, by which a replica knows whether its file is the right one.
package seal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

const keyLen = 32

var (
	// ErrWrongKey: the key of the file is not the one that the database was sealed with, or there is no file although the database has sealed with a key.
	ErrWrongKey = errors.New("not the seal key of this database")
	// ErrNoKey: the program has no seal key (it is not serving).
	ErrNoKey = errors.New("there is no seal key")
)

// Key is a seal key: AES-256 in GCM mode.
type Key struct {
	aead  cipher.AEAD
	check string // what the database keeps of the key
}

func newKey(raw []byte) (*Key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte("godepot seal check v1"))
	return &Key{aead: aead, check: hex.EncodeToString(mac.Sum(nil))}, nil
}

// ReadKey reads the key of a file: 32 random bytes in base64. The error is fs.ErrNotExist when there is no file.
func ReadKey(path string) (*Key, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own setting (GODEPOT_SEAL_KEY_FILE or the argument of reseal)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != keyLen {
		return nil, fmt.Errorf("%s is not a seal key: it must hold %d bytes in base64", path, keyLen)
	}
	return newKey(raw)
}

// MakeKey writes a new key to the file, readable by its owner only, and reads it. A file that is there is left as it is and read: two programs that start at once get one key.
func MakeKey(path string) (key *Key, made bool, err error) {
	if k, err := ReadKey(path); !errors.Is(err, fs.ErrNotExist) {
		return k, false, err
	}
	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(dir, ".seal-*") // 0600
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
	if err = errors.Join(err, tmp.Sync(), tmp.Close()); err != nil {
		return nil, false, err
	}
	// A link, not a rename: it fails when the file has appeared meanwhile.
	if err := os.Link(tmp.Name(), path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, err
		}
	} else {
		made = true
	}
	key, err = ReadKey(path)
	return key, made, err
}

// stored is the check value of the database, "" when it has sealed with no key yet.
func stored(ctx context.Context, db meta.DB) (string, error) {
	rows, err := db.Query(ctx, meta.S(`SELECT check_value FROM seal WHERE id = 1`))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return meta.Str(rows[0][0]), nil
}

// resetAdvice ends every message of a key that is not there or is not the right one: how to get back to work when it cannot be found again, and what that costs.
const resetAdvice = "If the key is lost for good: stop the replicas and run `godepot reset-seal-key -yes`. It removes the passwords kept in the database (everything else stays) and prints which to give again; then start, and give them (docs/secrets.md)"

func missing(path string) error {
	return fmt.Errorf("%w: the database has secrets sealed with a key and %s is not there. Put the key back (from its backup, or from the replica that made it); a new key is never made for a database that has one. %s", ErrWrongKey, path, resetAdvice)
}

func other(path string) error {
	return fmt.Errorf("%w: %s holds another key than the one the database was sealed with. Every replica needs the same file. %s", ErrWrongKey, path, resetAdvice)
}

// damaged is a file of the key that cannot be read as one, for a database that has a key.
func damaged(path string, err error) error {
	return fmt.Errorf("%w: %w. Put the key back. %s; remove the damaged %s before the start", ErrWrongKey, err, resetAdvice, path)
}

// Open gives the key of the file for this database, as serve does it. A database that has no key yet takes the one of the file, which is made when it is not there.
// A database that has one takes only that one: no key is made then. made says that the file was written now.
func Open(ctx context.Context, db meta.DB, path string, now time.Time) (key *Key, made bool, err error) {
	have, err := stored(ctx, db)
	if err != nil {
		return nil, false, err
	}
	if have == "" {
		if key, made, err = MakeKey(path); err != nil {
			return nil, false, err
		}
		if _, err := db.Exec(ctx, meta.S(`INSERT OR IGNORE INTO seal (id, check_value, at) VALUES (1, ?, ?)`, key.check, now.Unix())); err != nil {
			return nil, false, err
		}
		if have, err = stored(ctx, db); err != nil {
			return nil, false, err
		}
		if have != key.check && made {
			// Another replica was first: the file just written is of no use and would be in the way of the right one.
			_ = os.Remove(path)
		}
	} else if key, err = ReadKey(path); errors.Is(err, fs.ErrNotExist) {
		return nil, false, missing(path)
	} else if err != nil {
		return nil, false, damaged(path, err)
	}
	if have != key.check {
		return nil, false, other(path)
	}
	return key, made, nil
}

// Inspect says how the key of the file and the database stand, and changes nothing (godepot validate). A database with no tables yet has no key.
func Inspect(ctx context.Context, db meta.DB, path string) (string, error) {
	rows, err := db.Query(ctx, meta.S(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'seal'`))
	if err != nil {
		return "", err
	}
	var have string
	if meta.Int(rows[0][0]) > 0 {
		if have, err = stored(ctx, db); err != nil {
			return "", err
		}
	}
	key, err := ReadKey(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && have == "":
		return "not made yet (serve makes it)", nil
	case errors.Is(err, fs.ErrNotExist):
		return "", missing(path)
	case err != nil && have != "":
		return "", damaged(path, err)
	case err != nil:
		return "", err
	case have != "" && have != key.check:
		return "", other(path)
	}
	return "ok", nil
}

// Store reads and writes the sealed secrets of the database.
type Store struct {
	DB  meta.DB
	Now func() time.Time // default time.Now
	Key *Key             // set before the program serves
}

// Info is what may be told of a secret: never the secret.
type Info struct {
	Name  string `json:"name"`
	SetBy string `json:"set_by"`
	SetAt int64  `json:"set_at"`
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// The name is bound to what is sealed: a sealed text that is moved to another name does not open.
func (k *Key) seal(name, value string) string {
	// #nosec G407 -- the nonce is nil because cipher.NewGCMWithRandomNonce makes a random one for each call and puts it before the sealed text; it takes no other
	return "v1:" + base64.StdEncoding.EncodeToString(k.aead.Seal(nil, nil, []byte(value), []byte(name)))
}

func (k *Key) open(name, sealed string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	if err == nil && strings.HasPrefix(sealed, "v1:") {
		var plain []byte
		if plain, err = k.aead.Open(nil, nil, b, []byte(name)); err == nil {
			return string(plain), nil
		}
	}
	return "", fmt.Errorf("the stored secret %q cannot be opened: it was sealed with another key (restart after godepot reseal) or it is damaged: give it again (docs/secrets.md)", name)
}

// current stops a list of statements when the database has got another key meanwhile: nothing is written with an old one.
func current(k *Key) meta.Stmt {
	return meta.Guard("seal_changed", `SELECT 1 FROM seal WHERE id = 1 AND check_value = ?`, k.check)
}

// Set keeps the secret of a name, in the place of the one there was.
func (s *Store) Set(ctx context.Context, name, value, by string) error {
	if s.Key == nil {
		return ErrNoKey
	}
	_, err := s.DB.Exec(ctx, current(s.Key), meta.S(`INSERT INTO secrets (name, sealed, set_by, set_at) VALUES (?, ?, ?, ?) ON CONFLICT (name) DO UPDATE SET sealed = excluded.sealed, set_by = excluded.set_by, set_at = excluded.set_at`,
		name, s.Key.seal(name, value), by, s.now().Unix()))
	if why, refused := meta.RefusedBy(err); refused && why == "seal_changed" {
		return fmt.Errorf("%w: restart this replica with the new one", ErrWrongKey)
	}
	return err
}

// Get is the secret of a name; found is false when there is none.
func (s *Store) Get(ctx context.Context, name string) (value string, found bool, err error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT sealed FROM secrets WHERE name = ?`, name))
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	if s.Key == nil {
		return "", false, ErrNoKey
	}
	if value, err = s.Key.open(name, meta.Str(rows[0][0])); err != nil {
		return "", false, err
	}
	return value, true, nil
}

// Delete forgets the secret of a name; it is no error when there is none.
func (s *Store) Delete(ctx context.Context, name string) error {
	_, err := s.DB.Exec(ctx, meta.S(`DELETE FROM secrets WHERE name = ?`, name))
	return err
}

// List names the secrets there are.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.DB.Query(ctx, meta.S(`SELECT name, set_by, set_at FROM secrets ORDER BY name`))
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, r := range rows {
		out = append(out, Info{Name: meta.Str(r[0]), SetBy: meta.Str(r[1]), SetAt: meta.Int(r[2])})
	}
	return out, nil
}

// Reseal seals every secret again with the new key and makes it the key of the database, all together or not at all. It returns how many there were.
func (s *Store) Reseal(ctx context.Context, next *Key) (int, error) {
	if s.Key == nil {
		return 0, ErrNoKey
	}
	rows, err := s.DB.Query(ctx, meta.S(`SELECT name, sealed FROM secrets`))
	if err != nil {
		return 0, err
	}
	list := []meta.Stmt{current(s.Key)}
	for _, r := range rows {
		name, old := meta.Str(r[0]), meta.Str(r[1])
		value, err := s.Key.open(name, old)
		if err != nil {
			return 0, err
		}
		// A secret that was changed since it was read stops the whole list.
		list = append(list, meta.Guard("secret_changed", `SELECT 1 FROM secrets WHERE name = ? AND sealed = ?`, name, old),
			meta.S(`UPDATE secrets SET sealed = ? WHERE name = ?`, next.seal(name, value), name))
	}
	// And one that was added meanwhile would be left with the old key.
	list = append(list, meta.Guard("secret_changed", `SELECT 1 WHERE (SELECT COUNT(*) FROM secrets) = ?`, len(rows)),
		meta.S(`UPDATE seal SET check_value = ?, at = ? WHERE id = 1`, next.check, s.now().Unix()))
	if _, err := s.DB.Exec(ctx, list...); err != nil {
		return 0, err
	}
	s.Key = next
	return len(rows), nil
}

// Reset removes every sealed secret and the check value of the key: the way back to work when the key is lost (a backup of the database with no key). It returns the names of what was removed, which
// must be given again. Everything else in the database stays.
func (s *Store) Reset(ctx context.Context) ([]string, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, meta.S(`DELETE FROM secrets`), meta.S(`DELETE FROM seal`)); err != nil {
		return nil, err
	}
	names := []string{}
	for _, i := range list {
		names = append(names, i.Name)
	}
	return names, nil
}
