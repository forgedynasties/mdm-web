// Package metrics is the server's view of its own work: how much is arriving, how long
// it takes, what is queued, and what just happened.
//
// It is deliberately in-process and bounded. Everything lives in ring buffers sized in
// kilobytes, because this box is a t2.medium sharing 4GB with other stacks — the thing
// that measures the server must never be a reason the server struggles. Nothing is
// written to Postgres: the database is one of the things being measured, and metrics
// that add load to their own subject lie about it.
//
// History therefore dies with the process. That is the accepted trade: this answers
// "what is happening now", which is the question asked during an incident.
package metrics

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Series length: one hour at the 10-second sample interval.
const (
	SampleEvery = 10 * time.Second
	seriesLen   = 360
	eventsLen   = 200
	// Per-route latency sample. Enough for a stable p95 without keeping every
	// request: at 60 rps the busiest route still turns its sample over every 2s.
	routeSampleLen = 128
	// RouteWindow is what the routes table covers. Since-boot totals meant one bad
	// hour three days ago still sat on the page; a rolling window answers "now".
	RouteWindow = 15 * time.Minute
	windowMins  = int(RouteWindow / time.Minute)
)

// ProbesRoute is the one row health checks and static files fold into. Docker's
// healthcheck alone is a call every few seconds; as separate rows they crowded out
// the routes someone might actually need to look at.
const ProbesRoute = "health checks & static files"

func isProbe(path string) bool {
	return path == "/health" || path == "/healthz" || path == "/favicon.ico" || path == "/robots.txt" ||
		strings.HasPrefix(path, "/static/")
}

// Event is one thing the server did, for the live feed.
type Event struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`  // checkin | ws | command | ota | peer | alert | deploy
	Text  string    `json:"text"`  // already-rendered, short
	Class string    `json:"class"` // "" | ok | warn | bad
}

// RouteStat is one route's traffic over the last RouteWindow.
type RouteStat struct {
	Route  string  `json:"route"`
	Calls  int64   `json:"calls"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	Errors int64   `json:"errors"`
	// TotalMs is every call's latency added up: where the server's time actually
	// went. It is the sort key, so a busy 200ms route outranks one 2s call.
	TotalMs float64 `json:"total_ms"`
	// SharePct is TotalMs as a share of all request time in the window.
	SharePct float64 `json:"share_pct"`
	// DBMsPerCall and QueriesPerCall say where a slow route's time goes: one slow
	// query, dozens of fast ones (an N+1), or none at all (rendering). Queries run in
	// parallel inside one request add up, so DB time can exceed the latency.
	DBMsPerCall    float64 `json:"db_ms_per_call"`
	QueriesPerCall float64 `json:"queries_per_call"`
	Probe          bool    `json:"probe,omitempty"`
}

// DBUsage is what one request spent in Postgres. The access-log middleware puts a
// counter in the request context (WithDBUsage) and the pool's query tracer adds to it.
type DBUsage struct {
	queries atomic.Int64
	nanos   atomic.Int64
}

func (u *DBUsage) Add(d time.Duration) {
	u.queries.Add(1)
	u.nanos.Add(int64(d))
}

func (u *DBUsage) Queries() int64          { return u.queries.Load() }
func (u *DBUsage) Duration() time.Duration { return time.Duration(u.nanos.Load()) }

type dbUsageKey struct{}

// WithDBUsage returns a context that counts the database work done under it.
func WithDBUsage(ctx context.Context) (context.Context, *DBUsage) {
	u := &DBUsage{}
	return context.WithValue(ctx, dbUsageKey{}, u), u
}

// DBUsageFrom is the counter for ctx's request, or nil outside one.
func DBUsageFrom(ctx context.Context) *DBUsage {
	u, _ := ctx.Value(dbUsageKey{}).(*DBUsage)
	return u
}

// MinCallsForP95 is how many calls a route needs before its p95 means anything. Below
// it, "p95" of three samples is just the slowest one, and a single cold-cache hit
// would sit at the top of the table looking like a problem.
const MinCallsForP95 = 20

