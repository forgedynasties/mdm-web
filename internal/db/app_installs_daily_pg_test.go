package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAppInstallsDailyAgainstPostgres: today an install sent to two devices (one failed)
// and a group install delivered to one; a client update (app_update) does not count.
func TestAppInstallsDailyAgainstPostgres(t *testing.T) {
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
	before, err := d.AppInstallsDaily(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	var a, b uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ('AID-1') RETURNING id`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ('AID-2') RETURNING id`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'AID-%'`)
	c1, err := d.CreateCommand(ctx, "install_apk", "https://x/a.apk", []byte(`{}`), "devices", []uuid.UUID{a, b})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, c1.ID)
	if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'failed') ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'failed'`, c1.ID, a); err != nil {
		t.Fatal(err)
	}
	g := uuid.New()
	c2, err := d.CreateCommand(ctx, "install_apk", "https://x/b.apk", []byte(`{}`), "groups", []uuid.UUID{g})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, c2.ID)
	if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'installed') ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'installed'`, c2.ID, b); err != nil {
		t.Fatal(err)
	}
	c3, err := d.CreateCommand(ctx, "app_update", "https://x/c.apk", []byte(`{}`), "devices", []uuid.UUID{a})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, c3.ID)

	days, err := d.AppInstallsDaily(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 7 {
		t.Fatalf("days = %d", len(days))
	}
	now, was := days[6], before[6]
	if now.Sent-was.Sent != 3 || now.Failed-was.Failed != 1 {
		t.Errorf("today = %+v (before %+v), want 3 sent and 1 failed more", now, was)
	}
	if n, err := d.AppsAddedSince(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("AppsAddedSince(future) = %d, %v", n, err)
	}
}
