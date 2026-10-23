package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGuestInfoAgainstPostgres places a device at a venue and reads its guest info back
// the way the check-in and the dashboard pushes do (see
// TestShapedHistoryAgainstPostgres for how to start a throwaway database).
func TestGuestInfoAgainstPostgres(t *testing.T) {
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

	serial := "GUEST" + time.Now().Format("150405")
	b := 70
	id, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}

	// A lab unit: no venue, so no venue name.
	g, err := d.GuestForDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if g.AgentKind != "firmware" || g.Guest != (GuestInfo{}) {
		t.Fatalf("lab unit: %+v", g)
	}

	rest, err := d.CreateRestaurant(ctx, Restaurant{Name: "Guest Diner"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteRestaurant(ctx, rest.ID)
	if err := d.AssignDeviceToRestaurant(ctx, serial, &rest.ID); err != nil {
		t.Fatal(err)
	}
	// Whatever is left in the old guest Wi-Fi columns is never sent (removed 19 Oct).
	if _, err := d.pool.Exec(ctx, `UPDATE restaurants SET guest_wifi_ssid = 'Diner-Guest', guest_wifi_password = 'pizza1234' WHERE id = $1`, rest.ID); err != nil {
		t.Fatal(err)
	}
	// A placed device gets the venue's name and its hours (the fleet default here, since
	// 20 Oct).
	ok := func(g GuestInfo) bool {
		return g.RestaurantName == "Guest Diner" && g.ServiceHours != nil && g.ServiceHours.Open == "07:00" && g.ServiceHours.Close == "23:00"
	}
	if g, err := d.GuestForDevice(ctx, id); err != nil || !ok(g.Guest) {
		t.Fatalf("GuestForDevice = %+v, %v", g.Guest, err)
	}
	if gs, err := d.GuestForRestaurant(ctx, rest.ID); err != nil || len(gs) != 1 || gs[0].DeviceID != id || !ok(gs[0].Guest) {
		t.Fatalf("GuestForRestaurant = %+v, %v", gs, err)
	}
	if gs, err := d.GuestForDevices(ctx, []uuid.UUID{id}); err != nil || len(gs) != 1 || !ok(gs[0].Guest) {
		t.Fatalf("GuestForDevices = %+v, %v", gs, err)
	}

	// Deleting the venue sends the tablet back to the lab: no venue name any more.
	if err := d.DeleteRestaurant(ctx, rest.ID); err != nil {
		t.Fatal(err)
	}
	g, _ = d.GuestForDevice(ctx, id)
	if g.Guest.RestaurantName != "" {
		t.Fatalf("after deleting the venue: %+v", g.Guest)
	}
}
