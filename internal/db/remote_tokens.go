package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PutRemoteToken stores a single-use remote-control token for deviceID until now+ttl,
// and clears expired ones.
func (d *DB) PutRemoteToken(ctx context.Context, token string, deviceID uuid.UUID, user string, ttl time.Duration) error {
	if _, err := d.pool.Exec(ctx, `DELETE FROM remote_tokens WHERE expires < NOW()`); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO remote_tokens (token, device_id, expires, username) VALUES ($1, $2, NOW() + $3::interval, $4)`,
		token, deviceID, ttl.String(), user)
	return err
}

// TakeRemoteToken consumes token: the device it authorizes, if it existed and had not
// expired. Gone after the first look either way.
func (d *DB) TakeRemoteToken(ctx context.Context, token string) (uuid.UUID, string, bool, error) {
	var id uuid.UUID
	var user string
	var expires time.Time
	err := d.pool.QueryRow(ctx, `DELETE FROM remote_tokens WHERE token = $1 RETURNING device_id, username, expires`, token).Scan(&id, &user, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", false, err
	}
	return id, user, time.Now().Before(expires), nil
}
