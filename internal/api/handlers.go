package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/remote"
	"mdm/internal/shell"
	"mdm/internal/ws"
)

type Handler struct {
	db        *db.DB
	hub       *ws.Hub
	shell     *shell.Manager
	cfg       *config.Config
	geolocate *geolocate.Resolver
	remote    *remote.Manager
}

func NewHandler(d *db.DB, hub *ws.Hub, shellMgr *shell.Manager, cfg *config.Config, geo *geolocate.Resolver, rm *remote.Manager) *Handler {
	return &Handler{db: d, hub: hub, shell: shellMgr, cfg: cfg, geolocate: geo, remote: rm}
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

	// Flush any commands that were queued while the device was offline.
	h.flushPendingCommands(r.Context(), device.ID)
	h.flushPendingLogcatRequests(r.Context(), device.ID)

	// Request telemetry immediately, then on a repeating interval.
	reqMsg, _ := json.Marshal(map[string]any{"type": "telemetry_request"})
	h.hub.Push(device.ID, reqMsg)
	go h.runTelemetryRequestLoop(device.ID)

	go client.WritePump()
	client.ReadPump() // blocks until connection closes
}

// runTelemetryRequestLoop sends a telemetry_request to the device on the
// configured checkin interval. Exits when the device disconnects (Push returns
// false). Re-reads the interval each cycle so config changes take effect.
func (h *Handler) runTelemetryRequestLoop(deviceID uuid.UUID) {
	msg, _ := json.Marshal(map[string]any{"type": "telemetry_request"})
	for {
		interval := time.Duration(h.cfg.CheckinInterval()) * time.Second
		if interval < 10*time.Second {
			interval = 30 * time.Second
		}
		time.Sleep(interval)
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
	serial := strings.TrimSpace(r.PathValue("serial"))
	if serial == "" {
		http.Error(w, "serial query parameter required", http.StatusBadRequest)
		return
	}

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	if !h.hub.IsConnected(device.ID) {
		http.Error(w, "device not connected", http.StatusServiceUnavailable)
		return
	}

	_, err = h.remote.Start(device.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	startMsg, _ := json.Marshal(map[string]any{
		"type":    "start_capture",
		"quality": 60,
		"scale":   0.5,
		"max_fps": 10,
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

	// Write pump: relay device frames → dashboard (binary)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for data := range frameCh {
			dashConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := dashConn.WriteMessage(2, data); err != nil {
				return
			}
		}
	}()

	// Read pump: dashboard input events → device (JSON)
	for {
		_, msg, err := dashConn.ReadMessage()
		if err != nil {
			break
		}
		h.remote.RelayInput(device.ID, msg)
	}

	dashConn.WriteMessage(websocket.CloseMessage, []byte{})
	<-done
}

// flushPendingCommands pushes pending commands to the device over WS and
// marks them delivered (or completed for reboot). Stops after a reboot
// command so that later commands remain pending and are flushed after the
// device reconnects post-reboot.
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
		if cmd.Type == "reboot" {
			_ = h.db.AckCommand(ctx, cmd.ID, deviceID, "completed")
			break // stop here; remaining cmds flush after reconnect
		}
		_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
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
		if cmd.Type == "reboot" {
			_ = h.db.AckCommand(ctx, cmd.ID, deviceID, "completed")
		} else {
			_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
		}
	}
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
	enriched, err := json.Marshal(m)
	if err != nil {
		return extra
	}
	return json.RawMessage(enriched)
}

// ── Checkin (telemetry only) ──────────────────────────────────────────────────

type checkinRequest struct {
	SerialNumber  string          `json:"serial_number"`
	BuildID       string          `json:"build_id"`
	BatteryPct    int             `json:"battery_pct"`
	Extra         json.RawMessage `json:"extra,omitempty"`
	InstalledApps []struct {
		Package     string `json:"package"`
		Name        string `json:"name"`
		VersionName string `json:"version_name"`
	} `json:"installed_apps,omitempty"`
}

