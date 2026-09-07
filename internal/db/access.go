package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AccessGrant is one row of access_grants: an allow or deny of some actions on
// one scope (whole fleet, a venue, a group, or a single device) for one user.
type AccessGrant struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"user_id"`
	Effect    string     `json:"effect"`     // "allow" | "deny"
	ScopeType string     `json:"scope_type"` // "all" | "restaurant" | "group" | "device"
	ScopeID   *uuid.UUID `json:"scope_id"`   // restaurant / group / device id; nil for "all"
	ScopeName string     `json:"scope_name"` // venue or group name, device serial (joined)
	Actions   []string   `json:"actions"`    // action keys, or ["*"]
	Note      string     `json:"note"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedBy string     `json:"created_by"` // username of the granter
	CreatedAt time.Time  `json:"created_at"`
	// Expired is set by ListAccessGrants(includeExpired=true) for display.
	Expired bool `json:"expired"`
}

// Has reports whether the grant names the action (or "*").
func (g AccessGrant) Has(action string) bool {
	for _, a := range g.Actions {
		if a == "*" || a == action {
			return true
		}
	}
	return false
}

const grantSelect = `
SELECT g.id, g.user_id, g.effect, g.scope_type,
       COALESCE(g.restaurant_id, g.group_id, g.device_id),
       COALESCE(r.name, gr.name, d.serial_number, ''),
       g.actions, g.note, g.expires_at, g.created_by, g.created_at,
       (g.expires_at IS NOT NULL AND g.expires_at <= NOW())
FROM access_grants g
LEFT JOIN restaurants r ON r.id = g.restaurant_id
LEFT JOIN groups gr ON gr.id = g.group_id
LEFT JOIN devices d ON d.id = g.device_id`

func scanGrants(rows pgx.Rows) ([]AccessGrant, error) {
	defer rows.Close()
	var out []AccessGrant
	for rows.Next() {
		var g AccessGrant
		if err := rows.Scan(&g.ID, &g.UserID, &g.Effect, &g.ScopeType, &g.ScopeID, &g.ScopeName, &g.Actions, &g.Note, &g.ExpiresAt, &g.CreatedBy, &g.CreatedAt, &g.Expired); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListAccessGrants returns a user's grants, oldest first. Expired grants are
// left out unless includeExpired (the editor shows them greyed).
func (d *DB) ListAccessGrants(ctx context.Context, userID uuid.UUID, includeExpired bool) ([]AccessGrant, error) {
	q := grantSelect + ` WHERE g.user_id = $1`
	if !includeExpired {
		q += ` AND (g.expires_at IS NULL OR g.expires_at > NOW())`
	}
	rows, err := d.pool.Query(ctx, q+` ORDER BY g.created_at, g.id`, userID)
	if err != nil {
		return nil, err
	}
	return scanGrants(rows)
}

// GetAccessGrant loads one grant (any state).
func (d *DB) GetAccessGrant(ctx context.Context, id uuid.UUID) (*AccessGrant, error) {
	rows, err := d.pool.Query(ctx, grantSelect+` WHERE g.id = $1`, id)
	if err != nil {
		return nil, err
	}
	gs, err := scanGrants(rows)
	if err != nil || len(gs) == 0 {
		return nil, err
	}
	return &gs[0], nil
}

// AccessGrantsForDevice lists every live grant whose scope contains the device
// (its own row, its venue, its groups, or the whole fleet), for "who has access".
func (d *DB) AccessGrantsForDevice(ctx context.Context, deviceID uuid.UUID) ([]AccessGrant, error) {
	rows, err := d.pool.Query(ctx, grantSelect+`
		WHERE (g.expires_at IS NULL OR g.expires_at > NOW()) AND (
		      g.scope_type = 'all'
		   OR g.device_id = $1
		   OR g.restaurant_id = (SELECT restaurant_id FROM devices WHERE id = $1)
		   OR g.group_id IN (SELECT group_id FROM device_groups WHERE device_id = $1))
		ORDER BY g.user_id, g.created_at`, deviceID)
	if err != nil {
		return nil, err
	}
	return scanGrants(rows)
}

func scopeColumns(g AccessGrant) (rid, gid, did *uuid.UUID, err error) {
	switch g.ScopeType {
	case "all":
		if g.ScopeID != nil {
			return nil, nil, nil, fmt.Errorf("scope 'all' takes no id")
		}
	case "restaurant":
		rid = g.ScopeID
	case "group":
		gid = g.ScopeID
	case "device":
		did = g.ScopeID
	default:
		return nil, nil, nil, fmt.Errorf("unknown scope type %q", g.ScopeType)
	}
	if g.ScopeType != "all" && g.ScopeID == nil {
		return nil, nil, nil, fmt.Errorf("scope %s needs an id", g.ScopeType)
	}
	return
}

// AddAccessGrant inserts one grant and returns it with its id and names filled.
func (d *DB) AddAccessGrant(ctx context.Context, g AccessGrant) (*AccessGrant, error) {
	rid, gid, did, err := scopeColumns(g)
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	err = d.pool.QueryRow(ctx, `INSERT INTO access_grants (user_id, effect, scope_type, restaurant_id, group_id, device_id, actions, note, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		g.UserID, g.Effect, g.ScopeType, rid, gid, did, g.Actions, g.Note, g.ExpiresAt, g.CreatedBy).Scan(&id)
	if err != nil {
		return nil, err
	}
	return d.GetAccessGrant(ctx, id)
}

// UpdateAccessGrant rewrites effect, actions, note and expiry of one grant. The
// scope is fixed: changing where a grant applies is a delete + add, so the audit
// trail keeps both.
func (d *DB) UpdateAccessGrant(ctx context.Context, id uuid.UUID, effect string, actions []string, note string, expiresAt *time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE access_grants SET effect=$2, actions=$3, note=$4, expires_at=$5 WHERE id=$1`, id, effect, actions, note, expiresAt)
	return err
}

func (d *DB) DeleteAccessGrant(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM access_grants WHERE id=$1`, id)
	return err
}

// DeleteAccessGrantsForUser clears every grant (used when a role change makes
// them meaningless, e.g. promoting to admin).
func (d *DB) DeleteAccessGrantsForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM access_grants WHERE user_id=$1`, userID)
	return err
}

// SweepExpiredAccessGrants deletes grants past their expiry and returns them so
// the caller can audit each one.
func (d *DB) SweepExpiredAccessGrants(ctx context.Context) ([]AccessGrant, error) {
	rows, err := d.pool.Query(ctx, grantSelect+` WHERE g.expires_at IS NOT NULL AND g.expires_at <= NOW()`)
	if err != nil {
		return nil, err
	}
	gone, err := scanGrants(rows)
	if err != nil || len(gone) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(gone))
	for i, g := range gone {
		ids[i] = g.ID
	}
	_, err = d.pool.Exec(ctx, `DELETE FROM access_grants WHERE id = ANY($1)`, ids)
	return gone, err
}

