package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"

	"mdm/internal/apkstore"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/product"
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
	inbox, _ := h.db.ListOnboardingInbox(r.Context(), 50)
	restaurants, _ := h.db.ListRestaurants(r.Context())
	// Live refresh: the page re-fetches just the inbox on device events.
	if r.URL.Query().Get("partial") == "inbox" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.tmpl.ExecuteTemplate(w, "enroll-inbox", map[string]any{
			"Inbox": inbox, "Restaurants": restaurants, "Classes": product.Classes(), "Role": h.role(r),
		})
		return
	}
	profiles, _ := h.db.ListEnrollmentProfiles(r.Context())
	groups, _ := h.db.ListGroups(r.Context())
	stats, _ := h.db.EnrollmentStats(r.Context())
	h.render(w, r, "enrollment.html", map[string]any{
		"Title":          "Enrollment",
		"ActivePage":     "enrollment",
		"ServerURL":      h.baseURL(r),
		"DeviceKey":      deviceKey,
		"DeviceKeyMask":  masked,
		"AdminComponent": "aio.app.mdmclient.dpc/aio.app.mdmclient.dpc.MdmDeviceAdminReceiver",
		"AgentPackage":   "aio.app.mdmclient.dpc",
		"Profiles":       profiles,
		"Groups":         groups,
		"Restaurants":    restaurants,
		"Classes":        product.Classes(),
		"Inbox":          inbox,
		"Stats":          stats,
		"HasAgentAPK":    h.cfg.AgentAPKHosted() || (h.cfg.AgentAPKURL() != "" && h.cfg.AgentAPKChecksum() != ""),
		"AgentAPKURL":    h.agentAPKURL(r),
	})
}

// EnrollmentProfileCreate makes a new enrollment profile with a fresh "enr_..." token.
func (h *Handler) EnrollmentProfileCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		h.hxDoneToast(w, r, "/enrollment", "Profile name is required", "error")
		return
	}
	in := enrollmentProfileInputFromForm(r)
	in.Name = name
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	in.Token = "enr_" + hex.EncodeToString(raw)
	if _, err := h.db.CreateEnrollmentProfile(r.Context(), in); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Enrollment profile created", "success")
}

// enrollmentProfileInputFromForm reads the profile intent fields shared by create and
// edit: group, notes, class, site, expiry (days from now), max enrolls.
func enrollmentProfileInputFromForm(r *http.Request) db.EnrollmentProfileInput {
	in := db.EnrollmentProfileInput{Notes: strings.TrimSpace(r.FormValue("notes"))}
	if gid := r.FormValue("group_id"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			in.GroupID = &parsed
		}
	}
	if rid := r.FormValue("restaurant_id"); rid != "" {
		if parsed, err := uuid.Parse(rid); err == nil {
			in.RestaurantID = &parsed
		}
	}
	if c := strings.ToLower(strings.TrimSpace(r.FormValue("device_class"))); product.IsClass(c) {
		in.DeviceClass = c
	}
	if days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("expires_days"))); err == nil && days > 0 {
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		in.ExpiresAt = &t
	}
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_enrolls"))); err == nil && n > 0 {
		in.MaxEnrolls = &n
	}
	return in
}

