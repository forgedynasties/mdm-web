package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A person's own "Where" (picked B, "where on person", 2 Nov 2026): access only adds, so a
// role's default scope ("engineers operate Lab devices") could not be narrowed for one
// person. A Where replaces the role's grants for that person: their role still decides
// what they may do (its highest level), the Where decides on which devices (Lab, every
// device, deployed = at a venue, or some groups), and Elsewhere what they get on the rest
// (View only, or Hidden). Their own grants still add on top. No row = the role's defaults.
//
// A table of its own (one row per person) rather than access_levels rows: it isn't a grant
// that adds, it swaps the role's grants out. CREATE … IF NOT EXISTS, so the second server
// copy and every restart run it harmlessly.
const accessWhereSchema = `
CREATE TABLE IF NOT EXISTS access_where (
	username   TEXT PRIMARY KEY,
	scope      TEXT NOT NULL CHECK (scope IN ('lab', 'all', 'deployed', 'groups')),
	group_ids  UUID[] NOT NULL DEFAULT '{}',
	elsewhere  TEXT NOT NULL DEFAULT 'view' CHECK (elsewhere IN ('view', 'hidden')),
	set_by     TEXT NOT NULL DEFAULT '',
	set_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// AccessWhere is one person's Where.
type AccessWhere struct {
	Username  string
	Scope     string // lab | all | deployed | groups
	GroupIDs  []uuid.UUID
	Elsewhere string // view | hidden
	SetBy     string
	SetAt     time.Time
}

// ValidWhereScope reports whether s is a Where choice.
func ValidWhereScope(s string) bool {
	return s == "lab" || s == "all" || s == "deployed" || s == "groups"
}

// AccessWhereFor is the person's Where, or nil when they follow their role.
func (d *DB) AccessWhereFor(ctx context.Context, username string) (*AccessWhere, error) {
	var w AccessWhere
	err := d.pool.QueryRow(ctx, `SELECT username, scope, group_ids, elsewhere, set_by, set_at
		FROM access_where WHERE username = $1`, username).Scan(&w.Username, &w.Scope, &w.GroupIDs, &w.Elsewhere, &w.SetBy, &w.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ListAccessWhere is every person's Where, by username.
func (d *DB) ListAccessWhere(ctx context.Context) (map[string]AccessWhere, error) {
	rows, err := d.pool.Query(ctx, `SELECT username, scope, group_ids, elsewhere, set_by, set_at FROM access_where`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AccessWhere{}
	for rows.Next() {
		var w AccessWhere
		if err := rows.Scan(&w.Username, &w.Scope, &w.GroupIDs, &w.Elsewhere, &w.SetBy, &w.SetAt); err != nil {
			return nil, err
		}
		out[w.Username] = w
	}
	return out, rows.Err()
}

// SetAccessWhere sets (or replaces) a person's Where.
func (d *DB) SetAccessWhere(ctx context.Context, w AccessWhere) error {
	if !ValidWhereScope(w.Scope) {
		return fmt.Errorf("where %q", w.Scope)
	}
	if w.Scope == "groups" && len(w.GroupIDs) == 0 {
		return fmt.Errorf("where groups: no group")
	}
	if w.Scope != "groups" {
		w.GroupIDs = nil
	}
	if w.Elsewhere != "hidden" {
		w.Elsewhere = "view"
	}
	if w.GroupIDs == nil {
		w.GroupIDs = []uuid.UUID{}
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO access_where (username, scope, group_ids, elsewhere, set_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (username) DO UPDATE SET scope = EXCLUDED.scope, group_ids = EXCLUDED.group_ids,
			elsewhere = EXCLUDED.elsewhere, set_by = EXCLUDED.set_by, set_at = now()`,
		w.Username, w.Scope, w.GroupIDs, w.Elsewhere, w.SetBy)
	return err
}

// ClearAccessWhere puts a person back on their role's defaults.
func (d *DB) ClearAccessWhere(ctx context.Context, username string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM access_where WHERE username = $1`, username)
	return err
}

// EffectiveLevels applies a person's Where to the grants that apply to them (their role's
// and their own, as AccessLevelsFor returns them). With no Where they are returned as they
// are. With one, the role's grants are replaced by grants of SubjectType "where": the role's
// highest unexpired level on the Where's scope, View on every device when Elsewhere is
// "view"; the person's own grants stay.
func EffectiveLevels(rows []AccessLevel, w *AccessWhere, now time.Time) []AccessLevel {
	if w == nil {
		return rows
	}
	top := LevelNone
	var out []AccessLevel
	for _, g := range rows {
		if g.SubjectType == "role" {
			if !g.Expired(now) && g.Rank() > top {
				top = g.Rank()
			}
			continue
		}
		out = append(out, g)
	}
	mk := func(scope, state string, id *uuid.UUID, level int) AccessLevel {
		return AccessLevel{SubjectType: "where", Subject: w.Username, ScopeType: scope, State: state, ScopeID: id, Level: LevelName(level)}
	}
	if top > LevelNone {
		switch w.Scope {
		case "all":
			out = append(out, mk("all", "", nil, top))
		case "lab":
			out = append(out, mk("state", StateLab, nil, top))
		case "deployed":
			out = append(out, mk("state", StateSettingUp, nil, top), mk("state", StateInService, nil, top))
		case "groups":
			for i := range w.GroupIDs {
				id := w.GroupIDs[i]
				out = append(out, mk("group", "", &id, top))
			}
		}
	}
	if top > LevelNone && w.Elsewhere != "hidden" && w.Scope != "all" {
		out = append(out, mk("all", "", nil, LevelView))
	}
	return out
}
