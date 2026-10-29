// Command loadgen is a fleet of fake devices for measuring the MDM server (scalability
// plan item W1, "measure first"). Each fake speaks the real device protocol — HTTP
// check-in, WebSocket telemetry, command pull and ack — at the cadence of the client it
// imitates, so CPU, memory, job durations, DB write rate and check-in latency can be
// measured at fleet sizes we do not have yet.
//
// # Usage
//
//	go run ./tools/loadgen -server http://localhost:8080 -n 500 -duration 10m
//
//	-server            base URL of the MDM (http:// or https://; ws(s):// is derived)
//	-n                 number of fake devices (default 100)
//	-firmware-share    fraction behaving like the firmware client, the rest like the
//	                   Standard (DPC) client (default 0.5)
//	-ramp              time over which the devices are brought up (default 1m)
//	-duration          how long to run after the ramp starts; 0 = until Ctrl-C (default 5m)
//	-prefix            serial prefix (default LOADTEST-); serials are <prefix><n>
//	-key               device API key (default $MDM_DEVICE_API_KEY): the server's
//	                   DEVICE_API_KEY, sent as X-API-Key like both real clients do
//	-checkin-interval  override the check-in interval (default 0 = follow the
//	                   checkin_interval_seconds the server hands out, as real clients do)
//	-admin-key         admin key (default $MDM_ADMIN_KEY): when set, every report also
//	                   reads /debug/vars (ws_connected_devices, db_pool, ingest, jobs)
//	-cleanup           print the SQL that removes the fake devices, and exit
//
// A report goes to stdout every 10 s and once at the end: devices up and connected,
// check-ins, WS frames, commands acked, errors by status, p50/p95/p99 of HTTP check-in
// latency, of WS telemetry→config (the server answers every telemetry frame with a
// config frame, so that is the send→ack we can see) and of WS ping→pong, and how long
// the fleet took to come back each time a large share of sockets dropped at once (a
// server restart): from the first disconnect until every device is back.
//
// # What the fakes are, and what they are not
//
// Product key "loadtest", no device class, a build id outside the v2.x lines and an
// agent package that is not ours, so nothing that targets real hardware targets them:
// role-app installs and role firmware go by class (Device.Class() of an unknown product
// is ""), client auto-update requires the hosted package (versionComparable), APK
// auto-pull requires the apk_pull capability / apk_pull_cmd flag and aio.app.* packages,
// and they have no venue, so no lock rule or venue alert covers them. They send no
// wifi_scan, so the server never calls Google's geolocation API for them.
//
// They do still create real rows (devices, samples, state events, packages) and fleet-
// wide alerts can fire for them: a "new_device" alert per fake if that rule is on, and
// "offline" alerts 30 min after the run stops. Run -cleanup afterwards. Never point this
// at live; on stage, tell the team first (alert channels may notify).
//
// The default command answer is a terminal ack — "completed", or "failed" for anything
// install-shaped — so nothing queued for a fake piles up or is retried forever.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Cadences copied from the clients, so a fake costs the server what a real device does.
// Firmware: MdmService.java (HTTP_SAFETY_NET_MS, COMMAND_POLL_MS, COMMAND_POLL_FLOOR_MS,
// STALE_WS_THRESHOLD_SECS) and MdmWebSocketClient.java (BACKOFF_MS, 30 s client ping,
// 45 s dead-link rule). Standard client: MdmService.kt (same constants by design).
const (
	httpSafetyNet    = 5 * time.Minute // a full HTTP keyframe at least this often
	commandPollEvery = time.Minute     // pull cadence while the socket is not usable
	commandPollFloor = 5 * time.Minute // pull at least this often even on a healthy socket
	clientPingEvery  = 30 * time.Second
	deadLinkAfter    = 75 * time.Second // client ping + 45 s grace: no data at all means dead
	staleWS          = 120 * time.Second
	reportEvery      = 10 * time.Second

	// The Standard client sends a WS delta only when a gated key, the battery or a
	// temperature band moved (TelemetryDelta.kt); measured, that is ~390 stored rows a
	// day against ~288 HTTP keyframes, so roughly 1 opportunity in 30 sends a frame.
	stdDeltaChance = 0.035

	// An "outage" worth measuring is a drop of at least this share of the sockets that
	// were up: a server restart drops them all, a single device's blip does not count.
	outageShare = 0.10
)

// Reconnect backoff with equal jitter, exactly as both clients do it: a restart drops
// the whole fleet at once, and without jitter every device retries in lockstep.
var backoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 30 * time.Second}

func (d *device) backoffDelay(attempt int) time.Duration {
	base := backoff[min(attempt, len(backoff)-1)]
	return base/2 + time.Duration(d.randInt64N(int64(base/2)+1))
}

type options struct {
	server        string
	n             int
	firmwareShare float64
	ramp          time.Duration
	duration      time.Duration
	prefix        string
	key           string
	adminKey      string
	interval      time.Duration
	cleanup       bool
}

