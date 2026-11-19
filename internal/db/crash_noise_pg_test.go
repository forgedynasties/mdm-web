package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestIsKnownCrashNoise(t *testing.T) {
	noise := "com.google.android.gms.persistent — java.lang.NullPointerException: Attempt to invoke virtual method 'android.location.provider.ProviderProperties com.android.server.location.provider.LocationProviderManager.getProperties()' on a null object reference"
	if !isKnownCrashNoise(noise) {
		t.Fatal("the GMS location NPE must be noise")
	}
	for _, s := range []string{
		"com.google.android.gms.persistent — java.lang.NullPointerException: something else",
		"aio.app.kiosk — java.lang.NullPointerException: Attempt to invoke virtual method 'x'",
		"",
	} {
		if isKnownCrashNoise(s) {
			t.Fatalf("%q must not be noise", s)
		}
	}
}

// A noisy crash is not stored when a device reports it, a real one is, and the one-shot
// migration removes the noise rows that were stored before.
func TestCrashNoiseIngestAndCleanup(t *testing.T) {
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
	sfx := time.Now().Format("150405000")
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "CN"+sfx, "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	noise := "com.google.android.gms.persistent — java.lang.NullPointerException: Attempt to invoke virtual method 'android.location.provider.ProviderProperties com.android.server.location.provider.LocationProviderManager.getProperties()' on a null object reference"
	now := time.Now().UnixMilli()
	extra, _ := json.Marshal(map[string]any{"crash_events": []map[string]any{
		{"kind": "system_app_crash", "time_ms": now - 5000, "summary": noise, "trace": "t"},
		{"kind": "system_app_crash", "time_ms": now - 4000, "summary": "aio.app.kiosk — java.lang.IllegalStateException: boom", "trace": "t"},
	}})
	d.IngestDeviceEvents(ctx, id, "b1", extra, time.Now())
	var n int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM device_events WHERE device_id = $1 AND kind = 'system_app_crash'`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("stored %d crash rows, want 1 (the real one)", n)
	}
	// An old noise row, then the migration with its marker cleared.
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_events (device_id, kind, summary, detail, build_id, occurred_at) VALUES ($1, 'system_app_crash', $2, '', 'b1', now() - interval '1 hour')`, id, noise); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM access_seed WHERE k = 'crash-noise-gms-location-v1'`); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	d.pool.QueryRow(ctx, `SELECT count(*) FROM device_events WHERE device_id = $1 AND kind = 'system_app_crash'`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("after the migration %d crash rows, want 1", n)
	}
}

func TestGeocodeCacheAndDailyCount(t *testing.T) {
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
	cell := "t" + time.Now().Format("150405.000")
	if _, ok, _ := d.GetGeocodedAddress(ctx, cell, 30); ok {
		t.Fatal("unexpected hit")
	}
	if err := d.PutGeocodedAddress(ctx, cell, "1 Main St"); err != nil {
		t.Fatal(err)
	}
	if a, ok, err := d.GetGeocodedAddress(ctx, cell, 30); err != nil || !ok || a != "1 Main St" {
		t.Fatalf("get: %q %v %v", a, ok, err)
	}
	d.pool.Exec(ctx, `UPDATE geocode_cache SET updated_at = now() - interval '40 days' WHERE cell = $1`, cell)
	if _, ok, _ := d.GetGeocodedAddress(ctx, cell, 30); ok {
		t.Fatal("an address older than the window must not be used")
	}
	n1, err := d.CountGeocodeCall(ctx)
	n2, _ := d.CountGeocodeCall(ctx)
	if err != nil || n2 != n1+1 {
		t.Fatalf("count: %d %d %v", n1, n2, err)
	}
}

// Two server copies record the same online flip at the same instant: the second is a no-op.
func TestRecordPresenceTwiceIsNoError(t *testing.T) {
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
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "PR"+time.Now().Format("150405000"), "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	if err := d.RecordPresence(ctx, id, at, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPresence(ctx, id, at, false, ""); err != nil {
		t.Fatalf("the same flip written twice must not error: %v", err)
	}
	var n int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM device_state_events WHERE device_id = $1 AND key = 'online'`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}
