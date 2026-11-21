package format

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// ReadLog remembers which components were read, to the day, and writes it in batches (docs/spec.md, C4): a read only puts an id in a set in memory, so it never waits for the database; Flush writes the
// set (from a ticker, once a minute at most), and a component that is read again the same day costs nothing.
type ReadLog struct {
	mu   sync.Mutex
	seen map[string]int64 // component id -> the day it was last read, not yet written
	Now  func() time.Time
}

func (l *ReadLog) day() int64 {
	if l.Now != nil {
		return l.Now().Unix() / 86400
	}
	return time.Now().Unix() / 86400
}

// Add notes that the component was read now. nil: not kept.
func (l *ReadLog) Add(component string) {
	if l == nil || component == "" {
		return
	}
	d := l.day()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]int64{}
	}
	if len(l.seen) < 1_000_000 { // a bound: a flood of reads of a million components costs this much memory and no more
		l.seen[component] = d
	}
}

// Flush writes what was noted, a statement for each day and each 400 components.
func (l *ReadLog) Flush(ctx context.Context, db meta.DB) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	seen := l.seen
	l.seen = nil
	l.mu.Unlock()
	byDay := map[int64][]string{}
	for id, d := range seen {
		byDay[d] = append(byDay[d], id)
	}
	var failed []string
	var firstErr error
	for d, ids := range byDay {
		for len(ids) > 0 {
			n := min(len(ids), 400)
			batch := ids[:n]
			ids = ids[n:]
			args := []any{d}
			for _, id := range batch {
				args = append(args, id)
			}
			args = append(args, d)
			if _, err := db.Exec(ctx, meta.S(`UPDATE components SET last_read = ? WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`) AND last_read < ?`, args...)); err != nil {
				failed = append(failed, batch...)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(failed) > 0 { // put back what was not written: the next flush tries again
			l.mu.Lock()
			if l.seen == nil {
				l.seen = map[string]int64{}
			}
			for _, id := range failed {
				if l.seen[id] < d {
					l.seen[id] = d
				}
			}
			l.mu.Unlock()
			failed = nil
		}
	}
	return firstErr
}
