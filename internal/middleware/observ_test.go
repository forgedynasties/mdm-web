package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mdm/internal/metrics"
)

// An SSE handler is recognised by its Content-Type and measured as a stream, not as
// a request whose latency is the life of the tab.
func TestAccessLogSeparatesStreams(t *testing.T) {
	old := metrics.Default
	metrics.Default = metrics.New()
	defer func() { metrics.Default = old }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": connected\n\n")
		w.(http.Flusher).Flush()
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	// A stream route that refuses (401) before streaming is an ordinary error.
	mux.HandleFunc("GET /events/denied", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	h := AccessLog(mux)
	for _, p := range []string{"/events/devices", "/devices", "/events/denied"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}

	s := metrics.Default.Snapshot(0, 0)
	routes := map[string]metrics.RouteStat{}
	for _, r := range s.Routes {
		routes[r.Route] = r
	}
	if _, ok := routes["GET /events/devices"]; ok {
		t.Error("SSE stream landed in the latency table")
	}
	if routes["GET /devices"].Calls != 1 {
		t.Error("ordinary request not measured")
	}
	if routes["GET /events/denied"].Errors != 1 {
		t.Error("refused stream should count as an ordinary error")
	}
	if len(s.Streams) != 1 || s.Streams[0].Opened != 1 || s.Streams[0].Open != 0 {
		t.Errorf("streams = %+v, want one opened and closed", s.Streams)
	}
}
