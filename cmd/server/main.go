package main

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA tz database so LoadLocation works without OS tzdata

	"github.com/google/uuid"
	"mdm/internal/aiobackend"
	"mdm/internal/api"
	"mdm/internal/cluster"
	"mdm/internal/config"
	"mdm/internal/dashboard"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/jobs"
	"mdm/internal/logstream"
	"mdm/internal/metrics"
	"mdm/internal/middleware"
	"mdm/internal/ota"
	"mdm/internal/adbtunnel"
	"mdm/internal/remote"
	"mdm/internal/safehttp"
	"mdm/internal/scout"
	"mdm/internal/shell"
	"mdm/internal/ws"
)

// geoStore adapts *db.DB to geolocate.LocationStore, backing the WiFi geolocation
// resolver with the DB-persisted learned WiFi-AP index.
type geoStore struct{ db *db.DB }

func (s geoStore) LookupAPs(ctx context.Context, bssids []string, fresherThan time.Time) ([]geolocate.APLocation, error) {
	rows, err := s.db.LookupWifiAPs(ctx, bssids, fresherThan)
	if err != nil {
		return nil, err
	}
	out := make([]geolocate.APLocation, len(rows))
	for i, r := range rows {
		out[i] = geolocate.APLocation{BSSID: r.BSSID, Lat: r.Lat, Lon: r.Lon, Accuracy: r.Accuracy}
	}
	return out, nil
}

func (s geoStore) LearnAPs(ctx context.Context, bssids []string, lat, lon, accuracy float64) error {
	return s.db.LearnWifiAPs(ctx, bssids, lat, lon, accuracy)
}

