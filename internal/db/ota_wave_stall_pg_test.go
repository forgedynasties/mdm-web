package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestOTAWaveStallAgainstPostgres: a device waiting for a wave slot is never failed as
// stalled, one handed the OTA that went quiet is, and a row failed while still in line
// (before the fix) goes back to pending.
func TestOTAWaveStallAgainstPostgres(t *testing.T) {
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
	dev := map[string]uuid.UUID{}
	for _, sn := range []string{"OWS-WAIT", "OWS-SENT", "OWS-DL", "OWS-OLDFAIL", "OWS-REALFAIL"} {
		var id uuid.UUID
		if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ($1, 't7') RETURNING id`, sn).Scan(&id); err != nil {
			t.Fatal(err)
		}
		dev[sn] = id
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'OWS-%'`)
	var upd int
	if err := d.pool.QueryRow(ctx, `INSERT INTO updates (reboot_behavior, status, created_at) VALUES ('immediate', 'active', NOW() - interval '45 minutes') RETURNING id`).Scan(&upd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, upd)
	add := func(sn, status, code string, started bool) {
		t.Helper()
		st := "NULL"
		if started {
			st = "NOW() - interval '40 minutes'"
		}
		if _, err := d.pool.Exec(ctx, `INSERT INTO update_devices (update_id, device_id, status, error_code, updated_at, started_at)
			VALUES ($1, $2, $3, $4, NOW() - interval '40 minutes', `+st+`)`, upd, dev[sn], status, code); err != nil {
			t.Fatal(err)
		}
	}
	add("OWS-WAIT", "pending", "", false)          // in line, never offered
	add("OWS-SENT", "pending", "", false)          // offered 40 min ago, never acked
	add("OWS-DL", "downloading", "", true)         // downloading, silent
	add("OWS-OLDFAIL", "failed", "stalled", false) // failed while in line
	add("OWS-REALFAIL", "failed", "stalled", true) // a real stall: was offered
	for _, sn := range []string{"OWS-SENT", "OWS-REALFAIL"} {
		cmd, err := d.CreateCommand(ctx, "ota", "", []byte(`{}`), "devices", []uuid.UUID{dev[sn]})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = NOW() - interval '40 minutes' WHERE id = $1`, cmd.ID); err != nil {
			t.Fatal(err)
		}
	}

	stalled, err := d.ExpireStalledOTAs(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, s := range stalled {
		got[s.DeviceID] = true
	}
	if got[dev["OWS-WAIT"]] {
		t.Error("a device waiting for its wave slot was failed as stalled")
	}
	if !got[dev["OWS-SENT"]] || !got[dev["OWS-DL"]] {
		t.Errorf("stalled = %v, want the offered-and-silent and the silent download", stalled)
	}

	requeued, err := d.RequeueNeverOfferedOTAs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].DeviceID != dev["OWS-OLDFAIL"] {
		t.Errorf("requeued = %v, want only OWS-OLDFAIL", requeued)
	}
	status := func(sn string) (s, code string) {
		d.pool.QueryRow(ctx, `SELECT status, error_code FROM update_devices WHERE update_id = $1 AND device_id = $2`, upd, dev[sn]).Scan(&s, &code)
		return
	}
	if s, c := status("OWS-OLDFAIL"); s != "pending" || c != "" {
		t.Errorf("OWS-OLDFAIL = %s/%s, want pending", s, c)
	}
	if s, _ := status("OWS-REALFAIL"); s != "failed" {
		t.Errorf("OWS-REALFAIL = %s, want still failed (it was offered)", s)
	}
	if s, _ := status("OWS-WAIT"); s != "pending" {
		t.Errorf("OWS-WAIT = %s, want pending", s)
	}
}