// EnrollmentProfileUpdate edits a profile's intent (everything but the token).
func (h *Handler) EnrollmentProfileUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	in := enrollmentProfileInputFromForm(r)
	in.Name = strings.TrimSpace(r.FormValue("name"))
	if in.Name == "" {
		h.hxDoneToast(w, r, "/enrollment", "Profile name is required", "error")
		return
	}
	if err := h.db.UpdateEnrollmentProfile(r.Context(), id, in); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Profile updated", "success")
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
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME":   "aio.app.mdmclient.dpc/aio.app.mdmclient.dpc.MdmDeviceAdminReceiver",
		"android.app.extra.PROVISIONING_LEAVE_ALL_SYSTEM_APPS_ENABLED": true,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]string{
			"server_url":   h.baseURL(r),
			"enroll_token": p.Token,
		},
	}
	// QR provisioning needs a downloadable agent APK; without these env vars the code
	// still carries the extras (usable for docs/manual flows) but can't cold-provision.
	// A hosted APK wins: this server serves it and knows its file hash (the
	// PACKAGE_CHECKSUM variant). Otherwise an external URL + signing-certificate
	// checksum from Settings / env.
	if h.cfg.AgentAPKHosted() {
		sha, _, _, _ := h.cfg.AgentAPKHostedInfo()
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"] = h.baseURL(r) + agentAPKRoute
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_CHECKSUM"] = sha
		// PACKAGE_CHECKSUM (the file hash) has been deprecated since API 26, and a modern
		// setup wizard may ignore it. When the signing-certificate checksum is also known,
		// send it too: Android prefers it and checks the signer instead of the bytes, which
		// also survives re-hosting the same app signed with the same key.
		if sum := h.cfg.AgentAPKChecksum(); sum != "" {
			payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"] = sum
		}
	} else {
		if apkURL := h.cfg.AgentAPKURL(); apkURL != "" {
			payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"] = apkURL
		}
		if sum := h.cfg.AgentAPKChecksum(); sum != "" {
			payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"] = sum
		}
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// ?format=json shows the provisioning payload as text (support / verification
	// without scanning the code).
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(blob)
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

// vpnPackageRe is a plain Android package name (com.example.vpn).
var vpnPackageRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z][a-zA-Z0-9_]*)+$`)

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

// ── Geofencing ────────────────────────────────────────────────────────────────

// geoFix is one device's usable GPS fix, parsed out of latest_extra
// (location_lat / location_lon / location_age_s, reported by the agent only
// while fleet policy location_enabled is on).
type geoFix struct {
	Serial   string
	Lat, Lon float64
}

// geofenceView is one fence plus the live inside/outside split of located devices.
type geofenceView struct {
	db.Geofence
	Inside  []string
	Outside []string
}

// staleLocationAfter is how old a fix may be before the device counts as unlocated:
// the age the agent reported at check-in plus the time since we last heard from it.
const staleLocationAfter = time.Hour

// deviceGeoFix extracts a fresh GPS fix from a device's latest extra payload.
// ok is false when the device never reported one or the fix has gone stale.
func deviceGeoFix(d db.Device) (fix geoFix, ok bool) {
	if len(d.LatestExtra) == 0 {
		return fix, false
	}
	var m struct {
		Lat  *float64 `json:"location_lat"`
		Lon  *float64 `json:"location_lon"`
		AgeS float64  `json:"location_age_s"`
	}
	if json.Unmarshal(d.LatestExtra, &m) != nil || m.Lat == nil || m.Lon == nil {
		return fix, false
	}
	age := time.Duration(m.AgeS)*time.Second + time.Since(d.LastSeenAt)
	if age > staleLocationAfter {
		return fix, false
	}
	return geoFix{Serial: d.SerialNumber, Lat: *m.Lat, Lon: *m.Lon}, true
}

// haversineM is the great-circle distance between two lat/lon points in metres.
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusM = 6371000
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat, dLon := rad(lat2-lat1), rad(lon2-lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}

// ── Compliance ────────────────────────────────────────────────────────────────

// complianceIssue is one failed rule for one device, in words, carrying the
// rule's severity ("warn" | "violation") so the template can pick the chip colour.
type complianceIssue struct {
	Text     string
	Severity string
}

// complianceRow is one device's compliance state. Exported fields so templates can read them.
type complianceRow struct {
	Serial    string
	Build     string
	Battery   int
	Online    bool
	Compliant bool // no violation-severity issues (warn-only devices stay compliant)
	Warned    bool // has at least one warn-severity issue
	Issues    []complianceIssue
}

// complianceKinds is the fixed set of rule kinds the evaluator understands,
// mapped to the parameter each needs ("" = none, "int" / "text" otherwise).
// Keep in sync with ruleLabel, complianceIssues and the kind <select> in
// compliance.html when adding a kind.
var complianceKinds = map[string]string{
	"require_screen_lock":  "",
	"require_encryption":   "",
	"forbid_adb":           "",
	"min_battery":          "int",
	"require_build_prefix": "text",
	"max_offline_hours":    "int",
	// Security posture, reported by firmware, the DPC agent and MDM-lite alike.
	"forbid_dev_options":     "",
	"forbid_unknown_sources": "",
	"forbid_root":            "",
	"forbid_network_adb":     "",
	"require_play_protect":   "",
	"allowed_accessibility":  "text", // comma-separated allowlist of services (empty: none allowed)
	"forbid_apps":            "text", // comma-separated package names
}

// ruleLabel renders a rule as words for the rules list ("Battery at least 30%").
func ruleLabel(ru db.ComplianceRule) string {
	switch ru.Kind {
	case "require_screen_lock":
		return "Screen lock required"
	case "require_encryption":
		return "Storage encryption required"
	case "forbid_adb":
		return "ADB debugging forbidden"
	case "min_battery":
		return "Battery at least " + ru.Param + "%"
	case "require_build_prefix":
		return "OS build starts with " + ru.Param
	case "max_offline_hours":
		return "Seen within the last " + ru.Param + " hours"
	case "forbid_dev_options":
		return "Developer options forbidden"
	case "forbid_unknown_sources":
		return "Installs from unknown sources forbidden"
	case "forbid_root":
		return "Root (su) forbidden"
	case "forbid_network_adb":
		return "ADB over the network forbidden"
	case "require_play_protect":
		return "Play Protect required"
	case "allowed_accessibility":
		if strings.TrimSpace(ru.Param) == "" {
			return "No accessibility services"
		}
		return "Accessibility services only: " + ru.Param
	case "forbid_apps":
		return "Apps forbidden: " + ru.Param
	}
	return ru.Kind
}

// splitList parses a rule's comma/space-separated parameter into a set.
func splitList(param string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(param, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out[strings.TrimSpace(f)] = true
	}
	return out
}

// complianceIssues evaluates every enabled rule against one device and returns
// the failures in words. Posture flags the agent never reported (absent from
// latest_extra) are unknown, not failures — a fleet of legacy T7 clients doesn't
// go red the moment a posture rule is added. Shared by CompliancePage and the
// compliance CSV report so the two can never disagree.
//
// installed is the device's installed packages that appear in a forbid_apps rule
// (nil when no such rule is enabled).
func complianceIssues(rules []db.ComplianceRule, d db.Device, installed map[string]bool) []complianceIssue {
	var posture struct {
		ScreenLockSet    *bool     `json:"screen_lock_set"`
		AdbEnabled       *bool     `json:"adb_enabled"`
		StorageEncrypted *bool     `json:"storage_encrypted"`
		DevOptions       *bool     `json:"dev_options_enabled"`
		UnknownSources   *bool     `json:"unknown_sources"`
		UnknownApps      []string  `json:"unknown_source_apps"`
		SuPresent        *bool     `json:"su_present"`
		AdbTCP           *bool     `json:"adb_tcp"`
		PlayProtect      *bool     `json:"play_protect"`
		Accessibility    *[]string `json:"accessibility_services"`
	}
	if len(d.LatestExtra) > 0 {
		_ = json.Unmarshal(d.LatestExtra, &posture)
	}
	var issues []complianceIssue
	for _, ru := range rules {
		if !ru.Enabled {
			continue
		}
		var text string
		switch ru.Kind {
		case "require_screen_lock":
			if posture.ScreenLockSet != nil && !*posture.ScreenLockSet {
				text = "Screen lock not set"
			}
		case "require_encryption":
			if posture.StorageEncrypted != nil && !*posture.StorageEncrypted {
				text = "Storage not encrypted"
			}
		case "forbid_adb":
			if posture.AdbEnabled != nil && *posture.AdbEnabled {
				text = "ADB debugging enabled"
			}
		case "min_battery":
			if n, err := strconv.Atoi(ru.Param); err == nil && d.BatteryPct > 0 && d.BatteryPct < n {
				text = fmt.Sprintf("Battery below %d%%", n)
			}
		case "require_build_prefix":
			if ru.Param != "" && !strings.HasPrefix(d.BuildID, ru.Param) {
				text = "OS build not on " + ru.Param
			}
		case "max_offline_hours":
			if n, err := strconv.Atoi(ru.Param); err == nil && n > 0 && time.Since(d.LastSeenAt) > time.Duration(n)*time.Hour {
				text = fmt.Sprintf("Offline for more than %d hours", n)
			}
		case "forbid_dev_options":
			if posture.DevOptions != nil && *posture.DevOptions {
				text = "Developer options enabled"
			}
		case "forbid_unknown_sources":
			if posture.UnknownSources != nil && *posture.UnknownSources {
				text = "Unknown sources allowed"
				if len(posture.UnknownApps) > 0 {
					text += " (" + strings.Join(posture.UnknownApps, ", ") + ")"
				}
			}
		case "forbid_root":
			if posture.SuPresent != nil && *posture.SuPresent {
				text = "Rooted: su binary present"
			}
		case "forbid_network_adb":
			if posture.AdbTCP != nil && *posture.AdbTCP {
				text = "ADB listening on the network"
			}
		case "require_play_protect":
			if posture.PlayProtect != nil && !*posture.PlayProtect {
				text = "Play Protect off"
			}
		case "allowed_accessibility":
			if posture.Accessibility != nil {
				allowed := splitList(ru.Param)
				var extra []string
				for _, s := range *posture.Accessibility {
					if !allowed[s] && !allowed[strings.SplitN(s, "/", 2)[0]] {
						extra = append(extra, s)
					}
				}
				if len(extra) > 0 {
					text = "Unexpected accessibility service: " + strings.Join(extra, ", ")
				}
			}
		case "forbid_apps":
			var found []string
			for p := range splitList(ru.Param) {
				if installed[p] {
					found = append(found, p)
				}
			}
			if len(found) > 0 {
				sort.Strings(found)
				text = "Forbidden app installed: " + strings.Join(found, ", ")
			}
		}
		if text != "" {
			issues = append(issues, complianceIssue{Text: text, Severity: ru.Severity})
		}
	}
	return issues
}

// evaluateCompliance turns the device list into sorted compliance rows (worst
// first): a device is non-compliant when any violation-severity rule fails;
// warn-only failures show amber but don't break compliance.
func (h *Handler) evaluateCompliance(rules []db.ComplianceRule, devs []db.Device) (rows []complianceRow, compliant int) {
	rows = make([]complianceRow, 0, len(devs))
	// Installed packages matter only for forbid_apps: load just the forbidden ones.
	var forbidden []string
	for _, ru := range rules {
		if ru.Enabled && ru.Kind == "forbid_apps" {
			for p := range splitList(ru.Param) {
				forbidden = append(forbidden, p)
			}
		}
	}
	var installed map[uuid.UUID]map[string]bool
	if len(forbidden) > 0 {
		installed, _ = h.db.DevicesWithPackages(context.Background(), forbidden)
	}
	for _, d := range devs {
		issues := complianceIssues(rules, d, installed[d.ID])
		row := complianceRow{
			Serial:    d.SerialNumber,
			Build:     d.BuildID,
			Battery:   d.BatteryPct,
			Online:    h.hub.IsConnectedForDisplay(d.ID),
			Compliant: true,
			Issues:    issues,
		}
		for _, is := range issues {
			if is.Severity == "warn" {
				row.Warned = true
			} else {
				row.Compliant = false
			}
		}
		if row.Compliant {
			compliant++
		}
		rows = append(rows, row)
	}
	// Violations first, then warn-only devices, so problems ride to the top.
	rank := func(x complianceRow) int {
		switch {
		case !x.Compliant:
			return 0
		case x.Warned:
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i]) < rank(rows[j]) })
	return rows, compliant
}

// complianceRuleView is one stored rule plus its display label for the rules list.
type complianceRuleView struct {
	db.ComplianceRule
	Label string
}

// ── Fleet reports (CSV) ───────────────────────────────────────────────────────

// reportCSV sets the download headers for a dated fleet report and returns the
// CSV writer (caller must Flush).
func reportCSV(w http.ResponseWriter, name string) *csv.Writer {
	filename := fmt.Sprintf("aio-mdm-%s-%s.csv", name, time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	return csv.NewWriter(w)
}

// ReportInventoryCSV is the one-click fleet inventory download: one row per device.
func (h *Handler) ReportInventoryCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 5000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	groups, _ := h.db.GroupNamesByDevice(ctx)
	cw := reportCSV(w, "inventory")
	cw.Write([]string{"serial", "product", "model", "build_id", "battery_pct", "online", "last_seen", "groups", "restaurant"})
	for _, d := range devs {
		online := "no"
		if h.hub.IsConnectedForDisplay(d.ID) {
			online = "yes"
		}
		cw.Write([]string{
			d.SerialNumber,
			d.ProductLabel(),
			extraString(d.LatestExtra, "model"),
			d.BuildID,
			strconv.Itoa(d.BatteryPct),
			online,
			d.LastSeenAt.UTC().Format(time.RFC3339),
			strings.Join(groups[d.ID], ";"),
			d.RestaurantName,
		})
	}
	cw.Flush()
}

// ReportActivityCSV is the last 7 days of fleet activity, one row per day, from
// the device_daily_stats rollups (so "today" reflects the last rollup).
func (h *Handler) ReportActivityCSV(w http.ResponseWriter, r *http.Request) {
	stats, err := h.db.GetFleetDailyStats(r.Context(), 7)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	cw := reportCSV(w, "activity")
	cw.Write([]string{"date", "devices_seen", "checkins"})
	for _, s := range stats {
		cw.Write([]string{s.Day.Format("2006-01-02"), strconv.Itoa(s.Active), strconv.FormatInt(s.Checkins, 10)})
	}
	cw.Flush()
}

// agentAPKURL is the address a device downloads the agent from: this server's hosted
// copy when one is uploaded, else the external URL from Settings / env.
func (h *Handler) agentAPKURL(r *http.Request) string {
	if h.cfg.AgentAPKHosted() {
		return h.baseURL(r) + agentAPKRoute
	}
	return h.cfg.AgentAPKURL()
}

// agentUpdateTarget is the agent build this server is offering: the URL a device
// downloads it from, what it is, and the digest the agent verifies before replacing
// itself. ok is false when nothing is hosted, or when the hosted file's version
// could not be read — without a version code there is no way to tell whether a
// device is behind, so no update is offered.
func (h *Handler) agentUpdateTarget(r *http.Request, slot string) (url, pkg, version string, code int64, sha256Hex string, ok bool) {
	b, found := h.cfg.AgentAPKSlot(slot)
	if !found || b.Package == "" || b.VersionCode == 0 {
		return "", "", "", 0, "", false
	}
	name, known := AgentAPKSlots[slot]
	if !known {
		return "", "", "", 0, "", false
	}
	return h.baseURL(r) + "/agent/" + name, b.Package, b.Version, b.VersionCode, b.SHA256Hex, true
}

// agentUpdatePayload is what an app_update command carries: the package the agent
// must recognise as itself, the digest it checks the download against, and the
// version name for the activity log.
func agentUpdatePayload(pkg, version, sha256Hex string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"package": pkg, "version": version, "sha256": sha256Hex})
	return json.RawMessage(b)
}

// clientVersionOf is the build a device is actually running for its slot. For MDM-lite
// that is the *host app*: the library version says which Lite is embedded, but what a
// slot hosts and installs is the whole app, so comparing the library version against it
// would call a device behind a build it is already running.
func clientVersionOf(d *db.Device) (name string, code int64) {
	if d.IsMDMLite() {
		return extraNested(d.LatestExtra, "host_app", "version_name"),
			extraNestedInt64(d.LatestExtra, "host_app", "version_code")
	}
	name = extraString(d.LatestExtra, "agent_version")
	if name == "—" {
		name = ""
	}
	return name, extraInt64(d.LatestExtra, "agent_version_code")
}

// frameworkVersionReported spots the pre-1.0.2 bug in our own firmware client: it read
// the *framework's* version and sent that as its own, so a fleet of clients reported
// "15" (the platform release) with code 35 (the SDK). versionComparable already refuses
// to measure those against a hosted build; this is the cosmetic half, so the pages stop
// repeating a number that was never a client version.
//
// The version NAME is what gives it away. Every build of ours is dotted — 1.0.4, 1.2.0 —
// and a platform release is a bare integer. The code cannot be used for this: our own
// client passed through code 35 on the way to 40, so 35 alone proves nothing.
func frameworkVersionReported(name string, pkg string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "—" {
		return false
	}
	if pkg != "" && pkg != "—" {
		return false // it names its own package, so it is new enough to be believed
	}
	n, err := strconv.Atoi(name)
	// Android releases are small integers; a dotted version fails Atoi outright.
	return err == nil && n > 0 && n <= 30
}

// clientPackageOf is the app a device says its client is, "" when it does not say.
// Firmware clients before 1.0.2 reported the *framework's* version by accident — the
// platform release (15) and SDK (35) — which sailed past every "is it newer?" test and
// showed a whole fleet as up to date. Comparing packages makes that impossible: a
// version is only believed when it belongs to the app the slot hosts.
func clientPackageOf(d *db.Device) string {
	if d.IsMDMLite() {
		return extraNested(d.LatestExtra, "host_app", "package")
	}
	return extraString(d.LatestExtra, "agent_package")
}

// versionComparable reports whether a device's reported version can be measured
// against the hosted build. A device that does not name its package gets the benefit
// of the doubt (older clients never sent one); one naming a different package does not.
func versionComparable(d *db.Device, hostedPackage string) bool {
	pkg := clientPackageOf(d)
	if pkg == "" || pkg == "—" || hostedPackage == "" {
		return true
	}
	return pkg == hostedPackage
}

// AgentUpdateState is what the device page needs to word (or hide) the "Update
// agent" action: whether a newer hosted build exists for this device, and the two
// versions involved.
type AgentUpdateState struct {
	Available bool   // a hosted build, newer than what this device runs
	Version   string // the hosted build's version name
	Current   string // what the device last reported ("" = never reported one)
	Hosted    bool   // an APK is hosted at all (false = nothing uploaded yet)
	// Elsewhere names the MDM the device reports to, when that is not this one. The
	// action is hidden rather than disabled: it is not "not yet", it is "not here".
	Elsewhere string
}

// agentUpdateFor compares the hosted agent build with what a device reports. Only
// agents that advertise self_update are offered one; a device that has never
// reported its version code is treated as behind, since it predates the reporting
// and any hosted build is newer than it.
func (h *Handler) agentUpdateFor(r *http.Request, d *db.Device) AgentUpdateState {
	st := AgentUpdateState{Hosted: h.cfg.AgentAPKHosted()}
	if d == nil || !d.Supports(product.CapSelfUpdate) {
		return st
	}
	slot := agentSlotFor(d)
	st.Hosted = false
	if _, hosted := h.cfg.AgentAPKSlot(slot); hosted {
		st.Hosted = true
	}
	_, _, version, code, _, ok := h.agentUpdateTarget(r, slot)
	if !ok {
		return st
	}
	st.Version = version
	current, cur := clientVersionOf(d)
	st.Current = current
	_, hostedPkg, _, _, _, _ := h.agentUpdateTarget(r, slot)
	st.Available = versionComparable(d, hostedPkg) && cur < code
	// A device reporting to another MDM downloads its client from that server. Offering
	// the button here queues an install the device will never come back for.
	if c, err := h.db.DeviceCustody(r.Context(), d.ID); err == nil && c.Elsewhere() {
		st.Available, st.Elsewhere = false, c.Server
	}
	return st
}

// ── Clients page ──────────────────────────────────────────────────────────────

// ClientSlotView is one hosted agent build and how the fleet stands against it.
// ClientRailEntry is one row of the clients rail. It is not one-to-one with a slot:
// the firmware client is signed once per tree and so has a slot per tree, but it is
// one client and gets one row, with the trees as variants inside it.
type ClientRailEntry struct {
	Label    string
	Slot     string // the slot opening this row lands on
	Devices  int
	Behind   int
	Selected bool
}

type ClientSlotView struct {
	Slot     string
	Label    string
	Variant  string // short name inside a group ("QCOM · v2.1.x"); empty when ungrouped
	Note     string // what this slot is for, in words
	Hosted   bool
	Build    config.AgentAPKBuild
	URL      string
	Devices  int // devices this slot applies to
	UpToDate int
	Behind   int
	Unknown  int // never reported a version — an older client, or one that predates reporting
	Versions []ClientVersionCount
	// Filled for the selected client only: the page shows one client's devices and
	// releases at a time, so there is no reason to build them for the other four.
	Rows     []ClientDeviceRow
	Groups   []ClientDeviceGroup
	History  []ClientHistoryRow
	Selected bool
	// True while any row is mid-update, so the page can poll itself instead of
	// leaving someone staring at a button that came straight back.
	Updating bool
	// Filled for the selected client when it belongs to a group: the sibling variants,
	// in rail order, for the switcher above the detail. Empty for an ungrouped client.
	GroupLabel string
	GroupNote  string
	Variants   []*ClientSlotView
	// The serials "Update all behind" targets, built here rather than joined in the
	// template — a comma emitted from a loop index breaks the moment the sort changes.
	BehindSerials string
}

// ClientHistoryRow is one past publish on the Releases list. Archived says whether the
// APK's bytes are still on disk under their digest: builds published before the archive
// existed have a row and no artifact, and the row has to say so rather than offer a
// download that 404s.
type ClientHistoryRow struct {
	Build    config.AgentAPKBuild
	Archived bool
	Hosted   bool // the build this slot serves right now
}

// ClientVersionCount is one version and how many devices run it.
type ClientVersionCount struct {
	Version string
	Code    int64
	Count   int
	Current bool
}

// ClientDeviceRow is one device in the selected client's table.
type ClientDeviceRow struct {
	Serial   string
	Version  string // what it reports ("" = never reported one)
	Code     int64
	State    string // "current" | "behind" | "unknown" | "updating"
	Progress string // for "updating": the device's own word (pending/downloading/installing)
	LastSeen time.Time
	// The client sent the framework's version instead of its own (the pre-1.0.2 bug).
	// Version is blanked when this is set: "15" was never a client version.
	Misreported bool
	// Elsewhere: the device is reporting to another MDM, so this server cannot update
	// it however far behind it is. Carries the peer's name for the row to say where.
	Elsewhere string
	// ElsewhereURL is that server's dashboard, when it is a configured peer.
	ElsewhereURL string
	// Unreachable marks a device that has not checked in for long enough that an
	// update queued now would sit unclaimed. Not proof it moved — proof only that
	// pressing Update would achieve nothing anyone could see.
	Unreachable bool
	// Dormant devices have not checked in for clientDormantAfter. They are still
	// listed, but folded away: most of them are retired or boxed hardware, and left
	// inline they outnumber the rows an operator came to act on.
	Dormant bool
}

// clientDormantAfter is how long a device can go unseen before its row folds away in
// the devices list. Two weeks: longer than a closed-over-the-holidays restaurant, far
// shorter than the 60-90 day gaps the never-reported group is full of.
const clientDormantAfter = 14 * 24 * time.Hour

// ClientDeviceGroup is one labelled block of the devices table: the rows, what the
// group is called, and why its devices are in it. Grouping is the whole point — a flat
// list sorts the actionable rows to the top and then buries them under everything that
// cannot be acted on at all.
type ClientDeviceGroup struct {
	Key     string // behind | unknown | current
	Label   string
	Why     string // what an operator can do about this group, in words
	Rows    []ClientDeviceRow
	Dormant []ClientDeviceRow // unseen for clientDormantAfter, folded behind a count
}

// groupClientRows splits the devices table into the three blocks the page is actually
// about — what can be updated now, what cannot be updated from here at all, and what is
// already done — and folds each block's long-unseen devices away behind a count. A
// device mid-update stays with "behind": it is behind, it is just already being dealt
// with. Empty groups are dropped rather than rendered as a header over nothing.
func groupClientRows(rows []ClientDeviceRow, hosted string, now time.Time) []ClientDeviceGroup {
	behindWhy := "can install " + hosted + " now"
	if hosted == "" {
		behindWhy = "nothing is hosted for this client yet"
	}
	currentWhy := "on " + hosted
	if hosted == "" {
		currentWhy = "on the newest build seen"
	}
	defs := []ClientDeviceGroup{
		{Key: "behind", Label: "Behind", Why: behindWhy},
		{Key: "unknown", Label: "Never reported a version",
			Why: "needs a firmware OTA — the client on them predates client OTA"},
		{Key: "elsewhere", Label: "On another MDM",
			Why: "reporting to another server — it collects its updates there, not here"},
		{Key: "current", Label: "Up to date", Why: currentWhy},
	}
	at := map[string]int{"behind": 0, "updating": 0, "unknown": 1, "current": 3}
	for _, row := range rows {
		i, ok := at[row.State]
		if !ok {
			i = 1
		}
		// Custody outranks version: a device on another server is not ours to update,
		// whatever version it last told us about. Grouping it anywhere else would put
		// an Update button on a device that cannot receive one.
		if row.Elsewhere != "" {
			i = 2
		}
		if now.Sub(row.LastSeen) > clientDormantAfter {
			row.Dormant = true
			defs[i].Dormant = append(defs[i].Dormant, row)
			continue
		}
		defs[i].Rows = append(defs[i].Rows, row)
	}
	out := make([]ClientDeviceGroup, 0, len(defs))
	for _, g := range defs {
		if len(g.Rows) == 0 && len(g.Dormant) == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}

// clientHistoryRows pairs each past publish with whether its APK is still on disk. The
// archive is keyed by digest, so this is a stat per row — cheap, and the only way to
// know: the history is config, the bytes are a file, and before the archive existed the
// two could not agree.
func clientHistoryRows(slot string, hist []config.AgentAPKBuild, hostedSHA string) []ClientHistoryRow {
	out := make([]ClientHistoryRow, 0, len(hist))
	for _, b := range hist {
		row := ClientHistoryRow{Build: b, Hosted: b.SHA != "" && b.SHA == hostedSHA}
		if p := agentAPKArchivePath(slot, b.SHA256Hex); p != "" {
			if _, err := os.Stat(p); err == nil {
				row.Archived = true
			}
		}
		out = append(out, row)
	}
	return out
}

// ClientsPage is client management: which build of which agent each device runs,
// what this server hosts for it, and how far behind the fleet is. The agent is the
// thing that makes every other page work, so it gets a page rather than a corner of
// Settings.
// clientSlotOrder and clientSlotLabels are the client catalogue, shared by the
// Clients page and the device page's client pill so the two can never drift.
// Menu board + Lite dropped from the active client lineup entirely (2026-10-05) — just
// firmware and the standard (DPC) client now.
var clientSlotOrder = []string{"firmware-qcom", "firmware-gms", "dpc"}

var clientSlotLabels = map[string]string{
	"dpc":           "Standard MDM",
	"firmware-qcom": "Firmware MDM · QCOM (T7, kiosks)",
	"firmware-gms":  "Firmware MDM · GMS (frozen)",
}

// ClientPillView is the device hero's client pill: which client manages this device,
// the version it runs, whether that is the hosted build, and — for the popover the
// pill opens — every client with its hosted version plus this client's changelog.
// One pill in one place for firmware, DPC and Lite alike.
type ClientPillView struct {
	Kind    string // "Firmware MDM" | "Standard MDM" | "MDM Lite"
	Title   string // the tooltip the old tag carried
	Version string // what the device reports ("" = it has never said)
	// Set when the device reported the framework's version rather than its own; the
	// pill shows the reason instead of the number.
	Misreported bool
	Slot        string
	SlotLabel   string
	Hosted      bool // a build is hosted for this slot at all
	Latest      string
	Behind      bool
	Clients     []ClientPillRow
	Changelog   []config.AgentAPKBuild
}

// ClientPillRow is one client in the pill's popover.
type ClientPillRow struct {
	Slot    string
	Label   string
	Version string // hosted version, "" when nothing is hosted for it
	This    bool   // the client this device runs
}

// clientPillFor builds the pill for one device. Everything it needs is already on
// the page's data path: the device's reported version, and the hosted build per slot.
func (h *Handler) clientPillFor(d *db.Device) ClientPillView {
	v := ClientPillView{Kind: "Firmware MDM", Title: "Our hardware running the system-app client"}
	if d == nil {
		return v
	}
	switch {
	case d.IsMDMLite():
		v.Kind = "MDM Lite"
		v.Title = "Reported by the MDM Lite library inside an app (no Device Owner): vitals and that app's crashes, over periodic check-ins with no live connection."
	case d.IsDPC():
		v.Kind = "Standard MDM"
		v.Title = "Managed by the AIO MDM standard client (Device Owner) on a stock device."
	}
	v.Version, _ = clientVersionOf(d)
	if frameworkVersionReported(v.Version, clientPackageOf(d)) {
		v.Version, v.Misreported = "", true
	}
	v.Slot = agentSlotFor(d)
	v.SlotLabel = clientSlotLabels[v.Slot]
	for _, slot := range clientSlotOrder {
		row := ClientPillRow{Slot: slot, Label: clientSlotLabels[slot], This: slot == v.Slot}
		if b, hosted := h.cfg.AgentAPKSlot(slot); hosted {
			row.Version = b.Version
		}
		v.Clients = append(v.Clients, row)
	}
	b, hosted := h.cfg.AgentAPKSlot(v.Slot)
	if !hosted {
		return v
	}
	v.Hosted, v.Latest = true, b.Version
	// "Behind" is only claimed when the two versions are comparable at all — a client
	// reporting another package's version would otherwise read as up to date forever.
	if _, code := clientVersionOf(d); code > 0 && versionComparable(d, b.Package) {
		v.Behind = code < b.VersionCode
	}
	v.Changelog = h.cfg.AgentAPKHistory(v.Slot)
	if len(v.Changelog) > 4 {
		v.Changelog = v.Changelog[:4]
	}
	return v
}

func (h *Handler) ClientsPage(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	order := clientSlotOrder
	labels := clientSlotLabels
	// The firmware client is one client with one build per tree, because each tree
	// signs with its own platform key. That split has to stay — a device only installs
	// the key it already trusts — but it is not three clients, so it is one rail row
	// with the trees as variants inside it.
	firmwareGroup := []string{"firmware-qcom", "firmware-gms"}
	variants := map[string]string{
		"firmware-qcom": "QCOM · v2.1.x",
		"firmware-gms":  "GMS · v2.0.x",
	}
	const firmwareLabel = "Firmware MDM"
	const firmwareNote = "The system app in our AOSP images. One build per tree, covering that tree's user and userdebug builds alike: the client is platform-signed, and the platform key belongs to the tree, not the variant."
	notes := map[string]string{
		"dpc":           "Stock Android devices running the Device Owner agent. This build is also what a factory-reset device downloads from the enrollment QR, or what tools/enroll-adb.sh installs over adb.",
		"firmware-qcom": "Devices on the QCOM tree (v2.1.x), user and userdebug alike. Signed with that tree's platform key — a GMS device cannot install this build. The only actively published firmware line.",
		"firmware-gms":  "Devices on the GMS tree (v2.0.x), user and userdebug alike. Signed with that tree's platform key, which differs from QCOM's. GMS is no longer built — this slot stays only for devices still out there on it.",
	}

	views := map[string]*ClientSlotView{}
	counts := map[string]map[int64]*ClientVersionCount{}
	for _, slot := range order {
		b, hosted := h.cfg.AgentAPKSlot(slot)
		v := &ClientSlotView{Slot: slot, Label: labels[slot], Variant: variants[slot], Note: notes[slot], Hosted: hosted, Build: b}
		if hosted {
			v.URL = h.baseURL(r) + "/agent/" + AgentAPKSlots[slot]
		}
		views[slot] = v
		counts[slot] = map[int64]*ClientVersionCount{}
	}

	for i := range devices {
		d := &devices[i]
		v := views[agentSlotFor(d)]
		if v == nil {
			continue
		}
		v.Devices++
		name, code := clientVersionOf(d)
		switch {
		case code == 0 || !versionComparable(d, v.Build.Package):
			v.Unknown++
		case v.Hosted && code >= v.Build.VersionCode:
			v.UpToDate++
		case v.Hosted:
			v.Behind++
		}
		_ = name
		if code == 0 && name == "" {
			continue
		}
		c := counts[v.Slot][code]
		if c == nil {
			c = &ClientVersionCount{Version: name, Code: code}
			counts[v.Slot][code] = c
		}
		c.Count++
		c.Current = v.Hosted && code == v.Build.VersionCode
	}

	// Which client is open. An unknown or missing ?slot lands on the first one that has
	// devices, so the page opens on something worth looking at rather than an empty tab.
	selected := r.URL.Query().Get("slot")
	if _, ok := views[selected]; !ok {
		selected = order[0]
		for _, slot := range order {
			if views[slot].Devices > 0 {
				selected = slot
				break
			}
		}
	}
	views[selected].Selected = true
	views[selected].History = clientHistoryRows(selected, h.cfg.AgentAPKHistory(selected), views[selected].Build.SHA)
	if variants[selected] != "" {
		views[selected].GroupLabel = firmwareLabel
		views[selected].GroupNote = firmwareNote
		for _, slot := range firmwareGroup {
			views[slot].Selected = slot == selected
			views[selected].Variants = append(views[selected].Variants, views[slot])
		}
	}

	// The device rows, for the open client only.
	sel := views[selected]
	selIDs := make([]uuid.UUID, 0, len(devices))
	for i := range devices {
		if agentSlotFor(&devices[i]) == selected {
			selIDs = append(selIDs, devices[i].ID)
		}
	}
	inFlight, _ := h.db.InFlightAgentUpdates(r.Context(), selIDs)
	custody, _ := h.db.DeviceCustodyMap(r.Context(), selIDs)
	for i := range devices {
		d := &devices[i]
		if agentSlotFor(d) != selected {
			continue
		}
		name, code := clientVersionOf(d)
		row := ClientDeviceRow{
			Serial: d.SerialNumber, Version: name, Code: code,
			LastSeen: d.LastSeenAt,
		}
		if frameworkVersionReported(name, clientPackageOf(d)) {
			row.Version, row.Misreported = "", true
		}
		if c, ok := custody[d.ID]; ok && c.Elsewhere() {
			row.Elsewhere, row.ElsewhereURL = c.Server, c.URL
		}
		// Three times the check-in interval is the same staleness the fleet list uses
		// for "offline", so the two pages agree about what silent means.
		if row.Elsewhere == "" && time.Since(d.LastSeenAt) > time.Duration(h.cfg.CheckinInterval()*3)*time.Second {
			row.Unreachable = true
		}
		switch {
		case code == 0 || !versionComparable(d, sel.Build.Package):
			row.State = "unknown"
		case sel.Hosted && code >= sel.Build.VersionCode:
			row.State = "current"
		case sel.Hosted:
			row.State = "behind"
		default:
			row.State = "unknown"
		}
		// An update in flight outranks "behind": the device IS behind, but saying so
		// next to an Update button hides the fact that it is already working on it.
		if st, ok := inFlight[d.ID]; ok && row.State != "current" {
			row.State, row.Progress = "updating", st
			sel.Updating = true
		}
		sel.Rows = append(sel.Rows, row)
	}
	behind := make([]string, 0, len(sel.Rows))
	for _, row := range sel.Rows {
		// "Update all behind" targets devices this server can actually deliver to.
		// One reporting elsewhere is behind on paper and unreachable in fact.
		if row.State == "behind" && row.Elsewhere == "" {
			behind = append(behind, row.Serial)
		}
	}
	sel.BehindSerials = strings.Join(behind, ",")
	// Rows are built for the open client only, so this is the one view whose counts can
	// account for updates in flight — a device mid-update is no longer something
	// "Update all behind" should be offering to push to again.
	sel.Behind = len(behind)

	// Behind first — those are the rows an operator came here to act on — then by serial.
	rank := map[string]int{"behind": 0, "unknown": 1, "current": 2}
	sort.Slice(sel.Rows, func(i, j int) bool {
		if rank[sel.Rows[i].State] != rank[sel.Rows[j].State] {
			return rank[sel.Rows[i].State] < rank[sel.Rows[j].State]
		}
		return sel.Rows[i].Serial < sel.Rows[j].Serial
	})
	sel.Groups = groupClientRows(sel.Rows, sel.Build.Version, time.Now())

	var slots []*ClientSlotView
	for _, slot := range order {
		v := views[slot]
		for _, c := range counts[slot] {
			v.Versions = append(v.Versions, *c)
		}
		sort.Slice(v.Versions, func(i, j int) bool { return v.Versions[i].Code > v.Versions[j].Code })
		slots = append(slots, v)
	}

	// The rail: the firmware variants collapse into one row whose count is the whole
	// firmware fleet, and which opens on the variant already selected — or, coming in
	// cold, the one with devices on it rather than an empty tree.
	var rails []ClientRailEntry
	fw := ClientRailEntry{Label: firmwareLabel, Slot: firmwareGroup[0]}
	inGroup := map[string]bool{}
	for _, slot := range firmwareGroup {
		inGroup[slot] = true
		v := views[slot]
		fw.Devices += v.Devices
		fw.Behind += v.Behind
		if v.Selected {
			fw.Selected, fw.Slot = true, slot
		}
	}
	if !fw.Selected {
		for _, slot := range firmwareGroup {
			if views[slot].Devices > 0 {
				fw.Slot = slot
				break
			}
		}
	}
	for _, slot := range order {
		if inGroup[slot] {
			if slot == firmwareGroup[0] {
				rails = append(rails, fw)
			}
			continue
		}
		v := views[slot]
		rails = append(rails, ClientRailEntry{Label: v.Label, Slot: slot, Devices: v.Devices, Behind: v.Behind, Selected: v.Selected})
	}

	data := map[string]any{
		"Title":      "Clients",
		"ActivePage": "clients",
		"Slots":      slots,
		"Rails":      rails,
		"Selected":   sel,
		"ServerURL":  h.baseURL(r),
	}
	// Picking a client swaps the rail + detail rather than reloading the page, so the
	// header, dock and scroll position survive. withRole because the fragment renders
	// the same role-gated actions the full page does.
	if r.URL.Query().Get("partial") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.tmpl.ExecuteTemplate(w, "clients-split", h.withRole(r, data))
		return
	}
	// Device keys (device-key plan): how many devices still identify with the shared
	// key, which is public, against those with their own.
	data["KeysOwn"], data["KeysShared"], _ = h.db.DeviceKeyCounts(r.Context())
	h.render(w, r, "clients.html", data)
}

// knownAgentSlots names the publishable slots, sorted, for the error a build machine
// gets when it posts to one that does not exist — read off the map so removing a slot
// cannot leave the message advertising it.
func knownAgentSlots() []string {
	out := make([]string, 0, len(AgentAPKSlots))
	for slot := range AgentAPKSlots {
		out = append(out, slot)
	}
	sort.Strings(out)
	return out
}

// AgentAPKDir is where an uploaded agent APK lives (env AGENT_APK_DIR, default
// data/agent — the same data volume as splash images).
func AgentAPKDir() string {
	if d := strings.TrimSpace(os.Getenv("AGENT_APK_DIR")); d != "" {
		return d
	}
	return "data/agent"
}

const agentAPKFile = "aio-mdm-dpc.apk"
const agentAPKRoute = "/agent/" + agentAPKFile

// AgentAPKSlots are the agent builds this server can host at once. There is one per
// thing that has to be signed differently: the DPC agent has its own key, and the
// firmware client is signed with the platform key of the tree it was built in — QCOM
// with 01d88e1a…, GMS with c8a2e9bc…, each covering that tree's user and userdebug
// builds. A device is only ever offered the slot matching the key it already trusts,
// because Android rejects anything else.
var AgentAPKSlots = map[string]string{
	"dpc":           agentAPKFile,
	"firmware-qcom": "aio-mdm-firmware-qcom.apk",
	"firmware-gms":  "aio-mdm-firmware-gms.apk",
}

// firmwareSlotFor picks which firmware client a device can install. The key that signs
// the client belongs to the *tree* and to nothing else: QCOM signs with one platform
// certificate and GMS with another, and both ship `user` builds reporting
// build_tags=release-keys. Pointing them at one "release-keys" APK guarantees that one
// of the two fleets is handed a build it must reject. Trees are told apart by their
// build id line — QCOM is v2.1.x, GMS v2.0.x — which is how the OTA console versions
// them too.
//
// The build VARIANT does not come into it. Android.bp asks for certificate: "platform",
// which resolves to <tree>/build/make/target/product/security/platform.pk8 — a path with
// no variant in it. Checked against the artifacts: QCOM v2.1.010 (userdebug) and v2.1.017
// (user) both sign mdm-client with 01d88e1a…, GMS v2.0.98d (userdebug) and its user builds
// both with c8a2e9bc…. What misc_info's default_system_dev_certificate splits per variant
// (QCOM user=releasekey, userdebug=testkey) is the OTA *payload* key and the default for
// modules that name no certificate — neither applies to this APK. An earlier version of
// this function sent every userdebug device to a "firmware-test-keys" slot, which could
// only ever hold one tree's build and left those devices with no update offered at all.
func firmwareSlotFor(buildID, buildType string) string {
	switch {
	case strings.HasPrefix(buildID, "v2.1."):
		return "firmware-qcom"
	case strings.HasPrefix(buildID, "v2.0."):
		return "firmware-gms"
	}
	return "firmware-qcom"
}

// liteSlotPackages mapped MDM-lite host apps to a slot. Lite was dropped from the
// active client lineup (menu board + Lite demo app) — kept empty rather than removed
// so agentSlotFor's "nothing to offer it" fallback still applies cleanly to any
// MDM-lite device still out there instead of needing a special case.
var liteSlotPackages = map[string]string{}

// agentAPKPath is where a slot's APK lives on disk ("" for an unknown slot).
func agentAPKPath(slot string) string {
	name, ok := AgentAPKSlots[slot]
	if !ok {
		return ""
	}
	return filepath.Join(AgentAPKDir(), name)
}

// agentAPKArchivePath is where a published build is kept for good, named by its own
// digest: data/agent/archive/<slot>/<sha256hex>.apk. The slot file itself is a single
// path that every publish renames over, so before this the previous build's bytes were
// gone the moment a new one landed — history rows named versions nobody could download
// and a rollback meant rebuilding the APK in the AOSP tree with the right platform key.
// Keyed by digest rather than version because the digest is what the client verifies
// before replacing itself, and two builds of one version are two different artifacts.
func agentAPKArchivePath(slot, sha256Hex string) string {
	if _, ok := AgentAPKSlots[slot]; !ok || sha256Hex == "" {
		return ""
	}
	// Only a hex digest ever names a file here — the value reaches this from a URL.
	if _, err := hex.DecodeString(sha256Hex); err != nil || len(sha256Hex) != 64 {
		return ""
	}
	return filepath.Join(AgentAPKDir(), "archive", slot, sha256Hex+".apk")
}

// archiveAgentAPK keeps a copy of the bytes just published. A hard link when the
// filesystem allows it (same directory tree, so usually), a copy otherwise; either way
// the archive entry survives the next publish renaming the slot file away.
func archiveAgentAPK(slot, sha256Hex string, data []byte) error {
	dest := agentAPKArchivePath(slot, sha256Hex)
	if dest == "" {
		return errors.New("no archive path for slot " + slot)
	}
	if _, err := os.Stat(dest); err == nil {
		return nil // already archived: the same build published twice
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// SeedAgentAPKArchive back-fills the archive with whatever each slot is hosting right
// now, so the current build of every slot is downloadable and rollback-able from the
// first run after this ships. Builds published before the archive existed cannot be
// recovered — their bytes were overwritten — so only the current one per slot lands.
func SeedAgentAPKArchive(cfg *config.Config) (int, error) {
	seeded := 0
	for slot := range AgentAPKSlots {
		b, hosted := cfg.AgentAPKSlot(slot)
		if !hosted || b.SHA256Hex == "" {
			continue
		}
		dest := agentAPKArchivePath(slot, b.SHA256Hex)
		if dest == "" {
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		data, err := os.ReadFile(agentAPKPath(slot))
		if err != nil {
			continue
		}
		// Only archive it under a digest it actually has: a slot file and a config
		// that disagree would otherwise put the wrong bytes behind a known digest,
		// which is exactly what the client's checksum is there to catch.
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != b.SHA256Hex {
			continue
		}
		if err := archiveAgentAPK(slot, b.SHA256Hex, data); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

// agentSlotFor picks the slot a device can actually install: the DPC agent for a DPC
// device, else the firmware client built with the same platform key as the image on
// the device. A device that has not reported its build tags is assumed to be a user
// build, which is what the fleet runs.
func agentSlotFor(d *db.Device) string {
	if d == nil {
		return "dpc"
	}
	// MDM-lite first: it is stored as a DPC kind, but what updates is the host app.
	// The suffixed build variants of an app (…​.qa, …​.internal) are the same app.
	if d.IsMDMLite() {
		pkg := extraNested(d.LatestExtra, "host_app", "package")
		for base, slot := range liteSlotPackages {
			if pkg == base || strings.HasPrefix(pkg, base+".") {
				return slot
			}
		}
		return "" // Lite inside an app that is not ours: nothing to offer it
	}
	if d.IsDPC() {
		return "dpc"
	}
	return firmwareSlotFor(d.BuildID, extraString(d.LatestExtra, "build_type"))
}

// AgentAPKDownload serves the hosted agent APK. Unauthenticated on purpose: a
// factory-reset phone fetches it from the provisioning QR before any account
// exists. It contains nothing secret — the enrollment token travels in the QR's
// admin extras, not in the APK.
func (h *Handler) AgentAPKDownload(w http.ResponseWriter, r *http.Request) {
	// The file name in the URL names the slot; an unknown name is simply not found.
	want := path.Base(r.URL.Path)
	slot := ""
	for s, name := range AgentAPKSlots {
		if name == want {
			slot = s
			break
		}
	}
	if slot == "" {
		http.NotFound(w, r)
		return
	}
	if _, ok := h.cfg.AgentAPKSlot(slot); !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(agentAPKPath(slot))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="`+want+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, want, st.ModTime(), f)
}

