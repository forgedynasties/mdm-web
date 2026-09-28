package dashboard

import (
	"testing"
	"time"
)

// TestLastFullWeek pins the window a weekly report covers. The old rolling seven days
// meant the same link showed different numbers depending on when it was opened, and a
// report opened midweek described half of this week and half of last.
func TestLastFullWeek(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	cases := []struct {
		name             string
		now              time.Time
		wantFrom, wantTo time.Time
	}{
		// 13 Sep 2026 is a Sunday; 7 Sep is the Monday before it.
		{"midweek Friday", day(2026, 9, 18).Add(11 * time.Hour), day(2026, 9, 7), day(2026, 9, 13)},
		{"Monday morning", day(2026, 9, 14).Add(9 * time.Hour), day(2026, 9, 7), day(2026, 9, 13)},
		{"Saturday", day(2026, 9, 19), day(2026, 9, 7), day(2026, 9, 13)},
		// On a Sunday the week ending tonight is not claimed as finished yet.
		{"Sunday", day(2026, 9, 20).Add(20 * time.Hour), day(2026, 9, 7), day(2026, 9, 13)},
		// The following Monday rolls forward by exactly one week.
		{"next Monday", day(2026, 9, 21), day(2026, 9, 14), day(2026, 9, 20)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from, to := lastFullWeek(c.now)
			if !from.Equal(c.wantFrom) || !to.Equal(c.wantTo) {
				t.Errorf("got %s..%s, want %s..%s",
					from.Format("Mon 2 Jan"), to.Format("Mon 2 Jan"),
					c.wantFrom.Format("Mon 2 Jan"), c.wantTo.Format("Mon 2 Jan"))
			}
			if from.Weekday() != time.Monday {
				t.Errorf("window starts on %s, want Monday", from.Weekday())
			}
			if to.Weekday() != time.Sunday {
				t.Errorf("window ends on %s, want Sunday", to.Weekday())
			}
			if d := to.Sub(from).Hours() / 24; d != 6 {
				t.Errorf("window spans %.0f days between endpoints, want 6 (7 inclusive)", d)
			}
			// It must always be finished: the end is strictly before today.
			if !to.Before(c.now.UTC().Truncate(24 * time.Hour)) {
				t.Error("the window ends today or later — that week has not finished")
			}
		})
	}
}

// TestReportWeeks pins the picker: this week so far, then the two finished weeks
// before it, and last week chosen when nothing (or something unoffered) is asked for.
func TestReportWeeks(t *testing.T) {
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC) // a Wednesday
	weeks := reportWeeks(now)
	if len(weeks) != 3 {
		t.Fatalf("got %d weeks, want 3", len(weeks))
	}
	want := []struct {
		from, to string
		days     int
		current  bool
	}{
		{"2026-09-28", "2026-09-30", 3, true},
		{"2026-09-21", "2026-09-27", 7, false},
		{"2026-09-14", "2026-09-20", 7, false},
	}
	for i, w := range want {
		got := weeks[i]
		if got.From.Format("2006-01-02") != w.from || got.To.Format("2006-01-02") != w.to ||
			got.Days != w.days || got.Current != w.current {
			t.Errorf("week %d = %s..%s days=%d current=%v, want %s..%s days=%d current=%v", i,
				got.From.Format("2006-01-02"), got.To.Format("2006-01-02"), got.Days, got.Current,
				w.from, w.to, w.days, w.current)
		}
	}
	for q, wantFrom := range map[string]string{
		"":           "2026-09-21",
		"2026-09-28": "2026-09-28",
		"2026-09-14": "2026-09-14",
		"2026-09-07": "2026-09-21", // older than the picker reaches
		"junk":       "2026-09-21",
	} {
		if w, _ := pickReportWeek(q, now); w.Value() != wantFrom {
			t.Errorf("week=%q picked %s, want %s", q, w.Value(), wantFrom)
		}
	}
	// On a Monday, this week is one day long.
	if w := reportWeeks(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))[0]; w.Days != 1 || !w.Current {
		t.Errorf("Monday: this week = %d days current=%v, want 1 day current", w.Days, w.Current)
	}
	// A link to a finished week stays valid after it leaves the picker.
	if w, ok := reportWindowFor("2026-08-31", now); !ok || w.Days != 7 || w.Current {
		t.Errorf("old week: %+v ok=%v", w, ok)
	}
}
