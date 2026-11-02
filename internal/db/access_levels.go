package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Access by levels (user, 2 Nov 2026: "lets use groups for access controls, right now access
// control is so complicated … set engineer to read only all and allow a certain group"; picked
// L3 · G3 · R1 · U1 · A1 · V1, "yes to all").
//
// A grant gives a level — view, operate or manage — on a scope to a role (everyone with it)
// or to one person, optionally until a date. Scopes: every device ("all"), a device state
// (lab, setting_up, in_service), a group, a venue, or the venues of the person's open setup
// visits ("visit", Field). A team member's level on a device is the highest any unexpired
// grant of their role or their own gives; nothing subtracts. Owners and Staff keep their venue
// walls (access_grants rows read by decideCustomer), and shell and firmware updates stay with
// super admins whatever the level.
//
// The role baselines that reproduce the roles replan (30 Oct) are seeded once, with
// engineers' QA access added (the only access anyone gains); the old allow/deny rules of
// super admins and the unused access profiles are dropped once. Both run under a marker
// row in access_seed (inserted in this migration's transaction, so "at = now()" means "just
// now") so a re-run at every start (and the second server copy) changes nothing, and a
// baseline someone removed on the Access page stays removed.
const accessLevelsSchema = `
CREATE TABLE IF NOT EXISTS access_levels (
	id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	subject_type TEXT NOT NULL CHECK (subject_type IN ('role', 'user')),
	subject      TEXT NOT NULL,
	scope_type   TEXT NOT NULL CHECK (scope_type IN ('all', 'state', 'group', 'venue', 'visit')),
	scope_id     UUID,
	state        TEXT NOT NULL DEFAULT '',
	level        TEXT NOT NULL CHECK (level IN ('view', 'operate', 'manage')),
	until        TIMESTAMPTZ,
	created_by   TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS access_levels_one ON access_levels
	(subject_type, subject, scope_type, COALESCE(scope_id, '00000000-0000-0000-0000-000000000000'::uuid), state);
CREATE TABLE IF NOT EXISTS access_seed (k TEXT PRIMARY KEY, at TIMESTAMPTZ NOT NULL DEFAULT now());
WITH s AS (INSERT INTO access_seed (k) VALUES ('levels-v1') ON CONFLICT DO NOTHING RETURNING k)
INSERT INTO access_levels (subject_type, subject, scope_type, state, level, created_by)
SELECT 'role', v.role, v.scope, v.state, v.level, 'seed' FROM s CROSS JOIN (VALUES
	('fleet_admin',   'all',   '',           'manage'),
	('fleet_manager', 'state', 'setting_up', 'manage'),
	('fleet_manager', 'state', 'in_service', 'manage'),
	('operator',      'state', 'in_service', 'manage'),
	('support',       'state', 'in_service', 'manage'),
	('viewer',        'state', 'in_service', 'view'),
	('field',         'visit', '',           'manage'),
	('engineer',      'all',   '',           'view'),
	('engineer',      'state', 'lab',        'operate')
) AS v(role, scope, state, level)
ON CONFLICT DO NOTHING;
WITH s AS (SELECT 1 FROM access_seed WHERE k = 'levels-v1' AND at = now()),
     qa AS (SELECT id FROM groups WHERE name = 'QA' ORDER BY created_at LIMIT 1)
INSERT INTO access_levels (subject_type, subject, scope_type, scope_id, level, created_by)
SELECT 'role', 'engineer', 'group', qa.id, 'operate', 'seed' FROM s, qa
ON CONFLICT DO NOTHING;
WITH s AS (INSERT INTO access_seed (k) VALUES ('drop-rules-v1') ON CONFLICT DO NOTHING RETURNING k),
     del AS (DELETE FROM access_grants g USING users u, s WHERE u.id = g.user_id AND u.role = 'admin' RETURNING g.id)
UPDATE access_grants SET profile_id = NULL WHERE profile_id IS NOT NULL AND EXISTS (SELECT 1 FROM s);
DELETE FROM access_profiles WHERE EXISTS (SELECT 1 FROM access_seed WHERE k = 'drop-rules-v1' AND at = now());
`

// Access levels, low to high. 0 is none.
const (
	LevelNone    = 0
	LevelView    = 1
	LevelOperate = 2
	LevelManage  = 3
)

// LevelOf turns a stored level name into its rank.
func LevelOf(name string) int {
	switch name {
	case "view":
		return LevelView
	case "operate":
		return LevelOperate
	case "manage":
		return LevelManage
	}
	return LevelNone
}

// LevelName is the stored name of a level rank.
func LevelName(l int) string {
	switch l {
	case LevelView:
		return "view"
	case LevelOperate:
		return "operate"
	case LevelManage:
		return "manage"
	}
	return ""
}

// AccessLevel is one grant: a level on a scope for a role or a person.
type AccessLevel struct {
	ID          uuid.UUID
	SubjectType string // role | user
	Subject     string // role key or username
	ScopeType   string // all | state | group | venue | visit
	ScopeID     *uuid.UUID
	State       string
	Level       string
	Until       *time.Time
	CreatedBy   string
	CreatedAt   time.Time
	ScopeName   string // group or venue name (joined)
	Devices     int    // devices the scope covers now (all/state/group/venue)
}

// Expired reports whether the grant has ended.
func (a AccessLevel) Expired(now time.Time) bool { return a.Until != nil && !a.Until.After(now) }

// Rank is the grant's level rank.
func (a AccessLevel) Rank() int { return LevelOf(a.Level) }

const accessLevelSelect = `
SELECT a.id, a.subject_type, a.subject, a.scope_type, a.scope_id, a.state, a.level, a.until,
       a.created_by, a.created_at, COALESCE(g.name, r.name, ''),
       CASE a.scope_type
         WHEN 'all' THEN (SELECT count(*) FROM devices WHERE NOT hidden)
         WHEN 'group' THEN (SELECT count(*) FROM device_groups dg JOIN devices d ON d.id = dg.device_id WHERE dg.group_id = a.scope_id AND NOT d.hidden)
         WHEN 'venue' THEN (SELECT count(*) FROM devices WHERE restaurant_id = a.scope_id AND NOT hidden)
         WHEN 'state' THEN (SELECT count(*) FROM devices WHERE NOT hidden AND CASE a.state
             WHEN 'lab' THEN restaurant_id IS NULL
             WHEN 'setting_up' THEN restaurant_id IS NOT NULL AND handed_over_at IS NULL
             WHEN 'in_service' THEN restaurant_id IS NOT NULL AND handed_over_at IS NOT NULL END)
         ELSE 0 END
FROM access_levels a
LEFT JOIN groups g ON a.scope_type = 'group' AND g.id = a.scope_id
LEFT JOIN restaurants r ON a.scope_type = 'venue' AND r.id = a.scope_id`

func (d *DB) queryAccessLevels(ctx context.Context, where string, args ...any) ([]AccessLevel, error) {
	rows, err := d.pool.Query(ctx, accessLevelSelect+" "+where+" ORDER BY a.subject_type, a.subject, a.created_at", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessLevel
	for rows.Next() {
		var a AccessLevel
		if err := rows.Scan(&a.ID, &a.SubjectType, &a.Subject, &a.ScopeType, &a.ScopeID, &a.State, &a.Level, &a.Until,
			&a.CreatedBy, &a.CreatedAt, &a.ScopeName, &a.Devices); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccessLevelsFor is every grant (expired included) that applies to a person: their role's
// and their own. The evaluator skips expired ones.
func (d *DB) AccessLevelsFor(ctx context.Context, role, username string) ([]AccessLevel, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, subject_type, subject, scope_type, scope_id, state, level, until
		FROM access_levels WHERE (subject_type = 'role' AND subject = $1) OR (subject_type = 'user' AND subject = $2)`, role, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessLevel
	for rows.Next() {
		var a AccessLevel
		if err := rows.Scan(&a.ID, &a.SubjectType, &a.Subject, &a.ScopeType, &a.ScopeID, &a.State, &a.Level, &a.Until); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAccessLevels is every grant with its scope's name and size, for the Access page.
func (d *DB) ListAccessLevels(ctx context.Context) ([]AccessLevel, error) {
	return d.queryAccessLevels(ctx, "")
}

// UserAccessLevels is one person's own grants (not their role's).
func (d *DB) UserAccessLevels(ctx context.Context, username string) ([]AccessLevel, error) {
	return d.queryAccessLevels(ctx, "WHERE a.subject_type = 'user' AND a.subject = $1", username)
}

// RoleAccessLevels is one role's grants.
func (d *DB) RoleAccessLevels(ctx context.Context, role string) ([]AccessLevel, error) {
	return d.queryAccessLevels(ctx, "WHERE a.subject_type = 'role' AND a.subject = $1", role)
}

// GetAccessLevel loads one grant.
func (d *DB) GetAccessLevel(ctx context.Context, id uuid.UUID) (*AccessLevel, error) {
	out, err := d.queryAccessLevels(ctx, "WHERE a.id = $1", id)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("access level %s not found", id)
	}
	return &out[0], nil
}

// SetAccessLevel gives (or changes) a grant: the same subject and scope hold one level, so
// giving it again replaces the level and the end date.
func (d *DB) SetAccessLevel(ctx context.Context, a AccessLevel) error {
	if a.SubjectType != "role" && a.SubjectType != "user" {
		return fmt.Errorf("subject type %q", a.SubjectType)
	}
	if LevelOf(a.Level) == LevelNone {
		return fmt.Errorf("level %q", a.Level)
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO access_levels (subject_type, subject, scope_type, scope_id, state, level, until, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (subject_type, subject, scope_type, COALESCE(scope_id, '00000000-0000-0000-0000-000000000000'::uuid), state)
		DO UPDATE SET level = EXCLUDED.level, until = EXCLUDED.until, created_by = EXCLUDED.created_by, created_at = now()`,
		a.SubjectType, a.Subject, a.ScopeType, a.ScopeID, a.State, a.Level, a.Until, a.CreatedBy)
	return err
}

// DeleteAccessLevel removes a grant.
func (d *DB) DeleteAccessLevel(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM access_levels WHERE id = $1`, id)
	return err
}

// DeleteUserAccessLevels removes a person's own grants and their Where (account deleted or role changed to
// a customer one).
func (d *DB) DeleteUserAccessLevels(ctx context.Context, username string) error {
	if _, err := d.pool.Exec(ctx, `DELETE FROM access_levels WHERE subject_type = 'user' AND subject = $1`, username); err != nil {
		return err
	}
	return d.ClearAccessWhere(ctx, username)
}
