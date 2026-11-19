package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// "Why did it fail?" (19 Nov 2026): a failed install's result is often only
// `install failed: status=1 legacy=-999`. A person asks the device for the real reason;
// that sends one fixed read-only logcat shell command, and install_diagnoses ties it to
// the failed install so the pages can read its answer from command_results when they draw.
const installDiagnosesSchema = `
CREATE TABLE IF NOT EXISTS install_diagnoses (
	id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id         UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package           TEXT NOT NULL,
	failed_command_id UUID,
	diag_command_id   UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	created_by        TEXT NOT NULL DEFAULT '',
	created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_install_diagnoses_device_pkg ON install_diagnoses(device_id, package, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_install_diagnoses_failed ON install_diagnoses(failed_command_id);
`

// InstallDiagnosis is one ask, with the device's answer when it has come.
type InstallDiagnosis struct {
	DeviceID        uuid.UUID
	Package         string
	FailedCommandID *uuid.UUID
	DiagCommandID   uuid.UUID
	CreatedAt       time.Time
	Output          string // the shell output; "" until the device answers
	Answered        bool
}

// RecordInstallDiagnosis files an ask.
func (d *DB) RecordInstallDiagnosis(ctx context.Context, deviceID uuid.UUID, pkg string, failed *uuid.UUID, diagCmd uuid.UUID, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO install_diagnoses (device_id, package, failed_command_id, diag_command_id, created_by)
		VALUES ($1, $2, $3, $4, $5)`, deviceID, pkg, failed, diagCmd, by)
	return err
}

const installDiagnosisSelect = `
	SELECT i.device_id, i.package, i.failed_command_id, i.diag_command_id, i.created_at,
	       COALESCE(cr.output, ''), cr.id IS NOT NULL
	  FROM install_diagnoses i
	  LEFT JOIN command_results cr ON cr.command_id = i.diag_command_id AND cr.device_id = i.device_id`

func scanDiagnoses(d *DB, ctx context.Context, q string, args ...any) ([]InstallDiagnosis, error) {
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstallDiagnosis
	for rows.Next() {
		var x InstallDiagnosis
		if err := rows.Scan(&x.DeviceID, &x.Package, &x.FailedCommandID, &x.DiagCommandID, &x.CreatedAt, &x.Output, &x.Answered); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// InstallDiagnosesForCommand is the newest ask per device about one failed install command.
func (d *DB) InstallDiagnosesForCommand(ctx context.Context, failedCmd uuid.UUID) (map[uuid.UUID]InstallDiagnosis, error) {
	xs, err := scanDiagnoses(d, ctx, installDiagnosisSelect+` WHERE i.failed_command_id = $1 ORDER BY i.created_at`, failedCmd)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]InstallDiagnosis{}
	for _, x := range xs {
		out[x.DeviceID] = x // later rows win
	}
	return out, nil
}

// LatestInstallDiagnoses is the newest ask per package on one device (last 3 days).
func (d *DB) LatestInstallDiagnoses(ctx context.Context, deviceID uuid.UUID) (map[string]InstallDiagnosis, error) {
	xs, err := scanDiagnoses(d, ctx, installDiagnosisSelect+` WHERE i.device_id = $1 AND i.created_at > now() - interval '3 days' ORDER BY i.created_at`, deviceID)
	if err != nil {
		return nil, err
	}
	out := map[string]InstallDiagnosis{}
	for _, x := range xs {
		out[x.Package] = x
	}
	return out, nil
}

// InstallCommandPackage is the package an install command was for: the library's record
// of its APK. "" when unknown.
func (d *DB) InstallCommandPackage(ctx context.Context, cmdID uuid.UUID) string {
	var p string
	_ = d.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(a.package_name, ''), ap.package_name, '')
		  FROM commands c
		  LEFT JOIN apps a ON a.apk_url = c.apk_url AND c.apk_url <> ''
		  LEFT JOIN apk_packages ap ON ap.apk_url = c.apk_url AND c.apk_url <> ''
		 WHERE c.id = $1 AND c.type = 'install_apk' LIMIT 1`, cmdID).Scan(&p)
	return p
}