func (h *Handler) Checkin(w http.ResponseWriter, r *http.Request) {
	var req checkinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.SerialNumber == "" || req.BuildID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and build_id are required"})
		return
	}
	if req.BatteryPct < 0 || req.BatteryPct > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "battery_pct must be 0-100"})
		return
	}

	req.Extra = h.enrichLocation(r.Context(), req.Extra)

	deviceID, _, err := h.db.UpsertCheckin(r.Context(), req.SerialNumber, req.BuildID, req.BatteryPct, req.Extra)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	log.Printf("[checkin] serial=%s packages_count=%d", req.SerialNumber, len(req.InstalledApps))
	if len(req.InstalledApps) > 0 {
		seen := make(map[string]struct{})
		var pkgs []db.DevicePackage
		for _, p := range req.InstalledApps {
			if _, dup := seen[p.Package]; dup {
				continue
			}
			seen[p.Package] = struct{}{}
			pkgs = append(pkgs, db.DevicePackage{PackageName: p.Package, AppName: p.Name, VersionName: p.VersionName})
		}
		if err := h.db.UpsertDevicePackages(r.Context(), deviceID, pkgs); err != nil {
			log.Printf("[checkin] UpsertDevicePackages error: %v", err)
		}
	}

	h.hub.PublishDeviceUpdate(deviceID)

	// OTA check: resolve update from update_devices table.
	if upd, err := h.db.ResolveUpdateForDevice(r.Context(), deviceID); err != nil {
		log.Printf("[checkin] ResolveUpdateForDevice error: %v", err)
	} else if upd != nil && upd.OtaPackage != nil {
		pkg := upd.OtaPackage
		// If the device is already on the target build, mark as installed
		if pkg.TargetBuildID == req.BuildID {
			_ = h.db.SetUpdateDeviceStatus(r.Context(), upd.ID, deviceID, "installed")
			_ = h.db.CheckAndCompleteUpdate(r.Context(), upd.ID)
		} else {
			// Check if an OTA command is already in flight
			if hasPending, err := h.db.HasPendingOTACommand(r.Context(), deviceID); err != nil {
				log.Printf("[checkin] HasPendingOTACommand error: %v", err)
			} else if !hasPending {
				applicable := true
				if pkg.Type == "incremental" {
					applicable = pkg.SourceBuildID == req.BuildID
				}
				if applicable {
					p := map[string]any{
						"package_id":      pkg.ID,
						"build_id":        pkg.TargetBuildID,
						"update_url":      pkg.UpdateURL,
						"reboot_behavior": upd.RebootBehavior,
					}
					if upd.ScheduledTime != nil {
						p["scheduled_time"] = upd.ScheduledTime.UTC().Format(time.RFC3339)
					}
					payload, _ := json.Marshal(p)
					if cmd, err := h.db.CreateCommand(r.Context(), "ota", "", payload, "devices", []uuid.UUID{deviceID}); err != nil {
						log.Printf("[checkin] create OTA command error: %v", err)
					} else {
						_ = h.db.SetUpdateDeviceStatus(r.Context(), upd.ID, deviceID, "downloading")
						h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{deviceID})
					}
				}
			}
		}
	}

	deviceCfg, err := h.db.GetOrCreateDeviceConfig(r.Context(), deviceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	log.Printf("[checkin] %s → kiosk_enabled=%v kiosk_package=%q kiosk_features=%d",
		req.SerialNumber, deviceCfg.KioskEnabled, deviceCfg.KioskPackage, deviceCfg.KioskFeatures)

	// Include pending commands in checkin response for backwards compatibility
	// with older clients that poll via checkin instead of WebSocket.
	var cmdList []map[string]any
	if h.cfg.LegacyCheckin() && !h.hub.IsConnected(deviceID) {
		if cmds, err := h.db.GetPendingCommandsForDevice(r.Context(), deviceID); err == nil {
			for _, cmd := range cmds {
				cmdList = append(cmdList, map[string]any{
					"id":      cmd.ID,
					"type":    cmd.Type,
					"apk_url": cmd.ApkURL,
					"payload": cmd.Payload,
				})
				if cmd.Type == "reboot" {
					_ = h.db.AckCommand(r.Context(), cmd.ID, deviceID, "completed")
					break // stop here; remaining cmds delivered on next checkin post-reboot
				}
				_ = h.db.MarkCommandsDelivered(r.Context(), deviceID, []uuid.UUID{cmd.ID})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"commands": cmdList,
		"config": map[string]any{
			"kiosk_enabled":            deviceCfg.KioskEnabled,
			"kiosk_package":            deviceCfg.KioskPackage,
			"kiosk_features":           deviceCfg.KioskFeatures,
			"checkin_interval_seconds": h.cfg.CheckinInterval(),
		},
	})
}

// HandleWsCommandAck processes a "command_ack" message from a device over WS.
func (h *Handler) HandleWsCommandAck(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var body struct {
		CommandID uuid.UUID `json:"command_id"`
		Status    string    `json:"status"`
		Output    string    `json:"output"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.CommandID == uuid.Nil {
		log.Printf("[ws-ack] parse error or missing command_id: %v", err)
		return
	}
	if body.Status != "installed" && body.Status != "failed" && body.Status != "completed" {
		log.Printf("[ws-ack] invalid status: %q", body.Status)
		return
	}
	if err := h.db.AckCommand(ctx, body.CommandID, deviceID, body.Status); err != nil {
		log.Printf("[ws-ack] AckCommand error: %v", err)
		return
	}
	if body.Output != "" {
		_ = h.db.SaveCommandResult(ctx, body.CommandID, deviceID, body.Output)
	}
	h.hub.PublishDeviceUpdate(deviceID)
	h.hub.PublishCommandUpdate(body.CommandID)
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
}

// HandleWsOtaStatus processes an "ota_status" message from a device over WS.
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
	}
	if body.Status == "installed" {
		if cmd, err := h.db.CreateCommand(ctx, "reboot", "", nil, "devices", []uuid.UUID{deviceID}); err != nil {
			log.Printf("[ws-ota] create reboot command error: %v", err)
		} else {
			h.pushCommand(ctx, cmd, "devices", []uuid.UUID{deviceID})
		}
	}
	h.hub.PublishDeviceUpdate(deviceID)
}

// HandleWsTelemetry processes a "telemetry" message sent by a device over the
// WebSocket connection. It performs the same upsert, OTA check, and config push
// as the HTTP Checkin handler, but returns config to the device over WS instead
// of an HTTP response body.
func (h *Handler) HandleWsTelemetry(deviceID uuid.UUID, raw []byte) {
	ctx := context.Background()
	var req checkinRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		log.Printf("[ws-telemetry] parse error: %v", err)
		return
	}
	if req.SerialNumber == "" || req.BuildID == "" {
		log.Printf("[ws-telemetry] missing serial_number or build_id")
		return
	}

	req.Extra = h.enrichLocation(ctx, req.Extra)

	id, _, err := h.db.UpsertCheckin(ctx, req.SerialNumber, req.BuildID, req.BatteryPct, req.Extra)
	if err != nil {
		log.Printf("[ws-telemetry] UpsertCheckin error: %v", err)
		return
	}

	log.Printf("[ws-telemetry] serial=%s packages_count=%d", req.SerialNumber, len(req.InstalledApps))
	if len(req.InstalledApps) > 0 {
		seen := make(map[string]struct{})
		var pkgs []db.DevicePackage
		for _, p := range req.InstalledApps {
			if _, dup := seen[p.Package]; dup {
				continue
			}
			seen[p.Package] = struct{}{}
			pkgs = append(pkgs, db.DevicePackage{PackageName: p.Package, AppName: p.Name, VersionName: p.VersionName})
		}
		if err := h.db.UpsertDevicePackages(ctx, id, pkgs); err != nil {
			log.Printf("[ws-telemetry] UpsertDevicePackages error: %v", err)
		}
	}

	h.hub.PublishDeviceUpdate(id)

	// OTA check — same logic as HTTP Checkin.
	if upd, err := h.db.ResolveUpdateForDevice(ctx, id); err != nil {
		log.Printf("[ws-telemetry] ResolveUpdateForDevice error: %v", err)
	} else if upd != nil && upd.OtaPackage != nil {
		pkg := upd.OtaPackage
		if pkg.TargetBuildID == req.BuildID {
			_ = h.db.SetUpdateDeviceStatus(ctx, upd.ID, id, "installed")
			_ = h.db.CheckAndCompleteUpdate(ctx, upd.ID)
		} else {
			if hasPending, err := h.db.HasPendingOTACommand(ctx, id); err != nil {
				log.Printf("[ws-telemetry] HasPendingOTACommand error: %v", err)
			} else if !hasPending {
				applicable := true
				if pkg.Type == "incremental" {
					applicable = pkg.SourceBuildID == req.BuildID
				}
				if applicable {
					p := map[string]any{
						"package_id":      pkg.ID,
						"build_id":        pkg.TargetBuildID,
						"update_url":      pkg.UpdateURL,
						"reboot_behavior": upd.RebootBehavior,
					}
					if upd.ScheduledTime != nil {
						p["scheduled_time"] = upd.ScheduledTime.UTC().Format(time.RFC3339)
					}
					payload, _ := json.Marshal(p)
					if cmd, err := h.db.CreateCommand(ctx, "ota", "", payload, "devices", []uuid.UUID{id}); err != nil {
						log.Printf("[ws-telemetry] create OTA command error: %v", err)
					} else {
						_ = h.db.SetUpdateDeviceStatus(ctx, upd.ID, id, "downloading")
						h.pushCommand(ctx, cmd, "devices", []uuid.UUID{id})
					}
				}
			}
		}
	}

	deviceCfg, err := h.db.GetOrCreateDeviceConfig(ctx, id)
	if err != nil {
		log.Printf("[ws-telemetry] GetOrCreateDeviceConfig error: %v", err)
		return
	}

	cfgMsg, _ := json.Marshal(map[string]any{
		"type":                     "config",
		"kiosk_enabled":            deviceCfg.KioskEnabled,
		"kiosk_package":            deviceCfg.KioskPackage,
		"kiosk_features":           deviceCfg.KioskFeatures,
		"checkin_interval_seconds": h.cfg.CheckinInterval(),
	})
	h.hub.Push(deviceID, cfgMsg)
}

// ── Logcat ────────────────────────────────────────────────────────────────────

func (h *Handler) SubmitLogcat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SerialNumber string    `json:"serial_number"`
		RequestID    uuid.UUID `json:"request_id"`
		Content      string    `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.SerialNumber == "" || body.RequestID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and request_id are required"})
		return
	}

	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	if _, err := h.db.SaveLogcatResult(r.Context(), body.RequestID, device.ID, body.Content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishLogcatUpdate(device.ID)

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

func (h *Handler) GetDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	checkins, err := h.db.GetCheckins(r.Context(), device.ID, 100)
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
	validTypes := map[string]bool{"install_apk": true, "shell": true, "screenshot": true, "reboot": true, "ota": true}
	if !validTypes[body.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid type"})
		return
	}
	if body.Type == "install_apk" && body.ApkURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "apk_url is required for install_apk"})
		return
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

	cmd, err := h.db.CreateCommand(r.Context(), body.Type, body.ApkURL, body.Payload, body.TargetType, targetIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.pushCommand(r.Context(), cmd, body.TargetType, targetIDs)

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
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id)
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
		Status       string `json:"status"` // installed | failed | completed
		Output       string `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number is required"})
		return
	}
	if body.Status != "installed" && body.Status != "failed" && body.Status != "completed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be installed, failed, or completed"})
		return
	}

	device, err := h.db.GetDevice(r.Context(), body.SerialNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	if err := h.db.AckCommand(r.Context(), cmdID, device.ID, body.Status); err != nil {
		if errors.Is(err, db.ErrCommandNotTargeted) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "command does not target device"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if body.Output != "" {
		_ = h.db.SaveCommandResult(r.Context(), cmdID, device.ID, body.Output)
	}
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishCommandUpdate(cmdID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) OtaStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SerialNumber string    `json:"serial_number"`
		CommandID    uuid.UUID `json:"command_id"`
		Status       string    `json:"status"`     // downloaded | installed | error
		ErrorCode    string    `json:"error_code"` // optional, set when status=error
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and command_id are required"})
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

	// Clear in-memory OTA progress on terminal statuses.
	if body.Status == "installed" || body.Status == "error" {
		h.shell.ClearOTAProgress(device.ID)
	}

	// On installed: push a reboot command immediately via WS.
	if body.Status == "installed" {
		if cmd, err := h.db.CreateCommand(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID}); err != nil {
			log.Printf("[ota_status] create reboot command error: %v", err)
		} else {
			h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
		}
	}

	h.hub.PublishDeviceUpdate(device.ID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Productions ───────────────────────────────────────────────────────────────

func (h *Handler) ListProductions(w http.ResponseWriter, r *http.Request) {
	productions, err := h.db.ListProductions(r.Context())
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
		Name          string `json:"name"`
		ProductCode   string `json:"product_code"`
		ModelCode     string `json:"model_code"`
		Variant       string `json:"variant"`
		SKU           string `json:"sku"`
		BatchMonth    int    `json:"batch_month"`
		BatchYear     int    `json:"batch_year"`
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
	if body.BatchMonth < 1 || body.BatchMonth > 12 || body.BatchYear < 0 || body.BatchYear > 99 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch_month or batch_year"})
		return
	}
	if body.StartSequence < 1 || body.EndSequence < body.StartSequence {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid sequence range"})
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
		Batch:         db.EncodeBatch(body.BatchMonth, body.BatchYear),
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
	prod, err := h.db.GetProduction(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "production not found"})
		return
	}
	devices, err := h.db.GetProductionDevices(r.Context(), id)
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

func marshalCommand(id uuid.UUID, cmdType, apkURL string, payload json.RawMessage) []byte {
	msg, _ := json.Marshal(map[string]any{
		"type":         "command",
		"id":           id,
		"command_type": cmdType,
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
