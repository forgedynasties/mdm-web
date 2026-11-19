package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestVenueAppAutoInstall(t *testing.T) {
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
	r, err := d.CreateRestaurant(ctx, Restaurant{Name: "AutoApp " + time.Now().Format("150405000"), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteRestaurant(ctx, r.ID)
	if on, err := d.VenueAppAutoInstall(ctx, r.ID); err != nil || !on {
		t.Fatalf("default = %v, %v; want on", on, err)
	}
	if err := d.SetVenueAppAutoInstall(ctx, r.ID, false); err != nil {
		t.Fatal(err)
	}
	off, err := d.VenueAppAutoInstallOff(ctx)
	if err != nil || !off[r.ID] {
		t.Fatalf("off set = %v, %v; want venue in it", off, err)
	}
	if err := d.SetVenueAppAutoInstall(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if off, _ = d.VenueAppAutoInstallOff(ctx); off[r.ID] {
		t.Fatal("venue still off after turning on")
	}
}
