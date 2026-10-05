package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A diagnostic query sent to devices that never answered used to show pending / delivered
// for ever, because 'query' was missing from the types the command views expire. Run it
// with the Postgres from server_alerts_pg_test.go:
//
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run CommandExpiry
func TestShortLivedCommandsExpireInEveryView(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	b := 80
	reached, _, _, _, _, err := d.UpsertCheckin(ctx, "EXP-A"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	missed, _, _, _, _, err := d.UpsertCheckin(ctx, "EXP-B"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}

	for i, typ := range []string{"query", "mic_gain_read", "shell"} {
		cmd, err := d.CreateCommandBy(ctx, typ, "", []byte(`{"cmd":"true"}`), "devices", []uuid.UUID{reached, missed}, "test")
		if err != nil {
			t.Fatal(err)
		}
		// One device was sent it and never answered; the other was never reached at all.
		if err := d.MarkCommandsDelivered(ctx, reached, []uuid.UUID{cmd.ID}); err != nil {
			t.Fatal(err)
		}

		// Fresh: still in flight.
		dels, err := d.GetCommandDeliveries(ctx, cmd.ID, 300)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, x := range dels {
			seen[x.Status]++
		}
		if seen["pending"] != 1 || seen["delivered"] != 1 {
			t.Fatalf("%s fresh: want one pending and one delivered, got %v", typ, seen)
		}

		// Two hours on, nothing will deliver it any more: every view must say expired.
		if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, cmd.ID); err != nil {
			t.Fatal(err)
		}
		dels, err = d.GetCommandDeliveries(ctx, cmd.ID, 300)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range dels {
			if x.Status != "expired" {
				t.Errorf("%s deliveries: %s is %q, want expired", typ, x.SerialNumber, x.Status)
			}
		}
		// A different window each time: the rollup is cached for a few seconds per window.
		sums, err := d.GetCommandDeliverySummaries(ctx, 300, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := sums[cmd.ID]; !ok || s.Delivered != 0 {
			t.Errorf("%s summary: the delivered row must not count as in flight, got %+v", typ, s)
		}
		mine, err := d.GetDeviceCommands(ctx, reached, 300)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range mine {
			// shell keeps its old device-page behaviour (delivered stays delivered there).
			if c.ID == cmd.ID && typ != "shell" && c.Status != "expired" {
				t.Errorf("%s device page: %q, want expired", typ, c.Status)
			}
		}
	}
}