// StreamStat is one SSE route. A stream's duration is how long a tab stayed open, not
// how slow the server was, so streams are kept out of RouteStat entirely and measured
// by what does mean something for them: how fast they start, how many are open, and
// how long they live (a stream that lives seconds is a client reconnecting in a loop).
type StreamStat struct {
	Route     string  `json:"route"`
	Open      int64   `json:"open"`   // right now
	Opened    int64   `json:"opened"` // since boot
	TTFBP50Ms float64 `json:"ttfb_p50_ms"`
	TTFBP95Ms float64 `json:"ttfb_p95_ms"`
	LifeP50S  float64 `json:"life_p50_sec"`
}

// Sample is one point in the sampled series.
type Sample struct {
	At         time.Time `json:"at"`
	Requests   float64   `json:"requests"` // per second since the last sample
	Checkins   float64   `json:"checkins"` // per minute
	WS         int       `json:"ws"`       // live device WebSockets
	Goroutines int       `json:"goroutines"`
	HeapMB     float64   `json:"heap_mb"`
	SysMB      float64   `json:"sys_mb"`
	DBUsed     int32     `json:"db_used"`
	DBTotal    int32     `json:"db_total"`
	DBWaiting  int64     `json:"db_waiting"`
}

// routeAgg keeps one bucket per minute of the window, so a total over the last 15
// minutes is a sum of at most 15 small structs, and a sample of recent latencies,
// each stamped so a quiet route's hour-old calls drop out of its percentiles.
type routeAgg struct {
	buckets [windowMins]bucket
	samples []stamped
	next    int
}

type bucket struct {
	minute  int64 // unix minute this bucket holds; stale when older than the window
	calls   int64
	errors  int64
	totalMs float64
	dbMs    float64
	queries int64
}

type stamped struct {
	at int64 // unix seconds
	ms float64
}

type streamAgg struct {
	open, opened int64
	ttfb, life   ring // ms, seconds
}

// ring is a bounded sample: the newest routeSampleLen values.
type ring struct {
	v    []float64
	next int
}

func (r *ring) add(x float64) {
	if len(r.v) < routeSampleLen {
		r.v = append(r.v, x)
		return
	}
	r.v[r.next] = x
	r.next = (r.next + 1) % routeSampleLen
}

// Collector holds it all. One per process; the default is fine.
type Collector struct {
	mu sync.Mutex

	started time.Time
	now     func() time.Time // time.Now; tests move it
	routes  map[string]*routeAgg
	streams map[string]*streamAgg

	reqTotal     int64 // lifetime
	reqAtSample  int64 // value at the previous sample, for the per-second rate
	checkinTotal int64
	chkAtSample  int64

	series []Sample
	events []Event
}

var Default = New()

func New() *Collector {
	return &Collector{started: time.Now(), now: time.Now, routes: map[string]*routeAgg{}, streams: map[string]*streamAgg{}}
}

