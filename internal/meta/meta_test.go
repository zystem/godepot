package meta

import (
	"context"
	"errors"
	"os"
	"testing"
)

func testDB(t *testing.T) *Rqlite {
	t.Helper()
	url := os.Getenv("GODEPOT_RQLITE_URL")
	if url == "" {
		t.Skip("GODEPOT_RQLITE_URL is not set")
	}
	db := NewRqlite(url)
	// Tables of this test only: the tests of other packages use the real ones in the same database at the same time.
	for _, sql := range []string{
		`DROP TABLE IF EXISTS meta_t`, `DROP TABLE IF EXISTS meta_guard`,
		`CREATE TABLE meta_t (a INTEGER PRIMARY KEY, b TEXT)`,
	} {
		if _, err := db.Exec(context.Background(), S(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(context.Background(), S(`CREATE TABLE IF NOT EXISTS guard (ok INTEGER NOT NULL CHECK (ok = 1))`)); err != nil {
		t.Fatal(err)
	}
	return db
}

func count(t *testing.T, db DB) int64 {
	t.Helper()
	rows, err := db.Query(context.Background(), S(`SELECT COUNT(*) FROM meta_t`))
	if err != nil {
		t.Fatal(err)
	}
	return Int(rows[0][0])
}

func TestAListIsAllOrNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// A guard that does not hold undoes what came before it in the list and stops what comes after; the refusal names it.
	_, err := db.Exec(ctx,
		S(`INSERT INTO meta_t VALUES (1, 'one')`),
		Guard("no_seven", `SELECT 1 FROM meta_t WHERE a = ?`, 7),
		S(`INSERT INTO meta_t VALUES (2, 'two')`),
	)
	var r *Refused
	if !errors.As(err, &r) || r.Index != 1 || r.Why != "no_seven" {
		t.Fatalf("got %v", err)
	}
	if n := count(t, db); n != 0 {
		t.Fatalf("%d rows stayed of a list that was refused", n)
	}

	// A guard that holds lets the list through, and sees what the list itself wrote before it.
	n, err := db.Exec(ctx,
		S(`INSERT INTO meta_t VALUES (1, 'one')`),
		Guard("no_one", `SELECT 1 FROM meta_t WHERE a = ?`, 1),
		Unless("has_seven", `SELECT 1 FROM meta_t WHERE a = ?`, 7),
		S(`INSERT INTO meta_t VALUES (2, 'two')`),
		S(`UPDATE meta_t SET b = 'x'`),
	)
	if err != nil || len(n) != 5 || n[4] != 2 {
		t.Fatalf("%v %v", n, err)
	}

	// Unless stops the list when the thing is there.
	_, err = db.Exec(ctx, S(`DELETE FROM meta_t`), Unless("has_one", `SELECT 1 FROM meta_t WHERE a = 1`))
	if why, ok := RefusedBy(err); ok {
		t.Fatalf("the list deleted the row before the guard looked, and was refused all the same: %s", why)
	}
	_, _ = db.Exec(ctx, S(`INSERT INTO meta_t VALUES (1, 'one')`))
	_, err = db.Exec(ctx, Unless("has_one", `SELECT 1 FROM meta_t WHERE a = 1`), S(`DELETE FROM meta_t`))
	if why, _ := RefusedBy(err); why != "has_one" || count(t, db) != 1 {
		t.Fatalf("got %v", err)
	}

	// A unique key is a refusal too, with no word.
	_, err = db.Exec(ctx, S(`INSERT INTO meta_t VALUES (1, 'again')`))
	if why, ok := RefusedBy(err); !ok || why != "" {
		t.Fatalf("got %v", err)
	}
	// A statement that is wrong is not a refusal: the caller must not take it for "the data say no".
	_, err = db.Exec(ctx, S(`INSERT INTO no_such_table VALUES (1)`))
	var u *Unavailable
	if !errors.As(err, &u) {
		t.Fatalf("got %v", err)
	}
	if rows, _ := db.Query(ctx, S(`SELECT COUNT(*) FROM guard`)); Int(rows[0][0]) != 0 {
		t.Fatal("the guard table has rows")
	}
}

func TestCells(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const big = int64(1)<<62 + 12345 // more than a float64 holds exactly
	if _, err := db.Exec(ctx, S(`INSERT INTO meta_t VALUES (?, NULL)`, big), S(`INSERT INTO meta_t VALUES (2, ?)`, `a "quoted" text`)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, S(`SELECT a, b, 1.5 FROM meta_t ORDER BY a`))
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	if Str(rows[0][1]) != `a "quoted" text` || Null(rows[0][1]) {
		t.Fatalf("text: %#v", rows[0][1])
	}
	if Int(rows[1][0]) != big {
		t.Fatalf("a large number lost its last digits: %d", Int(rows[1][0]))
	}
	if !Null(rows[1][1]) || Str(rows[1][1]) != "" {
		t.Fatalf("null: %#v", rows[1][1])
	}
	if f, ok := rows[0][2].(float64); !ok || f != 1.5 {
		t.Fatalf("a fraction: %#v", rows[0][2])
	}
}

func TestADatabaseThatIsNotThere(t *testing.T) {
	db := NewRqlite("http://127.0.0.1:1")
	_, err := db.Exec(context.Background(), S(`SELECT 1`))
	var u *Unavailable
	if !errors.As(err, &u) {
		t.Fatalf("got %v", err)
	}
	if _, ok := RefusedBy(err); ok {
		t.Fatal("a database that is not there is not a refusal")
	}
}

func TestErrorWordsAndHelpers(t *testing.T) {
	if got := (&Refused{Index: 2, Why: "w", Msg: "m"}).Error(); got != "refused by statement 2 (w): m" {
		t.Fatal(got)
	}
	if got := (&Unavailable{Msg: "down"}).Error(); got != "database: down" {
		t.Fatal(got)
	}
	if Int(float64(7)) != 7 || Int("x") != 0 || Int(nil) != 0 || Str(5) != "" {
		t.Fatal("cells")
	}
	if _, ok := RefusedBy(errors.New("x")); ok {
		t.Fatal("a plain error is not a refusal")
	}
	if err := Migrate(context.Background(), NewRqlite("http://127.0.0.1:1")); err == nil {
		t.Fatal("Migrate with no database")
	}
}