func main() {
	var o options
	flag.StringVar(&o.server, "server", "http://localhost:8080", "MDM base URL")
	flag.IntVar(&o.n, "n", 100, "number of fake devices")
	flag.Float64Var(&o.firmwareShare, "firmware-share", 0.5, "fraction acting like the firmware client (rest: Standard client)")
	flag.DurationVar(&o.ramp, "ramp", time.Minute, "time to bring all devices up")
	flag.DurationVar(&o.duration, "duration", 5*time.Minute, "run time from start (0 = until interrupted)")
	flag.StringVar(&o.prefix, "prefix", "LOADTEST-", "serial prefix")
	flag.StringVar(&o.key, "key", os.Getenv("MDM_DEVICE_API_KEY"), "device API key (X-API-Key); default $MDM_DEVICE_API_KEY")
	flag.StringVar(&o.adminKey, "admin-key", os.Getenv("MDM_ADMIN_KEY"), "admin key for /debug/vars in reports; default $MDM_ADMIN_KEY")
	flag.DurationVar(&o.interval, "checkin-interval", 0, "override the check-in interval (0 = what the server says)")
	flag.BoolVar(&o.cleanup, "cleanup", false, "print the SQL that deletes the fake devices and exit")
	flag.Parse()

	if !validPrefix(o.prefix) {
		fatalf("-prefix %q: a serial may only hold letters, digits, '-' and '_' (the server refuses anything else)", o.prefix)
	}
	if o.cleanup {
		fmt.Print(cleanupSQL(o.prefix))
		return
	}
	if o.key == "" {
		fatalf("no device key: pass -key or set MDM_DEVICE_API_KEY")
	}
	if o.n < 1 || o.firmwareShare < 0 || o.firmwareShare > 1 {
		fatalf("-n must be >= 1 and -firmware-share within 0..1")
	}
	base, err := url.Parse(strings.TrimRight(o.server, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		fatalf("-server %q: want http(s)://host[:port]", o.server)
	}
	// The two hosts this must never be aimed at by accident. Not a security boundary —
	// whoever really means it can edit this line — just a guard against a pasted URL.
	if h := base.Hostname(); h == "mdm.dev.aioapp.com" && os.Getenv("LOADGEN_I_MEAN_LIVE") != "yes" {
		fatalf("refusing to load the live server (%s)", h)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.duration)
		defer cancel()
	}

	g := newGen(o, base)
	fmt.Printf("loadgen: %d devices (%d firmware, %d standard) → %s, ramp %s, duration %s\n",
		o.n, g.firmwareCount(), o.n-g.firmwareCount(), base, o.ramp, durOr(o.duration, "until Ctrl-C"))

	var wg sync.WaitGroup
	for i := 0; i < o.n; i++ {
		d := g.newDevice(i)
		// Evenly over the ramp, so the server sees enrolment as a steady stream and the
		// devices' 5-minute HTTP keyframes are spread rather than synchronised.
		delay := time.Duration(0)
		if o.n > 1 {
			delay = time.Duration(int64(o.ramp) * int64(i) / int64(o.n))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.run(ctx, delay)
		}()
	}
	go g.st.watchOutages(ctx, g)

	t := time.NewTicker(reportEvery)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			g.report(false)
		}
	}
	// Devices close their sockets on ctx; give them a moment so the final numbers are
	// whole, but never hang on a wedged one.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	g.report(true)
}

// ── generator (shared state) ─────────────────────────────────────────────────

type gen struct {
	o        options
	base     *url.URL
	wsURL    string
	client   *http.Client
	dialer   *websocket.Dialer
	st       *stats
	started  time.Time
	interval atomic.Int64 // seconds; adopted from the server's config unless overridden
}

func newGen(o options, base *url.URL) *gen {
	ws := *base
	if ws.Scheme == "https" {
		ws.Scheme = "wss"
	} else {
		ws.Scheme = "ws"
	}
	ws.Path = strings.TrimRight(ws.Path, "/") + "/api/v1/ws"
	// One transport for every fake, with enough idle connections that each device keeps
	// one warm, the way a real device keeps its own; otherwise every check-in would pay a
	// new TCP (and TLS) handshake and the latency measured would be the client's, not the
	// server's.
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        o.n * 2,
		MaxIdleConnsPerHost: o.n * 2,
		IdleConnTimeout:     6 * time.Minute,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	g := &gen{
		o:       o,
		base:    base,
		wsURL:   ws.String(),
		client:  &http.Client{Transport: tr, Timeout: 30 * time.Second},
		dialer:  &websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment},
		st:      newStats(),
		started: time.Now(),
	}
	sec := int64(60) // the server's default until its first answer says otherwise
	if o.interval > 0 {
		sec = int64(o.interval / time.Second)
	}
	g.interval.Store(max(sec, 10))
	return g
}

func (g *gen) checkinInterval() time.Duration { return time.Duration(g.interval.Load()) * time.Second }

// adoptInterval follows checkin_interval_seconds from a config answer, as applyConfig does
// in both clients (ignored below 10 s, like they do).
func (g *gen) adoptInterval(cfg map[string]any) {
	if g.o.interval > 0 {
		return
	}
	if v, ok := cfg["checkin_interval_seconds"].(float64); ok && v >= 10 {
		g.interval.Store(int64(v))
	}
}

// isFirmware spreads the two kinds evenly through the serial range rather than in two
// blocks, so a partial ramp already has the requested mix.
func (g *gen) isFirmware(i int) bool {
	s := g.o.firmwareShare
	return int(float64(i+1)*s) > int(float64(i)*s)
}

func (g *gen) firmwareCount() int {
	n := 0
	for i := 0; i < g.o.n; i++ {
		if g.isFirmware(i) {
			n++
		}
	}
	return n
}

func (g *gen) newDevice(i int) *device {
	width := len(strconv.Itoa(g.o.n))
	d := &device{
		g:       g,
		serial:  fmt.Sprintf("%s%0*d", g.o.prefix, width, i+1),
		fw:      g.isFirmware(i),
		rng:     rand.New(rand.NewPCG(uint64(i)+1, uint64(time.Now().UnixNano()))),
		boot:    time.Now().Add(-time.Duration(rand.IntN(72*3600)) * time.Second),
		pollNow: make(chan struct{}, 1),
	}
	d.battery = 40 + d.rng.IntN(55)
	d.charging = d.rng.IntN(2) == 0
	d.temp = 30 + d.rng.Float64()*5
	d.storage = 8 + float64(d.rng.IntN(40))/10
	d.ram = 900 + d.rng.IntN(600)
	d.rssi = -45 - d.rng.IntN(30)
	return d
}

// ── one fake device ──────────────────────────────────────────────────────────

type device struct {
	g      *gen
	serial string
	fw     bool       // firmware client, else Standard client
	rng    *rand.Rand // under mu once the device runs (see randInt64N)
	boot   time.Time

	// Simulated readings. Kept healthy on purpose (charge 30-95 %, cool, RAM well under
	// the 85 % rule) so the fakes trip no battery/thermal/memory alert of their own.
	mu       sync.Mutex
	battery  int
	charging bool
	temp     float64
	storage  float64
	ram      int
	rssi     int

	// Touched from the tick loop, the socket reader and checkin_now, hence atomics.
	appsKnown atomic.Bool  // the server has our app list (sent once, again on send_apps)
	lastHTTP  atomic.Int64 // unix nanos of the last HTTP keyframe attempt (stamped before sending, as the clients do)
	notBefore atomic.Int64 // unix nanos: a 429's Retry-After, honoured as a floor on the next check-in

	wmu       sync.Mutex // guards conn writes; gorilla allows one concurrent writer
	conn      *websocket.Conn
	lastData  atomic.Int64 // unix nanos of the last frame from the server
	sentAt    []time.Time  // telemetry frames waiting for their config answer (under wmu)
	connected atomic.Bool

	lastPoll time.Time
	pollNow  chan struct{}
}

