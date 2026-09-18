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
