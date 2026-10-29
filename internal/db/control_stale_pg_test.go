package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestStaleControlRefreshThrottle: a device with an open control_stale alert refreshed in the
// last 15 minutes is left out of the list, so the job does not rewrite every alert every minute.
func TestStaleControlRefreshThrottle(t *testing.T) {
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
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "CSTALE"+time.Now().Format("150405.000"), "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	listed := func() bool {
		t.Helper()
		all, err := d.StaleControlDevices(ctx, 48*time.Hour, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			if s.DeviceID == dev {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("a reporting device with no round trip should be listed")
	}
	if _, _, err := d.CreateOrEscalateAlert(ctx, nil, "control_stale", dev, "warning", "x", nil); err != nil {
		t.Fatal(err)
	}
	if listed() {
		t.Fatal("an alert refreshed just now should keep the device off the list")
	}
	if _, err := d.pool.Exec(ctx, `UPDATE alerts SET last_seen_at = NOW() - INTERVAL '20 minutes' WHERE type = 'control_stale' AND device_id = $1`, dev); err != nil {
		t.Fatal(err)
	}
	if !listed() {
		t.Fatal("an alert last refreshed 20 minutes ago is due again")
	}
}
