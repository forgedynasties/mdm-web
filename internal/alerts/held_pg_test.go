package alerts

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/db"
)

// TestDispatchSkipsPausedVenue: a notification for a device at a venue whose alerts are
// paused is dropped before any channel or webhook sees it; the others stay.
func TestDispatchSkipsPausedVenue(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var venue, paused, open, free uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('DSP Lab') RETURNING id`).Scan(&venue); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, venue)
	for _, x := range []struct {
		p *uuid.UUID
		s string
		r any
	}{{&paused, "DSP-1", venue}, {&open, "DSP-2", venue}, {&free, "DSP-3", nil}} {
		if err := pool.QueryRow(ctx, `INSERT INTO devices (serial_number, restaurant_id) VALUES ($1, $2) RETURNING id`, x.s, x.r).Scan(x.p); err != nil {
			t.Fatal(err)
		}
	}
	defer pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'DSP-%'`)
	dp := NewDispatcher(d, nil)
	all := []db.AlertNotification{{DeviceID: paused}, {DeviceID: free}}
	if got := dp.withoutHeld(ctx, all); len(got) != 2 {
		t.Fatalf("not paused: %d notifications kept, want 2", len(got))
	}
	until := time.Now().Add(time.Hour)
	if err := d.SetVenueAlertsPause(ctx, venue, db.VenueAlertsPause{Paused: true, PausedUntil: &until}); err != nil {
		t.Fatal(err)
	}
	got := dp.withoutHeld(ctx, append(all, db.AlertNotification{DeviceID: open}))
	if len(got) != 1 || got[0].DeviceID != free {
		t.Fatalf("paused: kept %+v, want only the device at no venue", got)
	}
}
