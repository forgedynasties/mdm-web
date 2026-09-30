package api

import (
	"testing"
	"time"
)

func TestPlaceReading(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	devNow := now.Add(-3 * time.Hour).UnixMilli() // the device's clock is 3 h slow
	// Same boot: time since boot wins, whatever the clock says.
	at, ok := placeReading(backfillReading{AtMs: 1, ElapsedMs: 1_000_000, BootID: "b2"}, now, devNow, 1_000_000+600_000, "b2")
	if !ok || !at.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("same boot: %v %v", at, ok)
	}
	// Earlier boot: the wall clock, corrected by today's skew.
	read := now.Add(-2 * time.Hour)
	at, ok = placeReading(backfillReading{AtMs: read.Add(-3 * time.Hour).UnixMilli(), ElapsedMs: 5, BootID: "b1"}, now, devNow, 999, "b2")
	if !ok || !at.Equal(read) {
		t.Errorf("earlier boot: %v %v", at, ok)
	}
	// A reset clock on an earlier boot can't be placed.
	if _, ok := placeReading(backfillReading{AtMs: 86_400_000, BootID: "b1"}, now, 86_400_000*2, 999, "b2"); ok {
		t.Error("a 1970 clock should be dropped")
	}
	// Nothing in the future, nothing older than 8 days.
	if _, ok := placeReading(backfillReading{AtMs: now.Add(time.Hour).UnixMilli(), BootID: "b1"}, now, now.UnixMilli(), 999, "b2"); ok {
		t.Error("future reading accepted")
	}
	if _, ok := placeReading(backfillReading{AtMs: now.Add(-9 * 24 * time.Hour).UnixMilli(), BootID: "b1"}, now, now.UnixMilli(), 999, "b2"); ok {
		t.Error("9-day-old reading accepted")
	}
}
