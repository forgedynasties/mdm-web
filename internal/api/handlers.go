package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"mdm/internal/db"
	"mdm/internal/shell"
	"mdm/internal/ws"
)

type Handler struct {
	db    *db.DB
	hub   *ws.Hub
	shell *shell.Manager
}

func NewHandler(d *db.DB, hub *ws.Hub, shellMgr *shell.Manager) *Handler {
	return &Handler{db: d, hub: hub, shell: shellMgr}
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

	go client.WritePump()
	client.ReadPump() // blocks until connection closes
}

// flushPendingCommands pushes all pending commands to the device over WS and
// marks them delivered (or completed for reboot).
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
		} else {
			_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
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
		if cmd.Type == "reboot" {
			_ = h.db.AckCommand(ctx, cmd.ID, deviceID, "completed")
		} else {
			_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
		}
	}
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
	body, _ := io.ReadAll(r.Body)
	log.Printf("[checkin] raw body: %s", body)
	r.Body = io.NopCloser(bytes.NewReader(body))

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

	// OTA check: inject an OTA command if the device needs an update and push it via WS.
	if hasPending, err := h.db.HasPendingOTACommand(r.Context(), deviceID); err != nil {
		log.Printf("[checkin] HasPendingOTACommand error: %v", err)
	} else if !hasPending {
		if upd, err := h.db.ResolveUpdateForDevice(r.Context(), deviceID); err != nil {
			log.Printf("[checkin] ResolveUpdateForDevice error: %v", err)
		} else if upd != nil && upd.OtaPackage != nil {
			pkg := upd.OtaPackage
			applicable := false
			if pkg.Type == "incremental" {
				// Incremental: only apply if device's current build matches source
				applicable = pkg.SourceBuildID == req.BuildID
			} else {
				// Full: apply if device is not already on target build
				applicable = pkg.TargetBuildID != req.BuildID
			}
			if applicable {
				p := map[string]any{
					"package_id":      pkg.ID,
					"build_id":        pkg.TargetBuildID,
					"update_url":      pkg.UpdateURL,
					"payload_offset":  pkg.PayloadOffset,
					"payload_size":    pkg.PayloadSize,
					"payload_headers": pkg.PayloadHeaders,
					"reboot_behavior": upd.RebootBehavior,
				}
				if upd.ScheduledTime != nil {
					p["scheduled_time"] = upd.ScheduledTime.UTC().Format(time.RFC3339)
				}
				payload, _ := json.Marshal(p)
				if cmd, err := h.db.CreateCommand(r.Context(), "ota", "", payload, "devices", []uuid.UUID{deviceID}); err != nil {
					log.Printf("[checkin] create OTA command error: %v", err)
				} else {
					h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{deviceID})
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

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"config": map[string]any{
			"kiosk_enabled":  deviceCfg.KioskEnabled,
			"kiosk_package":  deviceCfg.KioskPackage,
			"kiosk_features": deviceCfg.KioskFeatures,
		},
	})
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

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Devices ───────────────────────────────────────────────────────────────────

func (h *Handler) ListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "")
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if body.Output != "" {
		_ = h.db.SaveCommandResult(r.Context(), cmdID, device.ID, body.Output)
	}
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
