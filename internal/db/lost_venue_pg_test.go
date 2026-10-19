package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A lost_mode command to one device carries the device's venue and icon version for the
// lost screen, and the config's self_lock carries the same; an icon change moves the
// version and the config generation (client 1.13.4, L1).
func TestLostVenueAgainstPostgres(t *testing.T) {
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
	serial := "LOSTV-" + time.Now().Format("150405.000")
	b := 70
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	rest, err := d.CreateRestaurant(ctx, Restaurant{Name: "Pho 88"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteRestaurant(ctx, rest.ID)
	gen := d.LostConfigGen()
	if err := d.AssignDeviceToRestaurant(ctx, serial, &rest.ID); err != nil {
		t.Fatal(err)
	}
	if d.LostConfigGen() == gen {
		t.Fatal("moving a device to a venue must move the lost config generation")
	}

	lost := func() map[string]any {
		cmd, err := d.CreateCommandBy(ctx, "lost_mode", "", json.RawMessage(`{"on":true,"message":"m"}`), "devices", []uuid.UUID{dev}, "test")
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		_ = json.Unmarshal(cmd.Payload, &p)
		return p
	}
	if p := lost(); p["venue"] != "Pho 88" || p["venue_icon_v"] != float64(0) || p["beacon"] == nil {
		t.Fatalf("lost_mode without an icon: %v", p)
	}
	gen = d.LostConfigGen()
	if err := d.SetRestaurantIcon(ctx, rest.ID, []byte("\x89PNG fake"), "test"); err != nil {
		t.Fatal(err)
	}
	if d.LostConfigGen() == gen {
		t.Fatal("setting an icon must move the lost config generation")
	}
	p := lost()
	if v, _ := p["venue_icon_v"].(float64); v <= 0 {
		t.Fatalf("lost_mode with an icon: %v", p)
	}
	c, err := d.LostConfigFor(ctx, dev)
	if err != nil || c.Venue != "Pho 88" || c.VenueIconV != int64(p["venue_icon_v"].(float64)) {
		t.Fatalf("LostConfigFor = %+v, %v", c, err)
	}
	// Lifting carries nothing extra.
	off, err := d.CreateCommandBy(ctx, "lost_mode", "", json.RawMessage(`{"on":false}`), "devices", []uuid.UUID{dev}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var lp map[string]any
	_ = json.Unmarshal(off.Payload, &lp)
	if len(lp) != 1 || lp["on"] != false {
		t.Fatalf("lift payload = %s", off.Payload)
	}
}
