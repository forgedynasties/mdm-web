package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestOverviewScopeAgainstPostgres runs the Overview C queries (trend, rollouts, the
// wall's battery and temperature columns) on a real database: two kitchen displays at
// one venue and a tablet elsewhere, a day of stats, crashes, and an app pushed to both
// displays.
func TestOverviewScopeAgainstPostgres(t *testing.T) {
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
	var venue uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('ovscope-venue') RETURNING id`).Scan(&venue); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 3)
	for i, row := range []struct {
		serial, class string
		venue         *uuid.UUID
		temp          string
	}{{"OVS-KDS-1", "kds", &venue, "47.5"}, {"OVS-KDS-2", "kds", &venue, "n/a"}, {"OVS-T7-1", "t7", nil, "30"}} {
		if err := d.pool.QueryRow(ctx, `
			INSERT INTO devices (serial_number, device_class, restaurant_id, latest_battery_pct, latest_extra, last_seen_at)
			VALUES ($1, $2, $3, 15, jsonb_build_object('battery_temp_c', $4::text), NOW()) RETURNING id`,
			row.serial, row.class, row.venue, row.temp).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'OVS-%'`)
		d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, venue)
	}()
	for _, id := range ids {
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO device_daily_stats (device_id, day, checkin_count, battery_min, battery_avg, temp_max)
			VALUES ($1, CURRENT_DATE, 10, 12, 50, 46), ($1, CURRENT_DATE - 1, 10, 60, 70, 30)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `INSERT INTO device_events (device_id, kind, occurred_at) VALUES ($1, 'anr', NOW() - interval '1 hour'), ($1, 'reboot', NOW())`, id); err != nil {
			t.Fatal(err)
		}
	}

	tr, err := d.OverviewTrend(ctx, OverviewScope{Class: "kds", Venue: &venue}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Days) != 8 {
		t.Fatalf("days = %d, want 8 (every day present)", len(tr.Days))
	}
	today := tr.Days[7]
	// Kitchen displays are mains powered: no low-battery count however low the reading.
	if today.Active != 2 || today.Hot != 2 || today.LowBattery != 0 || today.Crashes != 2 || tr.Crashes24h != 2 {
		t.Errorf("today = %+v, crashes24h = %d", today, tr.Crashes24h)
	}
	fleet, err := d.OverviewTrend(ctx, OverviewScope{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Days[7].LowBattery < 1 {
		t.Errorf("the tablet's 12%% minimum should count as low: %+v", fleet.Days[7])
	}

	wall, err := d.FleetWall(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, w := range wall {
		switch w.Serial {
		case "OVS-KDS-1":
			seen++
			if w.HasBattery || w.TempC == nil || *w.TempC != 47.5 || w.BatteryPct != 15 {
				t.Errorf("KDS-1 = %+v", w)
			}
		case "OVS-KDS-2":
			seen++
			if w.TempC != nil {
				t.Errorf("a non-numeric temperature should read as none: %v", *w.TempC)
			}
		case "OVS-T7-1":
			seen++
			if !w.HasBattery {
				t.Error("a T7 has a battery")
			}
		}
	}
	if seen != 3 {
		t.Errorf("wall listed %d of the 3 test devices", seen)
	}

	var cmd uuid.UUID
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO commands (type, apk_url, target_type, created_by, payload)
		VALUES ('app_update', 'https://x/mdm.apk', 'devices', 'auto-update', '{"package":"com.aioapp.mdm","version":"9.9.9"}')
		RETURNING id`).Scan(&cmd); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, cmd)
	if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'completed'), ($1, $3, 'failed')`,
		cmd, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	// An app install from the library, one job per device; installs end 'installed'.
	if _, err := d.pool.Exec(ctx, `INSERT INTO apps (name, apk_url, package_name) VALUES ('OVS Nugget', 'https://x/ovs-nugget.apk', 'aio.app.ovsnugget')`); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM apps WHERE apk_url = 'https://x/ovs-nugget.apk'`)
	for _, id := range ids[:2] {
		var c uuid.UUID
		if err := d.pool.QueryRow(ctx, `INSERT INTO commands (type, apk_url, target_type, created_by, payload)
			VALUES ('install_apk', 'https://x/ovs-nugget.apk', 'devices', 'ovs-tester', '{}') RETURNING id`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		defer d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, c)
		if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'installed')`, c, id); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := d.RecentRollouts(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs {
		if r.Kind == "app_update" && r.Name == "9.9.9" {
			found = true
			if r.Package != "com.aioapp.mdm" || r.Total != 2 || r.Done != 1 || r.Failed != 1 || r.Who != "auto-update" || r.CommandID != cmd || r.Commands != 1 {
				t.Errorf("rollout = %+v", r)
			}
		}
	}
	app := false
	for _, r := range rs {
		if r.Kind == "install_apk" && r.Who == "ovs-tester" {
			app = true
			if r.Name != "OVS Nugget" || r.Total != 2 || r.Done != 2 || r.Failed != 0 || r.Commands != 2 || r.AppPackage != "aio.app.ovsnugget" {
				t.Errorf("app rollout = %+v", r)
			}
		}
	}
	if !app {
		t.Error("the two-job app install is not a rollout")
	}
	if !found {
		t.Errorf("the client update is not among %d rollouts", len(rs))
	}
}
