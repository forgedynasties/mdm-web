package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run ScreenBrightness
func TestScreenBrightnessAgainstPostgres(t *testing.T) {
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
	b := 80
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "BRI-"+time.Now().Format("150405.000"), "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := d.GetOrCreateDeviceConfig(ctx, id)
	if err != nil || cfg.ScreenBrightness != nil || cfg.ScreenBrightnessLocked {
		t.Fatalf("new device has no level of its own: %+v %v", cfg, err)
	}
	// A Tableside AI tablet with no level of its own is held at 122, locked (21 Oct).
	if cfg.DeviceClass != "t7" {
		t.Fatalf("class read with the config = %q, want t7", cfg.DeviceClass)
	}
	if b := cfg.Brightness(); b.Level != 122 || !b.Locked || !b.Default || b.Max != 122 {
		t.Fatalf("default = %+v", b)
	}
	if lvl, locked := cfg.BrightnessConfig(); lvl != 122 || !locked {
		t.Fatalf("config keys = %v %v", lvl, locked)
	}
	lvl := 80
	if err := d.SetScreenBrightness(ctx, id, &lvl, true, "ali"); err != nil {
		t.Fatal(err)
	}
	cfg, err = d.GetOrCreateDeviceConfig(ctx, id)
	if err != nil || cfg.ScreenBrightness == nil || *cfg.ScreenBrightness != 80 || !cfg.ScreenBrightnessLocked ||
		cfg.ScreenBrightnessSetBy != "ali" || cfg.ScreenBrightnessSetAt == nil {
		t.Fatalf("set 80 locked by ali: %+v %v", cfg, err)
	}
	if b := cfg.Brightness(); b.Level != 80 || b.Default {
		t.Fatalf("own level = %+v", b)
	}
	// Back to the default drops the level, the lock flag and who set it.
	if err := d.SetScreenBrightness(ctx, id, nil, true, "ali"); err != nil {
		t.Fatal(err)
	}
	cfg, err = d.GetOrCreateDeviceConfig(ctx, id)
	if err != nil || cfg.ScreenBrightness != nil || cfg.ScreenBrightnessLocked || cfg.ScreenBrightnessSetBy != "" || cfg.ScreenBrightnessSetAt != nil {
		t.Fatalf("back to default: %+v %v", cfg, err)
	}
	if b := cfg.Brightness(); b.Level != 122 || !b.Default {
		t.Fatalf("after reset = %+v", b)
	}

	// The re-lock job sees a 1.14 client that reports 59, unlocked: drifted. A kiosk
	// with no level of its own is not managed, so never drifts.
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET latest_extra = '{"screen_brightness": 59, "screen_brightness_locked": false}', last_seen_at = NOW() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	kid, _, _, _, _, err := d.UpsertCheckin(ctx, "BRK-"+time.Now().Format("150405.000"), "v2.1.099", &b, nil, false, "kiosk22")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET latest_extra = '{"screen_brightness": 200, "screen_brightness_locked": false}', last_seen_at = NOW() WHERE id = $1`, kid); err != nil {
		t.Fatal(err)
	}
	states, err := d.BrightnessStates(ctx, 600)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, st := range states {
		switch st.ID {
		case id:
			seen["t7"] = true
			if !st.Drifted() || st.Reported == nil || *st.Reported != 59 {
				t.Errorf("tablet at 59 unlocked should be drifted from 122 locked: %+v", st)
			}
		case kid:
			seen["kiosk"] = true
			if st.Drifted() {
				t.Errorf("a kiosk on the default is not managed: %+v", st)
			}
		}
	}
	if !seen["t7"] || !seen["kiosk"] {
		t.Errorf("states missing devices: %v", seen)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET latest_extra = '{"screen_brightness": 122, "screen_brightness_locked": true}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	states, _ = d.BrightnessStates(ctx, 600)
	for _, st := range states {
		if st.ID == id && st.Drifted() {
			t.Errorf("tablet at 122 locked is on its level: %+v", st)
		}
	}
}