func (d *device) run(ctx context.Context, delay time.Duration) {
	if !sleepCtx(ctx, delay) {
		return
	}
	d.g.st.started.Add(1)

	// The first HTTP check-in comes before the socket. A socket from a serial with no row
	// makes the server create one with no product (api.Connect), and an empty product is
	// read as the default product (T7) — for that moment a fake would be a Tableside AI
	// tablet to every job. The check-in fills in "loadtest" first.
	for attempt := 0; ; attempt++ {
		if d.httpCheckin(ctx) {
			break
		}
		if !sleepCtx(ctx, d.backoffDelay(attempt)) {
			return
		}
	}

	go d.wsLoop(ctx)
	go d.pollLoop(ctx)

	// A random phase inside the first interval, so devices that came up together do not
	// tick together for the rest of the run.
	if !sleepCtx(ctx, time.Duration(d.randInt64N(int64(d.g.checkinInterval())))) {
		return
	}
	for {
		d.tick(ctx)
		if !sleepCtx(ctx, d.g.checkinInterval()) {
			return
		}
	}
}

// tick is one pass of the client's check-in loop.
func (d *device) tick(ctx context.Context) {
	now := time.Now()
	httpDue := now.Sub(time.Unix(0, d.lastHTTP.Load())) >= httpSafetyNet
	if d.fw {
		// MdmService.performCheckin: telemetry over a live socket (a stale one is torn
		// down instead), and the HTTP keyframe every 5 minutes whatever the socket does.
		// With no socket there is no per-tick HTTP: the firmware client only checks in
		// over HTTP on the safety net, and pulls commands on its own loop.
		if d.connected.Load() {
			if since := time.Since(time.Unix(0, d.lastData.Load())); since > staleWS {
				d.closeConn()
			} else if !d.sendTelemetry(false) {
				// sendTelemetryOverWs falls back to an HTTP check-in when the send fails.
				d.httpCheckin(ctx)
				return
			}
		}
		if httpDue {
			d.httpCheckin(ctx)
		}
		return
	}
	// MdmService.kt startCheckinLoop: HTTP whenever the socket is down, the safety net is
	// due or the app list is still owed; otherwise a WS delta (often nothing at all).
	if !d.connected.Load() || httpDue || !d.appsKnown.Load() {
		d.httpCheckin(ctx)
		return
	}
	d.sendTelemetry(false)
}

// rand.Rand is not safe for concurrent use and a device's goroutines (tick loop, socket,
// reconnect) all draw from it, so every draw outside evolve goes through d.mu.
func (d *device) randInt64N(n int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rng.Int64N(n)
}

func (d *device) randFloat() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rng.Float64()
}

// evolve moves the readings a little, the way a device on a charger or on a table does.
func (d *device) evolve() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rng.IntN(40) == 0 {
		d.charging = !d.charging
	}
	if d.rng.IntN(6) == 0 {
		if d.charging {
			d.battery = min(d.battery+1, 95)
		} else {
			d.battery = max(d.battery-1, 30)
		}
	}
	if d.battery >= 95 {
		d.charging = false
	} else if d.battery <= 30 {
		d.charging = true
	}
	d.temp = math.Max(28, math.Min(37, d.temp+d.rng.Float64()-0.5))
	d.ram = max(700, min(1700, d.ram+d.rng.IntN(41)-20))
	d.rssi = max(-80, min(-40, d.rssi+d.rng.IntN(5)-2))
}

// ── payloads ─────────────────────────────────────────────────────────────────

// The installed-app list: small and fixed, so the inventory path (device_packages,
// packages hash) is exercised once per device without inflating every request. No
// aio.app.* package, so the APK auto-pull never asks a fake for one.
var fakeApps = []map[string]any{
	{"package": "com.android.settings", "name": "Settings", "version_name": "15", "is_system": true, "launchable": true},
	{"package": "com.android.chrome", "name": "Chrome", "version_name": "129.0.6668.100", "is_system": true, "launchable": true},
	{"package": "com.google.android.webview", "name": "Android System WebView", "version_name": "129.0.6668.100", "is_system": true, "launchable": false},
	{"package": "com.example.loadgen.menu", "name": "Loadgen Menu", "version_name": "1.0.0", "is_system": false, "launchable": true},
	{"package": "com.example.loadgen.helper", "name": "Loadgen Helper", "version_name": "2.3.1", "is_system": false, "launchable": false},
}

// appsHash is each client's own hash over its app list, sent as apps_hash on every
// keyframe. The server does not read it (it hashes the list itself and says send_apps
// when it needs one); it is here so the payload is the size and shape real ones are.
// Firmware: CRC32 over the sorted package names, hex. Standard: CRC32 over sorted
// "package:version" lines joined by '\n', hex.
func appsHash(firmware bool) string {
	var parts []string
	for _, a := range fakeApps {
		if firmware {
			parts = append(parts, a["package"].(string))
		} else {
			parts = append(parts, a["package"].(string)+":"+a["version_name"].(string))
		}
	}
	sort.Strings(parts)
	sep := ""
	if !firmware {
		sep = "\n"
	}
	return strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(strings.Join(parts, sep)))), 16)
}

// fakeCaps is what the fakes say they can run. Not apk_pull (the APK auto-pull would ask
// them for apps), not ota/update_splash/wlc/mic_gain (firmware-only hardware paths), not
// screen_capture/input (a remote session needs frames a fake does not send).
var fakeCaps = []string{"kiosk", "install_apk", "uninstall", "reboot", "telemetry", "logcat", "shell", "config"}

const (
	fakeProduct = "loadtest"
	fakeBuild   = "loadtest-1.0.0"
)

