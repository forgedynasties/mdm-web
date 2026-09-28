package metrics

import (
	"testing"
	"time"
)

// Streams must never reach the latency table: one open dashboard tab is an hour-long
// "request" and would top every percentile on the page.
func TestStreamsKeptOutOfRoutes(t *testing.T) {
	c := New()
	c.Observe("GET", "/devices", 200, 150*time.Millisecond)
	c.StreamOpen("GET", "/events/devices", 4*time.Millisecond)
	c.StreamOpen("GET", "/events/devices", 6*time.Millisecond)
	c.StreamClose("GET", "/events/devices", 77*time.Minute)

	s := c.Snapshot(0, 0)
	if len(s.Routes) != 1 || s.Routes[0].Route != "GET /devices" {
		t.Fatalf("routes = %+v, want only GET /devices", s.Routes)
	}
	if len(s.Streams) != 1 {
		t.Fatalf("streams = %+v, want one", s.Streams)
	}
	st := s.Streams[0]
	if st.Route != "GET /events/devices" || st.Open != 1 || st.Opened != 2 {
		t.Errorf("stream = %+v, want 1 open of 2 opened", st)
	}
	if st.LifeP50S != (77 * time.Minute).Seconds() {
		t.Errorf("life p50 = %v", st.LifeP50S)
	}
	if st.TTFBP95Ms < 4 || st.TTFBP95Ms > 6 {
		t.Errorf("ttfb p95 = %v, want 4..6", st.TTFBP95Ms)
	}
	if s.ReqTotal != 3 {
		t.Errorf("req total = %d, want 3 (streams still count as requests)", s.ReqTotal)
	}
}

// Routes are ranked by total time: a busy cheap route outranks one slow call.
func TestRoutesByTotalTime(t *testing.T) {
	c := New()
	c.Observe("GET", "/settings", 200, 2*time.Second)
	for i := 0; i < 40; i++ {
		c.Observe("GET", "/commands", 200, 200*time.Millisecond)
	}
	s := c.Snapshot(0, 0)
	if s.Routes[0].Route != "GET /commands" {
		t.Errorf("top route = %s, want GET /commands", s.Routes[0].Route)
	}
	if got := s.Routes[0].TotalMs; got < 7999 || got > 8001 {
		t.Errorf("total = %v ms, want 8000", got)
	}
}

// A close without a matching open (the map was full when it opened) must not panic
// or drive the gauge negative.
func TestStreamCloseUnknown(t *testing.T) {
	c := New()
	c.StreamClose("GET", "/events/x", time.Second)
	if len(c.Snapshot(0, 0).Streams) != 0 {
		t.Error("unknown close created a stream")
	}
}
