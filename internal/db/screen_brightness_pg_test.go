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
		t.Fatalf("new device must be unmanaged: %+v %v", cfg, err)
	}
	lvl := 122
	if err := d.SetScreenBrightness(ctx, id, &lvl, true); err != nil {
		t.Fatal(err)
	}
	cfg, err = d.GetOrCreateDeviceConfig(ctx, id)
	if err != nil || cfg.ScreenBrightness == nil || *cfg.ScreenBrightness != 122 || !cfg.ScreenBrightnessLocked {
		t.Fatalf("set 122 locked: %+v %v", cfg, err)
	}
	// Not managed drops the lock with the level.
	if err := d.SetScreenBrightness(ctx, id, nil, true); err != nil {
		t.Fatal(err)
	}
	cfg, err = d.GetOrCreateDeviceConfig(ctx, id)
	if err != nil || cfg.ScreenBrightness != nil || cfg.ScreenBrightnessLocked {
		t.Fatalf("cleared: %+v %v", cfg, err)
	}
}
