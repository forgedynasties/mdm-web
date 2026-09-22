package db

import "testing"

// The score exists to tell an operator whether a venue needs attention today. Before
// this was tuned, flat per-alert penalties (-15 a critical, -4 a warning) meant a
// handful of open alerts zeroed any venue regardless of its actual state — and a score
// that reads 0 for a fleet that is largely fine is worse than no score at all.
func TestHealthScoreIsProportionate(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name        string
		g           GroupHealth
		wantAtLeast int
		wantAtMost  int
		wantClass   string
	}{
		{
			name:        "healthy venue scores full marks",
			g:           GroupHealth{DeviceCount: 9},
			wantAtLeast: 100, wantAtMost: 100, wantClass: "ok",
		},
		{
			name: "an ordinary Tuesday — one device down, a couple of alerts, mid-rollout",
			g: GroupHealth{DeviceCount: 9, OfflineCount: 1, OpenCritical: 1, OpenWarning: 3,
				DistinctBuilds: 2},
			wantAtLeast: 78, wantAtMost: 95, wantClass: "ok",
		},
		{
			name: "alert pile-up alone cannot zero a venue",
			g:    GroupHealth{DeviceCount: 10, OpenWarning: 40, OpenCritical: 6},
			// Capped at -18 critical and -6 warning: bad, not annihilated.
			wantAtLeast: 70, wantAtMost: 80, wantClass: "ok",
		},
		{
			name:        "build spread is noticed, never fatal",
			g:           GroupHealth{DeviceCount: 8, DistinctBuilds: 9},
			wantAtLeast: 92, wantAtMost: 96, wantClass: "ok",
		},
		{
			name: "a venue that really is in trouble still fails",
			g: GroupHealth{DeviceCount: 6, OfflineCount: 6, OpenCritical: 4, OpenWarning: 12,
				ChargingAvg: f(0.1), BatteryDelta: f(-25), TempMax: f(51), DistinctBuilds: 4},
			wantAtLeast: 0, wantAtMost: 39, wantClass: "danger",
		},
		{
			name:        "an empty venue is not a sick one",
			g:           GroupHealth{DeviceCount: 0, OpenWarning: 3},
			wantAtLeast: 100, wantAtMost: 100, wantClass: "ok",
		},
	}
	for _, c := range cases {
		g := c.g
		g.computeScore()
		if g.Score < c.wantAtLeast || g.Score > c.wantAtMost {
			t.Errorf("%s: score %d, want %d..%d", c.name, g.Score, c.wantAtLeast, c.wantAtMost)
		}
		if g.ScoreClass != c.wantClass {
			t.Errorf("%s: class %q, want %q (score %d)", c.name, g.ScoreClass, c.wantClass, g.Score)
		}
	}
}

// Hardware that has been dark for months should not re-fail a venue every day. It is
// still counted — just as a nudge, not as today's outage.
func TestHealthScoreSeparatesDormantFromOffline(t *testing.T) {
	fresh := GroupHealth{DeviceCount: 10, OfflineCount: 6}
	fresh.computeScore()

	dormant := GroupHealth{DeviceCount: 10, OfflineCount: 6, DormantCount: 6}
	dormant.computeScore()

	if dormant.Score <= fresh.Score {
		t.Errorf("six devices dark for a month (%d) should score better than six that dropped today (%d)",
			dormant.Score, fresh.Score)
	}
	if dormant.Score < 94 {
		t.Errorf("dormant-only venue scored %d; the nudge is capped at -4", dormant.Score)
	}
	// And the distinction must not become a loophole: a venue losing devices right now
	// still takes the full penalty.
	if fresh.Score > 88 {
		t.Errorf("six of ten offline today scored %d — too generous", fresh.Score)
	}
}