// extra is the device's full report: the keys the server's own logic reads (agent
// identity, http_commands, battery/temperature, storage, RAM, Wi-Fi, uptime, timezone,
// kiosk state) with plausible values. Not every key the real clients send — the rest
// only feed device-page cards and cost the server one JSONB merge either way.
func (d *device) extra() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	uptime := int64(time.Since(d.boot).Seconds())
	e := map[string]any{
		// Commands come by pull after a "wake" frame, as with both real clients today.
		"http_commands":    true,
		"uptime_seconds":   uptime,
		"timezone":         "America/Los_Angeles",
		"storage_free_gb":  d.storage,
		"charging":         d.charging,
		"wifi":             "LoadgenNet",
		"wifi_rssi":        d.rssi,
		"ip_address":       "10.99.0.1",
		"screen_on":        true,
		"adb_enabled":      false,
		"lost_mode":        map[string]any{"on": false},
		"kiosk_staff_mode": false,
	}
	if d.fw {
		// Not "com.aioapp.mdm": versionComparable then keeps client auto-update away.
		e["agent_package"] = "com.example.loadgen.firmware"
		e["agent_version"] = "1.22.1"
		e["agent_version_code"] = 999
		e["build_type"] = "user"
		e["build_tags"] = "release-keys"
		e["ota_supported"] = false
		e["battery_temp_c"] = round1(d.temp)
		e["ram_usage_mb"] = d.ram
		e["kiosk_suspended"] = false
		// The real firmware client sends no capability list: its capabilities come from
		// the product catalog (DefaultCaps), which has none for "loadtest", so without this
		// a fake would refuse every command at creation. A firmware device's reported list
		// is merged into its set (Device.CapSet), so naming the plain ones makes the fakes
		// commandable for fan-out tests without claiming OTA, splash, WLC or remote screen.
		e["capabilities"] = fakeCaps
	} else {
		e["agent_type"] = "dpc" // what makes the server file it as a Standard-client device
		e["agent_package"] = "com.example.loadgen.dpc"
		e["agent_version"] = "0.13.1"
		e["agent_version_code"] = 999
		e["self_update_silent"] = false
		// No apk_pull (auto-pull), no ota: the generic stock-device set.
		e["capabilities"] = fakeCaps
		e["capabilities_degraded"] = []string{}
		// Model and manufacturer feed ClassForModel's guess; these contain none of its
		// keywords (kiosk, pos, hk1, rk3568…), so the fake stays classless.
		e["model"] = "Loadgen Virtual"
		e["manufacturer"] = "AIO Loadgen"
		e["android_release"] = "14"
		e["sdk_int"] = 34
		e["leanback"] = false
		e["cpu_temp_c"] = round1(d.temp + 12)
		e["battery_temp_c"] = round1(d.temp)
		e["ram_usage_mb"] = map[string]any{"total": 2048, "available": 2048 - d.ram, "used": d.ram}
		e["storage_total_bytes"] = int64(16) << 30
		e["kiosk_active"] = false
	}
	return e
}

// payload is a check-in body. full = a keyframe (the HTTP check-in and the first frame on
// a socket); otherwise a delta with the identity, volatile and a few changed keys, which
// the server merges into what it has.
func (d *device) payload(full, withApps bool) map[string]any {
	d.evolve()
	p := map[string]any{
		"serial_number": d.serial,
		"build_id":      fakeBuild,
		"product":       fakeProduct, // on every frame: an empty product reads as T7
	}
	d.mu.Lock()
	p["battery_pct"] = d.battery
	d.mu.Unlock()
	e := d.extra()
	if full {
		p["extra"] = e
		p["apps_hash"] = appsHash(d.fw)
		if withApps {
			p["installed_apps"] = fakeApps
		}
		return p
	}
	delta := map[string]any{}
	keep := []string{"uptime_seconds", "ram_usage_mb", "wifi_rssi", "battery_temp_c", "cpu_temp_c", "charging", "agent_type", "http_commands"}
	for _, k := range keep {
		if v, ok := e[k]; ok {
			delta[k] = v
		}
	}
	p["extra"] = delta
	return p
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type checkinAnswer struct {
	Status   string           `json:"status"`
	SendApps bool             `json:"send_apps"`
	Commands []map[string]any `json:"commands"`
	Config   map[string]any   `json:"config"`
}

// httpCheckin posts a full keyframe to /api/v1/checkin and handles the answer the way
// both clients do: adopt the interval, owe the app list again on send_apps, run any
// commands the response carries (the backup command channel), honour Retry-After.
func (d *device) httpCheckin(ctx context.Context) bool {
	if time.Now().UnixNano() < d.notBefore.Load() {
		return false
	}
	withApps := !d.appsKnown.Load()
	body, _ := json.Marshal(d.payload(true, withApps))
	d.lastHTTP.Store(time.Now().UnixNano())
	st := d.g.st
	t0 := time.Now()
	resp, err := d.post(ctx, "/api/v1/checkin", body)
	if err != nil {
		if ctx.Err() == nil {
			st.errorf("checkin %s", netErrKind(err))
		}
		return false
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	st.checkinLat.add(time.Since(t0))
	st.checkins.Add(1)
	if resp.StatusCode != http.StatusOK {
		st.errorf("checkin %d", resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests {
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				d.notBefore.Store(time.Now().Add(time.Duration(s) * time.Second).UnixNano())
			}
		}
		return false
	}
	var a checkinAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		st.errorf("checkin bad json")
		return false
	}
	st.checkinsOK.Add(1)
	if withApps {
		d.appsKnown.Store(true)
	}
	if a.SendApps {
		d.appsKnown.Store(false)
	}
	if a.Config != nil {
		d.g.adoptInterval(a.Config)
	}
	for _, c := range a.Commands {
		d.runCommand(ctx, c)
	}
	return true
}

func (d *device) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.g.base.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", d.g.o.key)
	return d.g.client.Do(req)
}

