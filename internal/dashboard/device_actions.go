package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mdm/internal/apkmeta"

	"github.com/google/uuid"
)

func (h *Handler) DeviceCommandCreate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}

	// Authorize the command type (role-keyed allowlist) before touching the
	// device, so an unknown/forbidden type never leaks device existence.
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	if cmdType == "shell" && !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if isDestructiveCmd(cmdType) && h.cfg.RequireReason() && reason == "" {
		http.Error(w, "A reason is required for this command.", http.StatusBadRequest)
		return
	}

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, policyActionForCommand(cmdType), device.ID) {
		return
	}
	// Capability gate, as the bulk Actions path does: never queue a command the
	// device's agent can't run (e.g. uninstall on MDM-lite, which has no Device Owner).
	if !device.Supports(cmdType) {
		http.Error(w, fmt.Sprintf("%s isn't available on this device's agent (%s).", cmdTypeLabel(cmdType), device.KindLabel()), http.StatusBadRequest)
		return
	}

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
		return
	}

	payload, msg := checkedPayload(cmdType, r)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	// An agent update names no APK: it is always the build this server hosts, so the
	// console never has to know a URL and can't point a device at the wrong one.
	if cmdType == "app_update" && apkURL == "" {
		url, pkg, version, _, sha, ok := h.agentUpdateTarget(r, agentSlotFor(device))
		if !ok {
			http.Error(w, "No agent build is hosted for this device's agent and signing keys — publish one first.", http.StatusBadRequest)
			return
		}
		apkURL = url
		payload = agentUpdatePayload(pkg, version, sha)
	}
	if cmdType == "install_apk" {
		// Capture APK size + ETag so the device can verify/resume the download.
		payload = apkmeta.Augment(r.Context(), apkURL, payload)
	}

	// Don't stack a duplicate: if an identical command (same type + key params) is already
	// A reboot during an OTA download throws the bytes away, and during an install can
	// leave a half-written slot — refuse it while either fleet's update is in flight.
	// The MDM's own post-OTA reboot does not come through here, so it still works.
	if cmdType == "reboot" {
		if blocked, why, err := h.db.RebootBlockedFor(r.Context(), device.ID); err == nil && blocked {
			msg := "Reboot refused: " + why + " on this device. It reboots on its own when the update is ready."
			if r.Header.Get("Accept") == "application/json" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": msg})
				return
			}
			http.Error(w, msg, http.StatusConflict)
			return
		}
	}

	// in flight for this device, bounce back to the (already-showing) pending row instead
	// of queuing a second the device must process. Install keys on APK URL; other types on
	// payload; reboot/screenshot collapse to one in-flight per type.
	if existing, err := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry()); err == nil {
		if dup, ok := findPendingLikeCommand(existing, cmdType, apkURL, payload); ok {
			if r.Header.Get("Accept") == "application/json" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "an identical command is already pending for this device"})
				return
			}
			if cmdType == "screenshot" || cmdType == "shell" || cmdType == "collect_logs" {
				// These types land the user on the command's own page once created — a
				// duplicate should do the same instead of dead-ending on a flash, since
				// there's already somewhere useful (and live) to send them: the pending
				// command they're about to duplicate.
				http.Redirect(w, r, "/commands/"+dup.ID.String()+"?from=/devices/"+serial, http.StatusFound)
				return
			}
			h.hxRedirect(w, r, "/devices/"+serial+"?flash="+url.QueryEscape(cmdTypeLabel(cmdType)+" is already pending for this device — check the Queue tab.")+"&flash_type=info")
			return
		}
	}

	cmd, err := h.db.CreateCommandBy(r.Context(), cmdType, apkURL, payload, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	detail := "device=" + serial
	if reason != "" {
		detail += ", reason=" + reason
	}
	detail += ", cmd=" + cmd.ID.String()
	h.audit(r, "command.send", cmdType, detail)
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": cmd.ID.String()})
		return
	}
	if cmdType == "screenshot" || cmdType == "shell" || cmdType == "collect_logs" {
		// Only jump to the command page when a result is imminent (device online). If the
		// device is offline the command is merely QUEUED — stay on the device page so the
		// user watches it in the Queue tab instead of landing on an empty "waiting…" command
		// page and losing the device view.
		if h.hub.IsConnected(device.ID) {
			http.Redirect(w, r, "/commands/"+cmd.ID.String()+"?from=/devices/"+serial, http.StatusFound)
			return
		}
		// The app-wide hx-boost means this plain form's htmx swap target is never inside
		// a <form> (it's the boosted body/main region), so layout.html's generic
		// "any successful mutation gets a Done toast" listener silently no-ops here
		// (its own form.closest('form') check finds nothing) — a bare 204 left this click
		// looking like it did nothing at all. Send an explicit toast so it doesn't.
		h.hxDoneToastEvents(w, r, "/devices/"+serial, cmdTypeLabel(cmdType)+" queued — check the Queue tab.", "info", "device-updated")
		return
	}
	// Uninstall from the Applications drawer: don't reload the page. Fire device-updated so
	// the drawer refreshes and shows the app as "Uninstalling…" until the device confirms
	// removal (mirrors the instant-install flow). Same silent-204 issue as above.
	if cmdType == "uninstall" && r.Header.Get("HX-Request") == "true" {
		h.hxDoneToastEvents(w, r, "/devices/"+serial, "Uninstalling…", "info", "device-updated")
		return
	}
	http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
}

