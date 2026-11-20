package format

import (
	"context"
	"strings"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/settings"
)

// Resolve finds the repository that answers for a name in a group (docs/spec.md, R2 and R3): it is the first member, in order, that has the name, and the others are not asked, so that the versions of
// one name are never mixed from two members. A hosted member that has the name always answers, whatever its place, and a name that a hosted member has published, or that a claim of the group names, is
// never asked of a proxy: a name that is claimed and that no hosted member has is not found (nil). has says whether one repository has the name; for a proxy it may ask the upstream, and an error is
// an error of the whole request, never a reason to try the next member (an upstream that is down must not let a later one answer for it).
func Resolve(ctx context.Context, db meta.DB, set *settings.Settings, group *settings.Repo, name string, has func(*settings.Repo) (bool, error)) (*settings.Repo, error) {
	leaves := set.Leaves(group.Name)
	for _, l := range leaves {
		if l.Kind != "hosted" {
			continue
		}
		if ok, err := has(l); err != nil || ok {
			if err != nil {
				return nil, err
			}
			return l, nil
		}
	}
	claimed, err := Claimed(ctx, db, set, group.Name, name)
	if err != nil || claimed {
		return nil, err
	}
	for _, l := range leaves {
		if l.Kind != "proxy" {
			continue
		}
		if ok, err := has(l); err != nil || ok {
			if err != nil {
				return nil, err
			}
			return l, nil
		}
	}
	return nil, nil
}

// Claimed says whether the name belongs to the hosted members of the group: it was published to one of them, or a prefix of the group's claims begins it.
func Claimed(ctx context.Context, db meta.DB, set *settings.Settings, group, name string) (bool, error) {
	for _, p := range set.ClaimPrefixes(group) {
		if strings.HasPrefix(name, p) {
			return true, nil
		}
	}
	rows, err := db.Query(ctx, meta.S(`SELECT 1 FROM claims WHERE grp = ? AND name = ?`, group, name))
	return len(rows) > 0, err
}

// ClaimStmts are the statements that claim a name in every group that holds the repository: they go in the transaction that publishes the name.
func ClaimStmts(set *settings.Settings, repo, name string) []meta.Stmt {
	if set == nil {
		return nil
	}
	var out []meta.Stmt
	for _, g := range set.GroupsOf(repo) {
		out = append(out, meta.S(`INSERT OR IGNORE INTO claims (grp, name) VALUES (?, ?)`, g, name))
	}
	return out
}
