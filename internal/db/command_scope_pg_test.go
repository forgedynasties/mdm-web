package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCommandScopeAgainstPostgres: a device added to a group after a group command (or
// enrolled after a fleet-wide one) does not inherit it in its history or queue, and an MDM
// agent update counts as an install in flight. See TestServerAndAlertQueriesAgainstPostgres
// for how to run it.
func TestCommandScopeAgainstPostgres(t *testing.T) {
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
	got, _, _, _, _, err := d.UpsertCheckin(ctx, "SCOPEA"+stamp, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	late, _, _, _, _, err := d.UpsertCheckin(ctx, "SCOPEB"+stamp, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	g, err := d.CreateGroup(ctx, "scope "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddDeviceToGroup(ctx, "SCOPEA"+stamp, g.ID); err != nil {
		t.Fatal(err)
	}
	old, err := d.CreateCommandBy(ctx, "screenshot", "", nil, "groups", []uuid.UUID{g.ID}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = now() - interval '3 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCommandStatusForDevices(ctx, old.ID, []uuid.UUID{got}, "completed", true); err != nil {
		t.Fatal(err)
	}
	if err := d.AddDeviceToGroup(ctx, "SCOPEB"+stamp, g.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.CreateCommandBy(ctx, "screenshot", "", nil, "groups", []uuid.UUID{g.ID}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	has := func(dev, cmd uuid.UUID) bool {
		cmds, err := d.GetDeviceCommands(ctx, dev, 300)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cmds {
			if c.ID == cmd {
				return true
			}
		}
		return false
	}
	if !has(got, old.ID) {
		t.Error("the device that ran the old group command lost it from its history")
	}
	if has(late, old.ID) {
		t.Error("a device added to the group later inherited its 3-day-old command")
	}
	if !has(late, fresh.ID) {
		t.Error("a group command still being delivered is missing for a member")
	}

	upd, err := d.CreateCommandBy(ctx, "app_update", "https://example/agent.apk", nil, "devices", []uuid.UUID{late}, "client-auto-update")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := d.InFlightInstalls(ctx, []uuid.UUID{got, late})
	if err != nil {
		t.Fatal(err)
	}
	if !busy[late] || busy[got] {
		t.Errorf("in flight = %v, want only the device with the agent update (%s)", busy, upd.ID)
	}
}
