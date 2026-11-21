// Package cleanup is the policy that removes what is old (docs/spec.md, C1 to C4). A policy is a list of rules of a repository (settings.Cleanup); it always makes a report of what it would remove and how
// many bytes that frees, and removes only when the repository says enforce. What it removes goes to the trash (internal/trash) and can be put back. A version is the unit: the files of one name and one
// version go together, whatever the format says a version is (Layout.Unit).
package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/trash"
)

// Unitor says which version a file of a repository belongs to; the formats built on raw have it (raw.Raw.Unit).
type Unitor interface {
	Unit(path string) (name, version string, ok bool)
}

// Engine runs policies.
type Engine struct {
	DB    meta.DB
	Trash *trash.Trash
	Audit *audit.Log
	// Units is the format of each repository's way to tell a version: by the name of the format.
	Units map[string]Unitor
	Now   func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Entry is one version that the policy removes (or would).
type Entry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Assets  int    `json:"assets"`
	Bytes   int64  `json:"bytes"`
	// AgeDays is how long ago the newest of its files was written.
	AgeDays int `json:"age_days"`
	// Removed is true once the version is in the trash (an enforced run).
	Removed bool   `json:"removed,omitempty"`
	Error   string `json:"error,omitempty"`

	ids []string
}

// Report is what a policy found, and what it did.
type Report struct {
	Repository string `json:"repository"`
	At         int64  `json:"at"`
	// Enforced: the versions of Remove were moved to the trash. False: a dry report, nothing was changed.
	Enforced bool             `json:"enforced"`
	Policy   settings.Cleanup `json:"policy"`
	// Considered is how many versions there are; Kept how many a keep rule holds.
	Considered int `json:"considered"`
	Kept       int `json:"kept"`
	// Remove are the versions that the policy removes, oldest first.
	Remove []Entry `json:"remove"`
	// Bytes is the sum of the sizes of their files; Freed is what is really freed once the trash is emptied (a blob that something else also names is not).
	Bytes int64 `json:"bytes"`
	Freed int64 `json:"freed"`
	// Note says why nothing is removed, when that is by the policy's own words.
	Note string `json:"note,omitempty"`
}

type unit struct {
	name, version string
	ids           []string
	created       int64
	lastRead      int64
	bytes         int64
	assets        int
	props         map[string]string
	digests       []digest
}

type digest struct {
	d    string
	size int64
}

// Plan makes the report of the repository's policy, changing nothing.
func (e *Engine) Plan(ctx context.Context, repo *settings.Repo) (*Report, error) {
	p := repo.Cleanup
	if p == nil {
		return nil, fmt.Errorf("the repository %s has no cleanup policy", repo.Name)
	}
	u := e.Units[repo.Format]
	if u == nil {
		return nil, fmt.Errorf("the format %s has no cleanup", repo.Format)
	}
	rows, err := e.DB.Query(ctx, meta.S(`SELECT c.id, c.props, c.last_read, a.id, a.path, a.digest, a.size, a.created_at FROM components c JOIN assets a ON a.component_id = c.id WHERE c.repo = ?`, repo.Name))
	if err != nil {
		return nil, err
	}
	units := map[string]*unit{}
	for _, r := range rows {
		name, version, ok := u.Unit(meta.Str(r[4]))
		if !ok {
			continue
		}
		k := name + "\x00" + version
		x := units[k]
		if x == nil {
			x = &unit{name: name, version: version, props: map[string]string{}}
			units[k] = x
		}
		props := map[string]string{}
		_ = json.Unmarshal([]byte(meta.Str(r[1])), &props)
		for pk, pv := range props {
			if _, seen := x.props[pk]; !seen {
				x.props[pk] = pv
			}
		}
		x.lastRead = max(x.lastRead, meta.Int(r[2]))
		x.ids = append(x.ids, meta.Str(r[3]))
		x.digests = append(x.digests, digest{meta.Str(r[5]), meta.Int(r[6])})
		x.bytes += meta.Int(r[6])
		x.assets++
		x.created = max(x.created, meta.Int(r[7]))
	}
	rep := &Report{Repository: repo.Name, At: e.now().Unix(), Policy: *p, Considered: len(units), Remove: []Entry{}}
	if p.RemoveOlderDays == nil {
		rep.Note = "remove_older_days is not set: this policy removes nothing"
	}
	byName := map[string][]*unit{}
	for _, x := range units {
		byName[x.name] = append(byName[x.name], x)
	}
	rank := map[*unit]int{}
	for _, list := range byName {
		sort.Slice(list, func(i, j int) bool {
			if list[i].created != list[j].created {
				return list[i].created > list[j].created
			}
			return list[i].version > list[j].version
		})
		for i, x := range list {
			rank[x] = i
		}
	}
	now := e.now().Unix()
	today := now / 86400
	var remove []*unit
	for _, x := range units {
		if keeps(p, x, rank[x], today) {
			rep.Kept++
			continue
		}
		if p.RemoveOlderDays != nil && now-x.created >= int64(*p.RemoveOlderDays)*86400 {
			remove = append(remove, x)
		}
	}
	sort.Slice(remove, func(i, j int) bool {
		if remove[i].created != remove[j].created {
			return remove[i].created < remove[j].created
		}
		return remove[i].name+remove[i].version < remove[j].name+remove[j].version
	})
	var removedAssets []digest
	for _, x := range remove {
		rep.Remove = append(rep.Remove, Entry{Name: x.name, Version: x.version, Assets: x.assets, Bytes: x.bytes, AgeDays: int((now - x.created) / 86400), ids: x.ids})
		rep.Bytes += x.bytes
		removedAssets = append(removedAssets, x.digests...)
	}
	if rep.Freed, err = e.freed(ctx, removedAssets); err != nil {
		return nil, err
	}
	return rep, nil
}