// pollLoop is the command pull (GET /api/v1/commands/pending): every minute while the
// socket is not usable, at a 5-minute floor even when it is (a half-open socket swallows
// wakes), and at once when a "wake" frame says there is work.
func (d *device) pollLoop(ctx context.Context) {
	if !sleepCtx(ctx, 15*time.Second) { // both clients let the first check-in settle
		return
	}
	t := time.NewTicker(commandPollEvery)
	defer t.Stop()
	for {
		nudged := false
		select {
		case <-ctx.Done():
			return
		case <-d.pollNow:
			nudged = true
		case <-t.C:
		}
		usable := d.connected.Load() && time.Since(time.Unix(0, d.lastData.Load())) <= staleWS
		if usable && !nudged && time.Since(d.lastPoll) < commandPollFloor {
			continue
		}
		d.lastPoll = time.Now()
		d.pollCommands(ctx)
	}
}

func (d *device) pollCommands(ctx context.Context) {
	st := d.g.st
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.g.base.String()+"/api/v1/commands/pending?serial="+url.QueryEscape(d.serial), nil)
	if err != nil {
		return
	}
	req.Header.Set("X-API-Key", d.g.o.key)
	t0 := time.Now()
	resp, err := d.g.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			st.errorf("pull %s", netErrKind(err))
		}
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	st.pollLat.add(time.Since(t0))
	st.polls.Add(1)
	if resp.StatusCode != http.StatusOK {
		st.errorf("pull %d", resp.StatusCode)
		return
	}
	var a struct {
		Commands []map[string]any `json:"commands"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return
	}
	for _, c := range a.Commands {
		d.runCommand(ctx, c)
	}
}

// ── commands ─────────────────────────────────────────────────────────────────

// installShaped commands are answered "failed": a fake installs nothing, and saying so
// is what stops the server's retry and give-up logic from treating it as done.
var installShaped = map[string]bool{
	"install_apk": true, "app_update": true, "uninstall": true, "ota": true, "pull_apk": true,
	"update_splash": true, "wipe": true,
}

// runCommand acknowledges receipt, then answers with a terminal status — over the socket
// when it is up (as the Standard client's Acker does), else over HTTP.
func (d *device) runCommand(ctx context.Context, c map[string]any) {
	id, _ := c["id"].(string)
	if id == "" {
		return
	}
	typ, _ := c["command_type"].(string)
	if typ == "" {
		typ, _ = c["type"].(string)
	}
	d.g.st.commands.Add(1)
	d.ack(ctx, id, "received", "")
	status, out := "completed", ""
	if installShaped[typ] {
		status, out = "failed", "loadgen fake device: "+typ+" not executed"
	}
	d.ack(ctx, id, status, out)
	d.g.st.acks.Add(1)
}

func (d *device) ack(ctx context.Context, id, status, output string) {
	if d.connected.Load() {
		frame := map[string]any{"type": "command_ack", "command_id": id, "serial_number": d.serial, "status": status}
		if output != "" {
			frame["output"] = output
		}
		if d.writeJSON(frame) == nil {
			return
		}
	}
	body := map[string]any{"serial_number": d.serial, "status": status}
	if output != "" {
		body["output"] = output
	}
	raw, _ := json.Marshal(body)
	resp, err := d.post(ctx, "/api/v1/commands/"+url.PathEscape(id)+"/ack", raw)
	if err != nil {
		if ctx.Err() == nil {
			d.g.st.errorf("ack %s", netErrKind(err))
		}
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.g.st.errorf("ack %d", resp.StatusCode)
	}
}

// ── WebSocket ────────────────────────────────────────────────────────────────

func (d *device) wsLoop(ctx context.Context) {
	header := http.Header{"X-API-Key": []string{d.g.o.key}}
	u := d.g.wsURL + "?serial=" + url.QueryEscape(d.serial)
	st := d.g.st
	for attempt := 0; ctx.Err() == nil; attempt++ {
		conn, resp, err := d.g.dialer.DialContext(ctx, u, header)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if resp != nil {
				st.errorf("ws dial %d", resp.StatusCode)
			} else {
				st.errorf("ws dial %s", netErrKind(err))
			}
			if !sleepCtx(ctx, d.backoffDelay(attempt)) {
				return
			}
			continue
		}
		attempt = -1 // a connect resets the backoff (the loop's ++ makes it 0)
		d.serve(ctx, conn)
		if ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, d.backoffDelay(0)) {
			return
		}
	}
}

// serve runs one socket until it drops.
func (d *device) serve(ctx context.Context, conn *websocket.Conn) {
	st := d.g.st
	d.wmu.Lock()
	d.conn = conn
	d.sentAt = nil
	d.wmu.Unlock()
	d.lastData.Store(time.Now().UnixNano())
	d.connected.Store(true)
	st.noteConnect()

	alive := func() {
		d.lastData.Store(time.Now().UnixNano())
		_ = conn.SetReadDeadline(time.Now().Add(deadLinkAfter))
	}
	_ = conn.SetReadDeadline(time.Now().Add(deadLinkAfter))
	conn.SetPingHandler(func(p string) error {
		alive()
		err := conn.WriteControl(websocket.PongMessage, []byte(p), time.Now().Add(10*time.Second))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		return err
	})
	conn.SetPongHandler(func(p string) error {
		alive()
		if len(p) == 8 {
			sent := int64(binary.BigEndian.Uint64([]byte(p)))
			st.pingLat.add(time.Duration(time.Now().UnixNano() - sent))
		}
		return nil
	})

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The client's own keepalive ping (both clients ping every 30 s; through the ALB this
	// is what keeps an idle socket open). Its pong is also our round-trip measure.
	go func() {
		t := time.NewTicker(clientPingEvery)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				var b [8]byte
				binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
				if conn.WriteControl(websocket.PingMessage, b[:], time.Now().Add(10*time.Second)) != nil {
					return
				}
			}
		}
	}()
	go func() { <-sctx.Done(); conn.Close() }() // ctx ends the run: drop the socket

	// The keyframe on (re)connect, as both clients send.
	d.sendTelemetry(true)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		alive()
		st.wsIn.Add(1)
		d.handleFrame(sctx, msg)
	}

	d.connected.Store(false)
	d.wmu.Lock()
	d.conn = nil
	d.sentAt = nil
	d.wmu.Unlock()
	conn.Close()
	if ctx.Err() == nil {
		st.noteDisconnect()
	} else {
		st.connected.Add(-1) // the run is ending, not an outage
	}
}

func (d *device) closeConn() {
	d.wmu.Lock()
	c := d.conn
	d.wmu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (d *device) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if d.conn == nil {
		return errors.New("no socket")
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return d.conn.WriteMessage(websocket.TextMessage, b)
}

// sendTelemetry sends one telemetry frame: a keyframe, or for the firmware client a delta
// on every opportunity, for the Standard client a delta only now and then (its frames are
// gated on change). Returns false only when a send was attempted and failed.
func (d *device) sendTelemetry(keyframe bool) bool {
	if !keyframe && !d.fw && d.randFloat() >= stdDeltaChance {
		return true // nothing changed that the Standard client would send
	}
	p := d.payload(keyframe, keyframe && !d.appsKnown.Load())
	p["type"] = "telemetry"
	b, _ := json.Marshal(p)
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if d.conn == nil {
		return false
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := d.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		d.g.st.errorf("ws send")
		return false
	}
	if _, ok := p["installed_apps"]; ok {
		d.appsKnown.Store(true)
	}
	// Bounded: if the server stops answering, the queue must not grow with it.
	if len(d.sentAt) < 64 {
		d.sentAt = append(d.sentAt, time.Now())
	}
	d.g.st.wsOut.Add(1)
	return true
}

func (d *device) handleFrame(ctx context.Context, msg []byte) {
	var m map[string]any
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	typ, _ := m["type"].(string)
	st := d.g.st
	switch typ {
	case "config":
		// The answer to a telemetry frame carries the per-check-in keys (temp_fast_sec is
		// set by ingestCheckin and nothing else); a config pushed from the dashboard does
		// not, and must not be matched against a pending send.
		if _, ok := m["temp_fast_sec"]; ok {
			d.wmu.Lock()
			if len(d.sentAt) > 0 {
				st.wsLat.add(time.Since(d.sentAt[0]))
				d.sentAt = d.sentAt[1:]
			}
			d.wmu.Unlock()
			st.configs.Add(1)
		}
		d.g.adoptInterval(m)
	case "telemetry_request":
		// The server asks every check-in interval; both clients answer with a delta.
		st.telemetryReqs.Add(1)
		go d.sendTelemetry(false)
	case "ping_request":
		go d.writeJSON(map[string]any{"type": "pong_response", "nonce": m["nonce"]})
	case "wake":
		select {
		case d.pollNow <- struct{}{}:
		default:
		}
	case "checkin_now":
		go d.httpCheckin(ctx)
	case "command":
		go d.runCommand(ctx, m)
	case "logcat_request":
		id, _ := m["id"].(string)
		go d.writeJSON(map[string]any{"type": "logcat_result", "request_id": id, "content": "loadgen: no logcat"})
	default:
		// guest, cancel_command, wake_screen, remote/shell session frames: nothing a fake
		// needs to answer for the queue to stay clean.
	}
}

// ── stats ────────────────────────────────────────────────────────────────────

type stats struct {
	started       atomic.Int64
	connected     atomic.Int64
	everConnected atomic.Int64 // devices that have held a socket at least once
	checkins      atomic.Int64 // HTTP check-ins answered (any status)
	checkinsOK    atomic.Int64
	polls         atomic.Int64
	wsOut         atomic.Int64 // telemetry frames sent
	wsIn          atomic.Int64 // frames received
	configs       atomic.Int64 // telemetry answers received
	telemetryReqs atomic.Int64
	commands      atomic.Int64
	acks          atomic.Int64
	connects      atomic.Int64
	disconnects   atomic.Int64

	checkinLat, pollLat, wsLat, pingLat *latencies

	mu     sync.Mutex
	errs   map[string]int64
	prev   map[string]int64 // counters at the previous report, for per-window rates
	outage *outage
	done   []outage
	peak   int64 // sockets up when the current outage began
}

type outage struct {
	firstDown  time.Time
	firstBack  time.Time
	allBack    time.Time
	minUp      int64
	before     int64
	recovering bool
}

func newStats() *stats {
	return &stats{
		errs: map[string]int64{}, prev: map[string]int64{},
		checkinLat: newLatencies(), pollLat: newLatencies(), wsLat: newLatencies(), pingLat: newLatencies(),
	}
}

func (s *stats) errorf(format string, a ...any) {
	k := fmt.Sprintf(format, a...)
	s.mu.Lock()
	s.errs[k]++
	s.mu.Unlock()
}

// The outage bookkeeping is driven from connects/disconnects so the first disconnect is
// timed exactly, not to the resolution of a polling loop.
func (s *stats) noteConnect() {
	s.connects.Add(1)
	s.connected.Add(1)
	s.mu.Lock()
	if o := s.outage; o != nil && o.firstBack.IsZero() {
		o.firstBack = time.Now()
	}
	s.mu.Unlock()
}

func (s *stats) noteDisconnect() {
	s.disconnects.Add(1)
	up := s.connected.Add(-1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outage == nil {
		s.outage = &outage{firstDown: time.Now(), before: up + 1, minUp: up}
		return
	}
	if up < s.outage.minUp {
		s.outage.minUp = up
	}
}

// watchOutages decides whether a run of disconnects was an outage (a large share of the
// sockets at once) or one device's blip, and times the recovery: done when every device
// that has ever connected holds a socket again.
func (s *stats) watchOutages(ctx context.Context, g *gen) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		o := s.outage
		if o == nil {
			s.mu.Unlock()
			continue
		}
		up := s.connected.Load()
		lost := o.before - o.minUp
		if !o.recovering && float64(lost) >= math.Max(2, outageShare*float64(o.before)) {
			o.recovering = true
			fmt.Printf("%s  outage: %d of %d sockets dropped — timing the recovery\n", stamp(g.started), lost, o.before)
		}
		if up >= o.before {
			if o.recovering {
				o.allBack = time.Now()
				s.done = append(s.done, *o)
				fmt.Printf("%s  recovered: all %d back %.1fs after the first disconnect (first back after %.1fs)\n",
					stamp(g.started), o.before, o.allBack.Sub(o.firstDown).Seconds(), o.firstBack.Sub(o.firstDown).Seconds())
			}
			s.outage = nil // a blip, or a finished outage
		}
		s.mu.Unlock()
	}
}

// latencies keeps every sample of the current report window and a bounded reservoir of
// the whole run (Algorithm R), so percentiles stay honest without unbounded memory.
type latencies struct {
	mu     sync.Mutex
	window []float64
	all    []float64
	seen   int64
	rng    *rand.Rand
}

const reservoirSize = 200_000

func newLatencies() *latencies { return &latencies{rng: rand.New(rand.NewPCG(1, 2))} }

func (l *latencies) add(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.window = append(l.window, ms)
	l.seen++
	if len(l.all) < reservoirSize {
		l.all = append(l.all, ms)
	} else if j := l.rng.Int64N(l.seen); j < reservoirSize {
		l.all[j] = ms
	}
}

// take returns the window's samples (and clears them) or the whole run's.
func (l *latencies) take(final bool) []float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if final {
		return append([]float64(nil), l.all...)
	}
	w := l.window
	l.window = nil
	return w
}

func pct(xs []float64) string {
	if len(xs) == 0 {
		return "—"
	}
	sort.Float64s(xs)
	at := func(p float64) float64 { return xs[min(len(xs)-1, int(math.Ceil(p*float64(len(xs))))-1)] }
	return fmt.Sprintf("p50 %s p95 %s p99 %s max %s (n=%d)", ms(at(.50)), ms(at(.95)), ms(at(.99)), ms(xs[len(xs)-1]), len(xs))
}

func ms(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2fs", v/1000)
	}
	return fmt.Sprintf("%.0fms", v)
}

func (g *gen) report(final bool) {
	s := g.st
	head := stamp(g.started)
	if final {
		head = "FINAL " + head
	}
	cur := map[string]int64{
		"checkins": s.checkins.Load(), "wsOut": s.wsOut.Load(), "configs": s.configs.Load(),
		"polls": s.polls.Load(), "acks": s.acks.Load(),
	}
	s.mu.Lock()
	delta := func(k string) int64 {
		if final {
			return cur[k]
		}
		return cur[k] - s.prev[k]
	}
	dc, dw, dcf, dp, da := delta("checkins"), delta("wsOut"), delta("configs"), delta("polls"), delta("acks")
	if !final {
		s.prev = cur
	}
	errKeys := make([]string, 0, len(s.errs))
	for k := range s.errs {
		errKeys = append(errKeys, k)
	}
	sort.Strings(errKeys)
	var errParts []string
	for _, k := range errKeys {
		errParts = append(errParts, fmt.Sprintf("%s ×%d", k, s.errs[k]))
	}
	outages := append([]outage(nil), s.done...)
	var open *outage // one still recovering when the run ends is reported too
	if s.outage != nil && s.outage.recovering {
		o := *s.outage
		open = &o
	}
	s.mu.Unlock()

	scope := "last 10s"
	if final {
		scope = "whole run"
	}
	fmt.Printf("%s  devices %d/%d up, ws %d connected | %s: checkins %d, ws frames %d (answered %d), pulls %d, commands acked %d | totals: checkins %d ok %d, connects %d, drops %d\n",
		head, s.started.Load(), g.o.n, s.connected.Load(), scope, dc, dw, dcf, dp, da,
		s.checkins.Load(), s.checkinsOK.Load(), s.connects.Load(), s.disconnects.Load())
	fmt.Printf("    http checkin  %s\n", pct(s.checkinLat.take(final)))
	fmt.Printf("    ws telem→cfg  %s\n", pct(s.wsLat.take(final)))
	fmt.Printf("    ws ping→pong  %s\n", pct(s.pingLat.take(final)))
	if final {
		fmt.Printf("    command pull  %s\n", pct(s.pollLat.take(true)))
	} else {
		s.pollLat.take(false)
	}
	if len(errParts) > 0 {
		fmt.Printf("    errors (run)  %s\n", strings.Join(errParts, ", "))
	}
	if final {
		if len(outages) == 0 && open == nil {
			fmt.Printf("    reconnects    no outage seen (no drop of ≥%.0f%% of sockets)\n", outageShare*100)
		}
		for i, o := range outages {
			fmt.Printf("    outage %d      %d of %d dropped; first back +%.1fs, all %d back +%.1fs after the first disconnect\n",
				i+1, o.before-o.minUp, o.before, o.firstBack.Sub(o.firstDown).Seconds(), o.before, o.allBack.Sub(o.firstDown).Seconds())
		}
		if open != nil {
			first := "none back yet"
			if !open.firstBack.IsZero() {
				first = fmt.Sprintf("first back +%.1fs", open.firstBack.Sub(open.firstDown).Seconds())
			}
			fmt.Printf("    outage %d      %d of %d dropped; %s; %d of %d back when the run ended, +%.1fs after the first disconnect (unfinished: run longer)\n",
				len(outages)+1, open.before-open.minUp, open.before, first, s.connected.Load(), open.before, time.Since(open.firstDown).Seconds())
		}
	}
	if g.o.adminKey != "" {
		g.reportServer()
	}
}

// reportServer prints the server's own view from /debug/vars: sockets it holds, its
// pool, the ingest queue and the slowest background jobs.
func (g *gen) reportServer() {
	req, _ := http.NewRequest(http.MethodGet, g.base.String()+"/debug/vars", nil)
	req.Header.Set("X-API-Key", g.o.adminKey)
	resp, err := g.client.Do(req)
	if err != nil {
		fmt.Printf("    server        /debug/vars: %s\n", netErrKind(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("    server        /debug/vars: HTTP %d\n", resp.StatusCode)
		return
	}
	var v struct {
		WS     int                                    `json:"ws_connected_devices"`
		Gor    int                                    `json:"goroutines"`
		Pool   map[string]int64                       `json:"db_pool"`
		Ingest map[string]any                         `json:"ingest"`
		Mem    struct{ HeapAlloc, Sys, NumGC uint64 } `json:"memstats"`
		Jobs   map[string]struct {
			Runs     int64 `json:"runs"`
			LastMs   int64 `json:"last_ms"`
			MaxMs    int64 `json:"max_ms"`
			TotalMs  int64 `json:"total_ms"`
			Overruns int64 `json:"overruns"`
			TimedOut int64 `json:"timed_out"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		fmt.Printf("    server        /debug/vars: %v\n", err)
		return
	}
	fmt.Printf("    server        ws %d, goroutines %d, heap %d MB, gc %d | pool acquired %d/%d, empty-acquire %d | ingest depth %v, written %v, shed %v, slots %v/%v, max flush %vms\n",
		v.WS, v.Gor, v.Mem.HeapAlloc>>20, v.Mem.NumGC, v.Pool["acquired_conns"], v.Pool["max_conns"], v.Pool["empty_acquire_count"],
		v.Ingest["depth"], v.Ingest["written"], v.Ingest["shed"], v.Ingest["slots_in_use"], v.Ingest["slots"], v.Ingest["max_flush_ms"])
	type job struct {
		name                 string
		runs, last, max, avg int64
		over, tout           int64
	}
	var js []job
	for name, j := range v.Jobs {
		avg := int64(0)
		if j.Runs > 0 {
			avg = j.TotalMs / j.Runs
		}
		js = append(js, job{name, j.Runs, j.LastMs, j.MaxMs, avg, j.Overruns, j.TimedOut})
	}
	sort.Slice(js, func(a, b int) bool { return js[a].max > js[b].max })
	var parts []string
	for i, j := range js {
		if i == 6 {
			break
		}
		p := fmt.Sprintf("%s max %dms avg %dms ×%d", j.name, j.max, j.avg, j.runs)
		if j.over > 0 || j.tout > 0 {
			p += fmt.Sprintf(" (overruns %d, timeouts %d)", j.over, j.tout)
		}
		parts = append(parts, p)
	}
	fmt.Printf("    jobs (%d)      slowest: %s\n", len(js), strings.Join(parts, "; "))
}

