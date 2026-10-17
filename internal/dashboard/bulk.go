package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strconv"
	"strings"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// BulkNickname renames the selected devices from one pattern. {n} is the 1-based
// position in the selection (order as picked on the page), {serial} the full
// serial and {last4} its tail, so "Register {n}" or "POS-{last4}" both work.
// An empty pattern clears the nicknames.
func (h *Handler) BulkNickname(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	start, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("start")))
	if start <= 0 {
		start = 1
	}
	done, denied := 0, 0
	acc := h.access(r)
	for i, serial := range serials {
		device, err := h.db.GetDevice(r.Context(), serial)
		if err != nil {
			continue
		}
		if !acc.decide("notes", &device.ID).Allowed {
			denied++
			continue
		}
		last4 := serial
		if len(last4) > 4 {
			last4 = last4[len(last4)-4:]
		}
		name := strings.NewReplacer("{n}", strconv.Itoa(start+i), "{serial}", serial, "{last4}", last4).Replace(pattern)
		if len(name) > 40 {
			name = name[:40]
		}
		if err := h.db.SetNickname(r.Context(), device.ID, name); err != nil {
			continue
		}
		h.hub.PublishDeviceUpdate(device.ID)
		done++
	}
	h.audit(r, "device.bulk_nickname", strings.Join(serials, ","), fmt.Sprintf("%d devices: %q", done, pattern))
	msg := fmt.Sprintf("Renamed %d device%s", done, plural(done))
	if pattern == "" {
		msg = fmt.Sprintf("Cleared %d nickname%s", done, plural(done))
	}
	typ := "success"
	if denied > 0 {
		msg += fmt.Sprintf(" · %d not allowed by your access policy", denied)
		if done == 0 {
			typ = "error"
		}
	}
	h.hxDoneToastEvents(w, r, "/devices", msg, typ, "refresh-devices")
}

// BulkRetire takes the selection out of the active fleet (lists, alerts, policy
// coverage) while keeping history and placement; a later check-in brings a device
// back. With action=unretire it does the reverse.
func (h *Handler) BulkRetire(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}
	unretire := r.FormValue("action") == "unretire"
	done := 0
	for _, serial := range serials {
		device, err := h.db.GetDevice(r.Context(), serial)
		if err != nil {
			continue
		}
		status := db.EnrollRetired
		if unretire {
			status = db.EnrollAuto
			if device.IsDPC() {
				status = db.EnrollEnrolled
			}
		}
		if err := h.db.SetEnrollmentStatus(r.Context(), device.ID, status); err != nil {
			continue
		}
		h.hub.PublishDeviceUpdate(device.ID)
		done++
	}
	verb, act := "Retired", "device.bulk_retire"
	if unretire {
		verb, act = "Brought back", "device.bulk_unretire"
	}
	h.audit(r, act, strings.Join(serials, ","), fmt.Sprintf("%d devices", done))
	h.hxDoneToastEvents(w, r, "/devices", fmt.Sprintf("%s %d device%s", verb, done, plural(done)), "success", "refresh-devices")
}

// BulkAssignRestaurant assigns the selected devices to a restaurant from the devices-page
// bulk-selection bar.
func (h *Handler) BulkAssignRestaurant(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") { // "Manage groups & venues"
		return
	}
	r.ParseForm()
	serials := h.keepVisibleSerials(r, parseSerialsField(r.Form["serials"]))
	rid, err := uuid.Parse(strings.TrimSpace(r.FormValue("restaurant_id")))
	if err != nil {
		http.Error(w, "Invalid restaurant", http.StatusBadRequest)
		return
	}
	if err := h.db.AssignDevicesToRestaurant(r.Context(), serials, rid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.assign", rid.String(), fmt.Sprintf("%d devices", len(serials)))
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, id := range ids {
			h.hub.PublishDeviceUpdate(id)
		}
		h.pushGuestToDevices(r.Context(), ids)
	}
	// Navigate to the restaurant so the operator lands on the result (the default
	// /devices view doesn't reflect a restaurant change in place). Boosted, so this
	// is a smooth <main> crossfade, not a hard reload.
	h.hxRedirect(w, r, "/restaurants/"+rid.String())
}

// pushKioskConfigToDevices fetches the current device_config for each device and
// pushes a "config" WebSocket message so connected devices apply kiosk changes
// immediately instead of waiting for the next check-in.
func (h *Handler) pushKioskConfigToDevices(ctx context.Context, deviceIDs []uuid.UUID) {
	interval := h.cfg.CheckinInterval()
	for _, id := range deviceIDs {
		cfg, err := h.db.GetOrCreateDeviceConfig(ctx, id)
		if err != nil {
			continue
		}
		msg, _ := json.Marshal(map[string]any{
			"type":                     "config",
			"kiosk_enabled":            cfg.KioskEnabled,
			"kiosk_package":            cfg.KioskPackage,
			"kiosk_features":           cfg.KioskFeatures,
			"kiosk_mode":               cfg.KioskMode,
			"kiosk_packages":           cfg.KioskPackages,
			"kiosk_url":                cfg.KioskURL,
			"kiosk_url_allow":          cfg.KioskURLAllow,
			"wlc_charging_enabled":     cfg.WlcChargingEnabled,
			"checkin_interval_seconds": interval,
		})
		h.hub.Push(id, msg)
		// Enabling app locks the device to one app — wake the screen too, so a device
		// that was asleep doesn't sit locked to a black screen until something else wakes
		// it. The client already handles this frame for remote-control capture; reused
		// as-is here, no new client support needed.
		if cfg.KioskEnabled {
			if wakeMsg, err := json.Marshal(map[string]any{"type": "wake_screen"}); err == nil {
				h.hub.Push(id, wakeMsg)
			}
		}
		h.hub.PublishDeviceUpdate(id)
	}
}

// BulkKioskApps returns the apps installed across the selected devices as JSON,
// each with the count of those devices that have it, so the bulk-kiosk picker can
// show apps common to every selected device and grey out partially-present ones.
func (h *Handler) BulkKioskApps(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := r.Form["serials"]
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Kiosk is a per-device action: judge each device, not the fleet (a fleet-level
	// check ignores restaurant and device rules entirely).
	deviceIDs, _ = h.access(r).filterDevices("kiosk", deviceIDs)
	apps, err := h.db.KioskAppsForDevices(r.Context(), deviceIDs)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total": len(deviceIDs),
		"apps":  apps,
	})
}

func (h *Handler) BulkKioskUpdate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := r.Form["serials"]
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}

	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	if enabled && pkg == "" {
		http.Error(w, "Kiosk package is required when enabling locked mode", http.StatusBadRequest)
		return
	}
	if !enabled {
		pkg = ""
	}

	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	deviceIDs, _, ok := h.enforceCommandTargets(w, r, "kiosk", "devices", deviceIDs)
	if !ok {
		return
	}
	if err := h.db.SetKioskConfigForDevices(r.Context(), deviceIDs, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.markKioskManual(r.Context(), deviceIDs)
	h.pushKioskConfigToDevices(r.Context(), deviceIDs)
	verb := "Disabled kiosk on"
	if enabled {
		verb = "Enabled kiosk on"
	}
	h.hxDoneToastEvents(w, r, "/devices", fmt.Sprintf("%s %d device%s", verb, len(deviceIDs), plural(len(deviceIDs))), "success", "refresh-devices")
}
