// Package audit writes the audit log (docs/spec.md, A6): one line in the log and one row in the database for every write, delete, change of accounts or tokens, and refusal. The row of a change
// is written in the same transaction as the change (Stmt), so that there is no change without its line; the row of a refusal is written by itself (Record).
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// Results of an event.
const (
	OK      = "ok"
	Refused = "refused"
	Failed  = "failed"
)

// Event is one line of the audit log. Nothing in it is a secret: a token is named by its id.
type Event struct {
	Actor  string // the account; "anonymous" when there was no token
	Token  string // the id of the token
	Remote string // where the request came from
	Action string // "put", "delete", "account.create", "token.create", "token.revoke", "auth", ...
	Repo   string
	Path   string
	Result string // OK, Refused, Failed
	Detail string // why, or what (a digest, a size)
}

// Log writes events.
type Log struct {
	DB  meta.DB
	Log *slog.Logger
	Now func() time.Time
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Stmt is the statement that writes the event, for the Exec of the change it describes. Emit it after the Exec has succeeded.
func (l *Log) Stmt(e Event) meta.Stmt {
	if e.Actor == "" {
		e.Actor = "anonymous"
	}
	return meta.S(`INSERT INTO audit (at, actor, token, remote, action, repo, path, result, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.now().Unix(), e.Actor, e.Token, e.Remote, e.Action, e.Repo, e.Path, e.Result, e.Detail)
}

// Emit writes the line of the log.
func (l *Log) Emit(e Event) {
	if e.Actor == "" {
		e.Actor = "anonymous"
	}
	if l.Log == nil {
		return
	}
	level := slog.LevelInfo
	if e.Result != OK {
		level = slog.LevelWarn
	}
	l.Log.Log(context.Background(), level, "audit", "actor", e.Actor, "token", e.Token, "remote", e.Remote, "action", e.Action, "repo", e.Repo, "path", e.Path, "result", e.Result, "detail", e.Detail)
}

// Record writes an event that has no change to go with: the row and the line. A failure to write the row is logged, not returned: the request that was refused is refused all the same.
func (l *Log) Record(ctx context.Context, e Event) {
	l.Emit(e)
	if _, err := l.DB.Exec(ctx, l.Stmt(e)); err != nil && l.Log != nil {
		l.Log.Error("the audit row could not be written", "err", err)
	}
}

// Row is one row as read back.
type Row struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Token  string `json:"token,omitempty"`
	Remote string `json:"remote,omitempty"`
	Action string `json:"action"`
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path,omitempty"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// List reads the newest rows first: at most limit of them, with an id below before (0: from the newest). action and actor narrow it when they are not "".
func (l *Log) List(ctx context.Context, before int64, limit int, action, actor string) ([]Row, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := l.DB.Query(ctx, meta.S(`SELECT id, at, actor, token, remote, action, repo, path, result, detail FROM audit
		WHERE id < ? AND (? = '' OR action = ?) AND (? = '' OR actor = ?) ORDER BY id DESC LIMIT ?`, before, action, action, actor, actor, limit))
	if err != nil {
		return nil, err
	}
	var out []Row
	for _, r := range rows {
		out = append(out, Row{meta.Int(r[0]), meta.Int(r[1]), meta.Str(r[2]), meta.Str(r[3]), meta.Str(r[4]), meta.Str(r[5]), meta.Str(r[6]), meta.Str(r[7]), meta.Str(r[8]), meta.Str(r[9])})
	}
	return out, nil
}