// keeps says whether a keep rule holds the version.
func keeps(p *settings.Cleanup, x *unit, rank int, today int64) bool {
	if p.KeepNewest > 0 && rank < p.KeepNewest {
		return true
	}
	if p.KeepReadDays > 0 && x.lastRead > 0 && x.lastRead >= today-int64(p.KeepReadDays) {
		return true
	}
	for _, pat := range p.Keep {
		if auth.Match(pat, x.name) {
			return true
		}
	}
	if len(p.KeepProps) > 0 {
		all := true
		for k, v := range p.KeepProps {
			if x.props[k] != v {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// freed counts the bytes that removing these assets frees: a blob counts when every reference to it is of one of these assets (so a blob that another version, another repository or the trash also holds does
// not).
func (e *Engine) freed(ctx context.Context, assets []digest) (int64, error) {
	removed := map[string]int{}
	size := map[string]int64{}
	for _, a := range assets {
		removed[a.d]++
		size[a.d] = a.size
	}
	var ds []string
	for d := range removed {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	var total int64
	for len(ds) > 0 {
		n := min(len(ds), 300)
		batch := ds[:n]
		ds = ds[n:]
		args := make([]any, len(batch))
		for i, d := range batch {
			args[i] = d
		}
		rows, err := e.DB.Query(ctx, meta.S(`SELECT digest, COUNT(*) FROM refs WHERE digest IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`) GROUP BY digest`, args...))
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if d := meta.Str(r[0]); int(meta.Int(r[1])) == removed[d] {
				total += size[d]
			}
		}
	}
	return total, nil
}

// Run makes the report and, when enforce is true, moves what it names to the trash. The report is kept as the last of the repository. enforce is the caller's word: the settings' own enforce is what the
// background asks for.
func (e *Engine) Run(ctx context.Context, repo *settings.Repo, enforce bool, actor string) (*Report, error) {
	rep, err := e.Plan(ctx, repo)
	if err != nil {
		return nil, err
	}
	if enforce {
		rep.Enforced = true
		for i := range rep.Remove {
			en := &rep.Remove[i]
			if _, err := e.Trash.Move(ctx, repo.Name, en.ids, en.Name, en.Version, actor, "cleanup"); err != nil {
				en.Error = err.Error()
				continue
			}
			en.Removed = true
		}
	}
	body, _ := json.Marshal(rep)
	enforced := 0
	if enforce {
		enforced = 1
	}
	if _, err := e.DB.Exec(ctx, meta.S(`INSERT INTO cleanup_reports (repo, at, enforced, body) VALUES (?, ?, ?, ?) ON CONFLICT (repo) DO UPDATE SET at = excluded.at, enforced = excluded.enforced, body = excluded.body`,
		repo.Name, rep.At, enforced, string(body))); err != nil {
		return rep, err
	}
	removed := 0
	for _, en := range rep.Remove {
		if en.Removed {
			removed++
		}
	}
	e.Audit.Record(ctx, audit.Event{Actor: actor, Action: "cleanup", Repo: repo.Name, Result: audit.OK, Detail: fmt.Sprintf("enforced=%v: %d versions named, %d removed, %d bytes, %d freed", enforce, len(rep.Remove), removed, rep.Bytes, rep.Freed)})
	return rep, nil
}

// Last reads the last report of a repository (nil: there is none).
func (e *Engine) Last(ctx context.Context, repo string) (*Report, error) {
	rows, err := e.DB.Query(ctx, meta.S(`SELECT body FROM cleanup_reports WHERE repo = ?`, repo))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal([]byte(meta.Str(rows[0][0])), &r); err != nil {
		return nil, err
	}
	return &r, nil
}
