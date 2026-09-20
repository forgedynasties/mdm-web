package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"mdm/internal/alerts"
	"mdm/internal/apkmeta"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/logstream"
	"mdm/internal/middleware"
	"mdm/internal/otagate"
	"mdm/internal/ratelimit"
	"mdm/internal/remote"
	"mdm/internal/shell"
	"mdm/internal/totp"
	"mdm/internal/ws"
)

// maxPackagesPerDevice caps how many installed-app rows a single check-in will
// persist, so an oversized list can't blow up the per-device package upsert.
const maxPackagesPerDevice = 2000

// maxIconBytes caps a single base64 launcher-icon string. A 96px PNG is well under
// this; anything larger is dropped (the app row is still stored, just without an icon).
const maxIconBytes = 96 * 1024

// Device-input bounds. Every device shares one API key, so treat each request as
// hostile: cap identifier lengths, the telemetry blob size, JSON nesting depth (a
// deeply-nested body can overflow the goroutine stack — a fatal, unrecoverable crash
// net/http's per-request recover does NOT catch), and the request rate per device.
const (
	maxSerialLen  = 64
	maxBuildIDLen = 128
	// The client's crash_events trace budget alone is 256 KiB (MAX_TOTAL_TRACE_BYTES),
	// and the rest of extra (wifi/ram/storage/boot/summaries) stacks on top — a
	// crash-heavy full-GMS device then blew past a 256 KiB cap and every check-in 413'd.
	// Keep the server ceiling comfortably above the client's max legitimate extra.
	maxExtraBytes   = 512 * 1024
	maxJSONDepth    = 64
	deviceRateBurst = 120 // max device requests per serial per minute
)

type Handler struct {
	db          *db.DB
	hub         *ws.Hub
	shell       *shell.Manager
	cfg         *config.Config
	geolocate   *geolocate.Resolver
	geocoder    *geolocate.Geocoder
	remote      *remote.Manager
	adminAPIKey string
	alerts      *alerts.Dispatcher
	deviceRate  *ratelimit.Counter // per-serial request throttle on the device API
	legacy      *legacyOTA         // legacy otautil protocol on the second listener (legacy_ota.go)
	logs        *logstream.Manager // live logcat, used to follow legacy installs (legacy_watch.go)
	otaGate     *otagate.Gate      // which builds can take an MDM OTA (the rest go legacy)

	// One telemetry-request loop per device, so a reconnect replaces its loop instead
	// of adding one. Guarded by its own mutex: Connect runs on every WS upgrade.
	telemetryMu    sync.Mutex
	telemetryLoops map[uuid.UUID]*telemetryLoop
}

// telemetryLoop is a running loop's handle. Compared by pointer on exit so a loop
// only ever removes its own entry — a superseded loop must not delete the map entry
// belonging to the connection that replaced it.
type telemetryLoop struct {
	cancel context.CancelFunc
}

func NewHandler(d *db.DB, hub *ws.Hub, shellMgr *shell.Manager, cfg *config.Config, geo *geolocate.Resolver, geocoder *geolocate.Geocoder, rm *remote.Manager, logMgr *logstream.Manager, adminAPIKey string) *Handler {
	return &Handler{db: d, hub: hub, shell: shellMgr, cfg: cfg, geolocate: geo, geocoder: geocoder, remote: rm, logs: logMgr, adminAPIKey: adminAPIKey, alerts: alerts.NewDispatcher(d, cfg), deviceRate: ratelimit.New(time.Minute), otaGate: otagate.New(d, cfg), telemetryLoops: map[uuid.UUID]*telemetryLoop{}}
}

// connectedSlice returns the live WebSocket-connected device IDs as a slice, so DB
// queries derive online/offline from real presence rather than check-in recency.
func (h *Handler) connectedSlice() []uuid.UUID {
	set := h.hub.ConnectedIDs()
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// decodeDeviceJSON reads a device request body (already MaxBytes-capped by the route
// wrapper) and decodes it into v, first rejecting pathologically nested JSON. The
// depth pre-scan uses encoding/json's iterative tokenizer (no recursion), so it can't
// itself be overflowed, and it runs before the recursive Unmarshal that could be.
func decodeDeviceJSON(body io.Reader, v any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break // EOF or malformed — let Unmarshal below surface the real error
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '[', '{':
				if depth++; depth > maxJSONDepth {
					return fmt.Errorf("json nesting too deep")
				}
			case ']', '}':
				depth--
			}
		}
	}
	return json.Unmarshal(data, v)
}

// deviceRateLimited records one request for serial and, if the per-minute burst is
// exceeded, writes a 429 and returns true. Keyed by serial (the device identity)
// rather than IP, since many devices share a restaurant/lab NAT.
func (h *Handler) deviceRateLimited(w http.ResponseWriter, serial string) bool {
	n, retry := h.deviceRate.Hit(serial)
	if n > deviceRateBurst {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return true
	}
	return false
}

// ── WebSocket ─────────────────────────────────────────────────────────────────

// Connect upgrades the connection to WebSocket for a device identified by
// ?serial=<serial_number>. On connect, any pending commands and logcat requests
// are flushed immediately. Going forward, commands and logcat requests are
// pushed as they are created.
func (h *Handler) Connect(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" {
		http.Error(w, "serial query parameter required", http.StatusBadRequest)
		return
	}
	if bound := middleware.BoundSerial(r); bound != "" && bound != serial {
		http.Error(w, "serial does not match device credential", http.StatusForbidden)
		return
	}

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	client, err := h.hub.Upgrade(w, r, device.ID)
	if err != nil {
		log.Printf("[ws] upgrade error for %s: %v", serial, err)
		return
	}

	// The device just (re)connected — proof that any reboot command it had received
	// actually rebooted it. Complete those before flushing, so a reboot delivered on the
	// previous session flips 'delivered' → 'completed' now instead of being falsely
	// completed at send time (FW-2026-000033).
	if err := h.db.CompleteDeliveredReboots(r.Context(), device.ID); err != nil {
		log.Printf("[ws] CompleteDeliveredReboots error for %s: %v", serial, err)
	}

	// Flush any commands that were queued while the device was offline.
	h.flushPendingCommands(r.Context(), device.ID)
	h.flushPendingLogcatRequests(r.Context(), device.ID)

	// Request telemetry immediately, then on a repeating interval.
	reqMsg, _ := json.Marshal(map[string]any{"type": "telemetry_request"})
	h.hub.Push(device.ID, reqMsg)
	h.startTelemetryRequestLoop(device.ID)

	go client.WritePump()
	client.ReadPump() // blocks until connection closes

	// The socket just closed: stamp last_seen with the moment the device stopped
	// being live over WS, so the fleet's "Last seen" tracks real WS liveness instead
	// of the last change-gated check-in (which can lag 5-8 min on an idle-but-online
	// device). r.Context() is already cancelled here, so use a fresh context.
	if err := h.db.TouchLastSeen(context.Background(), device.ID, time.Now()); err != nil {
		log.Printf("[ws] touch last_seen failed for %s: %v", serial, err)
	}
}

// startTelemetryRequestLoop starts this device's telemetry loop, replacing any loop
// left over from an earlier connection.
//
// Without the replace, a device that reconnects inside one check-in interval ends up
// with two loops, then three: the old loop is asleep, and when it wakes hub.Push finds
// the NEW client (the hub keys clients by device id, and register() refills the slot on
// reconnect), so the push succeeds and the old loop never exits. Every extra loop asks
// the device for telemetry on its own timer, which is answered, stored, and billed to
// the devices with the worst connectivity.
func (h *Handler) startTelemetryRequestLoop(deviceID uuid.UUID) {
	ctx, cancel := context.WithCancel(context.Background())
	loop := &telemetryLoop{cancel: cancel}

	h.telemetryMu.Lock()
	if prev, ok := h.telemetryLoops[deviceID]; ok {
		prev.cancel() // the connection it belonged to is gone or superseded
	}
	h.telemetryLoops[deviceID] = loop
	h.telemetryMu.Unlock()

	go h.runTelemetryRequestLoop(ctx, deviceID, loop)
}

// runTelemetryRequestLoop sends a telemetry_request to the device on the configured
// check-in interval. Exits when the device disconnects (Push returns false) or when a
// newer connection supersedes it (ctx cancelled). Re-reads the interval each cycle so
// config changes take effect.
func (h *Handler) runTelemetryRequestLoop(ctx context.Context, deviceID uuid.UUID, loop *telemetryLoop) {
	// Drop our own entry on the way out, and only ours: by the time a superseded loop
	// notices, the map already points at its replacement, and deleting that would leave
	// the live loop unreachable — a leak of exactly the kind this function fixes.
	defer func() {
		loop.cancel() // release the context's resources on every exit path
		h.telemetryMu.Lock()
		if h.telemetryLoops[deviceID] == loop {
			delete(h.telemetryLoops, deviceID)
		}
		h.telemetryMu.Unlock()
	}()

	msg, _ := json.Marshal(map[string]any{"type": "telemetry_request"})
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		interval := time.Duration(h.cfg.CheckinInterval()) * time.Second
		if interval < 10*time.Second {
			interval = 30 * time.Second
		}
		// A timer rather than time.Sleep: a superseded loop stops at once instead of
		// pushing one more request when it eventually wakes.
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !h.hub.Push(deviceID, msg) {
			log.Printf("[ws] telemetry loop exiting for device %s — send channel full or disconnected", deviceID)
			return
		}
	}
}


// PingDevice sends a ping_request to the device over WS and waits up to 5s for
// a pong_response. Reports whether the device is truly responsive or just appears connected.
func (h *Handler) PingDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	if !h.hub.IsConnected(device.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false, "responsive": false})
		return
	}
	nonce := uuid.New().String()
	ch := h.hub.RegisterPingWaiter(nonce)
	defer h.hub.UnregisterPingWaiter(nonce)
	start := time.Now()
	msg, _ := json.Marshal(map[string]any{"type": "ping_request", "nonce": nonce})
	h.hub.Push(device.ID, msg)
	select {
	case <-ch:
		writeJSON(w, http.StatusOK, map[string]any{
			"connected":  true,
			"responsive": true,
			"latency_ms": time.Since(start).Milliseconds(),
		})
	case <-time.After(5 * time.Second):
		writeJSON(w, http.StatusOK, map[string]any{
			"connected":  true,
			"responsive": false,
			"error":      "timeout — device connected but not responding",
		})
	}
}

