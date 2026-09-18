package dashboard

import (
	"testing"
	"time"

	"mdm/internal/db"
)

// buildSeries makes a newest-first series from (count, spacing) segments, oldest first.
func buildSeries(start time.Time, segs [][2]int) []db.Checkin {
	var asc []db.Checkin
	t := start
	for _, sg := range segs {
		for i := 0; i < sg[0]; i++ {
			asc = append(asc, db.Checkin{CreatedAt: t})
			t = t.Add(time.Duration(sg[1]) * time.Second)
		}
	}
	out := make([]db.Checkin, len(asc))
	for i := range asc {
		out[i] = asc[len(asc)-1-i] // newest first, as the queries return
	}
	return out
}

// maxGap returns the largest spacing between consecutive kept points, in seconds.
func maxGap(cs []db.Checkin) float64 {
	worst := 0.0
	for i := 1; i < len(cs); i++ {
		d := cs[i-1].CreatedAt.Sub(cs[i].CreatedAt).Seconds()
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst = d
		}
	}
	return worst
}

// TestDownsampleDoesNotInventGaps is the bug this replaced. A device whose reporting
// rate swings wildly — a flapping charger stores a row every 2s while it flaps and one
// every 140s when it settles — was thinned by row index, which left the busy stretch
// alone and stretched the quiet stretch's real spacing into holes the chart drew as
// missing data. Real proportions from AT070AABU00527.
func TestDownsampleDoesNotInventGaps(t *testing.T) {
	series := buildSeries(time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC), [][2]int{
		{26, 140}, // quiet hour
		{1980, 2}, // flapping
		{1588, 2}, // still flapping
		{24, 140}, // quiet again
	})
	before := maxGap(series)
	got := downsampleCheckins(series, 600)
	after := maxGap(got)

	if len(got) > 640 {
		t.Errorf("kept %d points, want about 600", len(got))
	}

	// The invariant. Bucketing by time can enlarge a gap by at most one bucket width —
	// the point that would have filled it is never further away than that — so this is
	// the tightest bound that is actually true, rather than a round number that happens
	// to pass. Anything beyond it means the thinning is inventing gaps again.
	span := series[0].CreatedAt.Sub(series[len(series)-1].CreatedAt).Seconds()
	bucket := span / 600
	if after > before+bucket {
		t.Errorf("largest gap went %.0fs -> %.0fs, more than one %.0fs bucket: thinning invented a gap",
			before, after, bucket)
	}

	// And the behaviour this replaced must genuinely be gone. Row-stride thinning of
	// this series uses stride 7, which turns the quiet stretch's 140s spacing into
	// roughly 980s — the holes that showed up on the chart as missing data.
	if after > 300 {
		t.Errorf("largest kept gap %.0fs; the real spacing never exceeds %.0fs", after, before)
	}
}

// TestDownsampleKeepsShortSeriesWhole: nothing to thin, nothing changed.
func TestDownsampleKeepsShortSeriesWhole(t *testing.T) {
	series := buildSeries(time.Now().Add(-time.Hour), [][2]int{{50, 60}})
	if got := downsampleCheckins(series, 600); len(got) != 50 {
		t.Errorf("got %d points from a 50-point series, want 50", len(got))
	}
}

// TestDownsampleKeepsBothEnds: the window must not shrink, or the chart's x-range
// silently narrows every time it thins.
func TestDownsampleKeepsBothEnds(t *testing.T) {
	series := buildSeries(time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC), [][2]int{{5000, 2}})
	got := downsampleCheckins(series, 100)
	if !got[0].CreatedAt.Equal(series[0].CreatedAt) {
		t.Error("newest point was dropped")
	}
	if !got[len(got)-1].CreatedAt.Equal(series[len(series)-1].CreatedAt) {
		t.Error("oldest point was dropped")
	}
}

// TestDownsampleAllAtOneInstant: a degenerate series must not divide by zero.
func TestDownsampleAllAtOneInstant(t *testing.T) {
	now := time.Now()
	series := make([]db.Checkin, 1000)
	for i := range series {
		series[i] = db.Checkin{CreatedAt: now}
	}
	if got := downsampleCheckins(series, 100); len(got) > 101 {
		t.Errorf("got %d points, want about 100", len(got))
	}
}
