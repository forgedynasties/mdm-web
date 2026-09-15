package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"mdm/internal/db"
)

func TestChargeRuns(t *testing.T) {
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	ci := func(sec int, extra string) db.Checkin {
		return db.Checkin{CreatedAt: base.Add(time.Duration(sec) * time.Second), Extra: json.RawMessage(extra)}
	}
	gap := int64(15 * 60 * 1000)
	runs := chargeRuns([]db.Checkin{
		ci(0, `{"charging":true}`),
		ci(30, `{"charging":true}`),
		ci(60, `{"charging":false}`), // flip
		ci(90, `{"charging":false}`),
		ci(90+20*60, `{"charging":false}`), // same state after a 20 min silence: new span
		ci(90+20*60+30, `{}`),              // no key
	}, gap)
	type want struct {
		from, to int
		c        string
	}
	exp := []want{{0, 30, "true"}, {60, 90, "false"}, {1290, 1290, "false"}, {1320, 1320, "nil"}}
	if len(runs) != len(exp) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(exp), runs)
	}
	for i, r := range runs {
		c := "nil"
		if r.C != nil {
			c = map[bool]string{true: "true", false: "false"}[*r.C]
		}
		from := int((r.From - base.UnixMilli()) / 1000)
		to := int((r.To - base.UnixMilli()) / 1000)
		if from != exp[i].from || to != exp[i].to || c != exp[i].c {
			t.Errorf("run %d = {%d %d %s}, want %+v", i, from, to, c, exp[i])
		}
	}
	if g := chartGapMs(30000); g != 900000 {
		t.Errorf("chartGapMs(30000) = %d, want 900000", g)
	}
	if g := chartGapMs(120000); g != 1200000 {
		t.Errorf("chartGapMs(120000) = %d, want 1200000", g)
	}
}
