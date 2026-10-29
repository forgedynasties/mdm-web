package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDevicesWithLiveCommandsAgainstPostgres: only devices with an unfinished command from
// the last hour (direct or by group) pass the filter.
func TestDevicesWithLiveCommandsAgainstPostgres(t *testing.T) {
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
	stamp := time.Now().Format("150405.000")
	b := 50
	mk := func(p string) uuid.UUID {
		id, _, _, _, _, err := d.UpsertCheckin(ctx, p+stamp, "v2.1.099", &b, nil, false, "t7")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending, done, grouped, idle, old := mk("LCP"), mk("LCD"), mk("LCG"), mk("LCI"), mk("LCO")
	if _, err := d.CreateCommandBy(ctx, "shell", "", nil, "devices", []uuid.UUID{pending}, "t"); err != nil {
		t.Fatal(err)
	}
	c2, err := d.CreateCommandBy(ctx, "shell", "", nil, "devices", []uuid.UUID{done}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetCommandStatusForDevices(ctx, c2.ID, []uuid.UUID{done}, "completed", true); err != nil {
		t.Fatal(err)
	}
	g, err := d.CreateGroup(ctx, "live "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddDeviceToGroup(ctx, "LCG"+stamp, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCommandBy(ctx, "shell", "", nil, "groups", []uuid.UUID{g.ID}, "t"); err != nil {
		t.Fatal(err)
	}
	c4, err := d.CreateCommandBy(ctx, "shell", "", nil, "devices", []uuid.UUID{old}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, c4.ID); err != nil {
		t.Fatal(err)
	}
	got, err := d.DevicesWithLiveCommands(ctx, []uuid.UUID{pending, done, grouped, idle, old})
	if err != nil {
		t.Fatal(err)
	}
	set := map[uuid.UUID]bool{}
	for _, id := range got {
		set[id] = true
	}
	if !set[pending] || !set[grouped] || set[done] || set[idle] || set[old] {
		t.Errorf("live = %v; want only the pending and the grouped device", got)
	}
}
