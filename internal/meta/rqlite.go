package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Rqlite is a DB kept in one rqlite (http://host:4001). It is safe for concurrent use.
type Rqlite struct {
	base string
	http *http.Client
}

// NewRqlite makes a client for the base URL.
func NewRqlite(base string) *Rqlite {
	return &Rqlite{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// refusals are the beginnings of the errors with which SQLite says that the data, not the database, refuse a statement.
var refusals = []string{"CHECK constraint failed", "UNIQUE constraint failed", "NOT NULL constraint failed", "FOREIGN KEY constraint failed"}

func (c *Rqlite) post(ctx context.Context, path string, stmts []Stmt) ([]json.RawMessage, error) {
	enc := make([]any, len(stmts))
	for i, s := range stmts {
		enc[i] = append([]any{s.SQL}, s.Args...)
	}
	body, err := json.Marshal(enc)
	if err != nil {
		return nil, fmt.Errorf("a statement cannot be sent: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, &Unavailable{err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Unavailable{err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &Unavailable{err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &Unavailable{fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data)}
	}
	var out struct {
		Results []json.RawMessage `json:"results"`
		Error   string            `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, &Unavailable{err.Error()}
	}
	// 200 is not success: during a change of leader rqlite answers {"results":[],"error":"..."} with 200.
	if out.Error != "" {
		return nil, &Unavailable{out.Error}
	}
	// In a transaction the results end at the statement that failed, so its index is the place of the error.
	for i, r := range out.Results {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(r, &e) != nil || e.Error == "" {
			continue
		}
		for _, p := range refusals {
			if strings.HasPrefix(e.Error, p) && i < len(stmts) {
				return nil, &Refused{Index: i, Why: stmts[i].Why, Msg: e.Error}
			}
		}
		return nil, &Unavailable{fmt.Sprintf("statement %d: %s", i, e.Error)}
	}
	return out.Results, nil
}

// Exec implements DB.
func (c *Rqlite) Exec(ctx context.Context, stmts ...Stmt) ([]int64, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	res, err := c.post(ctx, "/db/execute?transaction", stmts)
	if err != nil {
		return nil, err
	}
	if len(res) != len(stmts) {
		return nil, &Unavailable{fmt.Sprintf("%d results for %d statements", len(res), len(stmts))}
	}
	n := make([]int64, len(res))
	for i, r := range res {
		var e struct {
			Rows int64 `json:"rows_affected"`
		}
		_ = json.Unmarshal(r, &e)
		n[i] = e.Rows
	}
	return n, nil
}

// Query implements DB. The read is at the level "strong": a round of consensus, so that it sees what any replica has written.
func (c *Rqlite) Query(ctx context.Context, s Stmt) (Rows, error) {
	res, err := c.post(ctx, "/db/query?level=strong", []Stmt{s})
	if err != nil {
		return nil, err
	}
	if len(res) != 1 {
		return nil, &Unavailable{"no result"}
	}
	var r struct {
		Values [][]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(res[0], &r); err != nil {
		return nil, &Unavailable{err.Error()}
	}
	rows := make(Rows, len(r.Values))
	for i, row := range r.Values {
		rows[i] = make([]any, len(row))
		for j, cell := range row {
			rows[i][j] = decodeCell(cell)
		}
	}
	return rows, nil
}

// decodeCell keeps a whole number whole: a size or a time above 2^53 must not pass through a float64.
func decodeCell(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}
