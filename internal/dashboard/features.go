package dashboard

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
)

// This file holds the DPC-agent feature pages: enrollment, the fleet-wide
// device_policy pages (system update policy, network provisioning) and managed
// app configurations. Kept separate from the huge handlers.go so the feature
// surface is easy to find and extend. All pages render through h.render (which
// injects role/brand/nav via withRole).

// EnrollmentPage shows enrollment profiles (revocable QR/zero-touch tokens) plus the
// manual adb provisioning path with the shared device API key.
func (h *Handler) EnrollmentPage(w http.ResponseWriter, r *http.Request) {
	deviceKey := os.Getenv("DEVICE_API_KEY")
	masked := deviceKey
	if len(masked) > 6 {
		masked = masked[:3] + "••••••" + masked[len(masked)-3:]
	}
	profiles, _ := h.db.ListEnrollmentProfiles(r.Context())
	groups, _ := h.db.ListGroups(r.Context())
	h.render(w, r, "enrollment.html", map[string]any{
		"Title":          "Enrollment",
		"ActivePage":     "enrollment",
		"ServerURL":      h.baseURL(r),
		"DeviceKey":      deviceKey,
		"DeviceKeyMask":  masked,
		"AdminComponent": "com.skorra.agent/com.skorra.agent.MdmDeviceAdminReceiver",
		"AgentPackage":   "com.skorra.agent",
		"Profiles":       profiles,
		"Groups":         groups,
		"HasAgentAPK":    os.Getenv("AGENT_APK_URL") != "" && os.Getenv("AGENT_APK_CHECKSUM") != "",
	})
}

