package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// FleetAdbKey is the one adb key pair the firmware trusts, as stored: the private key
// is sealed (see internal/fleetkey) and never returned in plain form from here.
type FleetAdbKey struct {
	Version       int
	Fingerprint   string
	PublicKey     string
	PrivateSealed string
	UploadedBy    string
	UploadedAt    time.Time
}

// GetFleetAdbKey returns the stored key, or nil when none is uploaded.
func (d *DB) GetFleetAdbKey(ctx context.Context) (*FleetAdbKey, error) {
	var k FleetAdbKey
	err := d.pool.QueryRow(ctx, `
		SELECT version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at
		FROM fleet_adb_key WHERE id = 1`).Scan(&k.Version, &k.Fingerprint, &k.PublicKey, &k.PrivateSealed, &k.UploadedBy, &k.UploadedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// FleetAdbKeyVersion is the current version, 0 when no key is uploaded.
func (d *DB) FleetAdbKeyVersion(ctx context.Context) (int, error) {
	var v int
	err := d.pool.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM fleet_adb_key WHERE id = 1), 0)`).Scan(&v)
	return v, err
}

// PutFleetAdbKey stores (or replaces) the key; the version goes up on every upload,
// including a replacement after a delete.
func (d *DB) PutFleetAdbKey(ctx context.Context, publicKey, privateSealed, fingerprint, by string) (int, error) {
	var v int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO fleet_adb_key (id, version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at)
		VALUES (1, (SELECT COALESCE(MAX(last_version), 0) + 1 FROM fleet_adb_key_seq), $1, $2, $3, $4, NOW())
		ON CONFLICT (id) DO UPDATE SET
		  version = fleet_adb_key.version + 1, fingerprint = EXCLUDED.fingerprint, public_key = EXCLUDED.public_key,
		  private_key_sealed = EXCLUDED.private_key_sealed, uploaded_by = EXCLUDED.uploaded_by, uploaded_at = NOW()
		RETURNING version`, fingerprint, publicKey, privateSealed, by).Scan(&v)
	if err != nil {
		return 0, err
	}
	_, err = d.pool.Exec(ctx, `UPDATE fleet_adb_key_seq SET last_version = $1`, v)
	return v, err
}

// DeleteFleetAdbKey removes the key. The version counter is kept so the next upload is newer.
func (d *DB) DeleteFleetAdbKey(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM fleet_adb_key WHERE id = 1`)
	return err
}

// ListAuditByAction is the newest entries of one audit action.
func (d *DB) ListAuditByAction(ctx context.Context, action string, limit int) ([]AuditEntry, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, created_at, actor, action, target, detail FROM audit_log
		WHERE action = $1 ORDER BY created_at DESC LIMIT $2`, action, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
