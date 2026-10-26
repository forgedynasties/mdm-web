package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// Session ids are stored hashed (security audit, 26 Oct 2026): the id in a cookie or
// an app bearer token is the credential, so anyone who could read the table could use
// any row. Callers keep passing and seeing the raw id; only this file knows the
// difference. "h:" tells a hashed row from one written before the change (raw ids are
// plain hex), and a lookup that finds an old row hashes it, so nobody is signed out.
func storedSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "h:" + hex.EncodeToString(sum[:])
}

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
	`, storedSessionID(s.ID), s.UserID, s.Username, s.Role, s.ExpiresAt)
	return err
}

// GetSession returns the session by id, or an error if it does not exist.
func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	const q = `SELECT user_id, username, role, created_at, last_seen, expires_at FROM sessions WHERE id = $1`
	err := d.pool.QueryRow(ctx, q, storedSessionID(id)).Scan(&s.UserID, &s.Username, &s.Role, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt)
	if err != nil {
		// A row from before hashing (or written by the old server during a deploy):
		// accept it once and store it hashed.
		if err2 := d.pool.QueryRow(ctx, q, id).Scan(&s.UserID, &s.Username, &s.Role, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt); err2 != nil {
			return nil, err
		}
		_, _ = d.pool.Exec(ctx, `UPDATE sessions SET id = $2 WHERE id = $1`, id, storedSessionID(id))
	}
	s.ID = id
	return &s, nil
}

// TouchSession slides a session's last_seen forward to keep an active user
// logged in (the absolute expires_at is left unchanged).
func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `UPDATE sessions SET last_seen = NOW() WHERE id = $1`, storedSessionID(id))
	return err
}

// DeleteSession removes a single session (logout).
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1 OR id = $2`, storedSessionID(id), id)
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
