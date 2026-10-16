package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestQABatteryWindow(t *testing.T) {
	for _, c := range []struct {
		q    string
		span time.Duration
		bad  bool
	}{
		{"", 25 * time.Hour, false},
		{"range=7d", 7*24*time.Hour + time.Hour, false},
		{"range=1y", 0, true},
		{"start=2026-10-01T00:00:00Z&end=2026-10-02T00:00:00Z", 24 * time.Hour, false},
		{"start=2026-10-02T00:00:00Z&end=2026-10-01T00:00:00Z", 0, true},
		{"start=2026-08-01T00:00:00Z&end=2026-10-01T00:00:00Z", 0, true},
		{"start=yesterday", 0, true},
	} {
		from, to, err := qaBatteryWindow(httptest.NewRequest("GET", "/x?"+c.q, nil))
		if (err != nil) != c.bad {
			t.Errorf("%q: err=%v, want bad=%v", c.q, err, c.bad)
			continue
		}
		if !c.bad && to.Sub(from).Round(time.Minute) != c.span {
			t.Errorf("%q: span %v, want %v", c.q, to.Sub(from), c.span)
		}
	}
}
