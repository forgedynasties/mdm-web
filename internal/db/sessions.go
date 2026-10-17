package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ── Dashboard sessions ──────────────────────────────────────────────────────

// Session is a server-side dashboard session. UserID is nil for the static
// env-configured admin (which has no users row).
type Session struct {
	ID        string
	UserID    *uuid.UUID
	Username  string
	Role      string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
}

// CreateSession persists a new session row.
func (d *DB) CreateSession(ctx context.Context, s Session) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, username, role, created_at, last_seen, expires_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), $5)
	`, s.ID, s.UserID, s.Username, s.Role, s.ExpiresAt)
	return err
}

// GetSession returns the session by id, or an error if it does not exist.
func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := d.pool.QueryRow(ctx, `
		SELECT id, user_id, username, role, created_at, last_seen, expires_at
		FROM sessions WHERE id = $1
	`, id).Scan(&s.ID, &s.UserID, &s.Username, &s.Role, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// TouchSession slides a session's last_seen forward to keep an active user
// logged in (the absolute expires_at is left unchanged).
func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `UPDATE sessions SET last_seen = NOW() WHERE id = $1`, id)
	return err
}

// DeleteSession removes a single session (logout).
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// DeleteAllSessions removes every session (admin "log out all").
func (d *DB) DeleteAllSessions(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions`)
	return err
}

// DeleteSessionsForUser removes every session belonging to one user — used after a
// password reset so a stolen/old session can't outlive the credential change.
func (d *DB) DeleteSessionsForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	return err
}

// DeleteExpiredSessions prunes sessions past their absolute expiry; called by
// housekeeping. Idle-timeout enforcement happens at read time.
func (d *DB) DeleteExpiredSessions(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < NOW()`)
	return err
}
