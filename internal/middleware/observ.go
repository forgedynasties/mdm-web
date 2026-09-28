package middleware

import (
	"bufio"
	"errors"
	"expvar"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"mdm/internal/metrics"
)

// Request/error counters surfaced at /debug/vars for aggregate rate & error visibility
// (the access log below deliberately does NOT log every request, to avoid a firehose).
var (
	metricRequests = expvar.NewInt("http_requests_total")
	metricErrors   = expvar.NewInt("http_errors_total")
)

// logRW wraps ResponseWriter to capture the status and byte count, while forwarding
// Hijack (needed by the WebSocket upgrade) and Flush (needed by SSE) so wrapping it
// doesn't break the streaming endpoints.
type logRW struct {
	http.ResponseWriter
	r      *http.Request
	start  time.Time
	status int
	bytes  int
	wrote  bool
	stream bool // the response is an SSE stream (decided at the first write)
}

// first runs once, when the headers go out. That is the only point at which an SSE
// response can be told apart from an ordinary one — by the Content-Type its handler
// set — and the stream is counted open from then, with the wait so far as its TTFB.
func (w *logRW) first(code int) {
	w.status, w.wrote = code, true
	if code < 300 && strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.stream = true
		metrics.Default.StreamOpen(w.r.Method, w.r.URL.Path, time.Since(w.start))
	}
}

func (w *logRW) WriteHeader(code int) {
	if !w.wrote {
		w.first(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logRW) Write(b []byte) (int, error) {
	if !w.wrote {
		w.first(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *logRW) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *logRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("response writer does not support hijacking")
}

// AccessLog counts every request (http_requests_total / http_errors_total at
// /debug/vars) and logs only the INTERESTING ones — 4xx/5xx errors and slow non-
// streaming requests. Successful device check-ins (200, fast) are NOT logged, so this
// doesn't recreate the per-checkin firehose; a successful WebSocket (101) or SSE
// stream isn't logged either (a long-lived stream never trips the bounded slow check).
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Count the request's database work: the pool's query tracer adds every
		// query run under this context (see db.queryTracer).
		ctx, dbu := metrics.WithDBUsage(r.Context())
		r = r.WithContext(ctx)
		lw := &logRW{ResponseWriter: w, r: r, start: start, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		dur := time.Since(start)

		metricRequests.Add(1)
		if lw.status >= 500 {
			metricErrors.Add(1)
		}
		// Feed the in-process collector behind the Server page. This middleware already
		// wraps every route, so instrumenting here means no handler has to remember to
		// do it — and a route added later is measured the day it is added. Long-lived
		// connections stay out of the latency table: their "duration" is how long a
		// tab stayed open, and one 77-minute SSE stream swamps every percentile on the
		// page. A hijacked connection (WS upgrade, wrote==false) is skipped; an SSE
		// stream is closed out on its own stats.
		switch {
		case lw.stream:
			metrics.Default.StreamClose(r.Method, r.URL.Path, dur)
		case lw.wrote:
			metrics.Default.Observe(r.Method, r.URL.Path, lw.status, dur, dbu)
		}
		// Slow means a slow ordinary request; the 60s cap keeps large downloads out.
		if lw.status >= 400 || (lw.wrote && !lw.stream && dur > 2*time.Second && dur < 60*time.Second) {
			log.Printf("[http] %s %s %d %dms %dB %s", r.Method, r.URL.Path, lw.status, dur.Milliseconds(), lw.bytes, clientIP(r))
		}
	})
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
