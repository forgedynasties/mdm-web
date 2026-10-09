package db

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FleetAdbKey is one adb key pair the firmware trusts, as stored: the private key is
// sealed (see internal/fleetkey) and never returned in plain form from here. Label is
// which lot of devices it is for — "default" is the one the firmware carries today, the
// rest are per vendor ("sunmi", "mega-kiosk", "dongle").
type FleetAdbKey struct {
	Label         string
	Version       int
	Fingerprint   string
	PublicKey     string
	PrivateSealed string
	UploadedBy    string
	UploadedAt    time.Time
}

// DefaultFleetAdbKeyLabel is the key every caller means when it does not say a label.
const DefaultFleetAdbKeyLabel = "default"

var labelOK = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

// CleanFleetAdbKeyLabel lowercases a label and checks it. Labels end up in URLs and in
// file names on the apps' disks, so they stay to lowercase letters, digits and dashes.
func CleanFleetAdbKeyLabel(label string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		return DefaultFleetAdbKeyLabel, nil
	}
	if !labelOK.MatchString(l) {
		return "", errors.New("label: lowercase letters, digits and dashes, 2 to 32 characters")
	}
	return l, nil
}

const fleetKeyCols = `label, version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at`

func scanFleetKey(row pgx.Row) (*FleetAdbKey, error) {
	var k FleetAdbKey
	err := row.Scan(&k.Label, &k.Version, &k.Fingerprint, &k.PublicKey, &k.PrivateSealed, &k.UploadedBy, &k.UploadedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// GetFleetAdbKey returns the default key, or nil when none is uploaded.
func (d *DB) GetFleetAdbKey(ctx context.Context) (*FleetAdbKey, error) {
	return d.GetFleetAdbKeyByLabel(ctx, DefaultFleetAdbKeyLabel)
}

// GetFleetAdbKeyByLabel returns one key, or nil when that label has none.
func (d *DB) GetFleetAdbKeyByLabel(ctx context.Context, label string) (*FleetAdbKey, error) {
	return scanFleetKey(d.pool.QueryRow(ctx, `SELECT `+fleetKeyCols+` FROM fleet_adb_keys WHERE label = $1`, label))
}

// ListFleetAdbKeys is every key, default first and the rest by label.
func (d *DB) ListFleetAdbKeys(ctx context.Context) ([]FleetAdbKey, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+fleetKeyCols+` FROM fleet_adb_keys
		ORDER BY (label = '`+DefaultFleetAdbKeyLabel+`') DESC, label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetAdbKey
	for rows.Next() {
		var k FleetAdbKey
		if err := rows.Scan(&k.Label, &k.Version, &k.Fingerprint, &k.PublicKey, &k.PrivateSealed, &k.UploadedBy, &k.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FleetAdbKeyVersion is the newest version across every key, 0 when there are none. The
// apps watch this one number and re-fetch the whole set when it moves.
func (d *DB) FleetAdbKeyVersion(ctx context.Context) (int, error) {
	var v int
	err := d.pool.QueryRow(ctx, `SELECT COALESCE((SELECT MAX(version) FROM fleet_adb_keys), 0)`).Scan(&v)
	return v, err
}

// PutFleetAdbKey stores (or replaces) the default key.
func (d *DB) PutFleetAdbKey(ctx context.Context, publicKey, privateSealed, fingerprint, by string) (int, error) {
	return d.PutFleetAdbKeyByLabel(ctx, DefaultFleetAdbKeyLabel, publicKey, privateSealed, fingerprint, by)
}

// PutFleetAdbKeyByLabel stores (or replaces) one key. The version goes up on every upload,
// including a replacement after a delete, and is shared by all labels.
func (d *DB) PutFleetAdbKeyByLabel(ctx context.Context, label, publicKey, privateSealed, fingerprint, by string) (int, error) {
	var v int
	err := d.pool.QueryRow(ctx, `
		UPDATE fleet_adb_key_seq SET last_version = last_version + 1 WHERE id = 1 RETURNING last_version`).Scan(&v)
	if err != nil {
		return 0, err
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO fleet_adb_keys (label, version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (label) DO UPDATE SET
		  version = EXCLUDED.version, fingerprint = EXCLUDED.fingerprint, public_key = EXCLUDED.public_key,
		  private_key_sealed = EXCLUDED.private_key_sealed, uploaded_by = EXCLUDED.uploaded_by, uploaded_at = NOW()`,
		label, v, fingerprint, publicKey, privateSealed, by)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// DeleteFleetAdbKey removes the default key. The version counter is kept so the next
// upload is newer.
func (d *DB) DeleteFleetAdbKey(ctx context.Context) error {
	return d.DeleteFleetAdbKeyByLabel(ctx, DefaultFleetAdbKeyLabel)
}

// DeleteFleetAdbKeyByLabel removes one key.
func (d *DB) DeleteFleetAdbKeyByLabel(ctx context.Context, label string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM fleet_adb_keys WHERE label = $1`, label)
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
