package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// One update after another (27 Oct 2026): a row waiting for another update is not served
// until that one is installed on the device.
func TestUpdateAfterAgainstPostgres(t *testing.T) {
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
	sfx := time.Now().Format("150405")
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "AFTER-"+sfx, "v2.0.96l", nil, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(build string) *Update {
		p, err := d.CreateOTAPackage(ctx, "full", build+"-"+sfx, "", "https://x/"+build+".zip", "", "t7", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE releases SET status = 'published', published_at = NOW() WHERE id = $1`, p.ReleaseID); err != nil {
			t.Fatal(err)
		}
		u, err := d.CreateReleaseUpdate(ctx, p.ReleaseID, "immediate", nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SendUpdateToDevices(ctx, u.ID, []uuid.UUID{dev}, false); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1`, u.ID); err != nil {
			t.Fatal(err)
		}
		return u
	}
	second := mk("v2.1.041") // queued first, like #490
	first := mk("v2.0.997")  // added later, to go before it
	if err := d.SetUpdateDeviceAfter(ctx, second.ID, dev, first.ID); err != nil {
		t.Fatal(err)
	}
	got, err := d.ResolveUpdateForDevice(ctx, dev)
	if err != nil || got == nil || got.ID != first.ID {
		t.Fatalf("first resolve = %+v, %v; want update %d", got, err, first.ID)
	}
	ts, _ := d.GetUpdateTargets(ctx, second.ID)
	if len(ts) != 1 || !ts[0].AfterWaiting || ts[0].AfterUpdateID == nil || *ts[0].AfterUpdateID != first.ID {
		t.Fatalf("second's row = %+v", ts)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE update_devices SET status = 'installed' WHERE update_id = $1 AND device_id = $2`, first.ID, dev); err != nil {
		t.Fatal(err)
	}
	got, err = d.ResolveUpdateForDevice(ctx, dev)
	if err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("after the first installed = %+v, %v; want update %d", got, err, second.ID)
	}
	if ts, _ := d.GetUpdateTargets(ctx, second.ID); ts[0].AfterWaiting {
		t.Error("still waiting after the first installed")
	}
}
