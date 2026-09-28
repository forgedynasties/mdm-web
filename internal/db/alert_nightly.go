package db

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Once-a-night rules. slow_charge_night asks one question per venue per night — did
// this device charge in the first window_hours of the night? — and re-asking it every
// minute until morning bought nothing but load: it was the heaviest query on live.
// So it is checked once per restaurant per night, at the first tick after window_hours
// of that restaurant's overnight window have passed (23:30 + 2h = 01:30 by default).
// That is also the earliest minute the rule could fire before, so a stalled charger
// is reported exactly as early as it was; a device plugged in later is not re-checked.
//
// What has run is kept in memory. A restart re-runs tonight's check once, which is
// harmless: CreateAlertIfAbsent will not open a second alert for the same device.
var onceNightlyRules = map[string]bool{
	"slow_charge_night": true,
}

type nightlyTracker struct {
	mu  sync.Mutex
	ran map[string]string // rule/restaurant → night key of the last check
}

// nightlyPlan is one tick's view of a once-a-night rule.
type nightlyPlan struct {
	due  map[uuid.UUID]string // restaurants to check now → their night key
	keep []uuid.UUID          // devices mid-night whose venue is not due: leave their alerts be
}

// plan works out which restaurants are due this tick. offset is how far into the
// night the check happens. Devices without a restaurant are never due: the rule is
// overnight-windowed and so deployed-only.
func (t *nightlyTracker) plan(ruleID uuid.UUID, offset time.Duration, windows map[uuid.UUID]ServiceWindow, now time.Time) nightlyPlan {
	pl := nightlyPlan{due: map[uuid.UUID]string{}}
	t.mu.Lock()
	defer t.mu.Unlock()
	for dev, w := range windows {
		if w.RestaurantID == nil {
			continue
		}
		lt := localTime(now, w.TZ)
		m := lt.Hour()*60 + lt.Minute()
		if !inMinWindow(m, w.NightOpenMin, w.NightCloseMin) {
			continue // outside the night: the caller clears as usual
		}
		// A night that wraps midnight belongs to the date it started on, so the check
		// at 22:00 and the minutes after midnight are the same night.
		night := lt
		if m < w.NightOpenMin {
			night = lt.AddDate(0, 0, -1)
		}
		key := night.Format("2006-01-02")
		length := (w.NightCloseMin - w.NightOpenMin + 1440) % 1440
		elapsed := (m - w.NightOpenMin + 1440) % 1440
		rid := *w.RestaurantID
		if elapsed >= int(offset.Minutes()) && int(offset.Minutes()) < length &&
			t.ran[ruleID.String()+"/"+rid.String()] != key {
			pl.due[rid] = key
			continue
		}
		pl.keep = append(pl.keep, dev)
	}
	// A venue's devices all share its window, but one seen after the venue was marked
	// due still landed in keep above; a due venue's devices are the check's to clear.
	kept := pl.keep[:0]
	for _, dev := range pl.keep {
		if _, ok := pl.due[*windows[dev].RestaurantID]; !ok {
			kept = append(kept, dev)
		}
	}
	pl.keep = kept
	return pl
}

// mark records the restaurants just checked, once their alerts are written.
func (t *nightlyTracker) mark(ruleID uuid.UUID, due map[uuid.UUID]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ran == nil {
		t.ran = map[string]string{}
	}
	for rid, key := range due {
		t.ran[ruleID.String()+"/"+rid.String()] = key
	}
}
