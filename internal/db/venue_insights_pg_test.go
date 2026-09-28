package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVenueInsightsAgainstPostgres runs the venue report's queries on a throwaway,
// migrated database (see TestShapedHistoryAgainstPostgres for how to start one).
func TestVenueInsightsAgainstPostgres(t *testing.T) {
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
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	b := 50
	_, _, _, _, sh, err := d.UpsertCheckin(ctx, "VENUE"+time.Now().Format("150405"), "v2.1.099", &b, json.RawMessage(`{}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	id := sh.DeviceID
	from := time.Date(2026, 9, 21, 0, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 7)
	at := func(day, h, m, s int) time.Time {
		return from.AddDate(0, 0, day).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}

	// Samples: Tuesday 7pm at 80%, then 7:40pm at 4%.
	for _, s := range []struct {
		t   time.Time
		bat int
	}{{at(1, 19, 0, 0), 80}, {at(1, 19, 40, 0), 4}} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, $2, $3)`, id, s.t, s.bat); err != nil {
			t.Fatal(err)
		}
	}
	// Pad: a phone on at 8:00pm that flickers off for 30s twice and comes off at 8:25pm
	// (one session of ~25 min), then a phone put down for 40s at 9pm (not a charge).
	ev := []struct {
		t   time.Time
		val string
	}{
		{at(1, 20, 0, 0), "1"}, {at(1, 20, 10, 0), "0"}, {at(1, 20, 10, 30), "1"},
		{at(1, 20, 18, 0), "2"}, {at(1, 20, 18, 30), "1"}, {at(1, 20, 25, 0), "0"},
		{at(1, 21, 0, 0), "1"}, {at(1, 21, 0, 40), "0"},
	}
	for _, e := range ev {
		if _, err := d.pool.Exec(ctx, `INSERT INTO device_state_events (device_id, at, key, from_val, to_val) VALUES ($1, $2, 'wlc_status', '', $3)`, id, e.t, e.val); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_events (device_id, kind, summary, occurred_at) VALUES ($1, 'reboot', '', $2)`, id, at(2, 3, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		typ string
		t   time.Time
	}{{"offline_peak", at(4, 18, 20, 0)}, {"battery_low", at(4, 19, 0, 0)}, {"overheating", to.Add(time.Hour)}} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO alerts (type, device_id, fired_at, status) VALUES ($1, $2, $3, 'resolved')`, a.typ, id, a.t); err != nil {
			t.Fatal(err)
		}
	}

	ins, err := d.VenueInsightsFor(ctx, []uuid.UUID{id}, from, to, to.Add(12*time.Hour), loc)
	if err != nil {
		t.Fatal(err)
	}

	// Hours come back as local wall clock: Tuesday 7pm, lowest battery 4.
	if len(ins.Hours) != 1 || ins.Hours[0].Hour.Hour() != 19 || ins.Hours[0].Hour.Weekday() != time.Tuesday || ins.Hours[0].MinBattery != 4 {
		t.Errorf("hours = %+v, want one Tuesday 7pm hour at 4%%", ins.Hours)
	}
	// The flickering phone is one session of about 24.5 minutes (the 30 s gaps and the
	// status-2 blip excluded); the 40 s put-down is not a charge.
	if len(ins.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one", ins.Sessions)
	}
	if s := ins.Sessions[0]; s.Start.Hour() != 20 || s.Start.Minute() != 0 || s.Minutes < 23.5 || s.Minutes > 24.5 {
		t.Errorf("session = %+v, want 8pm, ~24 min", s)
	}
	if ins.Restarts[id] != 1 {
		t.Errorf("restarts = %v, want 1", ins.Restarts)
	}
	// Only the report's alert types, and only inside the week.
	if len(ins.Alerts) != 1 || ins.Alerts[0].Type != "offline_peak" || ins.Alerts[0].At.Hour() != 18 || ins.Alerts[0].At.Minute() != 20 {
		t.Errorf("alerts = %+v, want the Friday 6:20pm offline_peak alone", ins.Alerts)
	}
}
