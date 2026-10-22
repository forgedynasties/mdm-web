package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPresenceAgainstPostgres(t *testing.T) {
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
	var id uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class, last_seen_at) VALUES ('PR-1', 't7', NOW() - interval '1 hour') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'PR-1'`)
	find := func() PresenceRow {
		rows, err := d.PresenceRows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return r
			}
		}
		t.Fatal("device missing")
		return PresenceRow{}
	}
	if r := find(); r.Recorded != "" {
		t.Errorf("recorded = %q before any change", r.Recorded)
	}
	if err := d.RecordPresence(ctx, id, time.Now().Add(-2*time.Hour), true, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPresence(ctx, id, time.Now().Add(-time.Hour), false, "true"); err != nil {
		t.Fatal(err)
	}
	if r := find(); r.Recorded != "false" {
		t.Errorf("recorded = %q, want the latest (false)", r.Recorded)
	}
	ev, err := d.GetStateTimeline(ctx, id, []string{PresenceKey}, time.Now().Add(-90*time.Minute), time.Now())
	if err != nil || len(ev) != 2 || ev[0].Value != "true" || ev[1].Value != "false" {
		t.Errorf("timeline = %+v, %v; want the carried-in true then false", ev, err)
	}
}
