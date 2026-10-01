package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGuestInfoAgainstPostgres stores a venue's guest Wi-Fi and a device's table and
// reads them back the way the check-in and the dashboard pushes do (see
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

	// A lab unit: no venue, no table — every field empty, nothing about a network.
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
	if err := d.SetTableLabel(ctx, id, "  Table 12  "); err != nil {
		t.Fatal(err)
	}
	wifi, err := NormalizeGuestWifi(RestaurantGuestWifi{SSID: "Diner-Guest", Password: "pizza1234", AppPackage: "aio.app.nugget.uatv2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRestaurantGuestWifi(ctx, rest.ID, wifi); err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRestaurantGuestWifi(ctx, rest.ID); err != nil || got != wifi {
		t.Fatalf("stored wifi = %+v, %v; want %+v", got, err, wifi)
	}

	want := GuestInfo{RestaurantName: "Guest Diner", TableLabel: "Table 12", WifiSSID: "Diner-Guest",
		WifiPassword: "pizza1234", WifiSecurity: "WPA", AppPackage: "aio.app.nugget.uatv2"}
	if g, err := d.GuestForDevice(ctx, id); err != nil || g.Guest != want {
		t.Fatalf("GuestForDevice = %+v, %v; want %+v", g.Guest, err, want)
	}
	if gs, err := d.GuestForRestaurant(ctx, rest.ID); err != nil || len(gs) != 1 || gs[0].DeviceID != id || gs[0].Guest != want {
		t.Fatalf("GuestForRestaurant = %+v, %v", gs, err)
	}
	if gs, err := d.GuestForDevices(ctx, []uuid.UUID{id}); err != nil || len(gs) != 1 || gs[0].Guest != want {
		t.Fatalf("GuestForDevices = %+v, %v", gs, err)
	}

	// Clearing the network clears every Wi-Fi field the device is sent.
	cleared, _ := NormalizeGuestWifi(RestaurantGuestWifi{})
	if err := d.SetRestaurantGuestWifi(ctx, rest.ID, cleared); err != nil {
		t.Fatal(err)
	}
	g, _ = d.GuestForDevice(ctx, id)
	if g.Guest.WifiSSID != "" || g.Guest.WifiPassword != "" || g.Guest.WifiSecurity != "" || g.Guest.RestaurantName != "Guest Diner" {
		t.Fatalf("after clearing the network: %+v", g.Guest)
	}

	// Deleting the venue sends the tablet back to the lab: no venue name any more.
	if err := d.DeleteRestaurant(ctx, rest.ID); err != nil {
		t.Fatal(err)
	}
	g, _ = d.GuestForDevice(ctx, id)
	if g.Guest.RestaurantName != "" || g.Guest.TableLabel != "Table 12" {
		t.Fatalf("after deleting the venue: %+v", g.Guest)
	}
}
