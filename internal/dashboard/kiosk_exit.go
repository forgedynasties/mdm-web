package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Devices unlocked on site (kiosk-exit demo: the strip on the Kiosk page, the notice on the
// device page, and an alert). A technician who leaves kiosk on the device with the exit
// PIN or code takes it out until someone here answers: "Lock again" puts it back to what
// locked it (its rule, or its own setting), "Leave it out" keeps it unlocked and, under
// a rule, makes it an exception like a device changed by hand.

// kioskExitBack is where an answer returns to: the page it was given from.
func kioskExitBack(r *http.Request, serial string) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/devices/" + url.PathEscape(serial)
}

// DeviceKioskRelock is POST /devices/{serial}/kiosk/relock.
func (h *Handler) DeviceKioskRelock(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		http.NotFound(w, r)
		return
	}
	back := kioskExitBack(r, serial)
	how, ok, err := h.relockAfterExit(ctx, *dev)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		h.hxRedirect(w, r, withFlash(back, "It had already been answered."))
		return
	}
	h.auditDev(r, "kiosk.relock", dev.ID, serial, "after an exit on site, by "+how)
	h.hxRedirect(w, r, withFlash(back, "Locked again. It locks at its next check-in."))
}

// relockAfterExit answers an exit on site by locking the device again to what locked it:
// back under its rule (the enforcer locks it to whatever the rule says now), else to the
// package it was locked to. It resolves the "taken out" alert. ok is false when there was
// no open exit. Both "Lock again" here and a re-lock done on the tablet (staff tapped
// "Back to guest mode", extra.kiosk_relocked_at) come through it.
func (h *Handler) relockAfterExit(ctx context.Context, dev db.Device) (how string, ok bool, err error) {
	ex, err := h.db.ClearKioskExit(ctx, dev.ID)
	if err != nil || ex == nil {
		return "", false, err
	}
	how = "its own kiosk setting"
	if ex.RuleID != nil && h.kioskRuleFor(ctx, dev) != nil {
		_ = h.db.SetKioskOverride(ctx, dev.ID, false)
		h.reconcileKiosk(ctx)
		how = "its lock rule"
	} else if ex.Package != "" {
		if err := h.db.SetKioskConfig(ctx, dev.ID, true, ex.Package, 0); err != nil {
			return "", false, err
		}
		_ = h.db.MarkKioskManual(ctx, dev.ID, false)
		h.pushKioskConfigToDevices(ctx, []uuid.UUID{dev.ID})
	}
	_, _ = h.db.ResolveOpenAlert(ctx, "kiosk_exited", dev.ID)
	h.hub.PublishAlertUpdate()
	return how, true, nil
}

// RelockFromDevice is the check-in side of relockAfterExit: the tablet says staff put it
// back in guest mode at relockedAt. Only an exit from before that is answered, so a later
// exit is never undone by an old report. Wired into the api handler (main.go); reports
// whether it locked the device again.
func (h *Handler) RelockFromDevice(ctx context.Context, deviceID uuid.UUID, relockedAt time.Time) bool {
	ex := h.db.GetKioskExit(ctx, deviceID)
	if ex == nil || ex.At.After(relockedAt) {
		return false
	}
	dev, err := h.db.GetDeviceByID(ctx, deviceID)
	if err != nil || dev == nil {
		return false
	}
	how, ok, err := h.relockAfterExit(ctx, *dev)
	if err != nil || !ok {
		return false
	}
	_ = h.db.InsertAudit(ctx, "device", "kiosk.relock", dev.SerialNumber, "on the tablet (Back to guest mode), by "+how)
	return true
}

// DeviceKioskLeaveOut is POST /devices/{serial}/kiosk/leave-out.
func (h *Handler) DeviceKioskLeaveOut(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		http.NotFound(w, r)
		return
	}
	back := kioskExitBack(r, serial)
	ex, err := h.db.ClearKioskExit(ctx, dev.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ex == nil {
		h.hxRedirect(w, r, withFlash(back, "It had already been answered."))
		return
	}
	// Under a rule it becomes an exception, so the enforcer leaves it unlocked.
	_ = h.db.MarkKioskManual(ctx, dev.ID, h.kioskRuleFor(ctx, *dev) != nil)
	_, _ = h.db.ResolveOpenAlert(ctx, "kiosk_exited", dev.ID)
	h.hub.PublishAlertUpdate()
	h.auditDev(r, "kiosk.leave_out", dev.ID, serial, "after an exit on site")
	h.hxRedirect(w, r, withFlash(back, "Left unlocked."))
}

