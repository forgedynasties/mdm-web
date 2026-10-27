package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// H1 (27 Oct 2026): a venue's one channel becomes that channel per product, and the map
// round-trips with prod left out.
func TestVenueHostChannelsAgainstPostgres(t *testing.T) {
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
	var id uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name, host_app_channel) VALUES ('H1 Lab', 'uatv2') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, id)
	if err := d.RunMigrations(ctx); err != nil { // the fold runs on the next start
		t.Fatal(err)
	}
	got, err := d.VenueHostChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[id]["t7"] != "uatv2" || got[id]["kiosk"] != "uatv2" {
		t.Fatalf("folded = %v", got[id])
	}
	if err := d.SetVenueHostChannels(ctx, id, map[string]string{"t7": "uatv2", "kiosk": ""}); err != nil {
		t.Fatal(err)
	}
	got, _ = d.VenueHostChannels(ctx)
	if len(got[id]) != 1 || got[id]["t7"] != "uatv2" {
		t.Fatalf("after save = %v", got[id])
	}
	if err := d.SetVenueHostChannels(ctx, id, map[string]string{"t7": ""}); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.VenueHostChannels(ctx); got[id] != nil {
		t.Fatalf("all prod = %v", got[id])
	}
}