// Observe records one finished request. Called from the access-log middleware, which
// already wraps every route, so nothing has to be instrumented twice. db is what the
// request spent in Postgres; nil when it was not counted.
func (c *Collector) Observe(method, path string, status int, d time.Duration, db *DBUsage) {
	key := method + " " + normalizePath(path)
	if isProbe(path) {
		key = ProbesRoute
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqTotal++
	a := c.routes[key]
	if a == nil {
		// Bound the route map: a path that normalises badly (or an attacker walking
		// URLs) must not be able to grow this without limit.
		if len(c.routes) >= 400 {
			return
		}
		a = &routeAgg{}
		c.routes[key] = a
	}
	minute := now.Unix() / 60
	b := &a.buckets[minute%int64(windowMins)]
	if b.minute != minute {
		*b = bucket{minute: minute}
	}
	ms := float64(d) / float64(time.Millisecond)
	b.calls++
	if status >= 400 {
		b.errors++
	}
	b.totalMs += ms
	if db != nil {
		b.dbMs += float64(db.Duration()) / float64(time.Millisecond)
		b.queries += db.Queries()
	}
	st := stamped{at: now.Unix(), ms: ms}
	if len(a.samples) < routeSampleLen {
		a.samples = append(a.samples, st)
	} else {
		a.samples[a.next] = st
		a.next = (a.next + 1) % routeSampleLen
	}
}

// StreamOpen records an SSE stream starting, at the moment its headers go out; ttfb
// is how long the handler took to get there. Pair every call with StreamClose.
func (c *Collector) StreamOpen(method, path string, ttfb time.Duration) {
	key := method + " " + normalizePath(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqTotal++
	s := c.streams[key]
	if s == nil {
		if len(c.streams) >= 100 {
			return
		}
		s = &streamAgg{}
		c.streams[key] = s
	}
	s.open++
	s.opened++
	s.ttfb.add(float64(ttfb) / float64(time.Millisecond))
}

// StreamClose records an SSE stream ending after living for life.
func (c *Collector) StreamClose(method, path string, life time.Duration) {
	key := method + " " + normalizePath(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[key]
	if s == nil {
		return // opened while the map was full
	}
	if s.open > 0 {
		s.open--
	}
	s.life.add(life.Seconds())
}

// Checkin counts a device check-in, which is the fleet's own unit of load.
func (c *Collector) Checkin() {
	c.mu.Lock()
	c.checkinTotal++
	c.mu.Unlock()
}

// Emit adds one line to the live feed. Text is expected to be short and already
// readable — the feed is for a person watching, not for later parsing.
func (c *Collector) Emit(kind, class, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, Event{At: time.Now(), Kind: kind, Text: text, Class: class})
	if len(c.events) > eventsLen {
		c.events = c.events[len(c.events)-eventsLen:]
	}
}

// SampleNow takes one point. ws comes from the hub and the db stats from the pool;
// both are passed in so this package depends on neither.
func (c *Collector) SampleNow(ws int, dbUsed, dbTotal int32, dbWaiting int64) Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	elapsed := SampleEvery.Seconds()
	if n := len(c.series); n > 0 {
		if e := now.Sub(c.series[n-1].At).Seconds(); e > 0 {
			elapsed = e
		}
	}
	s := Sample{
		At:         now,
		Requests:   float64(c.reqTotal-c.reqAtSample) / elapsed,
		Checkins:   float64(c.checkinTotal-c.chkAtSample) / elapsed * 60,
		WS:         ws,
		Goroutines: runtime.NumGoroutine(),
		HeapMB:     float64(ms.HeapAlloc) / (1 << 20),
		SysMB:      float64(ms.Sys) / (1 << 20),
		DBUsed:     dbUsed,
		DBTotal:    dbTotal,
		DBWaiting:  dbWaiting,
	}
	c.reqAtSample, c.chkAtSample = c.reqTotal, c.checkinTotal
	c.series = append(c.series, s)
	if len(c.series) > seriesLen {
		c.series = c.series[len(c.series)-seriesLen:]
	}
	return s
}

// Snapshot is what the page renders and the stream sends.
type Snapshot struct {
	UptimeSec  float64      `json:"uptime_sec"`
	Series     []Sample     `json:"series"`
	Latest     Sample       `json:"latest"`
	Routes     []RouteStat  `json:"routes"`
	Streams    []StreamStat `json:"streams"`
	Events     []Event      `json:"events"`
	ReqTotal   int64        `json:"req_total"`
	CheckTotal int64        `json:"checkin_total"`
}

// Snapshot copies the current state. Routes cover the last RouteWindow and come back
// by total time spent, most
// first — "what is the server busy with" — rather than by p95, which a single slow
// call on a rarely used route would otherwise top. Streams come back most-open first.
func (c *Collector) Snapshot(routeLimit, eventLimit int) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Snapshot{
		UptimeSec:  time.Since(c.started).Seconds(),
		ReqTotal:   c.reqTotal,
		CheckTotal: c.checkinTotal,
		Series:     append([]Sample(nil), c.series...),
	}
	if n := len(c.series); n > 0 {
		out.Latest = c.series[n-1]
	}
	now := c.now()
	oldestMin := now.Unix()/60 - int64(windowMins) + 1
	oldestSec := now.Add(-RouteWindow).Unix()
	var allMs float64
	for route, a := range c.routes {
		st := RouteStat{Route: route, Probe: route == ProbesRoute}
		var dbMs float64
		var queries int64
		for _, b := range a.buckets {
			if b.minute < oldestMin || b.calls == 0 {
				continue
			}
			st.Calls += b.calls
			st.Errors += b.errors
			st.TotalMs += b.totalMs
			dbMs += b.dbMs
			queries += b.queries
		}
		if st.Calls == 0 {
			continue // nothing in the window
		}
		var lat []float64
		for _, sm := range a.samples {
			if sm.at >= oldestSec {
				lat = append(lat, sm.ms)
			}
		}
		st.P50Ms, st.P95Ms = percentiles(lat)
		st.DBMsPerCall = dbMs / float64(st.Calls)
		st.QueriesPerCall = float64(queries) / float64(st.Calls)
		allMs += st.TotalMs
		out.Routes = append(out.Routes, st)
	}
	if allMs > 0 {
		for i := range out.Routes {
			out.Routes[i].SharePct = out.Routes[i].TotalMs * 100 / allMs
		}
	}
	sort.Slice(out.Routes, func(i, j int) bool {
		if out.Routes[i].TotalMs != out.Routes[j].TotalMs {
			return out.Routes[i].TotalMs > out.Routes[j].TotalMs
		}
		return out.Routes[i].Route < out.Routes[j].Route
	})
	if routeLimit > 0 && len(out.Routes) > routeLimit {
		out.Routes = out.Routes[:routeLimit]
	}
	for route, s := range c.streams {
		t50, t95 := percentiles(s.ttfb.v)
		l50, _ := percentiles(s.life.v)
		out.Streams = append(out.Streams, StreamStat{Route: route, Open: s.open, Opened: s.opened,
			TTFBP50Ms: t50, TTFBP95Ms: t95, LifeP50S: l50})
	}
	sort.Slice(out.Streams, func(i, j int) bool {
		if out.Streams[i].Open != out.Streams[j].Open {
			return out.Streams[i].Open > out.Streams[j].Open
		}
		return out.Streams[i].Route < out.Streams[j].Route
	})
	ev := c.events
	if eventLimit > 0 && len(ev) > eventLimit {
		ev = ev[len(ev)-eventLimit:]
	}
	out.Events = append([]Event(nil), ev...)
	// Newest first: the feed reads downward from what just happened.
	for i, j := 0, len(out.Events)-1; i < j; i, j = i+1, j-1 {
		out.Events[i], out.Events[j] = out.Events[j], out.Events[i]
	}
	return out
}

