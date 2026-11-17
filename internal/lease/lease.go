// Package lease lets one replica at a time do the background work (docs/spec.md, O2). The lease is a row of the table lease: who holds it and until when. A holder renews it before it ends; a replica
// that dies is replaced after the time has run out. The clocks of the replicas are those of the machines: the lease time must be much longer than the difference between them (the default is two minutes).
package lease

import (
	"context"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// Lease is one named lease held, or tried for, by one replica.
type Lease struct {
	DB     meta.DB
	Name   string        // what the lease is for ("background")
	Holder string        // who asks: unique to the process (a host name and a random part)
	TTL    time.Duration // how long a grant lasts without being renewed
	Now    func() time.Time
}

func (l *Lease) now() int64 {
	if l.Now != nil {
		return l.Now().Unix()
	}
	return time.Now().Unix()
}

// Acquire takes the lease, or renews it when this replica holds it already, and says whether this replica holds it now. The grant lasts TTL from this call.
// A list sent twice is the same as once, so a lost answer of the database is safe: the caller asks again.
func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	now := l.now()
	n, err := l.DB.Exec(ctx,
		meta.S(`INSERT OR IGNORE INTO lease (name, holder, until) VALUES (?, ?, 0)`, l.Name, l.Holder),
		meta.S(`UPDATE lease SET holder = ?, until = ? WHERE name = ? AND (holder = ? OR until <= ?)`,
			l.Holder, now+int64(l.TTL/time.Second), l.Name, l.Holder, now),
	)
	if err != nil {
		return false, err
	}
	return n[1] == 1, nil
}

// Release lets go of the lease at once, if this replica holds it, so that another does not wait for the time to run out (a replica that is stopped calls it).
func (l *Lease) Release(ctx context.Context) error {
	_, err := l.DB.Exec(ctx, meta.S(`UPDATE lease SET until = 0 WHERE name = ? AND holder = ?`, l.Name, l.Holder))
	return err
}
