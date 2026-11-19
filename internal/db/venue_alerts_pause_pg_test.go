package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVenueAlertsPauseAgainstPostgres: a venue with its alerts paused raises nothing for its
// devices, its open alerts leave every list and count (and stay in the table), the dispatch
// check names its devices, and a resume (or an ended pause) brings them back. A device at
// another venue or at none is not touched.
func TestVenueAlertsPauseAgainstPostgres(t *testing.T) {
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
	var venue, other uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('VAP Lab') RETURNING id`).Scan(&venue); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('VAP Other') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	var in, out, free uuid.UUID
	for _, x := range []struct {
		p *uuid.UUID
		s string
		r any
	}{{&in, "VAP-IN", venue}, {&out, "VAP-OUT", other}, {&free, "VAP-FREE", nil}} {
		if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, restaurant_id) VALUES ($1, $2) RETURNING id`, x.s, x.r).Scan(x.p); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		d.pool.Exec(ctx, `DELETE FROM alerts WHERE device_id IN (SELECT id FROM devices WHERE serial_number LIKE 'VAP-%')`)
		d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'VAP-%'`)
		d.pool.Exec(ctx, `DELETE FROM restaurants WHERE name LIKE 'VAP %'`)
	}()
	for _, id := range []uuid.UUID{in, out, free} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO alerts (type, device_id, severity, summary) VALUES ('vap_x', $1, 'critical', 's')`, id); err != nil {
			t.Fatal(err)
		}
	}
	mine := func() (listed, stored, counted int) {
		act, err := d.ListActiveAlerts(ctx, 5000)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range act {
			if a.Type == "vap_x" {
				listed++
			}
		}
		if err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts WHERE type = 'vap_x'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		rows, err := d.AlertsByRestaurant(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.RestaurantID == venue {
				counted += r.Total
			}
		}
		return
	}
	if l, h, c := mine(); l != 3 || h != 3 || c != 1 {
		t.Fatalf("before the pause: listed %d, stored %d, venue count %d; want 3, 3, 1", l, h, c)
	}
	openBefore, _ := d.CountOpenAlerts(ctx, false)

	until := time.Now().Add(24 * time.Hour)
	if err := d.SetVenueAlertsPause(ctx, venue, VenueAlertsPause{Paused: true, PausedUntil: &until, By: "t"}); err != nil {
		t.Fatal(err)
	}
	if l, h, c := mine(); l != 2 || h != 3 || c != 0 {
		t.Errorf("paused: listed %d, stored %d, venue count %d; want 2, 3, 0", l, h, c)
	}
	if n, _ := d.CountOpenAlerts(ctx, false); n != openBefore-1 {
		t.Errorf("paused: open count %d, want %d", n, openBefore-1)
	}
	if n, _ := d.VenueHeldAlertCount(ctx, venue); n != 1 {
		t.Errorf("held back = %d, want 1", n)
	}
	if per, _ := d.ListDeviceActiveAlerts(ctx, in, 10); len(per) != 0 {
		t.Errorf("device page still lists %d alerts", len(per))
	}
	held, err := d.DevicesAlertsHeld(ctx, []uuid.UUID{in, out, free})
	if err != nil || !held[in] || held[out] || held[free] {
		t.Errorf("DevicesAlertsHeld = %v, %v", held, err)
	}
	// Raising: nothing for the paused venue, the others as usual.
	u, err := d.upsertAlert(ctx, nil, "vap_new", in, "warning", "s", nil)
	if err != nil || u.Inserted {
		t.Errorf("paused venue raised an alert: %+v %v", u, err)
	}
	u, err = d.upsertAlert(ctx, nil, "vap_new", out, "warning", "s", nil)
	if err != nil || !u.Inserted {
		t.Errorf("other venue raised nothing: %+v %v", u, err)
	}
	var n int
	d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts WHERE type = 'vap_new' AND device_id = $1`, in).Scan(&n)
	if n != 0 {
		t.Errorf("%d alerts stored for the paused venue's device", n)
	}

	// Resume: everything is back, nothing was resolved or deleted.
	if err := d.SetVenueAlertsPause(ctx, venue, VenueAlertsPause{By: "t"}); err != nil {
		t.Fatal(err)
	}
	if l, h, c := mine(); l != 3 || h != 3 || c != 1 {
		t.Errorf("resumed: listed %d, stored %d, venue count %d; want 3, 3, 1", l, h, c)
	}
	// A pause that ended by itself holds nothing either.
	past := time.Now().Add(-time.Minute)
	if err := d.SetVenueAlertsPause(ctx, venue, VenueAlertsPause{Paused: true, PausedUntil: &past, By: "t"}); err != nil {
		t.Fatal(err)
	}
	if l, _, _ := mine(); l != 3 {
		t.Errorf("ended pause still hides alerts: listed %d", l)
	}
	if held, _ := d.DevicesAlertsHeld(ctx, []uuid.UUID{in}); held[in] {
		t.Error("ended pause still holds the device")
	}
}
