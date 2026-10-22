package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVenueLostPauseAgainstPostgres: a paused venue sends no self-lock and raises no
// left_venue alert; its self-lock time comes back when the pause ends.
func TestVenueLostPauseAgainstPostgres(t *testing.T) {
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
	r, err := d.CreateRestaurant(ctx, Restaurant{Name: "LP test venue"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, r.ID)
	var dev uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class, product, agent_kind, restaurant_id,
		last_seen_at, latest_extra) VALUES ('LP-T7', 't7', 't7', 'firmware', $1, NOW() - interval '10 minutes',
		'{"charging": false, "charger_type": "none"}') RETURNING id`, r.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'LP-T7'`)
	leftVenue := func() bool {
		hits, _, err := d.detectLeftVenue(ctx, map[string]float64{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.Serial == "LP-T7" {
				return true
			}
		}
		return false
	}

	if err := d.SetVenueLostWatch(ctx, r.ID, VenueLostWatch{SelfLockMinutes: 15, By: "ali"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.LostConfigFor(ctx, dev); c.SelfLockMinutes != 15 {
		t.Errorf("watching: self-lock = %d, want 15", c.SelfLockMinutes)
	}
	if !leftVenue() {
		t.Error("watching: no left_venue hit")
	}

	until := time.Now().Add(time.Hour)
	if err := d.SetVenueLostWatch(ctx, r.ID, VenueLostWatch{SelfLockMinutes: 15, Paused: true, PausedUntil: &until, By: "ali"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.LostConfigFor(ctx, dev); c.SelfLockMinutes != 0 {
		t.Errorf("paused: self-lock = %d, want 0", c.SelfLockMinutes)
	}
	if leftVenue() {
		t.Error("paused: left_venue still raised")
	}
	if w, _ := d.VenueLostWatch(ctx, r.ID); w.SelfLockMinutes != 15 || !w.PausedAt(time.Now()) || w.By != "ali" {
		t.Errorf("stored = %+v", w)
	}

	past := time.Now().Add(-time.Minute)
	if _, err := d.pool.Exec(ctx, `UPDATE restaurants SET lost_paused_until = $2 WHERE id = $1`, r.ID, past); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.LostConfigFor(ctx, dev); c.SelfLockMinutes != 15 {
		t.Errorf("pause ended: self-lock = %d, want 15", c.SelfLockMinutes)
	}
	if !leftVenue() {
		t.Error("pause ended: no left_venue hit")
	}
}
