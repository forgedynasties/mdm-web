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
	"runtime"
	"sort"
	"strings"
	"sync"
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
)

// Event is one thing the server did, for the live feed.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`  // checkin | ws | command | ota | peer | alert | deploy
	Text string    `json:"text"`  // already-rendered, short
	Class string   `json:"class"` // "" | ok | warn | bad
}

// RouteStat is one route's traffic over the life of the process.
type RouteStat struct {
	Route  string  `json:"route"`
	Calls  int64   `json:"calls"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	Errors int64   `json:"errors"`
}

// Sample is one point in the sampled series.
type Sample struct {
	At         time.Time `json:"at"`
	Requests   float64   `json:"requests"`   // per second since the last sample
	Checkins   float64   `json:"checkins"`   // per minute
	WS         int       `json:"ws"`         // live device WebSockets
	Goroutines int       `json:"goroutines"`
	HeapMB     float64   `json:"heap_mb"`
	SysMB      float64   `json:"sys_mb"`
	DBUsed     int32     `json:"db_used"`
	DBTotal    int32     `json:"db_total"`
	DBWaiting  int64     `json:"db_waiting"`
}

type routeAgg struct {
	calls   int64
	errors  int64
	samples []float64 // ring of latencies in ms
	next    int
}

// Collector holds it all. One per process; the default is fine.
type Collector struct {
	mu sync.Mutex

	started time.Time
	routes  map[string]*routeAgg

	reqTotal     int64 // lifetime
	reqAtSample  int64 // value at the previous sample, for the per-second rate
	checkinTotal int64
	chkAtSample  int64

	series []Sample
	events []Event
}

var Default = New()

func New() *Collector {
	return &Collector{started: time.Now(), routes: map[string]*routeAgg{}}
}

// Observe records one finished request. Called from the access-log middleware, which
// already wraps every route, so nothing has to be instrumented twice.
func (c *Collector) Observe(method, path string, status int, d time.Duration) {
	key := method + " " + normalizePath(path)
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
		a = &routeAgg{samples: make([]float64, 0, routeSampleLen)}
		c.routes[key] = a
	}
	a.calls++
	if status >= 400 {
		a.errors++
	}
	ms := float64(d) / float64(time.Millisecond)
	if len(a.samples) < routeSampleLen {
		a.samples = append(a.samples, ms)
	} else {
		a.samples[a.next] = ms
		a.next = (a.next + 1) % routeSampleLen
	}
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
	UptimeSec  float64     `json:"uptime_sec"`
	Series     []Sample    `json:"series"`
	Latest     Sample      `json:"latest"`
	Routes     []RouteStat `json:"routes"`
	Events     []Event     `json:"events"`
	ReqTotal   int64       `json:"req_total"`
	CheckTotal int64       `json:"checkin_total"`
}

// Snapshot copies the current state. Routes come back slowest-first by p95 — the
// ordering an operator wants, since the question is always "what is slow".
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
	for route, a := range c.routes {
		p50, p95 := percentiles(a.samples)
		out.Routes = append(out.Routes, RouteStat{Route: route, Calls: a.calls, P50Ms: p50, P95Ms: p95, Errors: a.errors})
	}
	sort.Slice(out.Routes, func(i, j int) bool {
		if out.Routes[i].P95Ms != out.Routes[j].P95Ms {
			return out.Routes[i].P95Ms > out.Routes[j].P95Ms
		}
		return out.Routes[i].Calls > out.Routes[j].Calls
	})
	if routeLimit > 0 && len(out.Routes) > routeLimit {
		out.Routes = out.Routes[:routeLimit]
	}
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
