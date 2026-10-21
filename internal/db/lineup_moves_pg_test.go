package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestLineupMovesAgainstPostgres: a tablet finishes an OTA and a client update today, a
// kitchen display finishes one (other role); two prod firmware changes are audited.
func TestLineupMovesAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var tab, kds uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('LUM-T7', 't7') RETURNING id`).Scan(&tab); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('LUM-KDS', 'kds') RETURNING id`).Scan(&kds); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'LUM-%'`)
	var upd int
	if err := d.pool.QueryRow(ctx, `INSERT INTO updates (reboot_behavior, status) VALUES ('manual', 'active') RETURNING id`).Scan(&upd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, upd)
	if _, err := d.pool.Exec(ctx, `INSERT INTO update_devices (update_id, device_id, status, completed_at) VALUES ($1, $2, 'installed', NOW()), ($1, $3, 'installed', NOW())`, upd, tab, kds); err != nil {
		t.Fatal(err)
	}
	var cmd uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO commands (type, apk_url, target_type) VALUES ('app_update', 'x', 'devices') RETURNING id`).Scan(&cmd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, cmd)
	if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'installed')`, cmd, tab); err != nil {
		t.Fatal(err)
	}
	moves, err := d.RoleMovesDaily(ctx, "t7", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 7 {
		t.Fatalf("days = %d, want 7", len(moves))
	}
	if last := moves[6]; last.Firmware != 1 || last.Client != 1 {
		t.Errorf("today = %+v, want one firmware and one client move (the KDS is another role)", last)
	}

	if _, err := d.pool.Exec(ctx, `INSERT INTO audit_log (actor, action, target, detail, created_at) VALUES
		('a', 'release.firmware_prod', 'lum_model', 'v1', NOW() - interval '3 days'),
		('b', 'release.firmware_prod', 'lum_model', 'v2', NOW() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM audit_log WHERE target = 'lum_model'`)
	h, err := d.FirmwareProdHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := h["lum_model"]; len(got) != 2 || got[0].Version != "v1" || got[1].Version != "v2" || got[1].By != "b" {
		t.Errorf("history = %+v", got)
	}
}
