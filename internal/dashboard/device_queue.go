package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"

	"mdm/internal/db"

	"github.com/google/uuid"
)

func (h *Handler) CommandStatusPartial(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil || h.commandHiddenFrom(r, *cmd) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	deliveries = h.keepVisibleDeliveries(r, deliveries)
	h.markDeliveryPresence(deliveries)
	h.renderCachedHTML(w, r, "command-deliveries", map[string]any{
		"Command":    cmd,
		"Deliveries": deliveries,
		"Nicknames":  h.deliveryNicknames(r.Context(), deliveries),
		"Stats":      computeDeliveryStats(deliveries),
		"CanResend":  h.commandTypeAllowed(h.role(r), cmd.Type),
	})
}

// DeviceInstallCancel cancels the in-flight install(s) of a given APK on a device from
// the Applications drawer: it deletes the pending install command(s) and pushes a
// cancel_command frame so a device mid-download aborts. HTMX gets 204 + device-updated so
// the drawer refreshes and the tile disappears.
func (h *Handler) DeviceInstallCancel(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "queue", device.ID) {
		return
	}
	r.ParseForm()
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if apkURL == "" {
		http.Error(w, "apk_url required", http.StatusBadRequest)
		return
	}
	inFlight := func(s string) bool {
		return s == "pending" || s == "delivered" || s == "downloading" || s == "installing"
	}
	cmds, _ := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	n := 0
	for _, c := range cmds {
		if c.Type != "install_apk" || c.ApkURL != apkURL || !inFlight(c.Status) {
			continue
		}
		if err := h.db.DeleteCommand(r.Context(), c.ID); err == nil {
			if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": c.ID.String()}); e == nil {
				h.hub.Push(device.ID, msg)
			}
			n++
		}
	}
	h.audit(r, "install.cancel", serial, apkURL)
	// A cancelled install frees the device's install slot — release the next queued one.
	h.advanceQueue(r.Context(), device.ID)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", "device-updated")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
}

// advanceQueue delivers the next queued command(s) for a device now that one
// finished or was removed — the queue runs one at a time (delivery gate holds the
// rest) EXCEPT ota, which is exempt from the gate (see GetPendingCommandsForDevice)
// so it never blocks, or waits behind, anything else. That means more than one row
// can legitimately come back eligible at once (the regular queue's single oldest,
// plus an independent ota row), so every returned command is pushed — mirroring
// flushPendingCommands' loop — not just the first. No-op if the device is offline
// (it flushes on reconnect).
func (h *Handler) advanceQueue(ctx context.Context, deviceID uuid.UUID) {
	if !h.hub.IsConnected(deviceID) {
		return
	}
	cmds, err := h.db.GetPendingCommandsForDevice(ctx, deviceID)
	if err != nil {
		return
	}
	for _, cmd := range cmds {
		msg, _ := json.Marshal(map[string]any{
			"type":         "command",
			"id":           cmd.ID,
			"command_type": db.DeviceCommandType(cmd.Type),
			"apk_url":      cmd.ApkURL,
			"payload":      cmd.Payload,
		})
		if h.hub.Push(deviceID, msg) {
			_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
			h.hub.PublishCommandUpdate(cmd.ID)
		}
		if cmd.Type == "reboot" {
			break // remaining commands wait for the device to reconnect post-reboot
		}
	}
}

// DeviceQueueRemove removes one command from a single device's queue. Marks that device's
// per-command status 'cancelled' (per-device, so a group/all command is only dropped for
// this device) and, if the device is online, sends a cancel_command frame so an in-flight
// download aborts. 204 + device-updated so the Queue tab morphs in place.
func (h *Handler) DeviceQueueRemove(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "queue", device.ID) {
		return
	}
	if err := h.db.CancelDeviceCommand(r.Context(), id, device.ID); err != nil {
		if err == db.ErrCommandNotTargeted {
			http.Error(w, "Command does not target device", http.StatusForbidden)
			return
		}
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if h.hub.IsConnected(device.ID) {
		if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": id.String()}); e == nil {
			h.hub.Push(device.ID, msg)
		}
	}
	h.audit(r, "queue.remove", serial, id.String())
	h.hub.PublishCommandUpdate(id)
	h.hub.PublishDeviceUpdate(device.ID)
	// Removing an install may have been the one holding the queue — release the next.
	h.advanceQueue(r.Context(), device.ID)
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// DeviceQueueClear cancels every command currently in a device's queue (each cancelled for
// this device only), aborting any in-flight download, and clears the tab in one shot.
func (h *Handler) DeviceQueueClear(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "queue", device.ID) {
		return
	}
	queue, err := h.db.GetDeviceQueue(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	online := h.hub.IsConnected(device.ID)
	n := 0
	for _, cmd := range queue {
		if err := h.db.CancelDeviceCommand(r.Context(), cmd.ID, device.ID); err != nil {
			continue
		}
		if online {
			if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": cmd.ID.String()}); e == nil {
				h.hub.Push(device.ID, msg)
			}
		}
		h.hub.PublishCommandUpdate(cmd.ID)
		n++
	}
	h.audit(r, "queue.clear", serial, fmt.Sprintf("%d cleared", n))
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// nudgeCheckin asks online devices to perform a full check-in right now (a "checkin_now"
// WS frame). Used after assigning an OTA so the update resolves and pushes immediately
// instead of waiting out the device's periodic check-in (which can be minutes away,
// leaving the update stuck "pending"). Offline devices ignore it and pick the update up
// on their next check-in as before.
// pushOTAToConnected resolves and delivers the OTA command immediately to
// whichever of deviceIDs are currently connected, instead of leaving it to be
// picked up on their next periodic check-in (db.TryCreateOTACommand — the same
// resolver the check-in handler uses, so the conditions for creating the command
// can't diverge between the two paths). Offline devices are unaffected.
func (h *Handler) pushOTAToConnected(ctx context.Context, deviceIDs []uuid.UUID) {
	var connected []uuid.UUID
	for _, id := range deviceIDs {
		if h.hub.IsConnected(id) {
			connected = append(connected, id)
		}
	}
	if len(connected) == 0 {
		return
	}
	devices, err := h.db.GetDevicesByIDs(ctx, connected)
	if err != nil {
		log.Printf("[deploy] GetDevicesByIDs error: %v", err)
		return
	}
	for _, dev := range devices {
		upd, err := h.db.ResolveUpdateForDevice(ctx, dev.ID)
		if err != nil || upd == nil || upd.OtaPackage == nil {
			continue
		}
		if _, err := h.db.TryCreateOTACommand(ctx, upd, dev.ID, dev.BuildID); err != nil {
			log.Printf("[deploy] create OTA command for %s: %v", dev.SerialNumber, err)
			continue
		}
		// OTA is exempt from the delivery gate (GetPendingCommandsForDevice), so this
		// delivers the OTA command just created regardless of anything else already
		// queued for the device.
		h.advanceQueue(ctx, dev.ID)
	}
}

func (h *Handler) nudgeCheckin(deviceIDs []uuid.UUID) {
	if len(deviceIDs) == 0 {
		return
	}
	msg, err := json.Marshal(map[string]any{"type": "checkin_now"})
	if err != nil {
		return
	}
	for _, did := range deviceIDs {
		h.hub.Push(did, msg) // no-op when the device isn't connected
	}
}
