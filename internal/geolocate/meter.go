package geolocate

import (
	"sync"
	"time"
)

// Meter is a small in-memory usage counter for one Google API surface. It tracks
// lifetime totals (since server start) plus a rolling 24-hour, per-hour histogram
// of outbound requests for a sparkline. Safe for concurrent use.
//
// Counters are intentionally in-memory only: they reset on restart. That keeps the
// hot path (every check-in) free of DB writes; the settings panel labels the totals
// "since <server start>" so the reset is not surprising. If billing-grade history is
// needed later, snapshot these into a daily table from housekeeping.
type Meter struct {
	mu        sync.Mutex
	successes uint64 // requests that reached Google and returned a usable answer
	errors    uint64 // requests that failed (transport, non-200, or denied status)
	hits      uint64 // answered from the in-memory exact-scan cache — no request sent
	localHits uint64 // answered from the learned WiFi-AP index (DB) — no request sent
	cooldowns uint64 // skipped by the global cooldown — no request sent
	lastErr   string
	lastErrAt time.Time
	lastReqAt time.Time

	// ring[hourUnix % 24] holds the outbound request count (successes+errors) for
	// that hour. ringHour is the unix-hour the most recent bucket represents.
	ring     [24]uint64
	ringHour int64
}

func newMeter() *Meter { return &Meter{} }

// rollLocked advances the ring so the newest bucket is hour h, zeroing any hours
// skipped since the last update. Caller holds m.mu.
func (m *Meter) rollLocked(h int64) {
	if m.ringHour == 0 {
		m.ringHour = h
		return
	}
	if h-m.ringHour >= 24 {
		// Idle for a full day or more — the whole window is stale.
		for i := range m.ring {
			m.ring[i] = 0
		}
		m.ringHour = h
		return
	}
	for m.ringHour < h {
		m.ringHour++
		m.ring[m.ringHour%24] = 0
	}
}

func (m *Meter) markRequestLocked() {
	now := time.Now()
	m.rollLocked(now.Unix() / 3600)
	m.ring[(now.Unix()/3600)%24]++
	m.lastReqAt = now
}

// MarkSuccess records one successful outbound request.
func (m *Meter) MarkSuccess() {
	m.mu.Lock()
	m.successes++
	m.markRequestLocked()
	m.mu.Unlock()
}

// MarkError records one failed outbound request and its message.
func (m *Meter) MarkError(err error) {
	m.mu.Lock()
	m.errors++
	m.markRequestLocked()
	if err != nil {
		m.lastErr = err.Error()
		m.lastErrAt = time.Now()
	}
	m.mu.Unlock()
}

// MarkHit records an in-memory exact-scan cache hit (no request sent).
func (m *Meter) MarkHit() {
	m.mu.Lock()
	m.hits++
	m.mu.Unlock()
}

// MarkLocalHit records a lookup served from the learned WiFi-AP index (no request sent).
func (m *Meter) MarkLocalHit() {
	m.mu.Lock()
	m.localHits++
	m.mu.Unlock()
}

// MarkCooldown records a cooldown skip (no request sent).
func (m *Meter) MarkCooldown() {
	m.mu.Lock()
	m.cooldowns++
	m.mu.Unlock()
}

// MeterSnapshot is an immutable copy of a Meter for rendering.
type MeterSnapshot struct {
	Successes uint64    `json:"successes"`
	Errors    uint64    `json:"errors"`
	Requests  uint64    `json:"requests"` // successes + errors (billable outbound)
	Hits      uint64    `json:"hits"`
	LocalHits uint64    `json:"local_hits"`
	Cooldowns uint64    `json:"cooldowns"`
	Last24h   uint64    `json:"last_24h"`  // outbound requests in the rolling 24h window
	Hourly    []uint64  `json:"hourly"`    // 24 buckets, oldest→newest, ending this hour
	LastErr   string    `json:"last_err"`
	LastErrAt time.Time `json:"last_err_at"`
	LastReqAt time.Time `json:"last_req_at"`
}

// Snapshot returns a consistent copy of the meter's current state.
func (m *Meter) Snapshot() MeterSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := time.Now().Unix() / 3600
	m.rollLocked(h)

	hourly := make([]uint64, 24)
	var last24 uint64
	for i := 0; i < 24; i++ {
		hourUnix := h - int64(23-i)
		v := m.ring[((hourUnix%24)+24)%24]
		hourly[i] = v
		last24 += v
	}
	return MeterSnapshot{
		Successes: m.successes,
		Errors:    m.errors,
		Requests:  m.successes + m.errors,
		Hits:      m.hits,
		LocalHits: m.localHits,
		Cooldowns: m.cooldowns,
		Last24h:   last24,
		Hourly:    hourly,
		LastErr:   m.lastErr,
		LastErrAt: m.lastErrAt,
		LastReqAt: m.lastReqAt,
	}
}
