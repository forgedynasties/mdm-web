package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"mdm/internal/api"
	"mdm/internal/config"
	"mdm/internal/dashboard"
	"mdm/internal/db"
	"mdm/internal/middleware"
	"mdm/internal/shell"
	"mdm/internal/ws"
)

func main() {
	ctx := context.Background()

	port          := getEnv("PORT", "8080")
	dbHost        := getEnv("DB_HOST", "localhost")
	dbPort        := getEnv("DB_PORT", "5432")
	dbUser        := getEnv("DB_USER", "mdm")
	dbPass        := getEnv("DB_PASSWORD", "mdm")
	dbName        := getEnv("DB_NAME", "mdm")
	deviceAPIKey  := mustEnv("DEVICE_API_KEY")
	adminAPIKey   := mustEnv("ADMIN_API_KEY")
	dashUser      := getEnv("DASHBOARD_USER", "admin")
	dashPass      := mustEnv("DASHBOARD_PASSWORD")
	sessionSecret := getEnv("SESSION_SECRET", deviceAPIKey)
	configPath    := getEnv("CONFIG_PATH", "config/display.json")

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

	hub := ws.NewHub()
	shellMgr := shell.NewManager()
	hub.SetOnMessage(shellMgr.HandleDeviceMessage)

	mux := http.NewServeMux()

	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		staticFS.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := database.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"error","db":"unreachable"}`))
			return
		}
		w.Write([]byte(`{"status":"ok","db":"ok"}`))
	})

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	apiHandler := api.NewHandler(database, hub, shellMgr, cfg)

	deviceAuth := func(h http.Handler) http.Handler { return middleware.DeviceAPIKeyAuth(deviceAPIKey, h) }
	adminAuth := func(h http.Handler) http.Handler { return middleware.AdminAPIKeyAuth(adminAPIKey, h) }

	// WebSocket — device connects here for server-push command delivery
	mux.Handle("GET /api/v1/ws", deviceAuth(http.HandlerFunc(apiHandler.Connect)))

	// Device-authenticated endpoints
	mux.Handle("POST /api/v1/checkin",               deviceAuth(http.HandlerFunc(apiHandler.Checkin)))
	mux.Handle("POST /api/v1/commands/{id}/ack",     deviceAuth(http.HandlerFunc(apiHandler.AckCommand)))
	mux.Handle("POST /api/v1/logcat",                deviceAuth(http.HandlerFunc(apiHandler.SubmitLogcat)))
	mux.Handle("POST /api/v1/ota/status",            deviceAuth(http.HandlerFunc(apiHandler.OtaStatus)))

	// Admin-authenticated API endpoints
	mux.Handle("GET /api/v1/devices",                adminAuth(http.HandlerFunc(apiHandler.ListDevices)))
	mux.Handle("GET /api/v1/devices/{serial}",       adminAuth(http.HandlerFunc(apiHandler.GetDevice)))

	// Groups
	mux.Handle("GET /api/v1/groups",                 adminAuth(http.HandlerFunc(apiHandler.ListGroups)))
	mux.Handle("POST /api/v1/groups",                adminAuth(http.HandlerFunc(apiHandler.CreateGroup)))
	mux.Handle("GET /api/v1/groups/{id}",            adminAuth(http.HandlerFunc(apiHandler.GetGroup)))
	mux.Handle("DELETE /api/v1/groups/{id}",         adminAuth(http.HandlerFunc(apiHandler.DeleteGroup)))
	mux.Handle("POST /api/v1/groups/{id}/devices",   adminAuth(http.HandlerFunc(apiHandler.AddDeviceToGroup)))
	mux.Handle("DELETE /api/v1/groups/{id}/devices/{serial}", adminAuth(http.HandlerFunc(apiHandler.RemoveDeviceFromGroup)))

	// Commands
	mux.Handle("GET /api/v1/commands",               adminAuth(http.HandlerFunc(apiHandler.ListCommands)))
	mux.Handle("POST /api/v1/commands",              adminAuth(http.HandlerFunc(apiHandler.CreateCommand)))
	mux.Handle("GET /api/v1/commands/{id}",          adminAuth(http.HandlerFunc(apiHandler.GetCommandStatus)))

	dash := dashboard.NewHandler(database, hub, shellMgr, sessionSecret, dashUser, dashPass, cfg)
	dash.RegisterRoutes(mux)

	server := &http.Server{
		Addr:        ":" + port,
		Handler:     mux,
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
