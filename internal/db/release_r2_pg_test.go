package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestReleaseR2AgainstPostgres: crashes on one build by window (a reboot is not a crash,
// another build's crash does not count), and a release's installs this week.
func TestReleaseR2AgainstPostgres(t *testing.T) {
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
	var a, b uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ('R2-A') RETURNING id`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ('R2-B') RETURNING id`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'R2-%'`)
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_events (device_id, kind, build_id, occurred_at) VALUES
		($1, 'crash', 'r2-build', NOW() - interval '1 hour'),
		($2, 'anr',   'r2-build', NOW() - interval '2 hours'),
		($1, 'reboot','r2-build', NOW() - interval '3 hours'),
		($1, 'crash', 'r2-build', NOW() - interval '30 hours'),
		($2, 'crash', 'r2-build', NOW() - interval '4 days'),
		($1, 'crash', 'r2-other', NOW() - interval '95 minutes')`, a, b); err != nil {
		t.Fatal(err)
	}
	c, err := d.BuildCrashCounts(ctx, "r2-build")
	if err != nil {
		t.Fatal(err)
	}
	if c.Day != 2 || c.PrevDay != 1 || c.Week != 4 || c.DayDevices != 2 || c.WeekDevices != 2 {
		t.Errorf("counts = %+v", c)
	}

	var rel, upd int
	if err := d.pool.QueryRow(ctx, `INSERT INTO releases (version, product, status) VALUES ('r2-build', 't7', 'published') RETURNING id`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM releases WHERE id = $1`, rel)
	if err := d.pool.QueryRow(ctx, `INSERT INTO updates (release_id, reboot_behavior, status) VALUES ($1, 'manual', 'active') RETURNING id`, rel).Scan(&upd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, upd)
	if _, err := d.pool.Exec(ctx, `INSERT INTO update_devices (update_id, device_id, status, completed_at) VALUES
		($1, $2, 'installed', NOW() - interval '2 days'), ($1, $3, 'downloading', NULL)`, upd, a, b); err != nil {
		t.Fatal(err)
	}
	n, err := d.ReleaseInstallsSince(ctx, rel, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("installs this week = %d, want 1", n)
	}
}
