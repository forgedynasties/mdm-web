package db

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Per-user access policy ──────────────────────────────────────────────────
//
// AccessPolicy is a user's resolved access: the base (allow-then-exclude or
// deny-then-include), the out-of-scope presentation, and the grants that refine
// it. Grants live in access_grants (see access.go); Base/HideOutOfScope stay in
// users.access JSONB. Evaluation lives in dashboard/access.go.
type AccessPolicy struct {
	Base           string       `json:"base,omitempty"`             // "allow" (default) | "deny"
	HideOutOfScope bool         `json:"hide_out_of_scope,omitempty"` // hide devices the user can't view, vs show with actions disabled
	Rules          []AccessRule `json:"rules,omitempty"`             // legacy (pre access_grants); migrated at startup, then empty
	Grants         []AccessGrant `json:"-"`
}

// AccessRule is the legacy JSONB rule shape, kept only so MigrateAccessRules can
// read policies written before the access_grants table existed.
type AccessRule struct {
	Effect    string   `json:"effect"`             // "allow" | "deny"
	Actions   []string `json:"actions"`            // action keys, or ["*"]
	ScopeType string   `json:"scope_type"`         // "all" | "group" | "restaurant"
	ScopeID   string   `json:"scope_id,omitempty"` // group / restaurant id
}

// IsEmpty reports a policy that changes nothing (base allow, no grants, no hiding).
func (p AccessPolicy) IsEmpty() bool {
	return (p.Base == "" || p.Base == "allow") && len(p.Rules) == 0 && len(p.Grants) == 0 && !p.HideOutOfScope
}

// GetUserAccess loads a user's base settings and their live (unexpired) grants.
func (d *DB) GetUserAccess(ctx context.Context, username string) (AccessPolicy, error) {
	var raw []byte
	var id uuid.UUID
	var pol AccessPolicy
	err := d.pool.QueryRow(ctx, `SELECT id, access FROM users WHERE username = $1`, username).Scan(&id, &raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No account behind this name (deleted, renamed): nothing is granted.
			return AccessPolicy{Base: "deny"}, nil
		}
		return pol, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &pol)
	}
	pol.Grants, err = d.ListAccessGrants(ctx, id, false)
	return pol, err
}

// SetUserAccess stores the base settings only; grants are managed through
// AddAccessGrant / UpdateAccessGrant / DeleteAccessGrant.
func (d *DB) SetUserAccess(ctx context.Context, id uuid.UUID, pol AccessPolicy) error {
	b, err := json.Marshal(AccessPolicy{Base: pol.Base, HideOutOfScope: pol.HideOutOfScope})
	if err != nil {
		return err
	}
	_, err = d.pool.Exec(ctx, `UPDATE users SET access = $2 WHERE id = $1`, id, b)
	return err
}

// DeviceScope is what a device belongs to, for policy scope matching.
type DeviceScope struct {
	RestaurantID *uuid.UUID
	Groups       []uuid.UUID
	DPC          bool // runs the Device-Owner agent (no longer hidden from anyone)
}

// DeviceScopes loads every device's restaurant and group memberships (two small
// queries) so a request can evaluate scope rules for any number of devices.
func (d *DB) DeviceScopes(ctx context.Context) (map[uuid.UUID]DeviceScope, error) {
	out := map[uuid.UUID]DeviceScope{}
	rows, err := d.pool.Query(ctx, `SELECT id, restaurant_id, agent_kind = 'dpc' FROM devices`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var rid *uuid.UUID
		var dpc bool
		if err := rows.Scan(&id, &rid, &dpc); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = DeviceScope{RestaurantID: rid, DPC: dpc}
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `SELECT device_id, group_id FROM device_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var did, gid uuid.UUID
		if err := rows.Scan(&did, &gid); err != nil {
			return nil, err
		}
		sc := out[did]
		sc.Groups = append(sc.Groups, gid)
		out[did] = sc
	}
	return out, rows.Err()
}

// teamAccountSQL admits an audit actor only when it is a real team account: a user row
// with an @aioapp.com email. "API key", scheduled recipes, "System" and accounts that no
// longer exist are not people on the leaderboard (same rule as the Activity page).
const teamAccountSQL = `u.email ILIKE '%@aioapp.com'`