func (h *Handler) DeviceSetPollInterval(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	ms, err := strconv.Atoi(r.FormValue("poll_interval_ms"))
	if err != nil || ms < 5000 || ms > 3600000 {
		http.Error(w, "poll_interval_ms must be between 5000 and 3600000 (5s–1h)", http.StatusBadRequest)
		return
	}
	if err := h.db.SetDevicePollInterval(r.Context(), serial, ms); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
	}
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// thisServerURL is this server's own device-facing origin, so the move menu can say
// which entry means "stay here".
func (h *Handler) thisServerURL() string {
	if len(h.publicOrigins) > 0 {
		return strings.TrimRight(h.publicOrigins[0], "/")
	}
	return ""
}

// mdmServerChoices is the list an admin maintains in Settings — where a device can
// be moved to. This server's own device-facing URL is folded in so "move it back"
// is always on the menu even if someone trims the list.
func (h *Handler) mdmServerChoices() []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, o := range h.cfg.MDMServers() {
		add(o)
	}
	// Only the primary origin: a box that answers on several addresses does not need
	// all of them on this menu.
	add(h.thisServerURL())
	return out
}

// SettingsSetMDMServers replaces the move-a-device server list (one URL per line).
func (h *Handler) SettingsSetMDMServers(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var urls []string
	seen := map[string]bool{}
	for _, line := range strings.Split(r.FormValue("servers"), "\n") {
		u, err := url.Parse(strings.TrimSpace(line))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		clean := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		if seen[clean] {
			continue
		}
		seen[clean] = true
		urls = append(urls, clean)
	}
	if err := h.cfg.SetMDMServers(urls); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.mdm_servers", "", strings.Join(urls, " "))
	h.settingsRedirect(w, r)
}

// DeviceMoveServer points a firmware device at a different MDM and reboots it into
// the change. The client reads persist.sys.mdm.url at startup, so the move is two
// commands in order — set the prop, then reboot — and the device comes back talking
// to the other server. It then belongs to THAT server: this one keeps the row it
// already has and stops hearing from it, which is why this is admin-only and spelled
// out in the confirm.
func (h *Handler) DeviceMoveServer(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || device == nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if device.IsDPC() {
		h.hxDoneToast(w, r, "/devices/"+serial, "Standard MDM reads its server from its own config, not this property", "error")
		return
	}
	r.ParseForm()
	target := strings.TrimSpace(r.FormValue("server_url"))
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		h.hxDoneToast(w, r, "/devices/"+serial, "That is not a server URL — use https://host", "error")
		return
	}
	target = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")

	payload, _ := json.Marshal(map[string]string{"cmd": "setprop persist.sys.mdm.url " + target})
	setCmd, err := h.db.CreateCommandBy(r.Context(), "shell", "", payload, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, "/devices/"+serial, "Could not queue the server change", "error")
		return
	}
	h.pushCommand(r.Context(), setCmd, "devices", []uuid.UUID{device.ID})
	// Queued behind the setprop: the queue delivers in order and stops at a reboot,
	// so the property is written before the device goes down.
	rebootCmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, "/devices/"+serial, "Server set, but the reboot could not be queued — reboot it yourself to apply", "error")
		return
	}
	h.pushCommand(r.Context(), rebootCmd, "devices", []uuid.UUID{device.ID})
	// Record where we sent it. This is a suspicion, not a sighting — the device has
	// not arrived anywhere yet, and may never (a bad URL, a device that never comes
	// back up). It is marked source "move" and carries no seen-at, so the page can say
	// "moving to stage" rather than claiming it is there. A peer announcement or the
	// reconciliation sweep is what turns this into a fact.
	if _, err := h.db.SetDeviceCustody(r.Context(), device.ID, peerNameForURL(h.cfg, target), target, time.Time{}, "move"); err != nil {
		log.Printf("[device] record move custody for %s: %v", serial, err)
	}
	h.audit(r, "device.move_server", serial, target)
	h.hxDoneToast(w, r, "/devices/"+serial, serial+" is moving to "+target+" · it reboots now and reports there", "success")
}