func (s geoStore) BumpAPHits(ctx context.Context, bssids []string) error {
	return s.db.BumpWifiAPHits(ctx, bssids)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin-create" {
		os.Exit(runAdminCreate(os.Args[2:]))
	}
	ctx := context.Background()

	port := getEnv("PORT", "8080")
	dbHost := getEnv("DB_HOST", "localhost")
	dbPort := getEnv("DB_PORT", "5432")
	dbUser := getEnv("DB_USER", "mdm")
	dbPass := getEnv("DB_PASSWORD", "mdm")
	dbName := getEnv("DB_NAME", "mdm")
	deviceAPIKey := mustEnv("DEVICE_API_KEY")
	// DEVICE_ENROLL_KEY (optional): a second shared key that is never in any source tree.
	// Firmware images built from 30 Sep carry it instead of the public default, so the
	// public DEVICE_API_KEY can be retired (device-key plan, phases 3 and 4) without
	// cutting those images off. Both are accepted until then.
	deviceEnrollKey := strings.TrimSpace(getEnv("DEVICE_ENROLL_KEY", ""))
	adminAPIKey := mustEnv("ADMIN_API_KEY")
	// SESSION_SECRET signs the dashboard session cookie. It must be its OWN secret:
	// the previous fallback to DEVICE_API_KEY meant the cookie-signing key was the
	// fleet-wide key every managed device holds, so a single extracted device key
	// handed an attacker the dashboard's cookie integrity key. No fallback now.
	sessionSecret := mustEnv("SESSION_SECRET")
	if len(sessionSecret) < 32 {
		log.Fatalf("SESSION_SECRET must be at least 32 bytes for secure session cookie signing")
	}
	if deviceEnrollKey != "" && (len(deviceEnrollKey) < 32 || deviceEnrollKey == deviceAPIKey || deviceEnrollKey == adminAPIKey || deviceEnrollKey == sessionSecret) {
		log.Fatalf("DEVICE_ENROLL_KEY must be at least 32 bytes and distinct from the other keys")
	}
	if sessionSecret == deviceAPIKey || sessionSecret == adminAPIKey {
		log.Fatalf("SESSION_SECRET must be distinct from DEVICE_API_KEY and ADMIN_API_KEY")
	}
	// QA_API_KEY (optional): a separate, read-only key for the testing team's own
	// dashboard — GET /api/v1/testdata/* only, never the admin-key surface.
	// Unset disables the route entirely rather than falling back to the admin key.
	qaAPIKey := strings.TrimSpace(getEnv("QA_API_KEY", ""))
	if qaAPIKey != "" && (len(qaAPIKey) < 32 || qaAPIKey == deviceAPIKey || qaAPIKey == adminAPIKey || qaAPIKey == sessionSecret || qaAPIKey == deviceEnrollKey) {
		log.Fatalf("QA_API_KEY must be at least 32 bytes and distinct from the other keys")
	}
	configPath := getEnv("CONFIG_PATH", "config/display.json")

	// SSRF allowlist: comma-separated IPs/CIDRs the OTA-inspect / webhook clients may
	// reach despite being private (e.g. an internal LAN OTA package host). Empty by
	// default — the private-address block stays fully in force.
	if al := strings.TrimSpace(getEnv("SSRF_ALLOW_CIDRS", "")); al != "" {
		safehttp.SetAllowlist(strings.Split(al, ","))
		log.Printf("safehttp: SSRF allowlist = %s", al)
	}

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPass, dbName)

	var database *db.DB
	var lastErr error
	for i := 0; i < 10; i++ {
		var newErr error
		database, newErr = db.New(ctx, connStr)
		if newErr != nil {
			lastErr = newErr
			log.Printf("DB not ready (attempt %d/10): %v — retrying in 2s...", i+1, newErr)
			time.Sleep(2 * time.Second)
			continue
		}
		if pingErr := database.Ping(ctx); pingErr != nil {
			lastErr = pingErr
			log.Printf("DB not ready (attempt %d/10): %v — retrying in 2s...", i+1, pingErr)
			time.Sleep(2 * time.Second)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		log.Fatalf("failed to connect to database: %v", lastErr)
	}
	defer database.Close()
	log.Println("Connected to database")

	if err := database.RunMigrations(ctx); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	log.Println("Migrations applied")

	if err := database.EnsureDefaultRules(ctx); err != nil {
		log.Printf("seed default alert rules: %v", err)
	}

	// Best-effort, non-fatal: heal deployments stranded 'complete' with a device
	// still pending (so they resume on this device's next check-in). Kept out of
	// RunMigrations so a data-reconciliation hiccup can never crash startup, and
	// time-bounded so it can never stall startup waiting on a row lock (e.g. while
	// the previous container is still draining during a deploy) — that stall is
	// what turns into a 502 at the proxy. If it times out, the next boot retries.
	recCtx, recCancel := context.WithTimeout(ctx, 10*time.Second)
	if n, err := database.ReconcileStrandedUpdates(recCtx); err != nil {
		log.Printf("reconcile stranded updates (skipped, non-fatal): %v", err)
	} else if n > 0 {
		log.Printf("reconcile stranded updates: reactivated %d deployment(s)", n)
	}
	recCancel()

	hub := ws.NewHub()
	shellMgr := shell.NewManager()
	// Mark the command received the moment any OTA progress comes in (WS or
	// checkin-piggybacked) — otherwise it sits at command_status.status='delivered'
	// for the whole download/install and RedriveStuckDeliveries repeatedly re-pushes
	// it as "stuck" every ~90s even while the device is actively working on it. See
	// shell.Manager.OnOTAProgress.
	// A client that counts bytes gets them stored next to the percent, so the page can
	// say "214 MB of 1.6 GB" instead of a number that moves once per twenty megabytes.
	shellMgr.OnOTABytes = func(deviceID, commandID uuid.UUID, phase string, percent int, done, total int64) {
		if commandID == uuid.Nil {
			return
		}
		st := phase
		if st != "installing" {
			st = "downloading"
		}
		p := percent
		if err := database.SetCommandProgressBytes(context.Background(), commandID, deviceID, st, &p, done, total); err != nil &&
			!errors.Is(err, db.ErrCommandNotTargeted) {
			log.Printf("OnOTABytes: %v", err)
		}
	}
	shellMgr.OnOTAProgress = func(deviceID, commandID uuid.UUID) {
		if commandID == uuid.Nil { // legacy OTA progress has no command behind it
			return
		}
		if err := database.MarkCommandReceived(context.Background(), commandID, deviceID); err != nil && !errors.Is(err, db.ErrCommandNotTargeted) {
			log.Printf("OnOTAProgress: MarkCommandReceived error: %v", err)
		}
		// Write the progress through to command_status too — the live value normally
		// lives only in shellMgr's in-memory cache, which a server restart wipes,
		// leaving every in-progress OTA showing a blank "—" until the device happens
		// to report again. Persisting here lets rehydrateOTAProgress (below) restore
		// it on the next start. verifying/finalizing collapse into "installing" —
		// SetCommandProgress's status must stay one of the values the delivery/redrive
		// queries already recognize (see GetPendingCommandsForDevice).
		if p := shellMgr.GetOTAProgress(deviceID); p != nil {
			status := "installing"
			if p.Phase == "downloading" {
				status = "downloading"
			}
			pct := p.Percent
			if err := database.SetCommandProgress(context.Background(), commandID, deviceID, status, &pct); err != nil {
				log.Printf("OnOTAProgress: SetCommandProgress error: %v", err)
			}
		}
		// Live-refresh any open deployment page. Without this, a WS-connected device's
		// progress frames (the common case — most devices are online) only reached the
		// deployment detail page via its 30s fallback poll: the checkin-piggybacked and
		// HTTP-fallback progress paths already call this (see recordCheckinOtaProgress /
		// Handler.OtaProgress in internal/api/handlers.go), but this WS path — which fires
		// far more often, every few seconds during an active download — never did.
		hub.PublishDeploymentUpdate()
	}
	// Rehydrate shellMgr's in-memory OTA progress cache from what was last persisted,
	// so a redeploy doesn't blank every in-progress OTA's percent until the device
	// reports fresh progress again.
	if rows, err := database.ActiveOTAProgress(context.Background()); err != nil {
		log.Printf("ActiveOTAProgress: %v", err)
	} else {
		for _, r := range rows {
			shellMgr.SetOTAProgress(r.DeviceID, r.CommandID, r.Status, r.Progress)
		}
		if len(rows) > 0 {
			log.Printf("rehydrated OTA progress for %d device(s)", len(rows))
		}
	}
	// A shell command's live output stream closing ("command_done", carrying the
	// exit code) is its actual completion signal — nothing previously turned that
	// into a DB ack, so the terminal output finished while the delivery status/
	// summary pill on the Actions page stayed "in progress" until a reload
	// re-derived it fresh. Mirrors the same ack the WS "command_ack" path applies
	// for every other command type.
	// Set once the API handler exists (below): a finished shell command may be a
	// legacy-install progress probe, and on some devices its output arrives ONLY as
	// stream frames — their ack carries none — so this is where it gets read.
	var onShellOutput func(deviceID, commandID uuid.UUID, output string)
	shellMgr.OnCommandDone = func(deviceID, commandID uuid.UUID, exitCode int) {
		ctx := context.Background()
		out := shellMgr.CommandOutput(commandID, deviceID)
		if out != "" {
			if onShellOutput != nil {
				onShellOutput(deviceID, commandID, out)
			}
			// Persist the streamed output as the command's result. The firmware client
			// also acks over HTTP with what it collected (that write lands later and wins),
			// but the standard client only ever streams — so without this a DPC shell
			// completed with an empty result in the admin API and the command history.
			if err := database.SaveCommandResult(ctx, commandID, deviceID, out); err != nil {
				log.Printf("OnCommandDone: save output: %v", err)
			}
		}
		status := "completed"
		if exitCode != 0 {
			status = "failed"
		}
		if err := database.AckCommand(ctx, commandID, deviceID, status); err != nil {
			if !errors.Is(err, db.ErrCommandNotTargeted) {
				log.Printf("OnCommandDone: AckCommand error: %v", err)
			}
			return
		}
		hub.PublishCommandUpdate(commandID)
	}
	remoteMgr := remote.New(hub)
	// adb tunnels: a port range the server binds per session (ADB_TUNNEL_PORTS, e.g.
	// "42000-42019", also published in docker-compose.yml and opened in the security
	// group) and the host name admins `adb connect` to (ADB_TUNNEL_HOST, default: the
	// dashboard's own). Unset = the Wireless adb page offers no tunnels.
	tunnelMgr, err := adbtunnel.New(hub, os.Getenv("ADB_TUNNEL_HOST"), os.Getenv("ADB_TUNNEL_PORTS"))
	if err != nil {
		log.Fatalf("adb tunnels: %v", err)
	}
	if tunnelMgr.Enabled() {
		log.Printf("adb tunnels enabled on ports %s", tunnelMgr.PortRange())
	}
	logMgr := logstream.NewManager()
	hub.SetOnBinaryMessage(func(deviceID uuid.UUID, data []byte) {
		defer recoverLog("ws binary from " + deviceID.String())
		remoteMgr.RelayFrame(deviceID, data)
	})

	mux := http.NewServeMux()

	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	mux.Handle("GET /static/", middleware.CompressStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Disable the default directory autoindex: any request resolving to a
		// directory (path ends in "/") is rejected so the static tree can't be
		// enumerated for dev-only artifacts.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		staticFS.ServeHTTP(w, r)
	})))

	// Generated splash images (uploaded BMP/PNG/JPEG wrapped into splash.img by the
	// dashboard) live on a persistent volume and are downloaded by devices. Filenames
	// are unguessable UUIDs, so this is served unauthenticated like /static/.
	splashDir := dashboard.SplashStoreDir()
	if err := os.MkdirAll(splashDir, 0o755); err != nil {
		log.Fatalf("failed to create splash dir: %v", err)
	}
	splashFS := http.StripPrefix("/splash-img/", http.FileServer(http.Dir(splashDir)))
	mux.Handle("GET /splash-img/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		splashFS.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// The status code carries liveness for the load balancer; the body is
		// deliberately minimal and does not disclose the database backend or its
		// state to unauthenticated callers (F-08).
		w.Header().Set("Content-Type", "application/json")
		if err := database.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"error"}`))
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Live metrics for /debug/vars (admin-gated below): the DB pool (saturation is the
	// most likely silent degradation), connected-device count (mass disconnect), and
	// goroutine count (leak). Plus http_requests_total / http_errors_total from AccessLog.
	expvar.Publish("db_pool", expvar.Func(func() any { return database.PoolStats() }))
	expvar.Publish("ws_connected_devices", expvar.Func(func() any { return len(hub.ConnectedIDs()) }))
	expvar.Publish("goroutines", expvar.Func(func() any { return runtime.NumGoroutine() }))
	// Every periodic job: runs, last/max duration, timeouts, panics, overruns (jobs.Run).
	expvar.Publish("jobs", expvar.Func(func() any { return jobs.Default.Snapshot() }))
	// Devices that are reporting and cannot be commanded: checking in, but holding no
	// WebSocket for over commandChannelGrace. One number for the failure that hid behind
	// a healthy-looking fleet for a day — if it is not ~0, control is being lost somewhere.
	// Devices that reconnected without checking in are corrected here rather than left
	// counted, so the gauge does not drift upward on its own.
	expvar.Publish("devices_no_command_channel", expvar.Func(func() any {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		missing, err := database.ListWSMissingDevices(ctx, 20*time.Minute, 15*time.Minute)
		if err != nil {
			return -1
		}
		n := 0
		var reconnected []uuid.UUID
		for _, m := range missing {
			if hub.IsConnected(m.DeviceID) {
				reconnected = append(reconnected, m.DeviceID)
				continue
			}
			n++
		}
		if len(reconnected) > 0 {
			_ = database.ClearWSMissingForDevices(ctx, reconnected)
		}
		return n
	}))

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.CheckWritable(); err != nil {
		log.Printf("WARNING: config directory is not writable — settings changes will NOT persist across restarts and background jobs cannot save progress. Fix ownership of the data volume (e.g. `chown -R mdm:mdm /app/data`). Error: %v", err)
	}
	// Migrate a legacy single alert webhook into a default channel (one-time, no-op
	// if channels already exist or no legacy URL is set).
	if err := database.EnsureDefaultChannelFromWebhook(ctx, cfg.AlertWebhookURL()); err != nil {
		log.Printf("seed default alert channel: %v", err)
	}

	var geo *geolocate.Resolver
	if key := os.Getenv("GOOGLE_GEOLOCATION_API_KEY"); key != "" {
		geo = geolocate.New(key)
		// Back the resolver with the DB-persisted learned WiFi-AP index so scans that
		// overlap previously-seen APs resolve without calling Google.
		geo.SetStore(geoStore{db: database})
		log.Println("Geolocation resolver enabled (Google Geolocation API + learned WiFi index)")
	}
	var geocoder *geolocate.Geocoder
	if key := os.Getenv("GOOGLE_GEOCODING_API_KEY"); key != "" {
		geocoder = geolocate.NewGeocoder(key)
		log.Println("Reverse geocoder enabled (Google Geocoding API)")
	}
	apiHandler := api.NewHandler(database, hub, shellMgr, cfg, geo, geocoder, remoteMgr, logMgr, adminAPIKey)
	apiHandler.SetAdbTunnels(tunnelMgr)
	// Scout enrolment: a firmware device finds and enrols stock devices on its venue's
	// Wi-Fi. Frames arrive on the device WS (routed below); the scheduler and the
	// dashboard's approve action both drive it.
	scoutSvc := scout.New(database, hub, cfg)
	expvar.Publish("ingest", expvar.Func(func() any { return apiHandler.IngestStats() }))
	// Flush queued commands the moment a device's WS registers (socket writable). The HTTP
	// /connect flush can fire before the socket opens, and a never-delivered command has
	// nothing else to re-trigger it, so it would sit in the queue until the next reconnect.
	hub.SetOnConnect(func(deviceID uuid.UUID) {
		defer recoverLog("ws onConnect flush for " + deviceID.String())
		apiHandler.FlushPendingCommands(context.Background(), deviceID)
		// The venue, table and guest Wi-Fi the tablet shows its guests.
		apiHandler.PushGuest(context.Background(), deviceID)
		// A legacy install that started while this device was offline has no progress
		// source until its socket is back — pick the update_engine log up now.
		apiHandler.ResumeLegacyWatch(context.Background(), deviceID)
	})
	onShellOutput = func(deviceID, commandID uuid.UUID, output string) {
		apiHandler.LegacyProbeOutput(context.Background(), commandID, output)
	}
	// Watchers live in memory: pick up any legacy install that was running when this
	// process started (a deploy mid-install, a crash, a restart).
	go apiHandler.ResumeLegacyWatches(context.Background())
	// Packages added before the wipe flag existed, or whose zip could not be read when
	// they were added: read each once (a few KB of range requests) so a factory-reset
	// image is never mistaken for a safe fallback. Off the start path, one at a time.
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ota] wipe backfill panic: %v", rec)
			}
		}()
		time.Sleep(20 * time.Second)
		pkgs, err := database.PackagesNeedingWipeCheck(context.Background(), 1000)
		if err != nil {
			log.Printf("[ota] wipe backfill: %v", err)
			return
		}
		wipes := 0
		for _, p := range pkgs {
			if ota.RecordWipe(context.Background(), database, p.ID, p.URL, false) {
				wipes++
			}
		}
		if len(pkgs) > 0 {
			log.Printf("[ota] wipe backfill: read %d package(s), %d wipe the device", len(pkgs), wipes)
		}
	}()
	hub.SetOnMessage(func(deviceID uuid.UUID, raw []byte) {
		// This runs on the device's WS read-loop goroutine, which net/http does not
		// protect — a panic here would crash the process and drop the whole fleet.
		defer recoverLog("ws message from " + deviceID.String())
		var peek struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &peek)
		switch peek.Type {
		case "telemetry":
			apiHandler.HandleWsTelemetry(deviceID, raw)
		case "command_ack":
			apiHandler.HandleWsCommandAck(deviceID, raw)
		case "logcat_result":
			apiHandler.HandleWsLogcat(deviceID, raw)
		case "ota_status":
			apiHandler.HandleWsOtaStatus(deviceID, raw)
		case "pong_response":
			var p struct {
				Nonce string `json:"nonce"`
			}
			_ = json.Unmarshal(raw, &p)
			hub.SignalPong(p.Nonce)
		case "logcat_stream", "logcat_stream_end", "shell_data", "shell_end":
			logMgr.HandleDeviceMessage(raw)
		case "net_sighting":
			scoutSvc.IngestSighting(context.Background(), deviceID, raw)
		case "net_scan_done":
			// The sightings already landed; this is the only record of a sweep that
			// found nothing, so log it rather than drop it.
			var d struct {
				Hosts    int `json:"hosts"`
				Open     int `json:"open"`
				Accepted int `json:"accepted"`
			}
			if json.Unmarshal(raw, &d) == nil {
				log.Printf("[scout] net_scan_done from %s: %d hosts, %d open, %d ours",
					deviceID, d.Hosts, d.Open, d.Accepted)
			}
			// The venue is free for another scan now, rather than after scanTTL.
			scoutSvc.ScanDone(context.Background(), deviceID)
		case "remote_clipboard": // the device's clipboard, for its remote page (client 1.16.0)
			remoteMgr.RelayText(deviceID, raw)
		case "net_enroll_progress":
			scoutSvc.IngestProgress(context.Background(), raw)
		case "net_enroll_done":
			scoutSvc.IngestEnrollDone(context.Background(), deviceID, raw)
		default:
			shellMgr.HandleDeviceMessage(deviceID, raw)
		}
	})

	// Device auth accepts the legacy shared fleet key or a per-device key issued at
	// enrollment; per-device keys bind the request to its device's serial.
	deviceKeyLookup := func(ctx context.Context, hash string) (string, bool) {
		_, serial, err := database.DeviceSerialByKeyHash(ctx, hash)
		return serial, err == nil
	}
	deviceAuth := func(h http.Handler) http.Handler { return middleware.DeviceAuth([]string{deviceAPIKey, deviceEnrollKey}, deviceKeyLookup, h) }
	adminAuth := func(h http.Handler) http.Handler { return middleware.AdminAPIKeyAuth(adminAPIKey, h) }
	// maxDeviceBody caps device POST bodies (post-inflation). Check-ins carry the
	// installed-app list and a logcat result can be sizable, so it's generous, but
	// it stops a single device from exhausting memory with an unbounded body.
	const maxDeviceBody = 8 << 20 // 8 MiB
	devicePost := func(h http.HandlerFunc) http.Handler { return deviceAuth(middleware.MaxBytes(maxDeviceBody, h)) }

	// Metrics for monitoring (admin-gated): db pool, ws connected, goroutines, request
	// counters. Scrape as JSON; not exposed to devices or the public.
	mux.Handle("GET /debug/vars", adminAuth(expvar.Handler()))

	// Testing team's own read-only key — GET /api/v1/testdata/* only, so a leaked
	// key can't reach anything the admin key can (reboot, wipe, kiosk unlock, ...).
	// CORS is wide open (*) rather than an allowlist: the dashboard calling this is a
	// static page that can be hosted/moved anywhere, and the route is already gated by
	// its own key, so an origin check adds no real protection here.
	if qaAPIKey != "" {
		qaAuth := func(h http.Handler) http.Handler { return middleware.APIKeyAuth(qaAPIKey, `{"error":"unauthorized"}`, h) }
		qaCORS := func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Headers", "X-API-Key")
				w.Header().Set("Access-Control-Allow-Methods", "GET")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				h.ServeHTTP(w, r)
			})
		}
		mux.Handle("GET /api/v1/testdata/devices", qaCORS(qaAuth(http.HandlerFunc(apiHandler.ListTestDataDevices))))
		mux.Handle("OPTIONS /api/v1/testdata/devices", qaCORS(http.NotFoundHandler()))
		mux.Handle("GET /api/v1/testdata/devices/{serial}/history", qaCORS(qaAuth(http.HandlerFunc(apiHandler.ListTestDataDeviceHistory))))
		mux.Handle("OPTIONS /api/v1/testdata/devices/{serial}/history", qaCORS(http.NotFoundHandler()))
		mux.Handle("GET /api/v1/testdata/filters", qaCORS(qaAuth(http.HandlerFunc(apiHandler.ListTestDataFilters))))
		mux.Handle("OPTIONS /api/v1/testdata/filters", qaCORS(http.NotFoundHandler()))
		// Battery data (the /battery page's): who is sending, a device's rows, its climbs.
		mux.Handle("GET /api/v1/testdata/battery", qaCORS(qaAuth(http.HandlerFunc(apiHandler.ListTestDataBattery))))
		mux.Handle("OPTIONS /api/v1/testdata/battery", qaCORS(http.NotFoundHandler()))
		mux.Handle("GET /api/v1/testdata/devices/{serial}/battery", qaCORS(qaAuth(http.HandlerFunc(apiHandler.GetTestDataDeviceBattery))))
		mux.Handle("OPTIONS /api/v1/testdata/devices/{serial}/battery", qaCORS(http.NotFoundHandler()))
		mux.Handle("GET /api/v1/testdata/devices/{serial}/battery/climbs", qaCORS(qaAuth(http.HandlerFunc(apiHandler.GetTestDataDeviceBatteryClimbs))))
		mux.Handle("OPTIONS /api/v1/testdata/devices/{serial}/battery/climbs", qaCORS(http.NotFoundHandler()))
		// Host app on screen (21 Oct 2026): every device's state now, and one device's spans.
		mux.Handle("GET /api/v1/testdata/host-app", qaCORS(qaAuth(http.HandlerFunc(apiHandler.ListTestDataHostApp))))
		mux.Handle("OPTIONS /api/v1/testdata/host-app", qaCORS(http.NotFoundHandler()))
		mux.Handle("GET /api/v1/testdata/devices/{serial}/host-app", qaCORS(qaAuth(http.HandlerFunc(apiHandler.GetTestDataDeviceHostApp))))
		mux.Handle("OPTIONS /api/v1/testdata/devices/{serial}/host-app", qaCORS(http.NotFoundHandler()))
	}

	// WebSocket — device connects here for server-push command delivery
	mux.Handle("GET /api/v1/ws", deviceAuth(http.HandlerFunc(apiHandler.Connect)))
	// The device's leg of an adb tunnel stream (see internal/adbtunnel).
	mux.Handle("GET /api/v1/adb-tunnel/{session}/{stream}", deviceAuth(http.HandlerFunc(apiHandler.ConnectAdbTunnel)))

	// Enrollment — unauthenticated by design: the profile token IS the credential
	// (rate-limited per IP inside the handler). Exchanges a token for a device key.
	mux.Handle("POST /api/v1/enroll", middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.Enroll)))

	// Device-authenticated endpoints (body-size limited)
	mux.Handle("POST /api/v1/checkin", devicePost(apiHandler.Checkin))
	mux.Handle("POST /api/v1/checkins/backfill", deviceAuth(middleware.MaxBytes(1<<20, http.HandlerFunc(apiHandler.Backfill))))
	// Per-app usage, about every 15 minutes (api/app_usage.go).
	mux.Handle("POST /api/v1/app-usage", deviceAuth(middleware.MaxBytes(256<<10, http.HandlerFunc(apiHandler.AppUsage))))
	// Battery log (firmware client 1.10.1): up to 2000 CSV rows a request, ~150 KB inflated.
	mux.Handle("POST /api/v1/battery-log", deviceAuth(middleware.MaxBytes(2<<20, http.HandlerFunc(apiHandler.BatteryLog))))
	mux.Handle("POST /api/v1/device-key", devicePost(apiHandler.RegisterDeviceKey))
	mux.Handle("POST /api/v1/devices/{serial}/key-reset", adminAuth(http.HandlerFunc(apiHandler.ResetDeviceKey)))
	mux.Handle("POST /api/v1/devices/{serial}/simulate-offline", adminAuth(http.HandlerFunc(apiHandler.SimulateOffline)))
	// Pull delivery: the device asks for its own work, so a device with no socket is late
	// rather than unreachable. Same auth as the WebSocket; see api/commands_pull.go.
	mux.Handle("GET /api/v1/commands/pending", deviceAuth(http.HandlerFunc(apiHandler.PendingCommands)))
	mux.Handle("GET /api/v1/venue-icon", deviceAuth(http.HandlerFunc(apiHandler.VenueIcon)))
	mux.Handle("POST /api/v1/commands/{id}/ack", devicePost(apiHandler.AckCommand))
	// Save to library (26 Oct 2026): a device uploads an installed APK for a pull_apk.
	mux.Handle("POST /api/v1/apk-pull/url", devicePost(apiHandler.APKPullURL))
	mux.Handle("POST /api/v1/apk-pull/done", devicePost(apiHandler.APKPullDone))
	mux.Handle("POST /api/v1/logcat", devicePost(apiHandler.SubmitLogcat))
	mux.Handle("POST /api/v1/ota/status", devicePost(apiHandler.OtaStatus))
	mux.Handle("POST /api/v1/ota/progress", devicePost(apiHandler.OtaProgress))

	// Admin-authenticated API endpoints
	mux.Handle("GET /api/v1/restaurants", adminAuth(http.HandlerFunc(apiHandler.ListRestaurants)))
	mux.Handle("POST /api/v1/restaurants", adminAuth(middleware.MaxBytes(4<<10, http.HandlerFunc(apiHandler.CreateRestaurant))))
	mux.Handle("POST /api/v1/restaurants/{id}/devices", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.AssignRestaurantDevices))))
	mux.Handle("POST /api/v1/restaurants/{id}/icon", adminAuth(middleware.MaxBytes(3<<20, http.HandlerFunc(apiHandler.SetRestaurantIcon))))
	mux.Handle("GET /api/v1/devices", adminAuth(http.HandlerFunc(apiHandler.ListDevices)))
	mux.Handle("GET /api/v1/devices/{serial}", adminAuth(http.HandlerFunc(apiHandler.GetDevice)))
	mux.Handle("GET /api/v1/devices/{serial}/temp-fast", adminAuth(http.HandlerFunc(apiHandler.GetTempFast)))
	mux.Handle("GET /api/v1/hardware-serials", adminAuth(http.HandlerFunc(apiHandler.ListHardwareSerials)))
	mux.Handle("POST /api/v1/devices/{serial}/temp-fast", adminAuth(middleware.MaxBytes(4<<10, http.HandlerFunc(apiHandler.SetTempFast))))
	mux.Handle("POST /api/v1/devices/{serial}/ping", adminAuth(http.HandlerFunc(apiHandler.PingDevice)))
	mux.Handle("GET /api/v1/remote/{serial}", http.HandlerFunc(apiHandler.ConnectRemote))

	// Groups
	mux.Handle("GET /api/v1/groups", adminAuth(http.HandlerFunc(apiHandler.ListGroups)))
	mux.Handle("POST /api/v1/groups", adminAuth(http.HandlerFunc(apiHandler.CreateGroup)))
	mux.Handle("GET /api/v1/groups/{id}", adminAuth(http.HandlerFunc(apiHandler.GetGroup)))
	mux.Handle("DELETE /api/v1/groups/{id}", adminAuth(http.HandlerFunc(apiHandler.DeleteGroup)))
	mux.Handle("POST /api/v1/groups/{id}/devices", adminAuth(http.HandlerFunc(apiHandler.AddDeviceToGroup)))
	mux.Handle("DELETE /api/v1/groups/{id}/devices/{serial}", adminAuth(http.HandlerFunc(apiHandler.RemoveDeviceFromGroup)))

	mux.Handle("GET /api/v1/productions", adminAuth(http.HandlerFunc(apiHandler.ListProductions)))
	mux.Handle("POST /api/v1/productions", adminAuth(http.HandlerFunc(apiHandler.CreateProduction)))
	mux.Handle("GET /api/v1/productions/{id}", adminAuth(http.HandlerFunc(apiHandler.GetProduction)))
	mux.Handle("DELETE /api/v1/productions/{id}", adminAuth(http.HandlerFunc(apiHandler.DeleteProduction)))

	// Releases and OTA packages — how a build machine publishes without a browser.
	// Same rules as the dashboard forms (see internal/api/releases.go).
	mux.Handle("GET /api/v1/releases", adminAuth(http.HandlerFunc(apiHandler.ListReleases)))
	mux.Handle("POST /api/v1/releases", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.CreateRelease))))
	mux.Handle("GET /api/v1/releases/{id}", adminAuth(http.HandlerFunc(apiHandler.GetRelease)))
	mux.Handle("POST /api/v1/releases/{id}/packages", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.AddReleasePackage))))
	mux.Handle("POST /api/v1/releases/{id}/publish", adminAuth(http.HandlerFunc(apiHandler.PublishRelease)))

	// Enrollment profiles — how a build machine mints the token an MDM-lite host app
	// bakes in at compile time (see internal/api/enrollment_profiles.go).
	mux.Handle("GET /api/v1/enrollment-profiles", adminAuth(http.HandlerFunc(apiHandler.ListEnrollmentProfiles)))
	mux.Handle("POST /api/v1/enrollment-profiles", adminAuth(middleware.MaxBytes(16<<10, http.HandlerFunc(apiHandler.CreateEnrollmentProfile))))
	mux.Handle("POST /api/v1/devices/enrolled-by", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.SetEnrolledBy))))
	// The fleet adb key (internal/api/fleet_adb_key.go): upload / metadata / remove.
	mux.Handle("PUT /api/v1/fleet-adb-key", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.PutFleetAdbKey))))
	mux.Handle("GET /api/v1/fleet-adb-key", adminAuth(http.HandlerFunc(apiHandler.GetFleetAdbKey)))
	mux.Handle("DELETE /api/v1/fleet-adb-key", adminAuth(http.HandlerFunc(apiHandler.DeleteFleetAdbKey)))
	// The same three, for one vendor's key, plus the list.
	mux.Handle("PUT /api/v1/fleet-adb-key/{label}", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.PutFleetAdbKey))))
	mux.Handle("GET /api/v1/fleet-adb-key/{label}", adminAuth(http.HandlerFunc(apiHandler.GetFleetAdbKey)))
	mux.Handle("DELETE /api/v1/fleet-adb-key/{label}", adminAuth(http.HandlerFunc(apiHandler.DeleteFleetAdbKey)))
	mux.Handle("GET /api/v1/fleet-adb-keys", adminAuth(http.HandlerFunc(apiHandler.ListFleetAdbKeys)))

	// Deployments: cancelling one was dashboard-only, which left a stalled rollout
	// unclearable without a browser (see internal/api/releases.go).
	mux.Handle("POST /api/v1/deployments/{id}/cancel", adminAuth(http.HandlerFunc(apiHandler.CancelDeployment)))

	// Commands
	mux.Handle("GET /api/v1/commands", adminAuth(http.HandlerFunc(apiHandler.ListCommands)))
	mux.Handle("POST /api/v1/commands", adminAuth(http.HandlerFunc(apiHandler.CreateCommand)))
	mux.Handle("GET /api/v1/commands/{id}", adminAuth(http.HandlerFunc(apiHandler.GetCommandStatus)))

	database.SetCheckinSampleSec(cfg.CheckinSampleSec())
	dash := dashboard.NewHandler(database, hub, shellMgr, remoteMgr, logMgr, sessionSecret, cfg, adminAPIKey, os.Getenv("GOOGLE_MAPS_EMBED_API_KEY"), geo, geocoder)
	apiHandler.SetKioskRelocker(dash.RelockFromDevice)
	apiHandler.SetAPKPuller(dash)
	// Test-bench routes (admin key): lock/unlock one device, take one off a venue.
	mux.Handle("POST /api/v1/devices/{serial}/kiosk", adminAuth(http.HandlerFunc(dash.APISetDeviceKiosk)))
	mux.Handle("POST /api/v1/devices/{serial}/brightness", adminAuth(http.HandlerFunc(dash.APISetDeviceBrightness)))
	mux.Handle("POST /api/v1/devices/{serial}/wlc", adminAuth(http.HandlerFunc(dash.APISetDeviceWlc)))
	mux.Handle("DELETE /api/v1/restaurants/{id}/devices/{serial}", adminAuth(http.HandlerFunc(apiHandler.UnassignRestaurantDevice)))
	dash.SetAdbTunnels(tunnelMgr)
	dash.SetIngestStats(apiHandler.IngestStats)
	dash.SetKeyResetHook(apiHandler.ForgetOwnKey)
	dash.SetScout(scoutSvc)
	// AIO backend (23 Oct 2026): restaurants and devices from the restaurant backend's
	// Metabase, copied by the backend-sync job. Metabase db 3 = AIO Prod, 4 = AIO Stage.
	if mb := aiobackend.New(os.Getenv("AIO_BACKEND_METABASE_URL"), os.Getenv("AIO_BACKEND_METABASE_KEY"),
		map[aiobackend.Env]int{aiobackend.Prod: 3, aiobackend.Stage: 4}); mb != nil {
		dash.SetBackend(mb)
		log.Println("AIO backend enabled (Metabase)")
	}

	// The DPC agent APK this server hosts: what a factory-reset device downloads
	// during QR provisioning, and what an "Update agent" command installs. Posting
	// it here is how a build publishes a new agent without anyone opening Settings.
	// Peer endpoints: another MDM asking, or telling, where a device is reporting.
	// Authenticated by the peer key, which is not the admin key — see internal/api/peer.go.
	// Peer administration (admin key): configure the neighbours, test them, force a
	// reconciliation pass. No dashboard form — see internal/api/peers_admin.go.
	mux.Handle("GET /api/v1/peers", adminAuth(http.HandlerFunc(apiHandler.ListPeers)))
	mux.Handle("PUT /api/v1/peers", adminAuth(middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.SetPeers))))
	mux.Handle("POST /api/v1/peers/ping", adminAuth(http.HandlerFunc(apiHandler.PingPeers)))
	mux.Handle("POST /api/v1/peers/sweep", adminAuth(http.HandlerFunc(apiHandler.SweepPeers)))

	mux.Handle("POST /api/v1/peer/ping", http.HandlerFunc(apiHandler.PeerPing))
	mux.Handle("POST /api/v1/peer/device-seen", middleware.MaxBytes(16<<10, http.HandlerFunc(apiHandler.PeerDeviceSeen)))
	mux.Handle("POST /api/v1/peer/lookup", middleware.MaxBytes(256<<10, http.HandlerFunc(apiHandler.PeerLookup)))

	mux.Handle("POST /api/v1/agent-apk",
		adminAuth(middleware.MaxBytes(128<<20, http.HandlerFunc(dash.AgentAPKPublish))))
	dash.RegisterRoutes(mux)

	// Back-fill the agent-APK archive with whatever each slot hosts now, so the current
	// build of every client is downloadable and rollback-able from this boot onwards.
	if n, err := dashboard.SeedAgentAPKArchive(cfg); err != nil {
		log.Printf("[startup] seed agent APK archive: %v", err)
	} else if n > 0 {
		log.Printf("[startup] archived %d hosted agent APK(s)", n)
	}

	// bgCtx is cancelled on shutdown so the background loops below stop cleanly.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Each periodic job runs once across server copies (scale plan W2b): a tick runs only
	// in the copy that holds the job's lease in job_leases. This copy's name is unique per
	// process, so a restarted copy never mistakes an old lease for its own.
	jobs.Default.Lease = database
	{
		host, _ := os.Hostname()
		jobs.Default.Holder = fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano())
	}
	// Several copies sharing the fleet (scale plan W5): CLUSTER=1 joins this copy, under the
	// same unique name, before any device can connect. CLUSTER_ADDR is how the other copies
	// reach it on the private network.
	if os.Getenv("CLUSTER") == "1" {
		cl := cluster.New(database, hub, jobs.Default.Holder, os.Getenv("CLUSTER_ADDR"))
		hub.SetCluster(cl)
		cl.Publish()
		cl.Start(bgCtx)
	}

	// One metrics sample every 10s: request rate, check-in rate, live sockets, pool
	// pressure and Go's own memory. Cheap (a ReadMemStats and a pool Stat), bounded
	// (an hour of points in a ring), and never written to Postgres — the database is
	// one of the things being measured.
	safego("metrics-sample", func() {
		t := time.NewTicker(metrics.SampleEvery)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				ps := database.PoolStats()
				metrics.Default.SampleNow(len(hub.ConnectedIDs()),
					int32(ps["acquired_conns"]), int32(ps["max_conns"]), ps["empty_acquire_count"])
			}
		}
	})

	// Peer traffic: the outbox delivers arrival announcements, the sweep asks peers
	// about devices we have stopped hearing from. The sweep is what makes this
	// self-correcting — a lost announcement costs nothing, because the question gets
	// asked again on a schedule.
	safego("peer-outbox", func() { apiHandler.Peers().RunOutbox(bgCtx, time.Minute) })
	safego("peer-sweep", func() { apiHandler.Peers().RunSweep(bgCtx, 10*time.Minute) })
	safego("scout-scan", func() { scoutSvc.Run(bgCtx) })

	// One-time: fill the power/usage columns on days rolled up before they existed, so
	// the restaurant metrics cover a full week from the first deploy.
	safego("backfill-site-metrics", func() {
		if n, err := database.BackfillSiteMetrics(bgCtx, 14); err != nil {
			log.Printf("[startup] backfill site metrics: %v", err)
		} else if n > 0 {
			log.Printf("[startup] backfilled power/usage metrics for %d day(s)", n)
		}
	})

	// Move legacy JSONB access rules into access_grants (one-time; see MigrateAccessRules).
	safego("migrate-access-rules", func() {
		if moved, dropped, err := database.MigrateAccessRules(bgCtx); err != nil {
			log.Printf("[startup] migrate access rules: %v", err)
		} else if moved > 0 || dropped > 0 {
			log.Printf("[startup] access rules migrated: %d moved, %d dropped", moved, dropped)
		}
	})
	// Temporary grants: drop the expired ones every minute (they already stop
	// applying at their expiry; this keeps the table and the editor tidy).
	jobs.Default.Every(bgCtx, "sweep-access-grants", time.Minute, time.Minute, func(ctx context.Context) {
		gone, err := database.SweepExpiredAccessGrants(ctx)
		if err != nil {
			log.Printf("[access] sweep expired grants: %v", err)
		}
		for _, g := range gone {
			_ = database.InsertAudit(ctx, "system", "user.access.expired", g.UserID.String(), g.Effect+" "+strings.Join(g.Actions, ",")+" @"+g.ScopeType+" "+g.ScopeName)
		}
	})

	// Attribute authorless commands from the audit log (see BackfillCommandAuthors).
	safego("backfill-command-authors", func() {
		if n, err := database.BackfillCommandAuthors(bgCtx); err != nil {
			log.Printf("[startup] backfill command authors: %v", err)
		} else if n > 0 {
			log.Printf("[startup] attributed %d authorless command(s) from the audit log", n)
		}
	})

	// One-time seed of battery discharge-cycle counters (from pre-aggregated stats).
	safego("backfill-discharge-cycles", func() {
		if n, err := database.BackfillDischargeCycles(bgCtx); err != nil {
			log.Printf("[startup] backfill discharge cycles: %v", err)
		} else if n > 0 {
			log.Printf("[startup] backfilled discharge cycles for %d device(s)", n)
		}
	})

	// Periodic housekeeping: daily-stats rollup + retention pruning (hourly, and at start).
	jobs.Default.EveryNow(bgCtx, "housekeeping", time.Hour, 10*time.Minute, dash.RunHousekeeping)

	// Legacy OTA discovery config: republish ota_config.json when the rolling reboot
	// window moves, so otautil never reboots a device the MDM is about to reboot itself.
	jobs.Default.EveryNow(bgCtx, "ota-config-publish", time.Hour, time.Minute, dash.PublishOTAConfig)

	// The minute jobs (scalability plan W2a, 29 Oct 2026). Until then all of these ran one
	// after another in one ticker: a slow job delayed the rest, and once the loop passed a
	// minute the ticker dropped ticks for every job. Now each runs on its own ticker
	// (jobs.Every: staggered start, overruns logged and counted in /debug/vars "jobs").
	// Jobs that must keep their order, or that act on the same devices and check each
	// other's work, stay together in one group and run in sequence as before.
	every := func(name string, interval time.Duration, fn func(context.Context)) {
		jobs.Default.Every(bgCtx, name, interval, interval+5*time.Minute, fn)
	}
	// Command lifecycle, in order: due reboots, re-drives, stall sweeps, shell deadlines.
	every("commands", time.Minute, func(ctx context.Context) {
		runJob(ctx, "scheduled-reboots", time.Minute, apiHandler.ProcessDueScheduledReboots)
		// Re-issue reboots that were sent but never applied, so an installed-but-
		// unrebooted device can't keep its deployment 'active' forever.
		runJob(ctx, "redrive-stuck-reboots", time.Minute, apiHandler.RedriveStuckReboots)
		// Fail install-shaped deliveries (install_apk, app_update) whose
		// download/install stalled (device lost connectivity mid-install, terminal
		// ack lost) so they don't sit "in flight" forever — install commands are
		// exempt from the short TTL, and an un-expirable one does not just sit
		// there: it blocks every later command for that device. FW-2026-000020
		runJob(ctx, "expire-stalled-installs", time.Minute, apiHandler.ExpireStalledInstalls)
		// Same, for OTA: a download that stops acking leaves the deployment active
		// and blocks the reboot that would clear it (RebootBlockedFor).
		runJob(ctx, "expire-stalled-otas", time.Minute, apiHandler.ExpireStalledOTAs)
		// A shell line that hangs on the device must not hold its queue: fail it
		// once it is past its deadline (counted from when the device got it).
		runJob(ctx, "fail-overdue-shell", time.Minute, apiHandler.FailOverdueShellCommands)
		// Re-push commands (any type) wedged at 'delivered' on a half-open socket to
		// devices that are connected now, so an action can't silently never run.
		runJob(ctx, "redrive-stuck-deliveries", time.Minute, apiHandler.RedriveStuckDeliveries)
		// Safety net for the WS-connect flush itself failing (e.g. a transient DB
		// error mid-recovery/restart): re-attempt delivery for every currently
		// connected device. A command with nothing pending is a cheap no-op query,
		// so this is safe to run every tick rather than only on the connect event
		// that may have failed. Closes the gap RedriveStuckDeliveries can't: that
		// job only re-drives commands that reached 'delivered' at least once — a
		// flush that errored before ever pushing leaves no command_status row at
		// all, so it never entered that job's set.
		runJob(ctx, "redrive-unflushed", time.Minute, func(ctx context.Context) {
			for id := range hub.ConnectedIDs() {
				apiHandler.FlushPendingCommands(ctx, id)
			}
		})
	})
	// NOTE: expire-overdue-commands is intentionally NOT scheduled — the per-device
	// queue does not expire for now (a queued command runs whenever the device next
	// comes online). The ExpireOverdueCommands handler is kept for when per-type
	// expiry is layered back on.
	// Prove we can still command the fleet, rather than inferring it from a
	// socket: a daily no-op per device whose terminal ack stamps
	// last_round_trip_ok_at (opt-in, config command_canary), and an alert for
	// devices that are reporting with nothing we sent ever completing.
	every("canary", time.Minute, func(ctx context.Context) {
		runJob(ctx, "command-canary", time.Minute, apiHandler.RunCommandCanary)
		runJob(ctx, "control-stale-alerts", time.Minute, apiHandler.RunControlStaleAlerts)
	})
	// Recent-tier alert rules (point-in-time + rate/sustained); see Tier 5 §10.
	every("recent-alerts", time.Minute, dash.RunRecentAlerts)
	// Fire any scheduled recipes whose cron time has arrived.
	every("scheduled-recipes", time.Minute, dash.ProcessDueScheduledRecipes)
	// What goes onto devices, in order, because these act on the same devices and check
	// each other's work: the client update waits for an app install or an OTA in flight
	// and the role app install for a client update (db.InFlightInstalls); run side by
	// side, two of them could each see the device idle in the same second.
	every("rollouts", time.Minute, func(ctx context.Context) {
		// Keep each client slot on the build this server hosts, for the slots
		// where that was switched on. A device that was away collects it the
		// first minute it is seen again — see client_autoupdate.go.
		runJob(ctx, "client-auto-update", time.Minute, dash.RunClientAutoUpdate)
		// Install the product's prod app (Lineup → role → App) on online devices
		// that lack it, at a venue or not — see dashboard/role_app_install.go.
		runJob(ctx, "role-app-install", time.Minute, dash.RunRoleAppInstalls)
		// The role model (static/device-role-model-plan.html): close lab sessions
		// whose time is up, then keep each device on its role's app build. Both
		// act only where a lab session or a prod build exists.
		runJob(ctx, "lab-expiry", time.Minute, dash.RunLabExpiry)
		runJob(ctx, "role-firmware", time.Minute, dash.RunRoleFirmware)
		runJob(ctx, "lock-rules", time.Minute, dash.RunLockRules)
		runJob(ctx, "role-alerts", time.Minute, dash.RunRoleAlerts)
	})
	jobs.Default.Every(bgCtx, "apk-auto-pull", time.Minute, 5*time.Minute, dash.RunAPKAutoPull)
	jobs.Default.Every(bgCtx, "backend-sync", time.Minute, 5*time.Minute, dash.RunBackendSync)
	// Record when each device goes online and offline, for the network graph's
	// online strip (dashboard/presence_record.go).
	every("presence", time.Minute, dash.RunPresenceRecord)
	// Give every device at a venue the venue's Wi-Fi networks, and have them
	// forget removed ones (venue_wifi.go).
	every("venue-wifi", time.Minute, dash.RunVenueWifi)
	// Hold every device at its brightness: Tableside AI at 122, locked, unless
	// one has its own level; re-sends the config to devices that drifted
	// (dashboard/brightness_relock.go).
	jobs.Default.Every(bgCtx, "brightness-relock", time.Minute, 10*time.Minute, dash.RunBrightnessRelock)
	// Place the Wi-Fi scans lost or departed devices kept while out of reach
	// (lost-device reporting, C) — see dashboard/lost_reporting.go.
	every("lost-trail", time.Minute, dash.RunLostTrail)
	// Score every device against the compliance baseline and overrides; only
	// changed results are written, each change kept as an event (compliance.go).
	every("compliance", time.Minute, dash.RunCompliance)
	// The monthly device-security report to each venue's contact and owners, on
	// the 1st from 09:00 venue time — only while customer compliance reports are
	// switched on, once per venue per month (compliance_report_send.go).
	every("compliance-reports", time.Minute, dash.RunComplianceReports)
	// Telemetry tables by month (W3, PARTITION_TELEMETRY=1): convert, keep months ahead,
	// drop the old copies. A conversion copies history, so its timeout is long.
	jobs.Default.Every(bgCtx, "partitions", time.Hour, 2*time.Hour, dash.RunPartitions)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: middleware.AccessLog(middleware.CompressHTML(middleware.SecurityHeaders(middleware.DecompressRequest(dash.NotFoundMiddleware(mux))))),
		// ReadHeaderTimeout bounds a slow header send (slowloris) without breaking the
		// long-lived WS/SSE endpoints: after the WS upgrade the conn is hijacked, so the
		// server's read/write timeouts no longer apply. No global WriteTimeout for the
		// same reason (it would kill streaming responses).
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown: on SIGTERM/SIGINT (docker stop / rolling deploy), stop
	// accepting, drain in-flight requests, close device WebSockets cleanly so devices
	// back off instead of stampeding the new container, stop background loops, then
	// the deferred database.Close() drains the pool.
	shutCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Server listening on :%s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Legacy OTA listener: the otautil system app on pre-agent builds has this host
	// and port baked in. Only its routes live here (internal/api/legacy_ota.go).
	var legacyServer *http.Server
	if lp := getEnv("LEGACY_OTA_PORT", ""); lp != "" {
		apiHandler.SetLegacyOTAUpstream(getEnv("LEGACY_OTA_UPSTREAM", "http://host.docker.internal:8001"))
		legacyServer = &http.Server{
			Addr:              ":" + lp,
			Handler:           middleware.AccessLog(apiHandler.LegacyOTAMux()),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			log.Printf("Legacy OTA listening on :%s (mode %s)", lp, cfg.LegacyOTAMode())
			if err := legacyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("legacy OTA server error: %v", err)
			}
		}()
	}

	<-shutCtx.Done()

	// Close what never closes itself, FIRST. Shutdown waits for active requests to
	// finish, and an SSE stream is an active request that runs until the client goes
	// away — so doing this after Shutdown meant every deploy sat here for the full
	// timeout with nothing listening on :8082, which is where the 502s came from. The
	// container itself starts in under a second; this wait was the outage.
	//
	// Nothing is lost by closing early: browsers re-open an SSE stream on their own,
	// and devices get a close frame so they back off and reconnect to the new
	// container instead of stampeding it.
	log.Println("shutdown: closing streams and device sockets…")
	dashboard.BeginDrain() // SSE handlers return
	hub.CloseAll()         // close frames to every device socket

	log.Println("shutdown: draining in-flight requests…")
	// Ordinary requests only now, so this finishes in milliseconds. The timeout is the
	// backstop for a genuinely stuck handler, not the expected path.
	sdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if legacyServer != nil {
		_ = legacyServer.Shutdown(sdCtx)
	}
	if err := server.Shutdown(sdCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// No request can queue history any more, so write out what is buffered while the
	// pool is still open. A second at most in practice; the timeout keeps this plus the
	// drain above inside compose's 15 s stop_grace_period, past which Docker kills us
	// mid-write.
	log.Println("shutdown: writing queued check-in history…")
	ingCtx, ingCancel := context.WithTimeout(context.Background(), 4*time.Second)
	if err := apiHandler.CloseIngest(ingCtx); err != nil {
		log.Printf("shutdown: ingest queue: %v", err)
	}
	ingCancel()
	bgCancel() // stop the background ticker loops
	log.Println("shutdown: complete")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// recoverLog is a deferred panic guard for goroutines that net/http does NOT
// protect (background loops, the WS read-loop dispatch). Without it, a panic on one
// device's malformed frame — or in a ticker job — crashes the whole process and
// drops all 900 connections.
func recoverLog(what string) {
	if r := recover(); r != nil {
		log.Printf("[panic] %s: %v\n%s", what, r, debug.Stack())
	}
}

// safego runs fn in a panic-recovering goroutine.
func safego(name string, fn func()) {
	go func() {
		defer recoverLog(name)
		fn()
	}()
}

// runJob runs a periodic-job function with a timeout and panic recovery, so a hung
// query or a panic in one tick can neither wedge the ticker loop forever nor crash
// the process.
//
// Every run is recorded in jobs.Default (runs, durations, timeouts, panics), which
// /debug/vars publishes as "jobs".
func runJob(ctx context.Context, name string, timeout time.Duration, fn func(context.Context)) {
	jobs.Default.Run(ctx, name, timeout, fn)
}
