package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// A Standard client is a scout only when it lists net_scout; firmware comes first.
func TestScoutCandidatesStandardAgainstPostgres(t *testing.T) {
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
	var rid uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('Scout Test Venue') RETURNING id`).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, rid)
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'SCOUT-%'`)
	mk := func(serial, kind, caps string) {
		if _, err := d.pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, capabilities, restaurant_id) VALUES ($1, $2, $3::jsonb, $4)`,
			serial, kind, caps, rid); err != nil {
			t.Fatal(err)
		}
	}
	mk("SCOUT-DPC-OLD", "dpc", `["kiosk"]`)
	mk("SCOUT-DPC-NEW", "dpc", `["kiosk","net_scout"]`)

	got, err := d.ScoutCandidates(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Serial != "SCOUT-DPC-NEW" {
		t.Fatalf("candidates = %+v", got)
	}
	venues, err := d.RestaurantsToScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range venues {
		found = found || v == rid
	}
	if !found {
		t.Fatal("venue with a Standard scout not in RestaurantsToScan")
	}

	mk("SCOUT-FW", "firmware", `[]`)
	got, err = d.ScoutCandidates(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Serial != "SCOUT-FW" {
		t.Fatalf("firmware not first: %+v", got)
	}
	rows, err := d.ListVenueAutoEnroll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == rid && r.Scouts != 2 {
			t.Fatalf("scouts = %d, want 2", r.Scouts)
		}
	}
}