// DeviceNotesUpdate saves freeform operator notes for a device and returns the
// updated notes card for an htmx swap (falls back to a redirect without JS).
func (h *Handler) DeviceNotesUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "notes", device.ID) {
		return
	}
	r.ParseForm()
	notes := strings.TrimSpace(r.FormValue("notes"))
	const maxNotes = 4000
	if len(notes) > maxNotes {
		notes = notes[:maxNotes]
	}
	if err := h.db.SetDeviceNotes(r.Context(), device.ID, notes); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.notes", serial, fmt.Sprintf("len=%d", len(notes)))
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "device-notes", h.withRole(r, map[string]any{
		"Device": device,
		"Notes":  notes,
		"Saved":  true,
	}))
}

func (h *Handler) DeviceKioskUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "kiosk", device.ID) {
		return
	}

	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))

	// Locked mode: single app (classic), multi-app (extra packages allowed in
	// lock-task), or browser (locked browser on a URL). Older callers that don't
	// send a mode keep the classic single-app behaviour.
	mode := r.FormValue("kiosk_mode")
	switch mode {
	case "app", "multi", "browser":
	case "":
		mode = "app"
	default:
		http.Error(w, "Unknown locked mode", http.StatusBadRequest)
		return
	}

	// Browser app locks to the agent's built-in browser, not an installed app.
	kioskURL := strings.TrimSpace(r.FormValue("kiosk_url"))
	var urlAllow []string
	var extraPkgs []string
	if mode == "browser" {
		pkg = ""
		if enabled && !strings.HasPrefix(kioskURL, "http://") && !strings.HasPrefix(kioskURL, "https://") {
			http.Error(w, "Browser kiosk needs a start URL beginning with http:// or https://", http.StatusBadRequest)
			return
		}
		// One allowed URL prefix per textarea line.
		for _, line := range strings.Split(r.FormValue("kiosk_url_allow"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				urlAllow = append(urlAllow, line)
			}
		}
	} else {
		kioskURL = ""
		if enabled && pkg == "" {
			http.Error(w, "Kiosk package is required when enabling locked mode", http.StatusBadRequest)
			return
		}
	}
	if mode == "multi" {
		seen := map[string]bool{pkg: true}
		for _, p := range r.Form["kiosk_packages"] {
			if p = strings.TrimSpace(p); p != "" && !seen[p] {
				seen[p] = true
				extraPkgs = append(extraPkgs, p)
			}
		}
	}

	// Only allow locking to apps the device actually reports as installed. Enabling
	// kiosk for a package that isn't on the device stores an unenforceable policy: the
	// client can't launch it, so lock-task never engages and the device sits unlocked
	// while the dashboard shows kiosk "on". Reject it here so the state stays truthful.
	// The extra multi-app packages and the locked app must also pass the fleet kiosk
	// allowlist (the picker only offers allowed apps; enforce it server-side too).
	if enabled && mode != "browser" {
		pkgs, perr := h.db.GetDevicePackages(r.Context(), device.ID)
		if perr != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		installed := make(map[string]bool, len(pkgs))
		for _, p := range pkgs {
			installed[p.PackageName] = true
		}
		for _, p := range append([]string{pkg}, extraPkgs...) {
			if !installed[p] {
				http.Error(w, "That app is not installed on this device. Install it first, then enable kiosk.", http.StatusBadRequest)
				return
			}
			if !h.cfg.KioskAppAllowed(p) {
				http.Error(w, "That app is not on the kiosk allowlist (Settings → Kiosk).", http.StatusBadRequest)
				return
			}
		}
	}

	if err := h.db.SetKioskConfig(r.Context(), device.ID, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetKioskModeConfig(r.Context(), device.ID, mode, extraPkgs, kioskURL, urlAllow); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Set by hand. If a lock rule covers this device, this is now an exception to it
	// that the enforcer leaves alone (the device page offers "follow the rule again").
	rule := h.kioskRuleFor(r.Context(), *device)
	_ = h.db.MarkKioskManual(r.Context(), device.ID, rule != nil)
	detail := boolWord(enabled)
	if enabled && mode != "" {
		detail += " (" + mode + ")"
	}
	if rule != nil {
		detail += ", overrides rule " + rule.Name
	}
	h.auditDev(r, "device.kiosk", device.ID, serial, detail)
	h.pushKioskConfigToDevices(r.Context(), []uuid.UUID{device.ID})
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// DeviceWlcUpdate enables/disables wireless charging on a device's pad. The client
// writes the customer_gpio line on the next config push (pushKioskConfigToDevices
// carries wlc_charging_enabled) and re-applies it on boot.
func (h *Handler) DeviceWlcUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "kiosk", device.ID) {
		return
	}
	enabled := r.FormValue("wlc_charging_enabled") == "1"
	if err := h.db.SetWlcCharging(r.Context(), device.ID, enabled); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.auditDev(r, "device.wlc_charging", device.ID, serial, boolWord(enabled))
	h.pushKioskConfigToDevices(r.Context(), []uuid.UUID{device.ID})
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// DeviceBrightnessUpdate sets the screen brightness the client holds the device at
// (form screen_brightness 1-255, empty = stop managing; screen_brightness_locked=1 to
// lock). Pushed at once to a connected device.
func (h *Handler) DeviceBrightnessUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "kiosk", device.ID) {
		return
	}
	level, err := parseBrightness(r.FormValue("screen_brightness"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	locked := level != nil && r.FormValue("screen_brightness_locked") == "1"
	if err := h.db.SetScreenBrightness(r.Context(), device.ID, level, locked); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.auditDev(r, "device.screen_brightness", device.ID, serial, brightnessDetail(level, locked))
	h.pushKioskConfigToDevices(r.Context(), []uuid.UUID{device.ID})
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// APISetDeviceBrightness is DeviceBrightnessUpdate for scripts (POST
// /api/v1/devices/{serial}/brightness, admin key, {"level": 1-255|null, "locked": bool}).
func (h *Handler) APISetDeviceBrightness(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level  *int `json:"level"`
		Locked bool `json:"locked"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body must be {\"level\": 1-255 or null, \"locked\": bool}")
		return
	}
	if body.Level != nil && (*body.Level < 1 || *body.Level > 255) {
		writeJSONError(w, http.StatusBadRequest, "level must be 1-255, or null to stop managing it")
		return
	}
	locked := body.Level != nil && body.Locked
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil || dev == nil {
		writeJSONError(w, http.StatusNotFound, "device not found")
		return
	}
	if err := h.db.SetScreenBrightness(ctx, dev.ID, body.Level, locked); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.pushKioskConfigToDevices(ctx, []uuid.UUID{dev.ID})
	_ = h.db.InsertAudit(ctx, "API key", "device.screen_brightness", dev.SerialNumber, brightnessDetail(body.Level, locked))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"serial": dev.SerialNumber, "level": body.Level, "locked": locked,
		"connected": h.hub.IsConnected(dev.ID)})
}

// parseBrightness reads the form value: empty means not managed.
func parseBrightness(v string) (*int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 255 {
		return nil, fmt.Errorf("brightness must be 1-255, or empty to stop managing it")
	}
	return &n, nil
}

// brightnessDetail is the audit detail: "122, locked", "122", or "off" (not managed).
func brightnessDetail(level *int, locked bool) string {
	if level == nil {
		return "off"
	}
	if locked {
		return strconv.Itoa(*level) + ", locked"
	}
	return strconv.Itoa(*level)
}