// AccessGrantUserSummary is one row of the access overview: how many grants a
// user has and whether any of them is a sensitive one.
type AccessGrantCount struct {
	Grants    int
	Sensitive bool
}

// CountAccessGrants returns live grant counts per user id.
func (d *DB) CountAccessGrants(ctx context.Context, sensitive []string) (map[uuid.UUID]AccessGrantCount, error) {
	rows, err := d.pool.Query(ctx, `SELECT user_id, COUNT(*), bool_or(effect = 'allow' AND actions && $1)
		FROM access_grants WHERE expires_at IS NULL OR expires_at > NOW() GROUP BY user_id`, sensitive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]AccessGrantCount{}
	for rows.Next() {
		var id uuid.UUID
		var c AccessGrantCount
		if err := rows.Scan(&id, &c.Grants, &c.Sensitive); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// MigrateAccessRules moves legacy JSONB rules (users.access->'rules') into
// access_grants once, then strips them from the JSON. Rules pointing at a venue
// or group that no longer exists are dropped and logged. Idempotent: a user with
// no rules in JSON is skipped.
func (d *DB) MigrateAccessRules(ctx context.Context) (moved, dropped int, err error) {
	rows, err := d.pool.Query(ctx, `SELECT id, username, access FROM users WHERE access ? 'rules' AND jsonb_array_length(access->'rules') > 0`)
	if err != nil {
		return 0, 0, err
	}
	type pending struct {
		id       uuid.UUID
		username string
		pol      AccessPolicy
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var raw []byte
		if err := rows.Scan(&p.id, &p.username, &raw); err != nil {
			rows.Close()
			return 0, 0, err
		}
		_ = json.Unmarshal(raw, &p.pol)
		todo = append(todo, p)
	}
	rows.Close()
	for _, p := range todo {
		for _, r := range p.pol.Rules {
			g := AccessGrant{UserID: p.id, Effect: r.Effect, ScopeType: r.ScopeType, Actions: r.Actions, Note: "migrated from the previous access editor", CreatedBy: "system"}
			if g.ScopeType == "" {
				g.ScopeType = "all"
			}
			if g.ScopeType != "all" {
				sid, perr := uuid.Parse(r.ScopeID)
				if perr != nil {
					dropped++
					continue
				}
				g.ScopeID = &sid
			}
			if _, err := d.AddAccessGrant(ctx, g); err != nil {
				// most likely a foreign key miss: the venue/group is gone
				log.Printf("[access] migrate rule for %s (%s %v %v): %v", p.username, r.Effect, r.ScopeType, r.Actions, err)
				dropped++
				continue
			}
			moved++
		}
		if _, err := d.pool.Exec(ctx, `UPDATE users SET access = access - 'rules' WHERE id = $1`, p.id); err != nil {
			return moved, dropped, err
		}
	}
	return moved, dropped, nil
}
