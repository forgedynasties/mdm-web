package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestOTAWaveStallAgainstPostgres: a device waiting for a wave slot is never failed as
// stalled; an online one handed the OTA that went quiet is; an offline one goes back in
// line with its OTA command expired; a row failed while still in line (before the fix)
// goes back to pending.
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
	for _, sn := range []string{"OWS-WAIT", "OWS-SENT", "OWS-DL", "OWS-OLDFAIL", "OWS-REALFAIL", "OWS-OFFSENT", "OWS-OFFDL"} {
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
	add("OWS-OFFSENT", "pending", "", false)       // offered, then went offline
	add("OWS-OFFDL", "downloading", "", true)      // went offline mid-download
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() - interval '2 hours' WHERE serial_number IN ('OWS-OFFSENT', 'OWS-OFFDL')`); err != nil {
		t.Fatal(err)
	}
	offCmd := map[string]bool{}
	for _, sn := range []string{"OWS-SENT", "OWS-REALFAIL", "OWS-OFFSENT", "OWS-OFFDL"} {
		cmd, err := d.CreateCommand(ctx, "ota", "", []byte(`{}`), "devices", []uuid.UUID{dev[sn]})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = NOW() - interval '40 minutes' WHERE id = $1`, cmd.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE command_status SET updated_at = NOW() - interval '40 minutes' WHERE command_id = $1`, cmd.ID); err != nil {
			t.Fatal(err)
		}
		offCmd[sn] = true
	}
	_ = offCmd

	back, err := d.RequeueOfflineOTAs(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	gotBack := map[uuid.UUID]bool{}
	for _, s := range back {
		gotBack[s.DeviceID] = true
	}
	if len(back) != 2 || !gotBack[dev["OWS-OFFSENT"]] || !gotBack[dev["OWS-OFFDL"]] {
		t.Errorf("requeued offline = %v, want OWS-OFFSENT and OWS-OFFDL", back)
	}
	var open int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM command_status WHERE device_id IN ($1, $2) AND status <> 'expired'`, dev["OWS-OFFSENT"], dev["OWS-OFFDL"]).Scan(&open)
	if open != 0 {
		t.Errorf("%d OTA command(s) of the requeued devices still open", open)
	}
	// 40 minutes later, still offline: they stay in line, never failed.
	if _, err := d.pool.Exec(ctx, `UPDATE update_devices SET updated_at = NOW() - interval '40 minutes' WHERE update_id = $1`, upd); err != nil {
		t.Fatal(err)
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
	if got[dev["OWS-OFFSENT"]] || got[dev["OWS-OFFDL"]] {
		t.Error("an offline device was failed as stalled")
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
	for _, sn := range []string{"OWS-WAIT", "OWS-OFFSENT", "OWS-OFFDL"} {
		if s, _ := status(sn); s != "pending" {
			t.Errorf("%s = %s, want pending", sn, s)
		}
	}
}
