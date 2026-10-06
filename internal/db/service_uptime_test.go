package db

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUptimeDays(t *testing.T) {
	step := int64(uptimeStep / time.Second)
	// Now: 3 Oct 2026 12:00 UTC. Window 09:00–17:00 UTC, two days.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a, b := uuid.New(), uuid.New()
	created := map[uuid.UUID]time.Time{a: now.AddDate(0, 0, -30), b: now.AddDate(0, 0, -30)}
	up := map[uuid.UUID]map[int64]bool{a: {}, b: {}}
	// a is up for all of yesterday's hours; b for the first half only. Both are up today.
	yOpen := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC).Unix() / step
	for i := int64(0); i < 32; i++ {
		up[a][yOpen+i] = true
		if i < 16 {
			up[b][yOpen+i] = true
		}
	}
	tOpen := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC).Unix() / step
	for i := int64(0); i < 12; i++ {
		up[a][tOpen+i] = true
		up[b][tOpen+i] = true
	}
	days := uptimeDays(now, 2, "UTC", 9*60, 17*60, []uuid.UUID{a, b}, created, up, step)
	if len(days) != 2 {
		t.Fatalf("days = %d", len(days))
	}
	if y := days[0]; y.Expected != 64 || y.Up != 48 || y.DownDevice != 1 || y.Pct() != 75 {
		t.Errorf("yesterday = %+v (%.1f%%)", y, y.Pct())
	}
	// Today counts only the 12 whole steps since 09:00.
	if d := days[1]; d.Expected != 24 || d.Up != 24 || d.Pct() != 100 {
		t.Errorf("today = %+v", d)
	}
	// A window past midnight (18:00–02:00) is counted on the day it opened.
	late := uptimeDays(now, 1, "UTC", 18*60, 2*60, []uuid.UUID{a}, created, up, step)
	if late[0].Expected != 0 || late[0].Pct() != -1 {
		t.Errorf("not open yet today = %+v", late[0])
	}
}
