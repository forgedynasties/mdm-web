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
	"mdm/internal/api"
	"mdm/internal/config"
	"mdm/internal/dashboard"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/logstream"
	"mdm/internal/middleware"
	"mdm/internal/remote"
	"mdm/internal/safehttp"
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
	ctx := context.Background()

	port := getEnv("PORT", "8080")
	dbHost := getEnv("DB_HOST", "localhost")
	dbPort := getEnv("DB_PORT", "5432")
	dbUser := getEnv("DB_USER", "mdm")
	dbPass := getEnv("DB_PASSWORD", "mdm")
	dbName := getEnv("DB_NAME", "mdm")
	deviceAPIKey := mustEnv("DEVICE_API_KEY")
	adminAPIKey := mustEnv("ADMIN_API_KEY")
	dashUser := getEnv("DASHBOARD_USER", "admin")
	dashPass := mustEnv("DASHBOARD_PASSWORD")
	// SESSION_SECRET signs the dashboard session cookie. It must be its OWN secret:
	// the previous fallback to DEVICE_API_KEY meant the cookie-signing key was the
	// fleet-wide key every managed device holds, so a single extracted device key
	// handed an attacker the dashboard's cookie integrity key. No fallback now.
	sessionSecret := mustEnv("SESSION_SECRET")
	if len(sessionSecret) < 32 {
		log.Fatalf("SESSION_SECRET must be at least 32 bytes for secure session cookie signing")
	}
	if sessionSecret == deviceAPIKey || sessionSecret == adminAPIKey {
		log.Fatalf("SESSION_SECRET must be distinct from DEVICE_API_KEY and ADMIN_API_KEY")
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
	shellMgr.OnCommandDone = func(deviceID, commandID uuid.UUID, exitCode int) {
		status := "completed"
		if exitCode != 0 {
			status = "failed"
		}
		ctx := context.Background()
		if err := database.AckCommand(ctx, commandID, deviceID, status); err != nil {
			if !errors.Is(err, db.ErrCommandNotTargeted) {
				log.Printf("OnCommandDone: AckCommand error: %v", err)
			}
			return
		}
		hub.PublishCommandUpdate(commandID)
	}
	remoteMgr := remote.New(hub)
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
	apiHandler := api.NewHandler(database, hub, shellMgr, cfg, geo, geocoder, remoteMgr, adminAPIKey)
	// Flush queued commands the moment a device's WS registers (socket writable). The HTTP
	// /connect flush can fire before the socket opens, and a never-delivered command has
	// nothing else to re-trigger it, so it would sit in the queue until the next reconnect.
	hub.SetOnConnect(func(deviceID uuid.UUID) {
		defer recoverLog("ws onConnect flush for " + deviceID.String())
		apiHandler.FlushPendingCommands(context.Background(), deviceID)
	})
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
		case "logcat_stream", "logcat_stream_end":
			logMgr.HandleDeviceMessage(raw)
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
	deviceAuth := func(h http.Handler) http.Handler { return middleware.DeviceAuth(deviceAPIKey, deviceKeyLookup, h) }
	adminAuth := func(h http.Handler) http.Handler { return middleware.AdminAPIKeyAuth(adminAPIKey, h) }
	// maxDeviceBody caps device POST bodies (post-inflation). Check-ins carry the
	// installed-app list and a logcat result can be sizable, so it's generous, but
	// it stops a single device from exhausting memory with an unbounded body.
	const maxDeviceBody = 8 << 20 // 8 MiB
	devicePost := func(h http.HandlerFunc) http.Handler { return deviceAuth(middleware.MaxBytes(maxDeviceBody, h)) }

	// Metrics for monitoring (admin-gated): db pool, ws connected, goroutines, request
	// counters. Scrape as JSON; not exposed to devices or the public.
	mux.Handle("GET /debug/vars", adminAuth(expvar.Handler()))

	// WebSocket — device connects here for server-push command delivery
	mux.Handle("GET /api/v1/ws", deviceAuth(http.HandlerFunc(apiHandler.Connect)))

	// Enrollment — unauthenticated by design: the profile token IS the credential
	// (rate-limited per IP inside the handler). Exchanges a token for a device key.
	mux.Handle("POST /api/v1/enroll", middleware.MaxBytes(64<<10, http.HandlerFunc(apiHandler.Enroll)))

	// Device-authenticated endpoints (body-size limited)
	mux.Handle("POST /api/v1/checkin", devicePost(apiHandler.Checkin))
	mux.Handle("POST /api/v1/commands/{id}/ack", devicePost(apiHandler.AckCommand))
	mux.Handle("POST /api/v1/logcat", devicePost(apiHandler.SubmitLogcat))
	mux.Handle("POST /api/v1/ota/status", devicePost(apiHandler.OtaStatus))
	mux.Handle("POST /api/v1/ota/progress", devicePost(apiHandler.OtaProgress))

	// Admin-authenticated API endpoints
	mux.Handle("GET /api/v1/devices", adminAuth(http.HandlerFunc(apiHandler.ListDevices)))
	mux.Handle("GET /api/v1/devices/{serial}", adminAuth(http.HandlerFunc(apiHandler.GetDevice)))
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

	// Commands
	mux.Handle("GET /api/v1/commands", adminAuth(http.HandlerFunc(apiHandler.ListCommands)))
	mux.Handle("POST /api/v1/commands", adminAuth(http.HandlerFunc(apiHandler.CreateCommand)))
	mux.Handle("GET /api/v1/commands/{id}", adminAuth(http.HandlerFunc(apiHandler.GetCommandStatus)))

	database.SetCheckinSampleSec(cfg.CheckinSampleSec())
	dash := dashboard.NewHandler(database, hub, shellMgr, remoteMgr, logMgr, sessionSecret, dashUser, dashPass, cfg, adminAPIKey, os.Getenv("GOOGLE_MAPS_EMBED_API_KEY"), geo, geocoder)
	dash.RegisterRoutes(mux)

	// bgCtx is cancelled on shutdown so the background loops below stop cleanly.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// One-time backfill of daily stats for any historical days not yet rolled up.
	safego("backfill-daily-stats", func() {
		if n, err := database.BackfillDailyStats(bgCtx, cfg.CheckinRetentionDays()); err != nil {
			log.Printf("[startup] backfill daily stats: %v", err)
		} else if n > 0 {
			log.Printf("[startup] backfilled daily stats for %d day(s)", n)
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
	safego("sweep-access-grants", func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				gone, err := database.SweepExpiredAccessGrants(bgCtx)
				if err != nil {
					log.Printf("[access] sweep expired grants: %v", err)
				}
				for _, g := range gone {
					_ = database.InsertAudit(bgCtx, "system", "user.access.expired", g.UserID.String(), g.Effect+" "+strings.Join(g.Actions, ",")+" @"+g.ScopeType+" "+g.ScopeName)
				}
			}
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

	// Periodic housekeeping: daily-stats rollup + retention pruning (hourly).
	safego("housekeeping-loop", func() {
		runJob(bgCtx, "housekeeping", 10*time.Minute, dash.RunHousekeeping)
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				runJob(bgCtx, "housekeeping", 10*time.Minute, dash.RunHousekeeping)
			}
		}
	})

	// Minute dispatcher: scheduled reboots + recent-tier alerts + scheduled recipes.
	safego("minute-loop", func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				runJob(bgCtx, "scheduled-reboots", time.Minute, apiHandler.ProcessDueScheduledReboots)
				// Re-issue reboots that were sent but never applied, so an installed-but-
				// unrebooted device can't keep its deployment 'active' forever.
				runJob(bgCtx, "redrive-stuck-reboots", time.Minute, apiHandler.RedriveStuckReboots)
				// Fail install_apk deliveries whose download/install stalled (device lost
				// connectivity mid-install, terminal ack lost) so they don't sit "in
				// flight" forever — install commands are exempt from the short TTL. FW-2026-000020
				runJob(bgCtx, "expire-stalled-installs", time.Minute, apiHandler.ExpireStalledInstalls)
				// Re-push commands (any type) wedged at 'delivered' on a half-open socket to
				// devices that are connected now, so an action can't silently never run.
				runJob(bgCtx, "redrive-stuck-deliveries", time.Minute, apiHandler.RedriveStuckDeliveries)
				// Safety net for the WS-connect flush itself failing (e.g. a transient DB
				// error mid-recovery/restart): re-attempt delivery for every currently
				// connected device. A command with nothing pending is a cheap no-op query,
				// so this is safe to run every tick rather than only on the connect event
				// that may have failed. Closes the gap RedriveStuckDeliveries can't: that
				// job only re-drives commands that reached 'delivered' at least once — a
				// flush that errored before ever pushing leaves no command_status row at
				// all, so it never entered that job's set.
				runJob(bgCtx, "redrive-unflushed", time.Minute, func(ctx context.Context) {
					for id := range hub.ConnectedIDs() {
						apiHandler.FlushPendingCommands(ctx, id)
					}
				})
				// NOTE: expire-overdue-commands is intentionally NOT scheduled — the per-device
				// queue does not expire for now (a queued command runs whenever the device next
				// comes online). The ExpireOverdueCommands handler is kept for when per-type
				// expiry is layered back on.
				// Recent-tier alert rules (point-in-time + rate/sustained); see Tier 5 §10.
				runJob(bgCtx, "recent-alerts", time.Minute, dash.RunRecentAlerts)
				// Fire any scheduled recipes whose cron time has arrived.
				runJob(bgCtx, "scheduled-recipes", time.Minute, dash.ProcessDueScheduledRecipes)
			}
		}
	})

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
	log.Println("shutdown: draining in-flight requests…")
	sdCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if legacyServer != nil {
		_ = legacyServer.Shutdown(sdCtx)
	}
	if err := server.Shutdown(sdCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	bgCancel()     // stop the background ticker loops
	hub.CloseAll() // send close frames to all device sockets
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
func runJob(ctx context.Context, name string, timeout time.Duration, fn func(context.Context)) {
	defer recoverLog("job " + name)
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fn(jctx)
}
