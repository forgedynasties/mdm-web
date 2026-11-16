package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The fleet reset clears devices, venues, groups and their history, and keeps people,
// their fleet-wide access and the fleet-wide service window.
//
//	MDM_TEST_DATABASE_URL=postgres://… go test ./internal/db -run FleetReset
func TestFleetResetClearsFleetKeepsPeople(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	venue, err := d.CreateRestaurant(ctx, Restaurant{Name: "Reset venue " + suffix})
	if err != nil {
		t.Fatal(err)
	}
	grp, err := d.CreateGroup(ctx, "Reset group "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	b := 50
	devID, _, _, _, _, err := d.UpsertCheckin(ctx, "RESET-"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddDeviceToGroup(ctx, "RESET-"+suffix, grp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET restaurant_id = $1 WHERE id = $2`, venue.ID, devID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCommandBy(ctx, "reboot", "", nil, "devices", []uuid.UUID{devID}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAudit(ctx, "test", "test.action", "x", ""); err != nil {
		t.Fatal(err)
	}
	user, err := d.CreateUser(ctx, "reset-"+suffix+"@example.com", "x", "viewer", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO access_grants (user_id, effect, scope_type, actions) VALUES ($1,'allow','all','{view}')`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO access_grants (user_id, effect, scope_type, restaurant_id, actions) VALUES ($1,'allow','restaurant',$2,'{view}')`, user.ID, venue.ID); err != nil {
		t.Fatal(err)
	}
	var fleetWindows int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM service_windows WHERE restaurant_id IS NULL AND group_id IS NULL`).Scan(&fleetWindows)

	dry, err := d.FleetReset(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"devices", "restaurants", "groups", "commands", "audit_log"} {
		if dry[tbl] < 1 {
			t.Errorf("dry run: %s counted %d, want at least 1", tbl, dry[tbl])
		}
	}
	var n int
	if d.pool.QueryRow(ctx, `SELECT count(*) FROM devices`).Scan(&n); n == 0 {
		t.Fatal("dry run removed devices")
	}

	if _, err := d.FleetReset(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"devices", "restaurants", "groups", "commands", "command_targets", "device_groups", "audit_log", "enrollment_profiles"} {
		d.pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		if n != 0 {
			t.Errorf("after reset %s has %d rows, want 0", tbl, n)
		}
	}
	if d.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, user.ID).Scan(&n); n != 1 {
		t.Errorf("the user is gone")
	}
	if d.pool.QueryRow(ctx, `SELECT count(*) FROM access_grants WHERE user_id = $1 AND scope_type = 'all'`, user.ID).Scan(&n); n != 1 {
		t.Errorf("the fleet-wide grant is gone")
	}
	if d.pool.QueryRow(ctx, `SELECT count(*) FROM access_grants WHERE user_id = $1 AND scope_type = 'restaurant'`, user.ID).Scan(&n); n != 0 {
		t.Errorf("the grant on the deleted venue stayed")
	}
	if d.pool.QueryRow(ctx, `SELECT count(*) FROM service_windows WHERE restaurant_id IS NULL AND group_id IS NULL`).Scan(&n); n != fleetWindows {
		t.Errorf("fleet-wide service windows: %d, want %d", n, fleetWindows)
	}
}
