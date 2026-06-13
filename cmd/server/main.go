package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"mdm/internal/api"
	"mdm/internal/config"
	"mdm/internal/dashboard"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/middleware"
	"mdm/internal/remote"
	"mdm/internal/shell"
	"mdm/internal/ws"
)

var ()

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
	sessionSecret := getEnv("SESSION_SECRET", deviceAPIKey)
	configPath := getEnv("CONFIG_PATH", "config/display.json")

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

	hub := ws.NewHub()
	shellMgr := shell.NewManager()
	remoteMgr := remote.New(hub)
	hub.SetOnBinaryMessage(remoteMgr.RelayFrame)

	mux := http.NewServeMux()

	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Disable the default directory autoindex: any request resolving to a
		// directory (path ends in "/") is rejected so the static tree can't be
		// enumerated for dev-only artifacts.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		staticFS.ServeHTTP(w, r)
	}))

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

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	// Migrate a legacy single alert webhook into a default channel (one-time, no-op
	// if channels already exist or no legacy URL is set).
	if err := database.EnsureDefaultChannelFromWebhook(ctx, cfg.AlertWebhookURL()); err != nil {
		log.Printf("seed default alert channel: %v", err)
	}

	var geo *geolocate.Resolver
	if os.Getenv("GEOLOCATE_ENABLED") == "true" {
		geo = geolocate.New()
		log.Println("Geolocation resolver enabled (BeaconDB)")
	}
	apiHandler := api.NewHandler(database, hub, shellMgr, cfg, geo, remoteMgr, adminAPIKey)
	hub.SetOnMessage(func(deviceID uuid.UUID, raw []byte) {
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
		default:
			shellMgr.HandleDeviceMessage(deviceID, raw)
		}
	})

	deviceAuth := func(h http.Handler) http.Handler { return middleware.DeviceAPIKeyAuth(deviceAPIKey, h) }
	adminAuth := func(h http.Handler) http.Handler { return middleware.AdminAPIKeyAuth(adminAPIKey, h) }

	// WebSocket — device connects here for server-push command delivery
	mux.Handle("GET /api/v1/ws", deviceAuth(http.HandlerFunc(apiHandler.Connect)))

	// Device-authenticated endpoints
	mux.Handle("POST /api/v1/checkin", deviceAuth(http.HandlerFunc(apiHandler.Checkin)))
	mux.Handle("POST /api/v1/commands/{id}/ack", deviceAuth(http.HandlerFunc(apiHandler.AckCommand)))
	mux.Handle("POST /api/v1/logcat", deviceAuth(http.HandlerFunc(apiHandler.SubmitLogcat)))
	mux.Handle("POST /api/v1/ota/status", deviceAuth(http.HandlerFunc(apiHandler.OtaStatus)))

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

	dash := dashboard.NewHandler(database, hub, shellMgr, sessionSecret, dashUser, dashPass, cfg, adminAPIKey)
	dash.RegisterRoutes(mux)

	// One-time backfill of daily stats for any historical days not yet rolled up.
	// Runs in the background so it never blocks startup.
	go func() {
		if n, err := database.BackfillDailyStats(context.Background()); err != nil {
			log.Printf("[startup] backfill daily stats: %v", err)
		} else if n > 0 {
			log.Printf("[startup] backfilled daily stats for %d day(s)", n)
		}
	}()

	// Periodic housekeeping: daily-stats rollup + auto-hide stale devices + retention pruning.
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		dash.RunHousekeeping(context.Background())
		for range t.C {
			dash.RunHousekeeping(context.Background())
		}
	}()

	// OTA scheduled-reboot dispatcher: devices that installed an update under a
	// "scheduled" reboot policy get their reboot command once the time arrives.
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for range t.C {
			apiHandler.ProcessDueScheduledReboots(context.Background())
			// Recent-tier alert rules (point-in-time + rate/sustained); see Tier 5 §10.
			dash.RunRecentAlerts(context.Background())
		}
	}()

	server := &http.Server{
		Addr:        ":" + port,
		Handler:     middleware.SecurityHeaders(mux),
		IdleTimeout: 120 * time.Second,
	}

	log.Printf("Server listening on :%s", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
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
