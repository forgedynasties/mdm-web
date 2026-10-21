package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestKeyResetWarningsAgainstPostgres: a device reset while offline is not warned about
// (it cannot register until it checks in) and an old warning on it is resolved; a device
// that checked in after its reset without registering is.
func TestKeyResetWarningsAgainstPostgres(t *testing.T) {
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
	var offline, silent uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, last_seen_at, key_reset_at) VALUES ('KRS-OFF', NOW() - interval '5 days', NOW() - interval '2 hours') RETURNING id`).Scan(&offline); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, last_seen_at, key_reset_at) VALUES ('KRS-ON', NOW() - interval '5 minutes', NOW() - interval '2 hours') RETURNING id`).Scan(&silent); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'KRS-%'`)
	if _, err := d.CreateAlertIfAbsent(ctx, nil, "key_not_registered", offline, "warning", "old warning", nil); err != nil {
		t.Fatal(err)
	}

	over, err := d.OverdueKeyResets(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := over[offline]; ok {
		t.Error("a device offline since its reset is not overdue")
	}
	if over[silent] != "KRS-ON" {
		t.Errorf("the device that checked in without registering is overdue: %v", over)
	}
	n, err := d.ResolveAnsweredKeyAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("resolved %d, want the offline device's old warning", n)
	}
	var open int
	d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts WHERE device_id = $1 AND status <> 'resolved'`, offline).Scan(&open)
	if open != 0 {
		t.Error("the offline device still has an open warning")
	}
}
