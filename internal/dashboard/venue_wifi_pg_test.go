package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

func TestVenueWifiSeal(t *testing.T) {
	h := &Handler{reportSecret: "one"}
	sealed, err := h.sealWifiPassword("s3cretpass")
	if err != nil || strings.Contains(string(sealed), "s3cretpass") {
		t.Fatalf("seal: %v %q", err, sealed)
	}
	if pw, err := h.openWifiPassword(sealed); err != nil || pw != "s3cretpass" {
		t.Fatalf("open: %q %v", pw, err)
	}
	if _, err := (&Handler{reportSecret: "two"}).openWifiPassword(sealed); err == nil {
		t.Fatal("a different SESSION_SECRET must not open it")
	}
}

// TestVenueWifiAgainstPostgres runs the reconciler end to end: add, deliver, change the
// password, remove. Needs MDM_TEST_DATABASE_URL (see db's TestShapedHistoryAgainstPostgres).
func TestVenueWifiAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), reportSecret: "venue-wifi-test"}

	stamp := time.Now().Format("150405.000")
	rest, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "Wifi venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	b := 80
	capable := "WIFI-A" + stamp
	old := "WIFI-B" + stamp
	idA, _, _, _, _, err := d.UpsertCheckin(ctx, capable, "v2.1.099", &b, json.RawMessage(`{"wifi_config_cmd":true}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, old, "v2.1.099", &b, json.RawMessage(`{}`), false, "t7"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{capable, old} {
		if err := d.AssignDeviceToRestaurant(ctx, s, &rest.ID); err != nil {
			t.Fatal(err)
		}
	}

	save := func(pw string) db.VenueWifiNetwork {
		sealed, _ := h.sealWifiPassword(pw)
		n, err := d.SaveVenueWifi(ctx, db.VenueWifiNetwork{RestaurantID: rest.ID, SSID: "Nugget-Staff", Security: "wpa2", Secret: sealed, CreatedBy: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	pending := func() []db.Command {
		cs, err := d.GetPendingCommandsForDevice(ctx, idA)
		if err != nil {
			t.Fatal(err)
		}
		var out []db.Command
		for _, c := range cs {
			if c.Type == "wifi_config" {
				out = append(out, c)
			}
		}
		return out
	}
	devices := func() []db.Device {
		ds, err := d.ListDevices(ctx, db.DeviceFilter{RestaurantID: rest.ID}, 0, 100, "serial", "asc")
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}

	// Added: the capable device gets one command carrying the password; the old one can't.
	n := save("firstpass1")
	h.RunVenueWifi(ctx)
	cs := pending()
	if len(cs) != 1 || !strings.Contains(string(cs[0].Payload), `"password":"firstpass1"`) {
		t.Fatalf("first push: %d %v", len(cs), cs)
	}
	h.RunVenueWifi(ctx) // a second run queues nothing new while that one is pending
	if got := len(pending()); got != 1 {
		t.Fatalf("duplicate push: %d", got)
	}
	rows := h.venueWifiRows(ctx, rest.ID, devices())
	if len(rows) != 1 || rows[0].Unsupported != 1 || rows[0].Pending != 1 || rows[0].Complete() {
		t.Fatalf("rows while pending: %+v", rows)
	}
	if err := d.AckCommand(ctx, cs[0].ID, idA, "completed"); err != nil {
		t.Fatal(err)
	}
	rows = h.venueWifiRows(ctx, rest.ID, devices())
	if rows[0].Done != 1 || !rows[0].Complete() {
		t.Fatalf("rows when done: %+v", rows)
	}

	// The password changes: a new version, so a new push.
	if n2 := save("secondpass2"); n2.ID != n.ID || n2.Version != 2 {
		t.Fatalf("change: %+v", n2)
	}
	h.RunVenueWifi(ctx)
	cs = pending()
	if len(cs) != 1 || !strings.Contains(string(cs[0].Payload), `"password":"secondpass2"`) {
		t.Fatalf("second push: %v", cs)
	}
	_ = d.AckCommand(ctx, cs[0].ID, idA, "completed")

	// Removed: a device that still reports it is told to forget it.
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, capable, "v2.1.099", &b,
		json.RawMessage(`{"wifi_config_cmd":true,"wifi_networks":[{"ssid":"Nugget-Staff","security":"wpa2","connected":false}]}`), false, "t7"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveVenueWifi(ctx, rest.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	h.RunVenueWifi(ctx)
	cs = pending()
	var rm struct{ Action, SSID string }
	if len(cs) == 1 {
		_ = json.Unmarshal(cs[0].Payload, &rm)
	}
	if len(cs) != 1 || rm.Action != "remove" || rm.SSID != "Nugget-Staff" {
		t.Fatalf("remove push: %v", cs)
	}
	if rows := h.venueWifiRows(ctx, rest.ID, devices()); len(rows) != 0 {
		t.Fatalf("a removed network is not on the card: %+v", rows)
	}
}
