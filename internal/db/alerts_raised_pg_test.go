package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestAlertsRaisedAgainstPostgres: alerts raised today and the day before, by severity;
// one on a hidden device and one older than 48 h are not counted, and identity alerts
// only when the viewer may see them.
func TestAlertsRaisedAgainstPostgres(t *testing.T) {
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
	var dev, hidden uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ('ARC-1') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, hidden) VALUES ('ARC-H', true) RETURNING id`).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'ARC-%'`)
	before, err := d.AlertsRaisedCounts(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		typ, sev, ago string
		dev           uuid.UUID
	}{
		{"arc_a", "critical", "1 hour", dev},
		{"arc_b", "critical", "30 hours", dev},
		{"arc_c", "critical", "31 hours", dev},
		{"arc_d", "warning", "2 hours", dev},
		{"arc_e", "warning", "3 hours", hidden}, // hidden device
		{"arc_f", "warning", "60 hours", dev},   // too old
		{AlertTypeIdentityConflict, "critical", "1 hour", dev},
	}
	for _, r := range rows {
		if _, err := d.pool.Exec(ctx, `INSERT INTO alerts (type, device_id, severity, fired_at) VALUES ($1, $2, $3, NOW() - $4::interval)`,
			r.typ, r.dev, r.sev, r.ago); err != nil {
			t.Fatal(err)
		}
	}
	defer d.pool.Exec(ctx, `DELETE FROM alerts WHERE device_id IN (SELECT id FROM devices WHERE serial_number LIKE 'ARC-%')`)
	got, err := d.AlertsRaisedCounts(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	want := AlertsRaised{Critical24: before.Critical24 + 1, CriticalPrev: before.CriticalPrev + 2,
		Warning24: before.Warning24 + 1, WarningPrev: before.WarningPrev}
	if got != want {
		t.Errorf("hiding identity: got %+v, want %+v", got, want)
	}
	all, err := d.AlertsRaisedCounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if all.Critical24 != got.Critical24+1 {
		t.Errorf("a super admin also counts the identity alert: %+v vs %+v", all, got)
	}
}
