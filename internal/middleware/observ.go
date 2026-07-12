package middleware

import (
	"bufio"
	"errors"
	"expvar"
	"log"
	"net"
	"net/http"
	"time"
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
	status int
	bytes  int
	wrote  bool
}

func (w *logRW) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logRW) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
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
		lw := &logRW{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		dur := time.Since(start)

		metricRequests.Add(1)
		if lw.status >= 500 {
			metricErrors.Add(1)
		}
		// wrote==false means the handler hijacked the connection (a WS upgrade), so
		// skip it; bound the slow window below 60s so a live SSE stream isn't logged.
		if lw.status >= 400 || (lw.wrote && dur > 2*time.Second && dur < 60*time.Second) {
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