// ConnectRemote upgrades the dashboard HTTP connection to a WebSocket for
// remote control of the specified device. It starts a capture session, relays
// binary frames from the device to the dashboard, and relays input events back.
func (h *Handler) ConnectRemote(w http.ResponseWriter, r *http.Request) {
	log.Printf("[remote] ConnectRemote called from %s", r.RemoteAddr)
	// Auth is a single-use token minted by the (session-authenticated) dashboard,
	// not the admin API key — the key must never reach the browser / WS URL. It is
	// bound to the minting client's IP, so a leaked token can't be replayed elsewhere.
	tokenDeviceID, ok := h.remote.RedeemToken(r.URL.Query().Get("token"))
	if !ok {
		log.Printf("[remote] auth failed for %s", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	serial := strings.TrimSpace(r.PathValue("serial"))
	if serial == "" {
		http.Error(w, "device serial missing from URL path", http.StatusBadRequest)
		return
	}

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if device.ID != tokenDeviceID {
		// Token was issued for a different device than the URL names.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !h.hub.IsConnected(device.ID) {
		http.Error(w, "device not connected", http.StatusServiceUnavailable)
		return
	}

	_, err = h.remote.Start(device.ID)
	if err == remote.ErrSessionActive {
		// A prior operator's session leaked — the browser died without a WS close
		// frame (laptop sleep, network drop, tab killed), so its read pump never
		// unblocked to run the deferred Stop. Single-operator model: evict the
		// stale session and take over rather than 409-locking the device until a
		// server restart.
		log.Printf("[remote] evicting stale session for device %s (takeover)", device.ID)
		h.remote.Stop(device.ID)
		_, err = h.remote.Start(device.ID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// Capture params are driven by the operator's query string so the dashboard can pick
	// the codec and tune the stream. Defaults keep the safe still-image (JPEG) path; the
	// browser opts into hardware H.264 with ?codec=h264 (and must decode it via WebCodecs).
	q := r.URL.Query()
	// H.264 is the default; the device falls back to JPEG stills on its own if its
	// encoder can't do it, and the browser auto-detects the codec per frame. Only
	// an explicit ?codec=jpeg forces the still path.
	codec := q.Get("codec")
	if codec != "jpeg" {
		codec = "h264"
	}
	quality := clampInt(atoiOr(q.Get("quality"), 60), 1, 100)
	scale := clampFloat(atofOr(q.Get("scale"), 0.5), 0.1, 1.0)
	maxFps := clampInt(atoiOr(q.Get("max_fps"), 15), 1, 30)
	// Default 2 Mbps, not 4 — on a jittery device uplink (2.4 GHz Wi-Fi) 4 Mbps
	// overruns the sender queue, silently dropping delta frames and stalling the
	// browser decoder until the next key frame. 2 Mbps is plenty for a scaled
	// phone screen and keeps the stream realtime; bump via ?bitrate= if the link
	// is good.
	bitrate := clampInt(atoiOr(q.Get("bitrate"), 2_000_000), 250_000, 20_000_000)
	startMsg, _ := json.Marshal(map[string]any{
		"type":    "start_capture",
		"codec":   codec,
		"quality": quality,
		"scale":   scale,
		"max_fps": maxFps,
		"bitrate": bitrate,
	})
	h.hub.Push(device.ID, startMsg)

	upgrader := ws.Upgrader()
	dashConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.remote.Stop(device.ID)
		return
	}
	defer dashConn.Close()
	defer h.remote.Stop(device.ID)

	frameCh, _ := h.remote.SubscribeFrames(device.ID)

	// Keepalive: a silently-dead browser (sleep, network drop, killed tab) sends
	// no WS close frame, so without this the read pump below blocks forever, the
	// deferred Stop never runs, and the session leaks — 409-locking the device.
	// Pings from the write pump + a pong-extended read deadline detect the dead
	// peer and unblock the read so teardown happens.
	const pongWait = 60 * time.Second
	const pingPeriod = 25 * time.Second
	dashConn.SetReadDeadline(time.Now().Add(pongWait))
	dashConn.SetPongHandler(func(string) error {
		dashConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Write pump: relay device frames → dashboard (binary) + keepalive pings.
	// Single writer goroutine — gorilla/websocket forbids concurrent writes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ping := time.NewTicker(pingPeriod)
		defer ping.Stop()
		// Latency instrumentation: every 5s log frames received from the device
		// vs dropped at the relay vs actually written to this browser, plus the
		// relay-buffer occupancy. Diffing recv/drop against the prior tick shows
		// per-hop rates so we can see which hop holds latency.
		stats := time.NewTicker(5 * time.Second)
		defer stats.Stop()
		var wrote int64
		var lastRecv, lastDrop, lastWrote int64
		for {
			select {
			case data, ok := <-frameCh:
				if !ok {
					return
				}
				dashConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := dashConn.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
				wrote++
			case <-ping.C:
				dashConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := dashConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-stats.C:
				recv, drop, qlen, qcap, ok := h.remote.Stats(device.ID)
				if !ok {
					continue
				}
				log.Printf("[remote-stats] dev=%s recv=%d/5s drop=%d/5s wrote=%d/5s q=%d/%d",
					device.ID, recv-lastRecv, drop-lastDrop, wrote-lastWrote, qlen, qcap)
				lastRecv, lastDrop, lastWrote = recv, drop, wrote
			}
		}
	}()

	// Read pump: dashboard input events → device (JSON). A dead browser stops
	// sending pongs; the read deadline then fires here and breaks the loop so the
	// deferred Stop tears the session down instead of leaking it.
	for {
		_, msg, err := dashConn.ReadMessage()
		if err != nil {
			break
		}
		h.remote.RelayInput(device.ID, msg)
	}

	dashConn.Close() // unblock the write pump if it's parked on a write
	<-done
}

// atoiOr parses s as an int, returning def if s is empty or invalid.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// atofOr parses s as a float64, returning def if s is empty or invalid.
func atofOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// flushPendingCommands pushes pending commands to the device over WS and
// marks them delivered (or completed for reboot). Stops after a reboot
// command so that later commands remain pending and are flushed after the
// device reconnects post-reboot.
// FlushPendingCommands is the exported entry point for flushing a device's queued
// commands — wired to the hub's onConnect hook so delivery starts the instant the WS
// is up (see SetOnConnect).
func (h *Handler) FlushPendingCommands(ctx context.Context, deviceID uuid.UUID) {
	h.flushPendingCommands(ctx, deviceID)
}

func (h *Handler) flushPendingCommands(ctx context.Context, deviceID uuid.UUID) {
	cmds, err := h.db.GetPendingCommandsForDevice(ctx, deviceID)
	if err != nil {
		log.Printf("[ws] GetPendingCommandsForDevice error: %v", err)
		return
	}
	for _, cmd := range cmds {
		msg := marshalCommand(cmd.ID, cmd.Type, cmd.ApkURL, cmd.Payload)
		if !h.hub.Push(deviceID, msg) {
			continue
		}
		// Reboot is only 'delivered', not 'completed' — it completes when the device
		// reconnects (CompleteDeliveredReboots), which is the actual proof it rebooted
		// (FW-2026-000033).
		_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
		if cmd.Type == "reboot" {
			break // stop here; remaining cmds flush after the device reboots + reconnects
		}
	}
}

// flushPendingLogcatRequests pushes pending logcat requests to the device over WS.
func (h *Handler) flushPendingLogcatRequests(ctx context.Context, deviceID uuid.UUID) {
	reqs, err := h.db.GetPendingLogcatRequestsForDevice(ctx, deviceID)
	if err != nil {
		log.Printf("[ws] GetPendingLogcatRequestsForDevice error: %v", err)
		return
	}
	var delivered []uuid.UUID
	for _, req := range reqs {
		msg := marshalLogcatRequest(req.ID, req.Level, req.Lines, req.Tag)
		if h.hub.Push(deviceID, msg) {
			delivered = append(delivered, req.ID)
		}
	}
	if len(delivered) > 0 {
		_ = h.db.MarkLogcatRequestsDelivered(ctx, delivered)
	}
}

// pushCommand pushes a newly created command to all targeted online devices
// and marks delivery status.
func (h *Handler) pushCommand(ctx context.Context, cmd *db.Command, targetType string, targetIDs []uuid.UUID) {
	msg := marshalCommand(cmd.ID, cmd.Type, cmd.ApkURL, cmd.Payload)

	switch targetType {
	case "all":
		h.hub.Broadcast(msg)
		// Cannot track per-device delivery for "all"; devices get it on next connect if offline.
		return
	case "groups":
		ids, err := h.db.GetDeviceIDsByGroupIDs(ctx, targetIDs)
		if err != nil {
			log.Printf("[ws] GetDeviceIDsByGroupIDs error: %v", err)
			return
		}
		targetIDs = ids
	}

	for _, deviceID := range targetIDs {
		if !h.hub.Push(deviceID, msg) {
			continue
		}
		// Reboot is marked only 'delivered' here, never 'completed': a successful push
		// means the command was queued to the socket, not that the device rebooted. A
		// stale/half-open connection would otherwise false-complete an offline device
		// (FW-2026-000033). It flips to 'completed' when the device reconnects
		// (CompleteDeliveredReboots on the WS connect / next check-in).
		_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
	}
	// Notify the dashboard's command detail page of the new delivery/ack state.
	h.hub.PublishCommandUpdate(cmd.ID)
}

// enrichLocation resolves WiFi scan data to geographic coordinates and merges
// the result into the extra JSONB payload. If the geolocate resolver is nil or
// no wifi_scan data is present, returns unchanged extra.
func (h *Handler) enrichLocation(ctx context.Context, extra json.RawMessage) json.RawMessage {
	if h.geolocate == nil || len(extra) == 0 {
		return extra
	}
	aps := geolocate.ExtractWifiScan(extra)
	if len(aps) == 0 {
		return extra
	}
	lat, lon, accuracy, err := h.geolocate.Resolve(ctx, aps)
	if err != nil {
		if err != geolocate.ErrCooldown {
			log.Printf("[geolocate] resolve error: %v", err)
		}
		return extra
	}
	// Merge lat/lon/accuracy into the extra JSON object
	var m map[string]any
	if err := json.Unmarshal(extra, &m); err != nil {
		m = make(map[string]any)
	}
	m["latitude"] = lat
	m["longitude"] = lon
	m["location_accuracy"] = accuracy
	// Reverse-geocode to a street address (Google Geocoding API, separate key).
	// Best-effort and heavily cached — a failure just leaves the address absent,
	// and the page falls back to showing the raw coordinates.
	if h.geocoder != nil {
		if addr, gerr := h.geocoder.Reverse(ctx, lat, lon); gerr != nil {
			log.Printf("[geocode] reverse error: %v", gerr)
		} else if addr != "" {
			m["location_address"] = addr
		}
	}
	enriched, err := json.Marshal(m)
	if err != nil {
		return extra
	}
	return json.RawMessage(enriched)
}

// ── Enrollment ────────────────────────────────────────────────────────────────

// enrollFailures throttles bad-token attempts per source IP (the endpoint is
// unauthenticated by design — the profile token is the credential).
var (
	enrollFailures      = ratelimit.New(time.Minute)
	enrollMaxFailPerMin = 10
)

// Enroll exchanges an enrollment-profile token for a per-device API key. The device
// stores the key and uses it as its X-API-Key from then on; the server derives the
// acting device from the key (see middleware.DeviceAuth), closing the shared-key
// identity gap. Re-enrolling the same serial rotates its key.
func (h *Handler) Enroll(w http.ResponseWriter, r *http.Request) {
	ip := ratelimit.ClientIP(r)
	if n, retry := enrollFailures.Count(ip); n >= enrollMaxFailPerMin {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	var req struct {
		Token   string `json:"token"`
		Serial  string `json:"serial"`
		Product string `json:"product"`
	}
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.Serial = strings.TrimSpace(req.Serial)
	if req.Token == "" || req.Serial == "" || len(req.Serial) > maxSerialLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token and serial are required"})
		return
	}
	profile, err := h.db.ActiveEnrollmentProfileByToken(r.Context(), req.Token)
	if err != nil {
		enrollFailures.Hit(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid enrollment token"})
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	deviceKey := "dvk_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(deviceKey))
	res, err := h.db.EnrollDevice(r.Context(), profile, req.Serial, req.Product, hex.EncodeToString(sum[:]))
	if err != nil {
		log.Printf("[enroll] %s via %q: %v", req.Serial, profile.Name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	verb := "enrolled"
	if res.ReEnrolled {
		verb = "re-enrolled (key rotated)"
	}
	log.Printf("[enroll] device %s %s via profile %q", req.Serial, verb, profile.Name)
	h.hub.PublishDeviceUpdate(res.DeviceID)
	// Beyond the key, tell the agent where the device landed so its status screen can
	// show the operator "Enrolled · mPOS · Site X" (empty strings when the profile did
	// not set that field). Older agents read device_key and ignore the rest.
	writeJSON(w, http.StatusOK, map[string]any{
		"device_key":   deviceKey,
		"device_id":    res.DeviceID.String(),
		"profile":      profile.Name,
		"device_class": profile.DeviceClass,
		"site":         profile.RestaurantName,
		"group":        profile.GroupName,
		"re_enrolled":  res.ReEnrolled,
		"onboarded":    res.Onboarded,
	})
}

// requireBoundSerial rejects a request whose per-device credential doesn't match the
// serial it claims to act for. Legacy shared-key requests (no bound serial) pass —
// their identity stays client-supplied until the fleet is migrated to enrollment keys.
func requireBoundSerial(w http.ResponseWriter, r *http.Request, serial string) bool {
	if bound := middleware.BoundSerial(r); bound != "" && bound != serial {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "serial does not match device credential"})
		return false
	}
	return true
}

// ── Checkin (telemetry only) ──────────────────────────────────────────────────

type checkinRequest struct {
	SerialNumber string `json:"serial_number"`
	BuildID      string `json:"build_id"`
	// Product is the hardware category the device reports (e.g. "t7", "kiosk27"). Sent
	// on the full HTTP keyframe; delta/WS frames may omit it and the stored value sticks.
	Product string `json:"product,omitempty"`
	// Pointer so a delta telemetry frame that omits an unchanged battery_pct is
	// distinguishable from a real 0 — the server keeps the prior value in that case.
	BatteryPct    *int            `json:"battery_pct"`
	Extra         json.RawMessage `json:"extra,omitempty"`
	InstalledApps []struct {
		Package     string `json:"package"`
		Name        string `json:"name"`
		VersionName string `json:"version_name"`
		IsSystem    *bool  `json:"is_system"`  // nil when the client is too old to report it
		Launchable  *bool  `json:"launchable"` // nil when the client is too old to report it
		Icon        string `json:"icon"`       // base64 PNG launcher icon (optional)
	} `json:"installed_apps,omitempty"`
	// OTA progress piggybacked on the checkin so the dashboard keeps tracking
	// download/install percent even when the WebSocket is down.
	OtaProgress *struct {
		CommandID uuid.UUID `json:"command_id"`
		Phase     string    `json:"phase"`
		Percent   int       `json:"percent"`
	} `json:"ota_progress,omitempty"`
}

// isDPCPayload reports whether a check-in or telemetry frame came from a stock-device
// agent: the DPC agent (agent_type "dpc") or the MDM-lite library in an app ("mdm-lite").
// Every frame either sends carries extra.agent_type, so this
// answers from the payload without a database lookup — which is the point, since
// the caller uses it to skip all storage work.
func isDPCPayload(extra json.RawMessage) bool {
	if len(extra) == 0 {
		return false
	}
	var ident struct {
		AgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal(extra, &ident); err != nil {
		return false
	}
	return ident.AgentType == "dpc" || ident.AgentType == "mdm-lite"
}

// isMDMLitePayload reports whether a check-in came from the MDM-lite library.
// mdmLiteForeground reports whether an MDM-lite check-in came from the host app in the
// foreground (its 30 s loop) rather than the 15-minute background job. A payload
// without the field counts as foreground (older libraries only checked in from it).
func mdmLiteForeground(extra json.RawMessage) bool {
	var probe struct {
		AppForeground *bool `json:"app_foreground"`
	}
	if json.Unmarshal(extra, &probe) != nil || probe.AppForeground == nil {
		return true
	}
	return *probe.AppForeground
}

func isMDMLitePayload(extra json.RawMessage) bool {
	var ident struct {
		AgentType string `json:"agent_type"`
	}
	return len(extra) > 0 && json.Unmarshal(extra, &ident) == nil && ident.AgentType == "mdm-lite"
}

// recordCheckinOtaProgress stores OTA progress reported in a checkin payload.
func (h *Handler) recordCheckinOtaProgress(deviceID uuid.UUID, req *checkinRequest) {
	if req.OtaProgress == nil || req.OtaProgress.CommandID == uuid.Nil {
		return
	}
	h.shell.SetOTAProgress(deviceID, req.OtaProgress.CommandID, req.OtaProgress.Phase, req.OtaProgress.Percent)
	h.hub.PublishDeploymentUpdate() // live-refresh open deployment pages
}

// ingestSource is the transport a check-in arrived on. The two are not only plumbing:
// an HTTP check-in is the periodic full keyframe and replaces the stored snapshot,
// while a WebSocket frame is a delta merged into it.
type ingestSource int

const (
	sourceHTTP ingestSource = iota
	sourceWS
)

func (s ingestSource) tag() string {
	if s == sourceWS {
		return "ws-telemetry"
	}
	return "checkin"
}

// ingestResult is what the caller needs to answer the device.
type ingestResult struct {
	DeviceID uuid.UUID
	Config   map[string]any
	Commands []map[string]any // legacy poll list; only ever filled for HTTP
	SendApps bool             // ask the device for a full app list
}

// ingestCheckin stores one check-in and works out what to send back. Both transports
// run this; they differ only in how the payload arrives and how the answer is written.
//
// It used to be two hand-maintained copies of the same two hundred lines, and they had
// drifted: the WebSocket copy skipped the OTA capability gate, so a device whose build
// cannot apply an MDM OTA could still be handed the command, and it never asked a
// device for its app list however stale the inventory was.
func (h *Handler) ingestCheckin(ctx context.Context, req *checkinRequest, src ingestSource) (*ingestResult, error) {
	tag := src.tag()
	req.Extra = h.enrichLocation(ctx, req.Extra)

	// HTTP is the periodic full keyframe → replace latest_extra (clears stale keys).
	// A WS frame is a delta → merge it. Product is usually empty on deltas, and
	// UpsertCheckin keeps the previously learned value then.
	merge := src == sourceWS
	deviceID, _, isNew, err := h.db.UpsertCheckin(ctx, req.SerialNumber, req.BuildID, req.BatteryPct, req.Extra, merge, req.Product)
	if err != nil {
		return nil, err
	}
	h.db.IngestDeviceEvents(ctx, deviceID, req.BuildID, req.Extra)
	if isMDMLitePayload(req.Extra) {
		// MDM-lite has no live connection; its check-ins are its presence.
		h.hub.MarkCheckinPresence(deviceID, mdmLiteForeground(req.Extra))
	}
	if isNew {
		// A device must already exist to open its WS, so over that transport this is
		// rare — but onboarding must not depend on which one it used.
		h.notifyDeviceOnboarded(ctx, deviceID, req.SerialNumber)
	}

	if len(req.InstalledApps) > 0 {
		seen := make(map[string]struct{})
		var pkgs []db.DevicePackage
		for _, p := range req.InstalledApps {
			if _, dup := seen[p.Package]; dup {
				continue
			}
			seen[p.Package] = struct{}{}
			icon := p.Icon
			if len(icon) > maxIconBytes { // drop an oversized icon; keep the app row
				icon = ""
			}
			pkgs = append(pkgs, db.DevicePackage{PackageName: p.Package, AppName: p.Name, VersionName: p.VersionName, IsSystem: p.IsSystem, Launchable: p.Launchable, Icon: icon})
			if len(pkgs) >= maxPackagesPerDevice { // guard against an oversized list
				break
			}
		}
		if err := h.db.UpsertDevicePackages(ctx, deviceID, pkgs); err != nil {
			log.Printf("[%s] UpsertDevicePackages error: %v", tag, err)
		}
		// Clear any stuck "installing" whose app the device now reports present (e.g.
		// a terminal ack lost on a half-open socket): mark the install command
		// installed and refresh the UI. Without it a genuinely-installed app can sit
		// showing "failed" forever.
		if ids, err := h.db.ReconcileInstalledCommands(ctx, deviceID); err != nil {
			log.Printf("[%s] ReconcileInstalledCommands error: %v", tag, err)
		} else {
			for _, id := range ids {
				h.hub.PublishCommandUpdate(id)
			}
		}
	}

	h.recordCheckinOtaProgress(deviceID, req)

	// Reconcile completion first, independently of the resolver: if the device is now
	// running a deployment's target build, mark it installed. For an incremental-only
	// release ResolveUpdateForDevice returns nil once the device leaves the source
	// build, so without this the row would stay stuck at reboot_sent and the OTA could
	// be re-sent. Doing it before the resolver also excludes an installed row below,
	// preventing that re-send.
	if doneIDs, err := h.db.CompleteUpdatesAtTargetBuild(ctx, deviceID, req.BuildID); err != nil {
		log.Printf("[%s] CompleteUpdatesAtTargetBuild error: %v", tag, err)
	} else {
		for _, uid := range doneIDs {
			_ = h.db.CheckAndCompleteUpdate(ctx, uid)
		}
	}

	if src == sourceHTTP {
		// A device that also updates over the legacy path may just have booted into the
		// build that rollout offered — settle it here rather than waiting up to fifteen
		// minutes for its next otautil poll. Legacy devices only ever speak HTTP.
		h.SettleLegacyAtBuild(ctx, req.SerialNumber, req.BuildID)
	}

	// OTA check: resolve the update from the update_devices table.
	if upd, err := h.db.ResolveUpdateForDevice(ctx, deviceID); err != nil {
		log.Printf("[%s] ResolveUpdateForDevice error: %v", tag, err)
	} else if upd != nil && upd.OtaPackage != nil {
		pkg := upd.OtaPackage
		switch {
		case pkg.TargetBuildID == req.BuildID:
			// Already on the target build.
			_ = h.db.SetUpdateDeviceStatus(ctx, upd.ID, deviceID, "installed")
			_ = h.db.CheckAndCompleteUpdate(ctx, upd.ID)
		case upd.DeviceStatus == "reboot_sent" && bootedAfter(req.Extra, upd.DeviceRebootSentAt):
			h.failSlotSwitch(ctx, upd, deviceID, req.SerialNumber, req.BuildID)
		case upd.DeviceStatus == "awaiting_reboot" || upd.DeviceStatus == "reboot_sent":
			// Installed to the inactive slot, reboot pending (manual/scheduled) —
			// don't re-issue the OTA command.
		default:
			// The last gate before the wire: this build's client cannot apply an MDM
			// OTA, so never hand it the command however the row got created. The device
			// updates over the legacy otautil path instead. This used to be checked on
			// the HTTP path only, which let the same device get the command simply by
			// having a WebSocket open.
			if v := h.otaVerdict(ctx, req.BuildID, req.Product, req.Extra); !v.OK {
				log.Printf("[%s] %s on %s: %s — not sending the OTA command", tag, req.SerialNumber, req.BuildID, v.Describe())
			} else if cmd, err := h.db.TryCreateOTACommand(ctx, upd, deviceID, req.BuildID); err != nil {
				log.Printf("[%s] create OTA command error: %v", tag, err)
			} else if cmd != nil {
				h.pushCommand(ctx, cmd, "devices", []uuid.UUID{deviceID})
			}
		}
	}

	deviceCfg, err := h.db.GetOrCreateDeviceConfig(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	out := &ingestResult{DeviceID: deviceID}

	if src == sourceHTTP {
		log.Printf("[checkin] %s → kiosk_enabled=%v kiosk_package=%q kiosk_features=%d",
			req.SerialNumber, deviceCfg.KioskEnabled, deviceCfg.KioskPackage, deviceCfg.KioskFeatures)

		// A check-in while the device has no live WS is proof it is back after a reboot,
		// so complete any reboot that was only 'delivered' (FW-2026-000033). Guarded on
		// the WS being down: a still-connected device hasn't rebooted, so it must not be
		// completed off a routine keyframe.
		if !h.hub.IsConnected(deviceID) {
			_ = h.db.CompleteDeliveredReboots(ctx, deviceID)
		}

		// Pending commands ride the response for clients that poll instead of holding a
		// WebSocket: older firmware (when legacy check-in is on) and MDM-lite, which
		// never holds one.
		if (h.cfg.LegacyCheckin() || isMDMLitePayload(req.Extra)) && !h.hub.IsConnected(deviceID) {
			if cmds, err := h.db.GetPendingCommandsForDevice(ctx, deviceID); err == nil {
				for _, cmd := range cmds {
					out.Commands = append(out.Commands, map[string]any{
						"id":      cmd.ID,
						"type":    deviceCommandType(cmd.Type),
						"apk_url": cmd.ApkURL,
						"payload": cmd.Payload,
					})
					_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
					if cmd.Type == "reboot" {
						break // reboot only 'delivered'; completes on the next check-in
					}
				}
			}
		}

		// Ask for a full app list when ours is unknown or stale (a build change clears
		// it), or a re-flashed device keeps showing its old apps until its own app set
		// happens to change.
		out.SendApps = h.db.NeedsAppInventory(ctx, deviceID)
	}

	cfg := map[string]any{
		"kiosk_enabled":            deviceCfg.KioskEnabled,
		"kiosk_package":            deviceCfg.KioskPackage,
		"kiosk_features":           deviceCfg.KioskFeatures,
		"kiosk_mode":               deviceCfg.KioskMode,
		"kiosk_packages":           deviceCfg.KioskPackages,
		"kiosk_url":                deviceCfg.KioskURL,
		"kiosk_url_allow":          deviceCfg.KioskURLAllow,
		"wlc_charging_enabled":     deviceCfg.WlcChargingEnabled,
		"checkin_interval_seconds": h.cfg.CheckinInterval(),
	}
	// Fleet-wide policy (update_policy, location_enabled, network, app_restrictions…)
	// rides every config delivery; per-device keys above win on collision.
	for k, v := range h.cfg.DevicePolicy() {
		if _, taken := cfg[k]; !taken {
			cfg[k] = v
		}
	}
	addOfflineExit(cfg, deviceCfg)
	h.processOfflineExit(ctx, deviceID, req.SerialNumber, req.Extra, deviceCfg, cfg)

	// Publish AFTER processOfflineExit so this check-in's own (immediate) broadcast
	// already carries the flipped kiosk state — the dashboard updates as promptly as
	// battery/charging do, instead of waiting for a second trailing-throttle broadcast.
	h.hub.PublishDeviceUpdate(deviceID)

	out.Config = cfg
	return out, nil
}

func (h *Handler) Checkin(w http.ResponseWriter, r *http.Request) {
	var req checkinRequest
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// Trim the device-reported build id so trailing/leading whitespace can't make it
	// mismatch a stored target_build_id (which would miss "already on target" and
	// re-send an OTA to an up-to-date device, or block deployment completion).
	req.SerialNumber = strings.TrimSpace(req.SerialNumber)
	req.BuildID = strings.TrimSpace(req.BuildID)
	if req.SerialNumber == "" || req.BuildID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and build_id are required"})
		return
	}
	if len(req.SerialNumber) > maxSerialLen || len(req.BuildID) > maxBuildIDLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number or build_id too long"})
		return
	}
	if len(req.Extra) > maxExtraBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "extra telemetry too large"})
		return
	}
	if h.deviceRateLimited(w, req.SerialNumber) {
		return
	}
	if !requireBoundSerial(w, r, req.SerialNumber) {
		return
	}
	if req.BatteryPct != nil && (*req.BatteryPct < 0 || *req.BatteryPct > 100) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "battery_pct must be 0-100"})
		return
	}

	// DPC support can be switched off in Settings while the agent's write volume is
	// being worked on. Reply as usual so the agent keeps its normal interval instead
	// of retrying harder, but store nothing at all.
	if h.cfg.IgnoreDPCCheckins() && isDPCPayload(req.Extra) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "config": map[string]any{
			"checkin_interval_seconds": h.cfg.CheckinInterval(),
		}})
		return
	}

	res, err := h.ingestCheckin(r.Context(), &req, sourceHTTP)
	if err != nil {
		log.Printf("[checkin] ingest error for %s: %v", req.SerialNumber, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := map[string]any{
		"status":   "ok",
		"commands": res.Commands,
		"config":   res.Config,
	}
	if res.SendApps {
		resp["send_apps"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// addOfflineExit hands the device its unlock seed. Offline exit isn't a separate feature —
// it's part of kiosk — so the seed always rides the config; the code is only ever usable
// while the device is in kiosk (the on-device trigger is long-press Back in lock-task).
func addOfflineExit(cfgMap map[string]any, cfg *db.DeviceConfig) {
	cfgMap["offline_exit"] = map[string]any{
		"seed":   cfg.OfflineExitSeed,
		"digits": totp.DefaultDigits,
		"period": totp.DefaultPeriod,
	}
}

// processOfflineExit handles a device-reported offline kiosk exit: it flips the kiosk
// toggle OFF (the exit's effect on the server), reflects that in this same config response
// so the device converges to kiosk-off without re-locking, records the event, and acks so
// the client stops resending and clears its local "exited" guard.
func (h *Handler) processOfflineExit(ctx context.Context, deviceID uuid.UUID, serial string, extra json.RawMessage, cfg *db.DeviceConfig, cfgMap map[string]any) {
	if len(extra) == 0 {
		return
	}
	var e struct {
		OfflineExitAt int64 `json:"offline_exit_at"`
	}
	if err := json.Unmarshal(extra, &e); err != nil || e.OfflineExitAt <= 0 {
		return
	}
	log.Printf("[offline-exit] device %s (%s) exited kiosk offline at %d — disabling kiosk", serial, deviceID, e.OfflineExitAt)
	if cfg.KioskEnabled {
		if err := h.db.SetKioskConfig(ctx, deviceID, false, cfg.KioskPackage, cfg.KioskFeatures); err == nil {
			cfg.KioskEnabled = false
			cfgMap["kiosk_enabled"] = false
			// The caller publishes the device update AFTER this runs, so that broadcast
			// already carries the flipped state — no separate re-publish needed here.
		}
	}
	h.db.RecordOfflineExit(ctx, deviceID, e.OfflineExitAt)
	cfgMap["offline_exit_ack"] = e.OfflineExitAt
}

// notifyDeviceOnboarded records a persistent "new_device" alert and dispatches it
// to the configured outbound channels when a device checks in for the first time.
// Best-effort: failures are logged and never block the checkin response. Runs in
// its own goroutine with an independent context so a slow webhook can't stall the
// device's checkin.
func (h *Handler) notifyDeviceOnboarded(ctx context.Context, deviceID uuid.UUID, serial string) {
	ruleID, ok, err := h.db.EnabledRuleIDByType(ctx, "new_device")
	if err != nil {
		log.Printf("[onboard] lookup new_device rule: %v", err)
		return
	}
	if !ok {
		return // rule disabled — admin opted out of onboarding notifications
	}
	summary := fmt.Sprintf("New device onboarded: %s", serial)
	created, err := h.db.CreateAlertIfAbsent(ctx, &ruleID, "new_device", deviceID, "info", summary, nil)
	if err != nil {
		log.Printf("[onboard] create alert for %s: %v", serial, err)
		return
	}
	if !created {
		return // already recorded (e.g. concurrent first checkins)
	}
	n := db.AlertNotification{Type: "new_device", Severity: "info", Summary: summary, Serial: serial, EventAt: time.Now().UTC()}
	go h.alerts.Dispatch(context.WithoutCancel(ctx), []db.AlertNotification{n})
	h.hub.PublishAlertUpdate() // refresh the dashboard's live open-alert counter
}

// HandleWsCommandAck processes a "command_ack" message from a device over WS.
func (h *Handler) HandleWsCommandAck(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var body struct {
		CommandID uuid.UUID `json:"command_id"`
		Status    string    `json:"status"`
		Output    string    `json:"output"`
		Progress  *int      `json:"progress"`
		Package   string    `json:"package"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.CommandID == uuid.Nil {
		log.Printf("[ws-ack] parse error or missing command_id: %v", err)
		return
	}
	// Receipt ack: the device confirms it GOT the command (before/independent of running
	// it). Stamps received_at so redrive/redelivery stops re-pushing it — the key signal
	// that prevents both duplicate execution and re-push spam. Does not change status.
	if body.Status == "received" {
		if err := h.db.MarkCommandReceived(ctx, body.CommandID, deviceID); err != nil {
			log.Printf("[ws-ack] MarkCommandReceived error: %v", err)
			return
		}
		h.hub.PublishCommandUpdate(body.CommandID)
		return
	}
	// Interim install progress ('downloading'/'installing') updates status + percent
	// without finalizing; a separate path from the terminal ack so the dashboard can
	// show "Downloading 45%" / "Installing…" live.
	if body.Status == "downloading" || body.Status == "installing" {
		if err := h.db.SetCommandProgress(ctx, body.CommandID, deviceID, body.Status, body.Progress); err != nil {
			log.Printf("[ws-ack] SetCommandProgress error: %v", err)
			return
		}
		h.hub.PublishDeviceUpdate(deviceID)
		h.hub.PublishCommandUpdate(body.CommandID)
		return
	}
	if body.Status != "installed" && body.Status != "failed" && body.Status != "completed" {
		log.Printf("[ws-ack] invalid status: %q", body.Status)
		return
	}
	// Save the output BEFORE flipping status to terminal: a page load landing between the
	// two would see a terminal status (no longer polling) with no output attached yet,
	// leaving e.g. a screenshot thumbnail stuck on its placeholder until manual reload.
	if body.Output != "" {
		_ = h.db.SaveCommandResult(ctx, body.CommandID, deviceID, body.Output)
		// Same probe hook as the HTTP ack path: a device with a socket answers here
		// instead, and its progress probe would otherwise be read by nobody.
		h.LegacyProbeOutput(ctx, body.CommandID, body.Output)
	}
	if err := h.db.AckCommand(ctx, body.CommandID, deviceID, body.Status); err != nil {
		log.Printf("[ws-ack] AckCommand error: %v", err)
		return
	}
	if body.Status == "installed" && body.Package != "" {
		if cmd, err := h.db.GetCommand(ctx, body.CommandID); err == nil && cmd.ApkURL != "" {
			_ = h.db.LearnApkPackage(ctx, cmd.ApkURL, body.Package)
		}
	}
	// A completed mic_gain_read carries a fresher TX_DEC reading than the last check-in
	// snapshot: fold it into latest_extra so the device page's Hardware row (which reads
	// latest_extra.mic_gain) reflects the re-read without waiting for the next check-in.
	if body.Status == "completed" && strings.HasPrefix(strings.TrimSpace(body.Output), "{") {
		if cmd, err := h.db.GetCommand(ctx, body.CommandID); err == nil && (cmd.Type == "mic_gain_read" || cmd.Type == "mic_gain_set") {
			var mg map[string]json.RawMessage
			if json.Unmarshal([]byte(body.Output), &mg) == nil {
				if _, ok := mg["tx_dec"]; ok {
					patch, _ := json.Marshal(map[string]json.RawMessage{"mic_gain": json.RawMessage(body.Output)})
					if err := h.db.MergeLatestExtra(ctx, deviceID, patch); err != nil {
						log.Printf("[ws-ack] MergeLatestExtra(mic_gain) error: %v", err)
					}
				}
			}
		}
	}
	h.hub.PublishDeviceUpdate(deviceID)
	h.hub.PublishCommandUpdate(body.CommandID)
	// An install just reached a terminal state — release the next queued install for this
	// device (installs run one at a time; the delivery gate held the rest). Flushing here
	// makes the next one start immediately instead of waiting for a reconnect/redrive.
	h.flushPendingCommands(ctx, deviceID)
}

// HandleWsLogcat processes a "logcat_result" message from a device over WS.
func (h *Handler) HandleWsLogcat(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var body struct {
		RequestID uuid.UUID `json:"request_id"`
		Content   string    `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.RequestID == uuid.Nil {
		log.Printf("[ws-logcat] parse error or missing request_id: %v", err)
		return
	}
	if _, err := h.db.SaveLogcatResult(ctx, body.RequestID, deviceID, body.Content); err != nil {
		log.Printf("[ws-logcat] SaveLogcatResult error: %v", err)
		return
	}
	h.hub.PublishDeviceUpdate(deviceID)
	h.hub.PublishLogcatUpdate(deviceID)
	if dev, err := h.db.GetDeviceByID(ctx, deviceID); err == nil && dev != nil {
		h.legacyProgressFromLogcat(ctx, dev.SerialNumber, body.Content)
	}
}

// otaVerdict answers whether this check-in's device can take an MDM OTA: what the
// client advertised in extra.capabilities if it advertised anything, else the
// build rules. See internal/otagate.
func (h *Handler) otaVerdict(ctx context.Context, buildID, productKey string, extra json.RawMessage) otagate.Verdict {
	if len(extra) > 0 {
		var probe struct {
			AgentType    string   `json:"agent_type"`
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(extra, &probe); err == nil {
			if v, ok := otagate.ForAgentKind(probe.AgentType); ok {
				return v
			}
			if v, ok := otagate.Reported(probe.Capabilities); ok {
				return v
			}
		}
		if v, ok := otagate.ReportedExtra(extra); ok {
			return v
		}
	}
	return h.otaGate.Build(ctx, buildID, productKey)
}

// HandleWsOtaStatus processes an "ota_status" message from a device over WS.
// afterOtaTerminal applies deployment bookkeeping and the deployment's reboot
// policy after a device reports a terminal OTA status ("installed" or "error").
func (h *Handler) afterOtaTerminal(ctx context.Context, deviceID uuid.UUID, status, errorCode string) {
	upd, err := h.db.ResolveUpdateForDevice(ctx, deviceID)
	if err != nil {
		log.Printf("[ota] ResolveUpdateForDevice error: %v", err)
	}
	if status == "error" {
		if upd != nil {
			_ = h.db.SetUpdateDeviceFailed(ctx, upd.ID, deviceID, errorCode)
			// An incremental is a block diff against an exact source image; if it
			// failed (e.g. update_engine error 29, source partition mismatch) it can
			// never apply to this device. Pin the row to the full image so Retry on
			// the deployment page serves the guaranteed-applicable package instead
			// of the same failing diff. (Failed rows are not re-offered on their own.)
			if upd.OtaPackage != nil && upd.OtaPackage.Type == "incremental" {
				_ = h.db.SetUpdateDeviceForceFull(ctx, upd.ID, deviceID)
			}
			// 'failed' is terminal for this device — this may have been the last
			// non-terminal device, so re-check whether the deployment is now complete
			// (otherwise a failed device leaves it stuck 'active' forever).
			_ = h.db.CheckAndCompleteUpdate(ctx, upd.ID)
		}
		return
	}
	// status == "installed": the new build is applied to the inactive slot and
	// takes effect at the next reboot. What happens now is the deployment's call.
	// If we can't resolve which deployment this device belongs to, we don't know its
	// reboot policy — do NOT synthesize an immediate reboot (that would reboot a
	// 'manual' deployment's device and leave the row untracked). Let a later, re-
	// resolving checkin or an operator handle it.
	if upd == nil {
		return
	}
	// Proof: the build this device was on when it was targeted CAN apply an MDM OTA.
	// Record it, so the build gate learns from what actually happened instead of
	// relying on the cutoff alone. Never overrides an admin's explicit answer.
	if src, err := h.db.SourceBuildForTarget(ctx, upd.ID, deviceID); err == nil && src != "" {
		if dev, err := h.db.GetDeviceByID(ctx, deviceID); err == nil && dev != nil {
			if err := h.db.MarkOTAObserved(ctx, src, dev.ProductKey()); err == nil {
				h.otaGate.Invalidate()
			}
		}
	}
	behavior := upd.RebootBehavior
	_ = h.db.SetUpdateDeviceStatus(ctx, upd.ID, deviceID, "awaiting_reboot")
	switch behavior {
	case "manual":
		// An operator reboots the device when convenient — never auto-reboot.
		return
	case "scheduled":
		if upd.ScheduledTime != nil && upd.ScheduledTime.After(time.Now()) {
			return // ProcessDueScheduledReboots pushes the reboot when due
		}
		// No schedule or already past due — reboot now.
	}
	h.pushRebootFor(ctx, upd, deviceID)
}

// otaRebootPayload marks reboots the OTA flow sends on its own (no operator behind
// them); the dashboard clusters these and labels them "Sent automatically".
var otaRebootPayload = json.RawMessage(`{"reason":"ota"}`)

// pushRebootFor creates and pushes a reboot command for a device, recording
// reboot_sent on its deployment row when one is attached.
func (h *Handler) pushRebootFor(ctx context.Context, upd *db.Update, deviceID uuid.UUID) {
	cmd, err := h.db.CreateCommand(ctx, "reboot", "", otaRebootPayload, "devices", []uuid.UUID{deviceID})
	if err != nil {
		log.Printf("[ota] create reboot command error: %v", err)
		return
	}
	if upd != nil {
		_ = h.db.SetUpdateDeviceStatus(ctx, upd.ID, deviceID, "reboot_sent")
		h.optimisticallyCompleteReboot(ctx, upd.ID, deviceID)
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{deviceID})
}

// bootedAfter reports whether the device's current boot (now − uptime_seconds
// from the check-in extra) started after t. Used to tell "still waiting for the
// reboot" from "rebooted and came back on the old build".
func bootedAfter(extra json.RawMessage, t *time.Time) bool {
	if t == nil || len(extra) == 0 {
		return false
	}
	var m struct {
		Uptime *float64 `json:"uptime_seconds"`
	}
	if json.Unmarshal(extra, &m) != nil || m.Uptime == nil || *m.Uptime < 0 {
		return false
	}
	bootedAt := time.Now().Add(-time.Duration(*m.Uptime * float64(time.Second)))
	return bootedAt.After(t.Add(15 * time.Second))
}

// failSlotSwitch marks a reboot_sent row failed when the device has rebooted but
// reports the old build: update_engine applied the payload, the slot switch did
// not take. The deployment page shows it as failed with SLOT_SWITCH_FAILED.
func (h *Handler) failSlotSwitch(ctx context.Context, upd *db.Update, deviceID uuid.UUID, serial, buildID string) {
	_ = h.db.SetUpdateDeviceFailed(ctx, upd.ID, deviceID, "SLOT_SWITCH_FAILED")
	h.shell.ClearOTAProgress(deviceID)
	_ = h.db.CheckAndCompleteUpdate(ctx, upd.ID)
	h.hub.PublishDeploymentUpdate()
	target := ""
	if upd.OtaPackage != nil {
		target = upd.OtaPackage.TargetBuildID
	}
	log.Printf("[ota] %s rebooted but is on %s, not %s: slot switch failed (deployment %d)", serial, buildID, target, upd.ID)
}

// optimisticallyCompleteReboot wraps db.OptimisticallyCompleteReboot + the same
// CheckAndCompleteUpdate the confirming checkin path uses to flip the parent
// deployment to 'complete' — see db.OptimisticallyCompleteReboot for why every
// reboot-push site calls this instead of waiting for the device's next check-in.
func (h *Handler) optimisticallyCompleteReboot(ctx context.Context, updateID int, deviceID uuid.UUID) {
	if err := h.db.OptimisticallyCompleteReboot(ctx, updateID, deviceID); err != nil {
		log.Printf("[ota] OptimisticallyCompleteReboot error: %v", err)
		return
	}
	_ = h.db.CheckAndCompleteUpdate(ctx, updateID)
	h.hub.PublishDeploymentUpdate()
}

// ProcessDueScheduledReboots pushes reboot commands for devices whose
// scheduled-reboot deployments have come due. Called from a ticker in main.
func (h *Handler) ProcessDueScheduledReboots(ctx context.Context) {
	due, err := h.db.ListDueScheduledReboots(ctx)
	if err != nil {
		log.Printf("[ota-scheduler] ListDueScheduledReboots error: %v", err)
		return
	}
	for _, dr := range due {
		cmd, err := h.db.CreateCommand(ctx, "reboot", "", otaRebootPayload, "devices", []uuid.UUID{dr.DeviceID})
		if err != nil {
			log.Printf("[ota-scheduler] create reboot command error: %v", err)
			continue
		}
		_ = h.db.SetUpdateDeviceStatus(ctx, dr.UpdateID, dr.DeviceID, "reboot_sent")
		h.optimisticallyCompleteReboot(ctx, dr.UpdateID, dr.DeviceID)
		h.pushCommand(ctx, cmd, "devices", []uuid.UUID{dr.DeviceID})
		h.hub.PublishDeviceUpdate(dr.DeviceID)
		log.Printf("[ota-scheduler] scheduled reboot pushed device=%s update=%d", dr.DeviceID, dr.UpdateID)
	}
}

// RedriveStuckReboots re-issues the reboot for devices stuck at 'reboot_sent' (the
// reboot command was lost or declined) so a device that installed but never rebooted
// doesn't keep its deployment 'active' forever. Manual reboots ("Reboot to apply" /
// "Reboot all installed") reach the identical 'reboot_sent' status as auto
// immediate/scheduled reboots, so this applies to all of them alike. In practice
// this rarely finds anything now — optimisticallyCompleteReboot already flips the
// row to 'installed' the moment the original reboot is pushed — it mainly exists as
// a retry safety net if that first call failed.
func (h *Handler) RedriveStuckReboots(ctx context.Context) {
	const staleMinutes = 15
	stuck, err := h.db.ListStaleRebootSent(ctx, staleMinutes)
	if err != nil {
		log.Printf("[ota-scheduler] ListStaleRebootSent error: %v", err)
		return
	}
	for _, dr := range stuck {
		// Only re-drive a device that's actually online. An offline device can't receive
		// the push anyway, and creating a reboot row + refreshing updated_at every cycle
		// while it's gone just accumulates orphan command rows (one per 15-min window) for
		// a long-offline device. It stays 'reboot_sent' and gets re-driven the moment it
		// reconnects (Connect/Checkin also complete or re-drive it).
		if !h.hub.IsConnected(dr.DeviceID) {
			continue
		}
		cmd, err := h.db.CreateCommand(ctx, "reboot", "", otaRebootPayload, "devices", []uuid.UUID{dr.DeviceID})
		if err != nil {
			log.Printf("[ota-scheduler] re-drive reboot command error: %v", err)
			continue
		}
		// Refresh updated_at so this device isn't re-driven again for another window.
		_ = h.db.SetUpdateDeviceStatus(ctx, dr.UpdateID, dr.DeviceID, "reboot_sent")
		// Normally already a no-op by the time this runs (the original push already
		// optimistically completed it) — this is the retry safety net for the rare
		// case that first call failed.
		h.optimisticallyCompleteReboot(ctx, dr.UpdateID, dr.DeviceID)
		h.pushCommand(ctx, cmd, "devices", []uuid.UUID{dr.DeviceID})
		h.hub.PublishDeviceUpdate(dr.DeviceID)
		log.Printf("[ota-scheduler] re-drove stuck reboot device=%s update=%d", dr.DeviceID, dr.UpdateID)
	}
}

// ExpireStalledInstalls fails install_apk deliveries whose download or install stopped
// reporting progress past the stall window. A device that lost connectivity mid-install
// (see FW-2026-000020) exhausts its client-side retries and stops reporting — or its
// terminal ack is lost in the same outage — leaving the delivery stuck "in flight".
// Install commands are exempt from the short command TTL, so this sweep is their only
// terminal backstop. The window matches the install TTL leash (15 min).
func (h *Handler) ExpireStalledInstalls(ctx context.Context) {
	const stallMinutes = 15
	stalled, err := h.db.ExpireStalledInstalls(ctx, stallMinutes)
	if err != nil {
		log.Printf("[install-sweep] ExpireStalledInstalls error: %v", err)
		return
	}
	for _, s := range stalled {
		h.hub.PublishCommandUpdate(s.CommandID)
		h.hub.PublishDeviceUpdate(s.DeviceID)
		log.Printf("[install-sweep] failed stalled install command=%s device=%s", s.CommandID, s.DeviceID)
		// The failed install freed the device's install slot — release the next queued one.
		if h.hub.IsConnected(s.DeviceID) {
			h.flushPendingCommands(ctx, s.DeviceID)
		}
	}
}

// ExpireStalledOTAs settles OTA rows that stopped acking. Without it a dead download
// keeps its deployment 'active', and RebootBlockedFor then refuses the reboot that
// would clear it — the device is stuck until someone cancels the deployment by hand.
// The window is wider than the install sweep: an OTA package is far larger, and a slow
// link can legitimately go minutes between percent acks.
func (h *Handler) ExpireStalledOTAs(ctx context.Context) {
	const stallMinutes = 30
	stalled, err := h.db.ExpireStalledOTAs(ctx, stallMinutes)
	if err != nil {
		log.Printf("[ota-sweep] ExpireStalledOTAs error: %v", err)
		return
	}
	for _, s := range stalled {
		h.hub.PublishDeviceUpdate(s.DeviceID)
		log.Printf("[ota-sweep] failed stalled OTA device=%s (no progress for %dm)", s.DeviceID, stallMinutes)
	}
}

// ExpireOverdueCommands marks non-terminal commands (any type except reboot) that sat
// 'pending'/'delivered' past their per-type deadline as 'expired', so an undeliverable or
// never-acted-on command can't sit "in flight" forever. Backstop to the receipt-ack +
// redrive: those deliver to a device that returns; this terminalizes the ones that don't.
func (h *Handler) ExpireOverdueCommands(ctx context.Context) {
	expired, err := h.db.ExpireOverdueCommands(ctx)
	if err != nil {
		log.Printf("[command-sweep] ExpireOverdueCommands error: %v", err)
		return
	}
	for _, s := range expired {
		h.hub.PublishCommandUpdate(s.CommandID)
		h.hub.PublishDeviceUpdate(s.DeviceID)
		log.Printf("[command-sweep] expired overdue command=%s device=%s", s.CommandID, s.DeviceID)
	}
}

// RedriveStuckDeliveries re-pushes commands wedged at 'delivered' — the frame was
// enqueued onto the socket (so it was marked delivered) but a half-open socket swallowed
// it and the device never acted on it. The WS-connect flush only fires on a *new*
// connection, so a device that dropped and reconnected quickly (and then stayed
// connected) never gets the command again. This sweep closes that gap for every action
// type: for each stuck delivery whose device is connected right now, re-push over WS and
// refresh updated_at so it isn't re-driven again next tick. A command that was really
// received leaves 'delivered' (install → downloading, others → completed/failed) and so
// drops out of this set. Reboot and expired short-TTL one-shots are excluded in SQL.
// This is a server-side self-heal; the durable fix is a client receipt-ack so 'delivered'
// means "the device has it" rather than "we enqueued it".
func (h *Handler) RedriveStuckDeliveries(ctx context.Context) {
	const staleSeconds = 90
	stuck, err := h.db.ListStuckDeliveredCommands(ctx, staleSeconds)
	if err != nil {
		log.Printf("[delivery-sweep] ListStuckDeliveredCommands error: %v", err)
		return
	}
	for _, s := range stuck {
		// Only re-drive a device that's actually online now; an offline one can't receive
		// the push and self-heals via flushPendingCommands on its next WS connect.
		if !h.hub.IsConnected(s.DeviceID) {
			continue
		}
		msg := marshalCommand(s.CommandID, s.Type, s.ApkURL, s.Payload)
		if !h.hub.Push(s.DeviceID, msg) {
			continue
		}
		_ = h.db.MarkCommandsDelivered(ctx, s.DeviceID, []uuid.UUID{s.CommandID})
		h.hub.PublishCommandUpdate(s.CommandID)
		log.Printf("[delivery-sweep] re-drove stuck delivery command=%s type=%s device=%s", s.CommandID, s.Type, s.DeviceID)
	}
}

func (h *Handler) HandleWsOtaStatus(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var body struct {
		CommandID uuid.UUID `json:"command_id"`
		Status    string    `json:"status"`
		ErrorCode string    `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.CommandID == uuid.Nil {
		log.Printf("[ws-ota] parse error or missing command_id: %v", err)
		return
	}
	if body.Status != "downloaded" && body.Status != "installed" && body.Status != "error" {
		log.Printf("[ws-ota] invalid status: %q", body.Status)
		return
	}
	ackStatus := body.Status
	if body.Status == "error" {
		ackStatus = "failed"
	}
	if err := h.db.AckCommand(ctx, body.CommandID, deviceID, ackStatus); err != nil {
		log.Printf("[ws-ota] AckCommand error: %v", err)
		return
	}
	if body.ErrorCode != "" {
		_ = h.db.SaveCommandResult(ctx, body.CommandID, deviceID, body.ErrorCode)
	}
	if body.Status == "installed" || body.Status == "error" {
		h.shell.ClearOTAProgress(deviceID)
		h.afterOtaTerminal(ctx, deviceID, body.Status, body.ErrorCode)
	}
	h.hub.PublishDeviceUpdate(deviceID)
	h.hub.PublishDeploymentUpdate() // OTA status changed → refresh deployment pages
}

// HandleWsTelemetry processes a "telemetry" message sent by a device over the
// WebSocket connection. It performs the same upsert, OTA check, and config push
// as the HTTP Checkin handler, but returns config to the device over WS instead
// of an HTTP response body.
func (h *Handler) HandleWsTelemetry(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var req checkinRequest
	if err := decodeDeviceJSON(bytes.NewReader(raw), &req); err != nil {
		log.Printf("[ws-telemetry] parse error: %v", err)
		return
	}
	req.SerialNumber = strings.TrimSpace(req.SerialNumber)
	req.BuildID = strings.TrimSpace(req.BuildID)
	if req.SerialNumber == "" || req.BuildID == "" {
		log.Printf("[ws-telemetry] missing serial_number or build_id")
		return
	}
	if len(req.SerialNumber) > maxSerialLen || len(req.BuildID) > maxBuildIDLen || len(req.Extra) > maxExtraBytes {
		log.Printf("[ws-telemetry] oversized field from %s", req.SerialNumber)
		return
	}

	// Same gate as the HTTP path: DPC support off means store nothing. No config is
	// pushed back either — the frame is simply dropped.
	if h.cfg.IgnoreDPCCheckins() && isDPCPayload(req.Extra) {
		return
	}

	res, err := h.ingestCheckin(ctx, &req, sourceWS)
	if err != nil {
		log.Printf("[ws-telemetry] ingest error for %s: %v", req.SerialNumber, err)
		return
	}

	// The device gets its config back over the socket rather than in a response body.
	// "type" is what the client switches on, and it is set last so it cannot be
	// overwritten by a policy key of the same name.
	res.Config["type"] = "config"
	cfgMsg, _ := json.Marshal(res.Config)
	h.hub.Push(deviceID, cfgMsg)
}

// ── Logcat ────────────────────────────────────────────────────────────────────

func (h *Handler) SubmitLogcat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SerialNumber string    `json:"serial_number"`
		RequestID    uuid.UUID `json:"request_id"`
		Content      string    `json:"content"`
	}
	if err := decodeDeviceJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.SerialNumber == "" || body.RequestID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and request_id are required"})
		return
	}
	if h.deviceRateLimited(w, body.SerialNumber) {
		return
	}

	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	if _, err := h.db.SaveLogcatResult(r.Context(), body.RequestID, device.ID, body.Content); err != nil {
		if errors.Is(err, db.ErrLogcatNotTargeted) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "logcat request does not target this device"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishLogcatUpdate(device.ID)
	// A device mid legacy install with no live socket is followed through these
	// dumps — see pollLegacyInstall.
	h.legacyProgressFromLogcat(r.Context(), body.SerialNumber, body.Content)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Devices ───────────────────────────────────────────────────────────────────

func (h *Handler) ListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

// ListRestaurants returns the venues, for tooling that has to iterate them — the
// weekly report renderer runs off-box and needs to know what to render. Deliberately
// thin: id and name are what a caller needs to build a report URL, and this is an
// admin-key endpoint, not a place to widen the fleet's surface.
func (h *Handler) ListRestaurants(w http.ResponseWriter, r *http.Request) {
	rests, err := h.db.ListRestaurants(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]any, 0, len(rests))
	for _, x := range rests {
		out = append(out, map[string]any{
			"id": x.ID, "name": x.Name, "device_count": x.DeviceCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) GetDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	// Built from the shaped tables where they reach, so the response keeps the same
	// shape now that checkins.extra is no longer the source of truth; older windows
	// still come from the stored snapshots.
	checkins, ok, err := h.db.ShapedCheckins(r.Context(), device.ID, time.Now().UTC().Add(-7*24*time.Hour), time.Now().UTC(), 100)
	if err == nil && !ok {
		checkins, err = h.db.GetCheckins(r.Context(), device.ID, 100)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":   device,
		"checkins": checkins,
	})
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	g, err := h.db.CreateGroup(r.Context(), strings.TrimSpace(body.Name))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (h *Handler) ListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id"})
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found"})
		return
	}
	devices, err := h.db.ListGroupDevices(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "devices": devices})
}

func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id"})
		return
	}
	if err := h.db.DeleteGroup(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) AddDeviceToGroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id"})
		return
	}
	var body struct {
		SerialNumber string `json:"serial_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number is required"})
		return
	}
	if err := h.db.AddDeviceToGroup(r.Context(), body.SerialNumber, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) RemoveDeviceFromGroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id"})
		return
	}
	serial := r.PathValue("serial")
	if err := h.db.RemoveDeviceFromGroup(r.Context(), serial, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (h *Handler) CreateCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type       string          `json:"type"`
		ApkURL     string          `json:"apk_url"`
		Payload    json.RawMessage `json:"payload"`
		TargetType string          `json:"target_type"`
		Targets    []string        `json:"targets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.Type == "" {
		body.Type = "install_apk"
	}
	validTypes := map[string]bool{"install_apk": true, "shell": true, "screenshot": true, "reboot": true, "ota": true, "update_splash": true, "wipe": true, "uninstall": true,
		"app_reload": true, "app_restart": true, "app_clear_cache": true, "app_update_check": true,
		"app_update": true}
	if !validTypes[body.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid type"})
		return
	}
	if (body.Type == "install_apk" || body.Type == "app_update") && body.ApkURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "apk_url is required for " + body.Type})
		return
	}
	if body.Type == "uninstall" {
		// Package name travels in payload {"package": "..."}.
		var p struct {
			Package string `json:"package"`
		}
		_ = json.Unmarshal(body.Payload, &p)
		if strings.TrimSpace(p.Package) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payload.package is required for uninstall"})
			return
		}
	}
	if body.Type == "update_splash" {
		// URL travels in payload {"url": "...", "partition_size": <opt>}.
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body.Payload, &p)
		if strings.TrimSpace(p.URL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payload.url is required for update_splash"})
			return
		}
	}
	if body.TargetType != "all" && body.TargetType != "devices" && body.TargetType != "groups" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_type must be all, devices, or groups"})
		return
	}

	var targetIDs []uuid.UUID
	switch body.TargetType {
	case "devices":
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), body.Targets)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		targetIDs = ids
		// OTA is for our firmware devices only; a DPC device named here is dropped.
		if body.Type == "ota" {
			if targetIDs, err = h.db.FilterFirmwareDeviceIDs(r.Context(), targetIDs); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
	case "groups":
		for _, s := range body.Targets {
			id, err := uuid.Parse(s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id: " + s})
				return
			}
			targetIDs = append(targetIDs, id)
		}
	}

	// Capability gate (same as the dashboard): named devices whose agent can't run
	// this command are dropped and reported, rather than queued to fail.
	var unsupported []string
	if body.TargetType == "devices" && len(targetIDs) > 0 {
		if devs, err := h.db.GetDevicesByIDs(r.Context(), targetIDs); err == nil {
			kept := targetIDs[:0:0]
			for _, d := range devs {
				if d.Supports(body.Type) {
					kept = append(kept, d.ID)
				} else {
					unsupported = append(unsupported, d.SerialNumber)
				}
			}
			targetIDs = kept
			if len(targetIDs) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "none of the target devices' agents can run " + body.Type, "unsupported": unsupported})
				return
			}
		}
	}

	// A reboot during an OTA throws away the download or interrupts the install, so drop
	// those devices the same way installs drop duplicates. The MDM's own post-OTA reboot
	// is created directly (see ProcessDueScheduledReboots) and never passes through here.
	var rebootBusy []string
	if body.Type == "reboot" && body.TargetType == "devices" && len(targetIDs) > 0 {
		fresh := make([]uuid.UUID, 0, len(targetIDs))
		for _, id := range targetIDs {
			blocked, _, err := h.db.RebootBlockedFor(r.Context(), id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			if blocked {
				if d, e := h.db.GetDeviceByID(r.Context(), id); e == nil {
					rebootBusy = append(rebootBusy, d.SerialNumber)
				}
				continue
			}
			fresh = append(fresh, id)
		}
		targetIDs = fresh
		if len(targetIDs) == 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"created": false, "skipped": rebootBusy,
				"message": "every target device is taking an OTA; they reboot themselves when it is ready"})
			return
		}
	}

	// Don't pile up installs: drop devices that already have this exact APK in flight.
	var skipped []string
	if body.Type == "install_apk" && body.TargetType == "devices" && len(targetIDs) > 0 {
		inflight, err := h.db.DevicesWithPendingInstall(r.Context(), body.ApkURL, targetIDs, h.cfg.CommandExpiry())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if len(inflight) > 0 {
			fresh := make([]uuid.UUID, 0, len(targetIDs))
			var skippedIDs []uuid.UUID
			for _, id := range targetIDs {
				if inflight[id] {
					skippedIDs = append(skippedIDs, id)
				} else {
					fresh = append(fresh, id)
				}
			}
			if devs, e := h.db.GetDevicesByIDs(r.Context(), skippedIDs); e == nil {
				for _, d := range devs {
					skipped = append(skipped, d.SerialNumber)
				}
			}
			targetIDs = fresh
			if len(targetIDs) == 0 {
				writeJSON(w, http.StatusOK, map[string]any{"created": false, "skipped": skipped,
					"message": "all target devices already have this app installing"})
				return
			}
		}
	}

	// Capture the APK's size + ETag so the device can verify completeness and
	// resume a dropped download via HTTP Range. Best-effort: never blocks creation.
	if body.Type == "install_apk" {
		body.Payload = apkmeta.Augment(r.Context(), body.ApkURL, body.Payload)
	}

	cmd, err := h.db.CreateCommandBy(r.Context(), body.Type, body.ApkURL, body.Payload, body.TargetType, targetIDs, "API key")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.pushCommand(r.Context(), cmd, body.TargetType, targetIDs)

	if len(rebootBusy) > 0 {
		skipped = append(skipped, rebootBusy...)
	}
	if len(skipped) > 0 {
		writeJSON(w, http.StatusCreated, map[string]any{"command": cmd, "skipped": skipped})
		return
	}
	writeJSON(w, http.StatusCreated, cmd)
}

func (h *Handler) ListCommands(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommands(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, cmds)
}

func (h *Handler) GetCommandStatus(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid command id"})
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "command not found"})
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": cmd, "deliveries": deliveries})
}

func (h *Handler) AckCommand(w http.ResponseWriter, r *http.Request) {
	cmdID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid command id"})
		return
	}
	var body struct {
		SerialNumber string `json:"serial_number"`
		Status       string `json:"status"` // downloading | installing | installed | failed | completed
		Output       string `json:"output"`
		Progress     *int   `json:"progress"` // 0-100, meaningful while downloading
		Package      string `json:"package"`  // package the APK installed (learned on 'installed')
	}
	if err := decodeDeviceJSON(r.Body, &body); err != nil || body.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number is required"})
		return
	}
	if h.deviceRateLimited(w, body.SerialNumber) {
		return
	}
	interim := body.Status == "downloading" || body.Status == "installing"
	if !interim && body.Status != "received" && body.Status != "installed" && body.Status != "failed" && body.Status != "completed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be received, downloading, installing, installed, failed, or completed"})
		return
	}

	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	// Receipt ack over HTTP (fallback when WS is down): stamp received_at so redrive stops.
	if body.Status == "received" {
		if err := h.db.MarkCommandReceived(r.Context(), cmdID, device.ID); err != nil {
			if errors.Is(err, db.ErrCommandNotTargeted) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "command does not target device"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		h.hub.PublishCommandUpdate(cmdID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	// Interim progress updates the live status/percent without finalizing.
	if interim {
		if err := h.db.SetCommandProgress(r.Context(), cmdID, device.ID, body.Status, body.Progress); err != nil {
			if errors.Is(err, db.ErrCommandNotTargeted) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "command does not target device"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		h.hub.PublishDeviceUpdate(device.ID)
		h.hub.PublishCommandUpdate(cmdID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	// Save the output BEFORE flipping status to terminal: a page load landing between the
	// two would see a terminal status (no longer polling) with no output attached yet,
	// leaving e.g. a screenshot thumbnail stuck on its placeholder until manual reload.
	if body.Output != "" {
		_ = h.db.SaveCommandResult(r.Context(), cmdID, device.ID, body.Output)
		// A progress probe this server issued for a legacy install on a device with no
		// socket — read update_engine's own numbers out of it.
		h.LegacyProbeOutput(r.Context(), cmdID, body.Output)
	}
	if err := h.db.AckCommand(r.Context(), cmdID, device.ID, body.Status); err != nil {
		if errors.Is(err, db.ErrCommandNotTargeted) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "command does not target device"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Learn which package this APK installs, so future stuck installs auto-reconcile.
	if body.Status == "installed" && body.Package != "" {
		if cmd, err := h.db.GetCommand(r.Context(), cmdID); err == nil && cmd.ApkURL != "" {
			_ = h.db.LearnApkPackage(r.Context(), cmd.ApkURL, body.Package)
		}
	}
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishCommandUpdate(cmdID)
	// A command just reached a terminal state via the reliable HTTP ack path (installs and
	// other types ack here, not over WS). Release the next queued command for this device
	// immediately — the per-device serialization gate held the rest. Without this the queue
	// only advanced on the next reconnect, so a batch of installs would stall after the
	// first one completed while the device stayed online. Mirrors the WS ack handler.
	h.flushPendingCommands(r.Context(), device.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// OtaProgress is the HTTP fallback for live OTA download/install percent, used
// when the client's WS send fails or the socket is down (sendOtaProgressFrame
// on the client only ever tried WS, with no fallback — a flappy connection
// silently dropped every progress frame, leaving the dashboard's percent stuck
// even though the device was actually downloading the whole time).
func (h *Handler) OtaProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SerialNumber string    `json:"serial_number"`
		CommandID    uuid.UUID `json:"command_id"`
		Phase        string    `json:"phase"`
		Percent      int       `json:"percent"`
	}
	if err := decodeDeviceJSON(r.Body, &body); err != nil || body.SerialNumber == "" || body.CommandID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and command_id are required"})
		return
	}
	if h.deviceRateLimited(w, body.SerialNumber) {
		return
	}
	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	h.shell.SetOTAProgress(device.ID, body.CommandID, body.Phase, body.Percent)
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) OtaStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SerialNumber string    `json:"serial_number"`
		CommandID    uuid.UUID `json:"command_id"`
		Status       string    `json:"status"`     // downloaded | installed | error
		ErrorCode    string    `json:"error_code"` // optional, set when status=error
	}
	if err := decodeDeviceJSON(r.Body, &body); err != nil || body.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and command_id are required"})
		return
	}
	if h.deviceRateLimited(w, body.SerialNumber) {
		return
	}
	if body.Status != "downloaded" && body.Status != "installed" && body.Status != "error" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be downloaded, installed, or error"})
		return
	}

	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	ackStatus := body.Status
	if body.Status == "error" {
		ackStatus = "failed"
	}
	if err := h.db.AckCommand(r.Context(), body.CommandID, device.ID, ackStatus); err != nil {
		if errors.Is(err, db.ErrCommandNotTargeted) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "command does not target device"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if body.ErrorCode != "" {
		_ = h.db.SaveCommandResult(r.Context(), body.CommandID, device.ID, body.ErrorCode)
	}

	// Clear in-memory OTA progress and apply the deployment's reboot policy.
	if body.Status == "installed" || body.Status == "error" {
		h.shell.ClearOTAProgress(device.ID)
		h.afterOtaTerminal(r.Context(), device.ID, body.Status, body.ErrorCode)
	}

	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate() // OTA status changed → refresh deployment pages

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Productions ───────────────────────────────────────────────────────────────

func (h *Handler) ListProductions(w http.ResponseWriter, r *http.Request) {
	productions, err := h.db.ListProductions(r.Context(), h.connectedSlice())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if productions == nil {
		productions = []db.Production{}
	}
	writeJSON(w, http.StatusOK, productions)
}

func (h *Handler) CreateProduction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		ProductCode string `json:"product_code"`
		ModelCode   string `json:"model_code"`
		Variant     string `json:"variant"`
		SKU         string `json:"sku"`
		BatchMonth  int    `json:"batch_month"`
		BatchYear   int    `json:"batch_year"`
		// Batch is optional: blank encodes the code from batch_month+batch_year, which
		// is the convention. An early run whose serials predate it (e.g. "26") sets the
		// literal code here — those devices exist and must still match a production.
		Batch         string `json:"batch"`
		StartSequence int    `json:"start_sequence"`
		EndSequence   int    `json:"end_sequence"`
		Notes         string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(body.Name) == "" || body.ProductCode == "" || body.ModelCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, product_code, model_code required"})
		return
	}
	// The serial schema is a fixed 9-char prefix (product2+model2+variant1+sku2+batch2)
	// + 5-digit sequence = 14 chars, and the device-matching queries hard-code
	// LENGTH(serial)=14 and read the sequence at positions 10-14. A wrong-length code
	// shifts those positions and matches the wrong devices — the dashboard enforces
	// this, so the API must too.
	if len(body.ProductCode) != 2 || len(body.ModelCode) != 2 || len(body.Variant) != 1 || len(body.SKU) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "product_code and sku must be 2 chars, model_code 2, variant 1"})
		return
	}
	if body.BatchMonth < 1 || body.BatchMonth > 12 || body.BatchYear < 0 || body.BatchYear > 99 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch_month or batch_year"})
		return
	}
	if body.StartSequence < 1 || body.EndSequence < body.StartSequence {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid sequence range"})
		return
	}
	batch := strings.ToUpper(strings.TrimSpace(body.Batch))
	if batch == "" {
		batch = db.EncodeBatch(body.BatchMonth, body.BatchYear)
	} else if len(batch) != 2 || strings.IndexFunc(batch, func(c rune) bool {
		return !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z'))
	}) >= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batch must be 2 letters or digits"})
		return
	}
	variant := body.Variant
	if variant == "" {
		variant = "0"
	}
	sku := body.SKU
	if sku == "" {
		sku = "AA"
	}
	params := db.ProductionParams{
		Name:          strings.TrimSpace(body.Name),
		ProductCode:   strings.ToUpper(body.ProductCode),
		ModelCode:     body.ModelCode,
		Variant:       variant,
		SKU:           strings.ToUpper(sku),
		Batch:         batch,
		BatchMonth:    body.BatchMonth,
		BatchYear:     body.BatchYear,
		StartSequence: body.StartSequence,
		EndSequence:   body.EndSequence,
		Notes:         body.Notes,
	}
	prod, err := h.db.CreateProduction(r.Context(), params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, prod)
}

func (h *Handler) GetProduction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid production id"})
		return
	}
	prod, err := h.db.GetProduction(r.Context(), id, h.connectedSlice())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "production not found"})
		return
	}
	devices, err := h.db.GetProductionDevices(r.Context(), id, h.connectedSlice())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"production": prod, "devices": devices})
}

func (h *Handler) DeleteProduction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid production id"})
		return
	}
	if err := h.db.DeleteProduction(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// deviceCommandType is a thin alias for db.DeviceCommandType (the single source of
// truth) so the device always sees a type its agent understands.
func deviceCommandType(t string) string { return db.DeviceCommandType(t) }

func marshalCommand(id uuid.UUID, cmdType, apkURL string, payload json.RawMessage) []byte {
	msg, _ := json.Marshal(map[string]any{
		"type":         "command",
		"id":           id,
		"command_type": deviceCommandType(cmdType),
		"apk_url":      apkURL,
		"payload":      payload,
	})
	return msg
}

func marshalLogcatRequest(id uuid.UUID, level string, lines int, tag string) []byte {
	msg, _ := json.Marshal(map[string]any{
		"type":  "logcat_request",
		"id":    id,
		"level": level,
		"lines": lines,
		"tag":   tag,
	})
	return msg
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
