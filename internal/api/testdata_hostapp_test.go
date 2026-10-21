package api

import (
	"testing"
	"time"

	"mdm/internal/db"
)

func TestHostAppSpans(t *testing.T) {
	from := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	at := func(m int) time.Time { return from.Add(time.Duration(m) * time.Minute) }
	ev := func(m int, k, v string) db.ScreenStateEvent { return db.ScreenStateEvent{At: at(m), Key: k, To: v} }
	// In front with the screen on at the start; launcher at 30 min, back at 45; screen off
	// 120–150; offline from 200 min.
	spans, tot := hostAppSpans(map[string]string{"host_app_fg": "true", "screen_on": "true"},
		[]db.ScreenStateEvent{ev(30, "host_app_fg", "false"), ev(45, "host_app_fg", "true"),
			ev(120, "screen_on", "false"), ev(150, "screen_on", "true")},
		from, to, at(200))
	want := []struct {
		state, why string
		a, b       int
	}{
		{"on_screen", "app_in_foreground", 0, 30}, {"off_screen", "app_not_in_foreground", 30, 45},
		{"on_screen", "app_in_foreground", 45, 120}, {"off_screen", "screen_off", 120, 150},
		{"on_screen", "app_in_foreground", 150, 200}, {"unknown", "device_offline", 200, 240},
	}
	if len(spans) != len(want) {
		t.Fatalf("spans = %+v", spans)
	}
	for i, w := range want {
		s := spans[i]
		if s.State != w.state || s.Why != w.why || !s.From.Equal(at(w.a)) || !s.To.Equal(at(w.b)) {
			t.Errorf("span %d = %+v, want %+v", i, s, w)
		}
	}
	if tot.OnScreenS != 155*60 || tot.OffScreenS != 45*60 || tot.UnknownS != 40*60 || tot.OnScreenPct == nil || *tot.OnScreenPct != 77 {
		t.Errorf("totals = %+v", tot)
	}
	if n := lastSpanNow(spans); n.State != "unknown" || n.Why != "device_offline" {
		t.Errorf("now = %+v", n)
	}

	// No report kept until the first transition, online throughout.
	spans, tot = hostAppSpans(nil, []db.ScreenStateEvent{ev(60, "host_app_fg", "true")}, from, to, time.Time{})
	if len(spans) != 2 || spans[0].Why != "no_report" || spans[1].State != "on_screen" || *tot.OnScreenPct != 100 {
		t.Errorf("no-report start: %+v %+v", spans, tot)
	}
	// Nothing known at all.
	spans, tot = hostAppSpans(nil, nil, from, to, time.Time{})
	if len(spans) != 1 || spans[0].State != "unknown" || tot.OnScreenPct != nil {
		t.Errorf("empty: %+v %+v", spans, tot)
	}
}