// ── cleanup ──────────────────────────────────────────────────────────────────

// cleanupSQL is what removes the fakes. Deleting the device rows cascades to most of
// what they wrote (samples, state events, alerts, packages, configs, daily stats, app
// usage, command status/results, logcat, build history…); the statements before it
// handle what does not, checked against a freshly migrated schema (information_schema)
// on 29 Oct 2026:
//   - ON DELETE SET NULL: audit_log.device_id, release_problems.device_id — the rows
//     would survive pointing at nothing, so they go first.
//   - No foreign key at all: command_targets.target_id (it holds device OR group ids),
//     command_events.device_id, and the serial-keyed peer_outbox (arrival announcements
//     for peer MDMs), refused_checkins, hardware_serials, device_adb_key. The fakes send
//     no hw_serial and hold no adb key, so the last two are belt and braces.
//   - commands: a command targets many devices, so it is not owned by one. Commands whose
//     every device target is a fake are deleted with their targets and events;
//     a command that also reached real devices keeps everything but the fakes' rows.
//
// A table added later with a non-cascading device FK makes the final DELETE fail and the
// transaction roll back, which is the safe direction.
func cleanupSQL(prefix string) string {
	like := sqlLikePrefix(prefix)
	return fmt.Sprintf(`-- Remove the loadgen's fake devices (serials LIKE '%[1]s').
-- Review, then run against the server's database in one go.
BEGIN;
CREATE TEMP TABLE loadgen_fakes ON COMMIT DROP AS
  SELECT id, serial_number FROM devices WHERE serial_number LIKE '%[1]s' ESCAPE '\';
SELECT count(*) AS fake_devices FROM loadgen_fakes;

-- ON DELETE SET NULL references.
DELETE FROM audit_log        WHERE device_id IN (SELECT id FROM loadgen_fakes);
DELETE FROM release_problems WHERE device_id IN (SELECT id FROM loadgen_fakes);

-- Commands aimed at device targets that are all fakes. Their targets cascade; their
-- events have no foreign key, so they go by command id (some carry no device id).
CREATE TEMP TABLE loadgen_cmds ON COMMIT DROP AS
  SELECT c.id FROM commands c
   WHERE c.target_type = 'devices'
     AND EXISTS (SELECT 1 FROM command_targets t WHERE t.command_id = c.id)
     AND NOT EXISTS (SELECT 1 FROM command_targets t
                      WHERE t.command_id = c.id AND t.target_id NOT IN (SELECT id FROM loadgen_fakes));
DELETE FROM command_events WHERE command_id IN (SELECT id FROM loadgen_cmds);
DELETE FROM commands       WHERE id IN (SELECT id FROM loadgen_cmds);
-- No foreign key: what is left pointing at a fake.
DELETE FROM command_targets WHERE target_id IN (SELECT id FROM loadgen_fakes);
DELETE FROM command_events  WHERE device_id IN (SELECT id FROM loadgen_fakes);
DELETE FROM peer_outbox      WHERE serial        LIKE '%[1]s' ESCAPE '\';
DELETE FROM refused_checkins WHERE serial        LIKE '%[1]s' ESCAPE '\';
DELETE FROM hardware_serials WHERE serial_number LIKE '%[1]s' ESCAPE '\';
DELETE FROM device_adb_key   WHERE serial        LIKE '%[1]s' ESCAPE '\';

-- Everything else cascades.
DELETE FROM devices WHERE id IN (SELECT id FROM loadgen_fakes);
COMMIT;
`, like)
}

// sqlLikePrefix escapes LIKE's wildcards in the prefix, so LOADTEST_ means that literally.
func sqlLikePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `'`, `''`)
	return r.Replace(p) + "%"
}

// ── helpers ──────────────────────────────────────────────────────────────────

func validPrefix(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// netErrKind names a transport error by what happened, so the error table groups
// "connection refused" during a restart instead of listing every port number.
func netErrKind(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, websocket.ErrBadHandshake):
		return "bad handshake"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func stamp(start time.Time) string {
	return fmt.Sprintf("[%s]", time.Since(start).Truncate(time.Second))
}

func durOr(d time.Duration, zero string) string {
	if d == 0 {
		return zero
	}
	return d.String()
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "loadgen: "+format+"\n", a...)
	os.Exit(2)
}
