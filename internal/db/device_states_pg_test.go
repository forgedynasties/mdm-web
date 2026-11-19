package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Device states (simplified 19 Nov 2026): a device is To place (no venue) or In service (at a venue), and placing
// it at a venue puts it in service at once. The one-shot migration drops the Setting up grants
// and stamps the devices that were still waiting for a hand-over.
func TestDeviceStates(t *testing.T) {
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
	if err := d.RunMigrations(ctx); err != nil { // idempotent at every start
		t.Fatalf("second migration run: %v", err)
	}
	sfx := time.Now().Format("150405000")
	serial := "SV" + sfx
	id, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	venue, err := d.CreateRestaurant(ctx, Restaurant{Name: "State venue " + sfx})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		sc, err := d.DeviceScopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sc[id].State()
	}
	if s := state(); s != StateToPlace {
		t.Fatalf("new device: %s, want to place (no Lab state)", s)
	}
	// An old ?state=lab link filters nothing and does not fail.
	if _, err := d.CountDevices(ctx, DeviceFilter{State: "lab"}); err != nil {
		t.Fatalf("lab filter: %v", err)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, &venue.ID); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != StateInService {
		t.Fatalf("placed: %s, want in service", s)
	}
	// The state filter agrees, and an old ?state=setting_up filters nothing.
	n, err := d.CountDevices(ctx, DeviceFilter{State: StateInService, RestaurantID: venue.ID})
	if err != nil || n != 1 {
		t.Fatalf("in service filter: %d %v", n, err)
	}
	if _, err := d.CountDevices(ctx, DeviceFilter{State: "setting_up"}); err != nil {
		t.Fatalf("setting_up filter: %v", err)
	}
	// Even a row the old hand-over left NULL is in service.
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET handed_over_at = NULL, handed_over_by = '' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != StateInService {
		t.Fatalf("not handed over: %s, want in service", s)
	}
	// The migration, run again after its marker was cleared, stamps it and drops a Setting up
	// grant (Field's old one) without touching the others.
	if _, err := d.pool.Exec(ctx, `INSERT INTO access_levels (subject_type, subject, scope_type, state, level, created_by)
		VALUES ('user', $1, 'state', 'setting_up', 'manage', 'test'), ('user', $1, 'state', 'in_service', 'view', 'test')`, "u"+sfx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM access_seed WHERE k = 'no-setting-up-v1'`); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var by string
	var at *time.Time
	if err := d.pool.QueryRow(ctx, `SELECT handed_over_at, handed_over_by FROM devices WHERE id = $1`, id).Scan(&at, &by); err != nil || at == nil || by != "migrated" {
		t.Fatalf("migration stamp: %v %q %v", at, by, err)
	}
	var gone, kept int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM access_levels WHERE state = 'setting_up'`).Scan(&gone)
	d.pool.QueryRow(ctx, `SELECT count(*) FROM access_levels WHERE subject = $1 AND state = 'in_service'`, "u"+sfx).Scan(&kept)
	if gone != 0 || kept != 1 {
		t.Fatalf("grants after migration: %d setting_up, %d in_service kept", gone, kept)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, &venue.ID); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != StateInService {
		t.Fatalf("placed again: %s, want in service", s)
	}
}