// AgentAPKHistoryDownload serves an archived build by its digest. Authenticated,
// unlike the slot file: only the current build has to be reachable by a device with
// no session (QR provisioning), and an archived one is for an operator checking what
// shipped or staging a rollback.
func (h *Handler) AgentAPKHistoryDownload(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	sha := strings.TrimSuffix(path.Base(r.URL.Path), ".apk")
	p := agentAPKArchivePath(slot, sha)
	if p == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	name := AgentAPKSlots[slot]
	// Name the file after the build it is, so a folder of these is readable.
	for _, b := range h.cfg.AgentAPKHistory(slot) {
		if b.SHA256Hex == sha && b.Version != "" {
			name = strings.TrimSuffix(AgentAPKSlots[slot], ".apk") + "-" + b.Version + ".apk"
			break
		}
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// AgentAPKRollback makes an archived build the one a slot hosts again. This is the
// whole point of keeping the archive: before it, going back to yesterday's client
// meant rebuilding it in the AOSP tree with that tree's platform key. The bytes are
// checked against the digest they are filed under before anything is swapped — the
// client verifies the same digest before replacing itself, so a mismatch here would
// hand every device a download it must reject.
func (h *Handler) AgentAPKRollback(w http.ResponseWriter, r *http.Request) {
	slot := strings.TrimSpace(r.FormValue("slot"))
	sha := strings.TrimSpace(r.FormValue("sha256"))
	p := agentAPKArchivePath(slot, sha)
	if p == "" {
		h.hxDoneToast(w, r, "/clients", "Unknown client or digest", "error")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		h.hxDoneToast(w, r, "/clients?slot="+slot, "That build is not in the archive any more", "error")
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		h.hxDoneToast(w, r, "/clients?slot="+slot, "Archived build does not match its digest — not published", "error")
		return
	}
	var name string
	for _, b := range h.cfg.AgentAPKHistory(slot) {
		if b.SHA256Hex == sha {
			name = b.Name
			break
		}
	}
	if _, _, err := h.storeAgentAPK(data, name, slot, "rolled back to this build"); err != nil {
		h.hxDoneToast(w, r, "/clients?slot="+slot, "Rollback failed: "+err.Error(), "error")
		return
	}
	h.audit(r, "clients.agent_apk_rollback", slot, "sha256 "+sha)
	h.hxDoneToast(w, r, "/clients?slot="+slot, "Rolled back — this build is what devices install now", "success")
}

// SettingsAgentAPKUpload stores an uploaded agent APK and records its SHA-256 (URL-safe
// base64, as PROVISIONING_DEVICE_ADMIN_PACKAGE_CHECKSUM wants it). From then on the
// enrollment QR points at this server's copy.
func (h *Handler) SettingsAgentAPKUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		h.hxDoneToast(w, r, "/settings", "Upload failed: file too large or malformed", "error")
		return
	}
	file, hdr, err := r.FormFile("apk")
	if err != nil {
		h.hxDoneToast(w, r, "/settings", "Choose an APK file first", "error")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	sha, meta, err := h.storeAgentAPK(data, filepath.Base(hdr.Filename), "dpc", r.FormValue("changelog"))
	if err != nil {
		h.hxDoneToast(w, r, "/settings", "Upload failed: "+err.Error(), "error")
		return
	}
	built := describeAgentBuild(meta)
	h.audit(r, "settings.agent_apk_upload", hdr.Filename, fmt.Sprintf("%d bytes sha256 %s%s", len(data), sha, built))
	h.hxDoneToast(w, r, "/settings", "Agent APK hosted"+built, "success")
}

// AgentAPKPublish is the admin-API door onto the same room as the Settings upload:
// a build machine PUTs the signed APK here (X-API-Key, raw body or multipart) the
// moment it finishes signing, instead of someone carrying the file to a browser.
func (h *Handler) AgentAPKPublish(w http.ResponseWriter, r *http.Request) {
	slot := strings.TrimSpace(r.URL.Query().Get("slot"))
	if slot == "" {
		slot = "dpc"
	}
	if _, ok := AgentAPKSlots[slot]; !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown slot "+slot+" ("+strings.Join(knownAgentSlots(), ", ")+")")
		return
	}
	// Long-form notes travel as a query parameter: the body is the APK itself.
	changelog := r.URL.Query().Get("changelog")
	var (
		data []byte
		name = AgentAPKSlots[slot]
		err  error
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err = r.ParseMultipartForm(32 << 20); err != nil {
			writeJSONError(w, http.StatusBadRequest, "malformed multipart body")
			return
		}
		file, hdr, ferr := r.FormFile("apk")
		if ferr != nil {
			writeJSONError(w, http.StatusBadRequest, "no apk file field")
			return
		}
		defer file.Close()
		name = filepath.Base(hdr.Filename)
		if c := r.FormValue("changelog"); c != "" {
			changelog = c
		}
		data, err = io.ReadAll(file)
	} else {
		if n := strings.TrimSpace(r.URL.Query().Get("name")); n != "" {
			name = filepath.Base(n)
		}
		data, err = io.ReadAll(r.Body)
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "could not read body")
		return
	}
	sha, meta, err := h.storeAgentAPK(data, name, slot, changelog)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.audit(r, "api.agent_apk_publish", name, fmt.Sprintf("slot %s, %d bytes sha256 %s%s", slot, len(data), sha, describeAgentBuild(meta)))
	resp := map[string]any{"bytes": len(data), "sha256_base64url": sha, "slot": slot,
		"url": h.baseURL(r) + "/agent/" + AgentAPKSlots[slot]}
	if meta != nil {
		resp["package"] = meta.Package
		resp["version_name"] = meta.VersionName
		resp["version_code"] = meta.VersionCode
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// storeAgentAPK writes the agent APK this server hosts and records what it is. The
// file lands first, then the config, so a parse failure still leaves a downloadable
// APK for QR provisioning — it just can't be offered as an update (no version, no
// way to know who is behind). Returns the base64url digest for the QR extras.
func (h *Handler) storeAgentAPK(data []byte, filename, slot, changelog string) (string, *apkstore.Meta, error) {
	// An APK is a zip: PK. Anything else is a wrong file.
	if len(data) < 4 || string(data[:2]) != "PK" {
		return "", nil, errors.New("that is not an APK (zip) file")
	}
	if err := os.MkdirAll(AgentAPKDir(), 0o755); err != nil {
		return "", nil, err
	}
	dest := agentAPKPath(slot)
	if dest == "" {
		return "", nil, errors.New("unknown agent slot " + slot)
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	sha := base64.RawURLEncoding.EncodeToString(sum[:])
	// Keep the bytes under their digest too. Best-effort on purpose: a failure here
	// costs a rollback shortcut, and refusing the publish over it would cost the fleet
	// an update it can otherwise install right now.
	if err := archiveAgentAPK(slot, hex.EncodeToString(sum[:]), data); err != nil {
		log.Printf("[agent-apk] archiving %s: %v", slot, err)
	}
	if filename == "" {
		filename = AgentAPKSlots[slot]
	}
	build := config.AgentAPKBuild{
		SHA: sha, SHA256Hex: hex.EncodeToString(sum[:]),
		Name: filename, Size: int64(len(data)), At: time.Now(),
		Changelog: strings.TrimSpace(changelog),
	}
	meta, perr := apkstore.ParseFile(dest)
	if perr == nil {
		build.Package, build.Version, build.VersionCode = meta.Package, meta.VersionName, int64(meta.VersionCode)
	}
	if err := h.cfg.SetAgentAPKSlot(slot, build); err != nil {
		return "", nil, err
	}
	if perr != nil {
		return sha, nil, nil
	}
	return sha, meta, nil
}

// describeAgentBuild is the "— pkg 0.2.0 (8)" tail shared by the toast and the audit
// line, or a plain warning when the APK's version could not be read.
func describeAgentBuild(meta *apkstore.Meta) string {
	if meta == nil {
		return " — version unreadable, agent updates stay off"
	}
	return fmt.Sprintf(" — %s %s (%d)", meta.Package, meta.VersionName, meta.VersionCode)
}

// SettingsAgentAPKRemove stops hosting the uploaded APK.
func (h *Handler) SettingsAgentAPKRemove(w http.ResponseWriter, r *http.Request) {
	_ = os.Remove(filepath.Join(AgentAPKDir(), agentAPKFile))
	if err := h.cfg.SetAgentAPKHosted("", "", 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.agent_apk_remove", "", "")
	h.hxDoneToast(w, r, "/settings", "Hosted agent APK removed", "success")
}

// SettingsAgentAPK stores where QR provisioning downloads the DPC agent from and
// its signing-certificate checksum (Settings → App library → DPC agent).
func (h *Handler) SettingsAgentAPK(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.FormValue("agent_apk_url"))
	sum := strings.TrimSpace(r.FormValue("agent_apk_checksum"))
	if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		h.hxDoneToast(w, r, "/settings", "Agent APK URL must start with http:// or https://", "error")
		return
	}
	if err := h.cfg.SetAgentAPK(u, sum); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.agent_apk", u, "")
	msg := "Standard MDM APK saved — QR cold-provisioning is on"
	if u == "" || sum == "" {
		msg = "Standard MDM APK cleared — QR cold-provisioning is off"
	}
	h.hxDoneToast(w, r, "/settings", msg, "success")
}

// ── Device lifecycle (onboarding inbox, class, retire) ────────────────────────

// DeviceOnboard confirms a device's placement (site/group/class already set or set
// here) and takes it out of the onboarding inbox.
func (h *Handler) DeviceOnboard(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if rid := r.FormValue("restaurant_id"); rid != "" {
		if parsed, err := uuid.Parse(rid); err == nil {
			if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, &parsed); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			h.pushGuestToDevices(r.Context(), []uuid.UUID{device.ID})
		}
	}
	if c := strings.ToLower(strings.TrimSpace(r.FormValue("device_class"))); c != "" && product.IsClass(c) {
		if err := h.db.SetDeviceClass(r.Context(), device.ID, c); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}
	if err := h.db.MarkOnboarded(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.onboard", serial, "")
	h.hub.PublishDeviceUpdate(device.ID)
	from := r.FormValue("from")
	if from == "" {
		from = "/enrollment"
	}
	h.hxDoneToast(w, r, from, "Device onboarded", "success")
}

// DeviceSetClass overrides the form factor for one device.
func (h *Handler) DeviceSetClass(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	c := strings.ToLower(strings.TrimSpace(r.FormValue("device_class")))
	if c != "" && !product.IsClass(c) {
		http.Error(w, "Unknown class", http.StatusBadRequest)
		return
	}
	if err := h.db.SetDeviceClass(r.Context(), device.ID, c); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.class", serial, c)
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxDoneToast(w, r, "/devices/"+serial, "Class updated", "success")
}

// DeviceRetire takes a device out of the active fleet (lists, alerts, policy coverage)
// while keeping its history and placement. A later check-in or re-enrollment brings it
// back automatically; DeviceUnretire does it by hand.
func (h *Handler) DeviceRetire(w http.ResponseWriter, r *http.Request) {
	h.deviceSetLifecycle(w, r, db.EnrollRetired, "device.retire", "Device retired")
}

func (h *Handler) DeviceUnretire(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	status := db.EnrollAuto
	if device.IsDPC() {
		status = db.EnrollEnrolled
	}
	h.deviceSetLifecycle(w, r, status, "device.unretire", "Device back in the fleet")
}

func (h *Handler) deviceSetLifecycle(w http.ResponseWriter, r *http.Request, status, auditAction, toast string) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.db.SetEnrollmentStatus(r.Context(), device.ID, status); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, auditAction, serial, status)
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxDoneToast(w, r, "/devices/"+serial, toast, "success")
}

