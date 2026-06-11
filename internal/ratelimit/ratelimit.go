// Package ratelimit provides a tiny in-process fixed-window counter used to
// throttle authentication attempts. It is intentionally dependency-free (no
// Redis): the dashboard login and API-key surfaces are low-volume, and a single
// instance behind the load balancer is the common deployment. A distributed
// attacker spread across many source IPs is only partially mitigated by this; it
// is defense against single-source brute force and credential spray.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Counter tracks how many times each key has been seen within a rolling fixed
// window. It is safe for concurrent use.
type Counter struct {
	mu        sync.Mutex
	window    time.Duration
	hits      map[string]*entry
	lastSweep time.Time
}

type entry struct {
	n     int
	reset time.Time
}

// New returns a Counter whose per-key window has the given duration.
func New(window time.Duration) *Counter {
	return &Counter{window: window, hits: make(map[string]*entry)}
}

// Hit records one occurrence for key and returns the new count within the
// current window plus the time remaining until that window resets.
func (c *Counter) Hit(key string) (int, time.Duration) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(now)
	e := c.hits[key]
	if e == nil || now.After(e.reset) {
		e = &entry{reset: now.Add(c.window)}
		c.hits[key] = e
	}
	e.n++
	return e.n, time.Until(e.reset)
}

// Count returns the current count for key within its window without recording a
// new occurrence, plus the time until the window resets.
func (c *Counter) Count(key string) (int, time.Duration) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.hits[key]
	if e == nil || now.After(e.reset) {
		return 0, 0
	}
	return e.n, time.Until(e.reset)
}

// Reset clears the counter for key (e.g. after a successful login).
func (c *Counter) Reset(key string) {
	c.mu.Lock()
	delete(c.hits, key)
	c.mu.Unlock()
}

// sweep drops expired entries at most once per window to bound memory. Callers
// must hold the mutex.
func (c *Counter) sweep(now time.Time) {
	if now.Sub(c.lastSweep) < c.window {
		return
	}
	c.lastSweep = now
	for k, e := range c.hits {
		if now.After(e.reset) {
			delete(c.hits, k)
		}
	}
}

// ClientIP extracts the best-effort client address, honouring the first hop of
// X-Forwarded-For (set by the AWS load balancer) and falling back to the socket
// peer.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