func percentiles(in []float64) (p50, p95 float64) {
	if len(in) == 0 {
		return 0, 0
	}
	s := append([]float64(nil), in...)
	sort.Float64s(s)
	at := func(q float64) float64 {
		i := int(q*float64(len(s)-1) + 0.5)
		if i < 0 {
			i = 0
		}
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return at(0.50), at(0.95)
}

// normalizePath collapses the variable parts of a URL so one route is one row rather
// than one row per device. Serials, UUIDs and numeric ids all become placeholders;
// anything unrecognised is kept, which is what makes a new route show up by itself.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	for i, seg := range parts {
		switch {
		case seg == "":
		case looksLikeUUID(seg):
			parts[i] = "{id}"
		case looksNumeric(seg):
			parts[i] = "{n}"
		case looksLikeSerial(seg):
			parts[i] = "{serial}"
		}
	}
	out := strings.Join(parts, "/")
	if out == "" {
		return "/"
	}
	return out
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(byte(r)) {
				return false
			}
		}
	}
	return true
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// looksLikeSerial matches the shapes device identities actually take here: our
// hardware serials (AT070AABU00231), the android-<id> form MDM Lite falls back to, and
// the short hex ids some stock devices report.
func looksLikeSerial(s string) bool {
	if strings.HasPrefix(s, "android-") {
		return true
	}
	if len(s) < 8 || len(s) > 40 {
		return false
	}
	digits, letters := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
			digits++
		case (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z'):
			letters++
		default:
			return false
		}
	}
	return digits >= 3 && letters >= 1
}
