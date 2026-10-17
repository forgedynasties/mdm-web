package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLostModeAgainstPostgres: the fleet's "Lost" filter and the device page's lookup of
// who marked it lost. Needs MDM_TEST_DATABASE_URL.
func TestLostModeAgainstPostgres(t *testing.T) {
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
	b := 50
	serial := "LOSTF" + time.Now().Format("150405.000")
	id, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{"lost_mode_cmd":true}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := d.LatestDeviceCommand(ctx, id, "lost_mode"); err != nil || c != nil {
		t.Fatalf("none yet: %v %v", c, err)
	}
	cmd, err := d.CreateCommandBy(ctx, "lost_mode", "", json.RawMessage(`{"on":true,"message":"Hand it to staff","phone":"+1 415 555 0100"}`), "devices", []uuid.UUID{id}, "Sara")
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.LatestDeviceCommand(ctx, id, "lost_mode")
	if err != nil || c == nil || c.ID != cmd.ID || c.CreatedBy != "Sara" || c.Status != "" {
		t.Fatalf("latest: %+v %v", c, err)
	}
	only := []uuid.UUID{id}
	if n, _ := d.CountDevices(ctx, DeviceFilter{Kiosk: "lost", OnlyIDs: only}); n != 0 {
		t.Fatalf("not lost until the device says so: %d", n)
	}
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{"lost_mode_cmd":true,"lost_mode":{"on":true,"since_ms":1760700000000}}`), false, "t7"); err != nil {
		t.Fatal(err)
	}
	_ = d.AckCommand(ctx, cmd.ID, id, "completed")
	if n, _ := d.CountDevices(ctx, DeviceFilter{Kiosk: "lost", OnlyIDs: only}); n != 1 {
		t.Fatalf("lost filter: %d", n)
	}
	if c, _ := d.LatestDeviceCommand(ctx, id, "lost_mode"); c == nil || c.Status != "completed" {
		t.Fatalf("status after ack: %+v", c)
	}
}

// TestCapsByIDsSeeFirmwareFlags: the admin API's capability gate loads devices with
// GetDevicesByIDs, which must carry the extra flags the firmware client declares lost_mode
// and wifi_config with (found on stage 17 Oct: the API refused both to a 1.12.1 client).
func TestCapsByIDsSeeFirmwareFlags(t *testing.T) {
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
	b := 50
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "CAPS"+time.Now().Format("150405.000"), "v2.1.099", &b,
		json.RawMessage(`{"lost_mode_cmd":true,"wifi_config_cmd":true,"factory_reset":true,"big":"ignored"}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	devs, err := d.GetDevicesByIDs(ctx, []uuid.UUID{id})
	if err != nil || len(devs) != 1 {
		t.Fatalf("%v %v", devs, err)
	}
	for _, c := range []string{"lost_mode", "wifi_config", "wipe"} {
		if !devs[0].Supports(c) {
			t.Errorf("GetDevicesByIDs device does not support %s (extra %s)", c, devs[0].LatestExtra)
		}
	}
}