// serialTailLen is how many trailing characters of a serial carry the highlight on a
// device card. Three: the digits that actually differ between two devices on a shelf,
// and the ones people read out to each other. A serial shorter than that cannot be
// split, so the whole thing is the tail.
func serialTailLen(s string) int {
	if len(s) < 3 {
		return len(s)
	}
	return 3
}

// peerNameForURL turns the URL an operator picked in "Move & reboot" into the name of
// the peer it belongs to, so the device page says "stage" rather than a bare host. An
// address that is not a configured peer keeps its host as the label: a device can be
// sent somewhere this server has no relationship with, and that has to be sayable too.
func peerNameForURL(cfg *config.Config, target string) string {
	want := strings.TrimRight(strings.TrimSpace(target), "/")
	for _, p := range cfg.Peers(false) {
		if strings.TrimRight(strings.TrimSpace(p.URL), "/") == want ||
			strings.TrimRight(strings.TrimSpace(p.DashboardURL), "/") == want {
			return p.Name
		}
	}
	if u, err := url.Parse(want); err == nil && u.Host != "" {
		return u.Host
	}
	return want
}

// deviceCustodyOrEmpty is the template-facing form: an error reading custody must not
// take the device page down with it — the page's job is the device, and "we don't know
// where it is" is the same as "it is ours until something says otherwise".
func deviceCustodyOrEmpty(c db.Custody, err error) db.Custody {
	if err != nil {
		return db.Custody{}
	}
	return c
}

// dischargeSeriesJSON renders the lifetime-wear series for the device page. An error
// yields an empty array rather than failing the page: the graph falls back to counting
// the excursions it can see, which is what it did before this existed.
func dischargeSeriesJSON(pts []db.DischargePoint, err error) template.JS {
	if err != nil || len(pts) == 0 {
		return template.JS("[]")
	}
	// [[epochMillis, cumulativePercent], …] — two numbers a day keeps a year of
	// history well under the size of one chart frame.
	out := make([][2]int64, 0, len(pts))
	for _, p := range pts {
		out = append(out, [2]int64{p.Day.UnixMilli(), p.Cum})
	}
	b, jerr := json.Marshal(out)
	if jerr != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}
