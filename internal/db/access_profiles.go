package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// AccessProfile is a named set of actions — "Look only", "Look and fix" — that the
// access editor offers as the answer to "what can they do there?". Grants made from a
// profile carry its id; changing the profile's actions changes all of them.
type AccessProfile struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Actions     []string  `json:"actions"`
	Builtin     bool      `json:"builtin"`
	CreatedBy   string    `json:"created_by"`
	UpdatedAt   time.Time `json:"updated_at"`
	Users       []string  `json:"users,omitempty"` // usernames with a grant from it (filled by ListAccessProfiles)
}

// ListAccessProfiles returns the profiles in order, each with the users on it.
func (d *DB) ListAccessProfiles(ctx context.Context) ([]AccessProfile, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT p.id, p.name, p.description, p.actions, p.builtin, p.created_by, p.updated_at,
		       COALESCE(array_agg(DISTINCT u.username) FILTER (WHERE u.username IS NOT NULL), '{}')
		FROM access_profiles p
		LEFT JOIN access_grants g ON g.profile_id = p.id
		LEFT JOIN users u ON u.id = g.user_id
		GROUP BY p.id ORDER BY p.position, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessProfile
	for rows.Next() {
		var p AccessProfile
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Actions, &p.Builtin, &p.CreatedBy, &p.UpdatedAt, &p.Users); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveAccessProfile creates (id nil) or updates a profile, and brings every grant made
// from it in line with its actions. Returns the id.
func (d *DB) SaveAccessProfile(ctx context.Context, id *uuid.UUID, name, description string, actions []string, by string) (uuid.UUID, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	var pid uuid.UUID
	if id == nil {
		err = tx.QueryRow(ctx, `
			INSERT INTO access_profiles (name, description, actions, created_by, position)
			VALUES ($1, $2, $3, $4, COALESCE((SELECT MAX(position) FROM access_profiles), 0) + 1) RETURNING id`,
			name, description, actions, by).Scan(&pid)
	} else {
		pid = *id
		_, err = tx.Exec(ctx, `UPDATE access_profiles SET name = $2, description = $3, actions = $4, updated_at = NOW() WHERE id = $1`,
			pid, name, description, actions)
	}
	if err != nil {
		return uuid.Nil, err
	}
	// Grants keep any sensitive extras the editor added for "everything" on top; a
	// profile's grants are exactly its actions.
	if _, err := tx.Exec(ctx, `UPDATE access_grants SET actions = $2 WHERE profile_id = $1 AND effect = 'allow'`, pid, actions); err != nil {
		return uuid.Nil, err
	}
	return pid, tx.Commit(ctx)
}

// DeleteAccessProfile removes a non-built-in profile. Grants made from it keep their
// actions and become hand-made rules.
func (d *DB) DeleteAccessProfile(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM access_profiles WHERE id = $1 AND NOT builtin`, id)
	return err
}

// ProfileUserIDs returns the users with a grant from the profile.
func (d *DB) ProfileUserIDs(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT DISTINCT user_id FROM access_grants WHERE profile_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var u uuid.UUID
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ReplaceUserAccess sets a user's starting point and replaces all their grants in one
// transaction — what the simple editor saves.
func (d *DB) ReplaceUserAccess(ctx context.Context, userID uuid.UUID, base string, grants []AccessGrant) error {
	b, err := json.Marshal(AccessPolicy{Base: base})
	if err != nil {
		return err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET access = $2 WHERE id = $1`, userID, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM access_grants WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, g := range grants {
		rid, gid, did, err := scopeColumns(g)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_grants (user_id, effect, scope_type, restaurant_id, group_id, device_id, actions, note, expires_at, created_by, profile_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			userID, g.Effect, g.ScopeType, rid, gid, did, g.Actions, g.Note, g.ExpiresAt, g.CreatedBy, g.ProfileID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
