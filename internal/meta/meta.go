// Package meta is the database of godepot: statements in, rows out, and nothing else. Every other package reaches the database through the DB interface of this package, so that a second kind of database
// can be added without touching them (docs/spec.md, D2). The first kind is rqlite (rqlite.go).
//
// The database gives no transaction that a caller can read inside. A change that must be all or nothing is one call of Exec: a list of statements that are applied together or not at all. A condition
// ("only if this blob is live") is a Guard among them: a statement that fails when the condition does not hold, which undoes the whole list. Exec then returns a *Refused that names the guard.
package meta

import (
	"context"
	"errors"
	"fmt"
)

// Stmt is one statement with its parameters.
type Stmt struct {
	SQL  string
	Args []any
	// Why is set on a guard: the word a *Refused carries when this statement stops the list.
	Why string
}

// S makes a statement.
func S(sql string, args ...any) Stmt { return Stmt{SQL: sql, Args: args} }

// Guard makes a statement that stops the whole list unless the condition holds. The condition is the inside of an EXISTS: "SELECT 1 FROM blobs WHERE digest=? AND state='live'".
// Unless makes the opposite. why is a short word for the caller to tell guards apart ("blob_not_live").
func Guard(why, exists string, args ...any) Stmt {
	return Stmt{SQL: "INSERT INTO guard(ok) SELECT 0 WHERE NOT EXISTS (" + exists + ")", Args: args, Why: why}
}

// Unless makes a statement that stops the whole list if the condition holds.
func Unless(why, exists string, args ...any) Stmt {
	return Stmt{SQL: "INSERT INTO guard(ok) SELECT 0 WHERE EXISTS (" + exists + ")", Args: args, Why: why}
}

// Rows is the result of a query. A number is an int64 or a float64, a text a string, a null nil; read cells with Str and Int.
type Rows [][]any

// DB is what godepot asks of a database.
type DB interface {
	// Exec applies the statements together or not at all, and returns the number of rows each changed.
	// A statement that fails because the data say no (a guard, a unique key) gives a *Refused and changes nothing. Any other failure is an *Unavailable; the caller cannot know whether the list was applied
	// and must make its lists safe to send again.
	Exec(ctx context.Context, stmts ...Stmt) ([]int64, error)
	// Query runs one SELECT and sees everything that an Exec that has returned has written, whoever sent it.
	Query(ctx context.Context, s Stmt) (Rows, error)
}

// Refused says that a statement of a list said no and nothing was changed.
type Refused struct {
	Index int    // which statement of the list
	Why   string // the word of the guard, or "" when the statement was not a guard (a unique key, for example)
	Msg   string // the words of the database
}

func (e *Refused) Error() string {
	return fmt.Sprintf("refused by statement %d (%s): %s", e.Index, e.Why, e.Msg)
}

// Unavailable is a failure of the database itself (not reached, no leader, a malformed answer). The caller answers 503.
type Unavailable struct{ Msg string }

func (e *Unavailable) Error() string { return "database: " + e.Msg }

// RefusedBy returns the word of the guard that stopped the list, and whether err is a refusal at all.
func RefusedBy(err error) (string, bool) {
	var r *Refused
	if errors.As(err, &r) {
		return r.Why, true
	}
	return "", false
}

// Str reads a text cell; a null cell is "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Int reads a number cell; a null cell is 0.
func Int(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// Null says whether a cell is null.
func Null(v any) bool { return v == nil }