// withFlash adds the page's flash message to a local URL.
func withFlash(to, msg string) string {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	if i := strings.Index(to, "#"); i >= 0 {
		return to[:i] + sep + "flash=" + url.QueryEscape(msg) + to[i:]
	}
	return to + sep + "flash=" + url.QueryEscape(msg)
}

// kioskExitView is an exit decorated for the Kiosk page and the device page.
type kioskExitView struct {
	db.KioskExit
	AppName string
}

func appNameOf(apps map[string]db.FleetPackage, pkg string) string {
	if a, ok := apps[pkg]; ok && a.AppName != "" {
		return a.AppName
	}
	return pkg
}

// kioskExitFor is the device page's notice, nil when the device has no unanswered exit.
func (h *Handler) kioskExitFor(r *http.Request, dev *db.Device) *kioskExitView {
	if dev == nil {
		return nil
	}
	ex := h.db.GetKioskExit(r.Context(), dev.ID)
	if ex == nil {
		return nil
	}
	return &kioskExitView{KioskExit: *ex, AppName: appNameOf(h.fleetAppIndex(r), ex.Package)}
}

// lateSpansFor is the device page's first set of offline spans (readings sent later),
// the last two weeks; the chart adds more as it loads other windows.
func (h *Handler) lateSpansFor(r *http.Request, dev *db.Device) []db.TimeSpan {
	now := time.Now()
	spans, err := h.db.LateSpans(r.Context(), dev.ID, now.Add(-14*24*time.Hour), now, time.Duration(chartGapMs(dev.PollIntervalMs))*time.Millisecond)
	if err != nil || spans == nil {
		return []db.TimeSpan{}
	}
	return spans
}

// staffMode reports whether a device says staff mode is up (client 1.13.7): someone used
// the staff PIN, the host app is unlocked and the staff pill is on screen until they tap
// "Back to guest mode". Shown as a "Staff mode" chip on the device page and in lists.
func staffMode(extra json.RawMessage) bool {
	var x struct {
		On bool `json:"kiosk_staff_mode"`
	}
	return len(extra) > 0 && json.Unmarshal(extra, &x) == nil && x.On
}

// APISetDeviceKiosk locks or unlocks one device (POST /api/v1/devices/{serial}/kiosk,
// admin key, {"enabled": bool, "package": "…"}): what the device page's Lock does by hand,
// for test benches. A lock set this way is an exception to any lock rule, like one set on
// the device page. The config is pushed at once to a connected device.
func (h *Handler) APISetDeviceKiosk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool   `json:"enabled"`
		Package string `json:"package"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body must be {\"enabled\": bool, \"package\": string}")
		return
	}
	body.Package = strings.TrimSpace(body.Package)
	if body.Enabled && body.Package == "" {
		writeJSONError(w, http.StatusBadRequest, "package is required to lock")
		return
	}
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil || dev == nil {
		writeJSONError(w, http.StatusNotFound, "device not found")
		return
	}
	pkg := body.Package
	if !body.Enabled && pkg == "" {
		if cfg, err := h.db.GetOrCreateDeviceConfig(ctx, dev.ID); err == nil {
			pkg = cfg.KioskPackage
		}
	}
	if err := h.db.SetKioskConfig(ctx, dev.ID, body.Enabled, pkg, 0); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = h.db.MarkKioskManual(ctx, dev.ID, h.kioskRuleFor(ctx, *dev) != nil)
	if body.Enabled {
		_, _ = h.db.ResolveOpenAlert(ctx, "kiosk_exited", dev.ID)
	}
	h.pushKioskConfigToDevices(ctx, []uuid.UUID{dev.ID})
	detail := "off"
	if body.Enabled {
		detail = "on, " + pkg
	}
	_ = h.db.InsertAudit(ctx, "API key", "device.kiosk", dev.SerialNumber, detail)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"serial": dev.SerialNumber, "enabled": body.Enabled, "package": pkg,
		"connected": h.hub.IsConnected(dev.ID)})
}