// EnrollmentProfileCreate makes a new enrollment profile with a fresh "enr_..." token.
func (h *Handler) EnrollmentProfileCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		h.hxDoneToast(w, r, "/enrollment", "Profile name is required", "error")
		return
	}
	var groupID *uuid.UUID
	if gid := r.FormValue("group_id"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			groupID = &parsed
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	token := "enr_" + hex.EncodeToString(raw)
	if _, err := h.db.CreateEnrollmentProfile(r.Context(), name, token, groupID, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Enrollment profile created", "success")
}

func (h *Handler) enrollmentProfileSetRevoked(w http.ResponseWriter, r *http.Request, revoked bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	if err := h.db.SetEnrollmentProfileRevoked(r.Context(), id, revoked); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	msg := "Profile reactivated"
	if revoked {
		msg = "Profile revoked — its QR codes stop working immediately"
	}
	h.hxDoneToast(w, r, "/enrollment", msg, "success")
}

func (h *Handler) EnrollmentProfileRevoke(w http.ResponseWriter, r *http.Request) {
	h.enrollmentProfileSetRevoked(w, r, true)
}

func (h *Handler) EnrollmentProfileActivate(w http.ResponseWriter, r *http.Request) {
	h.enrollmentProfileSetRevoked(w, r, false)
}

func (h *Handler) EnrollmentProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteEnrollmentProfile(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Profile deleted", "success")
}

// EnrollmentProfileQR renders the Android managed-provisioning QR payload for a profile
// as a PNG. Scanned from the setup wizard (tap the welcome screen 6×), it installs the
// agent as Device Owner and hands it the server URL + enrollment token.
func (h *Handler) EnrollmentProfileQR(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetEnrollmentProfile(r.Context(), id)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	payload := map[string]any{
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME":   "com.skorra.agent/com.skorra.agent.MdmDeviceAdminReceiver",
		"android.app.extra.PROVISIONING_LEAVE_ALL_SYSTEM_APPS_ENABLED": true,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]string{
			"server_url":   h.baseURL(r),
			"enroll_token": p.Token,
		},
	}
	// QR provisioning needs a downloadable agent APK; without these env vars the code
	// still carries the extras (usable for docs/manual flows) but can't cold-provision.
	if apkURL := os.Getenv("AGENT_APK_URL"); apkURL != "" {
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"] = apkURL
	}
	if sum := os.Getenv("AGENT_APK_CHECKSUM"); sum != "" {
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"] = sum
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	png, err := qrcode.Encode(string(blob), qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store") // carries a live credential
	w.Write(png)
}

// ── System update policy ──────────────────────────────────────────────────────

// UpdatesPolicyPage manages the fleet-wide system-update policy the agent maps to
// DevicePolicyManager.setSystemUpdatePolicy (mode, install window, freeze periods).
func (h *Handler) UpdatesPolicyPage(w http.ResponseWriter, r *http.Request) {
	pol, _ := h.cfg.DevicePolicyKey("update_policy").(map[string]any)
	mode, _ := pol["mode"].(string)
	if mode == "" {
		mode = "default"
	}
	winStart, winEnd := 0, 240
	if v, ok := pol["window_start"].(float64); ok {
		winStart = int(v)
	}
	if v, ok := pol["window_end"].(float64); ok {
		winEnd = int(v)
	}
	var freezes []map[string]string
	if fp, ok := pol["freeze_periods"].([]any); ok {
		for _, f := range fp {
			if m, ok := f.(map[string]any); ok {
				s, _ := m["start"].(string)
				e, _ := m["end"].(string)
				freezes = append(freezes, map[string]string{"Start": s, "End": e})
			}
		}
	}
	h.render(w, r, "updates_policy.html", map[string]any{
		"Title":       "Updates policy",
		"ActivePage":  "updates-policy",
		"Mode":        mode,
		"WindowStart": minutesToHHMM(winStart),
		"WindowEnd":   minutesToHHMM(winEnd),
		"Freezes":     freezes,
	})
}

func minutesToHHMM(m int) string {
	if m < 0 || m >= 24*60 {
		m = 0
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func hhmmToMinutes(s string, fallback int) int {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return fallback
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return fallback
	}
	return hh*60 + mm
}

var monthDayRe = regexp.MustCompile(`^(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])$`)

// UpdatesPolicySave persists the fleet update policy; devices pick it up on their
// next check-in or live config frame.
func (h *Handler) UpdatesPolicySave(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("mode")
	switch mode {
	case "automatic", "windowed", "postpone", "default":
	default:
		h.hxDoneToast(w, r, "/updates-policy", "Unknown update mode", "error")
		return
	}
	if mode == "default" {
		if err := h.cfg.SetDevicePolicyKey("update_policy", nil); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.hxDoneToast(w, r, "/updates-policy", "Update policy cleared — devices return to user-controlled updates", "success")
		return
	}
	pol := map[string]any{"mode": mode}
	if mode == "windowed" {
		start := hhmmToMinutes(r.FormValue("window_start"), 0)
		end := hhmmToMinutes(r.FormValue("window_end"), 240)
		pol["window_start"] = start
		pol["window_end"] = end
	}
	var freezes []map[string]string
	starts, ends := r.Form["freeze_start"], r.Form["freeze_end"]
	for i := range starts {
		s, e := strings.TrimSpace(starts[i]), ""
		if i < len(ends) {
			e = strings.TrimSpace(ends[i])
		}
		if s == "" && e == "" {
			continue
		}
		if !monthDayRe.MatchString(s) || !monthDayRe.MatchString(e) {
			h.hxDoneToast(w, r, "/updates-policy", "Freeze periods must be MM-DD dates", "error")
			return
		}
		freezes = append(freezes, map[string]string{"start": s, "end": e})
	}
	if len(freezes) > 0 {
		pol["freeze_periods"] = freezes
	}
	if err := h.cfg.SetDevicePolicyKey("update_policy", pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/updates-policy", "Update policy saved — applies at each device's next config sync", "success")
}

// ── Network provisioning ──────────────────────────────────────────────────────
//
// The fleet `network` policy object the agent understands:
//   {"ca_certs":[{"name","pem_b64"}], "wifi_networks":[{"ssid","security":"wpa2"|"open","psk"}],
//    "vpn":{"package","lockdown"}}
// Each POST below reads the current object, mutates one section, and writes it back
// whole via SetDevicePolicyKey (clearing the key entirely when nothing is left).

// networkPolicy returns the current fleet network policy as a mutable map
// (DevicePolicy deep-copies, so edits here never touch shared state).
func (h *Handler) networkPolicy() map[string]any {
	m, _ := h.cfg.DevicePolicyKey("network").(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// saveNetworkPolicy persists the network object, removing the key when empty so
// devices with no network provisioning get no `network` entry at all.
func (h *Handler) saveNetworkPolicy(pol map[string]any) error {
	for k, v := range pol {
		if list, ok := v.([]any); ok && len(list) == 0 {
			delete(pol, k)
		}
	}
	if len(pol) == 0 {
		return h.cfg.SetDevicePolicyKey("network", nil)
	}
	return h.cfg.SetDevicePolicyKey("network", pol)
}

// netEntries pulls a []map section ("wifi_networks" / "ca_certs") out of the policy.
func netEntries(pol map[string]any, key string) []map[string]any {
	raw, _ := pol[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// NetworkPage renders the fleet network provisioning console: Wi-Fi networks,
// trusted CA certificates, and the always-on VPN, all backed by the `network`
// policy object delivered at each device's config sync.
func (h *Handler) NetworkPage(w http.ResponseWriter, r *http.Request) {
	pol := h.networkPolicy()
	type wifiRow struct {
		SSID, Security string
		HasPSK         bool
	}
	var wifis []wifiRow
	for _, m := range netEntries(pol, "wifi_networks") {
		ssid, _ := m["ssid"].(string)
		sec, _ := m["security"].(string)
		psk, _ := m["psk"].(string)
		wifis = append(wifis, wifiRow{SSID: ssid, Security: sec, HasPSK: psk != ""})
	}
	type caRow struct {
		Name string
		Size int
	}
	var cas []caRow
	for _, m := range netEntries(pol, "ca_certs") {
		name, _ := m["name"].(string)
		b64, _ := m["pem_b64"].(string)
		cas = append(cas, caRow{Name: name, Size: base64.StdEncoding.DecodedLen(len(b64))})
	}
	vpn, _ := pol["vpn"].(map[string]any)
	vpnPackage, _ := vpn["package"].(string)
	vpnLockdown, _ := vpn["lockdown"].(bool)
	h.render(w, r, "network.html", map[string]any{
		"Title":       "Network",
		"ActivePage":  "network",
		"Wifis":       wifis,
		"CACerts":     cas,
		"VPNPackage":  vpnPackage,
		"VPNLockdown": vpnLockdown,
	})
}

// NetworkWifiAdd adds (or, same SSID, replaces) a provisioned Wi-Fi network.
func (h *Handler) NetworkWifiAdd(w http.ResponseWriter, r *http.Request) {
	ssid := strings.TrimSpace(r.FormValue("ssid"))
	if ssid == "" || len(ssid) > 32 {
		h.hxDoneToast(w, r, "/network", "SSID is required (max 32 characters)", "error")
		return
	}
	security := r.FormValue("security")
	if security != "wpa2" && security != "open" {
		h.hxDoneToast(w, r, "/network", "Security must be WPA2 or open", "error")
		return
	}
	psk := r.FormValue("psk")
	if security == "wpa2" && (len(psk) < 8 || len(psk) > 63) {
		h.hxDoneToast(w, r, "/network", "WPA2 passphrase must be 8–63 characters", "error")
		return
	}
	if security == "open" {
		psk = ""
	}
	pol := h.networkPolicy()
	var wifis []any
	for _, m := range netEntries(pol, "wifi_networks") {
		if s, _ := m["ssid"].(string); s != ssid {
			wifis = append(wifis, m)
		}
	}
	wifis = append(wifis, map[string]any{"ssid": ssid, "security": security, "psk": psk})
	pol["wifi_networks"] = wifis
	if err := h.saveNetworkPolicy(pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/network", "Wi-Fi network saved — devices provision it at their next config sync", "success")
}

// NetworkWifiDelete removes a provisioned Wi-Fi network by SSID.
func (h *Handler) NetworkWifiDelete(w http.ResponseWriter, r *http.Request) {
	ssid := r.FormValue("ssid")
	pol := h.networkPolicy()
	var wifis []any
	for _, m := range netEntries(pol, "wifi_networks") {
		if s, _ := m["ssid"].(string); s != ssid {
			wifis = append(wifis, m)
		}
	}
	pol["wifi_networks"] = wifis
	if err := h.saveNetworkPolicy(pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/network", "Wi-Fi network removed", "success")
}

// NetworkCAAdd installs a trusted CA certificate: the pasted PEM is carried to
// devices base64-encoded (pem_b64) so the JSON stays clean of newlines.
func (h *Handler) NetworkCAAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		h.hxDoneToast(w, r, "/network", "Certificate name is required", "error")
		return
	}
	pem := strings.TrimSpace(r.FormValue("pem"))
	if !strings.Contains(pem, "BEGIN CERTIFICATE") || len(pem) > 64*1024 {
		h.hxDoneToast(w, r, "/network", "Paste a PEM certificate (-----BEGIN CERTIFICATE-----), max 64 KB", "error")
		return
	}
	pol := h.networkPolicy()
	var cas []any
	for _, m := range netEntries(pol, "ca_certs") {
		if n, _ := m["name"].(string); n != name {
			cas = append(cas, m)
		}
	}
	cas = append(cas, map[string]any{"name": name, "pem_b64": base64.StdEncoding.EncodeToString([]byte(pem))})
	pol["ca_certs"] = cas
	if err := h.saveNetworkPolicy(pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/network", "CA certificate added — installs at each device's next config sync", "success")
}

// NetworkCADelete removes a CA certificate by name.
func (h *Handler) NetworkCADelete(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	pol := h.networkPolicy()
	var cas []any
	for _, m := range netEntries(pol, "ca_certs") {
		if n, _ := m["name"].(string); n != name {
			cas = append(cas, m)
		}
	}
	pol["ca_certs"] = cas
	if err := h.saveNetworkPolicy(pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/network", "CA certificate removed", "success")
}

// vpnPackageRe is a plain Android package name (com.example.vpn).
var vpnPackageRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z][a-zA-Z0-9_]*)+$`)

// NetworkVPNSave sets or clears the always-on VPN package (+ optional lockdown,
// which blocks all traffic outside the tunnel).
func (h *Handler) NetworkVPNSave(w http.ResponseWriter, r *http.Request) {
	pol := h.networkPolicy()
	if r.FormValue("clear") == "1" {
		delete(pol, "vpn")
		if err := h.saveNetworkPolicy(pol); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.hxDoneToast(w, r, "/network", "Always-on VPN cleared", "success")
		return
	}
	pkg := strings.TrimSpace(r.FormValue("package"))
	if !vpnPackageRe.MatchString(pkg) {
		h.hxDoneToast(w, r, "/network", "Enter a valid VPN app package name (e.g. com.wireguard.android)", "error")
		return
	}
	pol["vpn"] = map[string]any{"package": pkg, "lockdown": r.FormValue("lockdown") == "on"}
	if err := h.saveNetworkPolicy(pol); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/network", "Always-on VPN saved — devices apply it at their next config sync", "success")
}

// ── Managed app configurations ────────────────────────────────────────────────
//
// The fleet `app_restrictions` policy array the agent applies via
// DevicePolicyManager.setApplicationRestrictions:
//   [{"package":"com.x","restrictions":{...}}]
// Each POST below reads the current array, mutates one entry, and writes it back
// whole via SetDevicePolicyKey (clearing the key entirely when nothing is left,
// mirroring the network code).

// appRestrictionEntries returns the current app_restrictions entries as []map
// (DevicePolicy deep-copies, so edits here never touch shared state).
func (h *Handler) appRestrictionEntries() []map[string]any {
	raw, _ := h.cfg.DevicePolicyKey("app_restrictions").([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// saveAppRestrictions persists the entries, removing the key when the list is
// empty so devices with no managed configurations get no `app_restrictions` at all.
func (h *Handler) saveAppRestrictions(entries []map[string]any) error {
	if len(entries) == 0 {
		return h.cfg.SetDevicePolicyKey("app_restrictions", nil)
	}
	list := make([]any, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	return h.cfg.SetDevicePolicyKey("app_restrictions", list)
}

// ManagedConfigsPage lists the fleet managed app configurations — per-package
// restriction bundles delivered at every config sync and applied by the agent via
// setApplicationRestrictions. Linked from the Apps page.
func (h *Handler) ManagedConfigsPage(w http.ResponseWriter, r *http.Request) {
	type mcRow struct {
		Package string
		JSON    string // pretty-printed restrictions object
		Keys    int
	}
	var rows []mcRow
	for _, m := range h.appRestrictionEntries() {
		pkg, _ := m["package"].(string)
		res, _ := m["restrictions"].(map[string]any)
		pretty, _ := json.MarshalIndent(res, "", "  ")
		rows = append(rows, mcRow{Package: pkg, JSON: string(pretty), Keys: len(res)})
	}
	h.render(w, r, "managed_configs.html", map[string]any{
		"Title":      "Managed configurations",
		"ActivePage": "setup",
		"Rows":       rows,
	})
}

// ManagedConfigSave adds (or, same package, replaces) a managed configuration.
func (h *Handler) ManagedConfigSave(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimSpace(r.FormValue("package"))
	if !vpnPackageRe.MatchString(pkg) {
		h.hxDoneToast(w, r, "/setup/managed-configs", "Enter a valid Android package name (e.g. com.example.app)", "error")
		return
	}
	var restrictions map[string]any
	raw := strings.TrimSpace(r.FormValue("restrictions"))
	if err := json.Unmarshal([]byte(raw), &restrictions); err != nil || restrictions == nil {
		h.hxDoneToast(w, r, "/setup/managed-configs", `Restrictions must be a JSON object, e.g. {"server_url":"https://…"}`, "error")
		return
	}
	entries := make([]map[string]any, 0, len(h.appRestrictionEntries())+1)
	for _, m := range h.appRestrictionEntries() {
		if p, _ := m["package"].(string); p != pkg {
			entries = append(entries, m)
		}
	}
	entries = append(entries, map[string]any{"package": pkg, "restrictions": restrictions})
	if err := h.saveAppRestrictions(entries); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/setup/managed-configs", "Managed configuration saved — applies at each device's next config sync", "success")
}

// ManagedConfigDelete removes a managed configuration by package.
func (h *Handler) ManagedConfigDelete(w http.ResponseWriter, r *http.Request) {
	pkg := r.FormValue("package")
	var entries []map[string]any
	for _, m := range h.appRestrictionEntries() {
		if p, _ := m["package"].(string); p != pkg {
			entries = append(entries, m)
		}
	}
	if err := h.saveAppRestrictions(entries); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/setup/managed-configs", "Managed configuration removed", "success")
}
