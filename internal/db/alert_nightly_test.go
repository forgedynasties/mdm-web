package db

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNightlyPlanOncePerVenuePerNight(t *testing.T) {
	rule := uuid.New()
	venueA, venueB := uuid.New(), uuid.New()
	a1, a2, b1, bench := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// 23:30 → 06:00 in UTC, both venues; the bench unit has no restaurant.
	win := func(r *uuid.UUID) ServiceWindow {
		return ServiceWindow{RestaurantID: r, NightOpenMin: 1410, NightCloseMin: 360, TZ: "UTC"}
	}
	windows := map[uuid.UUID]ServiceWindow{a1: win(&venueA), a2: win(&venueA), b1: win(&venueB), bench: win(nil)}
	var tr nightlyTracker
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	offset := 2 * time.Hour

	// 00:30 is inside the night but before the 01:30 check: nothing due, all kept.
	pl := tr.plan(rule, offset, windows, at("2026-09-29 00:30"))
	if len(pl.due) != 0 || len(pl.keep) != 3 {
		t.Fatalf("00:30: due %v keep %d, want none due and 3 kept", pl.due, len(pl.keep))
	}
	// 01:30: both venues due, keyed to the night that began on the 28th.
	pl = tr.plan(rule, offset, windows, at("2026-09-29 01:30"))
	if pl.due[venueA] != "2026-09-28" || pl.due[venueB] != "2026-09-28" || len(pl.keep) != 0 {
		t.Fatalf("01:30: due %v keep %d", pl.due, len(pl.keep))
	}
	tr.mark(rule, pl.due)
	// Later the same night: not due again, devices kept.
	pl = tr.plan(rule, offset, windows, at("2026-09-29 04:00"))
	if len(pl.due) != 0 || len(pl.keep) != 3 {
		t.Fatalf("04:00: due %v keep %d, want checked once", pl.due, len(pl.keep))
	}
	// Daytime: outside the night, neither due nor kept (the caller clears).
	pl = tr.plan(rule, offset, windows, at("2026-09-29 12:00"))
	if len(pl.due) != 0 || len(pl.keep) != 0 {
		t.Fatalf("12:00: due %v keep %d", pl.due, len(pl.keep))
	}
	// The next night is a new check.
	pl = tr.plan(rule, offset, windows, at("2026-09-30 01:45"))
	if pl.due[venueA] != "2026-09-29" {
		t.Fatalf("next night: due %v", pl.due)
	}
}

// A night that does not wrap midnight, and a window longer than the night.
func TestNightlyPlanEdges(t *testing.T) {
	rule, venue, dev := uuid.New(), uuid.New(), uuid.New()
	windows := map[uuid.UUID]ServiceWindow{dev: {RestaurantID: &venue, NightOpenMin: 60, NightCloseMin: 360, TZ: "UTC"}}
	var tr nightlyTracker
	now := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	if pl := tr.plan(rule, 2*time.Hour, windows, now); pl.due[venue] != "2026-09-29" {
		t.Errorf("01:00–06:00 night at 03:00: due %v", pl.due)
	}
	if pl := tr.plan(rule, 6*time.Hour, windows, now); len(pl.due) != 0 {
		t.Errorf("a 6h window in a 5h night should never be due: %v", pl.due)
	}
}
