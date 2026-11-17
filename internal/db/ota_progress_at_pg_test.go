package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestOTAProgressAtShared: "last moved" lives in command_status.progress_at, so every server
// copy reads the same thing (17 Nov 2026: update #620 flipped between Stuck and Installing).
// The same percent again keeps it; a new percent moves it.
func TestOTAProgressAtShared(t *testing.T) {
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
	var dev uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('OPA-1', 't7') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'OPA-1'`)
	var upd int
	if err := d.pool.QueryRow(ctx, `INSERT INTO updates (reboot_behavior, status, created_at) VALUES ('immediate', 'active', NOW() - interval '1 hour') RETURNING id`).Scan(&upd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, upd)
	if _, err := d.pool.Exec(ctx, `INSERT INTO update_devices (update_id, device_id, status, started_at) VALUES ($1, $2, 'downloading', NOW())`, upd, dev); err != nil {
		t.Fatal(err)
	}
	cmd, err := d.CreateCommand(ctx, "ota", "", []byte(`{}`), "devices", []uuid.UUID{dev})
	if err != nil {
		t.Fatal(err)
	}
	p := 92
	if err := d.SetCommandProgress(ctx, cmd.ID, dev, "installing", &p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE command_status SET progress_at = NOW() - interval '20 minutes' WHERE command_id = $1`, cmd.ID); err != nil {
		t.Fatal(err)
	}
	read := func() OTARowProgress {
		t.Helper()
		m, err := d.OTAProgressForUpdate(ctx, upd)
		if err != nil {
			t.Fatal(err)
		}
		return m[dev]
	}
	if err := d.SetCommandProgress(ctx, cmd.ID, dev, "installing", &p); err != nil { // same frame again
		t.Fatal(err)
	}
	if r := read(); time.Since(r.AdvancedAt) < 19*time.Minute || time.Since(r.ReportAt) > time.Minute {
		t.Fatalf("same percent: advanced %v ago, reported %v ago; want ~20 min and now", time.Since(r.AdvancedAt), time.Since(r.ReportAt))
	}
	p = 93
	if err := d.SetCommandProgress(ctx, cmd.ID, dev, "installing", &p); err != nil {
		t.Fatal(err)
	}
	if r := read(); time.Since(r.AdvancedAt) > time.Minute {
		t.Fatalf("new percent: advanced %v ago, want now", time.Since(r.AdvancedAt))
	}
}
