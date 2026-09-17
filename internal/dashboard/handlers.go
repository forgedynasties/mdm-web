package dashboard

import (
	"crypto/sha256"
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
	"mdm/internal/ai"
	"mdm/internal/alerts"
	"mdm/internal/apkmeta"
	"mdm/internal/apkstore"
	"mdm/internal/config"
	"mdm/internal/otaconfig"
	"mdm/internal/db"
	"mdm/internal/otagate"
	"mdm/internal/geolocate"
	"mdm/internal/logstream"
	"mdm/internal/mailer"
	"mdm/internal/notify"
	"mdm/internal/ota"
	"mdm/internal/product"
	"mdm/internal/ratelimit"
	"mdm/internal/remote"
	"mdm/internal/shell"
	"mdm/internal/totp"
	"mdm/internal/version"
	"mdm/internal/ws"
)

// renderCachedHTML renders a template fragment for an htmx swap target. It used
// to emit an ETag and 304 to short-circuit unchanged polls, but that is a footgun
// with htmx: a 304 has an empty body, and on any browser that doesn't transparently
// serve the cached copy (cache disabled, entry evicted, some privacy configs) htmx
// swaps the empty body IN and blanks the whole region. So it now always returns the
// full body with no-store — correctness over a few KB of bandwidth on these tiny,
// frequently-changing fragments.
func (h *Handler) renderCachedHTML(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// ── htmx helpers (no-reload mutations) ──────────────────────────────────────────
// The dashboard is progressively enhanced: forms keep action=/method=POST so they
// work without JS, but when the request comes from htmx (HX-Request) handlers can
// respond without a full-page redirect — either by swapping a fragment or by
// returning 204 + HX-Trigger events that make the affected regions refetch.

// hxReq reports whether the request was issued by htmx.
func hxReq(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// hxTriggerEvents sets HX-Trigger so htmx dispatches the named events on the
// client; page regions listen via hx-trigger="<name> from:body" and refetch.
func hxTriggerEvents(w http.ResponseWriter, events ...string) {
	if len(events) > 0 {
		w.Header().Set("HX-Trigger", strings.Join(events, ", "))
	}
}

// hxToast asks the client (via the global `toast` HX-Trigger listener in
// layout.html) to show a toast. Used for inline error feedback on hx requests.
func hxToast(w http.ResponseWriter, msg, typ string) {
	b, _ := json.Marshal(map[string]any{"toast": map[string]string{"msg": msg, "type": typ}})
	w.Header().Set("HX-Trigger", asciiJSON(b))
}

// asciiJSON escapes every non-ASCII rune as \uXXXX. HTTP headers are bytes, and a
// browser decodes them as latin-1, so a UTF-8 "·" in an HX-Trigger payload arrives
// as "Â·" in the toast. JSON's own escape survives that intact.
func asciiJSON(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		if r < 128 {
			sb.WriteRune(r)
			continue
		}
		if r > 0xFFFF { // outside the BMP: surrogate pair
			r -= 0x10000
			fmt.Fprintf(&sb, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			continue
		}
		fmt.Fprintf(&sb, "\\u%04x", r)
	}
	return sb.String()
}

// hxRedirect issues a 303 redirect. The whole app is hx-boosted, so for htmx
// requests htmx transparently follows the redirect and swaps only <main>
// (inherited hx-select) — no full-document reload — while plain requests get a
// normal redirect. Kept as a named helper so mutation handlers read intentionally
// and we can adjust the strategy in one place.
func (h *Handler) hxRedirect(w http.ResponseWriter, r *http.Request, url string) {
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// hxDone completes an in-place mutation without a full-page reload. For htmx
// requests it returns 204 No Content (htmx swaps nothing, so the content area
// never blinks) plus any HX-Trigger events, letting the affected regions refresh
// through their existing live listeners; plain (no-JS) requests fall back to a
// redirect. Most callers pass no events and rely on the hub/SSE broadcast the
// handler already published (the actor's own tab is subscribed too), which keeps
// every open tab consistent with a single refresh rather than a full <main> swap.
func (h *Handler) hxDone(w http.ResponseWriter, r *http.Request, redirectURL string, events ...string) {
	if hxReq(r) {
		if len(events) > 0 {
			hxTriggerEvents(w, events...)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// plural returns "s" unless n == 1, for building "N device(s)" toast messages.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// hxDoneToast is hxDone plus a confirmation toast for htmx requests — used for
// bulk actions where the in-place change alone may not be obvious enough to
// reassure the operator it happened. The toast rides HX-Trigger; the affected
// regions still refresh via the hub/SSE broadcast the handler published.
func (h *Handler) hxDoneToast(w http.ResponseWriter, r *http.Request, redirectURL, msg, typ string) {
	if hxReq(r) {
		hxToast(w, msg, typ)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// hxDoneToastEvents is hxDoneToast that ALSO fires named client events in the same
// HX-Trigger payload (e.g. "refresh-devices" to make the fleet roster refetch).
// Use this instead of relying on the hub broadcast when the actor's own refresh
// must be reliable: hub.PublishDeviceUpdate is throttled to once per 4s per device,
// so a device that just checked in would drop the actor's update — a direct client
// event is not throttled and lets the listening region refetch unconditionally.
func (h *Handler) hxDoneToastEvents(w http.ResponseWriter, r *http.Request, redirectURL, msg, typ string, events ...string) {
	if hxReq(r) {
		payload := map[string]any{"toast": map[string]string{"msg": msg, "type": typ}}
		for _, e := range events {
			payload[e] = nil
		}
		b, _ := json.Marshal(payload)
		w.Header().Set("HX-Trigger", asciiJSON(b))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

type Handler struct {
	db          *db.DB
	hub         *ws.Hub
	shell       *shell.Manager
	remote      *remote.Manager
	logs        *logstream.Manager
	store       *sessions.CookieStore
	tmpl        *template.Template
	user        string
	password    string
	cfg         *config.Config
	adminAPIKey string
	alerts      *alerts.Dispatcher
	otaGate     *otagate.Gate // which builds can take an MDM OTA (the rest go legacy)

	// mapsEmbedKey is the browser-facing Google Maps Embed API key used by the
	// device-page location map iframe. "" disables the map (page still shows the
	// resolved coordinates/address as text). Kept separate from the server-side
	// geolocation/geocoding keys: this one ships to the browser, so it must be
	// HTTP-referrer-restricted to the dashboard domain + Maps Embed API only.
	mapsEmbedKey string

	// Google API usage metering (Settings → Google APIs panel). geo/geocoder may be
	// nil when their keys are unset; mapViews counts device-page renders that emit
	// the Maps Embed iframe (a proxy for browser-side embed loads, which are free).
	geo       *geolocate.Resolver
	geocoder  *geolocate.Geocoder
	mapViews  atomic.Uint64
	startedAt time.Time
	apk       *apkstore.Store // S3-backed APK uploads; nil when S3 is not configured

	// assetVer is a cache-busting token appended to the stylesheet URL, derived
	// from style.css's mtime at startup. Static assets are served `immutable`
	// with a long max-age, so without a changing URL a CSS edit would never
	// reach a browser that already cached the old file. Recomputed each restart.
	assetVer string

	// publicOrigins is the set of trusted browser-facing origins (scheme://host)
	// for CSRF checks, e.g. ["https://udm.dev.aioapp.com",
	// "https://mdm.dev.aioapp.com"]. Parsed from the comma-separated PUBLIC_ORIGIN
	// env. When empty (unset), checks fall back to matching the request's own Host
	// header, which trusts whichever served domain the request came from.
	publicOrigins []string

	// loginFails throttles failed login attempts per source IP and per account
	// to blunt brute-force / credential-spray (F-01).
	loginFails *ratelimit.Counter

	// signupAttempts/resetAttempts throttle the public sign-up and forgot-password
	// endpoints per source IP and per email, same shape as loginFails.
	signupAttempts *ratelimit.Counter
	resetAttempts  *ratelimit.Counter

	// mail sends the sign-up verification and password-reset emails via the Amazon
	// SES v2 API (SigV4-signed with AWS creds — no SES-specific credentials).
	// Missing AWS_REGION/credentials makes Send a logged no-op instead of an error,
	// so those flows still exercise their DB/token logic in dev.
	mail *mailer.Client

	// Microsoft sign-in (Entra ID, single-tenant): MS_CLIENT_ID/MS_CLIENT_SECRET/
	// MS_TENANT_ID. Empty msClientID means not configured — the login page hides
	// the button (msLoginEnabled funcMap) and the auth handlers 404.
	msClientID, msClientSecret, msTenantID string

	// lastDigestDay is the YYYY-MM-DD of the most recent AI fleet digest sent, so
	// housekeeping posts it at most once per day. Touched only from the single
	// housekeeping goroutine.
	lastDigestDay string
}

var logcatSeverityRe = regexp.MustCompile(`\b([EWIDV])\/|\s([EWIDV])\s`)
var auditCmdIDRe = regexp.MustCompile(`cmd=([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

func extractCharging(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m["charging"]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

// extractBatteryMissing reports whether the device explicitly told us its pack is
// absent (latest_extra.battery_present == false). Only a device that sends the field
// can be missing a battery; an older client that never sends it reads as present, so
// this never fires on a fleet that has not been updated yet.
//
// On the T7 the charger IC infers presence from the NTC (thermistor) fault bits rather
// than a presence pin (sgm4154x_charger.c, POWER_SUPPLY_PROP_PRESENT), so this means
// "thermistor open/out of range, or the pack is unplugged" — and in that state the
// charger also refuses to charge while the fuel gauge keeps reporting a percentage off
// the rail. Showing that percentage would look like a healthy battery, so callers show
// a distinct state instead.
func extractBatteryMissing(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m["battery_present"]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return !b
}

func extractBatteryTempC(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false
	}
	v, ok := m["battery_temp_c"]
	if !ok {
		return 0, false
	}
	var temp float64
	if err := json.Unmarshal(v, &temp); err != nil {
		return 0, false
	}
	return temp, true
}

// MicGainView is the parsed `mic_gain` telemetry object the client reports (see the
// client's MicGain.java): the codec's TX_DEC0..7 Volume mixer controls. 102 on every
// channel = the trinket mic-gain fix is present; 84 = codec default (fix absent).
type MicGainView struct {
	TxDec      []int  `json:"tx_dec"`
	Source     string `json:"source"`     // "live" (vendor probe daemon) | "config" (mixer_paths xml)
	Configured bool   `json:"configured"` // config source: at least one top-level default present
	File       string `json:"file"`       // config source: xml file name
	TS         int64  `json:"ts"`         // live source: probe epoch seconds
	Target     *int   `json:"target"`     // live source: enforcement target set via mic_gain_set, if any
}

// Summary classifies the reading for the dashboard: "fixed" when every channel reads
// the fix value, "default" when every channel reads the codec default, else "mixed".
func (m MicGainView) Summary() string {
	if len(m.TxDec) == 0 {
		return ""
	}
	allFixed, allDefault := true, true
	for _, v := range m.TxDec {
		if v != micGainFixed {
			allFixed = false
		}
		if v != micGainDefault {
			allDefault = false
		}
	}
	switch {
	case allFixed:
		return "fixed"
	case allDefault:
		return "default"
	}
	return "mixed"
}

// Values renders the per-channel list, collapsing to a single number when uniform.
func (m MicGainView) Values() string {
	if len(m.TxDec) == 0 {
		return ""
	}
	uniform := true
	for _, v := range m.TxDec[1:] {
		if v != m.TxDec[0] {
			uniform = false
			break
		}
	}
	if uniform {
		return strconv.Itoa(m.TxDec[0])
	}
	parts := make([]string, len(m.TxDec))
	for i, v := range m.TxDec {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " ")
}

const (
	micGainFixed   = 102 // TX_DEC Volume set by the trinket mic fix (mixer_paths_idp.xml defaults)
	micGainDefault = 84  // codec power-on default when the HAL configures nothing
)

// extractMicGain parses latest_extra.mic_gain; ok=false when absent or malformed.
func extractMicGain(raw json.RawMessage) (MicGainView, bool) {
	var mg MicGainView
	if len(raw) == 0 {
		return mg, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return mg, false
	}
	v, ok := m["mic_gain"]
	if !ok {
		return mg, false
	}
	if err := json.Unmarshal(v, &mg); err != nil || len(mg.TxDec) == 0 {
		return mg, false
	}
	return mg, true
}

// micGainPtr is extractMicGain for templates: nil when the device reports no mic_gain.
func micGainPtr(raw json.RawMessage) *MicGainView {
	if mg, ok := extractMicGain(raw); ok {
		return &mg
	}
	return nil
}

func deviceRowClasses(dev db.Device) string {
	var classes []string

	if dev.BatteryPct < 20 {
		classes = append(classes, "row-alert-battery")
	}
	if temp, ok := extractBatteryTempC(dev.LatestExtra); ok && (temp >= 45 || temp <= 0) {
		classes = append(classes, "row-alert-temp")
	}

	return strings.Join(classes, " ")
}

// DeviceRowJSON holds the pre-computed, JSON-serialisable data for one fleet table row.
type DeviceRowJSON struct {
	Serial       string  `json:"serial"`
	BuildID      string  `json:"build_id"`
	Online       bool    `json:"online"`
	BatteryPct   int     `json:"battery_pct"`
	BatteryClass string  `json:"battery_class"`
	BatteryWidth string  `json:"battery_width"`
	RamPct       int     `json:"ram_pct"` // 0 = no data
	HasRam       bool    `json:"has_ram"`
	TempStr      string  `json:"temp_str"` // "" = no data
	TempClass    string  `json:"temp_class"`
	LastSeenISO  string  `json:"last_seen_iso"` // RFC3339, empty if zero
	TimeSince    string  `json:"time_since"`
	PollInterval int     `json:"poll_interval_ms"`
	KioskEnabled bool    `json:"kiosk_enabled"`
	KioskPackage string  `json:"kiosk_package"`
	Hidden       bool    `json:"hidden"`      // true once hidden; tells the live row patch to drop the row
	HasBattery   bool    `json:"has_battery"` // false = wall-powered (kiosk); live patch shows AC, not 0%
	// BatteryMissing: product has a battery but the device reports the pack absent
	// (NTC fault / unplugged). The live patch shows a "None" chip, never a percentage.
	BatteryMissing bool  `json:"battery_missing"`
	Charging     bool    `json:"charging"`
	Flapping     bool    `json:"flapping"`  // charger toggling >10×/min — show the fault glyph
	FlapRate     int     `json:"flap_rate"` // observed toggles/min, for the tooltip
	RowClasses   string  `json:"row_classes"`
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
}

func deviceToRowJSON(dev db.Device, online bool, staleThreshold time.Duration) DeviceRowJSON {
	r := DeviceRowJSON{
		Serial:       dev.SerialNumber,
		BuildID:      dev.BuildID,
		Online:       online,
		BatteryPct:   dev.BatteryPct,
		BatteryWidth: fmt.Sprintf("%d%%", dev.BatteryPct),
		PollInterval: dev.PollIntervalMs,
		KioskEnabled: dev.KioskEnabled,
		KioskPackage: dev.KioskPackage,
		Hidden:       dev.Hidden,
		HasBattery:     dev.HasBattery(),
		BatteryMissing: dev.HasBattery() && extractBatteryMissing(dev.LatestExtra),
		RowClasses:     deviceRowClasses(dev),
	}

	switch {
	case dev.BatteryPct < 20:
		r.BatteryClass = "battery-low"
	case dev.BatteryPct < 50:
		r.BatteryClass = "battery-mid"
	default:
		r.BatteryClass = "battery-ok"
	}

	if len(dev.LatestExtra) > 0 {
		var extra map[string]json.RawMessage
		if json.Unmarshal(dev.LatestExtra, &extra) == nil {
			if v, ok := extra["ram_usage_mb"]; ok {
				var ram map[string]int
				if json.Unmarshal(v, &ram) == nil {
					total := ram["total"]
					if total > 0 {
						r.HasRam = true
						r.RamPct = ram["used"] * 100 / total
						if r.RamPct > 100 {
							r.RamPct = 100
						}
					}
				}
			}
			if v, ok := extra["charging"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil && (staleThreshold == 0 || time.Since(dev.LastSeenAt) <= staleThreshold) {
					r.Charging = b
				}
			}
			if v, ok := extra["latitude"]; ok {
				var lat float64
				if json.Unmarshal(v, &lat) == nil {
					r.Latitude = lat
				}
			}
			if v, ok := extra["longitude"]; ok {
				var lon float64
				if json.Unmarshal(v, &lon) == nil {
					r.Longitude = lon
				}
			}
		}
	}

	if temp, ok := extractBatteryTempC(dev.LatestExtra); ok {
		r.TempStr = fmt.Sprintf("%.1f°C", temp)
		switch {
		case temp >= 60:
			r.TempClass = "danger"
		case temp >= 45:
			r.TempClass = "warn"
		case temp <= -10:
			r.TempClass = "danger"
		case temp <= 0:
			r.TempClass = "warn"
		default:
			r.TempClass = "ok"
		}
	}

	if !dev.LastSeenAt.IsZero() {
		r.LastSeenISO = dev.LastSeenAt.UTC().Format(time.RFC3339)
		d := time.Since(dev.LastSeenAt)
		switch {
		case d < time.Minute:
			r.TimeSince = "just now"
		case d < time.Hour:
			r.TimeSince = fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 24*time.Hour:
			r.TimeSince = fmt.Sprintf("%dh ago", int(d.Hours()))
		default:
			r.TimeSince = fmt.Sprintf("%dd ago", int(d.Hours()/24))
		}
	}

	return r
}

func colorizeLogcatText(content string) template.HTML {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	var out strings.Builder
	for i, line := range lines {
		className := ""
		if match := logcatSeverityRe.FindStringSubmatch(line); match != nil {
			severity := match[1]
			if severity == "" {
				severity = match[2]
			}
			className = "log-" + severity
		}

		escaped := html.EscapeString(line)
		if className != "" {
			out.WriteString(`<span class="`)
			out.WriteString(className)
			out.WriteString(`">`)
			out.WriteString(escaped)
			out.WriteString(`</span>`)
		} else {
			out.WriteString(escaped)
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}

	return template.HTML(out.String())
}

// updateEngineErrors maps android.os.UpdateEngine.ErrorCodeConstants values
// (the client reports them as "UPDATE_ERROR_<n>") to operator-readable text.
var updateEngineErrors = map[string]string{
	"1":  "Generic update_engine error.",
	"4":  "Filesystem copier error.",
	"5":  "Post-install step failed.",
	"6":  "Payload type mismatch — wrong package for this device.",
	"7":  "Couldn't open the target partition.",
	"8":  "Couldn't open the kernel partition.",
	"9":  "Download transfer error (network or source).",
	"10": "Payload hash mismatch — corrupted or wrong package.",
	"11": "Payload size mismatch — corrupted or wrong package.",
	"12": "Payload signature verification failed — signing-key mismatch.",
	"51": "Payload older than the installed build — downgrade blocked.",
	"52": "Updated, but the new slot did not become active.",
	"60": "Not enough free space to apply the update.",
	"61": "Device storage is corrupted.",
	"62": "Package excluded for this device.",
}

func NewHandler(d *db.DB, hub *ws.Hub, shellMgr *shell.Manager, remoteMgr *remote.Manager, logMgr *logstream.Manager, sessionSecret, user, password string, cfg *config.Config, adminAPIKey, mapsEmbedKey string, geo *geolocate.Resolver, geocoder *geolocate.Geocoder) *Handler {
	store := sessions.NewCookieStore([]byte(sessionSecret))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   cfg.SessionTimeout(),
		HttpOnly: true,
		// Secure by default so the session cookie is never sent over cleartext;
		// set COOKIE_SECURE=false for local HTTP development. SameSite=Lax keeps
		// the cookie off cross-site sub-requests, complementing the CSRF guard.
		Secure:   os.Getenv("COOKIE_SECURE") != "false",
		SameSite: http.SameSiteLaxMode,
	}
	// Align the securecookie codec max-age with the cookie so the timeout is
	// actually enforced server-side, not just by the browser.
	store.MaxAge(cfg.SessionTimeout())

	msLoginEnabled := os.Getenv("MS_CLIENT_ID") != "" && os.Getenv("MS_CLIENT_SECRET") != "" && os.Getenv("MS_TENANT_ID") != ""
	// userURL resolves a person's username or display name (as snapshotted on
	// commands / audit rows) to their profile page, or "" when unknown. Backed by
	// a 30 s cache of the users table so templates can link every name cheaply.
	var (
		uuMu   sync.Mutex
		uuAt   time.Time
		uuMap  map[string]string
	)
	// userBubble resolves a username or display name (as snapshotted on commands)
	// to what the avatar bubble needs: initial, display name, and the picture URL
	// (empty when none). Cached like userURL.
	type bubble struct {
		Initial, Name, URL string
		Admin              bool // author has the admin/dev role (or is the env dashboard admin)
	}
	var ubMu sync.Mutex
	var ubMap map[string]bubble
	var ubAt time.Time
	userBubble := func(name string) bubble {
		name = strings.TrimSpace(name)
		fallback := bubble{Initial: "?", Name: name, Admin: strings.EqualFold(name, "admin")}
		if name != "" {
			for _, r := range name {
				fallback.Initial = strings.ToUpper(string(r))
				break
			}
		}
		if name == "" {
			return fallback
		}
		ubMu.Lock()
		defer ubMu.Unlock()
		if ubMap == nil || time.Since(ubAt) > 30*time.Second {
			m := map[string]bubble{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if users, err := d.ListUsers(ctx); err == nil {
				for _, u := range users {
					b := bubble{Initial: u.Initial(), Name: u.DisplayName(), Admin: false} // only the built-in admin login counts as "admin actions"
					if u.HasAvatar() {
						b.URL = fmt.Sprintf("/users/%s/avatar.png?v=%d", u.ID, u.AvatarVer)
					}
					m[strings.ToLower(u.Username)] = b
					if dn := strings.ToLower(u.DisplayName()); dn != "" {
						m[dn] = b
					}
				}
			}
			cancel()
			ubMap, ubAt = m, time.Now()
		}
		if b, ok := ubMap[strings.ToLower(name)]; ok {
			return b
		}
		return fallback
	}
	userBubbleFn = func(name string) any { return userBubble(name) }
	userIsAdminFn = func(name string) bool { return userBubble(name).Admin }
	// authorIsAdmin: was this action sent by an admin account? Drives the
	// "Show admin actions" toggle on the Actions/History pages.
	authorIsAdmin := func(name string) bool { return userBubble(name).Admin }

	userURL := func(name string) string {
		name = strings.TrimSpace(name)
		if name == "" {
			return ""
		}
		uuMu.Lock()
		defer uuMu.Unlock()
		if uuMap == nil || time.Since(uuAt) > 30*time.Second {
			m := map[string]string{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if users, err := d.ListUsers(ctx); err == nil {
				for _, u := range users {
					url := "/users/" + u.ID.String() + "/profile"
					m[strings.ToLower(u.Username)] = url
					if dn := strings.ToLower(u.DisplayName()); dn != "" {
						m[dn] = url
					}
				}
			}
			cancel()
			uuMap, uuAt = m, time.Now()
		}
		return uuMap[strings.ToLower(name)]
	}
	funcMap := template.FuncMap{
		"userURL":       userURL,
		"userBubble":    userBubble,
		"authorIsAdmin": authorIsAdmin,
		// msLoginEnabled reports whether Microsoft sign-in is configured, so
		// login.html only shows the button when it'll actually work.
		"msLoginEnabled": func() bool { return msLoginEnabled },
		// atoi parses a string to int (0 on failure) for arithmetic in templates.
		"atoi": atoi,
		// isLegacyBuild reports whether a build ID is on the configured legacy (WS-incapable)
		// firmware list — such devices only HTTP check-in and never hold a live WebSocket.
		"isLegacyBuild": cfg.IsLegacyBuild,
		// extraStr reads one string key out of a device's latest_extra JSON blob.
		"extraStr": func(extra json.RawMessage, key string) string {
			var m map[string]any
			if json.Unmarshal(extra, &m) != nil {
				return ""
			}
			if v, ok := m[key].(string); ok {
				return v
			}
			return ""
		},
		// pct returns done/total as an integer percentage (0-100), for progress bars.
		"pct": func(done, total int) int {
			if total <= 0 {
				return 0
			}
			p := done * 100 / total
			if p > 100 {
				return 100
			}
			return p
		},
		// ringDash returns the stroke-dashoffset for a progress ring of radius 52
		// (circumference ≈ 327), so done/total fills the arc. Powers Mission Control.
		"ringDash": func(done, total int) int {
			const c = 327
			if total <= 0 {
				return c
			}
			off := c - c*done/total
			if off < 0 {
				off = 0
			}
			return off
		},
		// canAdmin reports whether a role has operational admin power in the UI:
		// both "admin" and "dev" do. Used to gate operational buttons/links;
		// settings and user-management UI stay on a literal `eq .Role "admin"`.
		"canAdmin": func(role string) bool { return role == "admin" },
		// whyScore: the penalties behind a venue/group score, for the "?" popover.
		"whyScore": whyScore,
		// canRelease: release / OTA / deployment controls — admin or dev.
		"canRelease": func(role string) bool { return role == "admin" || role == "dev" },
		// canSeeDev: who sees releases still marked dev — everyone but viewers/owners.
		"canSeeDev": roleSeesDev,
		"canOTA":     roleCanOTA,
		// canAppLibrary: who may open /apps and upload to the library (admin, dev, super op).
		"canAppLibrary": roleCanAppLibrary,
		// canSeeInactive: the Inactive device view, roles above operator.
		"canSeeInactive": roleAboveOperator,
		// canManageUsers: the Users pages (roster, activity, access control).
		"canManageUsers": roleManagesUsers,
		"canCreateUsers": roleCreatesUsers,
		"roleLabel":      roleLabel,
		"roleLevel":      roleLevel,
		// canAdminOrOperator mirrors requireAdminOrOperator: admin/dev plus the test team,
		// for group/restaurant creation and kiosk mode. Used to show those controls.
		"canAdminOrOperator": func(role string) bool { return roleCanOperate(role) },
		// canAct gates only the Actions dock link. Every authenticated role may
		// open the Actions page — viewers see it read-only (the builder, recipes,
		// resend and delete controls are all separately gated by canOperate /
		// canAdmin, so a viewer sees history but no way to act).
		"canAct": func(role string) bool {
			return role != ""
		},
		// canOperate reports action-level UI power: the test team plus admin/dev.
		// Mirrors requireOperatorOrAdmin on the server so the dashboard shows the
		// same actions those roles can actually perform.
		"canOperate": func(role string) bool {
			return roleCanOperate(role)
		},
		// plainAlert strips a humanized alert sentence to plain text (for search).
		"plainAlert": plainSentence,
		// alertTypeGroups feeds the per-channel alert-type filter in settings.
		"alertTypeGroups": alertTypeCatalog,
		// alertTypeLabel maps a raw alert type to its friendly catalog label
		// (falls back to the raw type when unknown).
		"alertTypeLabel": alertTypeLabel,
		// alertsQuery builds the /alerts query string preserving the active filters.
		"alertsQuery":   alertsQueryString,
		"alertsPageURL": alertsPageQueryString,
		// hasStr reports membership of s in list (template helper for checkbox state).
		"hasStr": func(list []string, s string) bool {
			for _, x := range list {
				if x == s {
					return true
				}
			}
			return false
		},
		"hasPrefix": strings.HasPrefix,
		"batteryClass": func(pct int) string {
			switch {
			case pct < 20:
				return "battery-low"
			case pct < 50:
				return "battery-mid"
			default:
				return "battery-ok"
			}
		},
		// width (in the 0..18 viewBox-unit range) of the battery glyph's inner fill
		"batteryFillW": func(pct int) int {
			w := pct * 18 / 100
			if w < 2 {
				w = 2
			}
			return w
		},
		"iconSrc": iconSrc,
		"formatTime": func(t time.Time) string {
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"utcDateTimeLocal": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format("2006-01-02T15:04")
		},
		"timeISO": func(t time.Time) string {
			return t.UTC().Format(time.RFC3339)
		},
		"localTime": func(t time.Time) template.HTML {
			iso := t.UTC().Format(time.RFC3339)
			fallback := t.UTC().Format("2006-01-02 15:04:05 UTC")
			return template.HTML(`<span class="js-local-time" data-utc="` + iso + `" data-format="full">` + fallback + `</span>`)
		},
		"localShortTime": func(t time.Time) template.HTML {
			iso := t.UTC().Format(time.RFC3339)
			fallback := t.UTC().Format("15:04")
			return template.HTML(`<span class="js-local-time" data-utc="` + iso + `" data-format="short">` + fallback + `</span>`)
		},
		// stalledFor: true when t is older than n minutes (an in-flight OTA row with
		// no progress that long is treated as stuck and offered Retry).
		"stalledFor": func(t time.Time, mins int) bool { return time.Since(t) > time.Duration(mins)*time.Minute },
		"nowUTC": func() time.Time {
			return time.Now().UTC()
		},
		// timeUntil is the forward-looking twin of timeSince ("in 3d", "in 2h"); a time
		// already past reads as "just now" / "…ago" so expired things say so.
		"timeUntil": func(t time.Time) string {
			d := time.Until(t)
			if d < 0 {
				d = -d
				switch {
				case d < time.Minute:
					return "just now"
				case d < time.Hour:
					return fmt.Sprintf("%dm ago", int(d.Minutes()))
				case d < 24*time.Hour:
					return fmt.Sprintf("%dh ago", int(d.Hours()))
				default:
					return fmt.Sprintf("%dd ago", int(d.Hours()/24))
				}
			}
			switch {
			case d < time.Minute:
				return "in under a minute"
			case d < time.Hour:
				return fmt.Sprintf("in %dm", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("in %dh", int(d.Hours()))
			default:
				return fmt.Sprintf("in %dd", int(d.Hours()/24))
			}
		},
		// timeSince takes a time.Time or a *time.Time so templates can print an
		// optional timestamp without unwrapping it first.
		"timeSince": func(v any) string {
			var t time.Time
			switch x := v.(type) {
			case time.Time:
				t = x
			case *time.Time:
				if x == nil {
					return "never"
				}
				t = *x
			default:
				return ""
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"batteryWidth": func(pct int) string {
			return fmt.Sprintf("%d%%", pct)
		},
		"shortTime": func(t time.Time) string {
			return t.UTC().Format("15:04")
		},
		// dayLabel: the calendar-day group a timestamp falls in — "Today"/"Yesterday",
		// a weekday name within the last week, else a short date. Used to group the
		// Actions/history timeline by day instead of a per-row "Sent" column.
		"dayLabel": func(t time.Time) string {
			loc := t.Location()
			now := time.Now().In(loc)
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
			switch diff := int(today.Sub(day).Hours() / 24); {
			case diff == 0:
				return "Today"
			case diff == 1:
				return "Yesterday"
			case diff > 1 && diff < 7:
				return t.Format("Monday")
			default:
				return t.Format("Jan 2")
			}
		},
		// shortDate: compact date for the device onboarding column, e.g. "Jun 20, 2026".
		"shortDate": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("Jan 2, 2006")
		},
		// isoDate: yyyy-mm-dd for prefilling an <input type="date">.
		"isoDate": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("2006-01-02")
		},
		"minuteOfDay": func(t time.Time) int {
			u := t.UTC()
			return u.Hour()*60 + u.Minute()
		},
		"reverseCheckins": func(checkins []db.Checkin) []db.Checkin {
			n := len(checkins)
			out := make([]db.Checkin, n)
			for i, c := range checkins {
				out[n-1-i] = c
			}
			return out
		},
		"statusClass": func(s string) string {
			switch s {
			case "installed", "completed":
				return "ok"
			case "failed":
				return "danger"
			case "expired":
				return "warn"
			case "downloading", "installing":
				return "active"
			default:
				return "muted"
			}
		},
		"cmdLabel": cmdTypeLabel,
		// auditDetailHTML linkifies the "cmd=<uuid>" token that command.send audit
		// entries carry (see the CommandCreate/GroupCommandCreate/DeviceCommandCreate
		// handlers) so the Activity page can jump straight to the command that was
		// created, without the Target column losing its readable command-type label.
		// Escapes first — the rest of a detail string (e.g. an operator's free-text
		// reason) is untrusted.
		"auditDetailHTML": func(detail string) template.HTML {
			esc := template.HTMLEscapeString(detail)
			return template.HTML(auditCmdIDRe.ReplaceAllString(esc, `cmd=<a href="/commands/$1">$1</a>`))
		},
		// cmdStatusLabel makes command status honest about what actually happened.
		// "delivered" only means the hub pushed the frame onto the socket, not that the
		// device got it — showing that to users reads as done when it isn't, so it's
		// displayed as "pending" until the device sends a "received" ack. On an offline
		// device, pending/delivered show as "queued" (it runs when the device reconnects).
		// Terminal/in-progress statuses are shown as-is.
		"cmdStatusLabel": func(status string, online bool) string {
			if !online && (status == "delivered" || status == "pending") {
				return "queued"
			}
			if status == "delivered" {
				return "pending"
			}
			return status
		},
		// productLabel maps a product key (e.g. "kiosk22") to its display label for the
		// releases UI, mirroring Device.ProductLabel() on the device side.
		// productLabel: the catalog label, or the real model name for a product the
		// catalog does not know ("Sunmi D3 PRO", not the wire key "d3_pro").
		"productLabel": productLabel,
		// clFormat makes a changelog line scannable: the lead sentence (up to the
		// first ". ") becomes a bold headline, the rest stays as body text. Input is
		// HTML-escaped first, so entries are plain text authored in version.go.
		"clFormat": func(s string) template.HTML {
			esc := template.HTMLEscapeString(s)
			lead, rest := esc, ""
			if i := strings.Index(esc, ". "); i > 0 && i < len(esc)-2 {
				lead, rest = esc[:i+1], esc[i+2:]
			}
			out := `<span class="cl-lead">` + lead + `</span>`
			if rest != "" {
				out += " " + rest
			}
			return template.HTML(out)
		},
		"cmdDetail": func(cmd db.Command) string {
			if cmd.ApkURL != "" {
				return cmd.ApkURL
			}
			if (cmd.Type == "shell" || cmd.Type == "query") && len(cmd.Payload) > 0 {
				var p struct {
					Cmd   string `json:"cmd"`
					Query string `json:"query"`
				}
				if json.Unmarshal(cmd.Payload, &p) == nil {
					// A query shows its friendly catalog label; a raw shell shows the command.
					if cmd.Type == "query" && p.Query != "" {
						return p.Query
					}
					if p.Cmd != "" {
						return p.Cmd
					}
				}
			}
			return "—"
		},
		// cmdHint is a short, human payload summary for the command history (what was run).
		"cmdHint": func(cmd db.Command) string {
			base := func(s string) string {
				if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
					s = s[i+1:]
				}
				return s
			}
			switch cmd.Type {
			case "install_apk":
				if cmd.ApkURL != "" {
					return base(cmd.ApkURL)
				}
			case "uninstall":
				var p struct {
					Package string `json:"package"`
				}
				if json.Unmarshal(cmd.Payload, &p) == nil && p.Package != "" {
					return p.Package
				}
			case "shell":
				var p struct {
					Cmd string `json:"cmd"`
				}
				if json.Unmarshal(cmd.Payload, &p) == nil && p.Cmd != "" {
					return p.Cmd
				}
			case "query":
				var p struct {
					Query string `json:"query"`
					Cmd   string `json:"cmd"`
				}
				if json.Unmarshal(cmd.Payload, &p) == nil {
					if p.Query != "" {
						return p.Query
					}
					if p.Cmd != "" {
						return p.Cmd
					}
				}
			case "logcat":
				var p struct {
					Level string `json:"level"`
					Lines int    `json:"lines"`
					Tag   string `json:"tag"`
				}
				if json.Unmarshal(cmd.Payload, &p) == nil {
					h := p.Level
					if p.Lines > 0 {
						h += fmt.Sprintf(" · %d lines", p.Lines)
					}
					if p.Tag != "" {
						h += " · " + p.Tag
					}
					return h
				}
			case "update_splash":
				return "boot logo"
			}
			return ""
		},
		"logcatStatusClass": func(s string) string {
			switch s {
			case "fulfilled":
				return "ok"
			case "delivered":
				return "warn"
			case "expired":
				return "danger"
			default:
				return "muted"
			}
		},
		"rawJSON": func(b []byte) string { return string(b) },
		// dict builds a map from alternating key/value args for passing ad-hoc data to
		// a sub-template (e.g. the blank "add channel" form row).
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				k, _ := kv[i].(string)
				m[k] = kv[i+1]
			}
			return m
		},
		"derefInt": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		// Nullable-float formatters for the health scorecard.
		"fnum": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return fmt.Sprintf("%.0f", *p)
		},
		"fpct": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return fmt.Sprintf("%.0f%%", *p*100)
		},
		"fdelta": func(p *float64) string {
			if p == nil {
				return "—"
			}
			if *p >= 0 {
				return fmt.Sprintf("▲ %.0f", *p)
			}
			return fmt.Sprintf("▼ %.0f", -*p)
		},
		"ramUsage": func(raw json.RawMessage) map[string]int {
			if len(raw) == 0 {
				return nil
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil
			}
			v, ok := m["ram_usage_mb"]
			if !ok {
				return nil
			}
			var ram map[string]int
			if err := json.Unmarshal(v, &ram); err != nil {
				return nil
			}
			return ram
		},
		"deviceTime": func(raw json.RawMessage) string {
			if len(raw) == 0 {
				return ""
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m["timezone"]
			if !ok {
				return ""
			}
			var tz string
			if err := json.Unmarshal(v, &tz); err != nil {
				return ""
			}
			// Map timezone string to offset.
			var loc *time.Location
			up := strings.ToUpper(tz)
			switch up {
			case "GMT", "UTC", "GMT+0", "GMT-0", "GMT+00:00", "GMT-00:00":
				loc = time.UTC
			default:
				if strings.HasPrefix(up, "GMT") {
					// Parse "GMT±H" AND "GMT±H:MM" (e.g. GMT+5:30 India, GMT-3:30) —
					// the old whole-hour-only parse silently fell back to UTC for
					// half-hour zones, showing a clock hours off with a wrong label.
					off := strings.TrimPrefix(up, "GMT")
					sign := 1
					if strings.HasPrefix(off, "-") {
						sign, off = -1, off[1:]
					} else {
						off = strings.TrimPrefix(off, "+")
					}
					hm := strings.SplitN(off, ":", 2)
					if h, err := strconv.Atoi(hm[0]); err == nil && h >= 0 && h <= 14 {
						mins := 0
						if len(hm) == 2 {
							mins, _ = strconv.Atoi(hm[1])
						}
						if mins >= 0 && mins < 60 {
							loc = time.FixedZone(tz, sign*(h*3600+mins*60))
						}
					}
				} else if l, err := time.LoadLocation(tz); err == nil {
					loc = l // IANA name (e.g. Asia/Kolkata) — DST-aware
				}
			}
			if loc == nil {
				loc = time.UTC
			}
			return time.Now().In(loc).Format("15:04:05") + " (" + tz + ")"
		},
		"charging": func(raw json.RawMessage) bool {
			return extractCharging(raw)
		},
		// batteryMissing: the device reports no pack on a product that should have one
		// (NTC fault or unplugged). Templates show a distinct chip rather than a
		// percentage read off the rail, or the kiosk "AC" chip which means by design.
		"batteryMissing": func(raw json.RawMessage) bool {
			return extractBatteryMissing(raw)
		},
		"notStale": func(t time.Time, thresholdSecs int) bool {
			if thresholdSecs <= 0 {
				return true
			}
			return time.Since(t) <= time.Duration(thresholdSecs)*time.Second
		},
		"batteryTemp": batteryTempStr,
		// micGain parses latest_extra.mic_gain for templates that only have the
		// device (vitals partial); nil when the device reports no mic gain.
		"micGain": micGainPtr,
		"batteryTempX": func(raw json.RawMessage) string {
			temp, ok := extractBatteryTempC(raw)
			if !ok {
				return ""
			}
			return fmt.Sprintf("%.1f°C", temp)
		},
		"tempClass": func(raw json.RawMessage) string {
			temp, ok := extractBatteryTempC(raw)
			if !ok {
				return ""
			}
			switch {
			case temp >= 60:
				return "danger"
			case temp >= 45:
				return "warn"
			case temp <= -10:
				return "danger"
			case temp <= 0:
				return "warn"
			default:
				return "ok"
			}
		},
		"ramPct": func(ram map[string]int) int {
			total, ok := ram["total"]
			if !ok || total == 0 {
				return 0
			}
			used, ok := ram["used"]
			if !ok {
				return 0
			}
			pct := used * 100 / total
			if pct > 100 { // used>total can happen from free/cached accounting skew
				pct = 100
			}
			return pct
		},
		"extraTempC": func(raw json.RawMessage) template.JS {
			temp, ok := extractBatteryTempC(raw)
			if !ok {
				return "null"
			}
			return template.JS(fmt.Sprintf("%.1f", temp))
		},
		"rowClasses": func(dev db.Device) string {
			return deviceRowClasses(dev)
		},
		"colorizeLogcat": func(content string) template.HTML {
			return colorizeLogcatText(content)
		},
		"extraRamPct": func(raw json.RawMessage) template.JS {
			if len(raw) == 0 {
				return "null"
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return "null"
			}
			v, ok := m["ram_usage_mb"]
			if !ok {
				return "null"
			}
			var ram map[string]int
			if err := json.Unmarshal(v, &ram); err != nil {
				return "null"
			}
			total := ram["total"]
			if total == 0 {
				return "null"
			}
			pct := float64(ram["used"]) * 100 / float64(total)
			if pct > 100 {
				pct = 100
			}
			return template.JS(fmt.Sprintf("%.1f", pct))
		},
		"wlcStatus": func(raw json.RawMessage) template.JS {
			if len(raw) == 0 {
				return "undefined"
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return "undefined"
			}
			v, ok := m["wlc_status"]
			if !ok {
				return "undefined"
			}
			var n int
			if err := json.Unmarshal(v, &n); err != nil {
				return "undefined"
			}
			if n < 0 || n > 2 {
				return "undefined"
			}
			return template.JS(strconv.Itoa(n))
		},
		"mbToGB": func(mb int) string { return fmt.Sprintf("%.1f GB", float64(mb)/1024) },
		"pctOf": func(n, total int) int {
			if total <= 0 {
				return 0
			}
			p := n * 100 / total
			if p > 100 {
				p = 100
			}
			return p
		},
		// uptimeShort formats latest_extra.uptime_seconds as "6d 4h" / "4h 20m" / "12m".
		"uptimeShort": func(raw []byte) string {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				return ""
			}
			v, ok := m["uptime_seconds"]
			if !ok {
				return ""
			}
			var sec int64
			if json.Unmarshal(v, &sec) != nil {
				var s string
				if json.Unmarshal(v, &s) == nil {
					sec, _ = strconv.ParseInt(s, 10, 64)
				}
			}
			if sec <= 0 {
				return ""
			}
			d := sec / 86400
			h := (sec % 86400) / 3600
			mn := (sec % 3600) / 60
			switch {
			case d > 0:
				return fmt.Sprintf("%dd %dh", d, h)
			case h > 0:
				return fmt.Sprintf("%dh %dm", h, mn)
			default:
				return fmt.Sprintf("%dm", mn)
			}
		},
		"nearbyWifi": func(raw []byte) []geolocate.WifiAP {
			aps := geolocate.ExtractWifiScan(raw)
			sort.Slice(aps, func(i, j int) bool { return aps[i].RSSI > aps[j].RSSI })
			return aps
		},
		// Capability gating (see internal/product/caps.go): offer a control only when the
		// device can honour it. `supports` is the yes/no; `degraded` adds "with limits".
		"supports":   func(d db.Device, cmdType string) bool { return d.Supports(cmdType) },
		"degraded":   func(d db.Device, cmdType string) bool { return d.Degraded(cmdType) },
		// joinLines renders a serial list for the shared picker's hidden field.
		"joinLines": func(v []string) string { return strings.Join(v, "\n") },
		"classLabel": product.ClassLabel,
		"classes":    product.Classes,
		"extraField": func(raw []byte, key string) string {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m[key]
			if !ok {
				return "—"
			}
			// strip quotes for plain strings
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				return s
			}
			return string(v)
		},
		"deployStatusClass": func(s string) string {
			switch s {
			case "complete", "installed":
				return "ok"
			case "active", "downloading", "awaiting_reboot", "reboot_sent":
				return "warn"
			case "failed":
				return "danger"
			default:
				return "muted"
			}
		},
		"releaseStatusClass": func(s string) string {
			switch s {
			case "published":
				return "ok"
			default: // draft
				return "muted"
			}
		},
		// otaErrorText turns a device-reported OTA error_code into operator-readable
		// text. The client sends DOWNLOAD_ERROR, UPDATE_ENGINE_BIND_ERROR, or
		// UPDATE_ERROR_<n> where <n> is an UpdateEngine.ErrorCodeConstants value.
		"otaErrorText": func(code string) string {
			if code == "" {
				return ""
			}
			switch code {
			case "UPDATE_ERROR_21":
				return "The package on the device did not read as an update (invalid payload header). Usually a resumed download on top of an older cancelled one — Retry downloads it fresh."
			case "SLOT_SWITCH_FAILED":
				return "Rebooted, but came back on the old build — the update was written but the slot switch did not take. Push the update again."
			case "REBOOT_NO_CHECKIN":
				return "Rebooted and never checked in again. The device has been offline since; check it on site, then Retry."
			case "DOWNLOAD_ERROR":
				return "Download failed — the device couldn't fetch the OTA package (check the URL is reachable from the device)."
			case "UPDATE_ENGINE_BIND_ERROR":
				return "Couldn't reach the device's system update service (update_engine)."
			case "CANCELLED":
				return "Cancelled by an operator while still downloading."
			}
			if n, ok := strings.CutPrefix(code, "UPDATE_ERROR_"); ok {
				if txt := updateEngineErrors[n]; txt != "" {
					return txt
				}
				return "update_engine error code " + n + "."
			}
			return code
		},
		// hrs renders a minute count for a power/usage tile. Whole hours once there is an
		// hour to show, otherwise minutes — twenty minutes on the charging pad is a real
		// reading and must not round to "0 hrs".
		"hrs": func(mins float64) string {
			switch {
			case mins <= 0:
				return "0"
			case mins < 60:
				return strconv.FormatFloat(mins, 'f', 0, 64) + " min"
			case mins < 600:
				return strconv.FormatFloat(mins/60, 'f', 1, 64)
			default:
				return strconv.FormatFloat(mins/60, 'f', 0, 64)
			}
		},
		// basename shows the artifact's file name instead of a long S3 URL whose first
		// 50 characters are identical on every package of every release.
		"basename": func(u string) string {
			if i := strings.IndexByte(u, '?'); i >= 0 {
				u = u[:i]
			}
			u = strings.TrimSuffix(u, "/")
			if i := strings.LastIndexByte(u, '/'); i >= 0 && i+1 < len(u) {
				return u[i+1:]
			}
			return u
		},
		"truncate": func(s string, n int) string {
			runes := []rune(s)
			if len(runes) <= n {
				return s
			}
			return string(runes[:n]) + "…"
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		// pctOfF is pctOf for the float durations the power/usage bars are drawn from.
		"pctOfF": func(n, total float64) int {
			if total <= 0 {
				return 0
			}
			p := int(n * 100 / total)
			if p > 100 {
				return 100
			}
			if p < 0 {
				return 0
			}
			return p
		},
		// dash: SVG ring stroke-dashoffset for a 0–100 value over a circumference.
		"dash": func(circ float64, pct int) string {
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
			return fmt.Sprintf("%.1f", circ*float64(100-pct)/100)
		},
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		// dur formats a duration in seconds as a compact "1h 2m" / "4m 23s" / "45s".
		// Negative means "not available" and renders as an em dash.
		"dur": func(sec int) string {
			if sec < 0 {
				return "—"
			}
			if sec < 60 {
				return fmt.Sprintf("%ds", sec)
			}
			if sec < 3600 {
				return fmt.Sprintf("%dm %ds", sec/60, sec%60)
			}
			return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
		},
		"hasBit": func(mask, bit int) bool { return mask&bit != 0 },
		"iter": func(start, end int) []int {
			var out []int
			for i := start; i <= end; i++ {
				out = append(out, i)
			}
			return out
		},
		// pageList returns a windowed page sequence for pagers: first, last, and the
		// current page ±1, with 0 marking an ellipsis gap (so we don't render every page).
		"pageList": func(cur, total int) []int {
			if total < 1 {
				return nil
			}
			if total <= 7 {
				out := make([]int, total)
				for i := range out {
					out[i] = i + 1
				}
				return out
			}
			want := map[int]bool{1: true, total: true, cur: true}
			if cur-1 >= 1 {
				want[cur-1] = true
			}
			if cur+1 <= total {
				want[cur+1] = true
			}
			var out []int
			prev := 0
			for p := 1; p <= total; p++ {
				if !want[p] {
					continue
				}
				if prev != 0 && p-prev > 1 {
					out = append(out, 0) // ellipsis
				}
				out = append(out, p)
				prev = p
			}
			return out
		},
	}

	// Weekly report helpers. Rollups are kept in minutes throughout; the report is the
	// only place that reads in hours, so the conversion lives here rather than in the
	// query.
	funcMap["hrs1"] = func(minutes float64) string { return fmt.Sprintf("%.1f", minutes/60) }
	// band colours an uptime meter: the legend in the report footer names these.
	funcMap["band"] = func(pct int) string {
		switch {
		case pct < 70:
			return "risk"
		case pct < 85:
			return "warn"
		}
		return ""
	}
	// padDrainBar scales a %/min drain rate onto a meter. 0.2%/min is the full bar:
	// at that rate a full battery is gone in about eight hours of pad use.
	funcMap["padDrainBar"] = func(rate float64) int { return pctCapped(rate, 0.2) }
	// initials is the two-character tile in front of a serial: its last two characters,
	// which are what actually differs between units on one site.
	funcMap["initials"] = func(serial string) string {
		if len(serial) <= 2 {
			return strings.ToUpper(serial)
		}
		return strings.ToUpper(serial[len(serial)-2:])
	}

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	// S3 APK store — nil (feature disabled) when S3_BUCKET is unset or AWS config fails.
	apkStore, apkErr := apkstore.New(context.Background())
	if apkErr != nil {
		log.Printf("apkstore init: %v (APK upload disabled)", apkErr)
	}

	return &Handler{
		db:            d,
		hub:           hub,
		apk:           apkStore,
		shell:         shellMgr,
		remote:        remoteMgr,
		logs:          logMgr,
		store:         store,
		tmpl:          tmpl,
		user:          user,
		password:      password,
		cfg:           cfg,
		adminAPIKey:   adminAPIKey,
		mapsEmbedKey:  mapsEmbedKey,
		geo:           geo,
		geocoder:      geocoder,
		startedAt:     time.Now(),
		alerts:        alerts.NewDispatcher(d, cfg),
		otaGate:       otagate.New(d, cfg),
		publicOrigins:  parseOrigins(os.Getenv("PUBLIC_ORIGIN")),
		loginFails:     ratelimit.New(15 * time.Minute),
		signupAttempts: ratelimit.New(time.Hour),
		resetAttempts:  ratelimit.New(time.Hour),
		mail:           mustMailer(),
		msClientID:     os.Getenv("MS_CLIENT_ID"),
		msClientSecret: os.Getenv("MS_CLIENT_SECRET"),
		msTenantID:     os.Getenv("MS_TENANT_ID"),
		assetVer:       assetVersion("static"),
	}
}

// mustMailer builds the SES mailer client, using AWS_REGION (the same var
// internal/apkstore reads for S3) and the default AWS credential chain — no
// SES-specific credentials needed. Falls back to a disabled client (Send logs,
// no-ops) on error so a misconfigured/missing AWS setup never blocks startup.
func mustMailer() *mailer.Client {
	m, err := mailer.New(context.Background(), os.Getenv("AWS_REGION"), mailFrom())
	if err != nil {
		log.Printf("mailer init: %v (email sending disabled)", err)
		m, _ = mailer.New(context.Background(), "", mailFrom())
	}
	return m
}

// mailFrom returns the SES "from" address, defaulting to a sensible sender when
// MAIL_FROM_EMAIL is unset. Must be a verified SES identity (or in a verified
// domain) or SES will reject the send. Defaults to the dev.aioapp.com subdomain
// (kept separate from the bare aioapp.com domain another app already sends from)
// — set MAIL_FROM_EMAIL to override.
func mailFrom() string {
	if from := os.Getenv("MAIL_FROM_EMAIL"); from != "" {
		return from
	}
	return "AIO MDM <mdm@dev.aioapp.com>"
}

// assetVersion returns a short cache-busting token for a static asset, derived
// from its modification time and size. Falls back to the build version if the
// file can't be stat'd, so the URL is always well-formed.
// assetVersion is the ?v= stamp on every /static/ URL. Static files are served
// "immutable" for a week, so the stamp is the only thing that can pull a changed
// file through a browser cache. It therefore covers the WHOLE tree, not one file:
// stamping it from style.css alone meant a deploy that changed picker.js but not
// the stylesheet shipped new behaviour nobody's browser would fetch.
func assetVersion(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return version.Current()
	}
	if !fi.IsDir() {
		return fmt.Sprintf("%x-%x", fi.ModTime().UnixNano(), fi.Size())
	}
	var newest int64
	var total, count int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // an unreadable entry just doesn't count
		}
		if m := info.ModTime().UnixNano(); m > newest {
			newest = m
		}
		total += info.Size()
		count++
		return nil
	})
	return fmt.Sprintf("%x-%x-%x", newest, total, count)
}

// parseOrigins splits the comma-separated PUBLIC_ORIGIN env into a normalized
// list of trusted origins (trailing slash and surrounding space removed).
func parseOrigins(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if o := strings.TrimRight(strings.TrimSpace(p), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// baseURL returns an absolute origin ("https://mdm.example.com") for building
// links that must work outside the current request's context (device-fetched APK
// URLs, emailed verify/reset links). Prefers the configured PUBLIC_ORIGIN;
// falls back to the request's own scheme+host.
func (h *Handler) baseURL(r *http.Request) string {
	if len(h.publicOrigins) > 0 {
		return h.publicOrigins[0]
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// loginMaxFailures is the number of failed login attempts (per IP or per
// account) within the limiter window before further attempts are refused.
const loginMaxFailures = 8

// sessionIdleTimeout logs out a session that has gone this long without an
// authenticated request, independent of the absolute lifetime
// (cfg.SessionTimeout(), default 24h). Activity slides last_seen forward
// (touchSession).
const sessionIdleTimeout = 24 * time.Hour

// currentSession resolves the server-side session referenced by the request
// cookie, enforcing both the absolute expiry and the idle timeout. An invalid,
// expired, or idle-timed-out session is deleted and reported as absent. This
// does a primary-key lookup per call; the dashboard is low-traffic, so the few
// calls per request are acceptable.
// ctxSessionKey lets a handler inject a synthetic session for the duration of one
// request (see SneakPeek), so the session-derived chrome (role, name, access)
// renders without a real cookie/DB session. Only ever set server-side.
type ctxSessionKey struct{}

func (h *Handler) currentSession(r *http.Request) (*db.Session, bool) {
	if s, ok := r.Context().Value(ctxSessionKey{}).(*db.Session); ok {
		return s, true
	}
	cookie, err := h.store.Get(r, "mdm-session")
	if err != nil {
		return nil, false
	}
	sid, _ := cookie.Values["sid"].(string)
	if sid == "" {
		return nil, false
	}
	s, err := h.db.GetSession(r.Context(), sid)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	if now.After(s.ExpiresAt) || now.Sub(s.LastSeen) > sessionIdleTimeout {
		_ = h.db.DeleteSession(r.Context(), sid)
		return nil, false
	}
	return s, true
}

// touchSession slides the session's last_seen forward so an active user is not
// idle-timed-out mid-use. Called once per authenticated request by the route
// guards. Best-effort; never blocks the request.
func (h *Handler) touchSession(r *http.Request) {
	cookie, err := h.store.Get(r, "mdm-session")
	if err != nil {
		return
	}
	if sid, _ := cookie.Values["sid"].(string); sid != "" {
		_ = h.db.TouchSession(r.Context(), sid)
	}
}

// newSessionID returns a 256-bit random opaque session identifier.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// startSession creates a server-side session row and writes its id into the
// signed cookie. userID is nil for the static env-configured admin.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, userID *uuid.UUID, username, role string) error {
	sid, err := newSessionID()
	if err != nil {
		return err
	}
	if err := h.db.CreateSession(r.Context(), db.Session{
		ID:        sid,
		UserID:    userID,
		Username:  username,
		Role:      role,
		ExpiresAt: time.Now().Add(time.Duration(h.cfg.SessionTimeout()) * time.Second),
	}); err != nil {
		return err
	}
	cookie, _ := h.store.Get(r, "mdm-session")
	cookie.Values = map[interface{}]interface{}{"sid": sid}
	return cookie.Save(r, w)
}

func (h *Handler) isAdmin(r *http.Request) bool {
	s, ok := h.currentSession(r)
	return ok && s.Role == "admin"
}

// isAuthenticated is kept for compatibility; use isAdmin for admin-only checks.
func (h *Handler) isAuthenticated(r *http.Request) bool { return h.isAdmin(r) }

func (h *Handler) isLoggedIn(r *http.Request) bool {
	_, ok := h.currentSession(r)
	return ok
}

func (h *Handler) role(r *http.Request) string {
	if s, ok := h.currentSession(r); ok {
		return s.Role
	}
	return ""
}

func (h *Handler) currentUsername(r *http.Request) string {
	if s, ok := h.currentSession(r); ok {
		return s.Username
	}
	return ""
}

// auditAs is h.audit with an explicit actor, for the sign-up/verify-email flows
// where the action happens before any session exists — h.audit's session-derived
// actor would otherwise fall back to "unknown".
func (h *Handler) auditAs(r *http.Request, actor, action, target, detail string) {
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAudit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[audit] insert failed: %v", err)
	}
}

// currentDisplayName is currentUsername, but prefers the user's first/last name
// (set at sign-up or by an admin) over their bare username/email when one is on
// file — shown top-right in the dashboard chrome and snapshotted onto commands
// and audit entries so "who did this" reads as a name where possible.
func (h *Handler) currentDisplayName(r *http.Request) string {
	s, ok := h.currentSession(r)
	if !ok {
		return ""
	}
	if s.UserID != nil {
		if u, err := h.db.GetUser(r.Context(), *s.UserID); err == nil && u != nil {
			return u.DisplayName()
		}
	}
	if s.Username == "admin" {
		return builtinAdminDisplayName
	}
	return s.Username
}

// builtinAdminDisplayName is shown for the config-defined "admin" login, which has
// no users row (and so no first/last name) to draw a display name from.
const builtinAdminDisplayName = "Ali"

// audit records an admin action (best-effort; never blocks the request).
//
// The actor is the user's stable username (their login identity), not their
// display name — a display name is editable from the Users page, and a
// snapshotted name would freeze at whatever it was on the day of the action.
// Storing the username lets every render path resolve it to the user's
// CURRENT display name (see actorDisplayNames), so a rename shows up on
// every past action too, not just new ones.
func (h *Handler) audit(r *http.Request, action, target, detail string) {
	actor := h.currentUsername(r)
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAudit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[audit] insert failed: %v", err)
	}
}

// actorDisplayNames maps every user's stable username to their current display
// name, for resolving the username snapshotted onto commands/audit_log rows
// (see audit and the CreateCommandBy call sites) back to a human name at
// render time — live, so a Users-page rename is reflected on every past
// action instead of being frozen at whatever the name was when it was logged.
func (h *Handler) actorDisplayNames(ctx context.Context) map[string]string {
	users, err := h.db.ListUsers(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.Username] = u.DisplayName()
	}
	return m
}

// resolveCommandActors rewrites each command's CreatedBy (a snapshotted
// username, or "" for system-initiated) to the user's current display name,
// in place. Falls back to leaving CreatedBy as-is when it doesn't match a
// known username (a pre-rename-fix row already carrying a snapshotted display
// name, or a non-account actor like "Scheduled recipe: ...").
func (h *Handler) resolveCommandActors(ctx context.Context, cmds []db.Command) {
	m := h.actorDisplayNames(ctx)
	if m == nil {
		return
	}
	for i := range cmds {
		if dn, ok := m[cmds[i].CreatedBy]; ok {
			cmds[i].CreatedBy = dn
		}
	}
}

func (h *Handler) withRole(r *http.Request, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	role := h.role(r)
	data["Role"] = role
	// Boosted: an hx-boost navigation. When true the layout emits only <main> so
	// htmx swaps just the content region (dock + footer scripts stay put).
	data["Boosted"] = r.Header.Get("HX-Boosted") == "true"
	data["CurrentUser"] = h.currentDisplayName(r)
	if userBubbleFn != nil {
		data["CurrentBubble"] = userBubbleFn(h.currentUsername(r))
	}
	data["Brand"] = h.cfg.CustomBrand()
	data["Use24Hour"] = h.cfg.Use24Hour()
	data["Version"] = version.Current()
	// First-run walkthrough: only on full page loads (the tour lives in the
	// non-boosted chrome), once per user.
	if role != "" && r.Header.Get("HX-Boosted") != "true" && r.Header.Get("HX-Request") != "true" {
		data["ShowTour"] = h.showTour(r)
	}
	data["AssetVer"] = h.assetVer
	if role != "" {
		if n, err := h.db.CountOpenAlerts(r.Context()); err == nil {
			data["AlertsOpenCount"] = n
		}
	}

	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/fleet-health"):
		data["ActivePage"] = "health"
	case strings.HasPrefix(path, "/alerts"):
		data["ActivePage"] = "alerts"
	case path == "/" || path == "/map":
		data["ActivePage"] = "overview"
	case strings.HasPrefix(path, "/devices") || path == "/export" || path == "/packages":
		data["ActivePage"] = "devices"
	case strings.HasPrefix(path, "/groups"):
		data["ActivePage"] = "groups"
	case strings.HasPrefix(path, "/restaurants"):
		data["ActivePage"] = "restaurants"
	case strings.HasPrefix(path, "/productions"):
		data["ActivePage"] = "productions"
	case strings.HasPrefix(path, "/manage"):
		data["ActivePage"] = "manage"
	case strings.HasPrefix(path, "/commands"):
		data["ActivePage"] = "commands"
	case strings.HasPrefix(path, "/releases"):
		// One dock entry ("Updates") covers releases and rollouts alike.
		data["ActivePage"] = "updates"
	case strings.HasPrefix(path, "/updates-policy"):
		data["ActivePage"] = "updates-policy"
	case strings.HasPrefix(path, "/updates"):
		data["ActivePage"] = "updates"
	case strings.HasPrefix(path, "/network"):
		data["ActivePage"] = "network"
	case strings.HasPrefix(path, "/compliance"):
		data["ActivePage"] = "compliance"
	case strings.HasPrefix(path, "/geofencing"):
		data["ActivePage"] = "geofencing"
	case strings.HasPrefix(path, "/setup/managed-configs"):
		data["ActivePage"] = "managed-configs" // lives under the Policies hub
	case strings.HasPrefix(path, "/setup"):
		data["ActivePage"] = "setup"
	case strings.HasPrefix(path, "/settings"):
		data["ActivePage"] = "settings"
	case strings.HasPrefix(path, "/users"):
		data["ActivePage"] = "users"
	case strings.HasPrefix(path, "/activity"), strings.HasPrefix(path, "/profile"):
		data["ActivePage"] = "users"
	case strings.HasPrefix(path, "/changelog"):
		data["ActivePage"] = "changelog"
	}
	// The unified Fleet surface (Devices/Restaurants/Groups tabs) needs all three
	// counts for its tab strip; fetch them only on those pages.
	if ap, _ := data["ActivePage"].(string); ap == "devices" || ap == "groups" || ap == "restaurants" || ap == "enrollment" {
		if fc, err := h.db.FleetCounts(r.Context(), h.access(r).hidesDPC()); err == nil {
			data["FleetCounts"] = fc
		}
	}
	return data
}

// prefetchableTemplates are the primary dock-nav destinations (see .dock in
// layout.html: Overview/Fleet/Health/Actions/Releases). The client warms these in
// the background after a page settles (see mdmPrefetchNav in layout.html) so
// clicking between them feels instant. Scoped to exactly these five, boosted-only —
// see the Cache-Control comment below for why this can't just apply everywhere.
var prefetchableTemplates = map[string]bool{
	"overview.html": true, "devices.html": true, "health.html": true,
	"commands.html": true, "updates.html": true,
}

// pageViewSkip are full-page templates that are not "the user looked at
// something": auth screens, error pages, and pages that are their own record.
var pageViewSkip = map[string]bool{"landing.html": true, "login.html": true, "404.html": true, "maintenance.html": true, "signup.html": true, "forgot_password.html": true, "reset_password.html": true, "verify_email.html": true}

// recentViews dedupes page-view audit rows: one per user+path per minute, so a
// reload or a live-refresh doesn't multiply entries.
var recentViews sync.Map // "user|path" -> time.Time

// logPageView records a dashboard page navigation in the audit log as
// PageViewAction. Only real navigations count: a GET that renders a full page
// template (a boosted nav or a hard load), not htmx partials or polls. Written
// asynchronously so the page is never slowed by it.
func (h *Handler) logPageView(r *http.Request, name string, data map[string]any) {
	if r.Method != http.MethodGet || !strings.HasSuffix(name, ".html") || pageViewSkip[name] {
		return
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		return // partial refresh, not a navigation
	}
	user := h.currentUsername(r)
	if user == "" {
		return
	}
	path := r.URL.Path
	key := user + "|" + path
	now := time.Now()
	if v, ok := recentViews.Load(key); ok && now.Sub(v.(time.Time)) < time.Minute {
		return
	}
	recentViews.Store(key, now)
	title, _ := data["Title"].(string)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.db.InsertAudit(ctx, user, db.PageViewAction, path, title); err != nil {
			log.Printf("[audit] page view: %v", err)
		}
	}()
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	// Execute into a buffer first: a template runtime error partway through
	// otherwise leaves a half-written page (header/dock already flushed, body
	// truncated) with the error silently discarded — the "blank page, no log"
	// failure mode. Buffering lets us turn that into a logged 500 instead.
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, h.withRole(r, data)); err != nil {
		log.Printf("template render %s (%s %s): %v", name, r.Method, r.URL.Path, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.logPageView(r, name, data)
	// Dynamic/authenticated responses default to no-store (SecurityHeaders). Punch a
	// short, narrow hole in that for the boosted (HX-Request) response of the five
	// main dock-nav pages only, so the browser's own HTTP cache can serve the
	// background prefetch's response back to htmx's real navigation fetch a few
	// seconds later — both requests carry the same HX-Request/HX-Boosted headers, so
	// Vary keys them to the same cache entry, and htmx never knows the difference.
	// The FULL (non-boosted) document response for these same URLs is untouched —
	// only this HX-Request-flagged fragment variant becomes briefly cacheable.
	if r.Header.Get("HX-Request") == "true" && prefetchableTemplates[name] {
		w.Header().Set("Cache-Control", "private, max-age=10")
		w.Header().Set("Vary", "HX-Request, HX-Boosted")
	}
	buf.WriteTo(w)
}

// NotFoundMiddleware serves the branded 404 page for any request that doesn't
// match a registered route, instead of net/http's bare "404 page not found" text.
// Must wrap the fully-populated mux (after RegisterRoutes and any other route
// registration), since ServeMux.Handler reports a match only against what's
// registered on it at call time — this reads it per-request, so registration
// order relative to wrapping doesn't matter, only that mux is the same instance.
func (h *Handler) NotFoundMiddleware(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			w.WriteHeader(http.StatusNotFound)
			h.render(w, r, "404.html", map[string]any{"Title": "Not found"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Changelog renders the "What's new" page from the in-binary version.Changelog.
func (h *Handler) Changelog(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "changelog.html", map[string]any{
		"Title":     "What's new",
		"Changelog": version.Changelog,
	})
}

// ChangelogLatest renders just the newest release as an HTML fragment for the
// top-bar "What's new" popup (lazy-loaded via htmx when the popup opens).
func (h *Handler) ChangelogLatest(w http.ResponseWriter, r *http.Request) {
	var latest *version.Entry
	if len(version.Changelog) > 0 {
		latest = &version.Changelog[0]
	}
	h.render(w, r, "changelog_latest.html", map[string]any{
		"Entry":   latest,
		"Version": version.Current(),
	})
}

// maintenanceGate sends non-admin users to the maintenance page while maintenance
// mode is on. Returns true when it handled the response. Admins pass; the device API
// never goes through the dashboard wrappers, so devices are unaffected.
func (h *Handler) maintenanceGate(w http.ResponseWriter, r *http.Request) bool {
	// Restaurant owners get a single page: their home plus their own devices'
	// pages (visibility-filtered) and their profile. Anything else goes home.
	if h.role(r) == "owner" && !ownerAllowedPath(r.URL.Path) {
		if hxReq(r) {
			w.Header().Set("HX-Redirect", "/")
			w.WriteHeader(http.StatusOK)
			return true
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return true
	}
	if !h.cfg.MaintenanceMode() || h.role(r) == "admin" {
		return false
	}
	if hxReq(r) {
		w.Header().Set("HX-Redirect", "/maintenance")
		w.WriteHeader(http.StatusOK)
		return true
	}
	http.Redirect(w, r, "/maintenance", http.StatusFound)
	return true
}

func ownerAllowedPath(p string) bool {
	if p == "/" || p == "/profile" || p == "/logout" || p == "/maintenance" || p == "/alerts/events" {
		return true
	}
	for _, pre := range []string{"/devices/", "/icon/", "/tour/", "/overview/layout", "/theme"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// OwnerHome is the restaurant owner's whole dashboard. Which devices it shows
// comes from their access policy (allow "view" per restaurant). Everything is
// phrased for someone who runs a restaurant, not a fleet: a health score with a
// sentence, a to-do list only when something needs a hand, today's service as
// an hourly strip, station cards, and a week-over-week report card.
func (h *Handler) OwnerHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acc := h.access(r)
	ids := acc.visibleIDs()
	if ids == nil {
		ids = []uuid.UUID{}
	}
	var devices []db.Device
	if len(ids) > 0 {
		var err error
		devices, err = h.db.ListDevices(ctx, db.DeviceFilter{OnlyIDs: ids, Connected: h.connectedSlice()}, 0, 500, "serial", "asc")
		if err != nil {
			log.Printf("[owner] list devices: %v", err)
		}
	}
	connected := h.hub.ConnectedIDsForDisplay()
	nick, _ := h.db.GetNicknames(ctx, ids)
	incidents, incCur, incPrev, _ := h.db.IncidentCounts(ctx, ids, 7)

	type station struct {
		Serial, Name, Product string
		Online, HasBattery    bool
		Battery               int
		Charging              bool
		Pad                   string // "docked" | "vacant" | "faulty" | ""
		Kiosk                 bool
		LastSeen              time.Time
		Temp                  string
		Crashes               int
		State                 string // ok | warn | bad
		Word                  string // roster state word
		Do                    template.HTML // what to do (may contain <b>)
	}
	type todo struct {
		Sev, Title, Body, Serial string
		Kind, Station            string // Kind groups like problems into one "Needs you" row
	}
	type venue struct {
		Name            string
		Stations        []station
		Online, Offline int
	}
	byName := map[string]*venue{}
	var order []string
	var todos []todo
	total, online, lowBatt := len(devices), 0, 0
	var restaurantID *uuid.UUID
	for _, d := range devices {
		name := d.RestaurantName
		if name == "" {
			name = "Unassigned"
		}
		if restaurantID == nil && d.RestaurantID != nil {
			restaurantID = d.RestaurantID
		}
		v, ok := byName[name]
		if !ok {
			v = &venue{Name: name}
			byName[name] = v
			order = append(order, name)
		}
		_, on := connected[d.ID]
		st := station{Serial: d.SerialNumber, Name: nick[d.ID], Product: product.Label(d.Product), Online: on,
			HasBattery: d.HasBattery(), Battery: d.BatteryPct, Kiosk: d.KioskEnabled, LastSeen: d.LastSeenAt, Crashes: incidents[d.ID]}
		if st.Name == "" {
			st.Name = d.SerialNumber
		}
		if len(d.LatestExtra) > 0 {
			var ex map[string]json.RawMessage
			if json.Unmarshal(d.LatestExtra, &ex) == nil {
				if v, ok := ex["charging"]; ok {
					var b bool
					if json.Unmarshal(v, &b) == nil {
						st.Charging = b
					}
				}
			}
			if w := wlcIntFromExtra(d.LatestExtra); w != nil {
				st.Pad = map[int]string{0: "vacant", 1: "docked", 2: "faulty"}[*w]
			}
			st.Temp = strings.TrimSpace(batteryTempStr(d.LatestExtra))
		}
		st.State = "ok"
		if on {
			online++
			v.Online++
		} else {
			v.Offline++
			st.State = "bad"
			todos = append(todos, todo{Sev: "bad", Kind: "offline", Station: st.Name, Title: st.Name + " is offline", Serial: d.SerialNumber,
				Body: "Last seen " + timeSinceStr(d.LastSeenAt) + ". Check that it is powered on and connected to Wi-Fi."})
		}
		if st.HasBattery && st.Battery < 20 {
			lowBatt++
			if !st.Charging {
				if st.State == "ok" {
					st.State = "warn"
				}
				todos = append(todos, todo{Sev: "warn", Kind: "battery", Station: st.Name, Title: st.Name + " is at " + strconv.Itoa(st.Battery) + "% and not charging", Serial: d.SerialNumber,
					Body: "Put it back on its charging dock so it lasts through service."})
			}
		}
		if st.Pad == "faulty" {
			if st.State == "ok" {
				st.State = "warn"
			}
			todos = append(todos, todo{Sev: "warn", Kind: "pad", Station: st.Name, Title: st.Name + "'s charging pad is misplaced", Serial: d.SerialNumber,
				Body: "Connection issue between the pad and the device. Please place it again; if it keeps happening, let the MDM team know."})
		}
		if on && !d.KioskEnabled {
			todos = append(todos, todo{Sev: "info", Kind: "kiosk", Station: st.Name, Title: st.Name + " is not locked to the app", Serial: d.SerialNumber,
				Body: "Guests can leave the app. Ask the MDM team to turn kiosk mode back on."})
		}
		if st.Crashes >= 3 {
			todos = append(todos, todo{Sev: "warn", Kind: "crashes", Station: st.Name, Title: st.Name + " crashed " + strconv.Itoa(st.Crashes) + " times this week", Serial: d.SerialNumber,
				Body: "The MDM team can see the details; a restart usually helps in the meantime."})
		}
		switch {
		case !on:
			st.Word, st.Do = "Offline", template.HTML("<b>Check the power cable</b> and that it is on the Wi-Fi.")
		case st.HasBattery && st.Battery < 20 && !st.Charging:
			st.Word, st.Do = "Not charging", template.HTML("<b>Put it back on the dock.</b> At "+strconv.Itoa(st.Battery)+"% it will not last the shift.")
		case st.Pad == "faulty":
			st.Word, st.Do = "Pad misplaced", template.HTML("<b>Please place it again</b> on the charging pad. If it keeps happening, tell the MDM team.")
		default:
			st.Word = "Ready"
			do := "Docked, locked, online."
			switch {
			case !st.HasBattery:
				do = "Plugged in, locked, online."
			case st.Charging && st.Battery < 100:
				do = "Charging, locked, online."
			case st.Pad == "vacant":
				do = "Off the dock but charged. Locked, online."
			}
			if !d.KioskEnabled {
				do = strings.Replace(do, "locked, ", "", 1) + " Not locked to the menu."
			}
			if st.Crashes > 0 {
				do += " Restarted " + strconv.Itoa(st.Crashes) + " time" + map[bool]string{true: "s", false: ""}[st.Crashes != 1] + " this week."
			}
			st.Do = template.HTML(do)
		}
		v.Stations = append(v.Stations, st)
	}
	rankOf := map[string]int{"bad": 0, "warn": 1, "ok": 2}
	for _, v := range byName {
		sort.SliceStable(v.Stations, func(i, j int) bool { return rankOf[v.Stations[i].State] < rankOf[v.Stations[j].State] })
	}
	var venues []*venue
	for _, n := range order {
		venues = append(venues, byName[n])
	}

	// Health score: offline share weighs most, then battery, pad and crashes.
	score := 100
	if total > 0 {
		score -= 60 * (total - online) / total
		score -= 15 * lowBatt / total
		if incCur > 0 {
			score -= min(15, 3*incCur)
		}
		for _, t := range todos {
			if t.Sev == "warn" && strings.Contains(t.Title, "pad") {
				score -= 5
				break
			}
		}
	}
	if score < 0 {
		score = 0
	}
	scoreClass, scoreWord := "ok", "Running smoothly"
	switch {
	case score < 50:
		scoreClass, scoreWord = "bad", "Needs your attention"
	case score < 80:
		scoreClass, scoreWord = "warn", "Mostly fine, a few things to fix"
	}

	// Today's service: hourly online counts for 24h, with the venue's service
	// window shaded and uptime-during-service computed for today so far.
	hourly, _ := h.db.HourlyOnline(ctx, ids, 24)
	type hourCell struct {
		Label   string
		Pct     int
		Service bool
		Now     bool
	}
	var win *db.ServiceWindow
	if restaurantID != nil {
		if sw, ok, err := h.db.GetRestaurantServiceWindow(ctx, *restaurantID); err == nil && ok {
			win = &sw
		}
	}
	loc := time.Local
	if win != nil && win.TZ != "" {
		if l, err := time.LoadLocation(win.TZ); err == nil {
			loc = l
		}
	}
	startHour := time.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	var cells []hourCell
	svcHours, svcOnline := 0, 0
	for i := 0; i < 24; i++ {
		t := startHour.Add(time.Duration(i) * time.Hour).In(loc)
		pct := 0
		if total > 0 {
			pct = 100 * hourly[i] / total
		}
		inSvc := false
		if win != nil {
			m := t.Hour()*60 + t.Minute()
			inSvc = (win.OpenMin != win.CloseMin && ((win.OpenMin < win.CloseMin && m >= win.OpenMin && m < win.CloseMin) || (win.OpenMin > win.CloseMin && (m >= win.OpenMin || m < win.CloseMin)))) ||
				(win.NightOpenMin != win.NightCloseMin && ((win.NightOpenMin < win.NightCloseMin && m >= win.NightOpenMin && m < win.NightCloseMin) || (win.NightOpenMin > win.NightCloseMin && (m >= win.NightOpenMin || m < win.NightCloseMin))))
		}
		if inSvc && i < 23 {
			svcHours++
			svcOnline += pct
		}
		cells = append(cells, hourCell{Label: t.Format("3PM"), Pct: pct, Service: inSvc, Now: i == 23})
	}
	serviceUptime := -1
	if svcHours > 0 {
		serviceUptime = svcOnline / svcHours
	}
	serviceLabel := ""
	if win != nil && win.OpenMin != win.CloseMin {
		serviceLabel = fmtMin(win.OpenMin) + " – " + fmtMin(win.CloseMin)
	}

	// Week report card from daily stats: uptime and battery, this week vs last.
	type delta struct {
		Label, Value, Prev string
		Up, Better         bool
		Has                bool
	}
	var report []delta
	if restaurantID != nil {
		if stats, err := h.db.GetRestaurantDailyStats(ctx, *restaurantID, 14); err == nil && len(stats) > 0 {
			var upCur, upPrev, batCur, batPrev float64
			var nCur, nPrev int
			cut := time.Now().UTC().AddDate(0, 0, -7)
			for _, st := range stats {
				if st.OnlineMinAvg == nil {
					continue
				}
				if st.Day.After(cut) {
					upCur += float64(*st.OnlineMinAvg) / 14.4
					if st.BatteryAvg != nil {
						batCur += float64(*st.BatteryAvg)
					}
					nCur++
				} else {
					upPrev += float64(*st.OnlineMinAvg) / 14.4
					if st.BatteryAvg != nil {
						batPrev += float64(*st.BatteryAvg)
					}
					nPrev++
				}
			}
			if nCur > 0 {
				u := upCur / float64(nCur)
				d := delta{Label: "Uptime in service", Value: fmt.Sprintf("%.0f%%", u), Has: true}
				if nPrev > 0 {
					pu := upPrev / float64(nPrev)
					d.Prev = fmt.Sprintf("%.0f%%", pu)
					d.Up, d.Better = u >= pu, u >= pu
				}
				report = append(report, d)
				b := batCur / float64(nCur)
				d2 := delta{Label: "Battery at close", Value: fmt.Sprintf("%.0f%%", b), Has: true}
				if nPrev > 0 {
					pb := batPrev / float64(nPrev)
					d2.Prev = fmt.Sprintf("%.0f%%", pb)
					d2.Up, d2.Better = b >= pb, b >= pb
				}
				report = append(report, d2)
			}
		}
	}
	report = append(report, delta{Label: "Incidents", Value: strconv.Itoa(incCur), Prev: strconv.Itoa(incPrev), Up: incCur > incPrev, Better: incCur <= incPrev, Has: true})

	hour := time.Now().In(loc).Hour()
	greeting := "Good evening"
	if hour < 12 {
		greeting = "Good morning"
	} else if hour < 17 {
		greeting = "Good afternoon"
	}
	name := h.currentUsername(r)
	if u, err := h.db.GetUserByUsername(ctx, name); err == nil && u != nil && u.FirstName != "" {
		name = u.FirstName
	}
	sort.SliceStable(todos, func(i, j int) bool {
		rank := map[string]int{"bad": 0, "warn": 1, "info": 2}
		return rank[todos[i].Sev] < rank[todos[j].Sev]
	})
	// Headline: "Six of eight stations are ready for dinner." Lede: the exceptions
	// in one breath, then "everything else is fine".
	ready := 0
	groups := map[string][]string{}
	for _, v := range venues {
		for _, st := range v.Stations {
			if st.Word == "Ready" {
				ready++
				continue
			}
			groups[st.Word] = append(groups[st.Word], st.Name)
		}
	}
	// One sentence per kind of problem. Up to two names are spelled out, more
	// becomes a count — twenty serials in a paragraph is noise, not a brief.
	names := func(list []string) string {
		switch len(list) {
		case 1:
			return "<b>" + list[0] + "</b>"
		case 2:
			return "<b>" + list[0] + "</b> and <b>" + list[1] + "</b>"
		}
		return "<b>" + list[0] + "</b>, <b>" + list[1] + "</b> and " + strconv.Itoa(len(list)-2) + " more"
	}
	var exceptions []string
	if l := groups["Offline"]; len(l) > 0 {
		if len(l) == 1 {
			exceptions = append(exceptions, names(l)+" is offline")
		} else {
			exceptions = append(exceptions, strconv.Itoa(len(l))+" stations are offline: "+names(l))
		}
	}
	if l := groups["Not charging"]; len(l) > 0 {
		if len(l) == 1 {
			exceptions = append(exceptions, names(l)+" is off its dock and running low")
		} else {
			exceptions = append(exceptions, strconv.Itoa(len(l))+" stations are off their docks and running low: "+names(l))
		}
	}
	if l := groups["Pad misplaced"]; len(l) > 0 {
		exceptions = append(exceptions, names(l)+map[bool]string{true: " need", false: " needs"}[len(l) > 1]+" placing again on the charging pad")
	}
	meal := "service"
	switch {
	case hour < 11:
		meal = "the day"
	case hour < 15:
		meal = "lunch"
	case hour < 22:
		meal = "dinner"
	}
	headline := "No stations are set up yet."
	if total > 0 {
		if ready == total {
			headline = "All " + numWord(total) + " station" + map[bool]string{true: "s are", false: " is"}[total != 1] + " ready for " + meal + "."
		} else if ready == 0 {
			headline = "None of your " + numWord(total) + " station" + map[bool]string{true: "s are", false: " is"}[total != 1] + " ready for " + meal + "."
		} else {
			headline = strings.Title(numWord(ready)) + " of " + numWord(total) + " stations " + map[bool]string{true: "are", false: "is"}[ready != 1] + " ready for " + meal + "."
		}
	}
	lede := ""
	if len(exceptions) > 0 {
		lede = strings.Join(exceptions, ". ") + "."
		if ready > 0 {
			lede += " Everything else is charged and locked to the menu."
		}
	} else if total > 0 {
		lede = "Every station is charged, locked to the menu and online."
	}
	// "Needs you" needGroups like problems into one row each (7 stations offline,
	// 13 pads misplaced, …) instead of one row per station per problem; the
	// stations list below already carries the per-station detail.
	type needGroup struct {
		Sev, Kind, Title, Body string
		Stations               []todo
		More                   int // stations beyond the first few chips
	}
	var needGroups []needGroup
	{
		idx := map[string]int{}
		order := []string{"offline", "battery", "pad", "crashes", "kiosk"}
		for _, k := range order {
			idx[k] = -1
		}
		for _, t := range todos {
			i, ok := idx[t.Kind]
			if !ok {
				continue
			}
			if i == -1 {
				needGroups = append(needGroups, needGroup{Sev: t.Sev, Kind: t.Kind, Body: t.Body})
				i = len(needGroups) - 1
				idx[t.Kind] = i
			}
			needGroups[i].Stations = append(needGroups[i].Stations, t)
		}
		sort.SliceStable(needGroups, func(a, b int) bool {
			pos := func(k string) int {
				for i, o := range order {
					if o == k {
						return i
					}
				}
				return len(order)
			}
			return pos(needGroups[a].Kind) < pos(needGroups[b].Kind)
		})
		for i := range needGroups {
			n := len(needGroups[i].Stations)
			plural := map[bool]string{true: "s", false: ""}[n != 1]
			switch needGroups[i].Kind {
			case "offline":
				needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " offline"
				needGroups[i].Body = "Check that they are powered on and connected to Wi-Fi."
			case "battery":
				needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " low and not charging"
				needGroups[i].Body = "Put them back on their charging docks so they last through service."
			case "pad":
				needGroups[i].Title = strconv.Itoa(n) + " charging pad" + plural + " misplaced"
				needGroups[i].Body = "Place each device again on its pad. If it keeps happening, let the MDM team know."
			case "crashes":
				needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " crashing this week"
				needGroups[i].Body = "The MDM team can see the details; a restart usually helps in the meantime."
			case "kiosk":
				needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " not locked to the app"
				needGroups[i].Body = "Guests can leave the app. Ask the MDM team to turn kiosk mode back on."
			}
			if n > 6 {
				needGroups[i].More = n - 6
				needGroups[i].Stations = needGroups[i].Stations[:6]
			}
		}
	}
	h.render(w, r, "owner_home.html", map[string]any{
		"TodoGroups":    needGroups,
		"TodoCount":     len(todos),
		"Headline":      headline,
		"Lede":          template.HTML(lede),
		"Title":         "Home",
		"Greeting":      greeting,
		"Name":          name,
		"DateLine":      time.Now().In(loc).Format("Monday 2 January"),
		"TimeLine":      strings.ToLower(time.Now().In(loc).Format("3:04 pm")),
		"Venues":        venues,
		"Total":         total,
		"Online":        online,
		"Offline":       total - online,
		"LowBatt":       lowBatt,
		"Score":         score,
		"ScoreClass":    scoreClass,
		"ScoreWord":     scoreWord,
		"Todos":         todos,
		"Hours":         cells,
		"ServiceUptime": serviceUptime,
		"ServiceLabel":  serviceLabel,
		"Report":        report,
		"VenueCount":    len(venues),
	})
}

// batteryTempStr renders battery_temp_c from extra as "39.2°C" or "".
func batteryTempStr(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	v, ok := m["battery_temp_c"]
	if !ok {
		return ""
	}
	var f float64
	if json.Unmarshal(v, &f) != nil {
		return ""
	}
	return fmt.Sprintf("%.1f°C", f)
}

// numWord spells small counts the way a sentence would ("six of eight").
func numWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

func fmtMin(m int) string {
	t := time.Date(2000, 1, 1, m/60, m%60, 0, 0, time.UTC)
	return strings.ToLower(t.Format("3:04pm"))
}

func timeSinceStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// deliveryNicknames maps the devices of a command's deliveries to their nicknames.
func (h *Handler) deliveryNicknames(ctx context.Context, ds []db.CommandDelivery) map[uuid.UUID]string {
	ids := make([]uuid.UUID, 0, len(ds))
	for _, d := range ds {
		ids = append(ids, d.DeviceID)
	}
	m, err := h.db.GetNicknames(ctx, ids)
	if err != nil || m == nil {
		return map[uuid.UUID]string{}
	}
	return m
}

// DeviceSetNickname stores a friendly name for a device ("Table 4"), shown to
// restaurant owners and next to the serial on the device page.
func (h *Handler) DeviceSetNickname(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "notes", device.ID) {
		return
	}
	name := strings.TrimSpace(r.FormValue("nickname"))
	if len(name) > 40 {
		name = name[:40]
	}
	if err := h.db.SetNickname(r.Context(), device.ID, name); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.nickname", device.SerialNumber, name)
	h.hxRedirect(w, r, "/devices/"+device.SerialNumber)
}

// MaintenancePage is the notice non-admin users see while maintenance mode is on.
// Outside maintenance (or for an admin) it just goes home.
// iconStore holds decoded app icons by content hash for /icon/{sha}.png. Pages
// used to inline every icon as a base64 data: URI — ~120 KB of uncompressible
// bytes per device page, re-sent on every load. Registering the bytes at render
// time and referencing them by hash lets the browser cache each icon once
// (immutable, keyed by content). The store is in memory: after a restart a
// browser without a cached copy 404s until the next page render re-registers
// the icon, which is the same request that shows it. Bounded; icons are few.
var (
	iconStore   sync.Map // sha (hex) -> []byte
	iconStoreN  atomic.Int32
	iconStoreMx = int32(2000)
)

func iconSrc(b64 string) string {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return ""
	}
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "data:image/png;base64," + b64 // unknown shape: fall back to inline
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:16])
	if _, loaded := iconStore.LoadOrStore(key, raw); !loaded {
		if iconStoreN.Add(1) > iconStoreMx { // crude bound: start over rather than grow forever
			iconStore.Range(func(k, _ any) bool { iconStore.Delete(k); return true })
			iconStoreN.Store(1)
			iconStore.Store(key, raw)
		}
	}
	return "/icon/" + key
}

// IconPNG serves an icon registered by iconSrc. Immutable cache: the URL is the
// content hash, so a changed icon is a different URL.
func (h *Handler) IconPNG(w http.ResponseWriter, r *http.Request) {
	v, ok := iconStore.Load(strings.TrimSuffix(r.PathValue("sha"), ".png"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	w.Write(v.([]byte))
}

func (h *Handler) MaintenancePage(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.MaintenanceMode() || h.role(r) == "admin" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "600")
	w.WriteHeader(http.StatusServiceUnavailable)
	h.render(w, r, "maintenance.html", map[string]any{"Title": "Under maintenance"})
}

// DeviceAlertsPanel renders the device page's Alerts tab body — this device's
// crash/ANR events (with traces) plus its active alerts — on first open of the
// tab. Kept out of the page render because a crashy device carries megabytes
// of traces here that most visits never look at.
func (h *Handler) DeviceAlertsPanel(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	crashes := toCrashCards(mustCrashes(h.db.ListDeviceCrashes(ctx, device.ID, 50)))
	raw, _ := h.db.ListDeviceActiveAlerts(ctx, device.ID, 50)
	role := h.role(r)
	canAct := roleCanOperate(role)
	var alerts []humanAlert
	for _, a := range raw {
		ha := humanizeAlert(a)
		ha.CanAct = canAct
		alerts = append(alerts, ha)
	}
	h.resolveAppIcons(ctx, [][]humanAlert{alerts}, crashes)
	h.render(w, r, "device-alerts-panel", map[string]any{
		"Device":        device,
		"DeviceCrashes": crashes,
		"DeviceAlerts":  alerts,
	})
}

// ProfilePage shows the signed-in user their own account details and footprint:
// actions, commands, devices touched, QA and release work, recent activity.
func (h *Handler) ProfilePage(w http.ResponseWriter, r *http.Request) {
	h.renderProfile(w, r, h.currentUsername(r), false, false)
}

// UserProfilePage is an admin's view of another user's profile (Users → Stats).
func (h *Handler) UserProfilePage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if u.Username == h.currentUsername(r) {
		h.renderProfile(w, r, u.Username, false, false)
		return
	}
	// Anyone signed in may look at a colleague's profile (stats, recent activity);
	// the management and access cards appear only for those who may manage them.
	canManage := roleManagesUsers(h.role(r)) && mayManageUser(h.role(r), u.Role, "")
	h.renderProfile(w, r, u.Username, true, canManage)
}

func (h *Handler) renderProfile(w http.ResponseWriter, r *http.Request, username string, viewingOther, canManage bool) {
	ctx := r.Context()
	stats, err := h.db.UserStats(ctx, username, h.user)
	if err != nil {
		log.Printf("[profile] stats for %q: %v", username, err)
		stats = &db.UserStats{Username: username}
	}
	user, _ := h.db.GetUserByUsername(ctx, username) // nil for the env dashboard login
	var recent []db.AuditEntry
	if !viewingOther || h.role(r) == "admin" {
		recent, _ = h.db.ListAuditFiltered(ctx, username, 12)
	}
	display := username
	if user != nil {
		display = user.DisplayName()
	}
	title := "My profile"
	if viewingOther {
		title = display
	}
	// A restaurant owner's own profile is just their account and their venues;
	// none of the operator statistics apply to them.
	var ownerVenues []string
	if !viewingOther && h.role(r) == "owner" {
		if pol, err := h.db.GetUserAccess(ctx, username); err == nil {
			rs, _ := h.db.ListRestaurants(ctx)
			byID := map[string]string{}
			for _, x := range rs {
				byID[x.ID.String()] = x.Name
			}
			for _, g := range pol.Grants {
				if g.Effect == "allow" && g.ScopeType == "restaurant" && g.ScopeID != nil {
					if n, ok := byID[g.ScopeID.String()]; ok {
						ownerVenues = append(ownerVenues, n)
					}
				}
			}
		}
	}
	var pol db.AccessPolicy
	if viewingOther && canManage && user != nil {
		pol, _ = h.db.GetUserAccess(ctx, user.Username)
	}
	h.render(w, r, "profile.html", map[string]any{
		"Policy":        pol,
		"PolicyEmpty":   pol.IsEmpty(),
		"PolicySummary": h.policySummary(ctx, user, pol),
		"Title":        title,
		"User":         user,
		"Bubble":       userBubbleFn(username),
		"CanEditAvatar": user != nil && h.mayEditAvatar(r, user),
		"Display":      display,
		"Username":     username,
		"Stats":        stats,
		"Recent":       recent,
		"ViewingOther": viewingOther,
		"CanManage":    canManage,
		"CanSeeActivity": !viewingOther || h.role(r) == "admin", // only a super admin reads someone else's activity
		"OwnerSelf":    !viewingOther && h.role(r) == "owner",
		"OwnerVenues":  ownerVenues,
		"UsersTab":     map[bool]string{true: "access", false: ""}[canManage],
		"Assignable":   assignableRoles(h.role(r)),
	})
}

// UserMergeActor re-points past activity recorded under a username that no longer
// has an account (deleted after e.g. a Microsoft sign-in replaced it) to an
// existing user, so Activity, Actions history and release sign-offs show one
// person. Only orphaned usernames can be merged: merging a live account would
// silently rewrite history for someone who still signs in.
func (h *Handler) UserMergeActor(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.FormValue("from"))
	toID, err := uuid.Parse(strings.TrimSpace(r.FormValue("to")))
	if from == "" || err != nil {
		http.Error(w, "Pick a username and a target user", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), toID)
	if err != nil {
		http.Error(w, "Target user not found", http.StatusNotFound)
		return
	}
	if u, err := h.db.GetUserByUsername(r.Context(), from); (err == nil && u != nil) || from == h.user {
		http.Error(w, "That username still has an account; delete it first if you really mean to merge it", http.StatusConflict)
		return
	}
	n, err := h.db.MergeActor(r.Context(), from, target.Username)
	if err != nil {
		log.Printf("[users] merge %q -> %q: %v", from, target.Username, err)
		http.Error(w, "Merge failed", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.merge", target.Username, fmt.Sprintf("%s → %s (%d rows)", from, target.Username, n))
	h.hxRedirect(w, r, "/users")
}

func (h *Handler) SettingsToggleMaintenance(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetMaintenanceMode(!h.cfg.MaintenanceMode())
	h.audit(r, "settings.maintenance", "", fmt.Sprintf("%t", h.cfg.MaintenanceMode()))
	h.settingsToggleResponse(w, r, "/settings/maintenance", h.cfg.MaintenanceMode())
}

// SettingsLegacyStripDone marks the background legacy check-in cleanup complete —
// used after the history was rebuilt in one go (tools/rebuild-checkins.sql), which
// makes the hourly job redundant.
func (h *Handler) SettingsLegacyStripDone(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg.SetLegacyStripCursor("done")
	h.audit(r, "settings.legacy_strip_done", "", "")
	h.hxRedirect(w, r, h.settingsDest(r))
}

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isLoggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireAdmin guards operational admin routes (releases, OTA, productions,
// restaurants, devices, commands, …). Both "admin" and "dev" pass: a dev has
// full operational power and differs from admin only in being barred from
// settings and user management — those use requireStrictAdmin instead.
// requireReleaseAdmin guards the release / OTA / deployment / QA-catalog routes:
// admin or dev. Dev is exactly "operator + releases + OTA"; everything else
// admin-only stays on requireAdmin.
func (h *Handler) requireReleaseAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "admin" && s.Role != "dev" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// An anonymous caller is redirected to login like any other protected
		// route, so admin-only paths don't stand out with a 403 (F-07). A
		// logged-in but non-admin user gets a genuine 403.
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireAdminOrOperator extends requireAdmin (admin/dev) to also allow operators, for
// the fleet-organisation tasks the test team owns: creating groups and restaurants
// and setting kiosk mode. Viewers and operators still can't reach these.
func (h *Handler) requireAdminOrOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleCanOperate(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireStrictAdmin guards the two areas a dev must never reach: settings and
// user management. Only the env-configured "admin" passes.
// requireUserManager guards the Users pages: admin or user_manager. Per-target
// elevation checks (who may edit whom) live in the handlers via mayManageUser.
// requireOTA guards firmware pushes: admin, dev, or the super op (who may deploy
// releases but not manage the release packages themselves).
// roleAboveOperator is true for the roles ranked above operator in roleLevels:
// access admin, super op, dev and admin.
func roleAboveOperator(role string) bool {
	return roleLevels[role] > roleLevels["operator"]
}

// roleCanAppLibrary: the app library (browse, upload, register) is open to the
// roles that push software: admin, dev and super op. Removing an APK is admin-only.
func roleCanAppLibrary(role string) bool {
	return role == "admin" || role == "dev" || role == "super_op"
}

// requireAppLibrary guards the app library page and its upload endpoints.
func (h *Handler) requireAppLibrary(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleCanAppLibrary(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// AppLibraryPage renders /apps: the library with upload, for admin, dev and super op.
func (h *Handler) AppLibraryPage(w http.ResponseWriter, r *http.Request) {
	families, suggestions, mode := h.libraryData(r.Context())
	apps, _ := h.db.ListApps(r.Context())
	h.render(w, r, "apps.html", map[string]any{
		"Title":         "App library",
		"ActivePage":    "commands",
		"Apps":          apps,
		"Families":      families,
		"Suggestions":   suggestions,
		"AppFamilyMode": mode,
	})
}

func (h *Handler) requireOTA(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleCanOTA(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireAccountAdmin guards account creation / deletion / merge: admin or access
// admin. A super op may open the Users pages and edit rules but not add accounts.
func (h *Handler) requireAccountAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleCreatesUsers(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (h *Handler) requireUserManager(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleManagesUsers(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireStrictAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireDev gates the release sign-off to the "dev" role only — the sign-off
// asserts the developers tested the build at their end, so only a dev may set it.
func (h *Handler) requireDev(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "dev" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireOperatorOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleCanOperate(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireOperatorRole gates QA result-marking to the operator role only. Managing test
// cases stays admin-only; recording pass/fail is the operator's job and nobody
// else's (viewer/operator/admin all see results read-only).
func (h *Handler) requireOperatorRole(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !roleIsOperatorLike(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// enforceSameOrigin wraps a state-changing dashboard handler and rejects
// requests that originate cross-site, mitigating CSRF (GB-03). Dashboard auth
// is a cookie the browser attaches automatically, so a malicious page could
// otherwise drive any POST (e.g. issuing a device command or logging the user
// out). The device/admin JSON API under /api/v1/* is authenticated by an
// X-API-Key header rather than an ambient cookie, so it is not CSRF-able and is
// intentionally not wrapped.
//
// The check uses two independent signals and blocks only when one positively
// indicates a cross-origin request, so legacy clients that send neither header
// still work:
//   - Sec-Fetch-Site (fetch metadata): must be same-origin, same-site, or none
//     when present. same-site is accepted because the deployment is reachable
//     over more than one subdomain of the same site; a real cross-subdomain
//     forgery is still caught by the Origin check below, which requires the
//     Origin host to match publicOrigin (or the request Host). Only a positive
//     cross-site signal is rejected here.
//   - Origin: must match publicOrigin (or the request Host) when present.
func (h *Handler) enforceSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "same-site", "none":
			// allowed, or header absent — fall through to the Origin check
		default:
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !h.originAllowed(origin, r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// originAllowed reports whether an Origin header value is this site. When
// publicOrigin is configured it must match exactly; otherwise the Origin's host
// must equal the request Host (works behind a TLS-terminating proxy).
func (h *Handler) originAllowed(origin string, r *http.Request) bool {
	if len(h.publicOrigins) > 0 {
		for _, o := range h.publicOrigins {
			if origin == o {
				return true
			}
		}
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// Root serves the public landing page to visitors without a session and the
// Overview dashboard to everyone else. /login stays reachable directly.
func (h *Handler) Root(w http.ResponseWriter, r *http.Request) {
	if !h.isLoggedIn(r) {
		h.Landing(w, r)
		return
	}
	h.requireAuth(h.Overview)(w, r)
}

// userBubbleFn is the template resolver for avatar bubbles, set when the template
// func map is built; handlers use it for the layout chip and profile hero.
var userBubbleFn func(name string) any

// userIsAdminFn reports whether a command author (username or display name) is an
// admin/dev account; set alongside userBubbleFn.
var userIsAdminFn func(name string) bool

// ---------------------------------------------------------------- profile pictures

// avatarSize is the stored edge length; bubbles render at 18-64px so this is plenty.
const avatarSize = 128

// UserAvatar serves the stored PNG. Versioned URLs (?v=) make it safe to cache hard.
func (h *Handler) UserAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	png, ver, err := h.db.GetUserAvatar(r.Context(), id)
	if err != nil || len(png) == 0 {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"av-%d"`, ver)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.Write(png)
}

// mayEditAvatar: your own picture, or any user's if you are an admin.
func (h *Handler) mayEditAvatar(r *http.Request, target *db.User) bool {
	if strings.EqualFold(h.currentUsername(r), target.Username) {
		return true
	}
	role := h.role(r)
	return role == "admin" || role == "dev"
}

// UserSetAvatar accepts an image upload (multipart field "avatar"), squares and
// downsizes it to avatarSize and stores it as PNG.
func (h *Handler) UserSetAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.mayEditAvatar(r, target) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Image too large (8 MB max).", http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("avatar")
	if err != nil {
		http.Error(w, "Choose an image first.", http.StatusBadRequest)
		return
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		http.Error(w, "That file is not a PNG, JPEG or GIF image.", http.StatusBadRequest)
		return
	}
	out := squareThumb(src, avatarSize)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		http.Error(w, "Could not encode image.", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserAvatar(r.Context(), id, buf.Bytes()); err != nil {
		http.Error(w, "Could not save picture.", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.avatar.set", target.Username, "")
	if to := r.FormValue("redirect"); strings.HasPrefix(to, "/users/") && !strings.Contains(to, "//") {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/users/"+id.String()+"/profile", http.StatusSeeOther)
}

// UserClearAvatar removes the picture (back to the initial bubble).
func (h *Handler) UserClearAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.mayEditAvatar(r, target) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	_ = h.db.ClearUserAvatar(r.Context(), id)
	h.audit(r, "user.avatar.clear", target.Username, "")
	if to := r.FormValue("redirect"); strings.HasPrefix(to, "/users/") && !strings.Contains(to, "//") {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/users/"+id.String()+"/profile", http.StatusSeeOther)
}

// squareThumb center-crops src to a square and scales it to size×size with a
// box filter (average of the covered source pixels), which is smooth enough for a
// small avatar and needs no extra dependency.
func squareThumb(src image.Image, size int) *image.NRGBA {
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for dy := 0; dy < size; dy++ {
		sy0 := y0 + dy*side/size
		sy1 := y0 + (dy+1)*side/size
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < size; dx++ {
			sx0 := x0 + dx*side/size
			sx1 := x0 + (dx+1)*side/size
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, as, n uint64
			for y := sy0; y < sy1; y++ {
				for x := sx0; x < sx1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA()
					rs += uint64(cr >> 8)
					gs += uint64(cg >> 8)
					bs += uint64(cb >> 8)
					as += uint64(ca >> 8)
					n++
				}
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = uint8(rs / n)
			dst.Pix[i+1] = uint8(gs / n)
			dst.Pix[i+2] = uint8(bs / n)
			dst.Pix[i+3] = uint8(as / n)
		}
	}
	return dst
}

// settingsRedirect sends a settings form back to the tab it came from: the page
// injects redirect=/settings#<tab> into every settings form. Requests made with
// fetch (X-Requested-With: fetch) get 204 instead so the page can stay put.
func (h *Handler) settingsRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, h.settingsDest(r), http.StatusFound)
}

// settingsDest is the settings URL (with #tab) a form asked to return to.
func (h *Handler) settingsDest(r *http.Request) string {
	dest := strings.TrimSpace(r.FormValue("redirect"))
	if !strings.HasPrefix(dest, "/settings") || strings.HasPrefix(dest, "//") {
		dest = "/settings"
	}
	return dest
}

// Landing renders the entry page for staff: what the MDM does, guides, FAQ, the
// training videos (streamed from S3 via short-lived presigned URLs), sign in, sign up.
func (h *Handler) Landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.tmpl.ExecuteTemplate(w, "landing.html", map[string]any{
		"Brand":         h.cfg.CustomBrand(),
		"AssetVer":      h.assetVer,
		"SignupEnabled": true,
		"Training":      h.trainingVideos(r.Context()),
	})
}

// SneakPeek renders a public, no-account preview of the dashboard: the REAL
// Overview template and chrome, rendered against synthetic fleet data instead of
// the database. Reachable from the landing page's "Take a sneak peek" button.
//
// It reuses overviewViewModel — the exact code the live Overview uses — so the
// preview is the real page, not a mock. A synthetic viewer session is injected for
// the duration of this one request so the role-gated chrome renders; no cookie,
// no DB session, and none of the numbers come from real devices. Viewer (not admin)
// so the preview only offers what it can actually show — the write-action controls
// (Send action, Deploy, Actions dock) are role-locked/hidden rather than links that
// bounce a visitor to /login.
func (h *Handler) SneakPeek(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts := sneakPeekFleet()
	activeSecs := h.cfg.CheckinInterval() * 3
	data := h.overviewViewModel(r, summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts, activeSecs, nil, 0)

	// Per-user widget layout (default arrangement for the unknown preview user).
	for k, v := range h.overviewLayoutData(r) {
		data[k] = v
	}
	// Synthetic map: a handful of located devices clustered around a few venues.
	// Empty embed key makes the map widget render its preview branch (a stylised map).
	data["DeviceLocations"] = sneakPeekMapJSON()
	data["DeviceMapCount"] = 10
	data["MapsEmbedKey"] = ""

	h.sneakPeekChrome(data, "overview", openCount)
	h.sneakPeekRender(w, "overview.html", data)
}

// sneakPeekAuth injects a synthetic viewer session for the life of one preview
// request, so the role-gated chrome (dock, name, access) renders without a real
// cookie/DB session. Viewer, so write actions are hidden/role-locked.
func sneakPeekAuth(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, &db.Session{
		Username: "guest", Role: "viewer",
		ExpiresAt: time.Now().Add(time.Hour), LastSeen: time.Now(),
	}))
}

// sneakPeekChrome fills the layout chrome keys a preview page needs when rendered
// directly (not via h.render, so nothing touches the DB or writes an audit row).
func (h *Handler) sneakPeekChrome(data map[string]any, active string, openAlerts int) {
	data["Role"] = "viewer"
	data["Boosted"] = false
	data["CurrentUser"] = "Guest preview"
	if userBubbleFn != nil {
		data["CurrentBubble"] = userBubbleFn("Guest")
	}
	data["Brand"] = h.cfg.CustomBrand()
	data["Use24Hour"] = h.cfg.Use24Hour()
	data["Version"] = version.Current()
	data["AssetVer"] = h.assetVer
	data["ActivePage"] = active
	data["ShowTour"] = false
	data["AlertsOpenCount"] = openAlerts
	data["Preview"] = true
}

// sneakPeekRender writes a preview page from the real template + synthetic data.
func (h *Handler) sneakPeekRender(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("sneak-peek render %s: %v", name, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

// SneakPeekAlerts is the preview's dummy Alerts inbox: the real alerts.html rendered
// against synthetic alerts, so the dock's Alerts item is navigable in the preview.
func (h *Handler) SneakPeekAlerts(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	summary, crit, watch, crashes := sneakPeekAlertData()
	data := map[string]any{
		"Title":         "Alerts",
		"Summary":       summary,
		"View":          "",
		"Critical":      groupAlerts(crit),
		"Watching":      groupAlerts(watch),
		"NeedsCount":    len(crit),
		"WatchCount":    len(watch),
		"ActiveCount":   len(crit) + len(watch),
		"Crashes":       crashes,
		"CrashCount":    len(crashes),
		"DeviceFilter":  "",
		"CrashPage":     1,
		"CrashPages":    1,
		"CrashPageBase": "/sneak-peek/alerts?view=crashes",
	}
	h.sneakPeekChrome(data, "alerts", len(crit)+len(watch))
	h.sneakPeekRender(w, "alerts.html", data)
}

// SneakPeekFleet is the preview's dummy Fleet roster: the real devices.html rendered
// against a synthetic device list, so the dock's Fleet item is navigable in the preview.
func (h *Handler) SneakPeekFleet(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	devices, online := sneakPeekDevices()
	summary := db.Summary{Total: 2622, RecentlyActive: 2611, LowBattery: 38, UniqueBuilds: 4, KioskCount: 2202}
	data := map[string]any{
		"Title": "Devices", "Devices": devices, "Online": online,
		"Nicknames": map[uuid.UUID]string{}, "Flapping": map[uuid.UUID]bool{},
		"Total": 2622, "FleetTotal": 2622, "FilterCount": 0, "InactiveCount": 0,
		"RailGroups": []any{}, "RailRestaurants": []any{}, "RailReleases": []any{}, "RailProducts": []any{},
		"Groups": []any{}, "Restaurants": []any{}, "Productions": []any{}, "Builds": []any{}, "Timezones": []any{},
		"Products": h.fleetProductFilters(r.Context(), "viewer"), "Classes": product.Classes(),
		"SelectedCollection": "All devices", "SelectedCount": 2622,
		"ActiveRestaurant": nil, "ActiveGroup": nil, "ActiveReleaseID": 0,
		"View": "", "ViewID": "", "ViewColl": "",
		"Page": 1, "TotalPages": 1, "Query": "", "PageSize": 60,
		"Summary": summary, "Sort": "", "SortDir": "",
		"FilterRestaurant": "", "FilterGroup": "", "FilterProduction": "", "FilterProduct": "",
		"FilterStatus": "", "FilterBuild": "", "FilterBattery": "", "FilterKiosk": "",
		"FilterCharging": "", "FilterTimezone": "", "FilterKind": "", "FilterClass": "",
		"FilterOnboarding": "", "FilterLifecycle": "", "FilterHidden": false,
		"ActiveThresholdSecs": 180, "ActiveThresholdLabel": "3 min",
		"Density": h.cfg.Density(), "MapsEmbedKey": "",
	}
	h.sneakPeekChrome(data, "devices", 11)
	h.sneakPeekRender(w, "devices.html", data)
}

// sneakPeekDevices builds ~30 synthetic devices spread across the preview's venues,
// plus the online map devices.html reads for the live status dot.
func sneakPeekDevices() ([]db.Device, map[uuid.UUID]bool) {
	type spec struct {
		rest, prod, build string
		bat, temp         int
		kiosk, online     bool
	}
	names := []string{"Harbor Grill", "Harbor Grill", "Harbor Kitchen", "Sausalito", "Sausalito",
		"Marina Point", "Marina Point", "Ferry Plaza", "Embarcadero", "North Beach",
		"Presidio", "Richmond", "SoMa", "Castro", "Dogpatch", "Glen Park",
		"Mission Rock", "Noe Valley", "Hayes Valley", "Bayview", "Potrero", "Chinatown",
		"Union Square", "Financial", "Bernal", "Cole Valley", "Nob Hill", "Russian Hill",
		"Sunset", "Harbor Café"}
	prods := []string{"kiosk22", "kiosk27", "t7", "kiosk18"}
	devices := make([]db.Device, 0, len(names))
	online := map[uuid.UUID]bool{}
	now := time.Now()
	for i, name := range names {
		id := uuid.New()
		prod := prods[i%len(prods)]
		build := "2026.06.01-release"
		if i%7 == 3 {
			build = "2026.05.14-release"
		}
		bat := 60 + (i*11)%40 // 60..99
		temp := 30 + (i*3)%14 // 30..43
		isOn := i%9 != 4       // most online
		if i == 0 {
			temp = 61 // the hot one
		}
		seen := now.Add(-time.Duration(2+i%9) * time.Minute)
		if !isOn {
			seen = now.Add(-time.Duration(3+i%6) * time.Hour)
		}
		hasBat := prod == "t7"
		extra := fmt.Sprintf(`{"model":"AIO-%s","manufacturer":"AIO","temp_c":%d,"charging":%t}`,
			strings.ToUpper(prod), temp, hasBat && bat < 90)
		devices = append(devices, db.Device{
			ID: id, SerialNumber: fmt.Sprintf("%s-%04d", map[bool]string{true: "TAB", false: "KSK"}[prod == "t7"], 1000+i),
			BuildID: build, BatteryPct: bat, LastSeenAt: seen, KioskEnabled: true,
			Product: prod, RestaurantName: name, DeployedEffective: true,
			AgentKind: "firmware", EnrollmentStatus: "auto", EnrolledAt: now.Add(-720 * time.Hour),
			LatestExtra: json.RawMessage(extra),
		})
		online[id] = isOn
	}
	return devices, online
}

// sneakPeekAlertData builds the synthetic alerts inbox for the preview, consistent
// with the fleet sneakPeekFleet paints (Harbor Grill hot, Sausalito offline, …).
func sneakPeekAlertData() (db.AlertSummary, []humanAlert, []humanAlert, []crashCardView) {
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }
	ha := func(icon, sev, headline, sentence, serial, rest string, mins, occ int) humanAlert {
		return humanAlert{
			ID: uuid.New(), Severity: sev, Status: "open", IconKey: icon,
			Headline: headline, Sentence: template.HTML(sentence), Serial: serial,
			Restaurant: rest, FiredAt: ago(mins), Occurrences: occ,
		}
	}
	crit := []humanAlert{
		ha("heat", "critical", "Device running hot", "Reached <b>61°C</b>, above the 60°C ceiling — throttling likely.", "KSK-1000", "Harbor Grill", 12, 4),
		ha("crash", "critical", "App crashing repeatedly", "<b>aio.app.pos</b> crashed 6 times in the last hour.", "KSK-1042", "Harbor Kitchen", 35, 6),
		ha("storage", "critical", "Storage almost full", "Only <b>2%</b> free on the data partition.", "KSK-1103", "Harbor Café", 64, 1),
	}
	watch := []humanAlert{
		ha("offline", "warning", "Device offline", "Stopped checking in <b>4h</b> ago — last seen 04:12.", "KSK-2117", "Sausalito", 240, 1),
		ha("battery", "warning", "Battery below 20%", "Discharged to <b>17%</b> and not on a charging pad.", "TAB-3088", "Marina Point", 26, 1),
		ha("charge", "warning", "Left off the charging pad", "Off power overnight; started the day at 41%.", "TAB-3091", "Ferry Plaza", 190, 1),
		ha("wifi", "warning", "Weak Wi-Fi signal", "Averaging <b>-78 dBm</b>; check the access point placement.", "KSK-2210", "Embarcadero", 88, 3),
		ha("memory", "warning", "High memory pressure", "Available memory under <b>8%</b> for 20 minutes.", "KSK-2255", "North Beach", 52, 2),
		ha("offline", "warning", "Device offline", "Stopped checking in <b>1h</b> ago.", "KSK-2260", "Presidio", 61, 1),
		ha("battery", "warning", "Battery draining fast", "Down 30% in an hour under load.", "TAB-3120", "Richmond", 44, 1),
		ha("generic", "warning", "Clock drift detected", "Device clock is <b>90s</b> behind the server.", "KSK-2301", "SoMa", 120, 1),
	}
	crash := func(kind, cls, serial, rest, pkg, summary string, mins int) crashCardView {
		return crashCardView{
			Serial: serial, Restaurant: rest, KindLabel: kind, KindClass: cls,
			BuildID: "2026.05.14-release", OccurredAt: ago(mins), Summary: summary, PackageName: pkg,
			Trace: "java.lang.RuntimeException: " + summary + "\n\tat " + pkg + ".MainActivity.onCreate(MainActivity.java:142)\n\tat android.app.Activity.performCreate(Activity.java:8000)",
		}
	}
	crashes := []crashCardView{
		crash("Crash", "crash", "KSK-1042", "Harbor Kitchen", "aio.app.pos", "aio.app.pos — NullPointerException in checkout flow", 35),
		crash("ANR", "anr", "KSK-1000", "Harbor Grill", "aio.app.kiosk", "aio.app.kiosk — Input dispatching timed out (main thread blocked)", 70),
		crash("Crash", "crash", "TAB-3088", "Marina Point", "aio.app.pay", "aio.app.pay — IllegalStateException reading card reader", 150),
	}
	summary := db.AlertSummary{Total: 361, Open: 11, Acknowledged: 2, Resolved: 348, Critical: 3, Warning: 8, Info: 0}
	return summary, crit, watch, crashes
}

// intPtr / f64Ptr / f32Ptr / strPtr are small helpers for the synthetic-data
// builder below (the db structs use pointer fields for "may be absent" metrics).
func intPtr(v int) *int         { return &v }
func f64Ptr(v float64) *float64 { return &v }
func f32Ptr(v float32) *float32 { return &v }
func strPtr(v string) *string   { return &v }

// sneakPeekVenueID is a stable synthetic restaurant UUID for venue i, so the crash
// map and the health list line up across the preview.
func sneakPeekVenueID(i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-a000-%012d", i+1))
}

// sneakPeekFleet builds the synthetic fleet the preview renders: a chain-scale fleet
// of 2500+ devices across 72 venues. Made up but internally consistent — every KPI,
// the health score, the sites heatmap and the rollouts are derived from the same
// per-venue numbers, so nothing contradicts anything else.
func sneakPeekFleet() (db.Summary, []db.GroupHealth, int, []db.FleetDailyStat, int, db.FleetCrashStats, []db.FleetVersion, []db.Update, map[string]int) {
	districts := []string{
		"Harbor", "Marina", "Ferry Plaza", "Embarcadero", "North Beach", "Mission Rock",
		"Presidio", "Sunset", "Richmond", "Castro", "Hayes Valley", "Nob Hill",
		"Russian Hill", "Potrero", "Dogpatch", "SoMa", "Union Square", "Financial",
		"Chinatown", "Glen Park", "Noe Valley", "Bernal", "Cole Valley", "Bayview",
	}
	suffixes := []string{"Grill", "Kitchen", "Café"}
	const venueCount = 72

	groups := make([]db.GroupHealth, 0, venueCount)
	total, online, openCount := 0, 0, 0
	crashBy := map[uuid.UUID]int{}
	for i := 0; i < venueCount; i++ {
		id := sneakPeekVenueID(i)
		dc := 22 + (i*13)%30 // 22..51 devices per venue
		g := db.GroupHealth{
			GroupID: id, Name: districts[i/3] + " " + suffixes[i%3],
			DeviceCount: dc, DeployedCount: dc, Deployed: true, DistinctBuilds: 1,
			BatteryAvg: f64Ptr(float64(74 + i%20)), ChargingAvg: f64Ptr(0.72 + float64(i%18)/100),
		}
		switch {
		case i < 3: // three venues at risk (worst first drives the hero + "why")
			g.ScoreClass, g.Score = "danger", []int{57, 62, 66}[i]
			g.OfflineCount, g.OpenCritical = []int{3, 2, 2}[i], 1
			openCount++
			crashBy[id] = []int{4, 2, 1}[i]
			if i < 2 { // the two hottest venues
				g.TempMax = f64Ptr([]float64{61, 51}[i])
				g.TempMaxSerial = strPtr(fmt.Sprintf("KSK-%04d", 1000+i))
			}
		case i < 11: // eight venues to watch
			g.ScoreClass, g.Score = "warn", 71+(i-3)
			g.OpenWarning = 1
			openCount++
			if i%2 == 0 {
				g.OfflineCount = 1
			}
			if i%3 == 0 {
				g.DistinctBuilds = 2
			}
		default: // healthy
			g.ScoreClass = "ok"
			if g.Score = 84 + (i*7)%15; g.Score > 99 {
				g.Score = 99
			}
		}
		total += dc
		online += dc - g.OfflineCount
		groups = append(groups, g)
	}

	hot := 6
	summary := db.Summary{
		Total: total, RecentlyActive: online, LowBattery: 38, UniqueBuilds: 4,
		KioskCount: total * 84 / 100,
	}

	// 14 days of fleet daily stats (oldest first) for the sparklines + week-over-week
	// trend. Deterministic gentle wave around the current online count.
	dip := []int{40, 18, 26, 10, 22, 34, 30, 14, 8, 20, 12, 24, 16, 6}
	batWave := []float32{77, 78, 78, 79, 78, 77, 78, 79, 80, 78, 79, 79, 78, 78}
	today := time.Now().Truncate(24 * time.Hour)
	d14 := make([]db.FleetDailyStat, 0, 14)
	for i := 0; i < 14; i++ {
		active := online - dip[i]
		d14 = append(d14, db.FleetDailyStat{
			Day: today.AddDate(0, 0, i-13), Active: active, LowBattery: 30 + i%12, Hot: 4 + i%4,
			BatteryAvg: f32Ptr(batWave[i]), Checkins: int64(active) * 288,
		})
	}

	crashStats := db.FleetCrashStats{
		Total24h: 34, Devices24h: 29, WorstBuild: "2026.05.14-release", WorstBuildN: 11,
		Daily:        []int{88, 72, 64, 79, 58, 66, 54},
		ByRestaurant: crashBy,
	}

	// Release adoption + rollouts, distributed across the fleet total.
	vLatest := total * 63 / 100
	v2 := total * 22 / 100
	v3 := total * 10 / 100
	v4 := total - vLatest - v2 - v3
	versions := []db.FleetVersion{
		{Version: "2026.06.01-release", Product: "kiosk22", DeviceCount: vLatest, ReleaseID: intPtr(90000), ReleaseStatus: "published"},
		{Version: "2026.05.14-release", Product: "kiosk27", DeviceCount: v2, ReleaseID: intPtr(89000), ReleaseStatus: "published"},
		{Version: "2026.04.02-release", Product: "t7", DeviceCount: v3, ReleaseID: intPtr(88000), ReleaseStatus: "published"},
		{Version: "2026.03.10-release", Product: "kiosk18", DeviceCount: v4},
	}

	now := time.Now()
	deployments := []db.Update{
		{ID: 9001, Status: "active", RebootBehavior: "immediate", CreatedAt: now.Add(-38 * time.Minute),
			Product: "kiosk22", DeviceTotal: total, DeviceInstalled: total * 64 / 100,
			DeviceDownloading: total * 9 / 100, DeviceFailed: total * 1 / 100,
			Release: &db.Release{ID: 90000, Version: "2026.06.01-release", Product: "kiosk22", Status: "published"}},
		{ID: 9002, Status: "active", RebootBehavior: "manual", CreatedAt: now.Add(-3 * time.Hour),
			Product: "kiosk27", DeviceTotal: 48, DeviceInstalled: 48, DeviceAwaiting: 6,
			Release: &db.Release{ID: 89000, Version: "2026.05.14-release", Product: "kiosk27", Status: "published"}},
	}

	k22 := total * 43 / 100
	k27 := total * 28 / 100
	t7 := total * 17 / 100
	prodCounts := map[string]int{"kiosk22": k22, "kiosk27": k27, "t7": t7, "kiosk18": total - k22 - k27 - t7}

	return summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts
}

// sneakPeekMapJSON is a synthetic set of located devices around a few Bay Area
// venues, in the same shape deviceLocationsJSON produces for the overview map.
func sneakPeekMapJSON() template.JS {
	pts := []deviceMapPoint{
		{Serial: "DEMO-0041", Name: "Harbor Grill", Lat: 37.8087, Lon: -122.4098, Online: true, Product: "kiosk22", Battery: 84, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Harbor Grill"},
		{Serial: "DEMO-0042", Name: "Harbor Grill", Lat: 37.8090, Lon: -122.4102, Online: true, Product: "kiosk22", Battery: 79, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Harbor Grill"},
		{Serial: "DEMO-0117", Name: "Sausalito", Lat: 37.8591, Lon: -122.4853, Online: false, Product: "kiosk27", Battery: 61, Kiosk: true, Build: "2026.05.14-release", Restaurant: "Sausalito"},
		{Serial: "DEMO-0119", Name: "Sausalito", Lat: 37.8570, Lon: -122.4840, Online: true, Product: "kiosk27", Battery: 88, Kiosk: true, Build: "2026.05.14-release", Restaurant: "Sausalito"},
		{Serial: "DEMO-0071", Name: "Marina Point", Lat: 37.8060, Lon: -122.4330, Online: true, Product: "t7", Battery: 92, Build: "2026.06.01-release", Restaurant: "Marina Point"},
		{Serial: "DEMO-0093", Name: "Ferry Plaza", Lat: 37.7955, Lon: -122.3937, Online: true, Product: "t7", Battery: 67, Build: "2026.06.01-release", Restaurant: "Ferry Plaza"},
		{Serial: "DEMO-0060", Name: "Embarcadero", Lat: 37.7929, Lon: -122.3968, Online: true, Product: "kiosk22", Battery: 90, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Embarcadero"},
		{Serial: "DEMO-0055", Name: "North Beach", Lat: 37.8003, Lon: -122.4104, Online: true, Product: "kiosk22", Battery: 85, Kiosk: true, Build: "2026.06.01-release", Restaurant: "North Beach"},
		{Serial: "DEMO-0080", Name: "Presidio", Lat: 37.7989, Lon: -122.4662, Online: true, Product: "kiosk27", Battery: 87, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Presidio"},
		{Serial: "DEMO-0102", Name: "Mission Rock", Lat: 37.7716, Lon: -122.3893, Online: true, Product: "kiosk22", Battery: 93, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Mission Rock"},
	}
	b, err := json.Marshal(pts)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

// trainingChapter is one entry of training/manifest.json in the S3 bucket (written by
// tools/reel/publish-training.sh) plus presigned URLs for the page.
type trainingChapter struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Intro    string `json:"intro"`
	Steps    int    `json:"steps"`
	Duration int    `json:"duration"` // seconds
	Video    string `json:"-"`
	Poster   string `json:"-"`
}

const trainingPresignTTL = 6 * time.Hour

var (
	trainingMu     sync.Mutex
	trainingCache  []trainingChapter
	trainingCached time.Time
)

// trainingVideos returns the chapters with presigned URLs, cached for 5 minutes so the
// landing page does not hit S3 on every visit. Empty when S3 is not configured or the
// manifest is missing; the template hides the section then.
func (h *Handler) trainingVideos(ctx context.Context) []trainingChapter {
	if h.apk == nil {
		return nil
	}
	trainingMu.Lock()
	defer trainingMu.Unlock()
	if time.Since(trainingCached) < 5*time.Minute {
		return trainingCache
	}
	trainingCached = time.Now() // also caches a miss, so a missing manifest is cheap
	body, _, _, _, err := h.apk.Get(ctx, "training/manifest.json", "")
	if err != nil {
		trainingCache = nil
		return nil
	}
	defer body.Close()
	var m struct{ Chapters []trainingChapter `json:"chapters"` }
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		log.Printf("[landing] training manifest: %v", err)
		trainingCache = nil
		return nil
	}
	out := m.Chapters[:0]
	for _, c := range m.Chapters {
		v, err1 := h.apk.PresignGet(ctx, "training/"+c.ID+".mp4", trainingPresignTTL)
		p, err2 := h.apk.PresignGet(ctx, "training/"+c.ID+".jpg", trainingPresignTTL)
		if err1 != nil {
			continue
		}
		c.Video = v
		if err2 == nil {
			c.Poster = p
		}
		out = append(out, c)
	}
	trainingCache = out
	return out
}

func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

func (h *Handler) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	user := r.FormValue("username")
	pass := r.FormValue("password")

	// Refuse further attempts once either the source IP or the targeted account
	// has accumulated too many recent failures (F-01).
	ip := ratelimit.ClientIP(r)
	userKey := "u:" + user
	for _, key := range []string{ip, userKey} {
		if n, retry := h.loginFails.Count(key); n >= loginMaxFailures {
			mins := int(retry.Minutes()) + 1
			w.WriteHeader(http.StatusTooManyRequests)
			h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
				"Error": fmt.Sprintf("Too many failed attempts. Try again in %d minute(s).", mins),
				"Brand": h.cfg.CustomBrand(),
			})
			return
		}
	}

	loginOK := func() {
		h.loginFails.Reset(ip)
		h.loginFails.Reset(userKey)
	}

	// Check admin credentials first (constant-time).
	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(h.user)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) == 1
	if userMatch && passMatch {
		loginOK()
		if err := h.startSession(w, r, nil, h.user, "admin"); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Fall back to DB users.
	dbUser, err := h.db.GetUserByUsername(r.Context(), user)
	if err == nil {
		if bcrypt.CompareHashAndPassword([]byte(dbUser.PasswordHash), []byte(pass)) == nil {
			// Self-signup accounts start with email_verified_at NULL until the emailed
			// link is clicked; admin-created accounts are pre-verified at creation time
			// (or have no email at all), so this never blocks them.
			if dbUser.Email != nil && dbUser.EmailVerifiedAt == nil {
				h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
					"Error": "Please verify your email before signing in — check your inbox for the link.",
					"Brand": h.cfg.CustomBrand(),
				})
				return
			}
			loginOK()
			uid := dbUser.ID
			if err := h.startSession(w, r, &uid, dbUser.Username, dbUser.Role); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	// Failed attempt — count it against both the IP and the account.
	h.loginFails.Hit(ip)
	h.loginFails.Hit(userKey)
	h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "Invalid credentials", "Brand": h.cfg.CustomBrand()})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// Delete the server-side session so the cookie is dead immediately, not just
	// dropped by the browser (GB-04).
	session, _ := h.store.Get(r, "mdm-session")
	if sid, _ := session.Values["sid"].(string); sid != "" {
		_ = h.db.DeleteSession(r.Context(), sid)
	}
	session.Options.MaxAge = -1
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound) // the public landing page
}

// newToken returns a random, unguessable, URL-safe token for the email-verify and
// password-reset links (32 bytes of entropy, hex-encoded).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// signupEmailDomain restricts self-service sign-up to the internal team.
const signupEmailDomain = "@aioapp.com"

func (h *Handler) SignupPage(w http.ResponseWriter, r *http.Request) {
	if h.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// SignupSubmit creates a new viewer-role account gated on an @aioapp.com email and
// emails a verify link via Resend. The account can't log in until that link is
// clicked (see LoginSubmit's EmailVerifiedAt check).
func (h *Handler) SignupSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))

	fail := func(msg string) {
		h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{
			"Error": msg, "Email": email, "FirstName": firstName, "LastName": lastName, "Brand": h.cfg.CustomBrand(),
		})
	}

	if firstName == "" || lastName == "" {
		fail("First and last name are required.")
		return
	}

	ip := ratelimit.ClientIP(r)
	for _, key := range []string{ip, "e:" + email} {
		if n, retry := h.signupAttempts.Count(key); n >= 5 {
			mins := int(retry.Minutes()) + 1
			w.WriteHeader(http.StatusTooManyRequests)
			fail(fmt.Sprintf("Too many sign-up attempts. Try again in %d minute(s).", mins))
			return
		}
	}
	h.signupAttempts.Hit(ip)
	h.signupAttempts.Hit("e:" + email)

	if !strings.HasSuffix(email, signupEmailDomain) {
		fail("Sign-up is limited to " + signupEmailDomain + " email addresses.")
		return
	}
	if len(password) < 6 {
		fail("Password must be at least 6 characters.")
		return
	}
	if password != confirm {
		fail("Passwords don't match.")
		return
	}
	if _, err := h.db.GetUserByEmail(r.Context(), email); err == nil {
		fail("An account with that email already exists.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail("Internal error, please try again.")
		return
	}
	u, err := h.db.CreateUserNamed(r.Context(), email, string(hash), "viewer", &email, false, firstName, lastName)
	if err != nil {
		fail("An account with that email already exists.")
		return
	}

	token, err := newToken()
	if err == nil {
		if err := h.db.CreateUserToken(r.Context(), token, u.ID, "verify", time.Now().Add(24*time.Hour)); err == nil {
			verifyURL := h.baseURL(r) + "/verify-email?token=" + token
			if err := h.mail.Send(r.Context(), email, "Confirm your email", mailer.VerifyEmailHTML(verifyURL)); err != nil {
				log.Printf("signup: send verify email to %s: %v", email, err)
			}
		} else {
			log.Printf("signup: create verify token for %s: %v", email, err)
		}
	} else {
		log.Printf("signup: generate verify token for %s: %v", email, err)
	}

	// Username (== email here), not the display name — see h.audit for why the
	// actor field stores the stable identifier rather than a name snapshot.
	h.auditAs(r, u.Username, "user.signup", u.ID.String(), email)
	h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{
		"Sent": true, "Email": email, "Brand": h.cfg.CustomBrand(),
	})
}

// VerifyEmailPage renders a confirm button rather than consuming the token
// immediately on GET — see db.PeekUserToken for why (mail-client link
// prefetching would otherwise silently burn the token before the user's real
// click). Read-only: PeekUserToken doesn't mark anything used.
func (h *Handler) VerifyEmailPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if _, err := h.db.PeekUserToken(r.Context(), token, "verify"); err != nil {
		h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
			"Invalid": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer,
		})
		return
	}
	h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
		"Token": token, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer,
	})
}

// VerifyEmailSubmit does the actual token consumption — only reachable via the
// confirm page's POST, which an automated prefetch/scanner never submits.
func (h *Handler) VerifyEmailSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	token := r.FormValue("token")
	userID, err := h.db.ConsumeUserToken(r.Context(), token, "verify")
	if err != nil {
		h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
			"Invalid": true, "Brand": h.cfg.CustomBrand(),
		})
		return
	}
	if err := h.db.SetUserEmailVerified(r.Context(), userID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	verifyActor := userID.String()
	if u, err := h.db.GetUser(r.Context(), userID); err == nil && u != nil {
		verifyActor = u.Username
	}
	h.auditAs(r, verifyActor, "user.email_verified", userID.String(), "")
	http.Redirect(w, r, "/login?verified=1", http.StatusFound)
}

func (h *Handler) ForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.tmpl.ExecuteTemplate(w, "forgot_password.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// ForgotPasswordSubmit emails a reset link when the address matches a user, but
// always shows the same generic response either way (no account enumeration).
func (h *Handler) ForgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))

	sent := func() {
		h.tmpl.ExecuteTemplate(w, "forgot_password.html", map[string]any{"Sent": true, "Brand": h.cfg.CustomBrand()})
	}

	ip := ratelimit.ClientIP(r)
	for _, key := range []string{ip, "e:" + email} {
		if n, _ := h.resetAttempts.Count(key); n >= 5 {
			sent() // still generic — don't reveal rate limiting to a prober either
			return
		}
	}
	h.resetAttempts.Hit(ip)
	h.resetAttempts.Hit("e:" + email)

	if email == "" {
		sent()
		return
	}
	u, err := h.db.GetUserByEmail(r.Context(), email)
	if err != nil {
		sent()
		return
	}
	token, err := newToken()
	if err != nil {
		sent()
		return
	}
	if err := h.db.CreateUserToken(r.Context(), token, u.ID, "reset", time.Now().Add(time.Hour)); err != nil {
		log.Printf("forgot-password: create reset token for %s: %v", email, err)
		sent()
		return
	}
	resetURL := h.baseURL(r) + "/reset-password?token=" + token
	if err := h.mail.Send(r.Context(), email, "Reset your password", mailer.ResetPasswordHTML(resetURL)); err != nil {
		log.Printf("forgot-password: send reset email to %s: %v", email, err)
	}
	h.audit(r, "user.password_reset_requested", u.ID.String(), "")
	sent()
}

func (h *Handler) ResetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Invalid": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
		return
	}
	h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Token": token, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// ResetPasswordSubmit validates the token, sets the new password, and — since a
// leaked old session could otherwise outlive the credential change — logs the
// account out everywhere by clearing its sessions.
func (h *Handler) ResetPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	token := r.FormValue("token")
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	invalid := func() {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Invalid": true, "Brand": h.cfg.CustomBrand()})
	}
	retry := func(msg string) {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Token": token, "Error": msg, "Brand": h.cfg.CustomBrand()})
	}

	if len(password) < 6 {
		retry("Password must be at least 6 characters.")
		return
	}
	if password != confirm {
		retry("Passwords don't match.")
		return
	}

	userID, err := h.db.ConsumeUserToken(r.Context(), token, "reset")
	if err != nil {
		invalid()
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserPassword(r.Context(), userID, string(hash)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.DeleteSessionsForUser(r.Context(), userID)
	h.audit(r, "user.password_reset", userID.String(), "")
	http.Redirect(w, r, "/login?reset=1", http.StatusFound)
}

const pageSize = 25

// summarizePreview produces a one-line plaintext preview of a cached summary. For a
// structured report it's the headline; otherwise the first meaningful Markdown line.
func summarizePreview(md string) string {
	if rep, ok := ai.ParseReport(md); ok {
		return rep.Headline
	}
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		t = strings.NewReplacer("**", "", "*", "", "`", "", "#", "").Replace(t)
		t = strings.TrimLeft(t, "-0123456789. ")
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(t) > 160 {
			t = t[:157] + "…"
		}
		return t
	}
	return ""
}

// connectedSlice returns the live WebSocket-connected device IDs as a slice, for DB
// queries that compute online/offline from real presence (the ws.Hub) instead of
// check-in recency. Empty slice = nobody online.
func (h *Handler) connectedSlice() []uuid.UUID {
	set := h.hub.ConnectedIDs()
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// deviceFilterFromRequest builds the roster DeviceFilter from the same query
// params DeviceList reads (search/group/restaurant/build/battery/etc, plus the
// view=restaurant|group scoping and the admin-only hidden=only override). Shared
// with DeviceSelectAllSerials so "Select all N matching" resolves the exact same
// set the roster is currently showing.
func (h *Handler) deviceFilterFromRequest(r *http.Request) db.DeviceFilter {
	f := h.deviceFilterFromRequestRaw(r)
	if ids := h.access(r).visibleIDs(); ids != nil {
		f.OnlyIDs = ids
	}
	return f
}

func (h *Handler) deviceFilterFromRequestRaw(r *http.Request) db.DeviceFilter {
	var groupID uuid.UUID
	if gid := r.URL.Query().Get("group"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			groupID = parsed
		}
	}

	var productionID uuid.UUID
	if pid := r.URL.Query().Get("production"); pid != "" {
		if parsed, err := uuid.Parse(pid); err == nil {
			productionID = parsed
		}
	}

	var restaurantID uuid.UUID
	if rid := r.URL.Query().Get("restaurant"); rid != "" {
		if parsed, err := uuid.Parse(rid); err == nil {
			restaurantID = parsed
		}
	}

	// Right-pane mode: "" = roster, "restaurants"/"groups" = collections grid,
	// "restaurant"/"group" = one collection's detail (scopes the device list).
	view := r.URL.Query().Get("view")
	viewID := r.URL.Query().Get("id")
	if view == "restaurant" && viewID != "" {
		if parsed, err := uuid.Parse(viewID); err == nil {
			restaurantID = parsed
		}
	}
	if view == "group" && viewID != "" {
		if parsed, err := uuid.Parse(viewID); err == nil {
			groupID = parsed
		}
	}

	activeThreshold := h.cfg.CheckinInterval() * 3
	// The "Inactive" view (hidden=only) is for the roles above operator (access
	// admin, super op, dev, admin), read-only; marking a device inactive stays
	// admin-only. Any other value collapses to active-only (there is no mixed view).
	hiddenParam := ""
	if r.URL.Query().Get("hidden") == "only" && roleAboveOperator(h.role(r)) {
		hiddenParam = "only"
	}
	return db.DeviceFilter{
		Search:              r.URL.Query().Get("q"),
		GroupID:             groupID,
		RestaurantID:        restaurantID,
		ProductionID:        productionID,
		Online:              r.URL.Query().Get("status"),
		BuildID:             r.URL.Query().Get("build"),
		Battery:             r.URL.Query().Get("battery"),
		Kiosk:               r.URL.Query().Get("kiosk"),
		Charging:            r.URL.Query().Get("charging"),
		Timezone:            r.URL.Query().Get("timezone"),
		Product:             r.URL.Query().Get("product"),
		// Mixed-fleet axes (see docs/ux-enrollment-refactor-plan.md §3.2).
		AgentKind:           r.URL.Query().Get("kind"),
		Class:               r.URL.Query().Get("class"),
		Onboarding:          r.URL.Query().Get("onboarding"),
		Lifecycle:           r.URL.Query().Get("lifecycle"),
		Hidden:              hiddenParam,
		ActiveThresholdSecs: activeThreshold,
		// Online/offline is live WebSocket presence: the status filter and the pill
		// counts (GetSummaryFiltered) resolve it against this connected set.
		Connected: h.connectedSlice(),
	}
}

// DeviceSelectAllSerials returns every serial matching the roster's current
// filter (not just the current page), backing the "Select all N matching" link
// that appears once a full page of checkboxes is selected but more devices exist
// beyond it.
func (h *Handler) DeviceSelectAllSerials(w http.ResponseWriter, r *http.Request) {
	filter := h.deviceFilterFromRequest(r)
	h.access(r).applyFilter(&filter)
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 10000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	serials := make([]string, len(devices))
	for i, d := range devices {
		serials[i] = d.SerialNumber
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"serials": serials})
}

func (h *Handler) DeviceList(w http.ResponseWriter, r *http.Request) {
	// A ?page_size=N from the main-page selector persists to config (survives
	// restarts) so the choice sticks across sessions and machines. Capped at 200 to
	// match the selector's own largest option (devices.html) — the clamp used to
	// allow up to 500 via a direct URL param even though the UI never offers past
	// 200, and 500 rows means thousands of DOM nodes (several inline SVGs per row)
	// that measurably slow down the page's client-side render.
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 && n != h.cfg.PageSize() {
			h.cfg.SetPageSize(n)
		}
	}
	pageSize := h.cfg.PageSize()
	q := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	dir := r.URL.Query().Get("dir")
	if sort == "" {
		sort = h.cfg.DefaultSort()
		if dir == "" {
			if sort == "last_seen" || sort == "created_at" {
				dir = "desc"
			} else {
				dir = "asc"
			}
		}
	}
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	offset := (page - 1) * pageSize

	activeThreshold := h.cfg.CheckinInterval() * 3
	activeThresholdLabel := fmt.Sprintf("%d min", activeThreshold/60)
	filter := h.deviceFilterFromRequest(r)
	// Right-pane mode ("" = roster, "restaurants"/"groups" = collections grid,
	// "restaurant"/"group" = one collection's detail) — filter.RestaurantID/GroupID
	// already carry the view=restaurant|group scoping resolved in deviceFilterFromRequest.
	view := r.URL.Query().Get("view")
	viewID := r.URL.Query().Get("id")
	restaurantID := filter.RestaurantID
	groupID := filter.GroupID

	var (
		devices     []db.Device
		total       int
		fleetTotal  int
		summary     db.Summary
		groups      []db.Group
		productions []db.Production
		builds      []string
		timezones   []string
		restaurants []db.Restaurant
		railGroups  []db.GroupHealth
		railRests   []db.GroupHealth
		railRels    []db.ReleaseRailItem
		prodCounts  map[string]int
		inactiveN   int
	)

	errCh := make(chan error, 10)
	var wg sync.WaitGroup
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				errCh <- err
			}
		}()
	}

	run(func() error {
		var err error
		devices, err = h.db.ListDevices(r.Context(), filter, offset, pageSize, sort, dir)
		return err
	})
	run(func() error {
		var err error
		total, err = h.db.CountDevices(r.Context(), filter)
		return err
	})
	run(func() error {
		var err error
		// Scope the quick-view pill counts to the active group/restaurant (and other
		// contextual filters) so they reflect the visible roster, not the whole fleet.
		summary, err = h.db.GetSummaryFiltered(r.Context(), filter)
		return err
	})
	run(func() error {
		var err error
		// The rail "All devices" tally is always the whole (non-hidden) fleet, never
		// the selected collection — so it doesn't change when a group/restaurant is open.
		fleetTotal, err = h.db.CountDevices(r.Context(), db.DeviceFilter{ActiveThresholdSecs: activeThreshold})
		return err
	})
	run(func() error {
		var err error
		groups, err = h.db.ListGroups(r.Context())
		return err
	})
	run(func() error {
		var err error
		productions, err = h.db.ListProductions(r.Context(), h.connectedSlice())
		return err
	})
	run(func() error {
		var err error
		builds, err = h.db.GetDistinctBuildIDs(r.Context())
		return err
	})
	run(func() error {
		var err error
		timezones, err = h.db.GetDistinctTimezones(r.Context())
		return err
	})
	run(func() error {
		var err error
		restaurants, err = h.db.ListRestaurants(r.Context())
		return err
	})
	run(func() error {
		var err error
		railGroups, err = h.db.GetGroupHealth(r.Context(), h.connectedSlice())
		return err
	})
	run(func() error {
		var err error
		railRests, err = h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), 7)
		return err
	})
	run(func() error {
		var err error
		railRels, err = h.db.ListPublishedReleasesForRail(r.Context())
		railRels = visibleRail(h.role(r), railRels)
		return err
	})
	run(func() error {
		var err error
		prodCounts, err = h.db.CountDevicesByProduct(r.Context())
		return err
	})
	run(func() error {
		var err error
		// Fleet KPI strip: devices marked inactive (hidden), scoped to the same
		// collection/filters as the roster so the tile matches what's on screen.
		inactiveFilter := filter
		inactiveFilter.Hidden = "only"
		inactiveN, err = h.db.CountDevices(r.Context(), inactiveFilter)
		return err
	})

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	connected := h.hub.ConnectedIDsForDisplay()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}

	// Devices whose charger is flapping (charging toggling >10×/min) get a fault symbol
	// on their card. Same window/threshold as the charger_flapping alert default.
	flapping, err := h.db.FlappingChargers(r.Context(), 5, 10)
	if err != nil {
		flapping = map[uuid.UUID]int{}
	}

	// Count of active dropdown filters (drives the "Filters" button badge).
	// Group/restaurant are excluded — those are driven by the collections rail.
	qv := r.URL.Query()
	filterCount := 0
	for _, k := range []string{"status", "production", "build", "battery", "kiosk", "charging", "timezone", "kind", "class", "onboarding", "lifecycle"} {
		if qv.Get(k) != "" {
			filterCount++
		}
	}
	// Name + size of the rail collection currently scoping the roster (heading)
	// and which rail item to mark active.
	selectedCollection := "All devices"
	selectedCount := summary.Total
	activeRestaurant, activeGroup := "", ""
	activeReleaseID := 0 // release DB id when a build scopes the roster (toolbar rename target)
	if restaurantID != uuid.Nil {
		activeRestaurant = restaurantID.String()
		for _, rr := range railRests {
			if rr.GroupID == restaurantID {
				selectedCollection, selectedCount = rr.Name, rr.DeviceCount
				break
			}
		}
	} else if groupID != uuid.Nil {
		activeGroup = groupID.String()
		for _, g := range railGroups {
			if g.GroupID == groupID {
				selectedCollection, selectedCount = g.Name, g.DeviceCount
				break
			}
		}
	} else if bid := qv.Get("build"); bid != "" {
		// A release selected from the rail scopes the roster like a group does.
		for _, rr := range railRels {
			if rr.Version == bid {
				name := rr.Name
				if name == "" {
					name = rr.Version
				}
				selectedCollection, selectedCount = name, rr.DeviceCount
				activeReleaseID = rr.ID
				break
			}
		}
	} else if pk := product.Normalize(qv.Get("product")); pk != "" {
		// A product selected from the rail scopes the roster too — name it by the
		// product label and count the filtered result, so the heading isn't "All devices".
		selectedCollection, selectedCount = productLabel(pk), total
	}

	// Products rail: one entry per catalog product that has at least one device
	// (0-device products are noise, same rule as releases). Counts come from the
	// normalized product column.
	type railProduct struct {
		Key         string
		Label       string
		DeviceCount int
	}
	var railProducts []railProduct
	for _, p := range h.fleetProductFilters(r.Context(), h.role(r)) {
		if n := prodCounts[p.Key]; n > 0 {
			railProducts = append(railProducts, railProduct{p.Key, p.Label, n})
		}
	}

	// For a detail view, the active collection's health entry (header + stats).
	var viewColl *db.GroupHealth
	if view == "restaurant" {
		for i := range railRests {
			if railRests[i].GroupID == restaurantID {
				viewColl = &railRests[i]
			}
		}
	} else if view == "group" {
		for i := range railGroups {
			if railGroups[i].GroupID == groupID {
				viewColl = &railGroups[i]
			}
		}
	}

	// Friendly names ("Table 4") so the roster leads with the name and shows the
	// serial as the subtle line.
	nickIDs := make([]uuid.UUID, 0, len(devices))
	for _, d := range devices {
		nickIDs = append(nickIDs, d.ID)
	}
	nicknames, _ := h.db.GetNicknames(r.Context(), nickIDs)
	if nicknames == nil {
		nicknames = map[uuid.UUID]string{}
	}
	data := map[string]any{
		"Title":                "Devices",
		"Devices":              devices,
		"Nicknames":            nicknames,
		"Flapping":             flapping,
		"Total":                total,
		"FleetTotal":           fleetTotal,
		"FilterCount":          filterCount,
		"InactiveCount":        inactiveN,
		"RailGroups":           railGroups,
		"RailRestaurants":      railRests,
		"RailReleases":         railRels,
		"RailProducts":         railProducts,
		"FilterRestaurant":     qv.Get("restaurant"),
		"SelectedCollection":   selectedCollection,
		"SelectedCount":        selectedCount,
		"ActiveRestaurant":     activeRestaurant,
		"ActiveGroup":          activeGroup,
		"ActiveReleaseID":      activeReleaseID,
		"View":                 view,
		"ViewID":               viewID,
		"ViewColl":             viewColl,
		"Page":                 page,
		"TotalPages":           totalPages,
		"Query":                q,
		"PageSize":             pageSize,
		"Summary":              summary,
		"Sort":                 sort,
		"SortDir":              dir,
		"Online":               online,
		"Groups":               groups,
		"Restaurants":          restaurants,
		"Productions":          productions,
		"Products":             h.fleetProductFilters(r.Context(), h.role(r)),
		"Builds":               builds,
		"Timezones":            timezones,
		"FilterGroup":          r.URL.Query().Get("group"),
		"FilterProduction":     r.URL.Query().Get("production"),
		"FilterProduct":        r.URL.Query().Get("product"),
		"FilterStatus":         r.URL.Query().Get("status"),
		"FilterBuild":          r.URL.Query().Get("build"),
		"FilterBattery":        r.URL.Query().Get("battery"),
		"FilterKiosk":          r.URL.Query().Get("kiosk"),
		"FilterCharging":       r.URL.Query().Get("charging"),
		"FilterTimezone":       r.URL.Query().Get("timezone"),
		"FilterKind":           r.URL.Query().Get("kind"),
		"FilterClass":          r.URL.Query().Get("class"),
		"FilterOnboarding":     r.URL.Query().Get("onboarding"),
		"FilterLifecycle":      r.URL.Query().Get("lifecycle"),
		"Classes":              product.Classes(),
		"FilterHidden":         filter.Hidden,
		"ActiveThresholdSecs":  activeThreshold,
		"ActiveThresholdLabel": activeThresholdLabel,
		"Density":              h.cfg.Density(),
		"MapsEmbedKey":         h.mapsEmbedKey,
	}

	// A rail collection switch (X-Roster-Meta) re-scopes the roster in place: the
	// device list is the main swap target, and the heading + quick-view counts ride
	// along as out-of-band swaps so only the changing bits update (the rail, search
	// bar and inspector are left untouched).
	if r.Header.Get("X-Roster-Meta") == "1" {
		rd := h.withRole(r, data)
		h.tmpl.ExecuteTemplate(w, "device-table", rd)
		h.tmpl.ExecuteTemplate(w, "roster-meta-oob", rd)
		return
	}
	// The table fragment is for the in-place refresh (hx-get polling), NOT for a
	// boosted full-page navigation — a boosted nav must get the whole page (which
	// the layout renders as main-only) so <main> is swapped correctly.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		h.tmpl.ExecuteTemplate(w, "device-table", h.withRole(r, data))
		return
	}
	h.render(w, r, "devices.html", data)
}

// sparkPoints converts a numeric series into an SVG polyline points string for a
// 58x22 viewbox, scaled to the series' own min/max so the shape (not magnitude)
// reads. Returns "" for fewer than two points so the template can omit the svg.
func sparkPoints(vals []float64) string {
	if len(vals) < 2 {
		return ""
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	const wd, ht, pad = 58.0, 22.0, 2.0
	span := hi - lo
	step := (wd - 2) / float64(len(vals)-1)
	var b strings.Builder
	for i, v := range vals {
		y := ht / 2
		if span > 0 {
			y = pad + (ht-2*pad)*(1-(v-lo)/span)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", 1+float64(i)*step, y)
	}
	return b.String()
}

// Overview renders the landing page: fleet pulse score, vital cards with 7-day
// sparklines, the cached AI fleet report, quick actions, and recent activity.
// The devices table itself lives on /devices.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	if h.role(r) == "owner" {
		h.OwnerHome(w, r)
		return
	}
	// The overview is fleet-wide totals; a user who only sees part of the fleet
	// lands on their device list instead.
	if h.access(r).hidesDevices() {
		http.Redirect(w, r, "/devices", http.StatusFound)
		return
	}
	ctx := r.Context()
	activeSecs := h.cfg.CheckinInterval() * 3
	var summary db.Summary
	var err error
	if h.access(r).hidesDPC() {
		// DPC devices are admin/super_op-only: totals exclude them for everyone else.
		summary, err = h.db.GetSummaryFiltered(ctx, db.DeviceFilter{AgentKind: "firmware", Connected: h.connectedSlice(), ActiveThresholdSecs: activeSecs})
	} else {
		summary, err = h.db.GetSummary(ctx, h.connectedSlice())
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The ~8 reads below are all independent (none consumes another's result), so
	// run them concurrently instead of one round trip after another — this is the
	// page every user lands on after login. d14 covers both the 7-day sparklines/
	// activity chart AND the 14-day week-over-week trend, so it's fetched once and
	// sliced, instead of the old code fetching 7 and then 14 days separately.
	var (
		groups      []db.GroupHealth
		hot         int
		d14         []db.FleetDailyStat
		openCount   int
		crashStats  db.FleetCrashStats
		versions    []db.FleetVersion
		deployments []db.Update
		prodCounts  map[string]int
	)
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { groups, _ = h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7) })
	run(func() { hot, _ = h.db.CountHotDevices(ctx) })
	run(func() { d14, _ = h.db.GetFleetDailyStats(ctx, 14) })
	run(func() { openCount, _ = h.db.CountOpenAlerts(ctx) })
	run(func() { crashStats, _ = h.db.GetFleetCrashStats(ctx, 4) })
	run(func() { versions, _ = h.db.GetFleetVersions(ctx) })
	run(func() { deployments, _ = h.db.ListDeployments(ctx) })
	run(func() { prodCounts, _ = h.db.CountDevicesByProduct(ctx) })
	wg.Wait()

	inbox, _ := h.db.ListOnboardingInbox(r.Context(), 5)
	inboxN := 0
	if fc, err := h.db.FleetCounts(r.Context(), h.access(r).hidesDPC()); err == nil {
		inboxN = fc.Inbox
	}
	data := h.overviewViewModel(r, summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts, activeSecs, inbox, inboxN)

	// Power & usage widget: the same figures the restaurant page shows per site, summed
	// across every placed device (uuid.Nil = the whole deployed fleet).
	if m, err := h.db.SiteMetricsFor(ctx, uuid.Nil, 7); err == nil {
		data["PowerMetrics"] = m
	}

	// Same cached hourly AI fleet report the devices page used to host.
	if s, err := h.db.GetAISummary(ctx, "fleet"); err == nil && s.Summary != "" {
		data["AISummary"] = s.Summary
		data["AISummaryModel"] = s.Model
		data["AISummaryAt"] = s.GeneratedAt.UTC().Format(time.RFC3339)
		data["AISummaryPreview"] = summarizePreview(s.Summary)
		if serials, err := h.db.ListAllSerials(ctx); err == nil {
			data["DeviceSerials"] = serialsJSON(serials)
		}
		data["HotSerials"] = serialsJSON(hotSerialsFromHealth(groups, h.alertThresholds(ctx).TempC))
		data["ReportRestaurants"] = restaurantLinksJSON(groups)
		data["SeriesJSON"] = fleetSeriesJSON(d14, crashStats.Daily, summary.Total)
	}

	// Fleet map: every device with a resolved location from the geolocation pipeline.
	locs, locCount := h.deviceLocationsJSON(ctx, h.access(r).hidesDPC())
	data["DeviceLocations"] = locs
	data["DeviceMapCount"] = locCount
	data["MapsEmbedKey"] = h.mapsEmbedKey

	// Per-user widget arrangement (hidden / order / preset).
	for k, v := range h.overviewLayoutData(r) {
		data[k] = v
	}

	h.render(w, r, "overview.html", data)
}

// overviewViewModel builds the Overview page's data map from already-fetched
// fleet data. Split out of Overview so the public /sneak-peek preview can render
// the exact same template against synthetic data (no DB reads, no real fleet).
func (h *Handler) overviewViewModel(r *http.Request, summary db.Summary, groups []db.GroupHealth, hot int, d14 []db.FleetDailyStat, openCount int, crashStats db.FleetCrashStats, versions []db.FleetVersion, deployments []db.Update, prodCounts map[string]int, activeSecs int, inbox []db.Device, inboxN int) map[string]any {
	daily := d14
	if n := len(d14); n > 7 {
		daily = d14[n-7:]
	}

	// Fleet score: device-weighted mean of the per-group health scores. Without
	// groups, fall back to an online-ratio penalty so the ring still means something.
	score := 100
	if summary.Total > 0 {
		num, den := 0, 0
		for _, g := range groups {
			num += g.Score * g.DeviceCount
			den += g.DeviceCount
		}
		if den > 0 {
			score = num / den
		} else {
			// No venue has devices yet (or no venues at all): fall back to the
			// online-ratio proxy so the ring still reflects reality.
			score -= 40 * (summary.Total - summary.RecentlyActive) / summary.Total
		}
	}
	scoreClass := "danger"
	switch {
	case score >= 80:
		scoreClass = "ok"
	case score >= 50:
		scoreClass = "warn"
	}
	// "Why this score": the venue penalties behind the ring, biggest first.
	healthReasons := explainFleetHealth(groups, crashStats.ByRestaurant)
	healthLost := 0.0
	for _, r := range healthReasons {
		healthLost += r.Points
	}

	offline := summary.Total - summary.RecentlyActive
	attention := offline + summary.LowBattery + hot
	verdict := "Fleet is healthy — all devices reporting"
	if attention == 1 {
		verdict = "Fleet is healthy — 1 device needs a look"
	} else if attention > 1 {
		verdict = fmt.Sprintf("%d devices need a look", attention)
	}

	var actS, offS, lowS, hotS []float64
	for _, ds := range daily {
		actS = append(actS, float64(ds.Active))
		offS = append(offS, math.Max(0, float64(summary.Total-ds.Active)))
		lowS = append(lowS, float64(ds.LowBattery))
		hotS = append(hotS, float64(ds.Hot))
	}

	// Activity bars for the overview chart: one bar per day, height relative to
	// the busiest day. The most recent day is flagged so the chart highlights it.
	var peak int
	for _, ds := range daily {
		if ds.Active > peak {
			peak = ds.Active
		}
	}
	activityBars := make([]map[string]any, 0, len(daily))
	for i, ds := range daily {
		pct := 4
		if peak > 0 {
			if pct = ds.Active * 100 / peak; pct < 4 {
				pct = 4
			}
		}
		activityBars = append(activityBars, map[string]any{
			"Label": ds.Day.Format("Mon")[:2],
			"Val":   ds.Active,
			"Pct":   pct,
			"Cur":   i == len(daily)-1,
		})
	}

	// Same series as a line/area chart for the overview Fleet-activity card.
	actPts := make([]map[string]any, 0, len(daily))
	for _, ds := range daily {
		actPts = append(actPts, map[string]any{"label": ds.Day.Format("Mon"), "val": ds.Active, "low": ds.LowBattery})
	}
	activityJSON := template.JS("[]")
	if b, err := json.Marshal(actPts); err == nil {
		activityJSON = template.JS(b)
	}

	hour := time.Now().Hour()
	greeting := "Good evening"
	if hour < 12 {
		greeting = "Good morning"
	} else if hour < 17 {
		greeting = "Good afternoon"
	}

	// ── Command-center additions ──
	sIf := func(n int) string {
		if n == 1 {
			return ""
		}
		return "s"
	}
	// Crash surfaces (24h rollup): signal tile + hero sentence.
	// (openCount, crashStats already fetched concurrently above.)
	var crS []float64
	for _, v := range crashStats.Daily {
		crS = append(crS, float64(v))
	}

	// Restaurant-level verdict + worst-first list for the "Where to look" bridge.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Score < groups[j].Score })
	attentionRest := 0
	worst := make([]db.GroupHealth, 0, 3)
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass != "ok" {
			attentionRest++
			if len(worst) < 3 {
				worst = append(worst, g)
			}
		}
	}
	restVerdict := "All restaurants healthy"
	switch {
	case attentionRest == 1:
		restVerdict = "1 restaurant needs a look today"
	case attentionRest > 1:
		restVerdict = fmt.Sprintf("%d restaurants need a look today", attentionRest)
	}
	statusWord := map[string]string{"ok": "Healthy", "warn": "Watch", "danger": "At risk"}[scoreClass]

	// Hero sentence: real live counts + the single worst location's reason.
	crashWord := "crashes"
	if crashStats.Total24h == 1 {
		crashWord = "crash"
	}
	heroSentence := fmt.Sprintf("%d of %d devices online · %d %s in the last 24h · %d open alert%s.",
		summary.RecentlyActive, summary.Total, crashStats.Total24h, crashWord,
		openCount, sIf(openCount))
	if len(worst) > 0 {
		w := worst[0]
		var bits []string
		if w.OfflineCount > 0 {
			bits = append(bits, fmt.Sprintf("%d offline", w.OfflineCount))
		}
		if w.TempMax != nil && *w.TempMax >= 45 {
			bits = append(bits, "a unit running hot")
		}
		if c := crashStats.ByRestaurant[w.GroupID]; c > 0 {
			bits = append(bits, fmt.Sprintf("%d crashes", c))
		}
		if len(bits) > 0 {
			heroSentence += fmt.Sprintf(" %s is the worst — %s.", w.Name, strings.Join(bits, ", "))
		}
	}

	// Week-over-week fleet-health trend: a connectivity proxy (100 − 40·offline-ratio,
	// the same fallback the ring uses) averaged this week vs the prior week.
	scoreDelta := 0
	if summary.Total > 0 {
		proxy := func(active int) float64 {
			off := float64(summary.Total - active)
			if off < 0 {
				off = 0
			}
			return 100 - 40*off/float64(summary.Total)
		}
		var recent, prior float64
		var rc, pc int
		n := len(d14)
		for i, ds := range d14 {
			if i >= n-7 {
				recent += proxy(ds.Active)
				rc++
			} else {
				prior += proxy(ds.Active)
				pc++
			}
		}
		if rc > 0 && pc > 0 {
			scoreDelta = int(math.Round(recent/float64(rc) - prior/float64(pc)))
		}
	}

	// Today-vs-yesterday deltas for the signal tiles.
	onToday, onYest, offToday, offYest, lowToday, lowYest := 0, 0, 0, 0, 0, 0
	if len(daily) >= 2 {
		a, b := daily[len(daily)-1], daily[len(daily)-2]
		onToday, onYest = a.Active, b.Active
		offToday, offYest = summary.Total-a.Active, summary.Total-b.Active
		lowToday, lowYest = a.LowBattery, b.LowBattery
	}
	crToday, crYest := 0, 0
	if n := len(crashStats.Daily); n >= 2 {
		crToday, crYest = crashStats.Daily[n-1], crashStats.Daily[n-2]
	}

	// ── Sites heatmap: every venue as a score-coloured tile, worst first ──
	type siteTile struct {
		ID           uuid.UUID
		Name         string
		Score        int
		Class        string
		Devices      int
		Offline      int
		Critical     int
		Warning      int
		Crashes      int
		Hot          bool
		BatteryAvg   int
		HasBattery   bool
		Why          []string // penalties behind the score, for the "?" popover
	}
	var sites []siteTile
	sitesOK, sitesWarn, sitesBad, deployedN := 0, 0, 0, 0
	for _, g := range groups {
		if g.DeviceCount == 0 {
			continue
		}
		deployedN += g.DeviceCount
		t := siteTile{
			ID: g.GroupID, Name: g.Name, Score: g.Score, Class: g.ScoreClass,
			Devices: g.DeviceCount, Offline: g.OfflineCount,
			Critical: g.OpenCritical, Warning: g.OpenWarning,
			Crashes: crashStats.ByRestaurant[g.GroupID],
			Hot:     g.TempMax != nil && *g.TempMax >= 45,
			Why:     whyScore(g),
		}
		if g.BatteryAvg != nil {
			t.BatteryAvg, t.HasBattery = int(math.Round(*g.BatteryAvg)), true
		}
		switch g.ScoreClass {
		case "ok":
			sitesOK++
		case "warn":
			sitesWarn++
		default:
			sitesBad++
		}
		sites = append(sites, t)
	}

	// ── Release adoption: top builds by device count + an "other" bucket ──
	type versionRow struct {
		Version string
		Product string
		Count   int
		Pct     int
		Status  string // published | draft | unmanaged
		Hidden  bool
	}
	versionsTotal := 0
	for _, v := range versions {
		versionsTotal += v.DeviceCount
	}
	var versionRows []versionRow
	versionOther, versionOtherN := 0, 0
	for i, v := range versions {
		if i >= 5 {
			versionOther += v.DeviceCount
			versionOtherN++
			continue
		}
		st := "unmanaged"
		if v.ReleaseID != nil {
			st = v.ReleaseStatus
			if st == "" {
				st = "draft"
			}
		}
		pct := 0
		if versionsTotal > 0 {
			pct = v.DeviceCount * 100 / versionsTotal
		}
		versionRows = append(versionRows, versionRow{
			Version: v.Version, Product: product.Label(v.Product), Count: v.DeviceCount,
			Pct: pct, Status: st, Hidden: v.ReleaseHidden,
		})
	}
	versionOtherPct := 0
	if versionsTotal > 0 {
		versionOtherPct = versionOther * 100 / versionsTotal
	}
	// Share of the fleet on a published (managed) build.
	onPublished := 0
	for _, v := range versions {
		if v.ReleaseID != nil && v.ReleaseStatus == "published" {
			onPublished += v.DeviceCount
		}
	}
	onPublishedPct := 0
	if versionsTotal > 0 {
		onPublishedPct = onPublished * 100 / versionsTotal
	}

	// ── Rollouts in flight: pending/active OTA deployments, newest first ──
	type rolloutRow struct {
		ID          int
		Version     string
		Product     string
		Status      string
		Total       int
		Installed   int
		Downloading int
		Failed      int
		PctDone     int
		PctDown     int
		PctFail     int
		Ago         string
	}
	var rollouts []rolloutRow
	rolloutsN := 0
	for _, u := range deployments {
		if u.Status != "pending" && u.Status != "active" {
			continue
		}
		rolloutsN++
		if len(rollouts) >= 4 {
			continue
		}
		rr := rolloutRow{
			ID: u.ID, Status: u.Status, Total: u.DeviceTotal, Installed: u.DeviceInstalled,
			Downloading: u.DeviceDownloading, Failed: u.DeviceFailed, Ago: agoShort(u.CreatedAt),
			Product: product.Label(u.Product),
		}
		if u.Release != nil {
			rr.Version = u.Release.Version
		}
		if rr.Total > 0 {
			rr.PctDone = rr.Installed * 100 / rr.Total
			rr.PctDown = rr.Downloading * 100 / rr.Total
			rr.PctFail = rr.Failed * 100 / rr.Total
		}
		rollouts = append(rollouts, rr)
	}

	// ── Product mix for the hero composition bar ──
	type productRow struct {
		Key   string
		Label string
		Count int
		Pct   int
	}
	var products []productRow
	for _, p := range h.fleetProductFilters(r.Context(), h.role(r)) {
		if n := prodCounts[p.Key]; n > 0 {
			pct := 0
			if summary.Total > 0 {
				pct = n * 100 / summary.Total
			}
			products = append(products, productRow{p.Key, p.Label, n, pct})
		}
	}
	sort.SliceStable(products, func(i, j int) bool { return products[i].Count > products[j].Count })

	// ── Fleet vitals ──
	batteryAvg, hasBatteryAvg := 0, false
	if n := len(daily); n > 0 && daily[n-1].BatteryAvg != nil {
		batteryAvg, hasBatteryAvg = int(math.Round(float64(*daily[n-1].BatteryAvg))), true
	}
	chargeNum, chargeDen := 0.0, 0
	var hottest float64
	hottestSerial := ""
	for _, g := range groups {
		if g.ChargingAvg != nil && g.DeviceCount > 0 {
			chargeNum += *g.ChargingAvg * float64(g.DeviceCount)
			chargeDen += g.DeviceCount
		}
		if g.TempMax != nil && *g.TempMax > hottest {
			hottest = *g.TempMax
			if g.TempMaxSerial != nil {
				hottestSerial = *g.TempMaxSerial
			}
		}
	}
	chargePct, hasCharge := 0, false
	if chargeDen > 0 {
		chargePct, hasCharge = int(math.Round(chargeNum/float64(chargeDen)*100)), true
	}
	pctOf := func(n int) int {
		if summary.Total == 0 {
			return 0
		}
		return n * 100 / summary.Total
	}
	data := map[string]any{
		"Title": "Overview",
		"NowUTC": time.Now().UTC().Format(time.RFC3339),
		// New command-center surfaces.
		"Sites":           sites,
		"SitesOK":         sitesOK,
		"SitesWarn":       sitesWarn,
		"SitesBad":        sitesBad,
		"DeployedCount":   deployedN,
		"DeployedPct":     pctOf(deployedN),
		"KioskPct":        pctOf(summary.KioskCount),
		"Versions":        versionRows,
		"VersionsTotal":   versionsTotal,
		"VersionsCount":   len(versions),
		"VersionOther":    versionOther,
		"VersionOtherN":   versionOtherN,
		"VersionOtherPct": versionOtherPct,
		"OnPublishedPct":  onPublishedPct,
		"Rollouts":        rollouts,
		"RolloutsCount":   rolloutsN,
		"Inbox":           inbox,
		"InboxCount":      inboxN,
		"Products":        products,
		"BatteryAvg":      batteryAvg,
		"HasBatteryAvg":   hasBatteryAvg,
		"ChargePct":       chargePct,
		"HasCharge":       hasCharge,
		"Hottest":         int(math.Round(hottest)),
		"HottestSerial":   hottestSerial,
		// withRole overwrites this on success; the default keeps the template's
		// numeric comparison safe if the alerts count query fails.
		"AlertsOpenCount": 0,
		"Summary":         summary,
		"Offline":         offline,
		"Hot":             hot,
		"Attention":       attention,
		"Score":           score,
		"HealthReasons":   healthReasons,
		"HealthLost":      healthLost,
		"ScoreClass":      scoreClass,
		// 2π·r44 = 276.5; the ring template animates to this offset.
		"RingOffset":           fmt.Sprintf("%.1f", 276.5*float64(100-score)/100),
		"Verdict":              verdict,
		"Greeting":             greeting,
		"DateLine":             time.Now().Format("Monday, January 2"),
		"SparkActive":          sparkPoints(actS),
		"SparkOff":             sparkPoints(offS),
		"SparkLow":             sparkPoints(lowS),
		"SparkHot":             sparkPoints(hotS),
		"ActivityBars":         activityBars,
		"ActivityJSON":         activityJSON,
		"ActivityPeak":         peak,
		"ActiveThresholdLabel": fmt.Sprintf("%d min", activeSecs/60),
		// Command-center fields.
		"RestVerdict":     restVerdict,
		"StatusWord":      statusWord,
		"ScoreDelta":      scoreDelta,
		"HeroSentence":    heroSentence,
		"AttentionRest":   attentionRest,
		"Crashes24h":      crashStats.Total24h,
		"CrashDevices24h": crashStats.Devices24h,
		"CrashSpark":      sparkPoints(crS),
		"OnlineDelta":     tileDelta(onToday, onYest, false),
		"OfflineDelta":    tileDelta(offToday, offYest, true),
		"LowDelta":        tileDelta(lowToday, lowYest, true),
		"CrashDelta":      tileDelta(crToday, crYest, true),
	}
	return data
}

// launchableOnly filters a device's package list down to app-drawer apps
// (launchable = true) — the full dump is mostly RRO overlays and framework
// plumbing nobody manages. Devices whose client predates the launchable flag
// (no row carries it) keep the full list rather than rendering an empty page.
// Display and kiosk pickers only: pending-install reconciliation and
// InstalledSet must keep working off the FULL list.
func launchableOnly(pkgs []db.DevicePackage) []db.DevicePackage {
	reported := false
	out := pkgs[:0:0]
	for _, p := range pkgs {
		if p.Launchable != nil {
			reported = true
			if *p.Launchable {
				out = append(out, p)
			}
		}
	}
	if !reported {
		return pkgs
	}
	return out
}

// pendingInstallRows returns "installing" rows (resolved app name + apk url) for
// install_apk commands not yet completed/failed — shown in the device Applications list.
// Commands are deduped by APK URL: re-sending the same install (or a group target
// re-firing it) yields one row with a Count, not a stack of identical rows.
// installedPkgs = the packages the device currently reports; apkPkg = learned
// apk_url→package map. A pending install whose APK maps to a package the device
// already has is treated as done and not shown (covers a lost terminal ack until the
// next check-in reconciles it in the DB).
func pendingInstallRows(commands []db.DeviceCommand, apps []db.App, installedPkgs []db.DevicePackage, apkPkg map[string]string) []map[string]string {
	name := make(map[string]string, len(apps))
	for _, a := range apps {
		name[a.ApkURL] = a.Name
	}
	have := make(map[string]bool, len(installedPkgs))
	for _, p := range installedPkgs {
		have[p.PackageName] = true
	}
	alreadyInstalled := func(apkURL string) bool {
		pkg, ok := apkPkg[apkURL]
		return ok && have[pkg]
	}
	// A human phase label for the row's spinner tag, plus the download percent.
	label := func(status string, progress *int) (string, string) {
		switch status {
		case "downloading":
			if progress != nil {
				return "downloading", strconv.Itoa(*progress)
			}
			return "downloading", ""
		case "installing":
			return "installing", ""
		default: // pending / delivered — queued, not yet picked up
			return "installing", ""
		}
	}
	inFlight := func(s string) bool {
		return s == "pending" || s == "delivered" || s == "downloading" || s == "installing"
	}
	var out []map[string]string
	idx := make(map[string]int) // ApkURL -> position in out
	for _, c := range commands {
		if c.Type == "install_apk" && inFlight(c.Status) && !alreadyInstalled(c.ApkURL) {
			if i, ok := idx[c.ApkURL]; ok {
				out[i]["Count"] = strconv.Itoa(atoi(out[i]["Count"]) + 1)
				continue
			}
			n := name[c.ApkURL]
			if n == "" {
				n = c.ApkURL
			}
			phase, pct := label(c.Status, c.Progress)
			idx[c.ApkURL] = len(out)
			out = append(out, map[string]string{"Name": n, "ApkURL": c.ApkURL, "Count": "1", "Phase": phase, "Progress": pct})
		}
	}
	return out
}

// pendingUninstallPkgs returns the set of package names with an in-flight uninstall
// command, so the Applications drawer can show them as "Uninstalling…" until the device
// confirms removal (mirrors pendingInstallRows for installs).
func pendingUninstallPkgs(commands []db.DeviceCommand) map[string]bool {
	inFlight := func(s string) bool {
		return s == "pending" || s == "delivered" || s == "downloading" || s == "installing"
	}
	out := map[string]bool{}
	for _, c := range commands {
		if c.Type != "uninstall" || !inFlight(c.Status) {
			continue
		}
		var p struct {
			Package string `json:"package"`
		}
		if json.Unmarshal(c.Payload, &p) == nil && p.Package != "" {
			out[p.Package] = true
		}
	}
	return out
}

// atoi is a forgiving strconv.Atoi: it returns 0 for unparseable input.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// commandDedupKey identifies "the same command" for duplicate suppression. Install keys
// on the APK URL only (its payload carries a volatile size/etag that must NOT defeat
// dedup); every other type keys on its payload, so an empty-payload one-shot like reboot
// or screenshot collapses to a single in-flight instance per type, while shell/uninstall/
// kiosk distinguish by their arguments.
func commandDedupKey(cmdType, apkURL string, payload json.RawMessage) string {
	if cmdType == "install_apk" {
		return "install_apk|" + apkURL
	}
	p := strings.TrimSpace(string(payload))
	if p == "{}" || p == "null" {
		p = ""
	}
	return cmdType + "|" + p
}

// hasPendingLikeCommand reports whether an identical command (same type + key params) is
// already in flight (pending/delivered/downloading/installing, i.e. not terminal) among
// the device's recent commands. Used to stop a repeat click — or a group target re-firing
// — from stacking a duplicate the device must process again.
func findPendingLikeCommand(commands []db.DeviceCommand, cmdType, apkURL string, payload json.RawMessage) (db.DeviceCommand, bool) {
	key := commandDedupKey(cmdType, apkURL, payload)
	for _, c := range commands {
		switch c.Status {
		case "pending", "delivered", "downloading", "installing":
			if commandDedupKey(c.Type, c.ApkURL, c.Payload) == key {
				return c, true
			}
		}
	}
	return db.DeviceCommand{}, false
}

// DeviceAppsList renders just the installed-apps list region for the device page,
// refreshed live via HTMX when the device checks in (so a freshly installed app
// shows up without a manual reload).
func (h *Handler) DeviceAppsList(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	installedPkgs, _ := h.db.GetDevicePackages(r.Context(), device.ID)
	commands, _ := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	apps, _ := h.db.ListApps(r.Context())
	apkPkg, _ := h.db.GetApkPackageMap(r.Context(), nil)
	h.tmpl.ExecuteTemplate(w, "device-apps-list", map[string]any{
		"Device": device,
		"Role":   h.role(r),
		// Display only drawer apps; the pending-install reconciliation below still
		// checks against the FULL list so a non-launchable install isn't re-shown.
		"InstalledPackages": launchableOnly(installedPkgs),
		"PendingInstalls":   pendingInstallRows(commands, apps, installedPkgs, apkPkg),
		"Uninstalling":      pendingUninstallPkgs(commands),
	})
}

// otaStatusView maps an update_devices status (+ optional live download/install
// progress reported by the device) to a human label, a .dq-pill status class to
// reuse (already styled on the device page for the command queue), and a 0-100
// progress-bar percent. Shared by DeviceDetail's initial render and
// DeviceOtaProgress's poll fragment so they can never disagree.
func otaStatusView(status string, p *shell.OTAProgress) (label, class string, percent int) {
	// A terminal DB status is authoritative over shell.Manager's in-memory cache, which
	// has no TTL and is never cleared by an out-of-band status correction — otherwise a
	// stale live percent can keep showing next to "Installed"/"Failed" indefinitely.
	if status == "installed" || status == "failed" {
		p = nil
	}
	if p != nil {
		percent = p.Percent
		// The live phase the device just reported is finer-grained and fresher than
		// update_devices.status, which only advances at coarse checkpoints (e.g. it
		// may still read "downloading" for a beat after the device has already moved
		// into verifying/installing/finalizing) — prefer it when we have it.
		switch p.Phase {
		case "downloading":
			return "Downloading", "dl", percent
		case "verifying":
			return "Verifying", "inst", percent
		case "installing":
			return "Installing", "inst", percent
		case "finalizing":
			return "Finalizing", "inst", percent
		}
	}
	switch status {
	case "pending":
		return "Queued", "run", percent
	case "downloading":
		// No live telemetry yet (p == nil) — show the real 0%, matching the
		// deployment page's device row, which shows no percent at all in this same
		// case rather than a made-up placeholder value.
		return "Downloading", "dl", percent
	case "installing":
		if percent == 0 {
			percent = 60
		}
		return "Installing", "inst", percent
	case "awaiting_reboot":
		return "Awaiting reboot", "done", 100
	case "reboot_sent":
		return "Reboot pending", "done", 95
	case "failed":
		return "Failed", "fail", 100
	case "installed":
		return "Installed", "done", 100
	default:
		return status, "run", percent
	}
}

// DeviceOtaProgress renders just the OTA progress card as a standalone fragment —
// the device page self-polls this every 2s while an update is in flight (see
// "device-ota-progress" in device.html), the same pattern device-apps-list uses
// for install progress, since /events SSE doesn't carry OTA progress.
func (h *Handler) DeviceOtaProgress(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	// ResolveUpdateForDevice returns (nil, nil) — not an error — when nothing is
	// targeting this device, so both must be checked before touching u's fields.
	// The poller (see device-ota-progress) keeps running while OtaUpdate is set even
	// during "pending" — the card itself just stays hidden until the device actually
	// starts downloading, so the transition out of "pending" is still caught live.
	u, err := h.db.ResolveUpdateForDevice(r.Context(), device.ID)
	data := map[string]any{"Device": device}
	if err == nil && u != nil {
		label, class, percent := otaStatusView(u.DeviceStatus, h.shell.GetOTAProgress(device.ID))
		data["OtaUpdate"] = u
		data["OtaLabel"] = label
		data["OtaClass"] = class
		data["OtaPercent"] = percent
	}
	h.tmpl.ExecuteTemplate(w, "device-ota-progress", data)
}

// downsampleCheckins keeps at most maxPoints evenly-strided rows, preserving whatever
// order it's given (GetCheckinsForDuration/GetCheckinsBetween return newest-first).
// Mirrors the stride used by DeviceChartData for the on-demand range fetch.
func downsampleCheckins(checkins []db.Checkin, maxPoints int) []db.Checkin {
	n := len(checkins)
	if n <= maxPoints {
		return checkins
	}
	stride := (n + maxPoints - 1) / maxPoints
	out := make([]db.Checkin, 0, maxPoints+1)
	for i := 0; i < n; i += stride {
		out = append(out, checkins[i])
	}
	return out
}

func (h *Handler) DeviceDetail(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}


	// The ~15 reads below are all independent of each other (each keyed only off
	// device.ID/role, none consumes another's result), so they were previously run
	// one at a time — total latency was the SUM of every query's round trip. Firing
	// them concurrently drops it to the SLOWEST single query. Only the four that were
	// already hard 500s on error (commands/apps/installedPkgs/kioskCfg) can fail this
	// group; the rest already tolerated errors silently (`_, err :=` discarded) and
	// keep doing so.
	var (
		chartCheckins   []db.Checkin
		chartCharge     []chargeRun
		commands        []db.DeviceCommand
		queue           []db.DeviceCommand
		apps            []db.App
		installedPkgs   []db.DevicePackage
		apkPkg          map[string]string
		kioskCfg        *db.DeviceConfig
		restaurants     []db.Restaurant
		deviceGroups    []db.Group
		addableGroups   []db.Group
		release         *db.Release
		notes           string
		flapRate        int
		crashCount      int
		otaUpdate       *db.Update
		otaLabel        string
		otaClass        string
		otaPercent      int
		buildChanges    []db.BuildChange
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	role := h.role(r)
	ctx := r.Context()
	t0 := time.Now()
	// Access policy: a device the user may not view is hidden (404) when their
	// policy hides out-of-scope devices, else shown read-only; a device they may
	// view but not act on renders with the viewer's action set.
	if acc := h.access(r); !acc.unrestricted() {
		if !acc.canDevice("view", device.ID) {
			if acc.hidesDevices() {
				http.NotFound(w, r)
				return
			}
			role = "viewer"
		} else if !acc.anyDeviceAction(device.ID) {
			role = "viewer"
		}
	}
	focusParam := r.URL.Query().Get("focus")

	run(func() {
		// The chart normally loads the recent window. When the page is opened to
		// focus a past incident (e.g. a heat call-out deep-links ?focus=temp),
		// center the fetch on the day of that metric's extreme so the spike is in
		// range — but bounded to ~2 days, since these devices can check in every
		// few seconds and a multi-day pull would bloat the page. The client then
		// zooms to a 1-hour window around the peak.
		var cc []db.Checkin
		var err error
		var havePeak bool
		focus := r.URL.Query().Get("focus")
		if focus != "" {
			if stats, statErr := h.db.GetDeviceDailyStats(ctx, device.ID, 8); statErr == nil {
				var peakDay time.Time
				if peakDay, havePeak = peakDayForFocus(stats, focus); havePeak {
					cc, err = h.db.GetCheckinsBetween(ctx, device.ID, peakDay.Add(-12*time.Hour), peakDay.Add(36*time.Hour))
				}
			}
		}
		if !havePeak {
			// Bake in only the default 6h view (matches the chart's default
			// currentDuration) instead of the full 48h range the duration buttons
			// can reach — these devices can check in every few seconds, so 48h can
			// be tens of thousands of rows, and rendering all of them into the page
			// HTML (3 template passes over the same slice, for battery/temp/ram)
			// was the single biggest driver of a slow first paint. The 12h/24h/48h
			// buttons pull their extra history from /chart-data on demand
			// (chartLoadOlder), and the client also warms that cache in the
			// background right after load — see chartWarmBackground in device.html.
			cc, err = h.db.GetCheckinsForDuration(ctx, device.ID, device.LastSeenAt.Add(-6*time.Hour))
		}
		if err != nil {
			fail(err)
			return
		}
		// Safety net in case a device's poll interval is far below normal — thin to
		// the same maxPoints the on-demand /chart-data endpoint already uses.
		if device.HasBattery() && device.HasCharging() {
			asc := make([]db.Checkin, len(cc))
			for i := range cc {
				asc[i] = cc[len(cc)-1-i]
			}
			chartCharge = chargeRuns(asc, chartGapMs(device.PollIntervalMs))
		}
		chartCheckins = downsampleCheckins(cc, 600) // the default 6h window; wider ranges come from /chart-data
	})
	run(func() {
		c, err := h.db.GetDeviceCommands(ctx, device.ID, h.cfg.CommandExpiry())
		if err != nil {
			fail(err)
			return
		}
		redactDeviceCommandURLs(role, c)
		commands = filterShellDeviceCommands(role, c)
	})
	run(func() {
		// Best-effort: build-change markers on the vitals chart. A failure here
		// only loses the markers, not the page.
		bc, err := h.db.GetBuildChanges(ctx, device.ID)
		if err != nil {
			log.Printf("[device] GetBuildChanges %s: %v", device.SerialNumber, err)
			return
		}
		mu.Lock()
		buildChanges = bc
		mu.Unlock()
	})
	run(func() {
		// The per-device command queue (non-terminal commands, FIFO) for the Queue tab.
		q, _ := h.db.GetDeviceQueue(ctx, device.ID)
		redactDeviceCommandURLs(role, q)
		queue = withoutOTA(filterShellDeviceCommands(role, q))
	})
	run(func() {
		a, err := h.db.ListApps(ctx)
		if err != nil {
			fail(err)
			return
		}
		apps = a
	})
	run(func() {
		p, err := h.db.GetDevicePackages(ctx, device.ID)
		if err != nil {
			fail(err)
			return
		}
		installedPkgs = p
	})
	run(func() { apkPkg, _ = h.db.GetApkPackageMap(ctx, nil) })
	run(func() {
		cfg, err := h.db.GetOrCreateDeviceConfig(ctx, device.ID)
		if err != nil {
			fail(err)
			return
		}
		kioskCfg = cfg
	})
	run(func() {
		if role == "admin" {
			restaurants, _ = h.db.ListRestaurants(ctx)
		}
	})
	run(func() {
		dg, _ := h.db.ListDeviceGroups(ctx, device.ID)
		deviceGroups = dg
		// Groups the device is NOT yet in — for the placement "+ group" picker.
		// Populated for the same roles the picker button renders for and the add
		// endpoint accepts (admin/dev/operator, i.e. canAdminOrOperator); gating this on
		// admin alone left an operator the button and popover but an empty group list,
		// so "+ group" did nothing.
		if roleCanOperate(role) {
			inGroup := make(map[uuid.UUID]bool, len(dg))
			for _, g := range dg {
				inGroup[g.ID] = true
			}
			if allGroups, err := h.db.ListGroups(ctx); err == nil {
				for _, g := range allGroups {
					if !inGroup[g.ID] {
						addableGroups = append(addableGroups, g)
					}
				}
			}
		}
	})
	run(func() {
		// Couple the device's reported build to a known release (release.version ==
		// device.build_id). nil = the device runs a build with no matching release.
		if device.BuildID != "" {
			release, _ = h.db.GetReleaseByVersion(ctx, device.BuildID, device.Product)
		}
	})
	run(func() { notes, _ = h.db.GetDeviceNotes(ctx, device.ID) })
	run(func() {
		// ResolveUpdateForDevice returns (nil, nil) — not an error — once nothing is
		// pushed/in-flight for this device (excludes status=='installed', and returns
		// no rows at all when there's no active deployment targeting it), so u must be
		// nil-checked too, not just err. That's what makes the device-page OTA card
		// disappear on its own once the update finishes.
		u, err := h.db.ResolveUpdateForDevice(ctx, device.ID)
		if err == nil && u != nil {
			otaUpdate = u
			otaLabel, otaClass, otaPercent = otaStatusView(u.DeviceStatus, h.shell.GetOTAProgress(device.ID))
		}
	})
	run(func() {
		// Charger-fault detection: charging toggles per minute recently. A high rate
		// means the charger/dock connection is dropping in and out (faulty
		// hardware). >10/min is the flapping threshold (matches the
		// charger_flapping alert default).
		flapRate, _ = h.db.DeviceChargerFlapRate(ctx, device.ID, 5)
	})
	// The Alerts tab body (crash cards with full traces — megabytes on a crashy
	// device) is fetched on first open via DeviceAlertsPanel; the page needs only
	// the count for the tab badge.
	run(func() {
		n, err := h.db.CountDeviceCrashes(ctx, device.ID)
		if err == nil {
			mu.Lock()
			crashCount = n
			mu.Unlock()
		}
	})

	wg.Wait()
	dbDur := time.Since(t0)
	if firstErr != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// In-flight installs: install_apk commands not yet completed/failed, so the
	// Applications list can show them as "installing" until the device reports them
	// (skipping any whose app the device already has present).
	pendingInstalls := pendingInstallRows(commands, apps, installedPkgs, apkPkg)

	// Kiosk unlock code (everyone except viewers) — shown inside the Kiosk section so an
	// operator/operator/admin can read it to a technician who needs to leave kiosk on-device.
	// The initial code renders server-side; the page then keeps it live via /offline-code.
	offlineCode, offlineSecs := "", 0
	if kioskCfg.OfflineExitSeed != "" && role != "viewer" {
		now := time.Now()
		offlineCode, _ = totp.Code(kioskCfg.OfflineExitSeed, now, totp.DefaultDigits, totp.DefaultPeriod)
		offlineSecs = totp.SecondsRemaining(now, totp.DefaultPeriod)
	}
	// Count a Maps Embed load whenever this render will actually emit the map iframe
	// (key configured + resolved coordinates present). Browser embed loads are free,
	// but the count lets admins see map activity in the Google APIs usage panel.
	if h.mapsEmbedKey != "" && extraHasCoords(device.LatestExtra) {
		h.mapViews.Add(1)
	}

	// Kiosk locked-app choices: this device's app-drawer apps (launchable, when
	// the client reports it) filtered by the kiosk allowlist (empty allowlist =
	// all apps). Keeps the picker to apps a person could actually be locked into.
	kioskApps := make([]db.DevicePackage, 0, len(installedPkgs))
	for _, p := range launchableOnly(installedPkgs) {
		if h.cfg.KioskAppAllowed(p.PackageName) {
			kioskApps = append(kioskApps, p)
		}
	}

	// Multi-app kiosk: which extra packages are currently allowed, as a set so the
	// modal's checkbox list can mark them checked with a plain index lookup.
	kioskExtras := make(map[string]bool, len(kioskCfg.KioskPackages))
	for _, p := range kioskCfg.KioskPackages {
		kioskExtras[p] = true
	}

	// Server-Timing: visible in the browser's Network panel, so a slow page can be
	// attributed to queries vs. view assembly without log digging.
	w.Header().Set("Server-Timing", fmt.Sprintf("db;dur=%d, build;dur=%d", dbDur.Milliseconds(), (time.Since(t0)-dbDur).Milliseconds()))
	devFams, _, _ := h.libraryData(r.Context())
	h.render(w, r, "device.html", map[string]any{
		"Title":               device.SerialNumber,
		"Device":              device,
		"IsDPC":               device.IsDPC(),
		// Which update path this device is on: a build whose client can't apply an
		// MDM OTA still updates, over the legacy otautil listener.
		"OTAGate":             h.otaGate.Device(r.Context(), *device),
		// Servers to offer when moving a device: this one first, then any peer named
		// in MDM_PEER_URLS (comma separated) — a stage box, usually.
		"MDMServers":          h.mdmServerChoices(),
		"ThisServer":          h.thisServerURL(),
		"Caps":                device.CapSet(),
		"Classes":             product.Classes(),
		"DeviceCrashCount":    crashCount,
		"OfflinePeriod":       totp.DefaultPeriod,
		"OfflineDigits":       totp.DefaultDigits,
		"OfflineCode":         offlineCode,
		"OfflineCodeSecs":     offlineSecs,
		"ChargerFlapRate":     flapRate,
		"ChargerFlapping":     flapRate > 10,
		"Notes":               notes,
		"Release":             release,
		"OtaUpdate":           otaUpdate,
		"OtaLabel":            otaLabel,
		"OtaClass":            otaClass,
		"OtaPercent":          otaPercent,
		"Online":              h.hub.IsConnectedForDisplay(device.ID),
		"ChartCheckins":       chartCheckins,
		// Whether to render any battery UI at all. The product catalog is the first
		// word, but a device can claim a battery its hardware never reports (a kiosk on
		// a legacy or unknown product key), and then every battery surface draws an
		// empty chart and a 0%. So a claimed battery must also show up in telemetry.
		"ShowBattery":         device.HasBattery() && deviceReportsBattery(device, chartCheckins),
		"ChartCharge":         chartCharge,
		"ChartFocus":          focusParam,
		"IsOwner":             h.role(r) == "owner",
		"WhoHasAccess":        h.whoHasAccess(r, device.ID),
		"Nickname":            func() string { m, _ := h.db.GetNicknames(ctx, []uuid.UUID{device.ID}); return m[device.ID] }(),
		"BuildChanges":        buildChanges,
		"Commands":            commands,
		"Queue":               queue,
		"ExtraColumns":        h.cfg.Columns(),
		"Apps":                apps,
		"Families":            devFams,
		"InstalledPackages":   launchableOnly(installedPkgs),
		"KioskApps":           kioskApps,
		"KioskAllowlistCSV":   strings.Join(h.cfg.KioskAllowlist(), ","),
		"KioskExtras":         kioskExtras,
		"PendingInstalls":     pendingInstalls,
		"Uninstalling":        pendingUninstallPkgs(commands),
		"InstalledSet":        pkgNameSet(installedPkgs),
		"KioskConfig":         kioskCfg,
		"WlcApplicable":       h.cfg.WlcApplies(device.ProductKey()),
		"MicGain":             micGainPtr(device.LatestExtra),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ShellEnabled":        h.cfg.ShellEnabled(),
		"RemoteEnabled":       h.cfg.RemoteEnabled(),
		"CanRemote":           h.access(r).canDevice("remote", device.ID),
		"CanShell":            h.access(r).canDevice("shell", device.ID),
		"Restaurants":         restaurants,
		"DeviceGroups":        deviceGroups,
		"AddableGroups":       addableGroups,
		"MapsEmbedKey":        h.mapsEmbedKey,
	})
}

// DeviceOfflineCode returns the current offline-exit unlock code + seconds remaining as
// JSON, so the device page can show a live, self-rotating code without a page reload and
// without needing browser crypto (works over plain HTTP). Admin-only.
func (h *Handler) DeviceOfflineCode(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	cfg, err := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	out := map[string]any{"enabled": false, "period": totp.DefaultPeriod, "kiosk_enabled": cfg.KioskEnabled}
	if cfg.OfflineExitSeed != "" {
		now := time.Now()
		code, _ := totp.Code(cfg.OfflineExitSeed, now, totp.DefaultDigits, totp.DefaultPeriod)
		out["enabled"] = true
		out["code"] = code
		out["seconds"] = totp.SecondsRemaining(now, totp.DefaultPeriod)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// DeviceRotateOfflineCode rotates a device's kiosk unlock seed. Old codes stop working
// immediately; the device picks up the new seed on its next check-in. Admin-only.
func (h *Handler) DeviceRotateOfflineCode(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if _, err := h.db.SetOfflineExit(r.Context(), device.ID, true, "", true); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.offline_code_rotate", serial, "")
	http.Redirect(w, r, "/devices/"+serial, http.StatusSeeOther)
}

func (h *Handler) DeviceRemote(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.RemoteEnabled() {
		http.Error(w, "Remote control is disabled by an administrator.", http.StatusForbidden)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	h.render(w, r, "remote.html", map[string]any{
		"Title":  "Remote — " + device.SerialNumber,
		"Serial": device.SerialNumber,
		"Online": h.hub.IsConnected(device.ID),
		"Device": device,
		// Single-use, short-lived token instead of the admin API key (which must
		// never reach the browser). The control WebSocket redeems it server-side.
		"Token": h.remote.IssueToken(device.ID, 2*time.Minute),
	})
}

// DeviceRemoteToken mints a fresh single-use control-socket token for a remote page
// that is already open. The page's embedded token is consumed by its first connect,
// so every reconnect (and the H.264 -> JPEG fallback) asks here for a new one. Same
// guards as the page itself.
func (h *Handler) DeviceRemoteToken(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.RemoteEnabled() {
		http.Error(w, "Remote control is disabled by an administrator.", http.StatusForbidden)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": h.remote.IssueToken(device.ID, 2*time.Minute)})
}

// DeviceChartData returns a device's battery / temperature / RAM series for an
// on-demand window [from,until] (epoch-ms), so the chart's custom-range and 7d/30d
// controls can pull OLDER history instead of only filtering the ~48h baked into the
// page. Older builds' data was always in the checkins table (battery + temp are
// recorded for every build) — it just was never fetched, so widening the range showed
// nothing before the initial window. Down-sampled to keep the payload small.
// ── Chart window cache ────────────────────────────────────────────────────────
// A 48h pull is thousands of rows scattered across a multi-GB checkins table, so
// it is dominated by random heap reads (measured: 2.4s cold for 4.4k rows, ~0.3s
// warm). The same window is asked for repeatedly — the page's background warm-up,
// then a range click, then anyone else opening the device — so memoise the encoded
// response briefly. Windows are keyed on their endpoints rounded to 30s so the
// warm-up and the click that follows it share one entry.
type chartCacheEntry struct {
	body []byte
	at   time.Time
}

var (
	chartCacheMu sync.Mutex
	chartCache   = map[string]chartCacheEntry{}
)

const (
	chartCacheTTL     = 45 * time.Second
	chartCacheMaxSize = 400
)

func chartCacheKey(deviceID uuid.UUID, fromMs, untilMs int64) string {
	const round = 30_000
	return fmt.Sprintf("%s|%d|%d", deviceID, fromMs/round, untilMs/round)
}

func chartCacheGet(key string) ([]byte, bool) {
	chartCacheMu.Lock()
	defer chartCacheMu.Unlock()
	e, ok := chartCache[key]
	if !ok || time.Since(e.at) > chartCacheTTL {
		return nil, false
	}
	return e.body, true
}

func chartCachePut(key string, body []byte) {
	chartCacheMu.Lock()
	defer chartCacheMu.Unlock()
	if len(chartCache) >= chartCacheMaxSize {
		for k, e := range chartCache { // drop expired first, else start fresh
			if time.Since(e.at) > chartCacheTTL {
				delete(chartCache, k)
			}
		}
		if len(chartCache) >= chartCacheMaxSize {
			chartCache = map[string]chartCacheEntry{}
		}
	}
	chartCache[key] = chartCacheEntry{body: body, at: time.Now()}
}

func (h *Handler) DeviceChartData(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	fromMs, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("from")), 10, 64)
	untilMs, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("until")), 10, 64)
	if untilMs <= 0 {
		untilMs = time.Now().UnixMilli()
	}
	if fromMs <= 0 || fromMs >= untilMs {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	ckey := chartCacheKey(device.ID, fromMs, untilMs)
	if body, ok := chartCacheGet(ckey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return
	}
	checkins, err := h.db.GetCheckinsBetween(r.Context(), device.ID, time.UnixMilli(fromMs), time.UnixMilli(untilMs))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// GetCheckinsBetween returns newest-first; the chart wants oldest-first.
	n := len(checkins)
	asc := make([]db.Checkin, n)
	for i := 0; i < n; i++ {
		asc[i] = checkins[n-1-i]
	}
	// These devices can check in every few seconds, so a multi-day pull is huge.
	// Build every point, then decimate each series to about maxPoints while
	// KEEPING each bucket's minimum and maximum, so a temperature spike or a
	// battery dip that is in the CSV is also on the graph (a plain stride skipped
	// them, which is where "the CSV says 49°C but the graph never shows it" came from).
	const maxPoints = 2500
	type bpt struct {
		X   int64 `json:"x"`
		Y   int   `json:"y"`
		Wlc *int  `json:"wlc"`
	}
	type pt struct {
		X int64   `json:"x"`
		Y float64 `json:"y"`
	}
	hasBattery := device.HasBattery()
	battery := make([]bpt, 0, n)
	temp := make([]pt, 0, n)
	ram := make([]pt, 0, n)
	for i := 0; i < n; i++ {
		c := asc[i]
		x := c.CreatedAt.UnixMilli()
		if hasBattery {
			battery = append(battery, bpt{X: x, Y: c.BatteryPct, Wlc: wlcIntFromExtra(c.Extra)})
		}
		if t, ok := extractBatteryTempC(c.Extra); ok {
			temp = append(temp, pt{X: x, Y: t})
		}
		if rp, ok := ramPctFromExtra(c.Extra); ok {
			ram = append(ram, pt{X: x, Y: rp})
		}
	}
	var charge []chargeRun
	if hasBattery && device.HasCharging() {
		charge = chargeRuns(asc, chartGapMs(device.PollIntervalMs))
	}
	if len(temp) > maxPoints {
		temp = decimateExtremes(temp, maxPoints, func(p pt) float64 { return p.Y })
	}
	if len(ram) > maxPoints {
		ram = decimateExtremes(ram, maxPoints, func(p pt) float64 { return p.Y })
	}
	if len(battery) > maxPoints {
		battery = decimateExtremes(battery, maxPoints, func(p bpt) float64 { return float64(p.Y) })
	}
	body, err := json.Marshal(map[string]any{"battery": battery, "temp": temp, "ram": ram, "charge": charge})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	chartCachePut(ckey, body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// decimateExtremes thins a time-ordered series to about maxPoints by splitting it
// into buckets of equal size and keeping each bucket's first point plus its minimum
// and maximum (in time order), so peaks and dips survive the thinning.
func decimateExtremes[T any](pts []T, maxPoints int, y func(T) float64) []T {
	n := len(pts)
	if n <= maxPoints || maxPoints < 3 {
		return pts
	}
	bucket := (n + maxPoints/3 - 1) / (maxPoints / 3)
	out := make([]T, 0, maxPoints+3)
	for i := 0; i < n; i += bucket {
		j := i + bucket
		if j > n {
			j = n
		}
		lo, hi := i, i
		for k := i + 1; k < j; k++ {
			if y(pts[k]) < y(pts[lo]) {
				lo = k
			}
			if y(pts[k]) > y(pts[hi]) {
				hi = k
			}
		}
		idx := []int{i}
		for _, k := range []int{lo, hi} {
			dup := false
			for _, e := range idx {
				if e == k {
					dup = true
				}
			}
			if !dup {
				idx = append(idx, k)
			}
		}
		sort.Ints(idx)
		for _, k := range idx {
			out = append(out, pts[k])
		}
	}
	return out
}

// wlcIntFromExtra returns the wireless-charging status (0..2) from a check-in's extra,
// or nil when absent/out of range — matching the wlcStatus template helper.
func wlcIntFromExtra(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	v, ok := m["wlc_status"]
	if !ok {
		return nil
	}
	var n int
	if json.Unmarshal(v, &n) != nil || n < 0 || n > 2 {
		return nil
	}
	return &n
}

// chargeRun is one span of a device's charging state for the battery chart's charging
// strip: C is nil where the check-ins carried no "charging" key. From/To are the first
// and last check-in in the span.
type chargeRun struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	C    *bool `json:"c"`
}

// chargingFromExtra returns a check-in's "charging" flag, or nil when absent.
func chargingFromExtra(raw json.RawMessage) *bool {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	v, ok := m["charging"]
	if !ok {
		return nil
	}
	var b bool
	if json.Unmarshal(v, &b) != nil {
		return nil
	}
	return &b
}

// chargeRuns folds oldest-first check-ins into charging-state spans. It runs over
// every check-in, before the battery series is thinned, so a charger flip between two
// kept samples still lands where it happened. A silence longer than gapMs ends a span,
// matching where the battery line breaks (the chart's gapThresholdMs).
func chargeRuns(asc []db.Checkin, gapMs int64) []chargeRun {
	runs := []chargeRun{}
	for _, c := range asc {
		x := c.CreatedAt.UnixMilli()
		st := chargingFromExtra(c.Extra)
		if n := len(runs); n > 0 {
			last := &runs[n-1]
			same := (last.C == nil && st == nil) || (last.C != nil && st != nil && *last.C == *st)
			if same && x-last.To <= gapMs {
				last.To = x
				continue
			}
		}
		runs = append(runs, chargeRun{From: x, To: x, C: st})
	}
	return runs
}

// chartGapMs mirrors device.html's gapThresholdMs: silence past max(poll×10, 15 min)
// is the device being dark, not batching.
func chartGapMs(pollIntervalMs int) int64 {
	poll := int64(pollIntervalMs)
	if poll <= 0 {
		poll = 30000
	}
	if g := poll * 10; g > 900000 {
		return g
	}
	return 900000
}

// ramPctFromExtra computes RAM used% from a check-in's extra, matching extraRamPct.
func ramPctFromExtra(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	v, ok := m["ram_usage_mb"]
	if !ok {
		return 0, false
	}
	var ram map[string]int
	if json.Unmarshal(v, &ram) != nil {
		return 0, false
	}
	total := ram["total"]
	if total == 0 {
		return 0, false
	}
	pct := float64(ram["used"]) * 100 / float64(total)
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

func (h *Handler) DeviceHistory(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	const pageSize = 25
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	offset := (page - 1) * pageSize

	total, err := h.db.GetCheckinsCount(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
		offset = (page - 1) * pageSize
	}

	checkins, err := h.db.GetCheckinsPaged(r.Context(), device.ID, pageSize, offset)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), commands)
	commands = filterShellDeviceCommands(h.role(r), commands)

	h.render(w, r, "device_history.html", map[string]any{
		"Title":        device.SerialNumber + " — History",
		"Device":       device,
		"Commands":     commands,
		"Checkins":     checkins,
		"ExtraColumns": h.cfg.Columns(),
		"CheckinPage":  page,
		"CheckinPages": totalPages,
		"CheckinTotal": total,
	})
}

// DeviceOnlineStatus returns a tiny HTML fragment with the current connection state.
// Kept for initial page render; live updates come from DevicePresenceStream.
func (h *Handler) DeviceOnlineStatus(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	online := h.hub.IsConnectedForDisplay(device.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if online {
		fmt.Fprint(w, `<span id="ws-badge" class="ws-badge online" title="WebSocket connected"><span class="ws-dot"></span>online</span>`)
	} else {
		fmt.Fprint(w, `<span id="ws-badge" class="ws-badge offline" title="No WebSocket connection"><span class="ws-dot"></span>offline</span>`)
	}
}

// DevicePresenceStream streams SSE events for a single device's connection state.
// Emits an initial snapshot, then pushes "online"/"offline" on change.
func (h *Handler) DevicePresenceStream(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		fmt.Fprintf(w, "event: presence\ndata: %s\n\n", state)
		flusher.Flush()
	}

	writeEvent(h.hub.IsConnectedForDisplay(device.ID))

	sub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// A real disconnect doesn't push "offline" straight away — it arms a
	// presenceGrace timer instead, so a brief blip never flashes the badge.
	// Reconnecting within the window cancels the timer. Going offline pushes
	// only if the device is still disconnected once the timer fires.
	var offlineTimer *time.Timer
	fireOffline := make(chan struct{}, 1)
	defer func() {
		if offlineTimer != nil {
			offlineTimer.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if ev.DeviceID != device.ID {
				continue
			}
			if ev.Online {
				if offlineTimer != nil {
					offlineTimer.Stop()
					offlineTimer = nil
				}
				writeEvent(true)
			} else if offlineTimer == nil {
				offlineTimer = time.AfterFunc(ws.PresenceGrace, func() {
					select {
					case fireOffline <- struct{}{}:
					default:
					}
				})
			}
		case <-fireOffline:
			offlineTimer = nil
			if !h.hub.IsConnected(device.ID) {
				writeEvent(false)
			}
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// FleetEvents streams per-device SSE updates for the fleet dashboard.
// Each event carries a JSON payload so the browser can patch only the affected row.
func (h *Handler) FleetEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	activeThreshold := time.Duration(h.cfg.CheckinInterval()*3) * time.Second

	acc := h.access(r)
	sendRow := func(deviceID uuid.UUID) {
		if !acc.visible(deviceID) {
			return
		}
		dev, err := h.db.GetDeviceByID(r.Context(), deviceID)
		if err != nil {
			return
		}
		row := deviceToRowJSON(*dev, h.hub.IsConnectedForDisplay(deviceID), activeThreshold)
		// The live row-patch repaints the battery chip, so it must carry the flap state
		// too — otherwise it overwrites the server-rendered fault glyph with a plain bolt.
		if rate, _ := h.db.DeviceChargerFlapRate(r.Context(), deviceID, 5); rate > 10 {
			row.Flapping, row.FlapRate = true, rate
		}
		b, err := json.Marshal(row)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: device-update\ndata: %s\n\n", b)
		flusher.Flush()
	}

	presenceSub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(presenceSub)
	updateSub := h.hub.SubscribeDeviceUpdates()
	defer h.hub.UnsubscribeDeviceUpdates(updateSub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// A row's own SSE patch reflects IsConnectedForDisplay already, so a disconnect
	// event itself is harmless (the grace window holds it "online"). But nothing
	// else re-patches that row once the grace window lapses, so schedule one
	// grace-delayed re-send per device that goes offline, to actually flip it.
	offlineTimers := map[uuid.UUID]*time.Timer{}
	fireOffline := make(chan uuid.UUID, 64)
	defer func() {
		for _, t := range offlineTimers {
			t.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-presenceSub:
			if !ok {
				return
			}
			sendRow(ev.DeviceID)
			if ev.Online {
				if t, ok := offlineTimers[ev.DeviceID]; ok {
					t.Stop()
					delete(offlineTimers, ev.DeviceID)
				}
			} else if _, ok := offlineTimers[ev.DeviceID]; !ok {
				id := ev.DeviceID
				offlineTimers[id] = time.AfterFunc(ws.PresenceGrace, func() {
					select {
					case fireOffline <- id:
					default:
					}
				})
			}
		case id := <-fireOffline:
			delete(offlineTimers, id)
			sendRow(id)
		case ev, ok := <-updateSub:
			if !ok {
				return
			}
			sendRow(ev.DeviceID)
		}
	}
}

type deviceEventPayload struct {
	TsMs       int64    `json:"ts_ms"`
	BuildID    string   `json:"build_id,omitempty"`
	BatteryPct int      `json:"battery_pct"`
	Wlc        *int     `json:"wlc"`      // nil = no data
	TempC      *float64 `json:"temp_c"`   // nil = no data
	RamPct     *float64 `json:"ram_pct"`  // nil = no data
	Charging   *bool    `json:"charging"` // nil = no data
	ScreenOn   *bool    `json:"screen_on"` // nil = firmware does not report it
	Latitude   *float64 `json:"latitude,omitempty"`
	Longitude  *float64 `json:"longitude,omitempty"`
	// KioskEnabled is the device's current kiosk state (from device_config, not the
	// checkin) so the device page reflects an on-device offline exit live. Pointer so
	// it is only sent when the SSE writer looked it up.
	KioskEnabled *bool `json:"kiosk_enabled,omitempty"`
}

func buildDeviceEventPayload(c *db.Checkin) deviceEventPayload {
	p := deviceEventPayload{
		TsMs:       c.CreatedAt.UnixMilli(),
		BuildID:    c.BuildID,
		BatteryPct: c.BatteryPct,
	}
	if len(c.Extra) > 0 {
		var extra map[string]json.RawMessage
		if json.Unmarshal(c.Extra, &extra) == nil {
			if v, ok := extra["wlc_status"]; ok {
				var n int
				if json.Unmarshal(v, &n) == nil {
					p.Wlc = &n
				}
			}
			if v, ok := extra["charging"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					p.Charging = &b
				}
			}
			if v, ok := extra["screen_on"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					p.ScreenOn = &b
				}
			}
			if v, ok := extra["ram_usage_mb"]; ok {
				var ram map[string]int
				if json.Unmarshal(v, &ram) == nil && ram["total"] > 0 {
					pct := float64(ram["used"]) * 100 / float64(ram["total"])
					if pct > 100 {
						pct = 100
					}
					p.RamPct = &pct
				}
			}
			if v, ok := extra["latitude"]; ok {
				var lat float64
				if json.Unmarshal(v, &lat) == nil {
					p.Latitude = &lat
				}
			}
			if v, ok := extra["longitude"]; ok {
				var lon float64
				if json.Unmarshal(v, &lon) == nil {
					p.Longitude = &lon
				}
			}
		}
	}
	if temp, ok := extractBatteryTempC(c.Extra); ok {
		p.TempC = &temp
	}
	return p
}

// CommandEvents streams SSE notifications for a command detail page.
func (h *Handler) CommandEvents(w http.ResponseWriter, r *http.Request) {
	cmdID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid command id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeCommandUpdates()
	defer h.hub.UnsubscribeCommandUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if ev.CommandID == cmdID {
				fmt.Fprint(w, "event: command-update\ndata: refresh\n\n")
				flusher.Flush()
			}
		}
	}
}

// CommandsFeedEvents streams SSE notifications for the action history page: it
// fires on every command update (no per-id filter), so an in-flight action that
// completes moves from "In progress" to "Completed" without a manual refresh —
// the same live behavior the device page has for its command list.
func (h *Handler) CommandsFeedEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeCommandUpdates()
	defer h.hub.UnsubscribeCommandUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: command-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// AlertEvents streams SSE notifications to the alerts page so an ack/resolve by
// any user refreshes every open alerts view in real time.
func (h *Handler) AlertEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeAlertUpdates()
	defer h.hub.UnsubscribeAlertUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()

	// emitCount sends the current open-alert count so every connected client (the
	// global nav badge and the alerts list) can update without each re-querying.
	emitCount := func() {
		n, err := h.db.CountOpenAlerts(ctx)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: alert-update\ndata: %d\n\n", n)
		flusher.Flush()
	}

	// Send the count once on connect so a freshly-loaded page syncs its badge
	// immediately rather than waiting for the next change.
	emitCount()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			emitCount()
		}
	}
}

// ReportAlertsByRestaurant renders the Daily Report's per-restaurant open-alert
// breakdown as an htmx fragment. The card re-fetches it on every mdm:alerts-update
// (fired by the global /alerts/events stream) so the breakdown tracks new, acked,
// and resolved alerts live without its own SSE connection.
func (h *Handler) ReportAlertsByRestaurant(w http.ResponseWriter, r *http.Request) {
	if h.access(r).hidesDevices() {
		http.Error(w, "This report covers the whole fleet.", http.StatusForbidden)
		return
	}
	rows, err := h.db.AlertsByRestaurant(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	total := 0
	for _, ra := range rows {
		total += ra.Total
	}
	// Cap the visible list; the header link covers the rest.
	shown := rows
	more := 0
	if len(shown) > 8 {
		more = len(shown) - 8
		shown = shown[:8]
	}
	h.tmpl.ExecuteTemplate(w, "report-alerts-by-restaurant", map[string]any{
		"Rows": shown, "Total": total, "More": more,
	})
}

// DeploymentEvents streams SSE notifications to open deployment pages so OTA
// progress and operator actions (retry/cancel/edit) refresh live without polling.
func (h *Handler) DeploymentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeDeploymentUpdates()
	defer h.hub.UnsubscribeDeploymentUpdates(sub)
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: deployment-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// ReleaseProblemEvents streams SSE notifications to the releases hub and any open
// release workspace, so the cross-release problems board and the workspace fragments
// refresh live when a problem (or a QA result that creates/resolves one) changes
// anywhere — no polling. Payload-less; each listener re-fetches its own scoped fragment.
func (h *Handler) ReleaseProblemEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeProblemUpdates()
	defer h.hub.UnsubscribeProblemUpdates(sub)
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: problem-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// DeviceEvents streams SSE notifications for a single device detail page.
func (h *Handler) DeviceEvents(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writePresence := func(online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		fmt.Fprintf(w, "event: presence\ndata: %s\n\n", state)
		flusher.Flush()
	}
	writeDeviceUpdate := func() {
		c, err := h.db.GetLatestCheckin(r.Context(), device.ID)
		if err != nil {
			fmt.Fprint(w, "event: device\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		p := buildDeviceEventPayload(c)
		// Carry live kiosk state so an on-device offline exit flips the toggle here
		// without a reload (kiosk_enabled lives in device_config, not the checkin).
		if cfg, cerr := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID); cerr == nil {
			ke := cfg.KioskEnabled
			p.KioskEnabled = &ke
		}
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "event: device\ndata: %s\n\n", b)
		flusher.Flush()
	}

	writePresence(h.hub.IsConnectedForDisplay(device.ID))
	writeDeviceUpdate()

	presenceSub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(presenceSub)
	updateSub := h.hub.SubscribeDeviceUpdates()
	defer h.hub.UnsubscribeDeviceUpdates(updateSub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// Same grace-debounce as DevicePresenceStream: a real disconnect arms a
	// presenceGrace timer instead of pushing "offline" straight away.
	var offlineTimer *time.Timer
	fireOffline := make(chan struct{}, 1)
	defer func() {
		if offlineTimer != nil {
			offlineTimer.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-presenceSub:
			if !ok {
				return
			}
			if ev.DeviceID == device.ID {
				if ev.Online {
					if offlineTimer != nil {
						offlineTimer.Stop()
						offlineTimer = nil
					}
					writePresence(true)
				} else if offlineTimer == nil {
					offlineTimer = time.AfterFunc(ws.PresenceGrace, func() {
						select {
						case fireOffline <- struct{}{}:
						default:
						}
					})
				}
			}
		case <-fireOffline:
			offlineTimer = nil
			if !h.hub.IsConnected(device.ID) {
				writePresence(false)
			}
		case ev, ok := <-updateSub:
			if !ok {
				return
			}
			if ev.DeviceID == device.ID {
				writeDeviceUpdate()
			}
		}
	}
}

func wlcStatusFromExtra(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m["wlc_status"]
	if !ok {
		return ""
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return ""
	}
	switch n {
	case 0:
		return "not_charging"
	case 1:
		return "charging"
	case 2: // gpio27 kept disagreeing instead of settling — a faulty/loose pad, not a fresh guest
		return "pad_flapping"
	case -1: // the sysfs read itself failed; the pad's actual state is unknown
		return "pad_unreadable"
	default:
		return ""
	}
}

func (h *Handler) DeviceBatteryCSV(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	hours := 48
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	checkins, err := h.db.GetCheckinsForDuration(r.Context(), device.ID, time.Now().UTC().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("%s_battery_%dh.csv", serial, hours)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	cw.Write([]string{"timestamp", "battery_pct", "wlc_status"})
	for i := len(checkins) - 1; i >= 0; i-- {
		c := checkins[i]
		cw.Write([]string{
			c.CreatedAt.Format(time.RFC3339),
			strconv.Itoa(c.BatteryPct),
			wlcStatusFromExtra(c.Extra),
		})
	}
	cw.Flush()
}

// DeviceDailyStatsJSON returns the device's rolled-up daily stats as JSON, oldest
// first. ?days=N selects the window (default 30, capped at 365). Drives the Trends tab.
func (h *Handler) DeviceDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}

	stats, err := h.db.GetDeviceDailyStats(r.Context(), device.ID, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.DeviceDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// GroupDailyStatsJSON returns daily stats aggregated across all devices in a group as
// JSON, oldest first. ?days=N selects the window (default 30, capped at 365).
func (h *Handler) GroupDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}

	stats, err := h.db.GetGroupDailyStats(r.Context(), id, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.GroupDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// AlertList renders the alerts page, optionally filtered by ?status=open|acknowledged|resolved.
func (h *Handler) AlertList(w http.ResponseWriter, r *http.Request) {
	// The inbox has one segmented control: All / Needs action / Watching. Resolved
	// history is intentionally not a view here — the inbox is about what's active.
	view := r.URL.Query().Get("view")
	switch view {
	case "", "needs", "watching", "crashes":
	default:
		view = ""
	}
	summary, err := h.db.AlertSummaryCounts(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	role := h.role(r)
	canAct := roleCanOperate(role)

	// Optional per-device filter (a device's Alerts-tab "Show more" links here so the
	// full list opens scoped to that unit). Resolve the serial to an ID; an unknown
	// serial simply yields an empty, clearly-labelled list rather than the whole fleet.
	deviceSerial := strings.TrimSpace(r.URL.Query().Get("device"))
	var deviceID *uuid.UUID
	if deviceSerial != "" {
		if dev, err := h.db.GetDevice(r.Context(), deviceSerial); err == nil {
			id := dev.ID
			deviceID = &id
		} else {
			id := uuid.Nil // no such device: force an empty result set
			deviceID = &id
		}
	}

	var active []db.Alert
	if deviceID != nil {
		active, err = h.db.ListDeviceActiveAlerts(r.Context(), *deviceID, 150)
	} else {
		active, err = h.db.ListActiveAlerts(r.Context(), 150)
		active = h.access(r).keepVisibleAlerts(active)
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var crit, watch []humanAlert
	for _, a := range active {
		ha := humanizeAlert(a)
		ha.CanAct = canAct
		// Crash/ANR alerts carry the real diagnostic: attach the latest stored stack
		// trace so it renders inline on the card (same as the release problems page),
		// instead of linking out to the old logcat capture.
		if a.Type == "device_crash" && a.DeviceID != nil {
			if trace, ok, _ := h.db.LatestCrashTrace(r.Context(), *a.DeviceID); ok {
				ha.Trace = trace
			}
		}
		if a.Severity == "critical" {
			crit = append(crit, ha)
		} else {
			watch = append(watch, ha)
		}
	}
	// Crash events (device_events) are separate from alert rows — crash alerts
	// auto-resolve, so past crashes vanish from the active-alert list. Fold the raw
	// crash feed in as its own paginated view so "View all crashes" actually shows them.
	const crashPageSize = 25
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	crashEvents, crashTotal, _ := h.db.ListRecentCrashGroupsPage(r.Context(), deviceID, 7, crashPageSize, (page-1)*crashPageSize)
	crashEventTotal, _ := h.db.CountRecentCrashEvents(r.Context(), deviceID, 7)
	// Clamp a past-the-end page back to the last real page so a stale ?page= link
	// lands on content, not an empty list.
	crashPages := (crashTotal + crashPageSize - 1) / crashPageSize
	if crashPages > 0 && page > crashPages {
		page = crashPages
		crashEvents, crashTotal, _ = h.db.ListRecentCrashGroupsPage(r.Context(), deviceID, 7, crashPageSize, (page-1)*crashPageSize)
	}
	crashes := toCrashCards(crashEvents)
	h.resolveAppIcons(r.Context(), [][]humanAlert{crit, watch}, crashes)
	// Fold each bucket by (type, site) AFTER icons are resolved, so a group's lead
	// card keeps the icon its members resolved.
	critGroups, watchGroups := groupAlerts(crit), groupAlerts(watch)
	// Preserve the device scope on the pager links.
	crashPageBase := "/alerts?view=crashes"
	if deviceSerial != "" {
		crashPageBase += "&device=" + url.QueryEscape(deviceSerial)
	}
	h.render(w, r, "alerts.html", map[string]any{
		"Title":         "Alerts",
		"Summary":       summary,
		"View":          view,
		"Critical":      critGroups,
		"Watching":      watchGroups,
		// Counts stay per-device: an operator wants "14 devices need attention", not
		// "1 group". Only the rendering folds.
		"NeedsCount":    len(crit),
		"WatchCount":    len(watch),
		"ActiveCount":   len(crit) + len(watch),
		"Crashes":       crashes,
		// The KPI counts crashes; the list below counts distinct crashes. Both are shown
		// because "3 signatures" and "212 crashes" answer different questions.
		"CrashCount":    crashEventTotal,
		"CrashGroups":   crashTotal,
		"DeviceFilter":  deviceSerial,
		"CrashPage":     page,
		"CrashPages":    crashPages,
		"CrashPageBase": crashPageBase,
	})
}

// alertGroup is one rule firing across one site, folded into a single row. The
// crash feed has merged by signature for a while (ListRecentCrashGroupsPage); the
// alert list did not, so one site-wide cause — fourteen T7s on the same weak
// circuit tripping slow_charge_night — arrived as fourteen identical cards and
// buried everything else. The unique index is (type, device_id), so the dedup that
// already exists can only ever collapse repeats on ONE device.
//
// Lead is the newest member and renders the collapsed row; Members carries every
// alert so the expander can list each device with its own sentence and its own
// ack/resolve buttons (there is no bulk-resolve endpoint, and inventing one here
// would be a bigger change than the noise warrants).
type alertGroup struct {
	Lead        humanAlert
	Members     []humanAlert
	DeviceCount int
	Occurrences int    // summed across members
	Search      string // every member's searchable text, so the client filter still finds a folded device
}

// groupAlerts folds a severity bucket by (type, restaurant). Order is preserved:
// a group lands where its newest member sat, so the existing severity/fired_at
// ordering from ListActiveAlerts still drives the page. A lone alert becomes a
// one-member group and renders exactly as it did before.
//
// Restaurant is part of the key on purpose. "Fourteen devices at Flights Vegas
// charged slowly" is one fact an operator can act on; the same rule firing at four
// different sites is four separate problems and stays four rows.
func groupAlerts(alerts []humanAlert) []alertGroup {
	groups := make([]alertGroup, 0, len(alerts))
	idx := make(map[string]int, len(alerts))
	for _, a := range alerts {
		// Crash alerts carry a per-device stack trace and app icon, so folding them
		// would hide the one thing that makes them useful. The crashes view already
		// groups them by signature.
		key := a.Type + "\x00" + a.Restaurant
		if a.Type == "device_crash" || a.Type == "" {
			key = "\x00unique\x00" + a.ID.String()
		}
		if i, ok := idx[key]; ok {
			g := &groups[i]
			g.Members = append(g.Members, a)
			g.DeviceCount++
			g.Occurrences += a.Occurrences
			g.Search += " " + a.Serial
			continue
		}
		idx[key] = len(groups)
		groups = append(groups, alertGroup{
			Lead: a, Members: []humanAlert{a}, DeviceCount: 1,
			Occurrences: a.Occurrences,
			Search:      a.Headline + " " + a.Serial + " " + a.Restaurant + " " + plainSentence(a.Sentence),
		})
	}
	return groups
}

// crashCardView is one crash/ANR/tombstone event rendered as a card on the alerts
// page and the device Alerts tab: a kind badge, device, build and relative time,
// with the full trace tucked into an expander.
type crashCardView struct {
	Serial     string
	Restaurant string
	KindLabel  string
	KindClass  string
	BuildID    string
	OccurredAt time.Time
	Summary    string
	Trace      string
	PackageName string // app package parsed from Summary, if any
	AppIcon    string // base64 PNG from the App library, resolved below
	// Merged-signature counts: how many devices hit this crash and how many times in
	// total. Zero when the row is a single raw event.
	DeviceCount int
	EventCount  int
}

// mustCrashes drops the error from a crash-event query — a failed crash lookup on a
// device page should degrade to an empty tab, not a 500.
func mustCrashes(events []db.CrashEvent, _ error) []db.CrashEvent { return events }

// toCrashCards maps raw crash events to their view rows, deriving the kind badge.
func toCrashCards(events []db.CrashEvent) []crashCardView {
	cards := make([]crashCardView, 0, len(events))
	for _, e := range events {
		label, class := crashKindBadge(e.Kind)
		cards = append(cards, crashCardView{
			Serial:     e.Serial,
			Restaurant: e.Restaurant,
			KindLabel:  label,
			KindClass:  class,
			BuildID:    e.BuildID,
			OccurredAt: e.OccurredAt,
			Summary:    e.Summary,
			Trace:      e.Detail,
			PackageName: extractPackageName(e.Summary),
			DeviceCount: e.DeviceCount,
			EventCount:  e.EventCount,
		})
	}
	return cards
}

// resolveAppIcons resolves each device_crash alert's / crash card's app icon from the
// App library's shared app_icons index in one batch query, so crash/ANR cards show the
// real app icon instead of the generic crash glyph wherever a package name was parsed
// out of the summary. Mutates alertGroups and crashes in place.
func (h *Handler) resolveAppIcons(ctx context.Context, alertGroups [][]humanAlert, crashes []crashCardView) {
	pkgSet := map[string]struct{}{}
	for _, group := range alertGroups {
		for _, ha := range group {
			if ha.PackageName != "" {
				pkgSet[ha.PackageName] = struct{}{}
			}
		}
	}
	for _, c := range crashes {
		if c.PackageName != "" {
			pkgSet[c.PackageName] = struct{}{}
		}
	}
	if len(pkgSet) == 0 {
		return
	}
	pkgs := make([]string, 0, len(pkgSet))
	for pkg := range pkgSet {
		pkgs = append(pkgs, pkg)
	}
	icons, err := h.db.GetAppIconsByPackages(ctx, pkgs)
	if err != nil {
		return
	}
	for _, group := range alertGroups {
		for i := range group {
			group[i].AppIcon = icons[group[i].PackageName]
		}
	}
	for i := range crashes {
		crashes[i].AppIcon = icons[crashes[i].PackageName]
	}
}

// AlertNewest returns the single most urgent active alert in friendly form as JSON,
// for the toast that pops when a new critical alert arrives.
func (h *Handler) AlertNewest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	alerts, _ := h.db.ListActiveAlerts(r.Context(), 20)
	alerts = h.access(r).keepVisibleAlerts(alerts)
	if len(alerts) == 0 {
		w.Write([]byte("null"))
		return
	}
	ha := humanizeAlert(alerts[0])
	href := ""
	if ha.Primary != nil {
		href = ha.Primary.Href
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       ha.ID.String(),
		"severity": ha.Severity,
		"headline": ha.Headline,
		"sentence": plainSentence(ha.Sentence),
		"href":     href,
	})
}

// AlertsRecent renders a compact list of the latest open alerts for the top-bar
// bell dropdown (lazy-loaded via htmx when the dropdown opens).
func (h *Handler) AlertsRecent(w http.ResponseWriter, r *http.Request) {
	alerts, err := h.db.ListActiveAlerts(r.Context(), 30)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if alerts = h.access(r).keepVisibleAlerts(alerts); len(alerts) > 6 {
		alerts = alerts[:6]
	}
	h.tmpl.ExecuteTemplate(w, "alerts-recent", map[string]any{"Alerts": humanizeAll(alerts)})
}

// AlertBulk applies an action (acknowledge|resolve) to the alert IDs selected via
// checkboxes on the alerts page.
func (h *Handler) AlertBulk(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	r.ParseForm()
	var status string
	switch r.FormValue("action") {
	case "acknowledge":
		status = "acknowledged"
	case "resolve":
		status = "resolved"
	default:
		h.hxDone(w, r, "/alerts")
		return
	}
	var ids []uuid.UUID
	for _, s := range r.Form["ids"] {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		if _, err := h.db.BulkSetAlertStatusByIDs(r.Context(), ids, status); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "alert."+status+"_selected", "", strconv.Itoa(len(ids)))
		h.hub.PublishAlertUpdate()
	}
	h.hxDone(w, r, "/alerts")
}

// AlertAck marks an alert acknowledged. AlertResolve resolves it.
func (h *Handler) AlertAck(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.setAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolve(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.setAlertStatus(w, r, "resolved")
}

func (h *Handler) setAlertStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}
	if err := h.db.SetAlertStatus(r.Context(), id, status); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert."+status, id.String(), "")
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

// AlertAckAll acknowledges every open alert; AlertResolveAll resolves every
// non-resolved alert.
func (h *Handler) AlertAckAll(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.bulkAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolveAll(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.bulkAlertStatus(w, r, "resolved")
}

// AlertClearAll empties the list: every open alert is resolved and muted for 24 hours.
// It is no longer a delete — see db.DeleteAllAlerts for why deleting achieved nothing.
func (h *Handler) AlertClearAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.db.DeleteAllAlerts(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert.clear_all", "", strconv.FormatInt(n, 10))
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

func (h *Handler) bulkAlertStatus(w http.ResponseWriter, r *http.Request, status string) {
	n, err := h.db.BulkSetAlertStatus(r.Context(), status)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert."+status+"_all", "", strconv.FormatInt(n, 10))
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

// FleetHealth renders the Tier 3 fleet/group health overview: a fleet summary plus a
// per-group scorecard ranked worst-first.
// FleetHealth is the detailed-analysis page: the cached fleet report on top, then —
// per signal category — the actual open alerts that back it (serial linked to the
// device page, fired-at, and the value-rich summary), then the per-group scorecard.
// Every serial/timestamp/value in the evidence comes straight from the alerts table,
// so the page can never cite a device the model invented. The "Detailed analysis"
// button on the hourly-report card points here.
func (h *Handler) FleetHealth(w http.ResponseWriter, r *http.Request) {
	// Scorecard window: 7 days by default, or 1 day (today vs yesterday) via ?days=1.
	windowDays := 7
	if r.URL.Query().Get("days") == "1" {
		windowDays = 1
	}
	if h.access(r).hidesDevices() {
		http.Redirect(w, r, "/devices", http.StatusFound)
		return
	}
	groups, err := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), windowDays)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	summary, _ := h.db.GetSummary(r.Context(), h.connectedSlice())
	openAlerts, _ := h.db.CountOpenAlerts(r.Context())
	alerts, _ := h.db.ListAlerts(r.Context(), "open", 500)
	serials, _ := h.db.ListAllSerials(r.Context())

	// Fleet score: device-weighted mean of the per-restaurant scores (same basis as
	// the overview ring), so the hero number agrees across pages.
	score := 100
	if summary.Total > 0 {
		num, den := 0, 0
		for _, g := range groups {
			num += g.Score * g.DeviceCount
			den += g.DeviceCount
		}
		if den > 0 {
			score = num / den
		} else {
			// No venue has devices yet (or no venues at all): fall back to the
			// online-ratio proxy so the ring still reflects reality.
			score -= 40 * (summary.Total - summary.RecentlyActive) / summary.Total
		}
	}
	scoreClass := "danger"
	switch {
	case score >= 80:
		scoreClass = "ok"
	case score >= 50:
		scoreClass = "warn"
	}
	// Restaurants needing a look = anything not scoring "ok" (or with a device).
	attention := 0
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass != "ok" {
			attention++
		}
	}
	verdict := "All restaurants healthy"
	if attention == 1 {
		verdict = "1 restaurant needs a look"
	} else if attention > 1 {
		verdict = fmt.Sprintf("%d restaurants need a look", attention)
	}

	hot := hotSerialsFromHealth(groups, h.alertThresholds(r.Context()).TempC)
	healthy := 0
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass == "ok" {
			healthy++
		}
	}

	// Crash surfaces: 24h rollup (signal tile + hero facts), the worst devices for
	// the crashes panel, a 7-day sparkline, and per-restaurant counts for the chips.
	crashStats, _ := h.db.GetFleetCrashStats(r.Context(), 6)
	crashRows := make([]crashRow, 0, len(crashStats.ByDevice))
	for _, c := range crashStats.ByDevice {
		label, class := crashKindBadge(c.Kind)
		crashRows = append(crashRows, crashRow{
			Serial: c.Serial, Restaurant: c.Restaurant,
			KindLabel: label, KindClass: class,
			BuildID: c.BuildID, Ago: agoShort(c.LatestAt), Count: c.Count,
		})
	}
	// Memory-pressure signal count (open alerts of that type).
	memory := 0
	for _, a := range alerts {
		if a.Type == "memory_pressure" {
			memory++
		}
	}
	// Triage list is worst-first (GetRestaurantHealth returns name order).
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Score < groups[j].Score })
	statusWord := map[string]string{"ok": "Healthy", "warn": "Watch", "danger": "Critical"}[scoreClass]

	data := map[string]any{
		"Title":           "Fleet Health",
		"Groups":          groups,
		"TotalDevices":    summary.Total,
		"OnlineDevices":   summary.RecentlyActive,
		"OfflineDevices":  summary.Total - summary.RecentlyActive,
		"LowBattery":      summary.LowBattery,
		"HotCount":        len(hot),
		"HealthyCount":    healthy,
		"OpenAlerts":      openAlerts,
		"UniqueBuilds":    summary.UniqueBuilds,
		"Evidence":        groupAlertsBySignal(alerts),
		"OpenAlertsCount": len(alerts),
		"DeviceSerials":   serialsJSON(serials),
		"HotSerials":      serialsJSON(hot),
		"WindowDays":      windowDays,
		"Score":           score,
		"HealthReasons":   explainFleetHealth(groups, crashStats.ByRestaurant),
		"ScoreClass":      scoreClass,
		// 2π·r40 = 251.3; the hero ring animates to this offset.
		"RingOffset": fmt.Sprintf("%.1f", 251.3*float64(100-score)/100),
		"Verdict":    verdict,
		"Attention":  attention,
		"StatusWord": statusWord,
		// Crash surfaces.
		"Crashes24h":        crashStats.Total24h,
		"CrashDevices24h":   crashStats.Devices24h,
		"CrashWorstBuild":   crashStats.WorstBuild,
		"CrashWorstBuildN":  crashStats.WorstBuildN,
		"CrashList":         crashRows,
		"CrashSpark":        crashSparkBars(crashStats.Daily),
		"RestaurantCrashes": crashStats.ByRestaurant,
		"MemoryCount":       memory,
	}
	// Fleet-wide 14-day series backing the report's evidence charts.
	if d14, err := h.db.GetFleetDailyStats(r.Context(), 14); err == nil {
		data["SeriesJSON"] = fleetSeriesJSON(d14, crashStats.Daily, summary.Total)
	}
	// Show the same cached fleet report as the main page (latest of hourly or manual).
	if s, err := h.db.GetAISummary(r.Context(), "fleet"); err == nil && s.Summary != "" {
		data["AISummary"] = s.Summary
		data["AISummaryAt"] = s.GeneratedAt.UTC().Format(time.RFC3339)
		data["AISummaryPreview"] = summarizePreview(s.Summary)
		data["ReportRestaurants"] = restaurantLinksJSON(groups)
	}
	h.render(w, r, "health.html", data)
}

// fleetSeriesJSON packs the 14-day fleet series (active / low battery / hot per
// day, battery average) plus crashes per day for the report evidence charts.
func fleetSeriesJSON(d14 []db.FleetDailyStat, crashes []int, total int) template.JS {
	type pt struct {
		Day    string   `json:"day"`
		Active int      `json:"active"`
		Low    int      `json:"low"`
		Hot    int      `json:"hot"`
		Batt   *float32 `json:"batt"`
	}
	series := make([]pt, 0, len(d14))
	for _, ds := range d14 {
		series = append(series, pt{ds.Day.Format("Jan 2"), ds.Active, ds.LowBattery, ds.Hot, ds.BatteryAvg})
	}
	b, err := json.Marshal(map[string]any{"days": series, "crashes": crashes, "total": total})
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b)
}

// reportSignals maps the fleet report's four signal categories to the alert types
// that evidence them. Order is worst-first display order. Each signal lines up with a
// ReportIssue.Area value, so the narrative issue and the device-level evidence below
// it speak the same vocabulary.
var reportSignals = []struct {
	Key, Label string
	Types      []string
}{
	{"heat", "Overheating", []string{"overheating"}},
	{"memory", "Memory pressure", []string{"memory_pressure"}},
}

// reportEvidence is one signal category plus the open alerts that evidence it. Every
// alert carries its own serial, fired-at, and a number-rich summary, so the template
// can link each unit to its device page with the real values behind the call-out.
type reportEvidence struct {
	Key, Label string
	Alerts     []db.Alert
}

// groupAlertsBySignal buckets the open alerts into the four report signal categories
// for the detailed-analysis evidence tables.
func groupAlertsBySignal(alerts []db.Alert) []reportEvidence {
	evidence := make([]reportEvidence, 0, len(reportSignals))
	for _, sig := range reportSignals {
		var matched []db.Alert
		for _, a := range alerts {
			for _, t := range sig.Types {
				if a.Type == t {
					matched = append(matched, a)
					break
				}
			}
		}
		evidence = append(evidence, reportEvidence{sig.Key, sig.Label, matched})
	}
	return evidence
}

// ── Fleet Health crash presentation ─────────────────────────────────────────

// crashRow is one row of the health page's crashes panel: a device that has been
// crashing, with its latest kind badge, count, build and a relative time.
type crashRow struct {
	Serial     string
	Restaurant string
	KindLabel  string
	KindClass  string
	BuildID    string
	Ago        string
	Count      int
}

// sparkBar is one bar of the hero's 7-day crashes-per-day sparkline.
type sparkBar struct {
	Label string
	Pct   int
	Class string // "hi" (danger) | "md" (warn) | "" — colour by height
}

// crashKindBadge maps a raw DropBox event kind to a short label + CSS class:
// ANR, native tombstone, or everything-else-is-a-crash.
func crashKindBadge(kind string) (label, class string) {
	k := strings.ToLower(kind)
	switch {
	case strings.Contains(k, "anr"):
		return "ANR", "anr"
	case strings.Contains(k, "tombstone") || strings.Contains(k, "native"):
		return "Native", "tomb"
	default:
		return "Crash", "crash"
	}
}

// agoShort renders a compact relative time ("just now", "14m ago", "3h ago").
func agoShort(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// absInt returns the absolute value of an int.
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// tileDelta formats a today-vs-yesterday change for an overview signal tile.
// badWhenUp says whether an increase is bad (offline/crashes) or good (online).
// Returns Show=false when there's no change so the tile reads "steady".
func tileDelta(today, yesterday int, badWhenUp bool) map[string]any {
	d := today - yesterday
	if d == 0 {
		return map[string]any{"Show": false}
	}
	up := d > 0
	return map[string]any{"Show": true, "N": absInt(d), "Up": up, "Bad": up == badWhenUp}
}

// crashSparkBars turns 7 daily counts into scaled, colour-coded bars. The tallest
// day sets the scale; empty days keep a small stub so the axis still reads. The
// last bar is labelled "Today", earlier ones by weekday.
func crashSparkBars(daily []int) []sparkBar {
	n := len(daily)
	max := 1
	for _, v := range daily {
		if v > max {
			max = v
		}
	}
	bars := make([]sparkBar, n)
	for i, v := range daily {
		pct := v * 100 / max
		if pct < 6 {
			pct = 6
		}
		class := ""
		if v > 0 {
			switch {
			case float64(v) >= 0.7*float64(max):
				class = "hi"
			case float64(v) >= 0.4*float64(max):
				class = "md"
			}
		}
		label := "Today"
		if i < n-1 {
			label = time.Now().AddDate(0, 0, -(n - 1 - i)).Format("Mon")
		}
		bars[i] = sparkBar{Label: label, Pct: pct, Class: class}
	}
	return bars
}

// hotSerialsFromHealth returns the serial of each restaurant's hottest unit whose
// window max temp reached the overheating threshold. These are exactly the devices a
// heat call-out names, so the report card deep-links their serials straight to the
// spike (?focus=temp) instead of the device's default recent window.
func hotSerialsFromHealth(groups []db.GroupHealth, tempC float64) []string {
	var out []string
	for _, g := range groups {
		if g.TempMax != nil && *g.TempMax >= tempC && g.TempMaxSerial != nil && *g.TempMaxSerial != "" {
			out = append(out, *g.TempMaxSerial)
		}
	}
	return out
}

// peakDayForFocus returns the rolled-up day holding the extreme of the focused metric
// (hottest temp, highest RAM, lowest battery), so the device chart can be centered on
// the incident. ok is false when no day has data for that metric.
func peakDayForFocus(stats []db.DeviceDailyStat, focus string) (time.Time, bool) {
	var best *db.DeviceDailyStat
	for i := range stats {
		s := &stats[i]
		switch focus {
		case "ram":
			if s.RAMPctPeak != nil && (best == nil || *s.RAMPctPeak > *best.RAMPctPeak) {
				best = s
			}
		case "battery":
			if s.BatteryMin != nil && (best == nil || *s.BatteryMin < *best.BatteryMin) {
				best = s
			}
		default: // temp
			if s.TempMax != nil && (best == nil || *s.TempMax > *best.TempMax) {
				best = s
			}
		}
	}
	if best == nil {
		return time.Time{}, false
	}
	return best.Day, true
}

// serialsJSON marshals the device serial list for the report card's client-side
// serial linkifier (it turns any serial named in the report prose into a device link).
func serialsJSON(serials []string) template.JS {
	b, err := json.Marshal(serials)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

// restaurantLinksJSON emits [{"name","id"}] for the restaurants the Daily Report may
// name, so the report card can turn each restaurant name in the prose into a link to
// that venue's page. Only named restaurants (Deployed) are included; longest name
// first so a shorter name can't shadow a longer one when the client linkifies.
func restaurantLinksJSON(groups []db.GroupHealth) template.JS {
	type link struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	var out []link
	for _, g := range groups {
		if g.Name == "" {
			continue
		}
		out = append(out, link{Name: g.Name, ID: g.GroupID.String()})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Name) > len(out[j].Name) })
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

// deviceMapPoint is one device pin on the overview fleet map.
type deviceMapPoint struct {
	Serial  string  `json:"serial"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Address string  `json:"address,omitempty"`
	Online  bool    `json:"online"`
	// Extra facets for the dedicated /map page's filters and list.
	Product      string `json:"product"`
	Battery      int    `json:"battery"`
	Kiosk        bool   `json:"kiosk"`
	Build        string `json:"build,omitempty"`
	RestaurantID string `json:"restaurant_id,omitempty"`
	Restaurant   string `json:"restaurant,omitempty"`
	LastSeen     string `json:"last_seen"`
}

// deviceLocationsJSON returns every device with a resolved lat/lon in its latest
// telemetry, as JSON for the overview fleet map, plus the count. Coordinates come
// from the geolocation pipeline (WiFi scan -> lat/lon merged into the device extra).
func (h *Handler) deviceLocationsJSON(ctx context.Context, excludeDPC bool) (template.JS, int) {
	f := db.DeviceFilter{}
	if excludeDPC {
		f.AgentKind = "firmware"
	}
	pts := h.devicePoints(ctx, f)
	b, err := json.Marshal(pts)
	if err != nil {
		return template.JS("[]"), 0
	}
	return template.JS(b), len(pts)
}

// DeviceMapData is the Fleet page's map view: the located devices matching the
// same query string the roster uses (collection, quick view, filters, search),
// as JSON {points:[…], total:N}.
func (h *Handler) DeviceMapData(w http.ResponseWriter, r *http.Request) {
	filter := h.deviceFilterFromRequest(r)
	h.access(r).applyFilter(&filter)
	pts := h.devicePoints(r.Context(), filter)
	total, _ := h.db.CountDevices(r.Context(), filter)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"points": pts, "total": total})
}

// devicePoints returns every device matching filter that has a resolved lat/lon.
func (h *Handler) devicePoints(ctx context.Context, filter db.DeviceFilter) []deviceMapPoint {
	devs, err := h.db.ListDevices(ctx, filter, 0, 5000, "", "")
	if err != nil {
		return []deviceMapPoint{}
	}
	online := h.hub.ConnectedIDs()
	pts := make([]deviceMapPoint, 0, 16)
	for _, dv := range devs {
		var m map[string]json.RawMessage
		if len(dv.LatestExtra) == 0 || json.Unmarshal(dv.LatestExtra, &m) != nil {
			continue
		}
		// Prefer the agent's precise GPS fix (extra.location_lat/lon, reported while
		// fleet policy location_enabled is on) over the WiFi-scan-derived coordinates.
		var lat, lon float64
		if json.Unmarshal(m["location_lat"], &lat) != nil || json.Unmarshal(m["location_lon"], &lon) != nil {
			if json.Unmarshal(m["latitude"], &lat) != nil || json.Unmarshal(m["longitude"], &lon) != nil {
				continue
			}
		}
		name := dv.RestaurantName
		if name == "" {
			name = dv.SerialNumber
		}
		var addr string
		if len(m["location_address"]) > 0 {
			_ = json.Unmarshal(m["location_address"], &addr)
		}
		_, isOn := online[dv.ID]
		p := deviceMapPoint{
			Serial: dv.SerialNumber, Name: name, Lat: lat, Lon: lon, Address: addr, Online: isOn,
			Product: dv.ProductLabel(), Battery: dv.BatteryPct, Kiosk: dv.KioskEnabled, Build: dv.BuildID,
			Restaurant: dv.RestaurantName, LastSeen: dv.LastSeenAt.UTC().Format(time.RFC3339),
		}
		if dv.RestaurantID != nil {
			p.RestaurantID = dv.RestaurantID.String()
		}
		pts = append(pts, p)
	}
	return pts
}

// DeviceShellPage renders the interactive shell console for a device. The console
// reuses the existing command-create (JSON) + command-output SSE endpoints: each
// submitted line is sent as a `shell` command and its output is streamed back.
func (h *Handler) DeviceShellPage(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	h.render(w, r, "device_shell.html", map[string]any{
		"Title":  "Shell · " + serial,
		"Device": device,
		"Online": h.hub.IsConnected(device.ID),
	})
}

// safeCSVFilename returns a filename safe for use in a Content-Disposition
// header: control chars, quotes, and path separators are replaced with '_'.
// Falls back to the provided default if the result would be empty.
func safeCSVFilename(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f, r == '"', r == '\\', r == '/', r == ':', r == '\r', r == '\n':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		s = fallback
	}
	return s + ".csv"
}

// ── Export ────────────────────────────────────────────────────────────────────

func (h *Handler) ExportPage(w http.ResponseWriter, r *http.Request) {
	var serialList []string
	if serials := r.URL.Query().Get("serials"); serials != "" {
		serialList = strings.Split(serials, ",")
	}
	// Opened without a selection (the Overview's Export button, or a bare /export):
	// render with nothing chosen and let the page's picker build the list, rather
	// than bouncing back to Fleet or silently exporting everything.
	fleetTotal := 0
	if n, err := h.db.CountDevices(r.Context(), db.DeviceFilter{}); err == nil {
		fleetTotal = n
	}

	h.render(w, r, "export.html", map[string]any{
		"Title":      "Export Data",
		"Serials":    serialList,
		"FleetTotal": fleetTotal,
		"ScopesJSON": h.pickerScopesJSON(r.Context()),
		"From":       backPath(r),
	})
}

// pickerScopesJSON is the restaurants + groups list the shared device picker
// offers as one-click shortcuts (templates/_picker.html).
func (h *Handler) pickerScopesJSON(ctx context.Context) template.JS {
	type scope struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := []scope{}
	if rests, err := h.db.ListRestaurants(ctx); err == nil {
		for _, r := range rests {
			out = append(out, scope{Kind: "restaurant", ID: r.ID.String(), Name: r.Name, Count: r.DeviceCount})
		}
	}
	if groups, err := h.db.ListGroups(ctx); err == nil {
		for _, g := range groups {
			out = append(out, scope{Kind: "group", ID: g.ID.String(), Name: g.Name, Count: g.DeviceCount})
		}
	}
	b, _ := json.Marshal(out)
	return template.JS(b)
}


// backPath resolves where a "← Back" link should return to: an explicit ?from=
// param if the caller set one, else the referring page (path+query only — the
// scheme/host are discarded, so a spoofed Referer can't produce an open redirect
// to another origin), else "" so the caller can fall back to a fixed default.
func backPath(r *http.Request) string {
	if from := safeRelPath(r.URL.Query().Get("from")); from != "" {
		return from
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			p := u.Path
			if u.RawQuery != "" {
				p += "?" + u.RawQuery
			}
			if rp := safeRelPath(p); rp != "" {
				return rp
			}
		}
	}
	return ""
}

// safeRelPath accepts only a same-origin relative path ("/x", not "//x" —
// protocol-relative — and not an absolute URL), rejecting anything that could
// redirect off-site.
func safeRelPath(p string) string {
	if p == "" || p[0] != '/' || (len(p) > 1 && p[1] == '/') {
		return ""
	}
	return p
}

func extraString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		// try number
		return strings.Trim(string(v), "\"")
	}
	// Android getSSID() wraps SSID in quotes — strip them
	s = strings.Trim(s, "\"")
	return s
}

func extraFloat(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return ""
	}
	return fmt.Sprintf("%.1f", f)
}

func extraInt(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return ""
	}
	return strconv.Itoa(n)
}

// extraBoolAsInt reads a boolean check-in field ("charging") as "1"/"0" rather than
// "true"/"false", matching wlc_status/battery_pct's plain-numeric CSV convention.
func extraBoolAsInt(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return ""
	}
	if b {
		return "1"
	}
	return "0"
}

func extraRamField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m["ram_usage_mb"]
	if !ok {
		return ""
	}
	var ram map[string]int
	if err := json.Unmarshal(v, &ram); err != nil {
		return ""
	}
	val, ok := ram[field]
	if !ok {
		return ""
	}
	return strconv.Itoa(val)
}

// maxExportRange caps the time window for a single CSV export to keep
// memory and download size bounded. 90 days is well past any reasonable
// dashboard use; bump it if a real workflow needs more.
const maxExportRange = 90 * 24 * time.Hour

// adminOnlyExportColumns are the export columns that identify where a device is
// and how to reach it — network identity and physical location — rather than how
// it is behaving. They are admin-only: an operator exporting battery history has
// no reason to carry a fleet's SSIDs, IPs and coordinates out of the dashboard.
// Enforced in ExportCSV, and the checkboxes are hidden in export.html.
var adminOnlyExportColumns = map[string]bool{
	"wifi":       true,
	"ip_address": true,
	"latitude":   true,
	"longitude":  true,
	"last_seen":  true,
}

func (h *Handler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	// The device picker posts one newline-separated field; the Fleet selection and
	// the device page post one value per serial. parseSerialsField takes both.
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		http.Error(w, "No devices selected", http.StatusBadRequest)
		return
	}

	// Parse time range
	startStr := r.FormValue("start")
	endStr := r.FormValue("end")
	if startStr == "" || endStr == "" {
		http.Error(w, "Start and end time required", http.StatusBadRequest)
		return
	}
	// datetime-local inputs are wall-clock in the user's timezone. Prefer an
	// explicit tz_offset (minutes east of UTC, as sent by JS via
	// new Date().getTimezoneOffset()*-1) when present; otherwise fall back to
	// the server's local time. Either way, normalize to UTC for the DB query.
	loc := time.Local
	if v := r.FormValue("tz_offset"); v != "" {
		if mins, errOff := strconv.Atoi(v); errOff == nil {
			loc = time.FixedZone("client", mins*60)
		}
	}
	start, err := time.ParseInLocation("2006-01-02T15:04", startStr, loc)
	if err != nil {
		http.Error(w, "Invalid start time", http.StatusBadRequest)
		return
	}
	end, err := time.ParseInLocation("2006-01-02T15:04", endStr, loc)
	if err != nil {
		http.Error(w, "Invalid end time", http.StatusBadRequest)
		return
	}
	if !end.After(start) {
		http.Error(w, "End time must be after start time", http.StatusBadRequest)
		return
	}
	if end.Sub(start) > maxExportRange {
		http.Error(w, "Time range too large (max 90 days). Narrow the window or use a coarser sampling interval.", http.StatusBadRequest)
		return
	}

	// Sampling interval
	intervalSec := 0
	if v := r.FormValue("interval"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			intervalSec = n
		}
	}

	// Cycles mode: rows at fixed grid marks (start, start+interval, …) instead of
	// raw check-in timestamps. Needs a positive step; default to hourly.
	cycles := r.FormValue("cycles") == "1"
	if cycles && intervalSec <= 0 {
		intervalSec = 3600
	}

	// Columns to include — at least one is required.
	columns := r.Form["columns"]
	if len(columns) == 0 {
		http.Error(w, "Select at least one column to export", http.StatusBadRequest)
		return
	}
	// Admin-only columns are dropped here, not just hidden in the picker: the form
	// posts plain column names, so anyone can add them back by hand. A non-admin
	// asking for them gets a CSV without them rather than an error — the rest of
	// the export is legitimate.
	if h.role(r) != "admin" {
		kept := columns[:0]
		for _, c := range columns {
			if !adminOnlyExportColumns[c] {
				kept = append(kept, c)
			}
		}
		columns = kept
		if len(columns) == 0 {
			http.Error(w, "Those columns are admin-only. Select at least one other column to export.", http.StatusForbidden)
			return
		}
	}

	// Resolve serials to device IDs
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
	if err != nil || len(deviceIDs) == 0 {
		http.Error(w, "No matching devices", http.StatusBadRequest)
		return
	}

	filename := "mdm_export.csv"
	if len(serials) == 1 {
		filename = safeCSVFilename(serials[0]+"_export", "mdm_export")
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	colSet := make(map[string]bool, len(columns))
	for _, c := range columns {
		colSet[c] = true
	}
	colOrder := []string{"battery_pct", "battery_temp_c", "charging", "build_id", "wifi", "ip_address",
		"ram_used_mb", "ram_total_mb", "storage_free_gb", "uptime_seconds", "wlc_status", "timezone",
		"latitude", "longitude", "last_seen"}

	// timestamp is the row's time in the requester's wall clock (with offset),
	// timestamp_utc the same instant in UTC, and sample_at the moment the check-in
	// behind the values was recorded — the point you would find on the device graph.
	header := []string{"serial_number", "timestamp", "timestamp_utc", "sample_at"}
	for _, c := range colOrder {
		if colSet[c] {
			header = append(header, c)
		}
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return
	}

	writeRow := func(row db.ExportRow) error {
		ts := row.Timestamp.In(loc) // both modes: the requester's wall clock, offset included
		sampleAt := ""
		if !row.Empty && !row.SampleAt.IsZero() {
			sampleAt = row.SampleAt.In(loc).Format(time.RFC3339)
		}
		rec := []string{
			row.SerialNumber,
			ts.Format(time.RFC3339),
			row.Timestamp.UTC().Format(time.RFC3339),
			sampleAt,
		}
		for _, c := range colOrder {
			if !colSet[c] {
				continue
			}
			// Grid mark with no check-in within one interval: leave data cells blank.
			if row.Empty && c != "last_seen" {
				rec = append(rec, "")
				continue
			}
			switch c {
			case "battery_pct":
				rec = append(rec, strconv.Itoa(row.BatteryPct))
			case "battery_temp_c":
				rec = append(rec, extraFloat(row.Extra, "battery_temp_c"))
			case "charging":
				rec = append(rec, extraBoolAsInt(row.Extra, "charging"))
			case "build_id":
				rec = append(rec, row.BuildID)
			case "wifi":
				rec = append(rec, extraString(row.Extra, "wifi"))
			case "ip_address":
				rec = append(rec, extraString(row.Extra, "ip_address"))
			case "ram_used_mb":
				rec = append(rec, extraRamField(row.Extra, "used"))
			case "ram_total_mb":
				rec = append(rec, extraRamField(row.Extra, "total"))
			case "storage_free_gb":
				rec = append(rec, extraFloat(row.Extra, "storage_free_gb"))
			case "uptime_seconds":
				rec = append(rec, extraInt(row.Extra, "uptime_seconds"))
			case "wlc_status":
				rec = append(rec, extraInt(row.Extra, "wlc_status"))
			case "timezone":
				rec = append(rec, extraString(row.Extra, "timezone"))
			case "latitude":
				rec = append(rec, extraFloat(row.Extra, "latitude"))
			case "longitude":
				rec = append(rec, extraFloat(row.Extra, "longitude"))
			case "last_seen":
				rec = append(rec, row.LastSeenAt.Format(time.RFC3339))
			}
		}
		return cw.Write(rec)
	}
	if cycles {
		err = h.db.StreamExportCycles(r.Context(), deviceIDs, start.UTC(), end.UTC(), intervalSec, writeRow)
	} else {
		err = h.db.StreamExportCheckins(r.Context(), deviceIDs, start.UTC(), end.UTC(), intervalSec, writeRow)
	}
	cw.Flush()
	if err != nil {
		// Headers already written; the partial CSV is the best we can do.
		// Log so operators can see disconnects / DB errors.
		log.Printf("export: stream error: %v", err)
	}
}

// ExportVisualizePage renders the "upload a CSV and visualize it" tool.
// Everything after that — reading the file, parsing it, building the table
// and charts — happens entirely client-side (see export_visualize.html): the
// file never gets sent to the server, so this handler has nothing to do but
// serve the static page.
func (h *Handler) ExportVisualizePage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "export_visualize.html", map[string]any{"Title": "Import & Visualize Export"})
}

func (h *Handler) DeviceStatsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	kioskCfg, _ := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID)
	h.renderCachedHTML(w, r, "device-stats", map[string]any{
		"Device":              device,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"KioskConfig":         kioskCfg,
	})
}

// DeviceVitalsPartial renders the hero vitals chips (battery, charging, temp,
// RAM, uptime, Wi-Fi) as a standalone fragment so the device page can refresh
// them live on each "device-updated" SSE event without a full reload.
func (h *Handler) DeviceVitalsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	flapRate, _ := h.db.DeviceChargerFlapRate(r.Context(), device.ID, 5)
	h.renderCachedHTML(w, r, "device-vitals", map[string]any{
		"Device":              device,
		"Role":                h.role(r), // the mic-gain chip is admin-only
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ChargerFlapRate":     flapRate,
		"ChargerFlapping":     flapRate > 10,
	})
}

// DeviceInspectorPanel renders the compact "inspector" card for a single
// device, loaded into the split-pane drawer on the devices list so an operator
// can glance at a unit without leaving the 1,000-row roster. It deliberately
// mirrors the at-a-glance vitals only; deep work still happens on the full
// /devices/{serial} page, linked from the panel.
func (h *Handler) DeviceInspectorPanel(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	var release *db.Release
	if device.BuildID != "" {
		release, _ = h.db.GetReleaseByVersion(r.Context(), device.BuildID, device.Product)
	}
	h.renderCachedHTML(w, r, "device-panel", map[string]any{
		"Device":              device,
		"Online":              h.hub.IsConnectedForDisplay(device.ID),
		"Release":             release,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ShellEnabled":        h.cfg.ShellEnabled(),
		"Role":                h.role(r),
	})
}

func (h *Handler) DeviceCommandsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), commands)
	commands = filterShellDeviceCommands(h.role(r), commands)
	h.renderCachedHTML(w, r, "device-commands", map[string]any{
		"Device":   device,
		"Commands": commands,
		"Online":   h.hub.IsConnectedForDisplay(device.ID),
	})
}

// withoutOTA drops OTA updates from the per-device command queue: an OTA runs
// alongside commands (its own progress card on the device page), it is not a
// queued command and never blocks one.
func withoutOTA(in []db.DeviceCommand) []db.DeviceCommand {
	out := in[:0:0]
	for _, c := range in {
		if c.Type != "ota" {
			out = append(out, c)
		}
	}
	return out
}

// DeviceQueuePartial renders the per-device command queue (non-terminal commands, FIFO)
// for the Queue tab — refetched live on device-updated so it drains in place as the device
// works through it.
func (h *Handler) DeviceQueuePartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	queue, err := h.db.GetDeviceQueue(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), queue)
	queue = withoutOTA(filterShellDeviceCommands(h.role(r), queue))
	h.renderCachedHTML(w, r, "device-queue", map[string]any{
		"Device": device,
		"Queue":  queue,
		"Online": h.hub.IsConnectedForDisplay(device.ID),
		"Role":   h.role(r),
	})
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (h *Handler) GroupNew(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 500, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	groups, _ := h.db.ListGroups(r.Context())
	productions, _ := h.db.ListProductions(r.Context(), h.connectedSlice())
	builds, _ := h.db.GetDistinctBuildIDs(r.Context())
	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}

	h.render(w, r, "group_form.html", map[string]any{
		"ScopesJSON": h.pickerScopesJSON(r.Context()),
		"Title":       "New Group",
		"Devices":     devices,
		"Online":      online,
		"Groups":      groups,
		"Productions": productions,
		"Builds":      builds,
	})
}

func (h *Handler) GroupNewDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	filter := db.DeviceFilter{
		Search:  q,
		Online:  r.URL.Query().Get("status"),
		BuildID: r.URL.Query().Get("build"),
		Battery: r.URL.Query().Get("battery"),
	}
	var groupID uuid.UUID
	if gid := r.URL.Query().Get("group"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			groupID = parsed
		}
	}
	filter.GroupID = groupID
	var productionID uuid.UUID
	if pid := r.URL.Query().Get("production"); pid != "" {
		if parsed, err := uuid.Parse(pid); err == nil {
			productionID = parsed
		}
	}
	filter.ProductionID = productionID

	devices, err := h.db.ListDevices(r.Context(), filter, 0, 500, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.renderCachedHTML(w, r, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": false,
	})
}

func (h *Handler) GroupList(w http.ResponseWriter, r *http.Request) {
	// The standalone list is retired — Groups now live in the Fleet collections
	// rail / in-shell grid. Redirect any old links/bookmarks there.
	http.Redirect(w, r, "/devices?view=groups", http.StatusFound)
	return

	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Per-group health scores for the card grid; missing entries render as "—".
	health := make(map[uuid.UUID]db.GroupHealth)
	if ghs, err := h.db.GetGroupHealth(r.Context(), h.connectedSlice()); err == nil {
		for _, gh := range ghs {
			health[gh.GroupID] = gh
		}
	}
	h.render(w, r, "groups.html", map[string]any{
		"Title":  "Groups",
		"Groups": groups,
		"Health": health,
	})
}

// onlineMap returns a device-id → true map of currently WS-connected devices,
// for the shared device-cards roster partial.
func (h *Handler) onlineMap() map[uuid.UUID]bool {
	connected := h.hub.ConnectedIDsForDisplay()
	m := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		m[id] = true
	}
	return m
}

func (h *Handler) GroupDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	data := map[string]any{
		"Title":               g.Name,
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	}
	data["ScopesJSON"] = h.pickerScopesJSON(r.Context())
	if r.URL.Query().Get("partial") == "kpis" { // the count cards, refreshed on group-updated
		_ = h.tmpl.ExecuteTemplate(w, "group-kpis", h.withRole(r, data))
		return
	}
	h.render(w, r, "group_detail.html", data)
}

// GroupDevicesModal serves just the "add/remove devices" card + members list —
// the fleet-page kebab menu's "Add/remove devices" popup loads this via htmx
// instead of navigating to the full group page.
func (h *Handler) GroupDevicesModal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	h.render(w, r, "group-devices-modal", map[string]any{
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"Lean":                true,
	})
}

// parseSerialsField splits the "serials" form field(s) into individual,
// trimmed, non-empty serial numbers. The group forms submit selected devices
// as a single newline-separated <textarea name="serials">, so the raw form
// value is one multi-line blob — it must be split, not used as-is. Also
// tolerates comma separators and repeated form values.
func parseSerialsField(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ','
		}) {
			if part = cleanSerialToken(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// cleanSerialToken strips list decoration people paste along with a serial —
// "-AT070AABU00333", "• AT070…", "1. AT070…", a trailing comma, quotes — from the
// ends only. Never from inside the token: this also parses package names (com.a.b).
func cleanSerialToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, " \t-–—*•·>#\"'`([{")
	// Numbered lists ("1. AT070…", "2) AT070…"). Only when a letter follows, so a
	// dotted value like "1.2.3" is left alone.
	if i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }); i > 0 && i < len(s) {
		if rest := strings.TrimSpace(s[i+1:]); (s[i] == '.' || s[i] == ')') && rest != "" && unicode.IsLetter(rune(rest[0])) {
			s = rest
		}
	}
	return strings.TrimRight(strings.TrimSpace(s), " \t,;:.\"'`)]}")
}

func (h *Handler) GroupCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/groups", http.StatusFound)
		return
	}
	group, err := h.db.CreateGroup(r.Context(), name)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) > 0 {
		if err := h.db.AddDevicesToGroup(r.Context(), serials, group.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/groups/"+group.ID.String(), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (h *Handler) GroupDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteGroup(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// A deleted group can't scope the roster any more, so fall back to all devices.
	localRedirect(w, r, "/devices")
}

// localRedirect sends the user to the form's "redirect" target when it is a safe
// same-site path (a single leading "/"), else to fallback. Lets the fleet toolbar's
// inline edits return to the exact roster view they were launched from, while still
// serving the old callers (group page) that post no redirect.
func localRedirect(w http.ResponseWriter, r *http.Request, fallback string) {
	if dest := r.FormValue("redirect"); strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, "//") {
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}

// GroupUpdate renames a group. A group has only a name, so this doubles as its full "edit".
func (h *Handler) GroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.RenameGroup(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "group.rename", id.String(), name)
	localRedirect(w, r, "/devices?group="+id.String())
}

// ReleaseRename changes only a release's display name (not its changelog or other
// meta), so the fleet toolbar's inline rename can't blank the release notes.
func (h *Handler) ReleaseRename(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseName(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.rename", strconv.Itoa(id), name)
	localRedirect(w, r, "/releases")
}

// ── Restaurants ─────────────────────────────────────────────────────────────────
//
// A restaurant is the venue a device physically lives in. Unlike free-form groups,
// each device belongs to at most one restaurant, and restaurants own the venue
// semantics (deployed flag, service window, health). See the design doc.

// parseFloatPtr parses an optional float form field; "" yields nil.
func parseFloatPtr(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// restaurantTimezones backs the timezone picker on the restaurant form, in the familiar
// "(GMT±HH:MM) City" style used by Google/Windows, ordered west→east. Values are IANA
// names so resolution is DST-aware (localTime); the offset shown is standard time.
type tzOption struct{ Value, Label string }

var restaurantTimezones = []tzOption{
	{"Pacific/Honolulu", "(GMT-10:00) Hawaii"},
	{"America/Anchorage", "(GMT-09:00) Alaska"},
	{"America/Los_Angeles", "(GMT-08:00) Pacific Time — Los Angeles, Vancouver"},
	{"America/Phoenix", "(GMT-07:00) Arizona — Phoenix"},
	{"America/Denver", "(GMT-07:00) Mountain Time — Denver"},
	{"America/Chicago", "(GMT-06:00) Central Time — Chicago"},
	{"America/Mexico_City", "(GMT-06:00) Mexico City"},
	{"America/New_York", "(GMT-05:00) Eastern Time — New York, Toronto"},
	{"America/Bogota", "(GMT-05:00) Bogotá, Lima"},
	{"America/Sao_Paulo", "(GMT-03:00) São Paulo"},
	{"UTC", "(GMT+00:00) UTC"},
	{"Europe/London", "(GMT+00:00) London, Dublin, Lisbon"},
	{"Africa/Lagos", "(GMT+01:00) West Africa — Lagos"},
	{"Europe/Paris", "(GMT+01:00) Paris, Madrid, Rome, Berlin"},
	{"Africa/Cairo", "(GMT+02:00) Cairo"},
	{"Africa/Johannesburg", "(GMT+02:00) Johannesburg"},
	{"Europe/Athens", "(GMT+02:00) Athens, Helsinki"},
	{"Asia/Jerusalem", "(GMT+02:00) Jerusalem"},
	{"Europe/Moscow", "(GMT+03:00) Moscow, Istanbul"},
	{"Asia/Dubai", "(GMT+04:00) Dubai, Abu Dhabi"},
	{"Asia/Karachi", "(GMT+05:00) Karachi, Tashkent"},
	{"Asia/Kolkata", "(GMT+05:30) India — Mumbai, Delhi, Kolkata"},
	{"Asia/Dhaka", "(GMT+06:00) Dhaka"},
	{"Asia/Bangkok", "(GMT+07:00) Bangkok, Jakarta, Hanoi"},
	{"Asia/Singapore", "(GMT+08:00) Singapore, Kuala Lumpur"},
	{"Asia/Hong_Kong", "(GMT+08:00) Hong Kong"},
	{"Asia/Shanghai", "(GMT+08:00) Beijing, Shanghai"},
	{"Australia/Perth", "(GMT+08:00) Perth"},
	{"Asia/Tokyo", "(GMT+09:00) Tokyo, Osaka"},
	{"Asia/Seoul", "(GMT+09:00) Seoul"},
	{"Australia/Sydney", "(GMT+10:00) Sydney, Melbourne"},
	{"Pacific/Auckland", "(GMT+12:00) Auckland"},
}

func (h *Handler) RestaurantList(w http.ResponseWriter, r *http.Request) {
	// The standalone list is retired — Restaurants now live in the Fleet
	// collections rail / in-shell grid. Redirect old links/bookmarks there.
	http.Redirect(w, r, "/devices?view=restaurants", http.StatusFound)
	return

	restaurants, err := h.db.ListRestaurants(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	health := make(map[uuid.UUID]db.GroupHealth)
	if rhs, err := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), 7); err == nil {
		for _, rh := range rhs {
			health[rh.GroupID] = rh // GroupHealth.GroupID carries the restaurant id
		}
	}
	h.render(w, r, "restaurants.html", map[string]any{
		"Title":       "Restaurants",
		"Restaurants": restaurants,
		"Health":      health,
	})
}

func (h *Handler) RestaurantNew(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "restaurant_form.html", map[string]any{
		"ScopesJSON": h.pickerScopesJSON(r.Context()),
		"Title":     "New restaurant",
		"Timezones": restaurantTimezones,
	})
}

func (h *Handler) RestaurantCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/restaurants", http.StatusFound)
		return
	}
	rest, err := h.db.CreateRestaurant(r.Context(), db.Restaurant{
		Name:      name,
		Address:   strings.TrimSpace(r.FormValue("address")),
		Latitude:  parseFloatPtr(r.FormValue("latitude")),
		Longitude: parseFloatPtr(r.FormValue("longitude")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
		Notes:     strings.TrimSpace(r.FormValue("notes")),
	})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.create", rest.ID.String(), name)
	// Deploy any devices staged in the creation picker (moves them to this venue).
	if serials := parseSerialsField(r.Form["serials"]); len(serials) > 0 {
		if err := h.db.AssignDevicesToRestaurant(r.Context(), serials, rest.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
			for _, did := range ids {
				h.hub.PublishDeviceUpdate(did)
			}
		}
	}
	http.Redirect(w, r, "/restaurants/"+rest.ID.String(), http.StatusFound)
}

// RestaurantNewDevices powers the deploy picker on the create form (no venue id yet):
// every assignable device, lab/unassigned shown first, deployed-elsewhere flagged.
func (h *Handler) RestaurantNewDevices(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	devices, err := h.db.ListAssignableDevices(r.Context(), uuid.Nil, q,
		r.URL.Query().Get("status"), r.URL.Query().Get("battery"), 60, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": true,
	})
}

func (h *Handler) RestaurantDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	win, hasOwn, _ := h.db.GetRestaurantServiceWindow(r.Context(), id)
	data := map[string]any{
		"Title":               rest.Name,
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ServiceWindow":       windowView(id.String(), rest.Name, win, hasOwn),
	}
	// Power and usage over the last week: uptime, guest-pad time and what it costs the
	// tablet's own battery, and the battery levels staff plug and unplug at.
	if m, err := h.db.SiteMetricsFor(r.Context(), id, 7); err == nil {
		data["Metrics"] = m
		// The evidence behind the headline: the same week, day by day.
		if daily, err := h.db.SiteMetricsDaily(r.Context(), id, 7); err == nil {
			data["MetricsDaily"] = daily
			max := 1.0
			for _, x := range daily {
				if x.PoweredMinutes > max {
					max = x.PoweredMinutes
				}
			}
			data["MetricsDailyMax"] = max
		}
	}
	data["ScopesJSON"] = h.pickerScopesJSON(r.Context())
	if r.URL.Query().Get("partial") == "kpis" { // the count cards, refreshed on restaurant-updated
		_ = h.tmpl.ExecuteTemplate(w, "restaurant-kpis", h.withRole(r, data))
		return
	}
	h.render(w, r, "restaurant_detail.html", data)
}

// reportBar is one day in the weekly report's powered-hours strip, already reduced
// to what the template draws: a label, the per-device hours, and a bar height.
type reportBar struct {
	Label string
	Hours float64
	Pct   int
}

// RestaurantReport renders a venue's weekly report: the same figures the venue page
// shows in its Power & usage card, plus a row per device, on a standalone printable
// page. Everything comes from device_daily_stats rollups, never raw check-ins.
func (h *Handler) RestaurantReport(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	const days = 7
	data := map[string]any{
		"Title":      rest.Name + " — Weekly report",
		"Restaurant": rest,
		"Days":       days,
		"WindowFrom": time.Now().AddDate(0, 0, -(days - 1)),
		"WindowTo":   time.Now(),
	}
	if m, err := h.db.SiteMetricsFor(r.Context(), id, days); err == nil {
		data["Metrics"] = m
	}
	// Bars are drawn per device against a 24-hour day, like the venue page, so a site
	// with more devices does not simply read as taller.
	if daily, err := h.db.SiteMetricsDaily(r.Context(), id, days); err == nil {
		bars := make([]reportBar, 0, len(daily))
		for _, x := range daily {
			n := x.Devices
			if n < 1 {
				n = 1
			}
			per := x.PoweredMinutes / float64(n)
			bars = append(bars, reportBar{
				Label: x.Day.Format("Mon"),
				Hours: per / 60,
				Pct:   pctCapped(per, 24*60),
			})
		}
		data["Bars"] = bars
	}
	weeks, err := h.db.RestaurantDeviceWeeks(r.Context(), id, days)
	if err != nil {
		log.Printf("[report] device weeks %s: %v", id, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// A viewer who cannot see a device on the fleet page must not see its row here.
	acc := h.access(r)
	visible := weeks[:0]
	for _, x := range weeks {
		if acc.visible(x.DeviceID) {
			visible = append(visible, x)
		}
	}
	data["DeviceWeeks"] = visible
	data["WindowHours"] = days * 24

	// Per-device averages for the header tiles. Divided by the devices that actually
	// reported, not by the venue's device count, so one never-deployed tablet cannot
	// halve the venue's uptime.
	if n := len(visible); n > 0 {
		var plugged, powered, pad, standby float64
		standbyDevices := 0
		for _, x := range visible {
			plugged += x.PluggedMinutes
			powered += x.PoweredMinutes
			pad += x.PadMinutes
			if x.HasStandby() {
				standby += x.StandbyMinutes()
				standbyDevices++
			}
		}
		full := float64(days) * 24 * 60
		data["AvgPluggedMinutes"] = plugged / float64(n)
		data["AvgPluggedPct"] = pctCapped(plugged/float64(n), full)
		data["AvgPoweredMinutes"] = powered / float64(n)
		data["AvgPadMinutes"] = pad / float64(n)
		data["AvgPadPct"] = pctCapped(pad/float64(n), full)
		if standbyDevices > 0 {
			data["AvgStandbyMinutes"] = standby / float64(standbyDevices)
		}
	}
	h.render(w, r, "restaurant_report.html", data)
}

// pctCapped is a share of a whole as a whole number, never past 100 — the report's
// meters are bars, and a bar past its track reads as a bug rather than as good news.
func pctCapped(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	p := int(part * 100 / whole)
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}

// reportRecipientDomain is the only domain a venue report may be sent to. A report
// names every device at a site and how it behaved all week, so it goes to colleagues
// and nowhere else — an open recipient box on an internal dashboard is a data-exfil
// path that looks like a feature.
const reportRecipientDomain = "@aioapp.com"

// RestaurantReportEmail mails a venue's weekly report. The email carries the figures
// themselves rather than a bare link, so it is readable without a login, and a link
// back for the full per-device table.
func (h *Handler) RestaurantReportEmail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	to := strings.TrimSpace(r.FormValue("to"))
	if to == "" || !strings.Contains(to, "@") {
		h.reportMailResult(w, r, false, "Enter an email address")
		return
	}
	if !strings.HasSuffix(strings.ToLower(to), reportRecipientDomain) {
		h.reportMailResult(w, r, false, "Reports can only be sent to "+reportRecipientDomain+" addresses")
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}

	const days = 7
	m, _ := h.db.SiteMetricsFor(r.Context(), id, days)
	weeks, err := h.db.RestaurantDeviceWeeks(r.Context(), id, days)
	if err != nil {
		log.Printf("[report] email device weeks %s: %v", id, err)
		h.reportMailResult(w, r, false, "Could not build the report")
		return
	}
	acc := h.access(r)
	visible := weeks[:0]
	for _, x := range weeks {
		if acc.visible(x.DeviceID) {
			visible = append(visible, x)
		}
	}

	subject := fmt.Sprintf("%s — weekly device report", rest.Name)
	body := reportEmailHTML(rest.Name, days, m, visible)
	if err := h.mail.Send(r.Context(), to, subject, body); err != nil {
		log.Printf("[report] send to %s: %v", to, err)
		h.reportMailResult(w, r, false, "Sending failed — check the server log")
		return
	}
	h.audit(r, "restaurant.report.email", rest.Name, to)
	h.reportMailResult(w, r, true, "Sent to "+to)
}

// reportMailResult answers the htmx form with a one-line status, or redirects back to
// the report when the browser posted the form without JS.
func (h *Handler) reportMailResult(w http.ResponseWriter, r *http.Request, ok bool, msg string) {
	if !hxReq(r) {
		h.hxRedirect(w, r, r.Header.Get("Referer"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	class := "bad"
	if ok {
		class = "ok"
	}
	fmt.Fprintf(w, `<span class="%s">%s</span>`, class, template.HTMLEscapeString(msg))
}

// reportEmailHTML renders the report for an email client. Tables and inline styles
// throughout, no stylesheet, no flex or grid and no CSS variables: Outlook renders
// with Word's engine, which supports none of them. The palette is the dashboard's so
// the mail reads as the same product as the page it came from.
func reportEmailHTML(venue string, days int, m db.SiteMetrics, weeks []db.DeviceWeek) string {
	const (
		ink    = "#2c2c2b"
		muted  = "#77736f"
		faint  = "#8a8a92"
		canvas = "#f7f7f6"
		line   = "#e6e5e3"
		coral  = "#ff654f"
		green  = "#39875f"
		amber  = "#b46b2d"
		red    = "#cc4c43"
	)
	hrs := func(min float64) string { return fmt.Sprintf("%.1f", min/60) }
	// Same thresholds as the report's own legend, so a row that reads amber on the
	// page reads amber in the mail.
	band := func(pct int) string {
		switch {
		case pct < 70:
			return red
		case pct < 85:
			return amber
		}
		return green
	}
	esc := template.HTMLEscapeString

	// tile is one headline figure: a big number over a small caption.
	tile := func(label, value, unit, note string) string {
		return fmt.Sprintf(`<td width="25%%" valign="top" style="padding:14px 16px;border-left:1px solid %s;">`+
			`<div style="font:600 11px -apple-system,Segoe UI,Roboto,sans-serif;letter-spacing:.06em;text-transform:uppercase;color:%s;">%s</div>`+
			`<div style="font:300 30px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:8px 0 0;">%s<span style="font-size:14px;color:%s;"> %s</span></div>`+
			`<div style="font:400 12px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:6px 0 0;">%s</div></td>`,
			line, muted, label, ink, value, muted, unit, muted, note)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><body style="margin:0;padding:0;background:%s;">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%" style="background:%s;padding:24px 12px;"><tr><td align="center">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="680" style="width:680px;max-width:100%%;background:#ffffff;border:1px solid %s;border-radius:14px;overflow:hidden;">`, line)

	// Header band.
	fmt.Fprintf(&b, `<tr><td style="padding:22px 24px 18px;border-bottom:1px solid %s;">`+
		`<div style="font:600 11px -apple-system,Segoe UI,Roboto,sans-serif;letter-spacing:.11em;text-transform:uppercase;color:%s;">Weekly device report</div>`+
		`<div style="font:600 26px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:6px 0 0;">%s</div>`+
		`<div style="font:400 13px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:6px 0 0;">Last %d days · %d device%s reporting</div>`+
		`</td></tr>`,
		line, coral, ink, esc(venue), muted, days, len(weeks), plural(len(weeks)))

	// Headline figures.
	uptimeNote := fmt.Sprintf("%s fleet hours", hrs(m.PoweredMinutes))
	padNote := "a guest phone charging on a tablet"
	costValue, costUnit, costNote := "—", "", "not enough charging time yet"
	if m.HasPadDrain() {
		costValue = fmt.Sprintf("%.2f", m.PadDrainPctPerMin())
		costUnit = "%/min"
		costNote = fmt.Sprintf("off mains, over %s hrs of charging", hrs(m.PadDrainMinutes))
	}
	standbyValue, standbyUnit, standbyNote := "—", "", "not reported by this firmware yet"
	if m.HasStandby() {
		standbyValue = fmt.Sprintf("%d", m.StandbyPct())
		standbyUnit = "%"
		standbyNote = "of powered time, screen off"
	}
	fmt.Fprintf(&b, `<tr><td style="padding:0;"><table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%"><tr>`)
	b.WriteString(strings.Replace(tile("Average uptime", fmt.Sprintf("%d", m.UptimeFullPct()), "%", uptimeNote), "border-left:1px solid "+line+";", "", 1))
	b.WriteString(tile("Wireless charging", hrs(m.PadMinutes), "hrs", padNote))
	b.WriteString(tile("Charging cost", costValue, costUnit, costNote))
	b.WriteString(tile("Standby", standbyValue, standbyUnit, standbyNote))
	fmt.Fprintf(&b, `</tr></table></td></tr>`)

	// Per-device table.
	fmt.Fprintf(&b, `<tr><td style="padding:0;border-top:1px solid %s;">`, line)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%" style="border-collapse:collapse;font:400 13px -apple-system,Segoe UI,Roboto,sans-serif;">`)
	th := `<th align="%s" style="padding:11px 16px;background:#faf9f8;border-bottom:1px solid ` + line + `;font:600 10.5px -apple-system,Segoe UI,Roboto,sans-serif;letter-spacing:.06em;text-transform:uppercase;color:` + muted + `;">%s</th>`
	fmt.Fprintf(&b, `<tr>`)
	fmt.Fprintf(&b, th, "left", "Device")
	fmt.Fprintf(&b, th, "right", "Uptime")
	fmt.Fprintf(&b, th, "right", "Wireless charging")
	fmt.Fprintf(&b, th, "right", "Plugged in")
	fmt.Fprintf(&b, th, "right", "Days")
	fmt.Fprintf(&b, `</tr>`)
	for _, x := range weeks {
		nick := ""
		if x.Nickname != "" {
			nick = fmt.Sprintf(`<div style="font-size:11.5px;color:%s;padding:2px 0 0;">%s</div>`, faint, esc(x.Nickname))
		}
		cell := `<td align="right" style="padding:11px 16px;border-bottom:1px solid ` + line + `;color:` + ink + `;">%s <span style="color:` + muted + `;font-size:11.5px;">%s</span></td>`
		fmt.Fprintf(&b, `<tr><td style="padding:11px 16px;border-bottom:1px solid %s;color:%s;font-weight:600;">%s%s</td>`, line, ink, esc(x.Serial), nick)
		fmt.Fprintf(&b, `<td align="right" style="padding:11px 16px;border-bottom:1px solid %s;color:%s;">%s <span style="color:%s;font-size:11.5px;">hrs</span> <span style="color:%s;font-weight:600;">%d%%</span></td>`,
			line, ink, hrs(x.PoweredMinutes), muted, band(x.UptimeFullPct()), x.UptimeFullPct())
		fmt.Fprintf(&b, cell, hrs(x.PadMinutes), "hrs")
		fmt.Fprintf(&b, cell, hrs(x.PluggedMinutes), "hrs")
		fmt.Fprintf(&b, `<td align="right" style="padding:11px 16px;border-bottom:1px solid %s;color:%s;">%d</td></tr>`, line, muted, x.DeviceDays)
	}
	fmt.Fprintf(&b, `</table></td></tr>`)

	fmt.Fprintf(&b, `<tr><td style="padding:14px 16px 18px;font:400 11.5px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;">`+
		`Every figure is measured over the device-days that actually reported, so a device deployed midweek shortens its own window instead of dragging the venue down. A dash means the reading was never measured, not that it was zero.`+
		`</td></tr>`, muted)

	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

// RestaurantDevicesModal serves just the "deploy devices" card + members list —
// the fleet-page kebab menu's "Add/remove devices" popup loads this via htmx
// instead of navigating to the full venue page.
func (h *Handler) RestaurantDevicesModal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	h.render(w, r, "restaurant-devices-modal", map[string]any{
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"Lean":                true,
	})
}

// peakRangeView / peakWindowView present a scope's peak-hour ranges for editing. When
// the restaurant has no rows of its own the fleet-default set is shown with Inherited=true.
type peakRangeView struct{ Start, End string }
type peakWindowView struct {
	Ranges    []peakRangeView
	Inherited bool // showing the fleet default (restaurant has no own rows)
}

// peakView resolves the peak ranges to display for a restaurant: its own rows if any,
// otherwise the fleet default (marked Inherited). Pass nil for the fleet default itself.
func (h *Handler) peakView(ctx context.Context, restaurantID *uuid.UUID) peakWindowView {
	rows, _ := h.db.GetPeakWindows(ctx, restaurantID)
	inherited := false
	if len(rows) == 0 && restaurantID != nil {
		rows, _ = h.db.GetPeakWindows(ctx, nil)
		inherited = true
	}
	v := peakWindowView{Inherited: inherited}
	for _, r := range rows {
		v.Ranges = append(v.Ranges, peakRangeView{Start: hhmm(r.StartMin), End: hhmm(r.EndMin)})
	}
	return v
}

func (h *Handler) RestaurantEdit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	// The whole app is hx-boosted (see layout.html), so a normal in-app nav to this
	// page still arrives as an HX-Request — Embed drops the page chrome (header/
	// footer/back-link) since layout.html's boosted shell already supplies it.
	embed := r.Header.Get("HX-Request") == "true"
	h.render(w, r, "restaurant_form.html", map[string]any{
		"ScopesJSON": h.pickerScopesJSON(r.Context()),
		"Title":      "Edit " + rest.Name,
		"Restaurant": rest,
		"Timezones":  restaurantTimezones,
		"Embed":      embed,
		"RedirectTo": "/devices?restaurant=" + id.String(),
	})
}

func (h *Handler) RestaurantUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateRestaurant(r.Context(), db.Restaurant{
		ID:        id,
		Name:      name,
		Address:   strings.TrimSpace(r.FormValue("address")),
		Latitude:  parseFloatPtr(r.FormValue("latitude")),
		Longitude: parseFloatPtr(r.FormValue("longitude")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
		Notes:     strings.TrimSpace(r.FormValue("notes")),
	}); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.update", id.String(), name)
	localRedirect(w, r, "/restaurants/"+id.String())
}

// RestaurantRename changes only a restaurant's display name — the fleet toolbar's
// inline rename posts here instead of RestaurantUpdate, which overwrites every
// field and would blank address/timezone/notes if only "name" were submitted.
func (h *Handler) RestaurantRename(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.RenameRestaurant(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.rename", id.String(), name)
	localRedirect(w, r, "/devices?restaurant="+id.String())
}

func (h *Handler) RestaurantDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteRestaurant(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.delete", id.String(), "")
	localRedirect(w, r, "/restaurants")
}

// RestaurantAssignDevices assigns one or more devices (by serial) to this restaurant.
func (h *Handler) RestaurantAssignDevices(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
		return
	}
	// Atomic bulk assign: avoids leaving a partial set assigned if one row errors.
	if err := h.db.AssignDevicesToRestaurant(r.Context(), serials, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.assign", id.String(), fmt.Sprintf("%d devices", len(serials)))
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
	}
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
}

// RestaurantDevicePicker renders the searchable candidate-device list for the assign
// picker (devices not already in this restaurant; unassigned first).
func (h *Handler) RestaurantDevicePicker(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	battery := r.URL.Query().Get("battery")
	devices, err := h.db.ListAssignableDevices(r.Context(), id, q, status, battery, 25, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": true,
	})
}

// RestaurantRemoveDevice unassigns a device from this restaurant (back to lab).
func (h *Handler) RestaurantRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, nil); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.unassign", id.String(), serial)
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
	}
	h.hxDone(w, r, "/restaurants/"+id.String(), "restaurant-updated")
}

// RestaurantMembers renders just the devices card of a restaurant for an in-place
// htmx refresh (fired by "restaurant-updated" after a device is removed).
func (h *Handler) RestaurantMembers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "restaurant-members", h.withRole(r, map[string]any{
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		// Set by the fleet-page devices popup's own hx-get URL (?lean=1) — see the
		// matching comment in GroupMembers.
		"Lean": r.URL.Query().Get("lean") == "1",
	}))
}

// RestaurantSetServiceWindow upserts (or resets) this restaurant's service window.
func (h *Handler) RestaurantSetServiceWindow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	if r.FormValue("action") == "reset" {
		_ = h.db.DeleteServiceWindow(r.Context(), id)
		h.audit(r, "restaurant.service_window", id.String(), "reset")
		http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
		return
	}
	sw := db.ServiceWindow{
		RestaurantID:  &id,
		OpenMin:       parseHHMM(r.FormValue("open"), 420),
		CloseMin:      parseHHMM(r.FormValue("close"), 1380),
		NightOpenMin:  parseHHMM(r.FormValue("night_open"), 1410),
		NightCloseMin: parseHHMM(r.FormValue("night_close"), 360),
		TZ:            strings.TrimSpace(r.FormValue("timezone")),
	}
	if err := h.db.SetServiceWindow(r.Context(), sw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.service_window", id.String(), "")
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
}

// RestaurantDailyStatsJSON powers the restaurant Trends chart.
func (h *Handler) RestaurantDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			days = n
		}
	}
	stats, err := h.db.GetRestaurantDailyStats(r.Context(), id, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.GroupDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// DeviceSetRestaurant assigns/clears a single device's restaurant from the device page.
func (h *Handler) DeviceSetRestaurant(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	ridStr := strings.TrimSpace(r.FormValue("restaurant_id"))
	var rid *uuid.UUID
	if ridStr != "" {
		id, err := uuid.Parse(ridStr)
		if err != nil {
			http.Error(w, "Invalid restaurant", http.StatusBadRequest)
			return
		}
		rid = &id
	}
	if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, rid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.restaurant", serial, ridStr)
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
	}
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

func (h *Handler) GroupAddDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	// Accept single serial_number (legacy) or multi-line serials textarea
	var serials []string
	if s := strings.TrimSpace(r.FormValue("serial_number")); s != "" {
		serials = append(serials, s)
	}
	serials = append(serials, parseSerialsField(r.Form["serials"])...)
	if len(serials) == 0 {
		h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
		return
	}
	if err := h.db.AddDevicesToGroup(r.Context(), serials, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
	}
	// HX request (the group page's add form): 204 + group-updated so the members
	// list refreshes in place. Plain POST (no JS) still redirects back to the group.
	h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
}

func (h *Handler) GroupDeviceSearch(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := db.DeviceFilter{
		Search:              query,
		ExcludeGroupID:      id,
		Online:              r.URL.Query().Get("status"),
		Battery:             r.URL.Query().Get("battery"),
		ActiveThresholdSecs: h.cfg.CheckinInterval() * 3,
	}
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 60, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Query":    query,
		"Devices":  devices,
		"Relocate": false,
	})
}

// DeviceSearch is a generic serial type-ahead (not scoped to a group), used by the
// command builder's "specific devices" target to look devices up instead of typing serials.
// CmdkIndex returns the navigable entities (groups, restaurants) as JSON, so
// the Cmd-K palette can fuzzy-match them alongside pages and devices. Productions are
// deliberately excluded — they're a manufacturing concern, not a navigation target.
func (h *Handler) CmdkIndex(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Label string `json:"label"`
		Sub   string `json:"sub"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	}
	out := []entry{}
	if gs, err := h.db.ListGroups(r.Context()); err == nil {
		for _, g := range gs {
			out = append(out, entry{g.Name, "Group", "/groups/" + g.ID.String(), "Group"})
		}
	}
	if rs, err := h.db.ListRestaurants(r.Context()); err == nil {
		for _, rest := range rs {
			out = append(out, entry{rest.Name, "Restaurant", "/restaurants/" + rest.ID.String(), "Restaurant"})
		}
	}
	// People: anyone signed in may open a colleague's profile (owners have no Users area).
	if h.role(r) != "owner" {
		if us, err := h.db.ListUsers(r.Context()); err == nil {
			for _, u := range us {
				sub := roleLabel(u.Role)
				if u.Username != u.DisplayName() {
					sub += " · " + u.Username
				}
				out = append(out, entry{u.DisplayName(), sub, "/users/" + u.ID.String() + "/profile", "User"})
			}
		}
	}
	// Releases are deliberately not indexed: dozens of version strings drowned
	// out the devices and sites people actually jump to. The Releases page itself
	// is still reachable from the palette's page list.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *Handler) DeviceSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	// The Cmd-K palette asks for JSON; the HTMX type-ahead inputs want the HTML list.
	wantJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	if query == "" {
		if wantJSON {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		h.tmpl.ExecuteTemplate(w, "device-search-results", map[string]any{"Query": "", "Devices": []db.Device{}})
		return
	}
	devices, err := h.db.SearchDevicesBySerial(r.Context(), query, 8)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	if wantJSON {
		out := make([]map[string]string, 0, len(devices))
		for _, d := range devices {
			out = append(out, map[string]string{"serial": d.SerialNumber, "build": d.BuildID})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-search-results", map[string]any{"Query": query, "Devices": devices})
}

func (h *Handler) GroupRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if err := h.db.RemoveDeviceFromGroup(r.Context(), serial, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
	}
	h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
}

// GroupBulkRemoveDevice removes several devices from a group in one request, so the
// members list can drop a multi-selection at once instead of one device at a time
// (FW-2026-000024). Serials arrive in the multi-valued "serials" field.
func (h *Handler) GroupBulkRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
		return
	}
	if err := h.db.RemoveDevicesFromGroup(r.Context(), serials, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
	}
	h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
}

// GroupMembers renders just the members card of a group for an in-place htmx
// refresh (fired by "group-updated" after a device is removed) — no page reload.
func (h *Handler) GroupMembers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "group-members", h.withRole(r, map[string]any{
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		// Set by the fleet-page devices popup's own hx-get URL (?lean=1) so this
		// live in-place refresh (fired on "group-updated") keeps the same lean
		// rendering the popup opened with, instead of reverting to the full
		// fleet-card list the standalone group page uses.
		"Lean": r.URL.Query().Get("lean") == "1",
	}))
}

func (h *Handler) GroupCommandCreate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}
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

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/groups/"+id.String(), http.StatusFound)
		return
	}

	payload := buildPayload(cmdType, r)
	if cmdType == "install_apk" {
		// Capture APK size + ETag so the device can verify/resume the download.
		payload = apkmeta.Augment(r.Context(), apkURL, payload)
	}

	gTargets, gType, ok := h.enforceCommandTargets(w, r, policyActionForCommand(cmdType), "groups", []uuid.UUID{id})
	if !ok {
		return
	}
	cmd, err := h.db.CreateCommandBy(r.Context(), cmdType, apkURL, payload, gType, gTargets, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, gType, gTargets)
	detail := "target=groups, group=" + id.String()
	if reason != "" {
		detail += ", reason=" + reason
	}
	detail += ", cmd=" + cmd.ID.String()
	h.audit(r, "command.send", cmdType, detail)
	http.Redirect(w, r, "/commands/"+cmd.ID.String(), http.StatusFound)
}

func (h *Handler) DeviceHide(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.db.HideDevice(r.Context(), serial); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.hide", serial, "")
	h.hub.PublishDeviceUpdate(device.ID)
	// 204 + device-updated instead of a full /devices reload: the fleet SSE row patch
	// (patchRow) drops the now-hidden card in place, so nothing flashes.
	h.hxDone(w, r, "/devices", "device-updated")
}

func (h *Handler) DeviceUnhide(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.db.UnhideDevice(r.Context(), serial); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.unhide", serial, "")
	h.hub.PublishDeviceUpdate(device.ID)
	// 204 + device-updated: the row patch handles the change in place (no full reload).
	h.hxDone(w, r, "/devices?hidden=only", "device-updated")
}

func (h *Handler) DeviceClearOTA(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.db.ClearPendingOTACommands(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

func (h *Handler) BulkHideDevices(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := r.Form["serials"]
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}
	if err := h.db.BulkHideDevices(r.Context(), serials); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.bulk_hide", strings.Join(serials, ","), fmt.Sprintf("%d devices", len(serials)))
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, id := range ids {
			h.hub.PublishDeviceUpdate(id)
		}
	}
	h.hxDoneToastEvents(w, r, "/devices", fmt.Sprintf("Hid %d device%s", len(serials), plural(len(serials))), "success", "refresh-devices")
}

func (h *Handler) BulkUnhideDevices(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := r.Form["serials"]
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices?hidden=only", http.StatusSeeOther)
		return
	}
	if err := h.db.BulkUnhideDevices(r.Context(), serials); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.bulk_unhide", strings.Join(serials, ","), fmt.Sprintf("%d devices", len(serials)))
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, id := range ids {
			h.hub.PublishDeviceUpdate(id)
		}
	}
	h.hxDoneToastEvents(w, r, "/devices?hidden=only", fmt.Sprintf("Unhid %d device%s", len(serials), plural(len(serials))), "success", "refresh-devices")
}

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

// BulkClass sets the device type (tablet, panel, kiosk, ...) on the selection.
func (h *Handler) BulkClass(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}
	c := strings.ToLower(strings.TrimSpace(r.FormValue("device_class")))
	if c != "" && !product.IsClass(c) {
		http.Error(w, "Unknown class", http.StatusBadRequest)
		return
	}
	done := 0
	for _, serial := range serials {
		device, err := h.db.GetDevice(r.Context(), serial)
		if err != nil {
			continue
		}
		if err := h.db.SetDeviceClass(r.Context(), device.ID, c); err != nil {
			continue
		}
		h.hub.PublishDeviceUpdate(device.ID)
		done++
	}
	h.audit(r, "device.bulk_class", strings.Join(serials, ","), fmt.Sprintf("%d devices: %s", done, c))
	h.hxDoneToastEvents(w, r, "/devices", fmt.Sprintf("Set type on %d device%s", done, plural(done)), "success", "refresh-devices")
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
	serials := parseSerialsField(r.Form["serials"])
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
		// Enabling kiosk locks the device to one app — wake the screen too, so a device
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
	if !h.requireFleetAction(w, r, "kiosk") {
		return
	}
	r.ParseForm()
	serials := r.Form["serials"]
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
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
	if !h.requireFleetAction(w, r, "kiosk") {
		return
	}
	r.ParseForm()
	serials := r.Form["serials"]
	if len(serials) == 0 {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
		return
	}

	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	if enabled && pkg == "" {
		http.Error(w, "Kiosk package is required when enabling kiosk mode", http.StatusBadRequest)
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
	if err := h.db.SetKioskConfigForDevices(r.Context(), deviceIDs, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushKioskConfigToDevices(r.Context(), deviceIDs)
	verb := "Disabled kiosk on"
	if enabled {
		verb = "Enabled kiosk on"
	}
	h.hxDoneToastEvents(w, r, "/devices", fmt.Sprintf("%s %d device%s", verb, len(serials), plural(len(serials))), "success", "refresh-devices")
}

// ── OTA Packages & Deployments ────────────────────────────────────────────────

// versionRow is one row of the unified releases-and-versions table: a release version,
// whether it's a tracked release (with lifecycle) or only seen on devices, and how many
// devices report it.
type versionRow struct {
	Version           string
	ReleaseID         *int
	Product           string // hardware product a tracked release targets ("" for untracked builds)
	Name              string
	Changelog         string     // release notes (tracked releases only) — shown as a snippet on the lean list
	Status            string     // "" when not tracked
	ReleasedAt        *time.Time // release date (tracked releases only) — the release's created_at, which also defines ordering
	Tracked           bool
	Hidden            bool
	DeviceCount       int
	PackageCount      int
	DeployCount       int
	QA                db.QASummary      // QA/test status; zero value (Total 0) when not tracked
	Problems          db.ProblemSummary // open/blocker problem counts for the tracked release
	SignedOffBy       string            // dev who signed off ("" = not signed off)
	SignedOffAt       *time.Time
	TestingDone       bool         // testing finished — the release is inactive (retired from active slot)
	IsBranch          bool         // off-mainline branch build (shown nested under its parent in the list)
	IsDev             bool         // dev-only release — listed for devs/admins, invisible to everyone else
	ParentID          *int         // for a branch/derivative, the release id it forks from / matches
	ParentVersion     string       // for a branch, the release it forked from; for a derivative, the release it matches
	AdoptableParentID *int         // set when this is an UNTRACKED reported build matching a release's naming — one-click adopt as a branch of it
	Children          []versionRow // branch builds + adoptable derivative builds forked off this release, shown indented beneath it
	SuggestedBranch   string       // next branch version to suggest for this release (version + -tN)
	QfilURL           string       // newest active QFIL flashing bundle URL ("" = none set)
	Merged            bool         // branch: has been merged onto main (terminal)
	MergedIntoVersion string       // branch: the mainline release it merged into
	MergedFromVersion string       // mainline node: the branch it absorbed on merge
}

// latestQfilURL returns the newest active QFIL bundle URL for a release, or ""
// when none is set (so the releases list can grey out the download icon).
func (h *Handler) latestQfilURL(r *http.Request, releaseID int) string {
	if qpkgs, _ := h.db.ListQFILPackagesByRelease(r.Context(), releaseID); len(qpkgs) > 0 {
		return qpkgs[0].URL
	}
	return ""
}

// ReleaseSetDev turns a release's dev flag on or off. Off publishes it to every
// role; on takes it back out of sight for anyone but devs and admins.
func (h *Handler) ReleaseSetDev(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	dev := r.FormValue("dev") == "1"
	if err := h.db.SetReleaseDev(r.Context(), id, dev); err != nil {
		h.hxDoneToast(w, r, fmt.Sprintf("/releases/%d", id), "Could not change the release visibility", "error")
		return
	}
	h.audit(r, "release.set_dev", strconv.Itoa(id), map[bool]string{true: "dev", false: "everyone"}[dev])
	msg := "Released to everyone · it now shows on the Updates page for every role"
	if dev {
		msg = "Marked dev · only devs and admins can see it"
	}
	h.hxDoneToast(w, r, fmt.Sprintf("/releases/%d", id), msg, "success")
}

// productLabels holds the model names learned from what devices report, keyed by
// product. Package-level because the template func map is built before any Handler
// exists; written by productFilters, read by the productLabel template func.
var productLabels atomic.Value // map[string]string

// productLabel is the template-facing label for a product key: a learned model name
// when we have one, else the catalog label (which falls back to the key itself).
func productLabel(key string) string {
	if m, ok := productLabels.Load().(map[string]string); ok {
		if label := m[product.Normalize(key)]; label != "" {
			return label
		}
	}
	return product.Label(key)
}

// productFilters is the catalog plus every other product the fleet actually reports,
// so a device that is not our own hardware — a Sunmi D3 PRO, a stock Pixel running the
// DPC agent — can still be filtered for instead of hiding behind "All products".
// Catalog order first, then the newcomers by how many devices run them.
func (h *Handler) productFilters(ctx context.Context) []product.Product {
	out := product.All()
	seen := make(map[string]bool, len(out))
	for _, p := range out {
		seen[p.Key] = true
	}
	counts, err := h.db.FleetProducts(ctx)
	if err != nil {
		return out
	}
	learned := map[string]string{}
	for _, c := range counts {
		if label := modelLabel(c.Manufacturer, c.Model); label != "" && !product.IsKnown(c.Product) {
			learned[product.Normalize(c.Product)] = label
		}
	}
	productLabels.Store(learned)
	for _, c := range counts {
		key := product.Normalize(c.Product)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		p, _ := product.Resolve(c.Product)
		if label := modelLabel(c.Manufacturer, c.Model); label != "" {
			p.Label = label
		}
		out = append(out, p)
	}
	return out
}

// fleetProductFilters is productFilters for the fleet surfaces: DPC-managed hardware
// (a Sunmi till, a stock Pixel) is an admin's concern, so only an admin sees those
// entries. Our own products are everyone's.
func (h *Handler) fleetProductFilters(ctx context.Context, role string) []product.Product {
	all := h.productFilters(ctx)
	if role == "admin" {
		return all
	}
	out := all[:0:0]
	for _, p := range all {
		if p.Kind == product.KindAndroid {
			continue
		}
		out = append(out, p)
	}
	return out
}

// releaseProductFilters is productFilters for everything release-shaped — Updates,
// Releases, rollouts, the push screen. A release is a firmware image for our own
// hardware; a DPC-managed device has no firmware we ship, so offering its product as
// a filter there only ever leads to an empty list.
func (h *Handler) releaseProductFilters(ctx context.Context) []product.Product {
	all := h.productFilters(ctx)
	out := all[:0:0]
	for _, p := range all {
		if p.Kind == product.KindAndroid {
			continue
		}
		out = append(out, p)
	}
	return out
}

// modelLabel turns what a device reports about itself into the name on the box:
// "SUNMI" + "D3 PRO" -> "Sunmi D3 PRO". Returns "" when the device said nothing,
// leaving the product key as the label.
func modelLabel(manufacturer, model string) string {
	man, mod := strings.TrimSpace(manufacturer), strings.TrimSpace(model)
	if mod == "" {
		return ""
	}
	if man == "" {
		return mod
	}
	// Manufacturers shout their name ("SUNMI", "SAMSUNG"); title-case it, and don't
	// repeat it when the model already carries it ("Pixel 8" from "Google").
	man = strings.ToUpper(man[:1]) + strings.ToLower(man[1:])
	if strings.HasPrefix(strings.ToLower(mod), strings.ToLower(man)) {
		return mod
	}
	return man + " " + mod
}

// roleSeesDev reports whether a role may see releases still marked dev. Everyone
// who works the fleet can: the flag keeps work in progress out of the DEFAULT view
// (the list folds dev releases away behind a toggle), not out of reach — so anyone
// with OTA rights can push one when that is what the job needs. Viewers, who only
// read, and owners, who see no fleet at all, do not.
func roleSeesDev(role string) bool {
	switch role {
	case "", "viewer", "owner":
		return false
	}
	return true
}

// visibleReleases drops dev-only releases for roles that may not see them.
func visibleReleases(role string, in []db.Release) []db.Release {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, rel := range in {
		if !rel.IsDev {
			out = append(out, rel)
		}
	}
	return out
}

// visibleRail is visibleReleases for the compact release rail.
func visibleRail(role string, in []db.ReleaseRailItem) []db.ReleaseRailItem {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, rel := range in {
		if !rel.IsDev {
			out = append(out, rel)
		}
	}
	return out
}

// visibleDeployments drops rollouts of dev-only releases for the same roles.
func visibleDeployments(role string, in []db.Update) []db.Update {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, u := range in {
		if !u.ReleaseIsDev {
			out = append(out, u)
		}
	}
	return out
}

func (h *Handler) ReleaseList(w http.ResponseWriter, r *http.Request) {
	releases, err := h.db.ListReleases(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Dev releases are invisible outside dev/admin: a build a device happens to
	// report still shows as an untracked version, which is honest — the fleet is
	// running it — but it carries none of the release's detail.
	if !roleSeesDev(h.role(r)) {
		kept := releases[:0]
		for _, rel := range releases {
			if !rel.IsDev {
				kept = append(kept, rel)
			}
		}
		releases = kept
	}
	// Merge tracked releases with the versions devices actually report, into one
	// version-centric table (fleet versions come pre-sorted by adoption desc).
	fleet, _ := h.db.GetFleetVersions(r.Context())
	hiddenVersions, _ := h.db.ListHiddenVersions(r.Context())
	problemsByRelease, _ := h.db.ProblemSummariesByRelease(r.Context())
	// The list-row problem badge should reflect what the workspace board shows —
	// native PLUS carried-forward problems — so a build whose only open blockers are
	// inherited doesn't read "0 open" in the list.
	carriedByRelease, _ := h.db.CarriedProblemCountsByRelease(r.Context())
	badgeFor := func(id int) db.ProblemSummary {
		s := problemsByRelease[id]
		c := carriedByRelease[id]
		s.Open += c.Open
		s.Blockers += c.Blockers
		return s
	}
	relByVersion := make(map[string]db.Release, len(releases))
	relByID := make(map[int]db.Release, len(releases))
	for _, rel := range releases {
		relByVersion[rel.Version] = rel
		relByID[rel.ID] = rel
	}
	// Mainline release versions for naming-based branch matching (longest prefix wins), so
	// a reported build like "v2.0.3-t1" is recognised as a variant of release "v2.0.3".
	type mrel struct {
		id  int
		ver string
	}
	var mainline []mrel
	for _, rel := range releases {
		if !rel.IsBranch {
			mainline = append(mainline, mrel{rel.ID, rel.Version})
		}
	}
	bestBranchParent := func(build string) (*int, string) {
		bestLen := -1
		var bid int
		var bver string
		for _, m := range mainline {
			if strings.HasPrefix(build, m.ver+"-") && len(m.ver) > bestLen {
				bestLen, bid, bver = len(m.ver), m.id, m.ver
			}
		}
		if bestLen < 0 {
			return nil, ""
		}
		return &bid, bver
	}
	var active, hidden []versionRow
	var trackedCount, notTrackedCount int
	var untrackedBuilds []string // all untracked reported builds, for the New-branch picker
	seen := make(map[string]bool)
	childrenByParent := map[int][]versionRow{}
	// Optional per-product filter (?product=): a release/build is kept when its product
	// resolves to the selected one (empty/legacy -> t7). Empty filter shows everything.
	filterProduct := strings.TrimSpace(r.URL.Query().Get("product"))
	productMatches := func(p string) bool {
		if filterProduct == "" {
			return true
		}
		want, _ := product.Resolve(filterProduct)
		got, _ := product.Resolve(p)
		return want.Key == got.Key
	}
	addRow := func(row versionRow) {
		if !productMatches(row.Product) {
			return
		}
		// Branch builds and adoptable derivative builds hang off their parent — collected
		// by parent id and rendered indented beneath it in the list (no separate tab).
		if row.IsBranch || row.AdoptableParentID != nil {
			pid := 0
			if row.AdoptableParentID != nil {
				pid = *row.AdoptableParentID
			} else if row.ParentID != nil {
				pid = *row.ParentID
			}
			childrenByParent[pid] = append(childrenByParent[pid], row)
			return
		}
		if row.Hidden {
			hidden = append(hidden, row)
			return
		}
		if row.Tracked {
			trackedCount++
		} else {
			notTrackedCount++
		}
		active = append(active, row)
	}
	branchRow := func(row *versionRow, rel db.Release) {
		row.IsBranch = rel.IsBranch
		if rel.ParentReleaseID != nil {
			row.ParentID = rel.ParentReleaseID
			row.ParentVersion = relByID[*rel.ParentReleaseID].Version
		}
		// Merge lineage: a merged branch is terminal; a mainline node born from a merge
		// carries a back-reference to the branch it absorbed. Both drive list chips.
		if rel.MergedIntoReleaseID != nil {
			row.Merged = true
			row.MergedIntoVersion = relByID[*rel.MergedIntoReleaseID].Version
		}
		if rel.MergedFromReleaseID != nil {
			row.MergedFromVersion = relByID[*rel.MergedFromReleaseID].Version
		}
	}
	for _, fv := range fleet {
		seen[fv.Version] = true
		row := versionRow{Version: fv.Version, DeviceCount: fv.DeviceCount, Product: fv.Product}
		if rel, ok := relByVersion[fv.Version]; ok {
			id := rel.ID
			row.Tracked, row.ReleaseID, row.Name = true, &id, rel.Name
			row.Product = rel.Product
			row.Changelog = rel.Changelog
			row.Status, row.Hidden, row.IsDev = rel.Status, rel.Hidden, rel.IsDev
			ca := rel.CreatedAt
			row.ReleasedAt = &ca
			row.PackageCount, row.DeployCount = rel.PackageCount, rel.DeployCount
			row.SignedOffBy, row.SignedOffAt = rel.SignedOffBy, rel.SignedOffAt
			row.TestingDone = rel.TestingDoneAt != nil
			branchRow(&row, rel)
			row.QA, _ = h.db.ReleaseQASummary(r.Context(), rel.ID)
			row.Problems = badgeFor(rel.ID)
			row.QfilURL = h.latestQfilURL(r, rel.ID)
		} else {
			row.Hidden = hiddenVersions[fv.Version] // not-tracked versions dismissed by ops
			if !row.Hidden {
				untrackedBuilds = append(untrackedBuilds, fv.Version)
				// Auto-match by naming: an untracked reported build that looks like a variant
				// of a mainline release is shown under it for one-click adoption.
				if pid, pver := bestBranchParent(fv.Version); pid != nil {
					row.AdoptableParentID, row.ParentVersion = pid, pver
				}
			}
		}
		addRow(row)
	}
	// Tracked releases nobody is running yet (not in the fleet list).
	for _, rel := range releases {
		if seen[rel.Version] {
			continue
		}
		id := rel.ID
		ca := rel.CreatedAt
		qa, _ := h.db.ReleaseQASummary(r.Context(), rel.ID)
		row := versionRow{
			Version: rel.Version, Tracked: true, ReleaseID: &id, Name: rel.Name,
			Product:    rel.Product,
			Changelog:  rel.Changelog,
			ReleasedAt: &ca,
			Status:     rel.Status, Hidden: rel.Hidden, IsDev: rel.IsDev,
			PackageCount: rel.PackageCount, DeployCount: rel.DeployCount, QA: qa,
			Problems:    badgeFor(rel.ID),
			SignedOffBy: rel.SignedOffBy, SignedOffAt: rel.SignedOffAt,
			TestingDone: rel.TestingDoneAt != nil,
			QfilURL:     h.latestQfilURL(r, rel.ID),
		}
		branchRow(&row, rel)
		addRow(row)
	}
	// Order strictly by release date, newest first. Undated rows (reported builds
	// that aren't managed releases) have no release date and sort to the bottom.
	// The legacy manual drag order is no longer applied — the drag handle was
	// removed from the list and its version_order table is stale/unreachable.
	sort.SliceStable(active, func(i, j int) bool {
		ri, rj := active[i].ReleasedAt, active[j].ReleasedAt
		if ri == nil || rj == nil {
			return ri != nil // dated (ri!=nil) sorts before undated; two undated keep order
		}
		return ri.After(*rj)
	})
	// Attach each release's branch forks (+ adoptable derivative builds) so they render
	// nested beneath it, and suggest the next branch version (version + -tN).
	for i := range active {
		if active[i].ReleaseID == nil {
			continue
		}
		pid := *active[i].ReleaseID
		active[i].Children = childrenByParent[pid]
		delete(childrenByParent, pid)
		n := 0
		for _, c := range active[i].Children {
			if c.IsBranch {
				n++
			}
		}
		active[i].SuggestedBranch = fmt.Sprintf("%s-t%d", active[i].Version, n+1)
	}
	// Orphan forks (parent hidden or not shown) — surface standalone so they aren't lost.
	for _, kids := range childrenByParent {
		active = append(active, kids...)
	}
	role := h.role(r)
	// Hidden releases are admin-only housekeeping — don't surface them (or the
	// "N hidden" count) to operators/operators.
	if role != "admin" {
		hidden = nil
	}
	fleetTotal, _ := h.db.CountDevices(r.Context(), db.DeviceFilter{})

	// Hub: the release currently under test drives the focus band, and every problem
	// across all releases drives the "all problems at a glance" board.
	activeRel, _ := h.db.ActiveRelease(r.Context())
	var activeQA db.QASummary
	var activeProblems db.ProblemSummary
	activeCarried := 0
	if activeRel != nil {
		activeQA, _ = h.db.ReleaseQASummary(r.Context(), activeRel.ID)
		activeProblems, _ = h.db.ReleaseProblemSummary(r.Context(), activeRel.ID)
		if carried, err := h.db.CarriedForwardProblems(r.Context(), activeRel.ID); err == nil {
			activeCarried = len(carried)
		}
	}
	problemBoard, _ := h.db.ListProblemBoard(r.Context())
	globalProblems, _ := h.db.GlobalProblemSummary(r.Context())

	// Map release id → version to resolve merge relationships for the timeline.
	verByID := make(map[int]string, len(releases))
	for _, rel := range releases {
		verByID[rel.ID] = rel.Version
	}
	// Release train: the mainline timeline — visible (non-hidden), non-branch builds
	// including the current draft, most recent 6, shown oldest→newest. `releases` is
	// newest-first. A merged branch shows which mainline release it merged into.
	var trainRels []db.Release
	for _, rel := range releases {
		if rel.Hidden || rel.IsBranch {
			continue
		}
		trainRels = append(trainRels, rel)
		if len(trainRels) == 6 {
			break
		}
	}
	// Branch forks hang off their parent node in the timeline; a merged branch also
	// carries the version it merged into so the graph can show the merge back to main.
	trainChildren := map[int][]map[string]any{}
	for _, rel := range releases {
		if !rel.IsBranch || rel.ParentReleaseID == nil || rel.Hidden {
			continue
		}
		pid := *rel.ParentReleaseID
		mergedInto := ""
		if rel.MergedIntoReleaseID != nil {
			mergedInto = verByID[*rel.MergedIntoReleaseID]
		}
		trainChildren[pid] = append(trainChildren[pid], map[string]any{
			"ID": rel.ID, "Version": rel.Version, "Name": rel.Name, "Status": rel.Status,
			"MergedInto": mergedInto, "Merged": mergedInto != "",
		})
	}
	var releaseTrain []map[string]any
	for i := len(trainRels) - 1; i >= 0; i-- {
		rel := trainRels[i]
		mergedFrom := ""
		if rel.MergedFromReleaseID != nil {
			mergedFrom = verByID[*rel.MergedFromReleaseID]
		}
		releaseTrain = append(releaseTrain, map[string]any{
			"ID":          rel.ID,
			"Version":     rel.Version,
			"Name":        rel.Name,
			"Status":      rel.Status,
			"SignedOffBy": rel.SignedOffBy,
			"Open":        problemsByRelease[rel.ID].Open,
			"Active":      activeRel != nil && activeRel.ID == rel.ID,
			"Branches":    trainChildren[rel.ID],
			"MergedFrom":  mergedFrom,
		})
	}

	// KPI strip counts (releases.html cc-kpis): published / draft / under-test tracked
	// releases in the current (product-filtered) view, plus the newest published
	// release's fleet adoption for the page verdict headline.
	publishedCount, draftCount, underTestCount, devCount := 0, 0, 0, 0
	latestPublishedPct := 0
	for _, v := range active {
		if !v.Tracked {
			continue
		}
		if v.IsDev {
			devCount++
		}
		switch v.Status {
		case "published":
			publishedCount++
			if latestPublishedPct == 0 && v.ReleasedAt != nil && fleetTotal > 0 {
				latestPublishedPct = v.DeviceCount * 100 / fleetTotal
			}
		case "draft":
			draftCount++
		}
		if !v.TestingDone {
			underTestCount++
		}
	}

	data := map[string]any{
		"Title":               "Releases",
		"Versions":            active,
		"HiddenReleases":      hidden,
		"UntrackedBuilds":     untrackedBuilds,
		"TrackedCount":        trackedCount,
		"NotTrackedCount":     notTrackedCount,
		"PublishedCount":      publishedCount,
		"DraftCount":          draftCount,
		"UnderTestCount":      underTestCount,
		"DevCount":            devCount,
		"LatestPublishedPct":  latestPublishedPct,
		"FleetTotal":      fleetTotal,
		"ActiveRelease":   activeRel,
		"ActiveQA":        activeQA,
		"ActiveProblems":  activeProblems,
		"ActiveCarried":   activeCarried,
		"ProblemBoard":    problemBoard,
		"GlobalProblems":  globalProblems,
		"ReleaseTrain":    releaseTrain,
		"Graph":           buildReleaseGraph(releases),
		"Products":        h.releaseProductFilters(r.Context()),
		"FilterProduct":   r.URL.Query().Get("product"),
	}
	// Live "OTA in progress" summary card — devices mid-OTA with their last-reported
	// percent. The card polls itself (htmx) via ?partial=ota-progress.
	activeOTA, _ := h.db.ActiveOTADevices(r.Context())
	// Overlay the live in-memory OTA progress (updated on every ota_progress WS frame —
	// verifying/finalizing land here in real time) over the lagging latest_extra copy.
	for i := range activeOTA {
		if p := h.shell.GetOTAProgress(activeOTA[i].DeviceID); p != nil {
			if p.Phase != "" {
				activeOTA[i].Phase = p.Phase
			}
			if p.Percent > 0 {
				activeOTA[i].Percent = p.Percent
			}
			// A device whose live progress is flowing is actively downloading/installing,
			// even if its update_devices row still reads "pending".
			if activeOTA[i].Status == "pending" {
				activeOTA[i].Status = "downloading"
			}
		}
	}
	data["ActiveOTA"] = activeOTA
	if r.URL.Query().Get("partial") == "ota-progress" {
		_ = h.tmpl.ExecuteTemplate(w, "ota-progress", h.withRole(r, data))
		return
	}
	h.render(w, r, "releases.html", data)
}

// ── Release lineage graph ─────────────────────────────────────────────────────
// A commit-style graph of the single main line: mainline releases form the spine
// (oldest→newest), each branch forks off its base node and, if merged, rejoins the
// main line at the successor it merged into. Coordinates are computed server-side and
// rendered as inline SVG by releases.html.

type relGraphNode struct {
	ID       int
	Label    string
	Sub      string
	IsBranch bool
	Merged   bool
	Fill     string
	X, Y, R  int
}

type relGraphEdge struct {
	Path string // SVG path 'd'
	Kind string // main | fork | merge
}

type relGraph struct {
	Nodes []relGraphNode
	Edges []relGraphEdge
	W, H  int
}

func statusFill(status string) string {
	switch status {
	case "published":
		return "#1c7d50"
	case "draft":
		return "#9a6512"
	default:
		return "#9aa0aa"
	}
}

// buildReleaseGraph lays out the release lineage. Mainline nodes are spaced evenly along
// the spine; branches are packed into stacked lanes above it (a greedy interval colouring
// so non-overlapping forks share a lane), with a fork edge down to their base and a merge
// edge across to the release they merged into.
func buildReleaseGraph(rels []db.Release) relGraph {
	var main, branch []db.Release
	for _, r := range rels {
		if r.Hidden {
			continue
		}
		if r.IsBranch {
			branch = append(branch, r)
		} else {
			main = append(main, r)
		}
	}
	if len(main)+len(branch) == 0 {
		return relGraph{}
	}
	sort.Slice(main, func(i, j int) bool { return main[i].CreatedAt.Before(main[j].CreatedAt) })
	sort.Slice(branch, func(i, j int) bool { return branch[i].CreatedAt.Before(branch[j].CreatedAt) })

	const stepX, marginX, laneStep, topPad, botPad, nodeR = 175, 72, 46, 30, 56, 7
	mainX := map[int]int{}
	for i, r := range main {
		mainX[r.ID] = marginX + i*stepX
	}
	baseXof := func(b db.Release) int {
		if b.ParentReleaseID != nil {
			if x, ok := mainX[*b.ParentReleaseID]; ok {
				return x
			}
		}
		return marginX
	}

	branchX := map[int]int{}
	laneOf := map[int]int{}
	var laneEnds []int // rightmost x used on each lane so far
	for _, b := range branch {
		baseX := baseXof(b)
		bx, x2 := baseX+stepX/2, baseX+stepX/2+nodeR
		if b.MergedIntoReleaseID != nil {
			if mx, ok := mainX[*b.MergedIntoReleaseID]; ok {
				bx, x2 = (baseX+mx)/2, mx
			}
		}
		lane := -1
		for l, end := range laneEnds {
			if end < baseX-24 {
				lane, laneEnds[l] = l, x2
				break
			}
		}
		if lane == -1 {
			lane = len(laneEnds)
			laneEnds = append(laneEnds, x2)
		}
		branchX[b.ID], laneOf[b.ID] = bx, lane
	}

	laneCount := len(laneEnds)
	spineY := topPad + laneCount*laneStep
	laneY := func(l int) int { return spineY - (l+1)*laneStep }
	// Width spans the rightmost node (a branch forking off the last main node can sit past
	// the spine) plus a margin so its label isn't clipped.
	maxX := marginX
	for _, x := range mainX {
		if x > maxX {
			maxX = x
		}
	}
	for _, x := range branchX {
		if x > maxX {
			maxX = x
		}
	}
	g := relGraph{W: maxX + marginX, H: spineY + botPad}

	// Edges first so nodes paint on top.
	for i := 1; i < len(main); i++ {
		x0, x1 := mainX[main[i-1].ID], mainX[main[i].ID]
		g.Edges = append(g.Edges, relGraphEdge{Kind: "main", Path: fmt.Sprintf("M%d %d L%d %d", x0, spineY, x1, spineY)})
	}
	for _, b := range branch {
		bx, by := branchX[b.ID], laneY(laneOf[b.ID])
		baseX := baseXof(b)
		ymid := (spineY + by) / 2
		g.Edges = append(g.Edges, relGraphEdge{Kind: "fork", Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", baseX, spineY, baseX, ymid, bx, ymid, bx, by)})
		if b.MergedIntoReleaseID != nil {
			if mx, ok := mainX[*b.MergedIntoReleaseID]; ok {
				g.Edges = append(g.Edges, relGraphEdge{Kind: "merge", Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", bx, by, bx, ymid, mx, ymid, mx, spineY)})
			}
		}
	}
	for _, r := range main {
		g.Nodes = append(g.Nodes, relGraphNode{ID: r.ID, Label: r.Version, Sub: r.Status, Fill: statusFill(r.Status), X: mainX[r.ID], Y: spineY, R: nodeR})
	}
	for _, b := range branch {
		g.Nodes = append(g.Nodes, relGraphNode{ID: b.ID, Label: b.Version, Sub: "branch", IsBranch: true, Merged: b.MergedIntoReleaseID != nil, X: branchX[b.ID], Y: laneY(laneOf[b.ID]), R: nodeR - 1})
	}
	return g
}

// ReleaseTrack starts tracking a device-reported version: it creates (or finds) a draft
// release for that version and drops the user on its detail page to add changelog/packages.
func (h *Handler) ReleaseTrack(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, "/releases", http.StatusSeeOther)
		return
	}
	// A version tracked from the live fleet inherits the product of the devices on it.
	product, _ := h.db.ProductForVersion(r.Context(), version)
	rel, err := h.db.GetOrCreateRelease(r.Context(), version, product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.track", version, "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", rel.ID), http.StatusSeeOther)
}

// ReorderVersions saves the drag-and-drop order of the releases list (JSON body
// {"versions": [...]} in display order). Returns 204 on success (called via fetch).
func (h *Handler) ReorderVersions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Versions []string `json:"versions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVersionOrder(r.Context(), body.Versions); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.reorder", "", strconv.Itoa(len(body.Versions)))
	w.WriteHeader(http.StatusNoContent)
}

// VersionHide dismisses a not-tracked device-reported version from the releases list
// (and from the "Add as release" options). VersionUnhide brings it back.
func (h *Handler) VersionHide(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version != "" {
		if err := h.db.HideVersion(r.Context(), version); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "version.hide", version, "")
	}
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

func (h *Handler) VersionUnhide(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version != "" {
		if err := h.db.UnhideVersion(r.Context(), version); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "version.unhide", version, "")
	}
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// ReleaseSetHidden hides/unhides a release from the main list (irrelevant releases).
func (h *Handler) ReleaseSetHidden(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	hidden := r.FormValue("hidden") == "1"
	if err := h.db.SetReleaseHidden(r.Context(), id, hidden); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	state := "unhidden"
	if hidden {
		state = "hidden"
	}
	h.audit(r, "release.hide", strconv.Itoa(id), state)
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// ReleaseDelete hard-deletes a release (cascades to its packages + deployments).
func (h *Handler) ReleaseDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	// A release that devices are still running can't be deleted — hide it instead
	// (keeps the changelog), or move those devices to another build first.
	if rel, err := h.db.GetRelease(r.Context(), id); err == nil {
		if n, err := h.db.CountDevicesByVersion(r.Context(), rel.Version, rel.Product); err == nil && n > 0 {
			http.Error(w, fmt.Sprintf("Cannot delete: %d device(s) are still running %s. Hide it instead (keeps the changelog), or move those devices to another build first.", n, rel.Version), http.StatusConflict)
			return
		}
	}
	if err := h.db.DeleteRelease(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.delete", strconv.Itoa(id), "")
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// createPackageFromForm parses the package form, decodes the base64 update_url
// (WAF bypass), validates, and creates the package — auto-creating/linking its
// release. When forcedTargetBuild is non-empty (adding to an existing release)
// it overrides the form's target build. Returns the created package or false
// after having written an error response.
func (h *Handler) createPackageFromForm(w http.ResponseWriter, r *http.Request, forcedTargetBuild, forcedProduct string) (*db.OTAPackage, bool) {
	r.ParseForm()

	typ := r.FormValue("type")
	if typ != "full" && typ != "incremental" {
		typ = "full"
	}
	targetBuildID := strings.TrimSpace(r.FormValue("target_build_id"))
	if forcedTargetBuild != "" {
		targetBuildID = forcedTargetBuild
	}
	sourceBuildID := strings.TrimSpace(r.FormValue("source_build_id"))
	updateURL := strings.TrimSpace(r.FormValue("update_url"))
	// The dashboard sends update_url as URL-safe base64 (update_url_b64) so an
	// upstream WAF doesn't see the raw URL — an internal OTA host like
	// http://10.0.0.5:3001/update.zip otherwise trips managed SSRF/RFI rules and
	// the ALB rejects the POST with a 403 before it reaches us. Plain update_url
	// stays as a fallback for API callers that don't encode.
	if b64 := strings.TrimSpace(r.FormValue("update_url_b64")); b64 != "" {
		dec, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			http.Error(w, "invalid update_url encoding", http.StatusBadRequest)
			return nil, false
		}
		updateURL = strings.TrimSpace(string(dec))
	}
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	// Product tags a NEWLY-created release. When adding a package to an EXISTING release
	// the caller forces the release's own product — otherwise an empty form product would
	// resolve to t7 in CreateOTAPackage and silently attach the package to (or create) a
	// DIFFERENT t7 release of the same version instead of this one.
	product := strings.TrimSpace(r.FormValue("product"))
	if forcedProduct != "" {
		product = forcedProduct
	}

	if targetBuildID == "" || updateURL == "" {
		http.Error(w, "target_build_id and update_url are required", http.StatusBadRequest)
		return nil, false
	}
	if typ == "incremental" && sourceBuildID == "" {
		http.Error(w, "source_build_id is required for incremental updates", http.StatusBadRequest)
		return nil, false
	}

	pkg, err := h.db.CreateOTAPackage(r.Context(), typ, targetBuildID, sourceBuildID, updateURL, changelog, product, time.Now().UTC())
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return pkg, true
}

// ReleaseCreate creates an empty release identified by its build id (the unique
// version). Packages — the full image and any incrementals — are added
// afterward on the release page.
func (h *Handler) ReleaseCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Error(w, "release build id is required", http.StatusBadRequest)
		return
	}
	product := strings.TrimSpace(r.FormValue("product"))
	rel, err := h.db.GetOrCreateRelease(r.Context(), version, product)
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if name != "" || changelog != "" {
		_ = h.db.SetReleaseMeta(r.Context(), rel.ID, name, changelog)
	}
	// New releases start dev (the form's checkbox is checked by default), so work in
	// progress is invisible to everyone but devs and admins until it is turned off.
	_ = h.db.SetReleaseDev(r.Context(), rel.ID, r.FormValue("is_dev") == "1")
	h.audit(r, "release.create", version, "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", rel.ID), http.StatusSeeOther)
}

// ReleaseAddPackage adds another package (e.g. an incremental) to an existing
// release; the target build is fixed to the release's version.
func (h *Handler) ReleaseAddPackage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// A release may hold exactly one full image (the baseline); additional
	// packages must be incrementals. Reject a second full server-side — the UI
	// also greys out the Full button once one exists.
	if r.FormValue("type") == "full" {
		pkgs, _ := h.db.ListPackagesByRelease(r.Context(), id)
		for _, p := range pkgs {
			if p.Type == "full" && p.Status == "active" {
				http.Error(w, "This release already has a full package. Add an incremental instead.", http.StatusBadRequest)
				return
			}
		}
	}
	if _, ok := h.createPackageFromForm(w, r, rel.Version, rel.Product); !ok {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// packageURLFromForm reads the OTA package URL from the add/inspect form, honoring the
// base64 (WAF-bypass) field the dashboard sends; falls back to the plain field.
func packageURLFromForm(r *http.Request) string {
	if b64 := strings.TrimSpace(r.FormValue("update_url_b64")); b64 != "" {
		if dec, err := base64.RawURLEncoding.DecodeString(b64); err == nil {
			return strings.TrimSpace(string(dec))
		}
	}
	return strings.TrimSpace(r.FormValue("update_url"))
}

// PackageInspect reads an OTA package's metadata straight from its URL (HTTP range
// requests — no multi-GB download) so the Add-package composer can auto-fill type and
// source/target build. Admin-only; returns JSON the composer consumes.
func (h *Handler) PackageInspect(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if _, err := h.db.GetRelease(r.Context(), id); err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	url := packageURLFromForm(r)
	if url == "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "Enter a package URL first."})
		return
	}
	m, err := ota.Inspect(r.Context(), url)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "Couldn't read the package: " + err.Error()})
		return
	}
	typ := "full"
	if m.IsIncremental {
		typ = "incremental"
	}
	// Deliberately NOT surfaced: any build id from the fingerprint. The device
	// fingerprint (and ro.build.id) is held constant for Play Integrity, so it doesn't
	// identify the version — reporting it would be misleading. We return what IS
	// meaningful: full vs incremental, the target device, and size/reachability.
	json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"size_bytes":     m.SizeBytes,
		"ota_type":       m.OTAType,
		"type":           typ,
		"is_incremental": m.IsIncremental,
		"device":         m.Device,
	})
}

// ReleaseCrashDelete removes a crash from a release's auto-detected crash list (admin
// cleanup of a noisy/irrelevant crash) and clears the device's crash alert, so it also
// disappears from the Alerts page.
func (h *Handler) ReleaseCrashDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	eid, err := uuid.Parse(r.PathValue("eid"))
	if err != nil {
		http.Error(w, "Invalid crash ID", http.StatusBadRequest)
		return
	}
	deviceID, ok, err := h.db.DeleteCrashEvent(r.Context(), eid)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ok {
		if err := h.db.DeleteDeviceCrashAlert(r.Context(), deviceID); err == nil {
			h.hub.PublishAlertUpdate() // refresh the Alerts page live
		}
	}
	h.audit(r, "release.crash.delete", strconv.Itoa(id), eid.String())
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/qa", id), http.StatusSeeOther)
}

// ReleaseCrashes is the full, paginated list of a release's auto-detected crashes,
// collapsed into signature groups (busiest first). Reached from the workspace's "Show
// all …" link when there are more crash groups than the workspace previews.
func (h *Handler) ReleaseCrashes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	const pageSize = 25
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	groups, total, _ := h.db.CrashGroupsOnBuild(r.Context(), rel.Version, pageSize, (page-1)*pageSize)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	// An out-of-range page (stale link) returns no rows and total 0; clamp and re-query so
	// the pager and count stay honest.
	if len(groups) == 0 && page > 1 {
		page = 1
		groups, total, _ = h.db.CrashGroupsOnBuild(r.Context(), rel.Version, pageSize, 0)
		totalPages = (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
	}
	h.render(w, r, "release_crashes.html", map[string]any{
		"Title":       "Crashes · " + rel.Version,
		"Release":     rel,
		"CrashGroups": groups,
		"Page":        page,
		"Total":       total,
		"TotalPages":  totalPages,
	})
}

// ReleaseCrashGroupDelete removes every crash matching a group's kind + signature on this
// build (admin cleanup of a noisy crash class) and clears each affected device's crash
// alert, so the group also disappears from the Alerts page.
func (h *Handler) ReleaseCrashGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	kind := strings.TrimSpace(r.FormValue("kind"))
	sig := strings.TrimSpace(r.FormValue("signature"))
	if kind == "" || sig == "" {
		http.Error(w, "kind and signature are required", http.StatusBadRequest)
		return
	}
	devIDs, err := h.db.DeleteCrashGroup(r.Context(), rel.Version, kind, sig)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	for _, dev := range devIDs {
		_ = h.db.DeleteDeviceCrashAlert(r.Context(), dev)
	}
	if len(devIDs) > 0 {
		h.hub.PublishAlertUpdate() // refresh the Alerts page live
	}
	h.audit(r, "release.crash.group.delete", strconv.Itoa(id), kind+" "+sig)
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if !strings.HasPrefix(redirect, "/releases") {
		redirect = fmt.Sprintf("/releases/%d/qa", id)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ReleaseAddQFIL attaches a QFIL flashing bundle to a release. Admin/dev only
// (see the requireAdmin route guard); the test team reads the resulting list on
// the release page. The bundle is referenced by an external URL like an OTA
// package — the URL may be base64-encoded (qfil_url_b64) to slip past an upstream
// WAF, mirroring the OTA package form.
func (h *Handler) ReleaseAddQFIL(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if _, err := h.db.GetRelease(r.Context(), id); err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	url := strings.TrimSpace(r.FormValue("qfil_url"))
	if b64 := strings.TrimSpace(r.FormValue("qfil_url_b64")); b64 != "" {
		dec, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			http.Error(w, "invalid qfil_url encoding", http.StatusBadRequest)
			return
		}
		url = strings.TrimSpace(string(dec))
	}
	if url == "" {
		http.Error(w, "qfil_url is required", http.StatusBadRequest)
		return
	}
	// A release carries a single QFIL bundle — adding one replaces any existing.
	if err := h.db.DeleteQFILPackagesByRelease(r.Context(), id); err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := h.db.CreateQFILPackage(r.Context(), id, label, url, notes, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.qfil.add", strconv.Itoa(id), label)
	// Return to wherever the edit came from (the list uses redirect=/releases);
	// only same-app release paths are honoured, defaulting to the release page.
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if !strings.HasPrefix(redirect, "/releases") {
		redirect = fmt.Sprintf("/releases/%d", id)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ReleaseDeleteQFIL removes a QFIL bundle from a release. Admin/dev only.
func (h *Handler) ReleaseDeleteQFIL(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	qid, err := strconv.Atoi(r.PathValue("qid"))
	if err != nil {
		http.Error(w, "Invalid QFIL ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteQFILPackage(r.Context(), qid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.qfil.delete", strconv.Itoa(id), strconv.Itoa(qid))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// ReleaseWorkspace is the unified per-release workspace: Overview / QA / Problems /
// Packages / Rollout tabs on one page (release_workspace.html). It replaces the old
// split between the release detail page and a separate QA page. ReleaseDetail serves
// /releases/{id} (tab from ?tab=, default overview); ReleaseQAPage serves the legacy
// /releases/{id}/qa URL as a deep-link to the QA tab.
func (h *Handler) ReleaseDetail(w http.ResponseWriter, r *http.Request) {
	h.renderReleaseWorkspace(w, r, r.URL.Query().Get("tab"))
}


func (h *Handler) renderReleaseWorkspace(w http.ResponseWriter, r *http.Request, tab string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// Live refresh: a hidden element on the workspace refetches ?partial=problems on the
	// problem-updated body event, and the returned OOB fragments swap the QA summary,
	// problem lists and tab badges in place — so another user's change lands here too.
	if r.URL.Query().Get("partial") == "problems" {
		h.writeReleaseQAResponse(w, r, id, nil)
		return
	}
	h.render(w, r, "release_workspace.html", h.releaseWorkspaceData(r, rel, tab))
}

// releaseWorkspaceData assembles the union of the release overview data and the
// QA/Problems data for the unified tabbed workspace. It builds on releaseQAData (which
// supplies Release/Role/Checklist/QA/Problems/ProblemSummary/CanRecord/CanReport) and
// adds packages, deployments, crashes and rollout facts.
func (h *Handler) releaseWorkspaceData(r *http.Request, rel *db.Release, tab string) map[string]any {
	ctx := r.Context()
	data := h.releaseQAData(r, rel)
	role, _ := data["Role"].(string)

	packages, _ := h.db.ListPackagesByRelease(ctx, rel.ID)
	qfilPackages, _ := h.db.ListQFILPackagesByRelease(ctx, rel.ID)
	deployments, _ := h.db.ListDeploymentsByRelease(ctx, rel.ID)
	// Adoption count only — links out to the Fleet page (filtered by this build) rather
	// than embedding the device roster.
	devicesCount, _ := h.db.CountDevicesByVersion(ctx, rel.Version, rel.Product)
	// Tracked releases back the incremental package's "From build" picker (its source).
	sourceReleases, _ := h.db.ListReleases(ctx)
	// Top crash groups for the workspace; crashTotal drives the "Show all …" link to the
	// full paginated /releases/{id}/crashes page.
	crashGroups, crashTotal, _ := h.db.CrashGroupsOnBuild(ctx, rel.Version, 5, 0)
	devs, _ := h.db.ListDevices(ctx, db.DeviceFilter{BuildID: rel.Version}, 0, 500, "serial", "asc")

	// hasFull gates the "Add full package" form; canPush gates the deploy CTA. An
	// incremental-only release is still pushable — the per-device resolver matches each
	// incremental to devices on its source build.
	hasFull, canPush := false, false
	for _, p := range packages {
		if p.Status != "active" {
			continue
		}
		canPush = true
		if p.Type == "full" {
			hasFull = true
		}
	}

	switch tab {
	case "overview", "qa", "problems", "packages", "rollout":
		// valid
	default:
		tab = "overview"
	}
	// Non-admins have no Packages tab; fall back so a stale deep-link isn't a blank page.
	if tab == "packages" && !(role == "admin" || role == "dev") {
		tab = "overview"
	}
	// Branch context: a branch build shows a banner linking to its parent. Branch builds
	// themselves are created / listed on the main /releases page (nested under the parent),
	// not as a workspace tab.
	data["IsBranch"] = rel.IsBranch
	if rel.IsBranch && rel.ParentReleaseID != nil {
		if parent, err := h.db.GetRelease(ctx, *rel.ParentReleaseID); err == nil {
			data["Parent"] = parent
		}
	}
	title := "Release " + rel.Version
	switch tab {
	case "qa":
		title = "QA · " + rel.Version
	case "problems":
		title = "Problems · " + rel.Version
	}

	data["Tab"] = tab
	data["Title"] = title
	data["Packages"] = packages
	data["QFILPackages"] = qfilPackages
	data["CanManageQFIL"] = role == "admin" || role == "dev"
	data["Deployments"] = deployments
	data["HasFull"] = hasFull
	data["CanPush"] = canPush
	data["DevicesCount"] = devicesCount
	data["SourceReleases"] = sourceReleases
	data["CrashGroups"] = crashGroups
	data["CrashGroupTotal"] = crashTotal
	data["DevicesOnVersion"] = devs
	return data
}

// ReleaseProblemCreate files a new problem report against a release. Any operator
// or operator (canOperate) can report; the build is snapshotted from the release.
func (h *Handler) ReleaseProblemCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		if hxReq(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"/qa", http.StatusFound)
		return
	}
	p := db.ReleaseProblem{
		ReleaseID:   id,
		BuildID:     rel.Version,
		Title:       title,
		Description: strings.TrimSpace(r.FormValue("description")),
		Severity:    r.FormValue("severity"),
		ReportedBy:  h.currentUsername(r),
	}
	if tc := strings.TrimSpace(r.FormValue("test_case_id")); tc != "" {
		if tcid, err := uuid.Parse(tc); err == nil {
			p.TestCaseID = &tcid
		}
	}
	// Device is picked by serial (the QA-relevant identifier).
	if serial := strings.TrimSpace(r.FormValue("device_serial")); serial != "" {
		if dev, err := h.db.GetDevice(r.Context(), serial); err == nil {
			p.DeviceID = &dev.ID
			if p.BuildID == "" {
				p.BuildID = dev.BuildID
			}
		}
	}
	if _, err := h.db.CreateReleaseProblem(r.Context(), p); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.problem.create", rel.Version, fmt.Sprintf("severity=%s, title=%s", p.Severity, title))
	h.hub.PublishProblemUpdate() // live-refresh the hub board + any open workspace
	if hxReq(r) {
		h.writeReleaseQAResponse(w, r, id, nil)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"/qa", http.StatusFound)
}

// ReleaseProblemUpdate changes a problem's status (and optionally severity).
func (h *Handler) ReleaseProblemUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid problem ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	status := r.FormValue("status")
	// Status transitions record the fix/verify trail against THIS release (the build the
	// user is looking at), so a bug's "fixed in / verified in" is captured as it moves
	// through the release train. Severity-only edits still go through UpdateReleaseProblem.
	var uerr error
	switch status {
	case "fixed":
		uerr = h.db.MarkProblemFixedIn(r.Context(), pid, id)
		// Marking a bug fixed in this build adds a "Verify fix: …" case to QA so the test
		// team confirms it (best-effort; de-duped per release+problem).
		if uerr == nil {
			if e := h.db.EnsureFixVerificationCase(r.Context(), id, pid, h.currentUsername(r)); e != nil {
				log.Printf("[qa] add fix-verify case: %v", e)
			}
		}
	case "verified":
		uerr = h.db.VerifyProblem(r.Context(), pid, id)
	case "open":
		uerr = h.db.MarkProblemFixedIn(r.Context(), pid, 0) // reopen: clears the fix/verify trail
	default: // wontfix, or a severity-only change
		uerr = h.db.UpdateReleaseProblem(r.Context(), pid, status, r.FormValue("severity"))
	}
	if uerr != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.problem.update", strconv.Itoa(id), fmt.Sprintf("problem=%s, status=%s", pid, status))
	h.hub.PublishProblemUpdate()
	if hxReq(r) {
		h.writeReleaseQAResponse(w, r, id, nil)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"/qa", http.StatusFound)
}

// ReleaseProblemDelete removes a problem (admin only).
func (h *Handler) ReleaseProblemDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid problem ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteReleaseProblem(r.Context(), pid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishProblemUpdate()
	if hxReq(r) {
		h.writeReleaseQAResponse(w, r, id, nil)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"/qa", http.StatusFound)
}

// ReleasePublish moves a release to published (deployable).
func (h *Handler) ReleasePublish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseStatus(r.Context(), id, "published"); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.publish", strconv.Itoa(id), "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// ReleaseSignOff records the current dev's smoke-test sign-off on a release
// ("tested at our end, OK for QA to pick up"). Dev-only (see requireDev).
func (h *Handler) ReleaseSignOff(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseSignOff(r.Context(), id, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.sign_off", strconv.Itoa(id), "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/qa", id), http.StatusSeeOther)
}

// ReleaseClearSignOff revokes a previously recorded dev sign-off. Dev-only.
func (h *Handler) ReleaseClearSignOff(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.ClearReleaseSignOff(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.sign_off_clear", strconv.Itoa(id), "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/qa", id), http.StatusSeeOther)
}



// ReleaseCreateBranch forks an off-mainline branch build from a release (admin/dev) — a
// temporary test build that stays off the main path (see CreateBranchRelease). Redirects
// to the new branch's workspace.
func (h *Handler) ReleaseCreateBranch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	parent, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if parent.IsBranch {
		http.Error(w, "Cannot branch from a branch build", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, fmt.Sprintf("/releases/%d?tab=branches", id), http.StatusFound)
		return
	}
	// Seed the branch with the parent's changelog so it starts from the same baseline.
	newID, err := h.db.CreateBranchRelease(r.Context(), id, version, strings.TrimSpace(r.FormValue("name")), parent.Changelog)
	if err != nil {
		http.Error(w, "Could not create branch — that version may already exist.", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.branch.create", version, fmt.Sprintf("from=%s", parent.Version))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", newID), http.StatusSeeOther)
}

// ReleaseMerge merges a validated branch onto the main line as a new draft mainline
// release at the given version. Gated on the branch being testing-done — its proving run
// must be complete before its delta graduates to main. The new release starts as a draft
// so the real v-next build is attached via the normal release-first flow.
func (h *Handler) ReleaseMerge(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	branch, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if !branch.IsBranch {
		http.Error(w, "Only a branch build can be merged to main.", http.StatusBadRequest)
		return
	}
	if branch.MergedIntoReleaseID != nil {
		http.Error(w, "This branch has already been merged.", http.StatusBadRequest)
		return
	}
	if branch.TestingDoneAt == nil {
		http.Error(w, "Finish testing on this branch before merging it to main.", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusFound)
		return
	}
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if changelog == "" {
		changelog = branch.Changelog // start the mainline release from the branch's notes
	}
	carryBuild := r.FormValue("carry_build") == "on"
	newID, err := h.db.MergeBranch(r.Context(), id, branch.Version, version, strings.TrimSpace(r.FormValue("name")), changelog, h.currentUsername(r), carryBuild)
	if err != nil {
		http.Error(w, "Could not merge — that version may already exist.", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.merge", version, fmt.Sprintf("from=%s", branch.Version))
	h.hub.PublishProblemUpdate() // refresh any open releases list (new node + branch closed)
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", newID), http.StatusSeeOther)
}

// ── Test team / QA handlers ──────────────────────────────────────────────────

var validTestStatuses = map[string]bool{
	"untested": true, "pass": true, "fail": true, "blocked": true, "skip": true,
}

// TestCaseCreate adds a base case (from the Releases page) or a release-specific case (when
// release_id is set, e.g. from the release detail page).
func (h *Handler) TestCaseCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	tc := db.TestCase{
		Title:     strings.TrimSpace(r.FormValue("title")),
		Area:      strings.TrimSpace(r.FormValue("area")),
		Steps:     strings.TrimSpace(r.FormValue("steps")),
		Expected:  strings.TrimSpace(r.FormValue("expected")),
		CreatedBy: h.currentUsername(r),
	}
	if tc.Title == "" {
		http.Error(w, "Title required", http.StatusBadRequest)
		return
	}
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if redirect == "" {
		redirect = "/releases"
	}
	if rid := strings.TrimSpace(r.FormValue("release_id")); rid != "" {
		if id, err := strconv.Atoi(rid); err == nil {
			tc.ReleaseID = &id
			redirect = fmt.Sprintf("/releases/%d/qa", id)
		}
	} else {
		tc.Base = true // a library case applies to every release
	}
	if _, err := h.db.CreateTestCase(r.Context(), tc); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "testcase.create", tc.Title, "")
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// TestCaseUpdate edits a base case's content / active flag.
func (h *Handler) TestCaseUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "Title required", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateTestCase(r.Context(), id,
		title, strings.TrimSpace(r.FormValue("area")),
		strings.TrimSpace(r.FormValue("steps")), strings.TrimSpace(r.FormValue("expected")),
		r.FormValue("active") == "on"); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "testcase.update", id.String(), "")
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if redirect == "" {
		redirect = "/releases"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// TestCaseDelete removes a test case (and its results).
func (h *Handler) TestCaseDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/releases"
	}
	if err := h.db.DeleteTestCase(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "testcase.delete", id.String(), "")
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}


// releaseQAData gathers the QA + Problems state that the release page and its htmx
// OOB fragments render from. Keys match what the rd-* partials expect.
func (h *Handler) releaseQAData(r *http.Request, rel *db.Release) map[string]any {
	ctx := r.Context()
	checklist, _ := h.db.GetReleaseChecklist(ctx, rel.ID)
	qa, _ := h.db.ReleaseQASummary(ctx, rel.ID)
	problems, _ := h.db.ListReleaseProblems(ctx, rel.ID)
	ps, _ := h.db.ReleaseProblemSummary(ctx, rel.ID)
	role := h.role(r)
	// Bugs that ride the release train onto this build (reported on an earlier build and
	// not yet verified fixed, plus any claimed fixed in this build awaiting verification).
	// Carry-forward is a dev/admin concern — operators/operators don't see it.
	var carried []db.ReleaseProblem
	if role == "admin" || role == "dev" {
		carried, _ = h.db.CarriedForwardProblems(ctx, rel.ID)
	}
	return map[string]any{
		"Release":        rel,
		"RelVersion":     rel.Version,
		"Role":           role,
		"Checklist":      checklist,
		"QA":             qa,
		"Problems":       problems,
		"Carried":        carried,
		"ProblemSummary": ps,
		"CanRecord":      roleIsOperatorLike(role),
		"CanReport":      roleCanOperate(role),
		"CanAct":         role == "admin" || role == "dev", // act on existing problems (fixed/verified/wontfix) — operators file only
	}
}

// writeReleaseQAResponse renders the htmx response for a QA/Problems mutation: the
// changed qa-row (when markedCase is non-nil) followed by out-of-band fragments that
// refresh the QA summary, problems list, counts and tab badges — so the page updates
// in place with no full reload.
func (h *Handler) writeReleaseQAResponse(w http.ResponseWriter, r *http.Request, releaseID int, markedCase *uuid.UUID) {
	rel, err := h.db.GetRelease(r.Context(), releaseID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	data := h.releaseQAData(r, rel)
	// Everything updates via the out-of-band swap set (which now includes the full QA
	// checklist), so QA marks and problem changes stay live without a reload — including a
	// newly-added "Verify fix" case appearing in the checklist.
	_ = markedCase // retained for call-site clarity; the whole checklist is re-rendered
	_ = h.tmpl.ExecuteTemplate(w, "release-oob", data)
}

// ReleaseEditMeta updates a release's editable metadata (name + changelog) after
// creation, so the changelog stays maintainable through the release lifecycle.
func (h *Handler) ReleaseEditMeta(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if err := h.db.SetReleaseMeta(r.Context(), id, name, changelog); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Optional release-date edit (HTML date input, yyyy-mm-dd). Setting created_at
	// re-dates the release for the list order and the OTA newer/older ranking.
	if d := strings.TrimSpace(r.FormValue("released_at")); d != "" {
		if t, perr := time.Parse("2006-01-02", d); perr == nil {
			_ = h.db.SetReleaseCreatedAt(r.Context(), id, t)
		}
	}
	h.audit(r, "release.edit_meta", strconv.Itoa(id), name)
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}


// PackageDelete removes a single package from a release.
func (h *Handler) PackageDelete(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid package ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteOTAPackage(r.Context(), pid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", relID), http.StatusSeeOther)
}

// UpdatesHub renders the global Updates page: every deployment across releases with
// its progress, plus a "new deployment" composer (admin/dev) that picks a published
// release and its targets. Operators/operators can view the list but not deploy.
// UpdatesHub is the combined Updates landing page: the releases we track and the
// rollouts carrying them, side by side, because a release and its rollout are the
// same story told twice. Legacy (otautil) rollouts join the same list, tagged, so
// what is deploying reads in one place. Full lists live at /releases and
// /updates/rollouts.
func (h *Handler) UpdatesHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = h.db.CompleteSettledDeployments(ctx)

	filterProduct := strings.TrimSpace(r.URL.Query().Get("product"))
	productMatches := func(p string) bool {
		if filterProduct == "" {
			return true
		}
		want, _ := product.Resolve(filterProduct)
		got, _ := product.Resolve(p)
		return want.Key == got.Key
	}

	// ── rollouts: fleet deployments, newest first, with legacy ones merged in ──
	deployments, _ := h.db.ListDeployments(ctx)
	deployments = visibleDeployments(h.role(r), deployments)
	type rolloutRow struct {
		URL        string
		Version    string
		Product    string
		Status     string
		CreatedAt  time.Time
		CreatedBy  string
		Reboot     string
		Legacy     bool
		Total      int
		Installed  int
		Installing int
		Failed     int
		Progress   int // 0-100, the download half and the install half weighted equally
	}

	// A device's journey scores out of 100: the download is half the work and
	// applying it is the other half. phaseScore folds in the live percent so a
	// fleet 70% downloaded reads 35 and climbs, instead of sitting on a flat 25
	// for the whole download and then jumping to done.
	const (
		pctDownloading = 25  // no live frame: assume partway through the first half
		pctInstalling  = 75  // no live frame: downloaded, partway through the second
		pctDone        = 100 // applied, with or without its reboot
	)
	phaseScore := func(phase string, pct int) int {
		installing := phase == "installing" || phase == "verifying" || phase == "finalizing"
		if pct < 0 || pct > 100 { // no live progress frame for this device
			if installing {
				return pctInstalling
			}
			return pctDownloading
		}
		if installing {
			return 50 + pct/2
		}
		return pct / 2
	}
	// Live percents live in memory, not in update_devices, so pull every mid-flight
	// target once and score them per rollout rather than per-rollout queries.
	liveScore := map[int]int{}
	if inflight, err := h.db.ListInProgressTargets(ctx); err == nil {
		for _, t := range inflight {
			phase, pct := t.Status, -1
			if p := h.shell.GetOTAProgress(t.DeviceID); p != nil {
				pct = p.Percent
				if p.Phase != "" {
					phase = p.Phase
				}
			}
			liveScore[t.UpdateID] += phaseScore(phase, pct)
		}
	}
	// legacyScore is the same read for an otautil device, whose live state is keyed
	// by serial and whose last reported percent is also persisted on the row.
	legacyScore := func(dv db.LegacyDeploymentDevice) int {
		phase, pct := dv.Status, -1
		if st, ok := ota.Legacy.Get(dv.Serial); ok {
			if st.Phase != "" {
				phase = st.Phase
			}
			pct = st.Percent
		} else if dv.Percent > 0 {
			pct = dv.Percent
		}
		return phaseScore(phase, pct)
	}
	var rollouts []rolloutRow
	rolloutsByID := map[int]int{}
	active, installing, failed, installed := 0, 0, 0, 0
	for _, d := range deployments {
		if !productMatches(d.Product) {
			continue
		}
		ver := ""
		if d.Release != nil {
			ver = d.Release.Version
		}
		pendingOrFailed := d.DeviceTotal - d.DeviceInstalled - d.DeviceDownloading - d.DeviceInstalling - d.DeviceAwaiting
		_ = pendingOrFailed
		row := rolloutRow{
			URL:       fmt.Sprintf("/releases/%d/deployments/%d", d.ReleaseID, d.ID),
			Version:   ver,
			Product:   d.Product,
			Status:    d.Status,
			CreatedAt: d.CreatedAt,
			CreatedBy: d.CreatedBy,
			Reboot:    d.RebootBehavior,
			Total:     d.DeviceTotal, Installed: d.DeviceInstalled,
			Installing: d.DeviceDownloading + d.DeviceInstalling, Failed: d.DeviceFailed,
		}
		// Weighted score, in points out of 100 per device; divided by the target count
		// below once the legacy half has been folded in.
		row.Progress = (d.DeviceInstalled+d.DeviceAwaiting)*pctDone + liveScore[d.ID]
		rolloutsByID[d.ID] = len(rollouts)
		rollouts = append(rollouts, row)
		if d.Status == "active" {
			active++
		}
		installing += d.DeviceDownloading
		failed += d.DeviceFailed
		installed += d.DeviceInstalled
	}
	// Legacy rollouts carry no product of their own — they are T7 builds by
	// definition — so they show unless another product is being filtered for.
	legacyByUpdate := map[int]db.LegacyDeployment{}
	if legacy, err := h.db.ListLegacyDeployments(ctx); err == nil && productMatches("t7") {
		for _, d := range legacy {
			// Part of a fleet deployment: its devices belong to that rollout's row.
			if d.UpdateID != nil {
				legacyByUpdate[*d.UpdateID] = d
				continue
			}
			ld := rolloutRow{
				URL: fmt.Sprintf("/updates/legacy/deployments/%d", d.ID), Version: d.ReleaseVersion, Product: d.ReleaseProduct,
				Status: d.Status, CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy,
				Legacy: true, Total: d.Total, Installed: d.Installed, Failed: d.Failed,
			}
			for _, dev := range d.Devices {
				switch dev.Status {
				case "installed", "awaiting_reboot":
					ld.Progress += pctDone
				case "installing", "verifying", "finalizing":
					ld.Progress += legacyScore(dev)
					ld.Installing++
				case "offered", "downloading":
					ld.Progress += legacyScore(dev)
					ld.Installing++
				}
			}
			if ld.Total > 0 {
				ld.Progress /= ld.Total
			}
			rollouts = append(rollouts, ld)
			if d.Status == "active" {
				active++
			}
			installing += ld.Installing
			failed += ld.Failed
			installed += ld.Installed
		}
	}
	// Fold each linked legacy half into its deployment's row: one push, one line.
	for updID, d := range legacyByUpdate {
		i, ok := rolloutsByID[updID]
		if !ok {
			continue
		}
		rollouts[i].Legacy = true
		rollouts[i].Total += d.Total
		rollouts[i].Installed += d.Installed
		rollouts[i].Failed += d.Failed
		installed += d.Installed
		failed += d.Failed
		for _, dev := range d.Devices {
			switch dev.Status {
			case "installed":
				rollouts[i].Progress += pctDone
			case "awaiting_reboot":
				rollouts[i].Progress += pctDone
			case "installing", "verifying", "finalizing":
				rollouts[i].Progress += legacyScore(dev)
				rollouts[i].Installing++
				installing++
			case "offered", "downloading":
				rollouts[i].Progress += legacyScore(dev)
				rollouts[i].Installing++
				installing++
			}
		}
	}
	for i := range rollouts {
		if rollouts[i].Total > 0 {
			rollouts[i].Progress /= rollouts[i].Total
		}
	}
	sort.SliceStable(rollouts, func(i, j int) bool { return rollouts[i].CreatedAt.After(rollouts[j].CreatedAt) })
	rolloutTotal := len(rollouts)
	if len(rollouts) > 8 {
		rollouts = rollouts[:8]
	}

	// ── releases: the tracked list, newest first, with fleet adoption ──
	const releasePaneRows = 8
	releases, _ := h.db.ListReleases(ctx)
	releases = visibleReleases(h.role(r), releases)
	adoption := map[string]int{}
	if fleet, err := h.db.GetFleetVersions(ctx); err == nil {
		for _, fv := range fleet {
			// Keyed by product too: a same-version release for another product must
			// not show these devices as on it.
			adoption[fv.Version+"|"+fv.Product] += fv.DeviceCount
		}
	}
	fleetTotal, _ := h.db.CountDevices(ctx, db.DeviceFilter{})
	type releaseRow struct {
		ID       int
		Version  string
		Name     string
		Product  string
		Status   string
		Branch   bool
		Dev      bool
		Packages int
		Deploys  int
		Devices  int
		Pct      int
		Created  time.Time
	}
	var relRows []releaseRow
	published, latestPublishedPct, tracked, devShown, shown := 0, 0, 0, 0, 0
	for _, rel := range releases {
		if rel.Hidden || !productMatches(rel.Product) {
			continue
		}
		tracked++
		n := adoption[rel.Version+"|"+rel.Product]
		pct := 0
		if fleetTotal > 0 {
			pct = n * 100 / fleetTotal
		}
		if rel.Status == "published" {
			published++
			if latestPublishedPct == 0 {
				latestPublishedPct = pct
			}
		}
		// The pane shows the newest few — but dev releases are folded away by default,
		// so counting them against the limit left the default view nearly empty when
		// the recent releases happen to be dev (two rows, six blanks). Fill each set
		// separately: the visible list always holds its eight, and the dev ones ride
		// along for the toggle.
		row := releaseRow{
			ID: rel.ID, Version: rel.Version, Name: rel.Name, Product: rel.Product,
			Status: rel.Status, Branch: rel.IsBranch, Dev: rel.IsDev,
			Packages: rel.PackageCount, Deploys: rel.DeployCount,
			Devices: n, Pct: pct, Created: rel.CreatedAt,
		}
		if rel.IsDev {
			if devShown < releasePaneRows {
				devShown++
				relRows = append(relRows, row)
			}
			continue
		}
		if shown < releasePaneRows {
			shown++
			relRows = append(relRows, row)
		}
	}
	// Back into date order, so revealing the dev ones slots them where they belong
	// rather than appending them under everything else.
	sort.SliceStable(relRows, func(i, j int) bool { return relRows[i].Created.After(relRows[j].Created) })

	// The legacy fleet as a line of state, not a button: how many devices speak the
	// old protocol, how many are mid-update right now, and who is answering their
	// port. All three are things an operator wants to see without clicking.
	legacySeen, _ := h.db.CountLegacyOTADevices(ctx)
	legacyUpdating, legacyRecent := 0, 0
	if devs, err := h.db.ListLegacyOTADevices(ctx); err == nil {
		for _, d := range devs {
			switch d.Status {
			case "offered", "downloading", "installing", "verifying", "finalizing", "awaiting_reboot":
				legacyUpdating++
			}
			if time.Since(d.LastSeen) < 30*time.Minute {
				legacyRecent++
			}
		}
	}
	h.render(w, r, "updates.html", map[string]any{
		"Title":              "Updates",
		"Rollouts":           rollouts,
		"RolloutTotal":       rolloutTotal,
		"ReleaseRows":        relRows,
		"ReleaseTotal":       tracked,
		"DevCount":           devShown,
		"PublishedCount":     published,
		"LatestPublishedPct": latestPublishedPct,
		"ActiveCount":        active,
		"InstallingCount":    installing,
		"FailedCount":        failed,
		"InstalledCount":     installed,
		"LegacySeen":         legacySeen,
		"LegacyUpdating":     legacyUpdating,
		"LegacyRecent":       legacyRecent,
		"LegacyMode":         h.cfg.LegacyOTAMode(),
		"LegacyPort":         os.Getenv("LEGACY_OTA_PORT"),
		"CanDeploy":          roleCanOTA(h.role(r)),
		"Products":           h.releaseProductFilters(ctx),
		"FilterProduct":      filterProduct,
	})
}

// UpdatesRollouts is the full rollout list — every deployment and how it is landing.
// The combined Updates page (UpdatesHub) shows the newest handful and links here.
func (h *Handler) UpdatesRollouts(w http.ResponseWriter, r *http.Request) {
	role := h.role(r)
	_ = h.db.CompleteSettledDeployments(r.Context())
	deployments, _ := h.db.ListDeployments(r.Context())
	deployments = visibleDeployments(role, deployments)
	deployable, _ := h.db.ListDeployableReleases(r.Context())
	deployable = visibleReleases(role, deployable)
	groups, _ := h.db.ListGroups(r.Context())

	// Optional per-product filter (?product=): keep deployments whose release product
	// resolves to the selected one (empty/legacy -> t7). Empty filter shows everything.
	filterProduct := strings.TrimSpace(r.URL.Query().Get("product"))
	if filterProduct != "" {
		want, _ := product.Resolve(filterProduct)
		kept := deployments[:0]
		for _, d := range deployments {
			if got, _ := product.Resolve(d.Product); got.Key == want.Key {
				kept = append(kept, d)
			}
		}
		deployments = kept
	}

	canDeploy := roleCanOTA(role)
	var devices []db.Device
	if canDeploy {
		devices, _ = h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "", "")
	}
	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for cid := range connected {
		online[cid] = true
	}
	h.render(w, r, "updates_rollouts.html", map[string]any{
		"Title":         "Rollouts",
		"Deployments":   deployments,
		"Releases":      deployable,
		"Devices":       devices,
		"Groups":        groups,
		"Online":        online,
		"CanDeploy":     canDeploy,
		"PreRelease":    r.URL.Query().Get("release"),
		"Products":      h.releaseProductFilters(r.Context()),
		"FilterProduct": filterProduct,
	})
}

// NewUpdatePage is the standalone "push an update" screen reached from the Updates
// page's "+ New Update" button. Step 1: pick a publishable release. Step 2 (once
// ?release= is set): pick the specific devices, reboot and delivery mode, then POST
// to /updates (DeployCreate). Device selection is scoped to the release's product.
func (h *Handler) NewUpdatePage(w http.ResponseWriter, r *http.Request) {
	deployable, _ := h.db.ListDeployableReleases(r.Context())
	deployable = visibleReleases(h.role(r), deployable)
	// The product is chosen first: a release only ever targets one product, and the
	// device list is scoped to it, so picking the hardware narrows both lists before
	// a version is even named. ?product= carries the choice; a chosen release always
	// wins, since its own product is the truth.
	selProduct := product.Normalize(r.URL.Query().Get("product"))
	if r.URL.Query().Get("product") == "" {
		selProduct = ""
	}

	// Which products actually have something to push, so the picker never offers a
	// dead end. ListDeployableReleases doesn't carry the product, so resolve each.
	prodOf := map[int]string{}
	if all, err := h.db.ListReleases(r.Context()); err == nil {
		for _, rel := range all {
			prodOf[rel.ID] = product.Normalize(rel.Product)
		}
	}
	var products []product.Product
	seen := map[string]bool{}
	scoped := deployable[:0]
	for _, rel := range deployable {
		pk := prodOf[rel.ID]
		if !seen[pk] {
			seen[pk] = true
			p, _ := product.Resolve(pk)
			products = append(products, p)
		}
		if selProduct == "" || pk == selProduct {
			rel.Product = pk
			scoped = append(scoped, rel)
		}
	}
	sort.SliceStable(products, func(i, j int) bool { return products[i].Label < products[j].Label })

	data := map[string]any{
		"Title":               "New Update",
		"Releases":            scoped,
		"PushProducts":        products,
		"SelectedProduct":     selProduct,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	}
	// Groups and restaurants power the target selector (deploy to a whole group/venue,
	// mirroring the Actions target picker). resolveEligibleDevices resolves them server-side.
	var groupsList []db.Group
	if groups, err := h.db.ListGroups(r.Context()); err == nil {
		data["Groups"] = groups
		groupsList = groups
	}
	if rests, err := h.db.ListRestaurants(r.Context()); err == nil {
		data["Restaurants"] = rests
	}

	// Step 2 only renders once a release is chosen.
	if relRaw := strings.TrimSpace(r.URL.Query().Get("release")); relRaw != "" {
		if relID, err := strconv.Atoi(relRaw); err == nil {
			if rel, err := h.db.GetRelease(r.Context(), relID); err == nil {
				data["Release"] = rel

				// Delivery relevance: forcing full only makes sense when the release has a
				// full image; smart-vs-full only differs when an incremental also exists.
				pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
				hasFull, hasIncremental := false, false
				sourceBuilds := map[string]bool{}
				for _, p := range pkgs {
					if p.Status != "active" {
						continue
					}
					if p.Type == "full" {
						hasFull = true
					} else {
						hasIncremental = true
						if p.SourceBuildID != "" {
							sourceBuilds[p.SourceBuildID] = true
						}
					}
				}
				data["HasFull"] = hasFull
				data["HasIncremental"] = hasIncremental
				data["SourceBuilds"] = sourceBuilds

				// Every device of the release's product; the template marks devices already
				// on this build as up to date and ones mid-update as updating.
				pushDevices, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{Product: rel.Product}, 0, 500, "serial", "asc")
				if acc := h.access(r); !acc.unrestricted() {
					kept := pushDevices[:0:0]
					for _, d := range pushDevices {
						if acc.canDevice("ota", d.ID) {
							kept = append(kept, d)
						}
					}
					pushDevices = kept
				}

				updating, _ := h.db.SerialsUpdating(r.Context())
				data["DevicesUpdating"] = updating
				data["OTAUnsupported"] = h.otaUnsupportedForDevices(r.Context(), pushDevices)
				blockedNewer, _ := h.db.SerialsOnNewerRelease(r.Context(), relID)
				data["DevicesBlocked"] = blockedNewer

				// Eligible-to-push devices first (same "blocked" test the template applies
				// per row: up to date / updating / on a newer release / no artifact for
				// this build), so an operator scanning the list isn't scrolling past a
				// long run of greyed-out rows before reaching anything they can select.
				// Stable sort keeps the existing serial-asc order within each group.
				sort.SliceStable(pushDevices, func(i, j int) bool {
					blockedFor := func(dv db.Device) bool {
						if dv.BuildID == rel.Version {
							return true
						}
						if updating[dv.SerialNumber] != "" || blockedNewer[dv.SerialNumber] != "" {
							return true
						}
						return !hasFull && !sourceBuilds[dv.BuildID]
					}
					return !blockedFor(pushDevices[i]) && blockedFor(pushDevices[j])
				})
				data["PushDevices"] = pushDevices
				data["ScopesJSON"] = h.pickerScopesJSON(r.Context())
				// ?serials= (the fleet selection panel's "Push update") starts the
				// picker with those devices already chosen.
				data["PreSerials"] = parseSerialsField([]string{r.URL.Query().Get("serials")})

				// Group/venue → member-serial maps (restricted to this push list) so the
				// target rail can tick a collection's devices in the list below and float
				// them to the top, client-side. Only serials present here are included, so
				// off-product/ineligible members are naturally excluded.
				idToSerial := make(map[uuid.UUID]string, len(pushDevices))
				for _, dv := range pushDevices {
					idToSerial[dv.ID] = dv.SerialNumber
				}
				groupMembers := map[string][]string{}
				groupIDs := make([]uuid.UUID, len(groupsList))
				for i, g := range groupsList {
					groupIDs[i] = g.ID
				}
				if byGroup, gerr := h.db.GetGroupDeviceIDsBatch(r.Context(), groupIDs); gerr == nil {
					for _, g := range groupsList {
						var serials []string
						for _, id := range byGroup[g.ID] {
							if s, ok := idToSerial[id]; ok {
								serials = append(serials, s)
							}
						}
						if len(serials) > 0 {
							groupMembers[g.ID.String()] = serials
						}
					}
				}
				restMembers := map[string][]string{}
				for _, dv := range pushDevices {
					if dv.RestaurantID != nil {
						rid := dv.RestaurantID.String()
						restMembers[rid] = append(restMembers[rid], dv.SerialNumber)
					}
				}
				gmJSON, _ := json.Marshal(groupMembers)
				rmJSON, _ := json.Marshal(restMembers)
				data["PushGroupMembers"] = template.JS(gmJSON)
				data["PushRestMembers"] = template.JS(rmJSON)
			}
		}
	}

	h.render(w, r, "updates_new.html", data)
}

// ReleaseDeploy deploys a whole release; the per-device artifact (full vs
// incremental) is chosen at resolve time. Only published releases can deploy.
func (h *Handler) ReleaseDeploy(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.deployRelease(w, r, relID)
}

// DeployCreate is the global Updates-hub deploy entry point: identical to
// ReleaseDeploy except the release is chosen in the composer (a form field) rather
// than taken from the URL path.
func (h *Handler) DeployCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	relID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("release_id")))
	if err != nil {
		http.Error(w, "Choose a release to deploy.", http.StatusBadRequest)
		return
	}
	h.deployRelease(w, r, relID)
}

// deployRelease validates the release is publishable (published + an active package)
// and fans the update out to the resolved target devices, then redirects to the new
// deployment's detail page. Shared by ReleaseDeploy (path) and DeployCreate (form).
func (h *Handler) deployRelease(w http.ResponseWriter, r *http.Request, relID int) {
	rel, err := h.db.GetRelease(r.Context(), relID)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// A dev release cannot be pushed by someone who cannot even see it.
	if rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if rel.Status != "published" {
		http.Error(w, "Release must be published before it can be deployed.", http.StatusBadRequest)
		return
	}
	// A push needs at least one active package. A full image covers any device;
	// an incremental-only release is still pushable — the per-device resolver
	// (ResolveUpdateForDevice) hands each incremental only to devices whose
	// current build matches its source_build_id, and skips the rest.
	pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
	hasActive, hasFull := false, false
	for _, p := range pkgs {
		if p.Status == "active" {
			hasActive = true
			if p.Type == "full" {
				hasFull = true
			}
		}
	}
	if !hasActive {
		http.Error(w, "Add an OTA package to this release before pushing.", http.StatusBadRequest)
		return
	}
	r.ParseForm()

	rebootBehavior := r.FormValue("reboot_behavior")
	if rebootBehavior == "" {
		rebootBehavior = "immediate"
	}
	scheduledTime := parseScheduledUTC(r.FormValue("scheduled_time"), rebootBehavior)

	// Delivery: "smart" (default) lets the resolver pick an incremental per device where
	// its current build matches, falling back to full. "full" pins every target to the
	// full image. Only meaningful when the release actually has a full package.
	forceFull := r.FormValue("delivery") == "full"
	if forceFull && !hasFull {
		http.Error(w, "This release has no full image — add one before forcing full delivery.", http.StatusBadRequest)
		return
	}

	// Resolve eligibility BEFORE creating the deployment row: a push that resolves to
	// zero eligible devices (a double-submitted push where a second request finds
	// everything already targeted/updated by the first, or simply selecting devices
	// that turn out ineligible) must fail with a clear reason instead of silently
	// creating and redirecting to an empty deployment record.
	eligible, err := h.resolveEligibleDevices(r, rel.Product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Drop devices already on a same-or-newer release — OTAing an older release onto them
	// is not allowed (they'd sit pending forever; the resolver would refuse to serve it).
	eligible, err = h.db.RemoveDowngradeTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Drop devices with no applicable artifact (incremental-only release + device not on a
	// source build) — the resolver could never serve them, so they'd strand as pending.
	eligible, err = h.db.RemoveInapplicableTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Split by transport rather than dropping: a build that cannot apply an MDM OTA
	// still updates, over the legacy otautil path. Both halves belong to one rollout.
	eligible, legacyIDs := h.splitOTACapable(r.Context(), eligible)
	legacySerials, err := h.db.SerialsForDeviceIDs(r.Context(), legacyIDs)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Serials the picker offered that are not in the fleet at all: otautil devices,
	// which only ever had the legacy path. They come straight from the form.
	legacySerials = append(legacySerials, h.legacyOnlySerials(r, rel)...)
	legacySerials = dedupeStrings(legacySerials)

	if len(eligible) == 0 && len(legacySerials) == 0 {
		http.Error(w, "No eligible devices — everything selected is already on this build or newer, has no applicable package, or is mid-update on another deployment.", http.StatusBadRequest)
		return
	}

	deployment, err := h.db.CreateReleaseUpdate(r.Context(), relID, rebootBehavior, scheduledTime, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(eligible) > 0 {
		if err := h.db.SendUpdateToDevices(r.Context(), deployment.ID, eligible, forceFull); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}
	// The legacy half: same release, same rollout, offered on each device's next
	// otautil poll instead of pushed as a command.
	if len(legacySerials) > 0 {
		legacyID, err := h.db.CreateLegacyDeployment(r.Context(), relID, legacySerials, h.currentUsername(r))
		if err != nil {
			log.Printf("[ota] legacy half of deployment %d: %v", deployment.ID, err)
		} else if err := h.db.LinkLegacyDeployment(r.Context(), legacyID, deployment.ID); err != nil {
			log.Printf("[ota] linking legacy rollout %d to deployment %d: %v", legacyID, deployment.ID, err)
		}
	}
	// Deliver to connected targets right now, the same way every other command
	// type (reboot, screenshot, ...) already does — don't wait for the device's
	// next periodic check-in to notice the pending update_devices row.
	h.pushOTAToConnected(r.Context(), eligible)
	// Belt-and-suspenders fallback: nudge online targets to check in NOW too, in
	// case a device's connection state raced between here and pushOTAToConnected
	// (CreateOTACommandIfNone is idempotent, so this can never double-send).
	// Offline devices pick the update up on their next check-in either way.
	h.nudgeCheckin(eligible)

	h.audit(r, "release.deploy", rel.Version, strconv.Itoa(len(eligible)))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, deployment.ID), http.StatusSeeOther)
}

func (h *Handler) DeploymentDetail(w http.ResponseWriter, r *http.Request) {
	_ = h.db.CompleteSettledDeployments(r.Context())
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	if rel, err := h.db.GetRelease(r.Context(), relID); err == nil && rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	targets, _ := h.db.GetUpdateTargets(r.Context(), did)
	upd.Targets = targets

	otaProgress := make(map[string]any)
	counts := make(map[string]int)
	done := 0
	durSum, durCount := 0, 0
	for i := range targets {
		t := targets[i]
		// Once a target is past the work, never show its live progress bar —
		// shell.Manager's in-memory cache has no TTL and nothing clears it when a
		// status is corrected out-of-band (e.g. a manual DB fix after a lost ack), so
		// a stale in-memory percent could otherwise sit next to "Installed" forever.
		// reboot_sent and canceled count too: a row reading "reboot sent · installing
		// 0%" looks like the reboot was pushed mid-install, when in fact the percent
		// is left over from a later, unrelated attempt.
		switch t.Status {
		case "installed", "failed", "canceled", "reboot_sent", "awaiting_reboot":
		default:
			if p := h.shell.GetOTAProgress(t.DeviceID); p != nil {
				otaProgress[t.DeviceID.String()] = p
				// The row's updated_at only moves at status checkpoints; progress
				// frames land in memory. Let the freshest progress frame count as the
				// row's last update so "Updated" and the 10-minute "stalled" check
				// reflect a download that is actually moving.
				if p.UpdatedAt.After(targets[i].UpdatedAt) {
					targets[i].UpdatedAt = p.UpdatedAt
				}
			}
		}
		counts[t.Status]++
		switch t.Status {
		case "installed", "awaiting_reboot", "reboot_sent":
			done++ // applied to the inactive slot or beyond
		}
		if d := t.DurationSeconds(); d >= 0 {
			durSum += d
			durCount++
		}
	}
	avgDuration := -1
	if durCount > 0 {
		avgDuration = durSum / durCount
	}
	pct := 0
	if len(targets) > 0 {
		pct = done * 100 / len(targets)
	}
	// Score-ring stroke-dashoffset (cc-ring: r=44, circumference 276.5) for the
	// deployment-detail hero — precomputed here since templates have no multiply func.
	ringOffset := 276.5 * float64(100-pct) / 100

	// The legacy half of this rollout, if it has one: devices whose build cannot
	// apply an MDM OTA were served the same release over the otautil path, and this
	// page is where the whole push is accounted for.
	legacyDep, _ := h.db.LegacyDeploymentForUpdate(r.Context(), did)
	legacyRows := []db.LegacyDeploymentDevice{}
	legacyDone := 0
	if legacyDep != nil {
		for i := range legacyDep.Devices {
			dv := &legacyDep.Devices[i]
			if st, ok := ota.Legacy.Get(dv.Serial); ok && (dv.Status == "downloading" || dv.Status == "installing" || dv.Status == "offered") {
				dv.Percent = st.Percent
				if st.Phase != "" {
					dv.Status = st.Phase
				}
			}
			switch {
			case dv.Status == "installed", dv.Status == "awaiting_reboot",
				dv.Status == "installing" && dv.Percent >= 100:
				legacyDone++
			}
		}
		legacyRows = legacyDep.Devices
	}
	totalTargets := len(targets) + len(legacyRows)
	if totalTargets > 0 {
		pct = (done + legacyDone) * 100 / totalTargets
		ringOffset = 276.5 * float64(100-pct) / 100
	}

	// The legacy devices count towards the same status chips. Without this the hero read
	// "18 / 19 installed" next to a lone "7 installed" chip, because the chips only knew
	// about the agent-managed half — and nothing on the page said which device was still
	// going, or why the ring was short of 100%.
	for _, dv := range legacyRows {
		switch dv.Status {
		case "offered":
			counts["pending"]++
		case "downloading", "installing", "verifying", "finalizing":
			counts["installing"]++
		case "installed", "awaiting_reboot", "updated":
			counts["installed"]++
		case "failed":
			counts["failed"]++
		default:
			counts[dv.Status]++
		}
	}
	// Ordered, non-zero status buckets for the rollup line (map iteration order
	// is unstable, so build a fixed-order slice for the template).
	var summary []map[string]any
	for _, s := range []string{"pending", "downloading", "installing", "installed", "awaiting_reboot", "reboot_sent", "failed"} {
		if counts[s] > 0 {
			summary = append(summary, map[string]any{"Status": s, "Count": counts[s]})
		}
	}

	data := map[string]any{
		"Title":        fmt.Sprintf("Deployment #%d", did),
		"Deployment":   upd,
		"Release":      upd.Release,
		"OTAProgress":  otaProgress,
		"Summary":      summary,
		"SummaryDone":  done + legacyDone,
		"SummaryTotal": totalTargets,
		"SummaryPct":   pct,
		"RingOffset":   ringOffset,
		"AvgDuration":  avgDuration, // seconds, or -1 if no device finished yet
		"DoneCount":    durCount,    // devices with a measured duration
		"LegacyDep":    legacyDep,
		"LegacyRows":   legacyRows,
	}

	// HTMX polling target: just the device-status table. Use the ETag/304 helper so an
	// unchanged poll short-circuits and htmx skips the swap — otherwise the every-3s
	// poll re-renders the table on every tick and the section visibly flickers.
	if r.URL.Query().Get("partial") == "targets" {
		h.renderCachedHTML(w, r, "deployment-targets", h.withRole(r, data))
		return
	}

	// The "add targets" picker only renders for operators on a non-canceled
	// deployment (see template) — skip the expensive full-fleet load otherwise so
	// viewers and canceled/finished deployments don't pay for a list they can't use.
	role := h.role(r)
	canOp := roleCanOperate(role)
	if !canOp || upd.Status == "canceled" {
		data["Devices"] = nil
		data["Online"] = map[uuid.UUID]bool{}
		data["Groups"] = nil
		h.render(w, r, "deployment_detail.html", data)
		return
	}

	// Only the full page needs the device/group lists for the "add targets" picker.
	devices, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "", "")
	groups, _ := h.db.ListGroups(r.Context())

	// Mirror the resolver's eligibility (see ReleaseDetail): an incremental-only
	// release only reaches devices whose current build matches an active
	// incremental's source_build_id, so limit the "add targets" device list to
	// those. A full image can flash any device, so leave the list unfiltered.
	packages, _ := h.db.ListPackagesByRelease(r.Context(), relID)
	hasFull := false
	sourceBuilds := map[string]bool{}
	for _, p := range packages {
		if p.Status != "active" {
			continue
		}
		if p.Type == "full" {
			hasFull = true
		} else if p.SourceBuildID != "" {
			sourceBuilds[p.SourceBuildID] = true
		}
	}
	// Devices already in this deployment shouldn't appear in the "add more"
	// picker — they're already receiving (or have received) this update.
	existing := make(map[uuid.UUID]bool, len(targets))
	for _, t := range targets {
		existing[t.DeviceID] = true
	}
	// A deployment is restricted to its release's product — the resolver only ever
	// hands the update to matching devices, so the picker must only offer those.
	wantProduct, _ := product.Resolve(upd.Release.Product)
	eligible := devices[:0]
	for _, d := range devices {
		if existing[d.ID] {
			continue
		}
		if got, _ := product.Resolve(d.Product); got.Key != wantProduct.Key {
			continue
		}
		if !hasFull && !sourceBuilds[d.BuildID] {
			continue
		}
		if !h.otaGate.Device(r.Context(), d).OK { // legacy-OTA-only build
			continue
		}
		eligible = append(eligible, d)
	}
	devices = eligible

	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for cid := range connected {
		online[cid] = true
	}
	data["Devices"] = devices
	data["HasFull"] = hasFull
	data["SourceBuilds"] = sourceBuilds
	data["Online"] = online
	data["Groups"] = groups

	h.render(w, r, "deployment_detail.html", data)
}

func (h *Handler) DeploymentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}

	rebootBehavior := strings.TrimSpace(r.FormValue("reboot_behavior"))
	switch rebootBehavior {
	case "immediate", "scheduled", "manual":
	default:
		rebootBehavior = "immediate"
	}
	scheduledTime := parseScheduledUTC(r.FormValue("scheduled_time"), rebootBehavior)

	if err := h.db.UpdateDeploymentRebootSettings(r.Context(), did, rebootBehavior, scheduledTime); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

func (h *Handler) DeploymentDelete(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteUpdate(r.Context(), did); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d", relID))
}

// DeploymentCancel stops an active deployment from reaching devices that
// haven't started yet (pending → canceled) and flips the update off 'active'.
func (h *Handler) DeploymentCancel(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "deploy") {
		return
	}
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	if err := h.db.CancelDeployment(r.Context(), did); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// One rollout, both halves: cancelling the deployment cancels the legacy targets
	// it was pushed with, or they would keep being offered on every poll.
	if legacy, err := h.db.LegacyDeploymentForUpdate(r.Context(), did); err == nil && legacy != nil {
		_ = h.db.CancelLegacyDeployment(r.Context(), legacy.ID)
	}
	h.audit(r, "deployment.cancel", strconv.Itoa(did), "")
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentCancelDeviceOTA aborts one device's in-flight OTA — only while it's
// still downloading, never mid-install (aborting update_engine partway through
// writing the inactive slot risks a corrupt/unbootable slot; a download can be
// safely thrown away and resumed from scratch). Pushes cancel_command over WS;
// the client checks its current phase and only actually cancels if it's still
// "downloading" (see MdmService's cancel_command handling), then reports a
// terminal "error"/CANCELLED through the same durable ack path every other OTA
// terminal status uses, so the usual afterOtaTerminal bookkeeping applies.
func (h *Handler) DeploymentCancelDeviceOTA(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	cmdID, ok, err := h.db.GetActiveOTACommandID(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ok {
		if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": cmdID.String()}); e == nil {
			h.hub.Push(device.ID, msg)
		}
	}
	// The device may never answer (rebooted mid-download, offline): settle the
	// row ourselves so it reads "failed · cancelled" and offers Retry, and clear
	// the OTA guard so a later retry can re-issue the update.
	_ = h.db.ClearPendingOTACommands(r.Context(), device.ID)
	_ = h.db.SetUpdateDeviceFailed(r.Context(), did, device.ID, "CANCELLED")
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.audit(r, "deployment.cancel_ota", r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRetryDevice re-arms one failed device on a deployment: it clears the
// OTA command guard and resets the device's row to pending, so the next check-in
// re-issues the update (same mechanism as DeviceClearOTA).
func (h *Handler) DeploymentRetryDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	if err := h.db.ClearPendingOTACommands(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Two retry modes: "full" pins the device to the full image (a failed device has
	// almost always tripped on an incremental that can't apply to its source build,
	// and the full package is guaranteed-applicable — also recovers devices that
	// failed before the auto-fallback existed). Plain retry leaves force_full alone
	// and just re-pends, so ResolveUpdateForDevice picks the same delivery it would
	// have chosen originally (e.g. a transient DOWNLOAD_ERROR unrelated to the
	// package itself, where the incremental is still the right, smaller download).
	if r.FormValue("delivery") == "full" {
		_ = h.db.SetUpdateDeviceForceFull(r.Context(), did, device.ID)
	} else {
		_ = h.db.ClearUpdateDeviceForceFull(r.Context(), did, device.ID)
	}
	_ = h.db.SetUpdateDeviceStatus(r.Context(), did, device.ID, "pending")
	// If the deployment was already marked complete, re-pending one device would
	// otherwise strand it (ResolveUpdateForDevice only serves status='active').
	_ = h.db.ReactivateUpdate(r.Context(), did)
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.audit(r, "deployment.retry", r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRebootDevice issues the reboot that applies an OTA which has finished
// installing to the inactive slot and is waiting (status 'awaiting_reboot' — the case
// for a manual, or a not-yet-due scheduled, reboot behavior). It pushes a reboot command
// and flips the row to 'reboot_sent', the same as the automatic/scheduled path — so a
// manual deployment can be applied on demand from the deployment page instead of only
// via a separate device reboot.
func (h *Handler) DeploymentRebootDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "reboot", device.ID) {
		return
	}
	cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.SetUpdateDeviceStatus(r.Context(), did, device.ID, "reboot_sent")
	// Optimistically complete rather than waiting for a confirming check-in, which
	// in a multi-instance deployment may never land on this instance at all — see
	// db.OptimisticallyCompleteReboot.
	if err := h.db.OptimisticallyCompleteReboot(r.Context(), did, device.ID); err != nil {
		log.Printf("[deployment-reboot] OptimisticallyCompleteReboot error: %v", err)
	} else {
		_ = h.db.CheckAndCompleteUpdate(r.Context(), did)
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.audit(r, "deployment.reboot", r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRebootAll reboots every device in a deployment that has installed and is
// waiting (status 'awaiting_reboot') — the bulk form of DeploymentRebootDevice, for
// applying a manual deployment to the whole fleet at once.
func (h *Handler) DeploymentRebootAll(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	ids, err := h.db.ListAwaitingRebootForUpdate(r.Context(), did)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	for _, deviceID := range ids {
		cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{deviceID}, h.currentUsername(r))
		if err != nil {
			log.Printf("[deployment-reboot-all] create reboot error device=%s: %v", deviceID, err)
			continue
		}
		_ = h.db.SetUpdateDeviceStatus(r.Context(), did, deviceID, "reboot_sent")
		if err := h.db.OptimisticallyCompleteReboot(r.Context(), did, deviceID); err != nil {
			log.Printf("[deployment-reboot-all] OptimisticallyCompleteReboot error device=%s: %v", deviceID, err)
		} else {
			_ = h.db.CheckAndCompleteUpdate(r.Context(), did)
		}
		h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{deviceID})
		h.hub.PublishDeviceUpdate(deviceID)
	}
	h.hub.PublishDeploymentUpdate()
	h.audit(r, "deployment.reboot_all", strconv.Itoa(did), strconv.Itoa(len(ids)))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRemoveDevice drops a still-pending device from a deployment so it will
// never receive the update. Only 'pending' rows can be removed (nothing has been
// sent yet); once a device has started downloading the request is a no-op.
func (h *Handler) DeploymentRemoveDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "reboot", device.ID) {
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	removed, err := h.db.RemoveDeviceFromUpdate(r.Context(), did, device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if removed {
		// Clear any guard so a queued OTA can't still reach the device, and refresh
		// the dashboard — mirrors the retry/clear-OTA path.
		_ = h.db.ClearPendingOTACommands(r.Context(), device.ID)
		h.hub.PublishDeviceUpdate(device.ID)
		h.hub.PublishDeploymentUpdate()
		h.audit(r, "deployment.remove_device", r.PathValue("serial"), strconv.Itoa(did))
	}
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentAddTargets widens an existing deployment by adding more devices or
// groups to it, instead of creating a separate deployment. Eligible devices
// (not already on an active update) are appended to the same update.
func (h *Handler) DeploymentAddTargets(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), relID)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	// Delivery choice mirrors the initial push: "full" pins the newly added devices
	// to the full image; default "smart" lets the resolver pick an incremental.
	forceFull := r.FormValue("delivery") == "full"
	if forceFull {
		pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
		hasFull := false
		for _, p := range pkgs {
			if p.Status == "active" && p.Type == "full" {
				hasFull = true
				break
			}
		}
		if !hasFull {
			http.Error(w, "This release has no full image — add one before forcing full delivery.", http.StatusBadRequest)
			return
		}
	}
	eligible, err := h.resolveEligibleDevices(r, rel.Product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Same monotonicity guard as the initial push — never add a device that's already on a
	// same-or-newer release.
	eligible, err = h.db.RemoveDowngradeTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// And drop devices with no applicable artifact for this release.
	eligible, err = h.db.RemoveInapplicableTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if len(eligible) > 0 {
		if err := h.db.SendUpdateToDevices(r.Context(), did, eligible, forceFull); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		// Nudge online targets to check in now so the OTA resolves immediately.
		h.nudgeCheckin(eligible)
	}
	h.audit(r, "deployment.add_targets", strconv.Itoa(did), strconv.Itoa(len(eligible)))
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

func parseScheduledUTC(raw, rebootBehavior string) *time.Time {
	if rebootBehavior != "scheduled" {
		return nil
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.UTC)
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// resolveEligibleDevices builds the target set from the form's serials/group_ids,
// dropping devices that already have an active update. When product != "" it also drops
// devices of a different product, so a release is never deployed cross-product (the
// resolver enforces this too, but filtering here keeps mismatched devices out of the
// deployment entirely).
func (h *Handler) resolveEligibleDevices(r *http.Request, product string) ([]uuid.UUID, error) {
	ids, err := h.resolveEligibleDevicesUnscoped(r, product)
	if err != nil {
		return nil, err
	}
	// A dev's OTA rules decide which of the selected devices they may target.
	kept, dropped := h.access(r).filterDevices("ota", ids)
	if dropped > 0 {
		log.Printf("[access] %s: OTA push narrowed to %d of %d device(s)", h.currentUsername(r), len(kept), len(ids))
	}
	return kept, nil
}

func (h *Handler) resolveEligibleDevicesUnscoped(r *http.Request, product string) ([]uuid.UUID, error) {
	var deviceIDs []uuid.UUID

	// "All devices" scope (Actions-style target rail on the push page): every device of
	// the release's product. The per-product filter below still applies; the union with
	// any explicitly ticked serials is harmless (dedup handles overlap).
	if r.FormValue("scope_all") == "1" {
		devs, err := h.db.ListDevices(r.Context(), db.DeviceFilter{Product: product}, 0, 100000, "serial", "asc")
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			deviceIDs = append(deviceIDs, d.ID)
		}
	}

	// serials come from the per-device checkboxes and/or a pasted bulk list — the
	// same parseSerialsField used by the Actions target picker splits commas/newlines.
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) > 0 {
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			return nil, err
		}
		deviceIDs = append(deviceIDs, ids...)
	}

	for _, s := range r.Form["group_ids"] {
		if gid, err := uuid.Parse(s); err == nil {
			ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), []uuid.UUID{gid})
			if err != nil {
				return nil, err
			}
			deviceIDs = append(deviceIDs, ids...)
		}
	}

	for _, s := range r.Form["restaurant_ids"] {
		if rid, err := uuid.Parse(s); err == nil {
			ids, err := h.db.GetDeviceIDsByRestaurantIDs(r.Context(), []uuid.UUID{rid})
			if err != nil {
				return nil, err
			}
			deviceIDs = append(deviceIDs, ids...)
		}
	}

	seen := make(map[uuid.UUID]bool)
	var unique []uuid.UUID
	for _, did := range deviceIDs {
		if !seen[did] {
			seen[did] = true
			unique = append(unique, did)
		}
	}

	// Keep only devices matching the release's product (wrong-product devices can't be
	// targeted at all). Skipped when no product is supplied.
	if product != "" && len(unique) > 0 {
		var err error
		unique, err = h.db.FilterDeviceIDsByProduct(r.Context(), unique, product)
		if err != nil {
			return nil, err
		}
	}

	var eligible []uuid.UUID
	for _, did := range unique {
		has, err := h.db.DeviceHasActiveUpdate(r.Context(), did)
		if err != nil {
			return nil, err
		}
		if !has {
			eligible = append(eligible, did)
		}
	}

	if limit, err := strconv.Atoi(strings.TrimSpace(r.FormValue("limit"))); err == nil && limit > 0 && len(eligible) > limit {
		eligible = eligible[:limit]
	}

	return eligible, nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

// actionsWindowDays bounds the main Actions page to recent commands: its triage
// buckets only need them (attention = failures < 3 days, in-progress is
// expiry-bounded, completed is a capped preview). Full history lives at
// /commands/history, which is unbounded + paginated.
const actionsWindowDays = 30

func (h *Handler) CommandList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// These 8 reads are all independent, so fire them concurrently instead of one
	// round trip after another — same pattern as Overview/DeviceDetail. The first
	// four were hard 500s on error and stay that way; the rest already tolerated
	// errors silently and keep doing so.
	var (
		cmds          []db.Command
		groups        []db.Group
		apps          []db.App
		fleetPackages []db.FleetPackage
		shellRecent   []string
		shellPopular  []string
		productions   []db.Production
		builds        []string
		summaries     map[uuid.UUID]db.CommandDeliverySummary
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() {
		c, err := h.db.ListCommandsSince(ctx, actionsWindowDays)
		if err != nil {
			fail(err)
			return
		}
		c = filterShellCommands(h.role(r), c) // operators never see shell history
		cmds = h.filterHiddenCommands(r, h.filterAdminCommands(r, c))
	})
	run(func() {
		g, err := h.db.ListGroups(ctx)
		if err != nil {
			fail(err)
			return
		}
		groups = g
	})
	run(func() {
		a, err := h.db.ListApps(ctx)
		if err != nil {
			fail(err)
			return
		}
		apps = a
	})
	run(func() {
		// Distinct packages seen across all devices — populates the Uninstall dropdown.
		fp, err := h.db.SearchFleetPackages(ctx, "")
		if err != nil {
			fail(err)
			return
		}
		fleetPackages = fp
	})
	run(func() { shellRecent, shellPopular, _ = h.db.ShellCommandSuggestions(ctx, 6) })
	run(func() { productions, _ = h.db.ListProductions(ctx, h.connectedSlice()) })
	run(func() { builds, _ = h.db.GetDistinctBuildIDs(ctx) })
	run(func() { summaries, _ = h.db.GetCommandDeliverySummaries(ctx, h.cfg.CommandExpiry(), actionsWindowDays) })
	wg.Wait()
	if firstErr != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.resolveCommandActors(ctx, cmds)

	// Clone: ?clone=<id> prefills the builder from an existing command, so the detail
	// page's "Duplicate & edit" opens the builder ready to tweak and re-send.
	prefill := template.JS("null")
	if cid := r.URL.Query().Get("clone"); cid != "" {
		if id, perr := uuid.Parse(cid); perr == nil {
			if c, gerr := h.db.GetCommand(r.Context(), id); gerr == nil {
				pf := map[string]any{"type": c.Type, "target": c.TargetType}
				if c.ApkURL != "" {
					pf["apk_url"] = c.ApkURL
				}
				if c.Type == "shell" {
					var p struct {
						Cmd string `json:"cmd"`
					}
					_ = json.Unmarshal(c.Payload, &p)
					pf["shell_cmd"] = p.Cmd
				}
				if c.TargetType == "devices" {
					if ser, e := h.db.GetCommandTargetSerials(r.Context(), id); e == nil {
						pf["serials"] = ser
					}
				} else if c.TargetType == "groups" {
					if gids, e := h.db.GetCommandTargetIDs(r.Context(), id); e == nil {
						gs := make([]string, len(gids))
						for i, g := range gids {
							gs[i] = g.String()
						}
						pf["groups"] = gs
					}
				}
				if b, e := json.Marshal(pf); e == nil {
					prefill = template.JS(b)
				}
			}
		}
	}

	// (shellRecent/shellPopular/productions/builds/summaries already fetched concurrently above.)

	// ── Recipes strip: saved presets + a "Re-run last" derived from history ──
	groupNames := make(map[uuid.UUID]string, len(groups))
	for _, g := range groups {
		groupNames[g.ID] = g.Name
	}
	// recipeView carries a recipe plus the JSON the builder's prefill path consumes
	// and a human target summary for the card.
	type recipeView struct {
		ID        uuid.UUID
		Name      string
		Type      string
		Label     string
		Summary   string
		Prefill   template.JS
		Deletable bool
	}
	buildPrefillJSON := func(cmdType, apkURL string, payload json.RawMessage, targetType string, serials []string, groupIDs []uuid.UUID) template.JS {
		pf := map[string]any{"type": cmdType, "target": targetType}
		if apkURL != "" {
			pf["apk_url"] = apkURL
		}
		switch cmdType {
		case "shell":
			var p struct {
				Cmd string `json:"cmd"`
			}
			_ = json.Unmarshal(payload, &p)
			pf["shell_cmd"] = p.Cmd
		case "uninstall":
			var p struct {
				Package string `json:"package"`
			}
			_ = json.Unmarshal(payload, &p)
			pf["package"] = p.Package
		case "query":
			var p struct {
				QueryID int `json:"query_id"`
			}
			_ = json.Unmarshal(payload, &p)
			if p.QueryID > 0 {
				pf["query_id"] = p.QueryID
			}
		}
		if len(serials) > 0 {
			pf["serials"] = serials
		}
		if len(groupIDs) > 0 {
			gs := make([]string, len(groupIDs))
			for i, g := range groupIDs {
				gs[i] = g.String()
			}
			pf["groups"] = gs
		}
		b, _ := json.Marshal(pf)
		return template.JS(b)
	}
	targetSummary := func(targetType string, serials []string, groupIDs []uuid.UUID) string {
		switch targetType {
		case "devices":
			if len(serials) == 1 {
				return serials[0]
			}
			return fmt.Sprintf("%d devices", len(serials))
		case "groups":
			if len(groupIDs) == 1 {
				if n, ok := groupNames[groupIDs[0]]; ok {
					return n
				}
			}
			return fmt.Sprintf("%d groups", len(groupIDs))
		default:
			return "All devices"
		}
	}

	var recipes []recipeView
	// "Re-run last" from the most recent command the builder can actually replay
	// (OTA isn't a builder action, so skip it).
	var last *db.Command
	for i := range cmds {
		if cmds[i].Type != "ota" {
			last = &cmds[i]
			break
		}
	}
	if last != nil {
		var serials []string
		var gids []uuid.UUID
		if last.TargetType == "devices" {
			serials, _ = h.db.GetCommandTargetSerials(r.Context(), last.ID)
		} else if last.TargetType == "groups" {
			gids, _ = h.db.GetCommandTargetIDs(r.Context(), last.ID)
		}
		recipes = append(recipes, recipeView{
			Name:    "Re-run last",
			Type:    last.Type,
			Label:   cmdTypeLabel(last.Type),
			Summary: cmdTypeLabel(last.Type) + " → " + targetSummary(last.TargetType, serials, gids),
			Prefill: buildPrefillJSON(last.Type, last.ApkURL, last.Payload, last.TargetType, serials, gids),
		})
	}
	saved, _ := h.db.ListRecipes(r.Context())
	role := h.role(r)
	canOp := roleCanOperate(role)
	for _, rec := range saved {
		// For roles that can act, hide recipes they aren't allowed to issue (so a
		// click never leads to a rejected send). Viewers see every recipe — the
		// strip is read-only for them, so it's just a catalogue of what's set up.
		if canOp && !h.commandTypeAllowed(h.role(r), rec.Type) {
			continue
		}
		recipes = append(recipes, recipeView{
			ID:        rec.ID,
			Name:      rec.Name,
			Type:      rec.Type,
			Label:     cmdTypeLabel(rec.Type),
			Summary:   cmdTypeLabel(rec.Type) + " → " + targetSummary(rec.TargetType, rec.TargetSerials, rec.TargetGroups),
			Prefill:   buildPrefillJSON(rec.Type, rec.ApkURL, rec.Payload, rec.TargetType, rec.TargetSerials, rec.TargetGroups),
			Deletable: h.role(r) == "admin" || h.role(r) == "dev",
		})
	}

	// ── Triage buckets (Option D): classify every command by its delivery
	// summary. Needs-attention (has failures) and In-progress (still in flight)
	// are always shown in full; Completed is paginated. Expiry is already applied
	// in the summary, so a stuck "pending" becomes failed → surfaces in attention
	// rather than sitting in-progress forever.
	// Failures older than this stop being "needs attention" — they drop into
	// Completed (still rendered as failed, just no longer flagged for triage).
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	var batches map[uuid.UUID][]db.Command
	cmds, summaries, batches = collapseBatches(cmds, summaries)
	var clusters map[uuid.UUID][]db.Command
	cmds, summaries, clusters = clusterSystemReboots(cmds, summaries)
	attn, prog, doneAll := classifyCommands(cmds, summaries, dismissed)

	// The Actions page shows a capped triage preview of each bucket; "Show all →"
	// links jump to the standalone /commands/history page (with pagination).
	const attnLimit, progLimit, doneLimit = 3, 8, 6
	attnShown, progShown, doneShown := capCmds(attn, attnLimit), capCmds(prog, progLimit), capCmds(doneAll, doneLimit)

	// Serial lookups only for the commands actually rendered this request, in one
	// batched query (avoids a per-command N+1).
	var serialIDs []uuid.UUID
	for _, set := range [][]db.Command{attnShown, progShown, doneShown} {
		for _, c := range set {
			if c.TargetType == "devices" {
				serialIDs = append(serialIDs, c.ID)
			}
		}
	}
	targetSerials, _ := h.db.GetCommandTargetSerialsBatch(r.Context(), serialIDs)

	// Collections for the palette's target dropdown (restaurants + releases with counts).
	scopeRestaurants, _ := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), 7)
	scopeReleases, _ := h.db.ListPublishedReleasesForRail(r.Context())
	scopeReleases = visibleRail(h.role(r), scopeReleases)

	// Diagnostics catalog for the palette (admin/dev/operator run them; the action
	// is hidden for viewers and when the catalog is empty).
	var actionQueries []db.DeviceQuery
	if roleCanOperate(role) {
		actionQueries, _ = h.db.ListEnabledDeviceQueries(r.Context())
	}

	// ── Palette datasets + "Recent sends" — full-page renders only (the histlive
	// partial re-runs this handler on every feed tick and doesn't need them). ──
	isHistPartial := r.URL.Query().Get("partial") == "histlive"

	// palAction describes one entry of the ⌘K composer's action dropdown. The list
	// is filtered by the SAME commandRoles gate the send path enforces (plus the
	// set_kiosk special case, which isn't a queued command), so the palette never
	// offers an action the current role can't actually send. Cap tags mark client
	// capability requirements: "system app" needs the privileged AOSP build,
	// "DPC only" needs the Device-Owner agent.
	type palAction struct {
		Type        string `json:"t"`
		Name        string `json:"n"`
		Desc        string `json:"d"`
		Payload     string `json:"p"`             // apps|pkgs|shell|query|kiosk|splash|none
		Cap         string `json:"cap,omitempty"` // "system app" | "DPC only"
		Destructive bool   `json:"destr,omitempty"`
	}
	var palActions []palAction
	palAllowed := map[string]bool{}
	var paletteJSON []byte
	type recentView struct {
		ID      uuid.UUID
		Type    string
		Label   string
		Detail  string
		Target  string
		When    time.Time
		By      string
		Prefill template.JS
		Count   int // "Most frequent" pane: how many times this exact send was made
	}
	var recents, frequent []recentView
	if !isHistPartial {
		// logcat and ota are deliberately absent: neither is a builder action here —
		// logcat fans out via logcat_requests (device pages), OTA via the releases
		// pages — so a POST /commands of either would queue a dead command.
		allActions := []palAction{
			{Type: "install_apk", Name: "Install app", Desc: "push apps from the library, silently", Payload: "apps"},
			{Type: "uninstall", Name: "Uninstall", Desc: "remove packages from the target", Payload: "pkgs"},
			{Type: "screenshot", Name: "Screenshot", Desc: "capture the live screen", Payload: "none", Cap: "system app"},
			{Type: "query", Name: "Device query", Desc: "vetted read-only diagnostic", Payload: "query"},
			{Type: "shell", Name: "Shell", Desc: "raw shell command", Payload: "shell", Cap: "system app"},
			{Type: "reboot", Name: "Reboot", Desc: "restart devices — confirm to send", Payload: "none", Destructive: true},
			{Type: "set_kiosk", Name: "Kiosk mode", Desc: "lock to one app, or unlock", Payload: "kiosk"},
			{Type: "update_splash", Name: "Boot splash", Desc: "replace the boot logo from an image URL", Payload: "splash", Cap: "system app", Destructive: true},
			{Type: "wipe", Name: "Factory wipe", Desc: "erase completely — typed confirm", Payload: "none", Cap: "DPC only", Destructive: true},
		}
		for _, a := range allActions {
			switch a.Type {
			case "set_kiosk":
				if !roleCanOperate(role) {
					continue
				}
			case "query":
				if !h.commandTypeAllowed(role, a.Type) || len(actionQueries) == 0 {
					continue
				}
			case "shell":
				if !h.commandTypeAllowed(role, a.Type) || !h.cfg.ShellEnabled() {
					continue
				}
			default:
				if !h.commandTypeAllowed(role, a.Type) {
					continue
				}
			}
			palActions = append(palActions, a)
			palAllowed[a.Type] = true
		}

		type palApp struct {
			URL     string `json:"url"`
			Name    string `json:"name"`
			Ver     string `json:"ver,omitempty"`
			Pkg     string `json:"pkg,omitempty"`
			Icon    string `json:"icon,omitempty"`
			Family  string `json:"family,omitempty"`
			Variant string `json:"variant,omitempty"`
			Latest  bool   `json:"latest,omitempty"`
		}
		// Ordered by family, prod first, newest first, so the picker reads as a catalogue.
		palFams, _, _ := h.libraryData(r.Context())
		palApps := make([]palApp, 0, len(apps))
		for _, f := range palFams {
			for _, v := range f.Variants {
				for _, ver := range v.Versions {
					a := ver.App
					palApps = append(palApps, palApp{URL: a.ApkURL, Name: a.Name, Ver: a.VersionName, Pkg: a.PackageName, Icon: a.Icon, Family: f.Name, Variant: v.Label, Latest: ver.Latest})
				}
			}
		}
		type palCollection struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		palGroups := make([]palCollection, 0, len(groups))
		for _, g := range groups {
			palGroups = append(palGroups, palCollection{ID: g.ID.String(), Name: g.Name, Count: g.DeviceCount})
		}
		palRests := make([]palCollection, 0, len(scopeRestaurants))
		for _, g := range scopeRestaurants {
			palRests = append(palRests, palCollection{ID: g.GroupID.String(), Name: g.Name, Count: g.DeviceCount})
		}
		type palQuery struct {
			ID    int    `json:"id"`
			Label string `json:"label"`
			Cat   string `json:"cat,omitempty"`
		}
		palQueries := make([]palQuery, 0, len(actionQueries))
		for _, q := range actionQueries {
			palQueries = append(palQueries, palQuery{ID: q.ID, Label: q.Label, Cat: q.Category})
		}
		type palPkg struct {
			Pkg   string `json:"pkg"`
			Name  string `json:"name"`
			Count int    `json:"count"`
			Icon  string `json:"icon,omitempty"`
		}
		palPkgs := make([]palPkg, 0, len(fleetPackages))
		for _, p := range fleetPackages {
			nm := p.AppName
			if nm == "" {
				nm = p.PackageName
			}
			palPkgs = append(palPkgs, palPkg{Pkg: p.PackageName, Name: nm, Count: p.DeviceCount, Icon: p.Icon})
		}
		paletteJSON, _ = json.Marshal(map[string]any{
			"actions":       palActions,
			"apps":          palApps,
			"groups":        palGroups,
			"restaurants":   palRests,
			"queries":       palQueries,
			"packages":      palPkgs,
			"shellRecent":   shellRecent,
			"shellPopular":  shellPopular,
			"requireReason": h.cfg.RequireReason(),
		})

		// Recent sends: the last few DISTINCT sends (this user's own preferred),
		// each carrying a prefill object the palette reloads in one click. Batch
		// installs/uninstalls collapse to one entry carrying every URL/package.
		appNamesByURL := apkURLToName(apps)
		mkRecentPrefill := func(c db.Command, serials []string) template.JS {
			pf := map[string]any{"type": c.Type, "target": c.TargetType}
			if members, ok := batches[c.ID]; ok && len(members) > 1 {
				var urls, pkgs []string
				for _, m := range members {
					if m.ApkURL != "" {
						urls = append(urls, m.ApkURL)
					}
					if c.Type == "uninstall" {
						var p struct {
							Package string `json:"package"`
						}
						if json.Unmarshal(m.Payload, &p) == nil && p.Package != "" {
							pkgs = append(pkgs, p.Package)
						}
					}
				}
				if len(urls) > 0 {
					pf["apk_urls"] = urls
				}
				if len(pkgs) > 0 {
					pf["packages"] = pkgs
				}
			} else if c.ApkURL != "" {
				pf["apk_url"] = c.ApkURL
			}
			switch c.Type {
			case "shell":
				var p struct {
					Cmd string `json:"cmd"`
				}
				_ = json.Unmarshal(c.Payload, &p)
				pf["shell_cmd"] = p.Cmd
			case "uninstall":
				if _, ok := pf["packages"]; !ok {
					var p struct {
						Package string `json:"package"`
					}
					_ = json.Unmarshal(c.Payload, &p)
					pf["package"] = p.Package
				}
			case "query":
				var p struct {
					QueryID int `json:"query_id"`
				}
				_ = json.Unmarshal(c.Payload, &p)
				if p.QueryID > 0 {
					pf["query_id"] = p.QueryID
				}
			}
			if len(serials) > 0 {
				pf["serials"] = serials
			}
			b, _ := json.Marshal(pf)
			return template.JS(b)
		}
		recentDetail := func(c db.Command) string {
			if members, ok := batches[c.ID]; ok && len(members) > 1 {
				return fmt.Sprintf("%d apps", len(members))
			}
			switch c.Type {
			case "install_apk":
				if n := appNamesByURL[c.ApkURL]; n != "" {
					return n
				}
				if i := strings.LastIndex(c.ApkURL, "/"); i >= 0 && i < len(c.ApkURL)-1 {
					return c.ApkURL[i+1:]
				}
			case "uninstall":
				var p struct {
					Package string `json:"package"`
				}
				if json.Unmarshal(c.Payload, &p) == nil {
					return p.Package
				}
			case "shell":
				var p struct {
					Cmd string `json:"cmd"`
				}
				if json.Unmarshal(c.Payload, &p) == nil {
					return p.Cmd
				}
			case "query":
				var p struct {
					Query string `json:"query"`
				}
				if json.Unmarshal(c.Payload, &p) == nil {
					return p.Query
				}
			}
			return ""
		}
		meNames := map[string]bool{}
		if u := h.currentUsername(r); u != "" {
			meNames[u] = true
		}
		if dn := h.currentDisplayName(r); dn != "" {
			meNames[dn] = true
		}
		const recentMax = 6
		// Only operator sends count as presets: admin accounts drive maintenance and
		// tests, and those must not shape what the console suggests to operators.
		resendable := func(c db.Command) bool {
			if !palAllowed[c.Type] || c.CreatedBy == "" || userIsAdminFn(c.CreatedBy) {
				return false
			}
			_, isCluster := clusters[c.ID]
			return !isCluster // system/OTA reboots aren't resendable presets
		}
		// viewOf builds the row once per command; the target lookups are the cost, so
		// the signature (what makes two sends "the same") comes with it.
		viewOf := func(c db.Command) (recentView, string) {
			serials, _ := h.db.GetCommandTargetSerials(r.Context(), c.ID)
			var gids []uuid.UUID
			if c.TargetType == "groups" {
				gids, _ = h.db.GetCommandTargetIDs(r.Context(), c.ID)
			}
			detail := recentDetail(c)
			target := targetSummary(c.TargetType, serials, gids)
			return recentView{
				ID:      c.ID,
				Type:    c.Type,
				Label:   cmdTypeLabel(c.Type),
				Detail:  detail,
				Target:  target,
				When:    c.CreatedAt,
				By:      c.CreatedBy,
				Prefill: mkRecentPrefill(c, serials),
			}, c.Type + "|" + c.ApkURL + "|" + detail + "|" + target
		}
		seenSig := map[string]bool{}
		addRecent := func(c db.Command) {
			if len(recents) >= recentMax || !resendable(c) {
				return
			}
			v, sig := viewOf(c)
			if seenSig[sig] {
				return
			}
			seenSig[sig] = true
			recents = append(recents, v)
		}
		for pass := 0; pass < 2 && len(recents) < recentMax; pass++ {
			scanned := 0
			for i := range cmds {
				if len(recents) >= recentMax || scanned > 60 {
					break
				}
				own := cmds[i].CreatedBy != "" && meNames[cmds[i].CreatedBy]
				if (pass == 0) != own {
					continue
				}
				scanned++
				addRecent(cmds[i])
			}
		}
		// Most frequent: the same send (type + payload + target) made repeatedly across
		// the loaded history, newest occurrence shown, ranked by count. Only sends made
		// more than once qualify — a one-off is already covered by "Recent".
		freqIdx := map[string]int{}
		scanned := 0
		for i := range cmds {
			if scanned >= 200 {
				break
			}
			if !resendable(cmds[i]) {
				continue
			}
			scanned++
			v, sig := viewOf(cmds[i])
			if j, ok := freqIdx[sig]; ok {
				frequent[j].Count++
				continue
			}
			v.Count = 1
			freqIdx[sig] = len(frequent)
			frequent = append(frequent, v)
		}
		sort.SliceStable(frequent, func(a, b int) bool { return frequent[a].Count > frequent[b].Count })
		n := 0
		for _, v := range frequent {
			if v.Count < 2 {
				break
			}
			n++
		}
		if n > recentMax {
			n = recentMax
		}
		frequent = frequent[:n]
	}

	data := map[string]any{
		"Title":            "Actions",
		"DeviceQueries":    actionQueries,
		"Commands":         cmds,
		"Attention":        attnShown,
		"InProgress":       progShown,
		"Completed":        doneShown,
		"AttnCount":        len(attn),
		"ProgCount":        len(prog),
		"DoneCount":        len(doneAll),
		"Groups":           groups,
		"ScopeRestaurants": scopeRestaurants,
		"ScopeReleases":    scopeReleases,
		"Productions":      productions,
		"Builds":           builds,
		"Apps":             apps,
		"AppNames":         apkURLToName(apps),
		"FleetPackages":    fleetPackages,
		"Recipes":          recipes,
		"Summaries":        summaries,
		"Batches":  batches,
		"Clusters": clusters,
		"AppIcons": apkURLToIcon(apps),
		"PkgIcons": pkgToIcon(apps),
		"TargetSerials":    targetSerials,
		"ShellRecent":      shellRecent,
		"ShellPopular":     shellPopular,
		"AIEnabled":        h.cfg.AIEnabled(),
		"Prefill":          prefill,
		"PaletteJSON":      template.JS(paletteJSON),
		"RecentSends":      recents,
		"FrequentSends":    frequent,
		"RequireReason":    h.cfg.RequireReason(),
	}
	// Live status: the Actions page's history table re-fetches just this fragment on a
	// command-update SSE event (and a slow poll), morphing it in place so statuses move
	// buckets without a reload or a flash.
	if r.URL.Query().Get("partial") == "histlive" {
		h.renderCachedHTML(w, r, "cmd-timeline-inner", h.withRole(r, data))
		return
	}
	h.render(w, r, "commands.html", data)
}

// capCmds returns the first n commands (or all if fewer).
func capCmds(cmds []db.Command, n int) []db.Command {
	if len(cmds) > n {
		return cmds[:n]
	}
	return cmds
}

// CommandHistory is the standalone, paginated history view reached from the
// Actions page's "Show all →" links. It shows one status bucket (or all) as a
// dense table with numbered pagination — the deep-browse counterpart to the
// capped triage preview on /commands.
func (h *Handler) CommandHistory(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommands(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	cmds = filterShellCommands(h.role(r), cmds) // operators never see shell history
	cmds = h.filterHiddenCommands(r, h.filterAdminCommands(r, cmds))
	h.resolveCommandActors(r.Context(), cmds)
	summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), 0) // full history
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	apps, _ := h.db.ListApps(r.Context())
	var batches map[uuid.UUID][]db.Command
	cmds, summaries, batches = collapseBatches(cmds, summaries)
	var clusters map[uuid.UUID][]db.Command
	cmds, summaries, clusters = clusterSystemReboots(cmds, summaries)
	attn, prog, doneAll := classifyCommands(cmds, summaries, dismissed)

	status := r.URL.Query().Get("status")
	var rows []db.Command
	var title string
	switch status {
	case "attention":
		rows, title = attn, "Needs attention"
	case "inprogress":
		rows, title = prog, "In progress"
	case "completed":
		rows, title = doneAll, "Completed"
	default:
		status = "all"
		title = "All actions"
		rows = make([]db.Command, 0, len(cmds))
		for _, c := range cmds { // everything except OTA, newest first
			if c.Type != "ota" {
				rows = append(rows, c)
			}
		}
	}

	// Optional server-side type filter — applied before pagination so it composes
	// with paging (selecting a type then paging keeps the type).
	cmdType := r.URL.Query().Get("type")
	if cmdType != "" {
		filtered := make([]db.Command, 0, len(rows))
		for _, c := range rows {
			if c.Type == cmdType {
				filtered = append(filtered, c)
			}
		}
		rows = filtered
	}

	// Pagination.
	const pageSize = 40
	total := len(rows)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	var pageRows []db.Command
	if total > 0 {
		pageRows = rows[start:end]
	}

	// Per-row bucket (for state/retry) + serials (one batched query), for the page.
	bucketByID := make(map[uuid.UUID]string, len(pageRows))
	var serialIDs []uuid.UUID
	for _, c := range pageRows {
		bucketByID[c.ID] = commandBucket(c, summaries[c.ID], dismissed[c.ID])
		if c.TargetType == "devices" {
			serialIDs = append(serialIDs, c.ID)
		}
	}
	targetSerials, _ := h.db.GetCommandTargetSerialsBatch(r.Context(), serialIDs)

	data := map[string]any{
		"Title":         "History — " + title,
		"Heading":       title,
		"Status":        status,
		"FilterType":    cmdType,
		"Rows":          pageRows,
		"BucketByID":    bucketByID,
		"Summaries":     summaries,
		"Batches":  batches,
		"Clusters": clusters,
		"AppIcons": apkURLToIcon(apps),
		"PkgIcons": pkgToIcon(apps),
		"AppNames": apkURLToName(apps),
		"TargetSerials": targetSerials,
		"AttnCount":     len(attn),
		"ProgCount":     len(prog),
		"DoneCount":     len(doneAll),
		"AllCount":      len(rows),
		"Page":          page,
		"Total":         total,
		"TotalPages":    totalPages,
	}
	// The #hist-live self-refresh requests partial=histlive and swaps the returned
	// fragment directly (no hx-select on a full document, which could parse to
	// nothing and blank the region). Serve just that block for the refresh.
	if r.URL.Query().Get("partial") == "histlive" {
		h.renderCachedHTML(w, r, "action-history-live", h.withRole(r, data))
		return
	}
	h.render(w, r, "command_history.html", data)
}

// CommandBrowseDevices renders the filtered device picker for the command builder's
// CommandResolveSerials backs the Actions console's paste-a-list flow: given the
// serials the operator pasted (comma/space/newline separated), it answers which ones
// name a real active device and which do not, so the console adds only the former
// and shows the rest in a popup instead of silently dropping them at send.
func (h *Handler) CommandResolveSerials(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("serials")
	var serials []string
	for _, t := range strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t' }) {
		if t = cleanSerialToken(t); t != "" {
			serials = append(serials, t)
		}
		if len(serials) >= 500 {
			break
		}
	}
	found, missing, err := h.db.ResolveSerials(r.Context(), serials)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Scope to what this user may target: devices outside their access policy count
	// as unknown, matching how the send path drops them.
	if ids := h.access(r).visibleIDs(); ids != nil {
		devices, _ := h.db.GetDevicesByIDs(r.Context(), ids)
		allowed := map[string]bool{}
		for _, d := range devices {
			allowed[d.SerialNumber] = true
		}
		kept := found[:0]
		for _, sn := range found {
			if allowed[sn] {
				kept = append(kept, sn)
			} else {
				missing = append(missing, sn)
			}
		}
		found = kept
	}
	if found == nil {
		found = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"found": found, "missing": missing})
}

// "Specific" target — the same filters as the Devices list and the new-group browser
// (search, status, group, production, build, battery). Checking rows feeds the target
// serial chips; "Select all matching" turns the current filter into the target set.
func (h *Handler) CommandBrowseDevices(w http.ResponseWriter, r *http.Request) {
	filter := db.DeviceFilter{
		Search:              r.URL.Query().Get("q"),
		Online:              r.URL.Query().Get("status"),
		BuildID:             r.URL.Query().Get("build"),
		Battery:             r.URL.Query().Get("battery"),
		Kiosk:               r.URL.Query().Get("kiosk"),
		ActiveThresholdSecs: h.cfg.CheckinInterval() * 3,
	}
	if gid := r.URL.Query().Get("group"); gid != "" {
		if id, err := uuid.Parse(gid); err == nil {
			filter.GroupID = id
		}
	}
	if pid := r.URL.Query().Get("production"); pid != "" {
		if id, err := uuid.Parse(pid); err == nil {
			filter.ProductionID = id
		}
	}
	// ?source=legacy: rows come from the otautil devices instead of the fleet.
	// They are not in the devices table by design, so they get their own path here
	// and the same markup, which is what lets one picker serve both.
	if r.URL.Query().Get("source") == "legacy" {
		h.browseLegacyDevices(w, r)
		return
	}
	// ?product=<key>: the hardware chosen on the push screen, before a release is
	// named. A ?release= below overrides it — the release's own product is the truth.
	if pk := strings.TrimSpace(r.URL.Query().Get("product")); pk != "" {
		filter.Product = product.Normalize(pk)
	}
	// ?release=<id>: a release only ever targets its own product, so scope the list
	// the way the push path does instead of listing devices it could never reach.
	var pushRel *db.Release
	if relRaw := strings.TrimSpace(r.URL.Query().Get("release")); relRaw != "" {
		if relID, err := strconv.Atoi(relRaw); err == nil {
			if rel, err := h.db.GetRelease(r.Context(), relID); err == nil && rel != nil {
				pushRel = rel
				filter.Product = rel.Product
			}
		}
	}
	if gid := r.URL.Query().Get("exclude_group"); gid != "" {
		if id, err := uuid.Parse(gid); err == nil {
			filter.ExcludeGroupID = id
		}
	}
	// The picker sends this when it is assigning devices to a venue; without it the
	// venue's own devices were offered back to it.
	if rid := r.URL.Query().Get("exclude_restaurant"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.ExcludeRestaurantID = id
		}
	}
	if rid := r.URL.Query().Get("restaurant"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.RestaurantID = id
		}
	}
	h.access(r).applyFilter(&filter)
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 500, r.URL.Query().Get("sort"), r.URL.Query().Get("dir"))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}
	// Per-device agent type so the Actions console rail can tag DPC devices and
	// filter/subset by capability. ListDevices already carries latest_extra.
	dpc := make(map[uuid.UUID]bool, len(devices))
	for _, d := range devices {
		if d.IsDPC() {
			dpc[d.ID] = true
		}
	}
	// ?release=<id>: annotate each row with whether this release can be pushed to
	// that device and, if it can, which artifact it would get. The Deploy page's
	// picker needs it; every other caller leaves the maps empty and the rows render
	// exactly as before.
	blocked, artifact := map[string]string{}, map[string]string{}
	var legacyRows []legacyPickerRow
	// A scoped request ("every device in this restaurant/group/production") is
	// answered by the picker with "select all of these", so it must contain only
	// devices that really are in that scope. Legacy-only devices are not in the
	// fleet at all and so belong to no restaurant or group — including them here
	// made a restaurant of 8 devices select 10.
	scoped := filter.RestaurantID != uuid.Nil || filter.GroupID != uuid.Nil || filter.ProductionID != uuid.Nil
	if pushRel != nil {
		blocked, artifact = h.releaseEligibility(r.Context(), pushRel, devices)
		// Devices that are not in the fleet at all but do poll the legacy listener:
		// one push covers both kinds now, so they belong in the same list.
		if !scoped {
			legacyRows = h.legacyPickerRows(r, pushRel, blocked, artifact, r.URL.Query().Get("q"), r.URL.Query().Get("status"))
		}
		// What can be pushed comes first. A blocked row still belongs in the list —
		// "why can't I pick this one" is the next question — but scrolling past a
		// screenful of up-to-date devices to reach the three that need the build is
		// the picker failing at its one job.
		sort.SliceStable(devices, func(i, j int) bool {
			return blocked[devices[i].SerialNumber] == "" && blocked[devices[j].SerialNumber] != ""
		})
		sort.SliceStable(legacyRows, func(i, j int) bool {
			return blocked[legacyRows[i].Device.Serial] == "" && blocked[legacyRows[j].Device.Serial] != ""
		})
	}
	h.tmpl.ExecuteTemplate(w, "cmd-device-browser", map[string]any{
		"Devices":     devices,
		"Online":      online,
		"DPC":         dpc,
		"Blocked":     blocked,
		"Artifact":    artifact,
		"LegacyRows":  legacyRows,
	})
}

// releaseEligibility answers, per serial, why a release cannot be pushed to that
// device ("" when it can) and which artifact it would receive. Same rules the
// push path enforces, so the picker can't offer a device the deploy would drop:
// already on the build, mid-update, on a newer release, no applicable package,
// or firmware older than the product's MDM OTA cutoff.
func (h *Handler) releaseEligibility(ctx context.Context, rel *db.Release, devices []db.Device) (map[string]string, map[string]string) {
	blocked, artifact := map[string]string{}, map[string]string{}
	relID := rel.ID
	hasFull := false
	sourceBuilds := map[string]bool{}
	pkgs, _ := h.db.ListPackagesByRelease(ctx, relID)
	for _, p := range pkgs {
		if p.Status != "active" {
			continue
		}
		if p.Type == "full" {
			hasFull = true
		} else if p.SourceBuildID != "" {
			sourceBuilds[p.SourceBuildID] = true
		}
	}
	updating, _ := h.db.SerialsUpdating(ctx)
	newer, _ := h.db.SerialsOnNewerRelease(ctx, relID)
	for _, d := range devices {
		s := d.SerialNumber
		// A build with no MDM OTA is not blocked any more — it is served the same
		// release over the legacy path, in the same rollout. Only a full image can
		// reach one, since the legacy client has no incremental story.
		legacy := !h.otaGate.Device(ctx, d).OK
		switch {
		case d.BuildID == rel.Version:
			blocked[s] = "up to date"
		case newer[s] != "":
			blocked[s] = "newer installed (" + newer[s] + ")"
		case updating[s] != "":
			blocked[s] = "already updating"
		case legacy && !hasFull:
			blocked[s] = "legacy OTA needs a full image"
		case legacy:
			artifact[s] = "legacy"
		case !hasFull && !sourceBuilds[d.BuildID]:
			blocked[s] = "no update for this build"
		case sourceBuilds[d.BuildID]:
			artifact[s] = "incremental"
		default:
			artifact[s] = "full"
		}
	}
	return blocked, artifact
}

// CommandImpact renders the Actions builder's live "blast radius" panel: given
// the current builder state (action type + target selection), it resolves the
// concrete device set and summarises online/offline split and action-specific
// risks (low battery for reboot, missing-package skips for uninstall). It reuses
// the same resolution the send path uses, so the preview matches what will fire.
func (h *Handler) CommandImpact(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}
	targetType := r.FormValue("target_type")

	// Resolve the target device set the same way CommandCreate does (all/devices/
	// groups/scope) so the impact count is exactly what a send would hit.
	ids, _ := h.resolveTargetDeviceIDs(r, targetType)
	devices, _ := h.db.GetDevicesByIDs(r.Context(), ids)
	connected := h.hub.ConnectedIDs()

	// DPC split: how many of the resolved targets run the Device-Owner agent.
	// The Actions console uses this (via data-dpc on the fragment root) to dim
	// system-app-only actions and to scope the DPC-only wipe. Additive — older
	// consumers of this fragment just ignore the extra attribute.
	dpcCount, _ := h.db.CountDPCDevices(r.Context(), ids)

	// Uninstall is a no-op on devices without the package — count and subtract.
	skipped := 0
	if cmdType == "uninstall" {
		if pkg := strings.TrimSpace(r.FormValue("package")); pkg != "" {
			have, _ := h.db.CountDevicesWithPackage(r.Context(), ids, pkg)
			skipped = len(devices) - have
			if skipped < 0 {
				skipped = 0
			}
		}
	}
	// Capability gating: devices whose agent cannot run the action are skipped at
	// send (see CommandCreate), so the preview subtracts them here and names them.
	// unsupportedBy carries the same count for every console action so the grid
	// can caveat each card without another round-trip.
	unsupported := 0
	var unsupSerials []string
	unsupportedBy := map[string]int{}
	for _, d := range devices {
		if !d.Supports(cmdType) {
			unsupported++
			if len(unsupSerials) < 500 {
				unsupSerials = append(unsupSerials, d.SerialNumber)
			}
		}
		for _, t := range []string{"install_apk", "uninstall", "reboot", "screenshot", "query", "shell", "set_kiosk", "update_splash", "wipe"} {
			need := t
			if t == "set_kiosk" {
				need = "kiosk_set"
			}
			if !d.Supports(need) {
				unsupportedBy[t]++
			}
		}
	}
	skipped += unsupported
	unsupJSON, _ := json.Marshal(unsupportedBy)

	type rollRow struct {
		Serial  string
		Online  bool
		Battery int
	}
	online, lowBatOnline := 0, 0
	roll := make([]rollRow, 0, len(devices))
	for _, d := range devices {
		_, up := connected[d.ID]
		if up {
			online++
			if d.BatteryPct > 0 && d.BatteryPct < 20 {
				lowBatOnline++
			}
		}
		if len(roll) < 8 {
			roll = append(roll, rollRow{Serial: d.SerialNumber, Online: up, Battery: d.BatteryPct})
		}
	}
	total := len(devices)
	effective := total - skipped // devices that will actually receive the action
	if effective < 0 {
		effective = 0
	}
	onlineEff := online
	if onlineEff > effective {
		onlineEff = effective
	}
	offlineEff := effective - onlineEff

	var warn string
	switch cmdType {
	case "reboot":
		if lowBatOnline > 0 {
			s := ""
			if lowBatOnline != 1 {
				s = "s"
			}
			verb := "is"
			if lowBatOnline != 1 {
				verb = "are"
			}
			warn = fmt.Sprintf("%d online device%s %s under 20%% battery — rebooting risks a unit that can't power back up.", lowBatOnline, s, verb)
		}
	case "uninstall":
		if skipped-unsupported > 0 {
			n := skipped - unsupported
			s, verb := "", "doesn't"
			if n != 1 {
				s, verb = "s", "don't"
			}
			warn = fmt.Sprintf("%d targeted device%s %s report this package — they'll be skipped automatically.", n, s, verb)
		}
	}
	if unsupported > 0 {
		s := ""
		if unsupported != 1 {
			s = "s"
		}
		msg := fmt.Sprintf("%d device%s can't run %s on their agent — skipped automatically.", unsupported, s, cmdTypeLabel(cmdType))
		if warn != "" {
			warn = msg + " " + warn
		} else {
			warn = msg
		}
	}

	h.tmpl.ExecuteTemplate(w, "cmd-impact", map[string]any{
		"Type":       cmdType,
		"Total":      total,
		"Effective":  effective,
		"Online":     onlineEff,
		"Offline":    offlineEff,
		"DPC":        dpcCount,
		"Skipped":    skipped,
		"Unsupported": unsupported,
		"UnsupJSON":   string(unsupJSON),
		"UnsupSerials": strings.Join(unsupSerials, ","),
		"LowBattery": lowBatOnline,
		"Screenshot": cmdType == "screenshot",
		"Warn":       warn,
		"Roll":       roll,
		"More":       total - len(roll),
	})
}

// CommandTargetPackages renders the uninstall package picker scoped to the current
// target selection, so the Actions uninstall list shows only apps actually installed
// on the devices that would receive the command. htmx-refreshed when the target
// changes (see the #uninstall-list container in commands.html).
func (h *Handler) CommandTargetPackages(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		targetType = "all"
	}
	ids, _ := h.resolveTargetDeviceIDs(r, targetType)
	pkgs, _ := h.db.PackagesForDevices(r.Context(), ids)
	h.tmpl.ExecuteTemplate(w, "uninstall-pkg-list", map[string]any{
		"FleetPackages": pkgs,
		"TargetCount":   len(ids),
	})
}

// TargetLivePage renders the working target-picker prototype (/demo/target-live) backed
// by the real fleet: restaurants, groups and releases with live counts. All three
// picker concepts share the JSON count endpoint below.
func (h *Handler) TargetLivePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	at := h.cfg.CheckinInterval() * 3
	rests, _ := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7)
	groups, _ := h.db.GetGroupHealth(ctx, h.connectedSlice())
	rels, _ := h.db.ListPublishedReleasesForRail(ctx)
	rels = visibleRail(h.role(r), rels)
	all, _ := h.db.ListDevices(ctx, db.DeviceFilter{ActiveThresholdSecs: at}, 0, 20000, "serial", "asc")
	connected := h.hub.ConnectedIDs()
	online := 0
	for _, d := range all {
		if _, up := connected[d.ID]; up {
			online++
		}
	}
	h.render(w, r, "target-live.html", map[string]any{
		"Title":       "Target playground",
		"Restaurants": rests,
		"Groups":      groups,
		"Releases":    rels,
		"FleetTotal":  len(all),
		"FleetOnline": online,
	})
}

// TargetCountJSON resolves a target spec to a live device count for the target-picker
// prototypes. mode = all | group | restaurant | build | audience | serials — every
// non-serial mode is just a DeviceFilter, so audience rules compose into one query.
func (h *Handler) TargetCountJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var devices []db.Device
	if q.Get("mode") == "serials" {
		ids, _ := h.db.GetDeviceIDsBySerials(ctx, db.ParseSerials(q.Get("serials")))
		devices, _ = h.db.GetDevicesByIDs(ctx, ids)
	} else {
		f := db.DeviceFilter{ActiveThresholdSecs: h.cfg.CheckinInterval() * 3}
		if v := q.Get("group"); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				f.GroupID = id
			}
		}
		if v := q.Get("restaurant"); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				f.RestaurantID = id
			}
		}
		f.BuildID = q.Get("build")
		f.Battery = q.Get("battery")
		f.Online = q.Get("status")
		f.Kiosk = q.Get("kiosk")
		f.Charging = q.Get("charging")
		devices, _ = h.db.ListDevices(ctx, f, 0, 20000, "serial", "asc")
	}
	devices = h.access(r).keepVisible(devices)
	connected := h.hub.ConnectedIDs()
	online := 0
	type samp struct {
		Serial  string `json:"serial"`
		Online  bool   `json:"online"`
		Battery int    `json:"battery"`
	}
	sample := make([]samp, 0, 60)
	for _, d := range devices {
		_, up := connected[d.ID]
		if up {
			online++
		}
		if len(sample) < 60 {
			sample = append(sample, samp{d.SerialNumber, up, d.BatteryPct})
		}
	}
	eff := len(devices)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"effective": eff, "online": online, "offline": eff - online, "sample": sample,
	})
}

// markDeliveryPresence flags each delivery online when the device currently
// holds a live WebSocket connection in the hub — so the UI can distinguish a
// device that's about to ack from one that's offline and won't receive the
// command until it reconnects.
func (h *Handler) markDeliveryPresence(deliveries []db.CommandDelivery) {
	connected := h.hub.ConnectedIDs()
	for i := range deliveries {
		_, deliveries[i].Online = connected[deliveries[i].DeviceID]
	}
}

// deliveryStats is the Mission-Control rollup for a command detail page: the
// progress-ring counts derived from the per-device delivery statuses.
type deliveryStats struct {
	Total   int
	Done    int // installed / completed
	Failed  int // failed / expired (terminal, needs attention)
	Pending int // pending / delivered (in flight)
	Offline int // in-flight and currently offline (will receive on reconnect)
	Pct     int // Done / Total, 0-100
}

func computeDeliveryStats(deliveries []db.CommandDelivery) deliveryStats {
	var s deliveryStats
	s.Total = len(deliveries)
	for _, d := range deliveries {
		switch d.Status {
		case "installed", "completed":
			s.Done++
		case "failed", "expired":
			s.Failed++
		default: // pending, delivered
			s.Pending++
			if !d.Online {
				s.Offline++
			}
		}
	}
	if s.Total > 0 {
		s.Pct = s.Done * 100 / s.Total
	}
	return s
}

func (h *Handler) CommandStatusPartial(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.markDeliveryPresence(deliveries)
	h.renderCachedHTML(w, r, "command-deliveries", map[string]any{
		"Command":    cmd,
		"Deliveries": deliveries,
		"Nicknames":  h.deliveryNicknames(r.Context(), deliveries),
		"Stats":      computeDeliveryStats(deliveries),
		"CanResend":  h.commandTypeAllowed(h.role(r), cmd.Type),
	})
}

// DeviceInstallProgress renders one combined progress page for a batch of app installs
// queued from the device page (ids = comma-separated command IDs), so the operator
// watches every app on a single page instead of a separate history page per app. Serves
// the full page on a normal request and just the rows on an htmx poll (HX-Request).
func (h *Handler) DeviceInstallProgress(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	idsParam := r.URL.Query().Get("ids")
	var order []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, s := range strings.Split(idsParam, ",") {
		if id, e := uuid.Parse(strings.TrimSpace(s)); e == nil && !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}
	cmds, _ := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	byID := make(map[uuid.UUID]db.DeviceCommand, len(cmds))
	for _, c := range cmds {
		byID[c.ID] = c
	}
	apps, _ := h.db.ListApps(r.Context())
	appByURL := make(map[string]db.App, len(apps))
	for _, a := range apps {
		appByURL[a.ApkURL] = a
	}
	type ipRow struct {
		ID       uuid.UUID
		Name     string
		Icon     string
		Status   string
		Progress *int
	}
	var rows []ipRow
	allDone := true
	for _, id := range order {
		c, ok := byID[id]
		if !ok {
			continue
		}
		a := appByURL[c.ApkURL]
		name := a.Name
		if name == "" {
			name = "App"
		}
		rows = append(rows, ipRow{ID: id, Name: name, Icon: a.Icon, Status: c.Status, Progress: c.Progress})
		switch c.Status {
		case "installed", "failed", "completed", "cancelled", "expired":
		default:
			allDone = false
		}
	}
	if len(rows) == 0 {
		allDone = true
	}
	data := map[string]any{
		"Title":   "Installing apps",
		"Device":  device,
		"Rows":    rows,
		"IDs":     idsParam,
		"AllDone": allDone,
		"Skipped": strings.TrimSpace(r.URL.Query().Get("skipped")),
	}
	if r.Header.Get("HX-Request") == "true" {
		if err := h.tmpl.ExecuteTemplate(w, "install-progress-rows", h.withRole(r, data)); err != nil {
			log.Printf("template render install-progress-rows: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
		return
	}
	h.render(w, r, "install_progress.html", data)
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

func (h *Handler) CommandDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	// Admin/dev can delete anything. Operator can only delete a command still In progress
	// or in Needs attention — the same two buckets they can already act on elsewhere
	// (Resend, Dismiss) — not settled history under Completed. Reuses the exact
	// classification the page itself displays, so "can I delete this row" always
	// matches what bucket it's actually shown in.
	if role := h.role(r); role != "admin" && role != "dev" {
		cmd, err := h.db.GetCommand(r.Context(), id)
		if err != nil {
			http.Error(w, "Command not found", http.StatusNotFound)
			return
		}
		summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), actionsWindowDays)
		dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
		if commandBucket(*cmd, summaries[id], dismissed[id]) == "done" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	// Grab the targeted devices BEFORE the delete cascades command_targets/status
	// away, so we can tell any device mid-download to stop.
	deviceIDs, _ := h.db.GetCommandDeviceIDs(r.Context(), id)
	if err := h.db.DeleteCommand(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Push a cancel frame to each reached device. Harmless to a device that isn't
	// running this command — the client only acts on a cmd id it's actively
	// downloading. Aborts the in-flight APK download so a removed action doesn't
	// keep pulling bytes (and installing) after the operator deleted it.
	if len(deviceIDs) > 0 {
		if msg, mErr := json.Marshal(map[string]any{"type": "cancel_command", "id": id.String()}); mErr == nil {
			for _, did := range deviceIDs {
				h.hub.Push(did, msg)
			}
		}
	}
	// Fires the same command-update SSE event a status change would, so every open
	// Actions/history view (including this tab) re-fetches its capped bucket and
	// morphs in place — the next hidden row slides into view instead of the bucket
	// just shrinking by one, and other tabs/users stay in sync.
	h.hub.PublishCommandUpdate(id)
	if hxReq(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

func (h *Handler) CommandResendDevice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if serial == "" {
		http.Error(w, "Serial required", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !h.commandTypeAllowed(h.role(r), cmd.Type) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), []string{serial})
	if err != nil || len(deviceIDs) == 0 {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, policyActionForCommand(cmd.Type), deviceIDs[0]) {
		return
	}
	newCmd, err := h.db.CreateCommandBy(r.Context(), cmd.Type, cmd.ApkURL, cmd.Payload, "devices", deviceIDs, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), newCmd, "devices", deviceIDs)
	http.Redirect(w, r, "/commands/"+newCmd.ID.String(), http.StatusFound)
}

func (h *Handler) CommandResendAll(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !h.commandTypeAllowed(h.role(r), cmd.Type) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	targetIDs, err := h.db.GetCommandTargetIDs(r.Context(), id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	targetIDs, targetType, ok := h.enforceCommandTargets(w, r, policyActionForCommand(cmd.Type), cmd.TargetType, targetIDs)
	if !ok {
		return
	}
	newCmd, err := h.db.CreateCommandBy(r.Context(), cmd.Type, cmd.ApkURL, cmd.Payload, targetType, targetIDs, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), newCmd, cmd.TargetType, targetIDs)
	http.Redirect(w, r, "/commands/"+newCmd.ID.String(), http.StatusFound)
}

func (h *Handler) CommandDetail(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.markDeliveryPresence(deliveries)
	// Operators must not see raw shell command detail (text/output) — hide its
	// existence, mirroring the shell filtering applied to every history listing.
	if cmd.Type == "shell" && hideShellForRole(h.role(r)) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	// Mirrors filterAdminCommands: operators must not reach an admin-created
	// command's detail page directly either.
	if len(h.filterHiddenCommands(r, []db.Command{*cmd})) == 0 {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	// The "show admin actions" toggle only filters lists: an admin or dev may
	// always open a command by URL (they land here right after sending one).
	if hideAdminCommandsForRole(h.role(r)) && h.isAdminAction(*cmd) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !canSeeCommandURLs(h.role(r)) {
		cmd.ApkURL = ""
	}
	single := []db.Command{*cmd}
	h.resolveCommandActors(r.Context(), single)
	*cmd = single[0]
	// Resolve the app behind an install/uninstall so the page can show its icon
	// and name instead of a download URL or package id.
	var app *db.App
	if cmd.Type == "install_apk" || cmd.Type == "uninstall" {
		var pkg string
		if cmd.Type == "uninstall" {
			var p struct {
				Package string `json:"package"`
			}
			if json.Unmarshal(cmd.Payload, &p) == nil {
				pkg = p.Package
			}
		}
		if apps, err := h.db.ListApps(r.Context()); err == nil {
			for i := range apps {
				if (cmd.ApkURL != "" && apps[i].ApkURL == cmd.ApkURL) || (pkg != "" && apps[i].PackageName == pkg) {
					app = &apps[i]
					break
				}
			}
		}
	}
	// Sibling commands from the same multi-app send, with their apps resolved.
	type sib struct {
		ID   uuid.UUID
		Name string
		Icon string
		Self bool
	}
	var siblings []sib
	if b := batchOf(*cmd); b != "" {
		if members, err := h.db.ListCommandsByBatch(r.Context(), b); err == nil && len(members) > 1 {
			apps, _ := h.db.ListApps(r.Context())
			byURL := map[string]db.App{}
			for _, a := range apps {
				byURL[a.ApkURL] = a
			}
			for _, m := range members {
				a := byURL[m.ApkURL]
				name := a.Name
				if name == "" {
					name = cmdTypeLabel(m.Type)
				}
				siblings = append(siblings, sib{ID: m.ID, Name: name, Icon: a.Icon, Self: m.ID == cmd.ID})
			}
		}
	}
	nick := h.deliveryNicknames(r.Context(), deliveries)
	h.render(w, r, "command_detail.html", map[string]any{
		"Title":      "Action " + id.String()[:8],
		"Command":    cmd,
		"App":        app,
		"Siblings":   siblings,
		"Nicknames":  nick,
		"Deliveries": deliveries,
		"Stats":      computeDeliveryStats(deliveries),
		"From":       from,
		"CanResend":  h.commandTypeAllowed(h.role(r), cmd.Type),
	})
}

// CommandScreenshot serves a screenshot delivery's PNG from a normal same-origin
// URL. The gallery stores each shot as a base64 data: URI, which renders fine as an
// inline <img> but cannot be opened or downloaded through a link: browsers block
// top-level navigation to data: URIs, so the "open" and "download" links landed on a
// blank tab until a manual refresh (FW-2026-000018). Decoding the stored base64 here
// gives those links a real URL that loads on first paint.
func (h *Handler) CommandScreenshot(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if cmd.Type != "screenshot" {
		http.Error(w, "Not a screenshot command", http.StatusBadRequest)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var b64 string
	for _, d := range deliveries {
		if d.SerialNumber == serial {
			b64 = d.Output
			break
		}
	}
	if b64 == "" {
		http.Error(w, "Screenshot not available", http.StatusNotFound)
		return
	}
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		http.Error(w, "Corrupt screenshot", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "screenshot-"+serial+".png"))
	}
	w.Write(png)
}

// cmdAuthz is the outcome of a command-authorization check. It distinguishes an
// unknown command type (a client error -> 400) from a known type the caller's
// role may not issue (an authorization error -> 403).
type cmdAuthz int

const (
	cmdAuthzOK cmdAuthz = iota
	cmdAuthzUnknownType
	cmdAuthzForbidden
)

// commandRoles is the single source of truth for which dashboard roles may
// issue (or resend) each command type. A type absent from this map is unknown
// and is rejected outright — this is what closes the previous default-allow gap
// where any arbitrary string (including destructive verbs) was accepted,
// persisted, and dispatched from the lowest-privilege role (GB-01/GB-02). The
// admin JSON API (internal/api) independently gates the same set behind the
// admin key. Keep the two lists in sync when adding a command type.
// Operators carry the everyday action set (screenshot/install/uninstall/reboot) plus
// their exclusive QA recording elsewhere, so they're listed alongside admin/dev on
// those types — but never on the admin/dev-only types (shell, ota, update_splash).
// Raw shell is limited to admin/dev; operators reach vetted commands via the device
// queries catalog instead (the "diagnostic" action on the Actions page).
var commandRoles = map[string][]string{
	"screenshot":    {"admin", "dev", "operator", "user_manager", "super_op", "viewer"},
	"install_apk":   {"admin", "dev", "operator", "user_manager", "super_op"},
	"uninstall":     {"admin", "dev", "operator", "user_manager", "super_op"},
	"reboot":        {"admin", "dev", "operator", "user_manager", "super_op"},
	"shell":         {"admin", "dev"},
	// "query" is a read-only diagnostic; its command text is admin-vetted (chosen by
	// query_id from the catalog, never user-supplied), so operators may issue it.
	"query":         {"admin", "dev", "operator", "user_manager", "super_op"},
	"ota":           {"admin", "dev", "super_op"},
	"update_splash": {"admin", "dev"},
	"logcat":        {"admin", "dev"},
	// Full-device factory reset — DPC-agent devices only, admin-only (most destructive action).
	"wipe":          {"admin"},
	// Read-only mic capture gain (TX_DEC0..7 Volume) probe. Admin-only for now: the
	// field it refreshes is only rendered for admins (see device.html Hardware card).
	"mic_gain_read": {"admin"},
	// mic_gain_set (the vendor daemon's TX_DEC enforcement target) is deliberately
	// absent: mic gain is read-only from the MDM, so the type is refused as unknown.
}

// ── Roles ────────────────────────────────────────────────────────────────────
//
// Levels, highest first: admin ("Super admin", 4) → dev (3) → user_manager (2)
// → operator (1) → viewer (0). A user may only create, edit or delete accounts
// strictly below their own level, and may only assign roles below their own.
// The env dashboard login is always an admin.
//   admin         everything
//   dev           releases / OTA / deployments / productions + every operator action
//   user_manager  users, access policies, activity + every operator action
//   operator      device actions, per access policy
//   viewer        read-only, per visibility policy

var roleLevels = map[string]int{"owner": 0, "viewer": 0, "operator": 1, "user_manager": 2, "super_op": 2, "dev": 3, "admin": 4}

var roleLabels = map[string]string{"admin": "Super Admin", "dev": "Dev", "user_manager": "Access admin", "super_op": "Super op", "operator": "Operator", "viewer": "Viewer", "owner": "Restaurant owner"}

// roleOrder is every assignable role, highest first.
var roleOrder = []string{"admin", "dev", "user_manager", "super_op", "operator", "viewer", "owner"}

func roleLevel(role string) int { return roleLevels[role] }

func roleLabel(role string) string {
	if l, ok := roleLabels[role]; ok {
		return l
	}
	return role
}

// roleCanOperate: roles with operator powers (device actions, QA, groups…).
func roleCanOperate(role string) bool {
	return role == "admin" || role == "dev" || role == "user_manager" || role == "super_op" || role == "operator"
}

// roleCanOTA: may push firmware updates (create deployments, add targets, retry,
// cancel). Release packages themselves (create / upload / publish / delete) stay
// with canRelease (admin + dev).
func roleCanOTA(role string) bool { return role == "admin" || role == "dev" || role == "super_op" }

// roleIsOperatorLike: the "test team" roles — operator, or user manager acting
// as one. Used where operators specifically (not admins) get a behaviour.
func roleIsOperatorLike(role string) bool { return role == "operator" || role == "user_manager" || role == "super_op" }

// roleManagesUsers: may open the Users pages and edit accounts below their level.
func roleManagesUsers(role string) bool { return role == "admin" || role == "user_manager" || role == "super_op" }

// roleCreatesUsers: may create, delete or merge accounts. The super op manages
// access rules, roles and passwords of existing accounts but does not add or
// remove them.
func roleCreatesUsers(role string) bool { return role == "admin" || role == "user_manager" }

// assignableRoles lists the roles an actor may grant: strictly below their level,
// except an admin who may also make admins.
func assignableRoles(actorRole string) []string {
	lvl := roleLevel(actorRole)
	var out []string
	for _, r := range roleOrder {
		if roleLevel(r) < lvl || (actorRole == "admin" && r == "admin") {
			out = append(out, r)
		}
	}
	return out
}

// mayManageUser: actor may act on a target of role targetRole (and, for role
// changes, grant newRole). Admins may manage anyone, including other admins.
func mayManageUser(actorRole, targetRole, newRole string) bool {
	if actorRole == "admin" {
		return true
	}
	if roleLevel(targetRole) >= roleLevel(actorRole) {
		return false
	}
	return newRole == "" || roleLevel(newRole) < roleLevel(actorRole)
}

// withBatch adds "batch": id to a command payload (JSON object; empty → {}).
func withBatch(payload json.RawMessage, id string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &m)
	}
	b, _ := json.Marshal(id)
	m["batch"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// batchOf returns the payload.batch id of a command, or "".
func batchOf(c db.Command) string {
	if len(c.Payload) == 0 {
		return ""
	}
	var p struct {
		Batch string `json:"batch"`
	}
	if json.Unmarshal(c.Payload, &p) != nil {
		return ""
	}
	return p.Batch
}

// collapseBatches folds commands that share a payload.batch id into their first
// member: the returned list has one representative per batch (others removed),
// the representative's summary is the sum of the members', and batches maps the
// representative's id to every member (itself first) so the history row can show
// "Install 3 apps" with an icon cluster. Commands without a batch pass through.
func collapseBatches(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary) ([]db.Command, map[uuid.UUID]db.CommandDeliverySummary, map[uuid.UUID][]db.Command) {
	batches := map[uuid.UUID][]db.Command{}
	rep := map[string]uuid.UUID{}
	var out []db.Command
	for _, c := range cmds {
		b := batchOf(c)
		if b == "" {
			out = append(out, c)
			continue
		}
		if r, ok := rep[b]; ok {
			batches[r] = append(batches[r], c)
			continue
		}
		rep[b] = c.ID
		batches[c.ID] = []db.Command{c}
		out = append(out, c)
	}
	if len(batches) == 0 {
		return cmds, summaries, map[uuid.UUID][]db.Command{}
	}
	merged := make(map[uuid.UUID]db.CommandDeliverySummary, len(summaries))
	for k, v := range summaries {
		merged[k] = v
	}
	for r, members := range batches {
		if len(members) < 2 {
			delete(batches, r)
			continue
		}
		var sum db.CommandDeliverySummary
		sum.CommandID = r
		for _, m := range members {
			sm := summaries[m.ID]
			sum.Pending += sm.Pending
			sum.Delivered += sm.Delivered
			sum.Completed += sm.Completed
			sum.Failed += sm.Failed
			sum.Downloading += sm.Downloading
			sum.Installing += sm.Installing
		}
		merged[r] = sum
	}
	return out, merged, batches
}

// pkgToIcon maps package name → base64 icon for the app library (uninstall rows
// carry a package, not an APK URL).
func pkgToIcon(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.PackageName != "" && a.Icon != "" {
			m[a.PackageName] = a.Icon
		}
	}
	return m
}

// apkURLToIcon maps APK URL → base64 icon for the app library.
func apkURLToIcon(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.ApkURL != "" && a.Icon != "" {
			m[a.ApkURL] = a.Icon
		}
	}
	return m
}

// policyActionForCommand maps a command type to the access-policy action key.
// Types outside the operator set (shell, ota, …) map to themselves and are never
// granted to operators by the role allowlist anyway.
func policyActionForCommand(cmdType string) string {
	if cmdType == "set_kiosk" {
		return "kiosk"
	}
	return cmdType
}

// authorizeCommand reports whether role may issue a command of cmdType, per the
// role allowlist in commandRoles.
func (h *Handler) authorizeCommand(role, cmdType string) cmdAuthz {
	roles, known := commandRoles[cmdType]
	if !known {
		return cmdAuthzUnknownType
	}
	allowed := false
	for _, r := range roles {
		if r == role {
			allowed = true
			break
		}
	}
	if !allowed {
		return cmdAuthzForbidden
	}
	return cmdAuthzOK
}

// writeCommandAuthzError writes the HTTP response for a non-OK command
// authorization result and reports whether it wrote anything. Callers return
// early when it returns true.
func writeCommandAuthzError(w http.ResponseWriter, authz cmdAuthz) bool {
	switch authz {
	case cmdAuthzUnknownType:
		http.Error(w, "Unknown command type", http.StatusBadRequest)
		return true
	case cmdAuthzForbidden:
		http.Error(w, "Forbidden", http.StatusForbidden)
		return true
	}
	return false
}

// commandTypeAllowed is a boolean convenience for read/template paths (e.g.
// whether to offer a Resend button). Handlers that create or resend commands
// use authorizeCommand directly so they can distinguish 400 from 403.
func (h *Handler) commandTypeAllowed(role, cmdType string) bool {
	return h.authorizeCommand(role, cmdType) == cmdAuthzOK
}

// attnMaxAge is how recent a failure must be to still count as "needs attention"
// — older failures drop into Completed so the triage list stays actionable.
const attnMaxAge = 72 * time.Hour

// commandBucket classifies one command from its delivery summary into
// "attn" (recent, undismissed failure), "prog" (in flight) or "done".
func commandBucket(c db.Command, s db.CommandDeliverySummary, dismissed bool) string {
	switch {
	case s.Failed > 0 && !dismissed && c.CreatedAt.After(time.Now().Add(-attnMaxAge)):
		return "attn"
	case s.Failed == 0 && (s.Pending > 0 || s.Delivered > 0):
		return "prog"
	default:
		return "done"
	}
}

// classifyCommands buckets non-OTA commands (newest first) into the three triage
// lists. Commands in the dismissed set are kept out of Needs-attention. Shared by
// the Actions page and the standalone history page.
func classifyCommands(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary, dismissed map[uuid.UUID]bool) (attn, prog, done []db.Command) {
	for _, c := range cmds {
		if c.Type == "ota" {
			continue
		}
		switch commandBucket(c, summaries[c.ID], dismissed[c.ID]) {
		case "attn":
			attn = append(attn, c)
		case "prog":
			prog = append(prog, c)
		default:
			done = append(done, c)
		}
	}
	return
}

// AttentionClear dismisses every command currently in Needs-attention from that
// list. It records dismissals only — the commands and their delivery history are
// left intact (they remain visible under Completed / history).
func (h *Handler) AttentionClear(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommandsSince(r.Context(), actionsWindowDays)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), actionsWindowDays)
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	attn, _, _ := classifyCommands(cmds, summaries, dismissed)
	ids := make([]uuid.UUID, 0, len(attn))
	for _, c := range attn {
		ids = append(ids, c.ID)
	}
	if err := h.db.DismissCommands(r.Context(), ids, h.role(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "command.attention_clear", "", fmt.Sprintf("cleared=%d", len(ids)))
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// cmdTypeLabel maps a command type to its friendly label for the dashboard.
// Package-level so both the template funcmap and handlers (recipes, timeline)
// share one source of truth.
func cmdTypeLabel(cmdType string) string {
	switch cmdType {
	case "install_apk":
		return "Install app"
	case "uninstall":
		return "Uninstall"
	case "shell":
		return "Shell"
	case "query":
		return "Query"
	case "screenshot":
		return "Screenshot"
	case "reboot":
		return "Reboot"
	case "wipe":
		return "Wipe"
	case "update_splash":
		return "Boot logo"
	case "logcat":
		return "Log capture"
	case "ota":
		return "OTA Update"
	case "mic_gain_read":
		return "Mic gain read"
	case "mic_gain_set":
		return "Mic gain set"
	default:
		return cmdType
	}
}

// canSeeCommandURLs reports whether a role may be shown internal APK/OTA URLs,
// which embed the object-store bucket name. Only operational roles need them;
// viewers must not see them, so the bucket name is not disclosed via command
// reads (GB-05).
func canSeeCommandURLs(role string) bool {
	return roleCanOperate(role)
}

// redactDeviceCommandURLs blanks the APK/OTA URL on a command-history slice for
// roles that may not see it. The template falls back to the command label.
func redactDeviceCommandURLs(role string, cmds []db.DeviceCommand) {
	if canSeeCommandURLs(role) {
		return
	}
	for i := range cmds {
		cmds[i].ApkURL = ""
	}
}

// hideShellForRole reports whether a role must not see raw shell commands in the
// history. Operators cannot run shell (commandRoles limits it to admin/dev) and must
// not browse the shell history either — its command text and captured output can
// expose sensitive operational detail. admin/dev/viewer see history unchanged.
func hideShellForRole(role string) bool { return role != "admin" && role != "dev" }

// filterShellDeviceCommands drops raw shell entries from a per-device command
// history when the viewer must not see them (hideShellForRole).
func filterShellDeviceCommands(role string, cmds []db.DeviceCommand) []db.DeviceCommand {
	if !hideShellForRole(role) {
		return cmds
	}
	out := make([]db.DeviceCommand, 0, len(cmds))
	for _, c := range cmds {
		if c.Type == "shell" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// filterShellCommands drops raw shell entries from an Actions/history command
// slice when the viewer must not see them (hideShellForRole).
func filterShellCommands(role string, cmds []db.Command) []db.Command {
	if !hideShellForRole(role) {
		return cmds
	}
	out := make([]db.Command, 0, len(cmds))
	for _, c := range cmds {
		if c.Type == "shell" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// hideAdminCommandsForRole reports whether a role must not see commands the
// (env-configured, single) admin account created — operators shouldn't see
// admin-issued actions (e.g. an internal test install) in their Actions view.
func hideAdminCommandsForRole(role string) bool { return roleIsOperatorLike(role) }

// filterAdminCommands drops commands created by the admin account from an
// Actions/history slice when the viewer must not see them
// (hideAdminCommandsForRole). "admin" is env-configured only (never a DB
// user, see validUserRole), so its username is always h.user.
func (h *Handler) filterAdminCommands(r *http.Request, cmds []db.Command) []db.Command {
	if !h.hideAdminActions(r) {
		return cmds
	}
	out := make([]db.Command, 0, len(cmds))
	for _, c := range cmds {
		if h.isAdminAction(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isAdminAction: sent by an admin account, of a type only admins can send (shell,
// boot logo), sent with the admin API key, or still unattributed after the
// startup backfill (BackfillCommandAuthors) — nothing in the audit log names a
// person for those, so they are treated as admin history. The OTA flow's own
// update reboots are the exception and stay visible, labelled automatic.
func (h *Handler) isAdminAction(c db.Command) bool {
	if isSystemReboot(c) {
		return false
	}
	// Unattributed or API-key commands count as admin actions; a shell or boot
	// logo command sent by a named dev is that person's own action.
	if c.CreatedBy == "" || c.CreatedBy == "API key" {
		return true
	}
	return h.isAdminAuthor(c.CreatedBy)
}

// showAdminActionsCookie is set by the "Show admin actions" toggle (admins only).
const showAdminActionsCookie = "mdm_show_admin_actions"

// hideAdminActions: operators/viewers never see actions sent by admin accounts;
// admins don't either unless they switched the toggle on. Server-side so the
// counts and pagination on the Actions/History pages stay right.
func (h *Handler) hideAdminActions(r *http.Request) bool {
	if hideAdminCommandsForRole(h.role(r)) {
		return true
	}
	c, err := r.Cookie(showAdminActionsCookie)
	return err != nil || c.Value != "1"
}

// isAdminAuthor: only the built-in (env-configured) super admin login. Team
// accounts, whatever their role, are people whose actions always show.
func (h *Handler) isAdminAuthor(name string) bool {
	if name == "" {
		return false // system-sent (e.g. OTA reboots) — shown, labelled automatic
	}
	return name == h.user || strings.EqualFold(name, "admin")
}

// isSystemReboot: a reboot nobody typed — the OTA flow's post-install reboot.
func isSystemReboot(c db.Command) bool { return c.Type == "reboot" && c.CreatedBy == "" }

// clusterSystemReboots folds runs of OTA-sent reboots (each targets one device,
// created within minutes of each other) into one representative row per run,
// mirroring collapseBatches. Returns the reduced list, merged summaries and the
// cluster members keyed by representative id.
func clusterSystemReboots(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary) ([]db.Command, map[uuid.UUID]db.CommandDeliverySummary, map[uuid.UUID][]db.Command) {
	const gap = 20 * time.Minute
	clusters := map[uuid.UUID][]db.Command{}
	var out []db.Command
	var rep uuid.UUID
	var last time.Time
	for _, c := range cmds {
		if !isSystemReboot(c) {
			out = append(out, c)
			continue
		}
		d := last.Sub(c.CreatedAt)
		if d < 0 {
			d = -d
		}
		if !last.IsZero() && d <= gap {
			clusters[rep] = append(clusters[rep], c)
		} else {
			rep = c.ID
			clusters[rep] = []db.Command{c}
			out = append(out, c)
		}
		last = c.CreatedAt
	}
	merged := make(map[uuid.UUID]db.CommandDeliverySummary, len(summaries))
	for k, v := range summaries {
		merged[k] = v
	}
	for r, members := range clusters {
		if len(members) < 2 {
			delete(clusters, r)
			continue
		}
		var sum db.CommandDeliverySummary
		sum.CommandID = r
		for _, m := range members {
			sm := summaries[m.ID]
			sum.Pending += sm.Pending
			sum.Delivered += sm.Delivered
			sum.Completed += sm.Completed
			sum.Failed += sm.Failed
		}
		merged[r] = sum
	}
	return out, merged, clusters
}

// resolveTargetDeviceIDs turns a command target spec (all/devices/groups/scope) into
// the concrete device-ID set — expanding groups/scope to their devices — for actions
// like kiosk/logcat that operate on device IDs directly rather than via CreateCommand.
func (h *Handler) resolveTargetDeviceIDs(r *http.Request, targetType string) ([]uuid.UUID, error) {
	switch targetType {
	case "groups":
		var gids []uuid.UUID
		for _, g := range r.Form["target_groups"] {
			if id, err := uuid.Parse(g); err == nil {
				gids = append(gids, id)
			}
		}
		return h.db.GetDeviceIDsInGroups(r.Context(), gids)
	case "devices":
		return h.db.GetDeviceIDsBySerials(r.Context(), db.ParseSerials(r.FormValue("target_serials")))
	case "scope":
		return h.resolveScopeDeviceIDs(r)
	case "mixed":
		return h.resolveMixedDeviceIDs(r)
	case "all":
		return h.db.GetAllDeviceIDs(r.Context())
	default:
		// No target chosen yet (the builder's default, empty state) — the impact
		// preview should show 0, not silently preview the whole fleet.
		return nil, nil
	}
}

// resolveMixedDeviceIDs is the console's "pick anything" target: any number of
// restaurants (target_restaurants), groups (target_groups) and serials
// (target_serials), unioned and de-duplicated by device, so a device that sits in
// two chosen scopes counts once. target_all=1 short-circuits to the whole fleet.
func (h *Handler) resolveMixedDeviceIDs(r *http.Request) ([]uuid.UUID, error) {
	if r.FormValue("target_all") == "1" {
		return h.db.GetAllDeviceIDs(r.Context())
	}
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	add := func(ids []uuid.UUID) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	parse := func(vals []string) []uuid.UUID {
		var ids []uuid.UUID
		for _, v := range vals {
			if id, err := uuid.Parse(v); err == nil {
				ids = append(ids, id)
			}
		}
		return ids
	}
	if ids, err := h.db.GetDeviceIDsInRestaurants(r.Context(), parse(r.Form["target_restaurants"])); err != nil {
		return nil, err
	} else {
		add(ids)
	}
	if ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), parse(r.Form["target_groups"])); err != nil {
		return nil, err
	} else {
		add(ids)
	}
	if serials := db.ParseSerials(r.FormValue("target_serials")); len(serials) > 0 {
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			return nil, err
		}
		add(ids)
	}
	return out, nil
}

// buildScopeFilter reads the scope-rail form fields (a primary collection —
// all/restaurant/group/build — plus optional refine filters) into a DeviceFilter.
func (h *Handler) buildScopeFilter(r *http.Request) db.DeviceFilter {
	f := db.DeviceFilter{ActiveThresholdSecs: h.cfg.CheckinInterval() * 3}
	switch r.FormValue("scope_mode") {
	case "restaurant":
		if id, err := uuid.Parse(r.FormValue("scope_id")); err == nil {
			f.RestaurantID = id
		}
	case "group":
		if id, err := uuid.Parse(r.FormValue("scope_id")); err == nil {
			f.GroupID = id
		}
	case "build":
		f.BuildID = r.FormValue("scope_id")
	}
	// Refine pills compose on top of the collection.
	f.Online = r.FormValue("scope_status")
	f.Battery = r.FormValue("scope_battery")
	f.Kiosk = r.FormValue("scope_kiosk")
	return f
}

// resolveScopeDeviceIDs snapshots the scope filter to the matching device IDs (like the
// "all" path — future devices that later match are unaffected). scope_mode is empty
// until the user actually picks something in the Target rail (the builder's default
// targets no devices, not the whole fleet) — buildScopeFilter's switch has no case for
// that, so an empty/unrecognized scope_mode would otherwise fall through to an
// unfiltered DeviceFilter{} and silently match every device. Refuse to resolve until
// scope_mode is a real, explicit choice.
func (h *Handler) resolveScopeDeviceIDs(r *http.Request) ([]uuid.UUID, error) {
	switch r.FormValue("scope_mode") {
	case "restaurant", "group", "build", "all":
	default:
		return nil, nil
	}
	devs, err := h.db.ListDevices(r.Context(), h.buildScopeFilter(r), 0, 20000, "serial", "asc")
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(devs))
	for i := range devs {
		ids[i] = devs[i].ID
	}
	return ids, nil
}

// applyKioskForTargets sets (or clears) kiosk mode on the resolved target devices and
// pushes the config so they pick it up on the next check-in. Mirrors BulkKioskUpdate,
// but resolves its target from the Actions composer's target section.
func (h *Handler) applyKioskForTargets(w http.ResponseWriter, r *http.Request, targetType string) {
	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	if enabled && pkg == "" {
		http.Error(w, "Pick an app to lock to when enabling kiosk mode.", http.StatusBadRequest)
		return
	}
	if !enabled {
		pkg = ""
	}
	ids, err := h.resolveTargetDeviceIDs(r, targetType)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if len(ids) == 0 {
		h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape("No devices matched the target.")+"&flash_type=info")
		return
	}
	if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
		http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
		return
	}
	if err := h.db.SetKioskConfigForDevices(r.Context(), ids, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushKioskConfigToDevices(r.Context(), ids)
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	h.audit(r, "device.kiosk_bulk", verb, fmt.Sprintf("devices=%d, package=%s", len(ids), pkg))
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("%s kiosk on %d device(s).", verb, len(ids)))+"&flash_type=success")
}

// resolvePolicyTargetIDs resolves a stored kiosk policy's target to the matching
// device IDs AT THIS MOMENT — a snapshot, not a live binding (same convention as
// resolveTargetDeviceIDs uses for the "all" target elsewhere: a device added to a
// target group later doesn't retroactively pick up the policy without a re-save).
func (h *Handler) resolvePolicyTargetIDs(ctx context.Context, targetType string, targetID *uuid.UUID, targetSerial string) ([]uuid.UUID, error) {
	switch targetType {
	case "all":
		return h.db.GetAllDeviceIDs(ctx)
	case "restaurant":
		if targetID == nil {
			return nil, nil
		}
		return h.db.GetDeviceIDsByRestaurantIDs(ctx, []uuid.UUID{*targetID})
	case "group":
		if targetID == nil {
			return nil, nil
		}
		return h.db.GetDeviceIDsInGroups(ctx, []uuid.UUID{*targetID})
	case "device":
		if targetSerial == "" {
			return nil, nil
		}
		return h.db.GetDeviceIDsBySerials(ctx, []string{targetSerial})
	default:
		return nil, nil
	}
}

// manageTargetLabel renders a policy's target as the short tag shown on its card
// ("Drive-Thru Kiosks (group)", "Riverside Ave (restaurant)", a bare serial, or
// "All devices").
func manageTargetLabel(p db.KioskPolicy, restaurants []db.Restaurant, groups []db.Group) string {
	switch p.TargetType {
	case "all":
		return "All devices"
	case "restaurant":
		for _, r := range restaurants {
			if p.TargetID != nil && r.ID == *p.TargetID {
				return r.Name + " (restaurant)"
			}
		}
		return "Restaurant"
	case "group":
		for _, g := range groups {
			if p.TargetID != nil && g.ID == *p.TargetID {
				return g.Name + " (group)"
			}
		}
		return "Group"
	case "device":
		return p.TargetSerial
	default:
		return "—"
	}
}

// Manage renders the standing device-configuration page: named kiosk policies (more
// policy types — e.g. charging-pad — land here later), not a device list or a
// one-shot bulk-apply form. A policy is a durable object (create/edit/duplicate/
// delete); applying one writes device_config directly, same mechanism the page
// always used, just now remembered as a named thing instead of a fire-and-forget
// action. See resolvePolicyTargetIDs for what "device count" means here.
func (h *Handler) Manage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	policies, err := h.db.ListKioskPolicies(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	restaurants, _ := h.db.ListRestaurants(ctx)
	groups, _ := h.db.ListGroups(ctx)
	fleetPackages, _ := h.db.SearchFleetPackages(ctx, "")
	pkgNames := make(map[string]string, len(fleetPackages))
	for _, p := range fleetPackages {
		if p.AppName != "" {
			pkgNames[p.PackageName] = p.AppName
		}
	}

	type policyView struct {
		db.KioskPolicy
		AppName      string
		TargetLabel  string
		DeviceCount  int
		CoveragePct  int
		Unsupported  int // targets whose agent cannot lock the screen
	}
	unlockedCount, _ := h.db.CountUnlockedDevices(ctx)
	totalDevices, _ := h.db.CountDevices(ctx, db.DeviceFilter{})

	type resolved struct {
		p   db.KioskPolicy
		ids []uuid.UUID
	}
	resolvedPolicies := make([]resolved, 0, len(policies))
	covered := 0
	groupTargets := map[uuid.UUID]bool{}
	// Mixed fleet: a kiosk policy only lands on devices whose agent can lock the
	// screen. Count, per policy and overall, the targets that cannot honour it, and
	// split coverage by kind so it is obvious when a policy misses one side.
	coveredByKind := map[string]int{}
	unsupportedByPolicy := map[uuid.UUID]int{}
	seenCovered := map[uuid.UUID]bool{}
	for _, p := range policies {
		ids, _ := h.resolvePolicyTargetIDs(ctx, p.TargetType, p.TargetID, p.TargetSerial)
		resolvedPolicies = append(resolvedPolicies, resolved{p, ids})
		covered += len(ids)
		if p.TargetType == "group" && p.TargetID != nil {
			groupTargets[*p.TargetID] = true
		}
		if devs, err := h.db.GetDevicesByIDs(ctx, ids); err == nil {
			for _, d := range devs {
				if !d.Supports("kiosk_set") {
					unsupportedByPolicy[p.ID]++
				}
				if !seenCovered[d.ID] {
					seenCovered[d.ID] = true
					coveredByKind[d.AgentKind]++
				}
			}
		}
	}
	_, fleetFirmware, fleetDPC, _ := h.db.FleetComposition(ctx, h.access(r).hidesDPC())
	// Coverage math (for the "X of Y devices" headline and per-policy meters) needs
	// a denominator at least as large as what's covered: resolvePolicyTargetIDs can
	// legitimately include devices CountDevices excludes (e.g. hidden/retired units
	// still sitting in a targeted group), so a raw fleet count can undercount.
	if covered > totalDevices {
		totalDevices = covered
	}

	views := make([]policyView, 0, len(resolvedPolicies))
	for _, rp := range resolvedPolicies {
		appName := rp.p.KioskPackage
		if n, ok := pkgNames[rp.p.KioskPackage]; ok {
			appName = n
		}
		pct := 0
		if totalDevices > 0 {
			pct = len(rp.ids) * 100 / totalDevices
			if pct == 0 && len(rp.ids) > 0 {
				pct = 1
			}
		}
		views = append(views, policyView{
			KioskPolicy: rp.p, AppName: appName,
			TargetLabel: manageTargetLabel(rp.p, restaurants, groups),
			DeviceCount: len(rp.ids),
			CoveragePct: pct,
			Unsupported: unsupportedByPolicy[rp.p.ID],
		})
	}

	// "Default" — devices with no lock applied. Read live off device_config rather
	// than derived set-subtraction from policy targets: a device can be unlocked
	// directly (device page) without ever being "released" by a policy, and the
	// stored device_config row is the actual truth of what's on the device.

	role := h.role(r)
	h.render(w, r, "manage.html", map[string]any{
		"Title":          "Manage",
		"Policies":       views,
		"PolicyCount":    len(views),
		"CoveredCount":   covered,
		"UnlockedCount":  unlockedCount,
		"TotalDevices":   totalDevices,
		"GroupsTargeted": len(groupTargets),
		"Restaurants":    restaurants,
		"Groups":         groups,
		"CanEdit":        roleCanOperate(role),
		"CoveredFirmware": coveredByKind["firmware"],
		"CoveredDPC":      coveredByKind["dpc"],
		// Targets that resolve to inactive (hidden) devices: counted in "covered"
		// but not in either kind, since the split reads active devices only.
		"CoveredInactive": covered - coveredByKind["firmware"] - coveredByKind["dpc"],
		"FleetFirmware":   fleetFirmware,
		"FleetDPC":        fleetDPC,
		"ActivePage":      "manage",
	})
}

// managePolicyFormData builds the data every new/edit policy form page needs:
// target pickers (restaurant/group dropdowns + a searchable device list) and the
// app picker. Shared so the two page handlers below stay in sync.
func (h *Handler) managePolicyFormData(r *http.Request) map[string]any {
	ctx := r.Context()
	restaurants, _ := h.db.ListRestaurants(ctx)
	groups, _ := h.db.ListGroups(ctx)
	fleetPackages, _ := h.db.SearchFleetPackages(ctx, "")
	devices, _ := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "", "")
	connected := h.hub.ConnectedIDsForDisplay()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}
	role := h.role(r)
	return map[string]any{
		"Restaurants":   restaurants,
		"Groups":        groups,
		"Devices":       devices,
		"Online":        online,
		"FleetPackages": fleetPackages,
		"CanEdit":       roleCanOperate(role),
	}
}

// ManagePolicyNew renders the "new kiosk policy" page — a standalone page rather
// than a modal, so the target/app pickers have room to be more than cramped popup
// widgets.
func (h *Handler) ManagePolicyNew(w http.ResponseWriter, r *http.Request) {
	if role := h.role(r); !roleCanOperate(role) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	data := h.managePolicyFormData(r)
	data["Title"] = "New kiosk policy"
	h.render(w, r, "manage_policy_form.html", data)
}

// ManagePolicyEditPage renders the same form pre-filled for an existing policy.
func (h *Handler) ManagePolicyEditPage(w http.ResponseWriter, r *http.Request) {
	if role := h.role(r); !roleCanOperate(role) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	policy, err := h.db.GetKioskPolicy(r.Context(), id)
	if err != nil {
		http.Error(w, "Policy not found", http.StatusNotFound)
		return
	}
	data := h.managePolicyFormData(r)
	data["Title"] = "Edit kiosk policy"
	data["Policy"] = policy
	h.render(w, r, "manage_policy_form.html", data)
}

// ManagePolicySave creates a new kiosk policy or updates an existing one (an "id"
// form field selects update), then immediately applies it to its target's current
// devices — same write path applyKioskForTargets always used.
func (h *Handler) ManagePolicySave(w http.ResponseWriter, r *http.Request) {
	if role := h.role(r); !roleCanOperate(role) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	targetType := r.FormValue("target_type")
	if name == "" || pkg == "" {
		h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape("A policy needs a name and a locked app.")+"&flash_type=info")
		return
	}
	var targetID *uuid.UUID
	if idStr := r.FormValue("target_id"); idStr != "" {
		if id, err := uuid.Parse(idStr); err == nil {
			targetID = &id
		}
	}
	targetSerial := strings.TrimSpace(r.FormValue("target_serial"))
	if targetType != "all" && targetType != "restaurant" && targetType != "group" && targetType != "device" {
		h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape("Pick what this policy applies to.")+"&flash_type=info")
		return
	}

	ctx := r.Context()
	var policyID uuid.UUID
	if idStr := r.FormValue("id"); idStr != "" {
		if id, err := uuid.Parse(idStr); err == nil {
			policyID = id
			if err := h.db.UpdateKioskPolicy(ctx, id, name, pkg, targetType, targetID, targetSerial); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
		}
	}
	if policyID == uuid.Nil {
		id, err := h.db.CreateKioskPolicy(ctx, name, pkg, targetType, targetID, targetSerial)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		policyID = id
	}

	ids, err := h.resolvePolicyTargetIDs(ctx, targetType, targetID, targetSerial)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
		http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
		return
	}
	if err := h.db.SetKioskConfigForDevices(ctx, ids, true, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushKioskConfigToDevices(ctx, ids)
	h.audit(r, "device.kiosk_policy_save", name, fmt.Sprintf("policy=%s, devices=%d, package=%s", policyID, len(ids), pkg))
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("Saved %q — locked %d device(s).", name, len(ids)))+"&flash_type=success")
}

// ManagePolicyDuplicate clones a policy (name suffixed) without re-applying it —
// the clone starts as its own independent policy the user can retarget before saving.
func (h *Handler) ManagePolicyDuplicate(w http.ResponseWriter, r *http.Request) {
	if role := h.role(r); !roleCanOperate(role) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid policy ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	p, err := h.db.GetKioskPolicy(ctx, id)
	if err != nil {
		http.Error(w, "Policy not found", http.StatusNotFound)
		return
	}
	if _, err := h.db.CreateKioskPolicy(ctx, p.Name+" (copy)", p.KioskPackage, p.TargetType, p.TargetID, p.TargetSerial); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.kiosk_policy_duplicate", p.Name, "")
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("Duplicated %q.", p.Name))+"&flash_type=success")
}

// ManagePolicyDelete removes a policy and unlocks whatever devices it currently
// covers — deleting the thing that locked them releases them, matching what a user
// expects "delete the policy" to mean rather than leaving devices silently locked
// with nothing left to manage them.
func (h *Handler) ManagePolicyDelete(w http.ResponseWriter, r *http.Request) {
	if role := h.role(r); !roleCanOperate(role) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid policy ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	p, err := h.db.GetKioskPolicy(ctx, id)
	if err != nil {
		http.Error(w, "Policy not found", http.StatusNotFound)
		return
	}
	ids, _ := h.resolvePolicyTargetIDs(ctx, p.TargetType, p.TargetID, p.TargetSerial)
	if len(ids) > 0 {
		if err := h.db.SetKioskConfigForDevices(ctx, ids, false, "", 0); err == nil {
			h.pushKioskConfigToDevices(ctx, ids)
		}
	}
	if err := h.db.DeleteKioskPolicy(ctx, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.kiosk_policy_delete", p.Name, fmt.Sprintf("devices_unlocked=%d", len(ids)))
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("Deleted %q — unlocked %d device(s).", p.Name, len(ids)))+"&flash_type=success")
}

// BootLogo renders the Boot logo config page: a dedicated splash-upload + target
// picker whose submit runs through the normal update_splash command path (POST
// /commands), plus the status of the most recently applied splash. update_splash
// is hidden from the Actions/history/device command lists (see ListCommandsSince /
// GetDeviceCommands), so this page is the single place boot logos are managed.
func (h *Handler) BootLogo(w http.ResponseWriter, r *http.Request) {
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var (
		last       *db.Command
		stats      deliveryStats
		deliveries []db.CommandDelivery
		splashURL  string
	)
	if c, err := h.db.GetLatestCommandByType(r.Context(), "update_splash"); err == nil && c != nil {
		last = c
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		splashURL = p.URL
		if ds, err := h.db.GetCommandDeliveries(r.Context(), c.ID, h.cfg.CommandExpiry()); err == nil {
			h.markDeliveryPresence(ds)
			deliveries = ds
			stats = computeDeliveryStats(ds)
		}
	}
	h.render(w, r, "boot_logo.html", map[string]any{
		"Title":      "Boot logo",
		"Groups":     groups,
		"Last":       last,
		"SplashURL":  splashURL,
		"Stats":      stats,
		"Deliveries": deliveries,
	})
}

func (h *Handler) CommandCreate(w http.ResponseWriter, r *http.Request) {
	// update_splash may carry a file upload (multipart); other types are urlencoded.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(uploadSizeLimit); err != nil {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
	} else {
		r.ParseForm()
	}
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}

	// set_kiosk isn't a queued command — it writes kiosk config to the target set (like
	// the old bulk-kiosk action). Gate it directly (admin/dev/operator), not via the
	// command-role machinery the real commands use.
	if cmdType == "set_kiosk" {
		if role := h.role(r); !roleCanOperate(role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	} else if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	if cmdType == "shell" && !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}

	// A "query" is a read-only diagnostic whose command text comes only from the
	// admin-curated catalog (chosen by query_id), never from the client. That's why
	// admin/dev/operator may issue it without raw-shell rights: whatever shell_cmd the
	// client might send is ignored — the payload is rebuilt from the catalog here.
	// The command is stored as type "query" and only translated to "shell" at the
	// device boundary (see deviceCommandType), so the dashboard shows it as a Query.
	var queryPayload json.RawMessage
	if cmdType == "query" {
		qid, _ := strconv.Atoi(r.FormValue("query_id"))
		q, err := h.db.GetDeviceQuery(r.Context(), qid)
		if err != nil || !q.Enabled {
			http.Error(w, "Unknown query", http.StatusBadRequest)
			return
		}
		b, _ := json.Marshal(map[string]any{"cmd": q.Command, "query_id": q.ID, "query": q.Label})
		queryPayload = json.RawMessage(b)
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if isDestructiveCmd(cmdType) && h.cfg.RequireReason() && reason == "" {
		http.Error(w, "A reason is required for this command.", http.StatusBadRequest)
		return
	}

	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}

	// "set_kiosk" writes kiosk config to the resolved target devices immediately
	// (pushed on their next check-in), then returns — no command row is created.
	if cmdType == "set_kiosk" {
		h.applyKioskForTargets(w, r, targetType)
		return
	}

	// install_apk and uninstall accept MULTIPLE selections — one command is created
	// per chosen app / package (the command + client model is one-app-per-command).
	// Every other type carries a single payload.
	installURLs := dedupeStrings(formValues(r, "apk_url"))
	uninstallPkgs := dedupeStrings(formValues(r, "package"))
	if cmdType == "install_apk" && len(installURLs) == 0 {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}
	if cmdType == "uninstall" && len(uninstallPkgs) == 0 {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}
	if cmdType == "update_splash" {
		// An uploaded BMP/PNG/JPEG is wrapped into a splash.img server-side and
		// served from /splash-img/; its URL then drives the normal flow. A pasted
		// splash_url is the fallback when no file is uploaded.
		url, err := h.generateSplashFromUpload(r)
		if err != nil {
			http.Error(w, "splash upload failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		if url != "" {
			r.Form.Set("splash_url", url)
		}
		if strings.TrimSpace(r.FormValue("splash_url")) == "" {
			http.Redirect(w, r, "/commands", http.StatusFound)
			return
		}
	}

	// Resolve the target device set once — every command we create shares it.
	var targetIDs []uuid.UUID
	switch targetType {
	case "all":
		// Snapshot: resolve all current device IDs so future devices are unaffected.
		ids, err := h.db.GetAllDeviceIDs(r.Context())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "devices":
		serials := db.ParseSerials(r.FormValue("target_serials"))
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetIDs = ids
	case "groups":
		var gids []uuid.UUID
		for _, gid := range r.Form["target_groups"] {
			if id, err := uuid.Parse(gid); err == nil {
				gids = append(gids, id)
			}
		}
		// Snapshot group membership to concrete device IDs NOW, like all/scope — a one-shot
		// command (install/reboot/…) targets the CURRENT members, so a device that joins the
		// group later doesn't pick up a stale command on its first connect, and the install
		// dedup (device-targeted) runs for group sends too.
		ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), gids)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "scope":
		// Snapshot the scope-rail selection (collection + refine) to device IDs now.
		ids, err := h.resolveScopeDeviceIDs(r)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "mixed":
		// Restaurants + groups + serials in one send, de-duplicated by device.
		ids, err := h.resolveMixedDeviceIDs(r)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	}
	// Per-user access policy: drop the devices this operator may not send this
	// command to. Nothing left → refuse; some dropped → proceed and say so.
	if acc := h.access(r); !acc.unrestricted() {
		kept, dropped := acc.filterDevices(policyActionForCommand(cmdType), targetIDs)
		if len(kept) == 0 && len(targetIDs) > 0 {
			http.Error(w, "Your access policy does not allow this command on the selected devices.", http.StatusForbidden)
			return
		}
		if dropped > 0 {
			log.Printf("[access] %s: %s skipped %d device(s) outside their policy", h.currentUsername(r), cmdType, dropped)
		}
		targetIDs = kept
	}

	// Capability gating: a device whose agent cannot run this command is dropped
	// here (and named in the result) instead of receiving a command it would only
	// fail. Mirrors the impact preview so what was promised is what fires.
	unsupportedSkipped := 0
	unsupSkipped := map[string]bool{}
	if product.CommandNeeds(cmdType) != "" && len(targetIDs) > 0 {
		if devs, err := h.db.GetDevicesByIDs(r.Context(), targetIDs); err == nil {
			can := make(map[uuid.UUID]bool, len(devs))
			for _, d := range devs {
				if d.Supports(cmdType) {
					can[d.ID] = true
				} else {
					unsupSkipped[d.SerialNumber] = true
					unsupportedSkipped++
				}
			}
			kept := targetIDs[:0:0]
			for _, id := range targetIDs {
				if can[id] {
					kept = append(kept, id)
				}
			}
			if len(kept) == 0 {
				http.Error(w, fmt.Sprintf("None of the selected devices can run %s on their agent.", cmdTypeLabel(cmdType)), http.StatusBadRequest)
				return
			}
			if unsupportedSkipped > 0 {
				h.audit(r, "command.send.unsupported", cmdType, fmt.Sprintf("skipped %d device(s) whose agent cannot run it", unsupportedSkipped))
			}
			targetIDs = kept
		}
	}

	// Reject a command that resolves to no devices (e.g. "all" on an empty fleet, or
	// serials that match nothing) rather than inserting an orphan command with no
	// targets that no device will ever pick up.
	if len(targetIDs) == 0 {
		http.Error(w, "No matching target devices.", http.StatusBadRequest)
		return
	}

	// Build the per-command work items (apkURL + payload).
	type cmdItem struct {
		apkURL  string
		payload json.RawMessage
	}
	var items []cmdItem
	switch cmdType {
	case "install_apk":
		for _, u := range installURLs {
			items = append(items, cmdItem{apkURL: u})
		}
	case "uninstall":
		for _, p := range uninstallPkgs {
			b, _ := json.Marshal(map[string]string{"package": p})
			items = append(items, cmdItem{payload: json.RawMessage(b)})
		}
	case "query":
		items = []cmdItem{{payload: queryPayload}}
	default:
		items = []cmdItem{{apkURL: strings.TrimSpace(r.FormValue("apk_url")), payload: buildPayload(cmdType, r)}}
	}

	// Create one command per item. For install_apk, drop target devices that already
	// have this exact APK in flight or already report its package (unless "Reinstall
	// anyway"); an app whose whole target set is skipped simply creates no command.
	reinstall := r.FormValue("reinstall") != ""
	skippedSet := unsupSkipped // starts with the devices whose agent cannot run the command
	var created []*db.Command

	// Pre-fetch APK size/ETag for every install URL CONCURRENTLY with a short bound, so a
	// multi-app install doesn't serialize N slow HEAD requests (that hung the install
	// popup for many seconds). Augment already degrades gracefully to no size/etag on a
	// slow/failed HEAD, so a laggy file host can't block command creation.
	augmentedPayload := map[string]json.RawMessage{}
	if cmdType == "install_apk" && len(items) > 0 {
		actx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, it := range items {
			u := it.apkURL
			wg.Add(1)
			go func(base json.RawMessage) {
				defer wg.Done()
				p := apkmeta.Augment(actx, u, base)
				mu.Lock()
				augmentedPayload[u] = p
				mu.Unlock()
			}(it.payload)
		}
		wg.Wait()
		cancel()
	}

	// Several apps in one send form one batch: every command carries the same
	// payload.batch id, and the history shows them as one "Install N apps" row.
	batchID := ""
	if len(items) > 1 {
		batchID = uuid.New().String()
	}
	for _, it := range items {
		ids := targetIDs
		if cmdType == "install_apk" && targetType == "devices" && len(ids) > 0 && !reinstall {
			skip := make(map[uuid.UUID]bool)
			inflight, err := h.db.DevicesWithPendingInstall(r.Context(), it.apkURL, ids, h.cfg.CommandExpiry())
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			for id := range inflight {
				skip[id] = true
			}
			installed, err := h.db.DevicesWithPackageInstalled(r.Context(), it.apkURL, ids)
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			for id := range installed {
				skip[id] = true
			}
			if len(skip) > 0 {
				fresh := make([]uuid.UUID, 0, len(ids))
				var skippedIDs []uuid.UUID
				for _, id := range ids {
					if skip[id] {
						skippedIDs = append(skippedIDs, id)
					} else {
						fresh = append(fresh, id)
					}
				}
				if devs, e := h.db.GetDevicesByIDs(r.Context(), skippedIDs); e == nil {
					for _, d := range devs {
						skippedSet[d.SerialNumber] = true
					}
				}
				ids = fresh
			}
		}
		if len(ids) == 0 {
			continue // every target already has / is installing this app
		}
		payload := it.payload
		if cmdType == "install_apk" {
			// Use the size/ETag fetched concurrently above (falls back to the base payload).
			if p, ok := augmentedPayload[it.apkURL]; ok {
				payload = p
			}
		}
		if batchID != "" {
			payload = withBatch(payload, batchID)
		}
		cmd, err := h.db.CreateCommandBy(r.Context(), cmdType, it.apkURL, payload, targetType, ids, h.currentUsername(r))
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.pushCommand(r.Context(), cmd, targetType, ids)
		created = append(created, cmd)
	}

	if len(created) == 0 {
		h.audit(r, "command.send.skip", cmdType, fmt.Sprintf("all %d target(s) already have or are installing the selected app(s)", len(targetIDs)))
		h.hxRedirect(w, r, "/commands?flash="+url.QueryEscape("All selected device(s) already have or are installing the selected app(s) — nothing queued.")+"&flash_type=info")
		return
	}

	detail := fmt.Sprintf("target=%s, devices=%d, commands=%d", targetType, len(targetIDs), len(created))
	if reason != "" {
		detail += ", reason=" + reason
	}
	if len(created) == 1 {
		detail += ", cmd=" + created[0].ID.String()
	}
	h.audit(r, "command.send", cmdType, detail)

	// Boot logo is applied from its own config page — return there (with status),
	// not to the command-detail view, and never into the command history flow.
	if cmdType == "update_splash" {
		h.hxRedirect(w, r, "/boot-logo?flash="+url.QueryEscape(fmt.Sprintf("Boot logo applied to %d device(s).", len(targetIDs)))+"&flash_type=success")
		return
	}

	skipped := make([]string, 0, len(skippedSet))
	for s := range skippedSet {
		skipped = append(skipped, s)
	}
	sort.Strings(skipped)

	// Device-page install (progress_view=1): don't navigate anywhere. Fire device-updated
	// so the device page's Applications drawer refreshes and shows the queued apps inline
	// as "Installing…" — the popup already closed client-side. No separate progress/history
	// page; the drawer IS the combined live view.
	if cmdType == "install_apk" && r.FormValue("progress_view") == "1" {
		w.Header().Set("HX-Trigger", "device-updated")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// One command → its detail view (with any skip note). Multiple → back to Actions
	// with a summary, since there's no single command to open.
	if len(created) == 1 {
		dest := "/commands/" + created[0].ID.String()
		if n := len(skipped); n > 0 {
			msg := fmt.Sprintf("Sent to %d device(s). Skipped %d that already have or are installing this app: %s", len(targetIDs), n, strings.Join(skipped, ", "))
			dest += "?flash=" + url.QueryEscape(msg) + "&flash_type=info"
		}
		h.hxRedirect(w, r, dest)
		return
	}
	msg := fmt.Sprintf("Queued %d commands to %d device(s).", len(created), len(targetIDs))
	if n := len(skipped); n > 0 {
		msg += fmt.Sprintf(" Skipped %d device(s) that already had one of the apps.", n)
	}
	h.hxRedirect(w, r, "/commands?flash="+url.QueryEscape(msg)+"&flash_type=success")
}

// pkgNameSet is the set of package names a device currently reports installed, so the
// install picker can hide apps that are already on the device.
func pkgNameSet(pkgs []db.DevicePackage) map[string]bool {
	m := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if p.PackageName != "" {
			m[p.PackageName] = true
		}
	}
	return m
}

// apkURLToName maps each library app's apk_url to its display name, so the command
// history can label an install with the app name instead of the raw APK URL.
func apkURLToName(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.ApkURL != "" && a.Name != "" {
			m[a.ApkURL] = a.Name
		}
	}
	return m
}

// formValues returns the trimmed, non-empty values of a repeated form field.
func formValues(r *http.Request, key string) []string {
	var out []string
	for _, v := range r.Form[key] {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// dedupeStrings preserves order while dropping duplicate values.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RecipeCreate saves the current Actions-builder state as a named recipe that can
// be replayed in one click. It mirrors CommandCreate's field parsing but persists
// the preset instead of dispatching it.
func (h *Handler) RecipeCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	cmdType := r.FormValue("type")
	if name == "" || cmdType == "" {
		http.Error(w, "name and type are required", http.StatusBadRequest)
		return
	}
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		targetType = "all"
	}
	rec := db.Recipe{
		Name:          name,
		Type:          cmdType,
		ApkURL:        strings.TrimSpace(r.FormValue("apk_url")),
		Payload:       buildPayload(cmdType, r),
		TargetType:    targetType,
		TargetSerials: db.ParseSerials(r.FormValue("target_serials")),
		CreatedBy:     h.role(r),
	}
	for _, g := range r.Form["target_groups"] {
		if id, err := uuid.Parse(g); err == nil {
			rec.TargetGroups = append(rec.TargetGroups, id)
		}
	}
	// Capture the scope-rail selection so a scheduled run can re-resolve it.
	if targetType == "scope" {
		scope, _ := json.Marshal(map[string]string{
			"mode":    r.FormValue("scope_mode"),
			"id":      r.FormValue("scope_id"),
			"status":  r.FormValue("scope_status"),
			"battery": r.FormValue("scope_battery"),
			"kiosk":   r.FormValue("scope_kiosk"),
		})
		rec.Scope = scope
	}
	if _, err := h.db.CreateRecipe(r.Context(), rec); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// RecipeDelete removes a saved recipe.
func (h *Handler) RecipeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid recipe ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteRecipe(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// ── Scheduled recipes ────────────────────────────────────────────────────────────

// schedulableType reports whether a recipe of this command type can be scheduled.
// logcat and set_kiosk are excluded — they aren't queued commands (they fan out via
// their own mechanisms and their params aren't captured in a recipe payload).
func schedulableType(t string) bool {
	switch t {
	case "install_apk", "uninstall", "reboot", "screenshot", "shell", "update_splash":
		return true
	}
	return false
}

// ProcessDueScheduledRecipes fires every schedule whose next run has arrived, then
// advances it (next cron time) or disables it (run-once). Called from a 1-min ticker.
func (h *Handler) ProcessDueScheduledRecipes(ctx context.Context) {
	now := time.Now().UTC()
	due, err := h.db.ListDueScheduledRecipes(ctx, now)
	if err != nil {
		log.Printf("[recipe-scheduler] list due: %v", err)
		return
	}
	for _, s := range due {
		if err := h.fireSchedule(ctx, s); err != nil {
			log.Printf("[recipe-scheduler] fire schedule=%s recipe=%q: %v", s.ID, s.RecipeName, err)
		}
		// Advance: compute the next cron time, or disable a run-once (or unparseable) one.
		var next *time.Time
		disable := s.RunOnce
		if !disable {
			if n, err := db.NextCron(s.CronExpr, now); err == nil {
				n = n.UTC()
				next = &n
			} else {
				disable = true
			}
		}
		if err := h.db.MarkScheduleFired(ctx, s.ID, now, next, disable); err != nil {
			log.Printf("[recipe-scheduler] mark fired %s: %v", s.ID, err)
		}
	}
}

// fireSchedule resolves the recipe's target now and dispatches it as a command.
func (h *Handler) fireSchedule(ctx context.Context, s db.ScheduledRecipe) error {
	rec, err := h.db.GetRecipe(ctx, s.RecipeID)
	if err != nil {
		return err
	}
	if !schedulableType(rec.Type) {
		return fmt.Errorf("recipe type %q is not schedulable", rec.Type)
	}
	ids, err := h.db.ResolveTargetIDs(ctx, rec.TargetType, rec.TargetSerials, rec.TargetGroups, rec.Scope)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		log.Printf("[recipe-scheduler] schedule=%s recipe=%q matched 0 devices — skipped", s.ID, rec.Name)
		return nil
	}
	if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
		return fmt.Errorf("resolved %d devices exceeds the max-targets limit %d", len(ids), max)
	}
	cmd, err := h.db.CreateCommandBy(ctx, rec.Type, rec.ApkURL, rec.Payload, "devices", ids, "Scheduled recipe: "+rec.Name)
	if err != nil {
		return err
	}
	h.pushCommand(ctx, cmd, "devices", ids)
	log.Printf("[recipe-scheduler] fired schedule=%s recipe=%q type=%s devices=%d", s.ID, rec.Name, rec.Type, len(ids))
	return nil
}

// ScheduleList renders the scheduled-recipes page.
func (h *Handler) ScheduleList(w http.ResponseWriter, r *http.Request) {
	schedules, err := h.db.ListScheduledRecipes(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	recipes, _ := h.db.ListRecipes(r.Context())
	// Only offer schedulable recipes in the "new schedule" picker.
	var pick []db.Recipe
	for _, rec := range recipes {
		if schedulableType(rec.Type) {
			pick = append(pick, rec)
		}
	}
	h.render(w, r, "schedules.html", map[string]any{
		"Title":     "Scheduled recipes",
		"Schedules": schedules,
		"Recipes":   pick,
	})
}

// ScheduleCreate schedules a recipe on a cron.
func (h *Handler) ScheduleCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	recipeID, err := uuid.Parse(r.FormValue("recipe_id"))
	if err != nil {
		http.Error(w, "Pick a recipe", http.StatusBadRequest)
		return
	}
	rec, err := h.db.GetRecipe(r.Context(), recipeID)
	if err != nil {
		http.Error(w, "Recipe not found", http.StatusNotFound)
		return
	}
	if !schedulableType(rec.Type) {
		http.Error(w, "That recipe's action can't be scheduled.", http.StatusBadRequest)
		return
	}
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), rec.Type)) {
		return
	}
	cronExpr := strings.TrimSpace(r.FormValue("cron_expr"))
	next, err := db.NextCron(cronExpr, time.Now().UTC())
	if err != nil {
		h.hxRedirect(w, r, "/schedules?flash="+url.QueryEscape("Invalid schedule: "+err.Error())+"&flash_type=error")
		return
	}
	if _, err := h.db.CreateScheduledRecipe(r.Context(), recipeID, cronExpr, r.FormValue("run_once") == "on", next.UTC(), h.role(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "schedule.create", rec.Name, "cron="+cronExpr)
	http.Redirect(w, r, "/schedules", http.StatusFound)
}

// ScheduleDelete removes a schedule.
func (h *Handler) ScheduleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	_ = h.db.DeleteScheduledRecipe(r.Context(), id)
	http.Redirect(w, r, "/schedules", http.StatusFound)
}

// ScheduleToggle enables/disables a schedule (recomputing next run when enabling).
func (h *Handler) ScheduleToggle(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	enable := r.FormValue("enable") == "1"
	var next *time.Time
	if enable {
		// need the cron to recompute — fetch via the list (small) then set.
		list, _ := h.db.ListScheduledRecipes(r.Context())
		for _, s := range list {
			if s.ID == id {
				if n, err := db.NextCron(s.CronExpr, time.Now().UTC()); err == nil {
					n = n.UTC()
					next = &n
				}
				break
			}
		}
	}
	_ = h.db.SetScheduleEnabled(r.Context(), id, enable, next)
	http.Redirect(w, r, "/schedules", http.StatusFound)
}

// ScheduleRunNow fires a schedule immediately (without changing its cadence).
func (h *Handler) ScheduleRunNow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	list, _ := h.db.ListScheduledRecipes(r.Context())
	for _, s := range list {
		if s.ID == id {
			if err := h.fireSchedule(r.Context(), s); err != nil {
				h.hxRedirect(w, r, "/schedules?flash="+url.QueryEscape("Run failed: "+err.Error())+"&flash_type=error")
				return
			}
			ranAt := time.Now().UTC()
			_ = h.db.MarkScheduleFired(r.Context(), id, ranAt, s.NextRunAt, false)
			break
		}
	}
	h.hxRedirect(w, r, "/schedules?flash="+url.QueryEscape("Recipe run now.")+"&flash_type=success")
}

// isDestructiveCmd marks command types that change device state in a way that
// warrants a reason when the RequireReason setting is on.
func isDestructiveCmd(t string) bool {
	return t == "reboot" || t == "ota" || t == "update_splash" || t == "wipe"
}

func buildPayload(cmdType string, r *http.Request) json.RawMessage {
	switch cmdType {
	case "shell":
		cmd := strings.TrimSpace(r.FormValue("shell_cmd"))
		b, _ := json.Marshal(map[string]string{"cmd": cmd})
		return json.RawMessage(b)
	case "uninstall":
		b, _ := json.Marshal(map[string]string{"package": strings.TrimSpace(r.FormValue("package"))})
		return json.RawMessage(b)
	case "mic_gain_set":
		// value = TX_DEC Volume to enforce (e.g. 102); empty clears enforcement.
		b, _ := json.Marshal(map[string]string{"value": strings.TrimSpace(r.FormValue("value"))})
		return json.RawMessage(b)
	case "update_splash":
		// Client downloads url (a splash.img: 0x4000 zero filler + BMP), validates
		// the BMP signature at 0x4000 and (if given) partition_size, then stages +
		// triggers the init broker.
		m := map[string]any{"url": strings.TrimSpace(r.FormValue("splash_url"))}
		if ps, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("partition_size")), 10, 64); err == nil && ps > 0 {
			m["partition_size"] = ps
		}
		b, _ := json.Marshal(m)
		return json.RawMessage(b)
	default:
		return json.RawMessage("{}")
	}
}

// ── Setup ─────────────────────────────────────────────────────────────────────

func (h *Handler) SetupPage(w http.ResponseWriter, r *http.Request) {
	apps, err := h.db.ListApps(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "setup.html", map[string]any{
		"Title": "App Repository",
		"Apps":  apps,
	})
}

func (h *Handler) SetupCreateApp(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if _, err := h.db.CreateApp(r.Context(), name, apkURL, pkg); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup", http.StatusFound)
}

func (h *Handler) SetupCreateAppJSON(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	app, err := h.db.CreateApp(r.Context(), name, apkURL, pkg)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// ---- S3 APK uploads: browser uploads directly to S3 via a presigned PUT ------

// AppUploadURL returns a presigned S3 PUT URL so the browser sends the APK straight
// to S3, never through this server. Admin only.
func (h *Handler) AppUploadURL(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "APK upload is not configured")
		return
	}
	key := h.apk.Key(uuid.NewString() + ".apk")
	url, err := h.apk.PresignPut(r.Context(), key, "application/vnd.android.package-archive", 15*time.Minute)
	if err != nil {
		log.Printf("presign put: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "could not create upload URL")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url, "key": key})
}

// AppRegister reads the freshly-uploaded object back from S3, parses its package,
// version and launcher icon, creates the repository app, and feeds the shared icon
// index (so the icon shows everywhere immediately). Admin only.
func (h *Handler) AppRegister(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "APK upload is not configured")
		return
	}
	var req struct {
		Key string `json:"key"`
		// Client-parsed metadata (fast path — the browser already has the file, so it
		// parses locally and the server never downloads the APK from S3). If Package is
		// empty, the server falls back to parsing the object itself.
		Package string `json:"package"`
		Version string `json:"version"`
		Name    string `json:"name"`
		Icon    string `json:"icon"` // base64 PNG
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "key required")
		return
	}
	pkg, version, name, icon := req.Package, req.Version, req.Name, req.Icon
	if pkg == "" {
		// Fallback: no client metadata — parse the object server-side (slower).
		meta, err := h.apk.Parse(r.Context(), req.Key)
		if err != nil {
			log.Printf("apk parse %s: %v", req.Key, err)
			writeJSONError(w, http.StatusBadRequest, "could not parse APK — is it a valid .apk?")
			return
		}
		pkg, version, name, icon = meta.Package, meta.VersionName, meta.Label, meta.IconPNGB64
	}
	if len(icon) > 128*1024 { // safety cap; a 96px PNG is far smaller
		icon = ""
	}
	if name == "" {
		name = pkg
	}
	// apk_url must be an absolute, device-reachable URL (the device fetches it directly).
	base := h.baseURL(r)
	app, err := h.db.CreateS3App(r.Context(), name, pkg, version, req.Key, base)
	if err != nil {
		log.Printf("create s3 app: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "could not save app")
		return
	}
	if icon != "" {
		if err := h.db.UpsertAppIcon(r.Context(), pkg, icon); err != nil {
			log.Printf("upsert app icon: %v", err)
		}
	}
	// Warm the local cache in the background so device installs serve from LAN disk
	// (S3 here is too slow to stream a large APK within the device's download timeout).
	go func(key string) {
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("apk cache warm %s: %v", key, err)
		}
	}(req.Key)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// AppAPK 302-redirects to a fresh presigned S3 download URL for the app's object, so
// the bucket stays private and the stored apk_url never expires. Open: the device's
// package installer fetches it by plain URL with no dashboard session.
func (h *Handler) AppAPK(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	app, err := h.db.GetApp(r.Context(), id)
	if err != nil || app.S3Key == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+app.PackageName+".apk\"")
	// Prefer the local cache: serving from disk is LAN-fast and http.ServeFile supports
	// Range/resume, so a device that lost its connection can continue where it left off.
	// (S3 here is ~0.2 MB/s, far too slow to stream a large APK within the device's
	// download timeout — that caused the endless progress reset.)
	if path, ok := h.apk.CachedPath(app.S3Key); ok {
		http.ServeFile(w, r, path)
		return
	}
	// Not cached yet: kick off a background fill so future installs are fast, and stream
	// from S3 this time as a fallback — honoring the client's Range header so a dropped
	// slow transfer RESUMES instead of restarting (the device sends Range on retry; if we
	// ignored it and returned 200, the client would delete its partial and start over).
	go func(key string) {
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("apk cache %s: %v", key, err)
		}
	}(app.S3Key)
	body, size, ct, contentRange, err := h.apk.Get(r.Context(), app.S3Key, r.Header.Get("Range"))
	if err != nil {
		log.Printf("apk get %s: %v", app.S3Key, err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if contentRange != "" { // S3 returned a partial → mirror it as 206
		w.Header().Set("Content-Range", contentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	io.Copy(w, body)
}

// SetupUpdateApp edits an existing repository app (name, APK URL, package name).
func (h *Handler) SetupUpdateApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if _, err := h.db.UpdateApp(r.Context(), id, name, apkURL, pkg); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup", http.StatusFound)
}

func (h *Handler) SetupDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteApp(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.DeleteEmptyAppFamilies(r.Context())
	h.audit(r, "apps.delete", id.String(), "")
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Back to the library the user was on (/apps or Settings), never the old /setup.
	if ref := r.Header.Get("Referer"); strings.Contains(ref, "/settings") {
		http.Redirect(w, r, "/settings#applibrary", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (h *Handler) SettingsPage(w http.ResponseWriter, r *http.Request) {
	setFams, setSugg, setMode := h.libraryData(r.Context())
	dbStats, _ := h.db.TableStats(r.Context())
	aiTotals, _ := h.db.GetAIUsageTotals(r.Context())
	aiDaily, _ := h.db.GetAIUsageDaily(r.Context(), 30)
	if aiDaily == nil {
		aiDaily = []db.AIUsageDay{}
	}
	aiDailyJSON, _ := json.Marshal(aiDaily)
	fleetWindow, groupWindows := h.buildServiceWindowViews(r.Context())
	channels, _ := h.db.ListAlertChannels(r.Context(), false)
	baseCases, _ := h.db.ListBaseTestCases(r.Context(), false)
	// Kiosk allowlist picker: only apps that show in a launcher (DPC agents
	// report the launchable flag; older clients fall back to "user app").
	allPkgs, _ := h.db.ListPackagesAdmin(r.Context(), "")
	kioskFleetApps := allPkgs[:0:0]
	for _, p := range allPkgs {
		if p.InAppDrawer() {
			kioskFleetApps = append(kioskFleetApps, p)
		}
	}
	googleUsage := h.buildGoogleUsage(r.Context())
	googleUsageJSON, _ := json.Marshal(googleUsage)
	learnedAPs, _ := h.db.ListWifiAPsRecent(r.Context(), 25)
	deviceQueries, _ := h.db.ListDeviceQueries(r.Context())
	repoApps, _ := h.db.ListApps(r.Context())
	productions, _ := h.db.ListProductions(r.Context(), h.connectedSlice())
	h.render(w, r, "settings.html", map[string]any{
		"Title":                "Settings",
		"AgentAPKURL":          h.cfg.AgentAPKURL(),
		"AgentAPKChecksum":     h.cfg.AgentAPKChecksum(),
		"AgentAPKHosted":       h.cfg.AgentAPKHosted(),
		"AgentAPKHostedInfo": func() map[string]any {
			sha, name, size, at := h.cfg.AgentAPKHostedInfo()
			return map[string]any{"SHA": sha, "Name": name, "Size": size, "At": at, "URL": h.agentAPKURL(r)}
		}(),
		"AgentAPKFromEnv":      h.cfg.AgentAPKURLVal == "" && os.Getenv("AGENT_APK_URL") != "",
		"Apps":                 repoApps,
		"Families":             setFams,
		"Suggestions":          setSugg,
		"AppFamilyMode":        setMode,
		"Productions":          productions,
		"DeviceQueries":        deviceQueries,
		"BaseCases":            baseCases,
		"ExtraColumns":         h.cfg.Columns(),
		"LegacyCheckin":        h.cfg.LegacyCheckin(),
		"LegacyBuilds":         h.cfg.LegacyBuilds(),
		"WlcProducts":          h.cfg.WlcProducts(),
		"CheckinInterval":      h.cfg.CheckinInterval(),
		"ShellEnabled":         h.cfg.ShellEnabled(),
		"RemoteEnabled":        h.cfg.RemoteEnabled(),
		"CommandExpiry":        h.cfg.CommandExpiry(),
		"MaxTargets":           h.cfg.MaxTargets(),
		"RequireReason":        h.cfg.RequireReason(),
		"SessionTimeout":       h.cfg.SessionTimeout(),
		"BrandName":            h.cfg.BrandName(),
		"PageSize":             h.cfg.PageSize(),
		"DefaultSort":          h.cfg.DefaultSort(),
		"Density":              h.cfg.Density(),
		"Use24Hour":            h.cfg.Use24Hour(),
		"AlertWebhookSet":      h.cfg.AlertWebhookURL() != "",
		"AlertRules":           h.buildAlertRuleViews(r.Context()),
		"FleetWindow":          fleetWindow,
		"GroupWindows":         groupWindows,
		"AlertChannels":        channels,
		"ChannelTest":          r.URL.Query().Get("channel_test"),
		"AIKeySet":             h.cfg.AIEnabled(),
		"AIProvider":           h.cfg.AIProvider(),
		"AnthropicModel":       h.cfg.AnthropicModel(),
		"AIBaseURL":            h.cfg.AIBaseURL(),
		"AIDigestEnabled":      h.cfg.AIDigestEnabled(),
		"AIUsage":              aiTotals,
		"AIUsageTotal":         aiTotals.InputTokens + aiTotals.OutputTokens,
		"AIUsageDailyJSON":     template.JS(aiDailyJSON),
		"AutoHideDays":         h.cfg.AutoHideDays(),
		"CheckinRetentionDays": h.cfg.CheckinRetentionDays(),
		"LogcatRetentionDays":  h.cfg.LogcatRetentionDays(),
		"CheckinSampleSec":     h.cfg.CheckinSampleSec(),
		"LegacyStripCursor":    h.cfg.LegacyStripCursor(),
		"LegacyStripPct":       h.legacyStripPct(r.Context()),
		"MaintenanceMode":      h.cfg.MaintenanceMode(),
		"IgnoreDPCCheckins":    h.cfg.IgnoreDPCCheckins(),
		"DBStats":              dbStats,
		"KioskAllowlist":       strings.Join(h.cfg.KioskAllowlist(), "\n"),
		"OTACutoffRows":        h.otaCutoffRows(r.Context()),
		"OTABuildRows":         h.otaBuildRows(r.Context()),
		"MDMServers":           h.cfg.MDMServers(),
		"LegacyOTAMode":        h.cfg.LegacyOTAMode(),
		"LegacyOTAPort":        os.Getenv("LEGACY_OTA_PORT"),
		"LegacyOTAUpstream":    os.Getenv("LEGACY_OTA_UPSTREAM"),
		"OTAConfigManaged":     h.cfg.OTAConfigManaged(),
		"OTAConfigOpts":        h.cfg.OTAConfigOptions(),
		"OTAConfigLast":        otaConfigLastView(h.cfg),
		"OTAConfigNext":        otaConfigNextView(h.cfg),
		"OTAConfigS3":          h.apk != nil,
		"KioskFleetApps":       kioskFleetApps,
		"GoogleUsage":          googleUsage,
		"GoogleUsageJSON":      template.JS(googleUsageJSON),
		"LearnedAPs":           learnedAPs,
	})
}

// ── Google API usage panel ──────────────────────────────────────────────────

// Google API list-price per 1,000 requests (USD). Geolocation and Geocoding are
// billed per request; Maps Embed is free/unlimited. Used only for the estimate
// shown in Settings → Google APIs — Google's own billing is authoritative.
const (
	costPerKGeolocation = 5.0
	costPerKGeocoding   = 5.0
)

// googleAPIStat is one Google API surface's usage, shaped for the settings panel
// and the /settings/google-usage JSON poll.
type googleAPIStat struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Purpose    string   `json:"purpose"`
	Enabled    bool     `json:"enabled"`
	Billable   bool     `json:"billable"`
	Requests   uint64   `json:"requests"` // outbound requests actually sent to Google
	Successes  uint64   `json:"successes"`
	Errors     uint64   `json:"errors"`
	Hits       uint64   `json:"hits"`       // answered from in-memory cache — request avoided
	LocalHits  uint64   `json:"local_hits"` // answered from learned WiFi index — request avoided
	Cooldowns  uint64   `json:"cooldowns"`  // skipped by cooldown — request avoided
	Avoided    uint64   `json:"avoided"`    // hits + local_hits + cooldowns
	HitRatio   int      `json:"hit_ratio"`  // % of lookups served without a request
	Last24h    uint64   `json:"last24h"`
	Hourly     []uint64 `json:"hourly"`
	BarPct     []int    `json:"bar_pct"` // per-hour bar height 0-100 (relative to peak)
	PeakHour   uint64   `json:"peak_hour"`
	LastErr    string   `json:"last_err"`
	LastErrAgo string   `json:"last_err_ago"`
	LastReqAgo string   `json:"last_req_ago"`
	UnitCostK  float64  `json:"unit_cost_k"`  // $ per 1,000 requests
	CostToDate float64  `json:"cost_to_date"` // requests/1000 * unit
	ProjMonth  float64  `json:"proj_month"`   // last24h * 30 / 1000 * unit
}

// learnedIndexView summarizes the DB-persisted learned WiFi-AP index.
type learnedIndexView struct {
	Enabled        bool    `json:"enabled"`         // geolocation resolver is on
	Total          int64   `json:"total"`           // learned APs (all)
	Fresh          int64   `json:"fresh"`           // APs within the freshness window
	DistinctPlaces int64   `json:"distinct_places"` // ~unique points
	ServedLookups  int64   `json:"served_lookups"`  // all-time local lookups served (SUM hits)
	LocalHits      uint64  `json:"local_hits"`      // this-session lookups served from the index
	CostSaved      float64 `json:"cost_saved"`      // est. $ saved this session by local hits
	LastLearnedAgo string  `json:"last_learned_ago"`
}

// googleUsageView aggregates all three Google surfaces for the template + JSON.
type googleUsageView struct {
	AnyEnabled     bool             `json:"any_enabled"`
	SinceUnix      int64            `json:"since_unix"`
	SinceAgo       string           `json:"since_ago"`
	Stats          []googleAPIStat  `json:"stats"`
	Learned        learnedIndexView `json:"learned"`
	TotalCostToDay float64          `json:"total_cost_to_date"`
	TotalProjMonth float64          `json:"total_proj_month"`
}

func statFromMeter(key, name, purpose string, enabled, billable bool, unitK float64, requestsOverride *uint64, m *geolocate.MeterSnapshot) googleAPIStat {
	s := googleAPIStat{Key: key, Name: name, Purpose: purpose, Enabled: enabled, Billable: billable, UnitCostK: unitK, Hourly: make([]uint64, 24)}
	if m != nil {
		s.Requests = m.Requests
		s.Successes = m.Successes
		s.Errors = m.Errors
		s.Hits = m.Hits
		s.LocalHits = m.LocalHits
		s.Cooldowns = m.Cooldowns
		s.Last24h = m.Last24h
		s.Hourly = m.Hourly
		s.LastErr = m.LastErr
		s.LastErrAgo = agoString(m.LastErrAt)
		s.LastReqAgo = agoString(m.LastReqAt)
	}
	if requestsOverride != nil {
		s.Requests = *requestsOverride
		s.Successes = *requestsOverride
	}
	s.Avoided = s.Hits + s.LocalHits + s.Cooldowns
	if lookups := s.Requests + s.Avoided; lookups > 0 {
		s.HitRatio = int(s.Avoided * 100 / lookups)
	}
	for _, v := range s.Hourly {
		if v > s.PeakHour {
			s.PeakHour = v
		}
	}
	s.BarPct = make([]int, len(s.Hourly))
	for i, v := range s.Hourly {
		if s.PeakHour > 0 {
			// Floor non-zero hours at 6% so a single request is still a visible tick.
			p := int(v * 100 / s.PeakHour)
			if v > 0 && p < 6 {
				p = 6
			}
			s.BarPct[i] = p
		}
	}
	if billable {
		s.CostToDate = float64(s.Requests) / 1000 * unitK
		s.ProjMonth = float64(s.Last24h) * 30 / 1000 * unitK
	}
	return s
}

// buildGoogleUsage snapshots all three Google API meters into a render-ready view.
func (h *Handler) buildGoogleUsage(ctx context.Context) googleUsageView {
	v := googleUsageView{SinceUnix: h.startedAt.Unix(), SinceAgo: agoString(h.startedAt)}

	var geoSnap, geoc *geolocate.MeterSnapshot
	if h.geo != nil {
		s := h.geo.Stats()
		geoSnap = &s
	}
	if h.geocoder != nil {
		s := h.geocoder.Stats()
		geoc = &s
	}
	mapViews := h.mapViews.Load()

	v.Stats = []googleAPIStat{
		statFromMeter("geolocation", "Geolocation API", "WiFi scan → coordinates",
			h.geo != nil, true, costPerKGeolocation, nil, geoSnap),
		statFromMeter("geocoding", "Geocoding API", "Coordinates → street address",
			h.geocoder != nil, true, costPerKGeocoding, nil, geoc),
		statFromMeter("embed", "Maps Embed API", "Device-page location map (free)",
			h.mapsEmbedKey != "", false, 0, &mapViews, nil),
	}
	for _, s := range v.Stats {
		if s.Enabled {
			v.AnyEnabled = true
		}
		v.TotalCostToDay += s.CostToDate
		v.TotalProjMonth += s.ProjMonth
	}

	// Learned WiFi-AP index summary (DB-persisted). local_hits are Google calls the
	// index avoided this session; value them at the Geolocation unit price.
	v.Learned.Enabled = h.geo != nil
	if geoSnap != nil {
		v.Learned.LocalHits = geoSnap.LocalHits
		v.Learned.CostSaved = float64(geoSnap.LocalHits) / 1000 * costPerKGeolocation
	}
	if st, err := h.db.GetWifiAPStats(ctx, geolocate.LearnedFreshWindow); err == nil {
		v.Learned.Total = st.Total
		v.Learned.Fresh = st.Fresh
		v.Learned.DistinctPlaces = st.DistinctPlaces
		v.Learned.ServedLookups = st.ServedLookups
		v.Learned.LastLearnedAgo = agoString(st.LastLearnedAt)
	}
	return v
}

// GoogleUsageJSON serves the live Google API usage snapshot for the settings
// panel's auto-refresh poll. Admin-only.
func (h *Handler) GoogleUsageJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(h.buildGoogleUsage(r.Context()))
}

// agoString renders a compact "3m ago" / "2h ago" / "5d ago" for a timestamp, or
// "never" for the zero time.
func agoString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// extraHasCoords reports whether a device's latest extra payload carries a
// resolved latitude/longitude (so the device page will render the Maps Embed).
func extraHasCoords(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	lat, okLat := m["latitude"]
	lon, okLon := m["longitude"]
	return okLat && okLon && lat != nil && lon != nil
}

// settingsToggleResponse renders the on/off switch back in place for htmx (so the
// setting flips without a full-page reload), or redirects to /settings without JS.
func (h *Handler) settingsToggleResponse(w http.ResponseWriter, r *http.Request, action string, on bool) {
	if hxReq(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.tmpl.ExecuteTemplate(w, "settings-toggle", map[string]any{"Action": action, "On": on})
		return
	}
	h.settingsRedirect(w, r)
}

// SettingsToggleIgnoreDPC switches DPC agent support off (and back on). While it
// is on, check-ins and WS telemetry from DPC agents are answered normally and
// stored nowhere. Enrolled DPC devices keep their history and stop updating.
func (h *Handler) SettingsToggleIgnoreDPC(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg.SetIgnoreDPCCheckins(!h.cfg.IgnoreDPCCheckins())
	h.audit(r, "settings.ignore_dpc_checkins", "", fmt.Sprintf("%t", h.cfg.IgnoreDPCCheckins()))
	h.settingsToggleResponse(w, r, "/settings/ignore-dpc/toggle", h.cfg.IgnoreDPCCheckins())
}

func (h *Handler) SettingsToggleLegacyCheckin(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetLegacyCheckin(!h.cfg.LegacyCheckin())
	h.settingsToggleResponse(w, r, "/settings/legacy-checkin/toggle", h.cfg.LegacyCheckin())
}

// DemoPage serves a self-contained devices-page UI/UX exploration from
// templates/demo/<n>.html. Gated behind requireAuth so these design mockups are
// never exposed unauthenticated (unlike the removed static/preview.html). These
// are throwaway design demos with synthetic data, not wired to the real fleet.
func (h *Handler) DemoPage(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("n") {
	case "5", "8", "main1", "main2", "merge1", "merge2", "merge3",
		"index", "report-flagship", "report-revamp", "report-redesign", "report-live", "report-gallery", "fleet-health",
		"actions", "actions-launchpad", "actions-palette", "actions-flightdeck",
		"actions-v2-flow", "actions-v2-console", "actions-v2-palette",
		"actions-stepper", "actions-accordion", "actions-drawer",
		"actions-pick-pills", "actions-pick-spotlight", "actions-pick-toolbar",
		"actions-panes-studio", "actions-panes-console", "actions-panes-guided",
		"actions-target", "actions-target-rail", "actions-target-audience", "actions-target-split",
		"actions-install-picker", "actions-kiosk-config", "manage-table", "manage-kiosk-dialog", "manage-policy-model",
		"action-types", "action-rollout", "action-cockpit",
		"export", "export-builder", "export-compact", "timesel",
		"health-pulse", "health-triage", "health-grid",
		"health-command", "health-reliability", "health-stream", "health-icons",
		"overview-command", "overview-redesign",
		"alerts-inbox", "alerts-grouped", "notifications", "toasts", "liquid-glass",
		"release-pipeline", "release-cockpit", "ota-flow", "history-hierarchy", "owner-home", "action-detail":
	default:
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile("templates/demo/" + r.PathValue("n") + ".html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// InternalPage serves admin/dev-only presenter material — the system audit and the
// demo runbook — from templates/demo/<n>.html. Kept separate from DemoPage so these
// candid, internal-facing documents sit behind requireAdmin rather than requireAuth,
// and so they're never exposed to a viewer/operator/operator or an external guest login.
func (h *Handler) InternalPage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/demo/")
	switch name {
	case "audit", "runbook":
	default:
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile("templates/demo/" + name + ".html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (h *Handler) SettingsSetCommandExpiry(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sec := 300
	if n, err := strconv.Atoi(r.FormValue("expiry")); err == nil && n >= 30 {
		sec = n
	}
	h.cfg.SetCommandExpiry(sec)
	// Redirect (not 204): the value may have been clamped to a default, so re-render
	// the form to show the actually-stored value rather than the rejected input.
	h.hxRedirect(w, r, h.settingsDest(r))
}

func (h *Handler) SettingsSetMaxTargets(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	n := 0
	if v, err := strconv.Atoi(r.FormValue("max_targets")); err == nil && v >= 0 {
		n = v
	}
	h.cfg.SetMaxTargets(n)
	h.hxRedirect(w, r, h.settingsDest(r)) // may clamp to 0/default — re-render the stored value
}

// ── Device diagnostics catalog ───────────────────────────────────────────────
// Admins curate a set of read-only device queries (label + shell command). They're
// surfaced on the device page as "retrieve property" buttons; because the command
// text comes only from this catalog (never user input), operators may run them even
// though they cannot send raw shell.

// deviceQueryFromForm reads the shared create/edit form fields into a DeviceQuery.
func deviceQueryFromForm(r *http.Request) db.DeviceQuery {
	q := db.DeviceQuery{
		Label:       strings.TrimSpace(r.FormValue("label")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Command:     strings.TrimSpace(r.FormValue("command")),
		Category:    strings.TrimSpace(r.FormValue("category")),
		Enabled:     r.FormValue("enabled") == "on",
	}
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("sort"))); err == nil {
		q.Sort = n
	}
	if q.Category == "" {
		q.Category = "General"
	}
	return q
}

func (h *Handler) SettingsQueryCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := deviceQueryFromForm(r)
	q.CreatedBy = h.currentUsername(r)
	if q.Label == "" || q.Command == "" {
		http.Error(w, "A label and a command are required.", http.StatusBadRequest)
		return
	}
	if _, err := h.db.CreateDeviceQuery(r.Context(), q); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "query.create", q.Label, q.Command)
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsQueryEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	q := deviceQueryFromForm(r)
	q.ID = id
	if q.Label == "" || q.Command == "" {
		http.Error(w, "A label and a command are required.", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateDeviceQuery(r.Context(), q); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "query.edit", q.Label, q.Command)
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsQueryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.db.DeleteDeviceQuery(r.Context(), id)
	h.audit(r, "query.delete", strconv.Itoa(id), "")
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsQueryToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.db.SetDeviceQueryEnabled(r.Context(), id, r.FormValue("enabled") == "1")
	h.settingsRedirect(w, r)
}

// RunRecentAlerts evaluates the recent-tier rules (point-in-time + rate/sustained T7
// matrix rules) and dispatches any new alerts. Called every minute from main.go so
// 5-minute-offline / SoC-now / discharge-rate alerts fire promptly, not hourly.
func (h *Handler) RunRecentAlerts(ctx context.Context) {
	// The offline-family rules must not page a device that still has a live WebSocket,
	// so pass the currently-connected set (see offlineHitsQuery / the alerts-offline
	// design note).
	connSet := h.hub.ConnectedIDs()
	connected := make([]uuid.UUID, 0, len(connSet))
	for id := range connSet {
		connected = append(connected, id)
	}
	created, resolved, err := h.db.EvaluateRecentAlerts(ctx, connected)
	if err != nil {
		log.Printf("[recent-alerts] evaluate: %v", err)
		return
	}
	if len(created) > 0 || resolved > 0 {
		log.Printf("[recent-alerts] %d new, %d resolved", len(created), resolved)
		h.hub.PublishAlertUpdate()
	}
	// A crash/ANR alert carries the real diagnostic — the DropBox stack/ANR/tombstone
	// trace the client now sends with the crash event (see LatestCrashTrace, rendered
	// on the alert). We deliberately do NOT auto-capture logcat here: a delayed *:E grab
	// fires a minute+ after the crash and returns ambient system noise, not the crash.
	h.dispatchAlertNotifications(ctx, created)
}

// dispatchAlertNotifications routes freshly-created alerts to the configured
// channels. The routing logic is shared with the device API (new-device alerts)
// via internal/alerts.
func (h *Handler) dispatchAlertNotifications(ctx context.Context, created []db.AlertNotification) {
	h.alerts.Dispatch(ctx, created)
}

// RunHousekeeping applies the configured auto-hide and retention policies.
// Safe to call repeatedly; each step is a no-op when its setting is 0.
// inactiveAfterDays is how long a device may go silent before it is auto-marked
// inactive (hidden) and dropped from every list, count, and health stat. It comes
// back automatically the moment it checks in again.
const inactiveAfterDays = 100

func (h *Handler) RunHousekeeping(ctx context.Context) {
	// Roll up daily stats first — refresh today and finalize yesterday — so checkins
	// are always aggregated before the retention prune below can delete them.
	now := time.Now().UTC() // roll by UTC day (matches the DB session tz) for a stable boundary
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		if _, err := h.db.RollupDailyStats(ctx, day); err != nil {
			log.Printf("[housekeeping] rollup daily stats %s: %v", day.Format("2006-01-02"), err)
		}
	}
	// Recompute battery discharge cycles from the freshly rolled-up daily swings.
	if n, err := h.db.RecomputeDischargeCycles(ctx); err != nil {
		log.Printf("[housekeeping] recompute discharge cycles: %v", err)
	} else if n > 0 {
		log.Printf("[housekeeping] recomputed discharge cycles for %d device(s)", n)
	}
	// Evaluate daily-tier alert rules against the freshly rolled-up stats, then notify.
	if created, resolved, err := h.db.EvaluateAlerts(ctx, h.connectedSlice()); err != nil {
		log.Printf("[housekeeping] evaluate alerts: %v", err)
	} else {
		if len(created) > 0 || resolved > 0 {
			log.Printf("[housekeeping] alerts: %d new, %d resolved", len(created), resolved)
			h.hub.PublishAlertUpdate()
		}
		h.dispatchAlertNotifications(ctx, created)
	}
	// Auto-inactivate devices that have been silent for over inactiveAfterDays: they
	// stop appearing in every list, count, and health stat so long-dead units don't
	// skew the fleet. They return automatically on their next check-in (UpsertCheckin
	// clears hidden). Runs before the summary refresh so the cached counts drop them.
	if n, err := h.db.HideStaleDevices(ctx, inactiveAfterDays); err != nil {
		log.Printf("[housekeeping] auto-inactivate stale devices: %v", err)
	} else if n > 0 {
		log.Printf("[housekeeping] marked %d device(s) inactive (silent > %dd)", n, inactiveAfterDays)
		h.hub.PublishAlertUpdate() // nudge the dashboard's live counts to refresh
	}
	// Clear out any alerts still open on now-inactive devices.
	if n, err := h.db.ResolveAlertsForHiddenDevices(ctx); err != nil {
		log.Printf("[housekeeping] resolve inactive-device alerts: %v", err)
	} else if n > 0 {
		log.Printf("[housekeeping] resolved %d alert(s) on inactive devices", n)
		h.hub.PublishAlertUpdate()
	}
	h.refreshFleetSummary(ctx)
	h.maybeSendDigest(ctx)
	h.applyPrunes(ctx)
	h.stripLegacyCheckins(ctx)
	h.backfillBuildHistory(ctx)
}

// stripLegacyCheckins walks the check-in history one UTC day at a time, oldest
// first, rewriting rows that still carry the keys UpsertCheckin now strips at
// insert (crash_events, wifi_scan). Those two keys were ~80% of the production
// table. Bounded per run (time budget + day cap) so it never competes with real
// work for long on a small box; progress persists in config so it resumes across
// restarts and finishes on its own. Space is reclaimed by autovacuum for reuse;
// returning it to the OS needs a one-off pg_repack / VACUUM FULL afterwards.
// backfillBuildHistory fills device_build_history from check-in history, one UTC
// day per step, newest first — so the device graph's recent build markers are
// right after the first run and older history fills in over the following hours.
// Bounded per run; the cursor persists in config; stops at the oldest check-in.
func (h *Handler) backfillBuildHistory(ctx context.Context) {
	cur := h.cfg.BuildHistoryCursor()
	if cur == "done" {
		return
	}
	oldest, ok, err := h.db.OldestCheckinDay(ctx)
	if err != nil {
		log.Printf("[build-history] oldest checkin: %v", err)
		return
	}
	if !ok {
		_ = h.cfg.SetBuildHistoryCursor("done")
		return
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if cur != "" {
		if day, err = time.Parse("2006-01-02", cur); err != nil {
			log.Printf("[build-history] bad cursor %q, restarting", cur)
			_ = h.cfg.SetBuildHistoryCursor("")
			return
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	var total int64
	for steps := 0; steps < 20 && !day.Before(oldest) && time.Now().Before(deadline); steps++ {
		n, err := h.db.BackfillBuildHistoryDay(ctx, day)
		if err != nil {
			log.Printf("[build-history] %s: %v", day.Format("2006-01-02"), err)
			return
		}
		total += n
		day = day.AddDate(0, 0, -1)
		if err := h.cfg.SetBuildHistoryCursor(day.Format("2006-01-02")); err != nil {
			log.Printf("[build-history] save cursor: %v", err)
			return
		}
	}
	if day.Before(oldest) {
		_ = h.cfg.SetBuildHistoryCursor("done")
		log.Printf("[build-history] backfill finished (%d change(s) this run)", total)
		return
	}
	log.Printf("[build-history] %d change(s) this run; next day %s", total, day.Format("2006-01-02"))
}

func (h *Handler) stripLegacyCheckins(ctx context.Context) {
	cur := h.cfg.LegacyStripCursor()
	if cur == "done" {
		return
	}
	var day time.Time
	if cur == "" {
		oldest, ok, err := h.db.OldestCheckinDay(ctx)
		if err != nil {
			log.Printf("[legacy-strip] oldest checkin: %v", err)
			return
		}
		if !ok {
			_ = h.cfg.SetLegacyStripCursor("done")
			return
		}
		day = oldest
		_ = h.cfg.SetLegacyStripStart(day.Format("2006-01-02"))
	} else {
		var err error
		if day, err = time.Parse("2006-01-02", cur); err != nil {
			log.Printf("[legacy-strip] bad cursor %q, restarting", cur)
			_ = h.cfg.SetLegacyStripCursor("")
			return
		}
	}
	// Stop at yesterday: today's rows are already written stripped, and yesterday's
	// may still be in flight across the UTC boundary — they get picked up next run.
	//
	// Pacing: small batches with a pause between them and a modest per-run budget.
	// The rows being rewritten carry tens of KB each, so throughput here is I/O the
	// dashboard and device traffic need too. Hourly runs finish the history in days,
	// which is fine — this is a one-off.
	const batch, maxRows = 1000, 20000
	stop := time.Now().UTC().Truncate(24 * time.Hour)
	deadline := time.Now().Add(3 * time.Minute)
	var total int64
	for day.Before(stop) && total < maxRows && time.Now().Before(deadline) {
		n, err := h.db.StripLegacyCheckinKeys(ctx, day, batch)
		if err != nil {
			log.Printf("[legacy-strip] %s: %v", day.Format("2006-01-02"), err)
			return
		}
		total += n
		if n < int64(batch) {
			// Day is clean — advance the cursor.
			day = day.AddDate(0, 0, 1)
			if err := h.cfg.SetLegacyStripCursor(day.Format("2006-01-02")); err != nil {
				log.Printf("[legacy-strip] save cursor: %v", err)
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	if !day.Before(stop) {
		_ = h.cfg.SetLegacyStripCursor("done")
		log.Printf("[legacy-strip] finished (%d row(s) rewritten this run)", total)
		return
	}
	if total > 0 {
		log.Printf("[legacy-strip] rewrote %d row(s); cursor at %s", total, day.Format("2006-01-02"))
	}
}

// legacyStripPct is the cleanup's progress, 0–100, for the Settings page: days done
// between the start day and today. The start day is recorded when the job begins;
// for a run that started before that field existed it is looked up once (indexed
// MIN(created_at)) and saved.
func (h *Handler) legacyStripPct(ctx context.Context) int {
	cur := h.cfg.LegacyStripCursor()
	if cur == "done" {
		return 100
	}
	if cur == "" {
		return 0
	}
	curDay, err := time.Parse("2006-01-02", cur)
	if err != nil {
		return 0
	}
	start := h.cfg.LegacyStripStart()
	if start == "" {
		if oldest, ok, err := h.db.OldestCheckinDay(ctx); err == nil && ok {
			start = oldest.Format("2006-01-02")
			_ = h.cfg.SetLegacyStripStart(start)
		}
	}
	startDay, err := time.Parse("2006-01-02", start)
	if err != nil {
		return 0
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	total := today.Sub(startDay).Hours() / 24
	if total <= 0 {
		return 100
	}
	pct := int(100 * curDay.Sub(startDay).Hours() / 24 / total)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// applyPrunes deletes check-ins and logcat results past their retention windows
// (each a no-op when disabled). These are destructive — rows are removed.
func (h *Handler) applyPrunes(ctx context.Context) {
	if d := h.cfg.CheckinRetentionDays(); d > 0 {
		if n, err := h.db.PruneCheckins(ctx, d); err != nil {
			log.Printf("[retention] prune checkins: %v", err)
		} else if n > 0 {
			log.Printf("[retention] pruned %d checkin(s) older than %dd", n, d)
		}
	}
	if d := h.cfg.LogcatRetentionDays(); d > 0 {
		if n, err := h.db.PruneLogcat(ctx, d); err != nil {
			log.Printf("[retention] prune logcat: %v", err)
		} else if n > 0 {
			log.Printf("[retention] pruned %d logcat row(s) older than %dd", n, d)
		}
	}
	if n, err := h.db.PruneResolvedAlerts(ctx, resolvedAlertRetentionDays); err != nil {
		log.Printf("[retention] prune resolved alerts: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d resolved alert(s) older than %dd", n, resolvedAlertRetentionDays)
	}
	if err := h.db.DeleteExpiredSessions(ctx); err != nil {
		log.Printf("[retention] prune sessions: %v", err)
	}
}

// resolvedAlertRetentionDays is how long resolved alerts are kept before the
// housekeeping prune deletes them (open/acknowledged alerts are never pruned).
const resolvedAlertRetentionDays = 90

// refreshFleetSummary regenerates the cached fleet AI summary shown on the main
// page, at most hourly. It's a no-op unless AI is configured, and it skips when the
// cached summary is still fresh (< 55 min) so frequent housekeeping runs or restarts
// don't trigger extra API calls.
func (h *Handler) refreshFleetSummary(ctx context.Context) {
	if !h.cfg.AIEnabled() {
		return
	}
	if cur, err := h.db.GetAISummary(ctx, "fleet"); err == nil && cur.Summary != "" &&
		!cur.GeneratedAt.IsZero() && time.Since(cur.GeneratedAt) < 55*time.Minute {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := h.generateFleetSummary(cctx); err != nil {
		log.Printf("[ai-summary] %v", err)
	} else {
		log.Printf("[ai-summary] refreshed fleet summary")
	}
}

// generateFleetSummary runs the fleet analysis (health + open alerts), records token
// usage, caches the result, and returns it. Always generates — callers gate freshness.
func (h *Handler) generateFleetSummary(ctx context.Context) (db.AISummary, error) {
	groups, err := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 1) // Daily Report: one day
	if err != nil {
		return db.AISummary{}, err
	}
	summary, _ := h.db.GetSummary(ctx, h.connectedSlice())
	openAlerts, _ := h.db.CountOpenAlerts(ctx)
	alerts, _ := h.db.ListAlerts(ctx, "open", 40)

	deployed, lab, _ := h.db.DeploymentCounts(ctx)
	client := ai.New(h.cfg.AIProvider(), h.cfg.AnthropicAPIKey(), h.cfg.AnthropicModel(), h.cfg.AIBaseURL())
	text, usage, err := client.AnalyzeFleet(ctx, groups, summary.Total, summary.RecentlyActive, openAlerts, deployed, lab, alerts, h.alertThresholds(ctx))
	if err != nil {
		return db.AISummary{}, err
	}
	if err := h.db.RecordAIUsage(ctx, usage.InputTokens, usage.OutputTokens); err != nil {
		log.Printf("[ai-summary] record usage: %v", err)
	}
	// Store the report as canonical JSON: the model sometimes wraps it in stray
	// characters, which would otherwise render as raw text in the card.
	if rep, ok := ai.ParseReport(text); ok {
		if c := rep.Canonical(); c != "" {
			text = c
		}
	}
	if err := h.db.SetAISummary(ctx, "fleet", text, client.Model()); err != nil {
		return db.AISummary{}, err
	}
	return db.AISummary{Summary: text, Model: client.Model(), GeneratedAt: time.Now()}, nil
}

// AISummaryRefresh forces a fresh fleet summary (the main-page card's refresh button)
// and returns the new text + timestamp.
func (h *Handler) AISummaryRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.AIEnabled() {
		writeJSONError(w, http.StatusServiceUnavailable, "AI analysis is not configured — add an API key in Settings.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	s, err := h.generateFleetSummary(ctx)
	if err != nil {
		log.Printf("[ai-summary] refresh: %v", err)
		writeJSONError(w, http.StatusBadGateway, "Refresh failed: "+err.Error())
		return
	}
	h.audit(r, "ai.summary_refresh", "", "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"text":         s.Summary,
		"generated_at": s.GeneratedAt.UTC().Format(time.RFC3339),
	})
}

// maybeSendDigest posts a once-a-day AI fleet summary to the alert webhook. It is
// a no-op unless AI analysis + the digest toggle + a webhook are all configured. It
// fires on the first housekeeping run at or after 08:00 local each day, so the
// summary reflects daytime service rather than overnight charging.
func (h *Handler) maybeSendDigest(ctx context.Context) {
	if !h.cfg.AIDigestEnabled() {
		return
	}
	url := h.cfg.AlertWebhookURL()
	if url == "" {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	if h.lastDigestDay == today || now.Hour() < 8 {
		return
	}
	groups, err := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 1) // Daily Report: one day
	if err != nil {
		log.Printf("[digest] group health: %v", err)
		return
	}
	summary, _ := h.db.GetSummary(ctx, h.connectedSlice())
	openAlerts, _ := h.db.CountOpenAlerts(ctx)
	alerts, _ := h.db.ListAlerts(ctx, "open", 40)

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	deployed, lab, _ := h.db.DeploymentCounts(cctx)
	// Deterministic "fleet health, explained" is the floor: it always goes out.
	// The AI narrative is layered on top when it is configured and answers.
	fleetScore, den := 0, 0
	for _, g := range groups {
		fleetScore += g.Score * g.DeviceCount
		den += g.DeviceCount
	}
	if den > 0 {
		fleetScore /= den
	} else {
		fleetScore = 100
	}
	crashStats, _ := h.db.GetFleetCrashStats(cctx, 4)
	msg := healthExplainText(fleetScore, 0, explainFleetHealth(groups, crashStats.ByRestaurant))
	if h.cfg.AIEnabled() {
		client := ai.New(h.cfg.AIProvider(), h.cfg.AnthropicAPIKey(), h.cfg.AnthropicModel(), h.cfg.AIBaseURL())
		text, usage, err := client.AnalyzeFleet(cctx, groups, summary.Total, summary.RecentlyActive, openAlerts, deployed, lab, alerts, h.alertThresholds(cctx))
		if err != nil {
			log.Printf("[digest] analyze: %v (sending the explained score instead)", err)
		} else {
			if err := h.db.RecordAIUsage(cctx, usage.InputTokens, usage.OutputTokens); err != nil {
				log.Printf("[digest] record usage: %v", err)
			}
			// The report is structured JSON; flatten it to readable text for the webhook.
			narrative := text
			if rep, ok := ai.ParseReport(text); ok {
				narrative = rep.Text()
			}
			msg = narrative + "\n\n" + msg
		}
	}
	if err := notify.SendWebhook(ctx, url, "*Daily fleet digest*\n"+msg); err != nil {
		log.Printf("[digest] webhook: %v", err)
		return
	}
	h.lastDigestDay = today // only on success, so a transient failure retries next hour
	log.Printf("[digest] sent daily fleet digest")
}

func (h *Handler) SettingsSetRetention(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	atoiNonNeg := func(s string) int {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
		return 0
	}
	h.cfg.SetDataLifecycle(
		atoiNonNeg(r.FormValue("auto_hide_days")),
		atoiNonNeg(r.FormValue("checkin_retention_days")),
		atoiNonNeg(r.FormValue("logcat_retention_days")),
		atoiNonNeg(r.FormValue("checkin_sample_sec")),
	)
	h.db.SetCheckinSampleSec(h.cfg.CheckinSampleSec())
	// Prunes can delete many rows, so run them in the background to keep Save snappy.
	go h.applyPrunes(context.Background())
	h.hxRedirect(w, r, h.settingsDest(r)) // retention fields may be normalized — re-render them
}

func (h *Handler) SettingsToggleRequireReason(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetRequireReason(!h.cfg.RequireReason())
	h.settingsToggleResponse(w, r, "/settings/require-reason", h.cfg.RequireReason())
}

func (h *Handler) SettingsSetDashboard(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 && n <= 200 {
		h.cfg.SetPageSize(n)
	}
	if s := r.FormValue("default_sort"); s != "" {
		h.cfg.SetDefaultSort(s)
	}
	if d := r.FormValue("density"); d == "compact" || d == "comfortable" {
		h.cfg.SetDensity(d)
	}
	h.cfg.SetUse24Hour(r.FormValue("time_format") == "24")
	h.settingsRedirect(w, r)
}

// otaCutoffRow is one product's "oldest build with MDM OTA support" setting row.
type otaCutoffRow struct {
	ProductKey   string
	ProductLabel string
	Releases     []db.Release // published releases of this product, newest first
	Current      int          // chosen cutoff release id, 0 = none
}

func (h *Handler) otaCutoffRows(ctx context.Context) []otaCutoffRow {
	rels, _ := h.db.ListReleases(ctx)
	cur := h.cfg.OTAMinRelease()
	byProduct := map[string][]db.Release{}
	var order []string
	for _, rel := range rels {
		if rel.Status != "published" {
			continue
		}
		pk := product.Normalize(rel.Product)
		if _, ok := byProduct[pk]; !ok {
			order = append(order, pk)
		}
		byProduct[pk] = append(byProduct[pk], rel)
	}
	sort.Strings(order)
	var rows []otaCutoffRow
	for _, pk := range order {
		list := byProduct[pk]
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
		rows = append(rows, otaCutoffRow{ProductKey: pk, ProductLabel: product.Label(pk), Releases: list, Current: cur[pk]})
	}
	return rows
}

// otaUnsupportedBuilds returns the release versions of a product that predate
// the configured OTA support cutoff (their firmware has no MDM OTA agent), or
// nil when no cutoff is set for that product.
func (h *Handler) otaUnsupportedBuilds(ctx context.Context, productKey string) map[string]bool {
	out := map[string]bool{}
	for build := range h.otaGate.Unsupported(ctx, productKey) {
		out[build] = true
	}
	return out
}

// filterOTACapable splits device ids into those whose build can take an MDM OTA and
// those that cannot. Ids in, ids out — the push path works in ids.
func (h *Handler) filterOTACapable(ctx context.Context, ids []uuid.UUID) (kept, dropped []uuid.UUID) {
	for _, id := range ids {
		d, err := h.db.GetDeviceByID(ctx, id)
		if err != nil || d == nil {
			kept = append(kept, id) // can't tell: let the existing paths decide
			continue
		}
		if h.otaGate.Device(ctx, *d).OK {
			kept = append(kept, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return kept, dropped
}

// splitOTACapable divides device ids by the transport their build supports.
func (h *Handler) splitOTACapable(ctx context.Context, ids []uuid.UUID) (mdm, legacy []uuid.UUID) {
	for _, id := range ids {
		d, err := h.db.GetDeviceByID(ctx, id)
		if err != nil || d == nil {
			mdm = append(mdm, id) // can't tell: the existing paths decide
			continue
		}
		if h.otaGate.Device(ctx, *d).OK {
			mdm = append(mdm, id)
		} else {
			legacy = append(legacy, id)
		}
	}
	return mdm, legacy
}

// legacyOnlySerials picks the submitted serials that belong to no fleet device but do
// poll the legacy listener — otautil devices, which the unified picker now offers
// alongside the fleet.
func (h *Handler) legacyOnlySerials(r *http.Request, rel *db.Release) []string {
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		return nil
	}
	known, err := h.db.ListLegacyOTADevices(r.Context())
	if err != nil {
		return nil
	}
	legacy := make(map[string]bool, len(known))
	for _, d := range known {
		legacy[d.Serial] = true
	}
	var out []string
	for _, s := range serials {
		if !legacy[s] {
			continue
		}
		if dev, err := h.db.GetDevice(r.Context(), s); err == nil && dev != nil {
			continue // in the fleet: already decided by the gate
		}
		out = append(out, s)
	}
	return out
}

// otaUnsupportedForDevices is the same answer keyed by the builds these devices are
// actually on, so a build nobody tracked as a release is covered too.
func (h *Handler) otaUnsupportedForDevices(ctx context.Context, devices []db.Device) map[string]bool {
	out := map[string]bool{}
	for _, d := range devices {
		if _, seen := out[d.BuildID]; seen {
			continue
		}
		if !h.otaGate.Device(ctx, d).OK {
			out[d.BuildID] = true
		}
	}
	return out
}

// SettingsSetOTAMinRelease stores, per product, the oldest release with MDM OTA
// support (form fields ota_min_<product>, 0 or empty = no cutoff).
// SettingsSetLegacyOTA switches the legacy otautil routes between the MDM and the
// old ota-server (pass-through). Takes effect on the next device poll.
func (h *Handler) SettingsSetLegacyOTA(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("legacy_ota_mode")
	if err := h.cfg.SetLegacyOTAMode(mode); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.legacy_ota_mode", "", h.cfg.LegacyOTAMode())
	if next := r.FormValue("next"); strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	h.settingsRedirect(w, r)
}

// SettingsSetOTAConfig saves the legacy OTA discovery-config settings and publishes the
// file immediately, so an admin sees the result rather than waiting for the hourly job.
func (h *Handler) SettingsSetOTAConfig(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	pollMS, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("poll_interval_ms")))
	leadHours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("lead_hours")))
	if err := h.cfg.SetOTAConfig(
		r.FormValue("managed") == "on",
		r.FormValue("api_base_url"),
		pollMS,
		r.FormValue("timezone"),
		leadHours,
	); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.ota_config", "", "")
	// Publishing on save keeps "what the fleet reads" and "what this page shows" in step.
	h.PublishOTAConfig(r.Context())
	h.hxDoneToast(w, r, h.settingsDest(r), "OTA discovery config saved", "success")
}

func (h *Handler) SettingsSetOTAMinRelease(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	m := map[string]int{}
	for k, v := range r.Form {
		if !strings.HasPrefix(k, "ota_min_") || len(v) == 0 {
			continue
		}
		if id, err := strconv.Atoi(strings.TrimSpace(v[0])); err == nil && id > 0 {
			m[strings.TrimPrefix(k, "ota_min_")] = id
		}
	}
	if err := h.cfg.SetOTAMinRelease(m); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.otaGate.Invalidate()
	h.audit(r, "settings.ota_min_release", "", fmt.Sprint(m))
	h.settingsRedirect(w, r)
}

// SettingsSetOTASupport records an explicit MDM OTA answer for one build, or clears
// it so the build falls back to the product cutoff.
func (h *Handler) SettingsSetOTASupport(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	build := strings.TrimSpace(r.FormValue("build_id"))
	pk := product.Normalize(r.FormValue("product"))
	if build == "" {
		h.settingsRedirect(w, r)
		return
	}
	switch r.FormValue("value") {
	case "yes":
		_ = h.db.SetOTASupport(r.Context(), build, pk, true, "manual", "")
	case "no":
		_ = h.db.SetOTASupport(r.Context(), build, pk, false, "manual", "no MDM OTA on this build — legacy OTA only")
	default:
		_ = h.db.DeleteOTASupport(r.Context(), build, pk)
	}
	h.otaGate.Invalidate()
	h.audit(r, "settings.ota_support", build, r.FormValue("value"))
	h.settingsRedirect(w, r)
}

// otaBuildRows lists every build the fleet is actually running with the gate's
// verdict for it, so the cutoff can be checked against reality rather than trusted.
type otaBuildRow struct {
	BuildID      string
	ProductKey   string
	ProductLabel string
	Devices      int
	OK           bool
	Reason       string
	Source       string
	Tracked      bool
}

func (h *Handler) otaBuildRows(ctx context.Context) []otaBuildRow {
	fleet, _ := h.db.GetFleetVersions(ctx)
	releases, _ := h.db.ListReleases(ctx)
	tracked := map[string]bool{}
	for _, rel := range releases {
		tracked[product.Normalize(rel.Product)+"|"+rel.Version] = true
	}
	rows := make([]otaBuildRow, 0, len(fleet))
	for _, fv := range fleet {
		pk := product.Normalize(fv.Product)
		v := h.otaGate.Build(ctx, fv.Version, pk)
		rows = append(rows, otaBuildRow{
			BuildID: fv.Version, ProductKey: pk, ProductLabel: product.Label(pk),
			Devices: fv.DeviceCount, OK: v.OK, Reason: v.Reason, Source: v.Source,
			Tracked: tracked[pk+"|"+fv.Version],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Devices > rows[j].Devices })
	return rows
}

// SettingsSetKioskAllowlist replaces the kiosk locked-app allowlist from a free-form
// field (patterns separated by newlines, commas, or spaces). Empty = allow all.
func (h *Handler) SettingsSetKioskAllowlist(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	seen := map[string]bool{}
	var pats []string
	for _, tok := range strings.FieldsFunc(r.FormValue("allowlist"), func(c rune) bool {
		return c == '\n' || c == '\r' || c == ',' || c == ' ' || c == '\t' || c == ';'
	}) {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		pats = append(pats, tok)
	}
	if err := h.cfg.SetKioskAllowlist(pats); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsSetAlertWebhook(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The form masks the stored URL (a bearer secret) and never re-sends it, so a blank
	// submission means "keep the existing value" rather than "clear it".
	if u := strings.TrimSpace(r.FormValue("alert_webhook_url")); u != "" {
		h.cfg.SetAlertWebhookURL(u)
	}
	h.audit(r, "alerts.webhook", "", "")
	h.hxDoneToast(w, r, h.settingsDest(r), "Settings saved", "success")
}

// hhmm renders minutes-past-midnight as a "HH:MM" string for <input type=time>.
func hhmm(min int) string {
	min = ((min % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// parseHHMM parses "HH:MM" into minutes-past-midnight, returning def on bad input.
func parseHHMM(s string, def int) int {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return def
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return def
	}
	return h*60 + m
}

// serviceWindowView is one editable service window (fleet default or a group's).
type serviceWindowView struct {
	GroupID, GroupName                 string // GroupID "" = fleet default; otherwise a restaurant id
	Open, Close, NightOpen, NightClose string // HH:MM
	TZ                                 string
	HasOwn                             bool // restaurant has its own row (vs inheriting fleet)
}

func windowView(restaurantID, name string, w db.ServiceWindow, hasOwn bool) serviceWindowView {
	return serviceWindowView{
		GroupID: restaurantID, GroupName: name,
		Open: hhmm(w.OpenMin), Close: hhmm(w.CloseMin),
		NightOpen: hhmm(w.NightOpenMin), NightClose: hhmm(w.NightCloseMin),
		TZ: w.TZ, HasOwn: hasOwn,
	}
}

// buildServiceWindowViews returns the fleet default plus each restaurant's window for the
// Settings "Service hours" card.
func (h *Handler) buildServiceWindowViews(ctx context.Context) (serviceWindowView, []serviceWindowView) {
	fleet, _ := h.db.GetFleetServiceWindow(ctx)
	fleetView := windowView("", "", fleet, true)
	restaurants, _ := h.db.ListRestaurants(ctx)
	windows, _ := h.db.ListRestaurantServiceWindows(ctx) // one query, not one per restaurant
	var out []serviceWindowView
	for _, rest := range restaurants {
		if w, ok := windows[rest.ID]; ok {
			out = append(out, windowView(rest.ID.String(), rest.Name, w, true))
		} else {
			out = append(out, windowView(rest.ID.String(), rest.Name, fleet, false))
		}
	}
	return fleetView, out
}

// SettingsSetServiceWindow upserts a service window from the Settings form. An empty
// group_id sets the fleet default; "reset" on a restaurant deletes its row (inherit fleet).
// The form field is named group_id for back-compat but carries a restaurant id.
func (h *Handler) SettingsSetServiceWindow(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	gidStr := strings.TrimSpace(r.FormValue("group_id"))
	if gidStr != "" && r.FormValue("action") == "reset" {
		if gid, err := uuid.Parse(gidStr); err == nil {
			_ = h.db.DeleteServiceWindow(r.Context(), gid)
		}
		h.audit(r, "alerts.service_window", gidStr, "reset")
		h.settingsRedirect(w, r)
		return
	}
	sw := db.ServiceWindow{
		OpenMin:       parseHHMM(r.FormValue("open"), 420),
		CloseMin:      parseHHMM(r.FormValue("close"), 1380),
		NightOpenMin:  parseHHMM(r.FormValue("night_open"), 1410),
		NightCloseMin: parseHHMM(r.FormValue("night_close"), 360),
		TZ:            strings.TrimSpace(r.FormValue("timezone")),
	}
	if gidStr != "" {
		if gid, err := uuid.Parse(gidStr); err == nil {
			sw.RestaurantID = &gid
		} else {
			http.Error(w, "Invalid restaurant", http.StatusBadRequest)
			return
		}
	}
	if err := h.db.SetServiceWindow(r.Context(), sw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.service_window", gidStr, "")
	h.settingsRedirect(w, r)
}

// SettingsSaveChannel creates a channel (no id) or updates one (id present).
func (h *Handler) SettingsSaveChannel(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	kind := r.FormValue("kind")
	if kind != "teams" {
		kind = "webhook"
	}
	// Unescape HTML entities (e.g. "&amp;" -> "&") before storing: a webhook URL pasted
	// from a rendered HTML source would otherwise persist a mangled query string
	// (sp/sv/sig become amp;sp/...), silently breaking all delivery.
	url := html.UnescapeString(strings.TrimSpace(r.FormValue("url")))
	c := db.AlertChannel{
		Name:          strings.TrimSpace(r.FormValue("name")),
		Kind:          kind,
		URL:           url,
		MinSeverity:   r.FormValue("min_severity"),
		Mode:          r.FormValue("mode"),
		ActiveWindow:  r.FormValue("active_window"),
		Enabled:       r.FormValue("enabled") == "on",
		NotifyResolve: r.FormValue("notify_resolve") == "on",
	}
	switch c.MinSeverity {
	case "info", "warning", "critical":
	default:
		c.MinSeverity = "warning"
	}
	if c.Mode != "digest" {
		c.Mode = "realtime"
	}
	if c.ActiveWindow != "service" && c.ActiveWindow != "overnight" {
		c.ActiveWindow = ""
	}
	// Per-type allowlist: keep only known types, de-duped. If every type is selected,
	// store nothing so the channel stays "all types" (back-compat and tidy).
	seen := map[string]bool{}
	for _, t := range r.Form["alert_types"] {
		if validAlertType(t) && !seen[t] {
			seen[t] = true
			c.AlertTypes = append(c.AlertTypes, t)
		}
	}
	if len(c.AlertTypes) >= countAlertTypes() {
		c.AlertTypes = nil
	}
	var err error
	if idStr := r.FormValue("id"); idStr != "" {
		if c.ID, err = uuid.Parse(idStr); err != nil {
			http.Error(w, "Invalid channel", http.StatusBadRequest)
			return
		}
		// The edit form masks the stored URL and never re-sends it, so a blank URL on
		// an update means "keep the existing one" rather than blanking delivery.
		if url == "" {
			if existing, e := h.db.GetAlertChannel(r.Context(), c.ID); e == nil {
				c.URL = existing.URL
			}
		}
		err = h.db.UpdateAlertChannel(r.Context(), c)
	} else {
		_, err = h.db.CreateAlertChannel(r.Context(), c)
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.channel", c.Name, "")
	h.settingsRedirect(w, r)
}

// SettingsTestChannel sends a sample alert to one channel so the admin can confirm the
// webhook URL works before a real alert fires. Redirects back with a ?channel_test result.
func (h *Handler) SettingsTestChannel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid channel", http.StatusBadRequest)
		return
	}
	c, err := h.db.GetAlertChannel(r.Context(), id)
	if err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}
	sample := db.AlertNotification{
		Type: "test", Severity: "info", Serial: "TEST-DEVICE",
		Summary: "Test alert from AIO MDM — this channel is wired up correctly.",
		EventAt: time.Now().UTC(),
	}
	result := "ok"
	if err := alerts.SendToChannel(r.Context(), c, sample); err != nil {
		log.Printf("[alert] test channel %q failed: %v", c.Name, err)
		result = "fail"
	}
	h.audit(r, "alerts.channel.test", c.Name, result)
	http.Redirect(w, r, "/settings?channel_test="+result+"#channels", http.StatusFound)
}

// SettingsDeleteChannel removes an alert channel.
func (h *Handler) SettingsDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid channel", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteAlertChannel(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.channel.delete", id.String(), "")
	h.settingsRedirect(w, r)
}

// alertParamField describes one tunable threshold of an alert rule.
type alertParamField struct {
	Key, Label, Unit string
	Step, Default    float64
}

// alertRuleDefs is the catalog of configurable alert rules and their thresholds,
// in display order. Keys/defaults mirror db.go's detectRule/detectRecentRule param()
// calls. Windowed rules are operational (deployed units only) and expose an active-window
// selector; Recent rules are evaluated every minute, the rest hourly during housekeeping.
var alertRuleDefs = []struct {
	Type, Label, Desc, Category string
	Fields                      []alertParamField
	Windowed, Recent            bool
}{
	// ── Thermal ──
	{"overheating", "Device overheating", "Fires within ~1 minute when a device's current temperature is at or above the limit; a device on the wireless charger uses the higher on-pad limit. Auto-resolves once it cools.", "Thermal", []alertParamField{
		{"temp_c", "Limit (off pad)", "°C", 1, 45},
		{"temp_c_wlc", "Limit (on charger)", "°C", 1, 65},
	}, false, true},
	{"temp_elevated", "Temperature elevated", "Fires when device temperature holds in the elevated band for >15 min (trending toward throttle).", "Thermal", []alertParamField{
		{"temp_min", "Band low", "°C", 1, 38},
		{"temp_max", "Band high", "°C", 1, 45},
	}, false, true},
	// ── Connectivity ──
	{"offline", "Device offline", "Fires when any device (deployed or bench) is silent longer than the threshold. Self-suppresses overnight via its own quiet window.", "Connectivity", []alertParamField{
		{"offline_minutes", "Offline after", "min", 1, 5},
	}, false, true},
	{"offline_peak", "Offline during peak", "Fires when a deployed device goes offline during its restaurant's peak hours. Configure peak ranges per restaurant.", "Connectivity", []alertParamField{
		{"offline_minutes", "Offline after", "min", 1, 5},
	}, true, true},
	{"wifi_weak", "Weak Wi-Fi signal", "Fires when the connected Wi-Fi RSSI holds below the floor for the sustain window — packet loss territory for voice/payment APIs.", "Connectivity", []alertParamField{
		{"rssi_dbm", "Signal floor", "dBm", 1, -75},
		{"sustain_min", "Sustained for", "min", 1, 10},
	}, false, true},
	{"wifi_unstable", "Frequent Wi-Fi disconnects", "Fires when the device reports at least this many Wi-Fi disconnects within the last hour.", "Connectivity", []alertParamField{
		{"disconnects", "Disconnects / hr", "", 1, 3},
	}, false, true},
	// ── Storage ──
	{"storage_low", "Storage critically low", "Fires when free storage falls below the critical floor.", "Storage", []alertParamField{
		{"free_gb", "Free floor", "GB", 0.1, 1},
	}, false, true},
	{"storage_warning", "Storage low", "An earlier, gentler heads-up: fires when free storage drops below this level. Automatically stops once it falls low enough for the critical alert to take over.", "Storage", []alertParamField{
		{"free_gb", "Warning level", "GB", 1, 14},
	}, false, true},
	{"storage_filling", "Storage filling fast", "Watches how FAST space is disappearing, not how low it is: fires when a device loses more than this much free storage in 24 h.", "Storage", []alertParamField{
		{"drop_gb", "24h drop", "GB", 0.1, 0.2},
	}, false, false},
	// ── Battery ──
	{"battery_low", "Battery low during peak", "Fires when a deployed device's battery drops below the threshold during peak hours.", "Battery", []alertParamField{
		{"soc_pct", "Battery floor", "%", 1, 20},
	}, true, true},
	{"battery_high_night", "Battery high overnight", "Fires when a deployed device sits at/above the threshold overnight instead of cycling down.", "Battery", []alertParamField{
		{"soc_pct", "Battery level", "%", 1, 60},
	}, true, true},
	// ── Power / wireless charging ──
	{"wlc_continuous", "Continuous wireless charging", "Fires when a device has been on the wireless charger continuously for at least the threshold (heat / battery stress).", "Power", []alertParamField{
		{"sustain_min", "Continuous for", "min", 5, 60},
	}, false, true},
	{"wlc_dead", "Wireless charger not functional all day", "Fires when a deployed unit's pad was never readable for a whole day, though it worked within the prior week (daily).", "Power", []alertParamField{}, false, false},
	{"charger_flapping", "Charger flapping / faulty", "Fires when a device's charging state toggles on/off more than this many times per minute — a faulty charger, dock, or cable dropping the connection in and out.", "Power", []alertParamField{
		{"flaps_per_min", "Toggles / min", "", 1, 10},
		{"window_min", "Measured over", "min", 1, 5},
	}, false, true},
	{"slow_charge_night", "Slow overnight charging", "Fires when a device charges overnight but its battery gains at most this much over the window (stalled/trickle charge).", "Power", []alertParamField{
		{"max_gain_pct", "Max gain", "%", 1, 15},
		{"window_hours", "Over", "h", 1, 2},
	}, true, true},
	// ── System health ──
	{"memory_pressure", "Memory pressure", "Fires when a device's peak RAM usage exceeds the threshold (predicts crashes/reboots). Also the cutoff the Daily Report uses for memory.", "System", []alertParamField{
		{"ram_pct", "RAM usage", "%", 1, 85},
	}, false, false},
	{"memory_low", "Memory low (available)", "Fires when available RAM (total − used) holds below the floor for >8 min — Android's low-memory killer territory.", "System", []alertParamField{
		{"avail_mb", "Available floor", "MB", 10, 400},
	}, false, true},
	{"device_crash", "Device crash / ANR", "Fires when the device reports an app/system crash, ANR, or native tombstone (from DropBox) within the window.", "System", []alertParamField{
		{"window_min", "Look-back", "min", 1, 15},
	}, false, true},
}

// alertTypeOption / alertTypeGroup back the per-channel alert-type filter UI.
type alertTypeOption struct{ Type, Label string }
type alertTypeGroup struct {
	Category string
	Types    []alertTypeOption
}

// alertTypeCatalog groups every dispatchable alert type by category for the
// per-channel filter. It is built from alertRuleDefs plus the dispatchable types
// that aren't threshold-configurable rules (so a filtered channel can still keep
// them rather than silently dropping them).
func alertTypeCatalog() []alertTypeGroup {
	var order []string
	byCat := map[string][]alertTypeOption{}
	add := func(cat, typ, label string) {
		if _, ok := byCat[cat]; !ok {
			order = append(order, cat)
		}
		byCat[cat] = append(byCat[cat], alertTypeOption{typ, label})
	}
	for _, d := range alertRuleDefs {
		add(d.Category, d.Type, d.Label)
	}
	add("Lifecycle", "new_device", "New device onboarded")
	groups := make([]alertTypeGroup, 0, len(order))
	for _, c := range order {
		groups = append(groups, alertTypeGroup{c, byCat[c]})
	}
	return groups
}

// alertTypeLabel returns the friendly catalog label for an alert type, falling
// back to the raw type string when it isn't a known catalog entry.
func alertTypeLabel(t string) string {
	for _, g := range alertTypeCatalog() {
		for _, o := range g.Types {
			if o.Type == t {
				return o.Label
			}
		}
	}
	return t
}

// alertCategory returns the catalog category (Thermal, Storage, …) for an alert
// type, or "" when the type isn't in the catalog.
func alertCategory(t string) string {
	for _, g := range alertTypeCatalog() {
		for _, o := range g.Types {
			if o.Type == t {
				return g.Category
			}
		}
	}
	return ""
}

// alertCategories lists the catalog categories in display order, for the Alerts
// page type filter.
func alertCategories() []string {
	out := make([]string, 0, 8)
	for _, g := range alertTypeCatalog() {
		out = append(out, g.Category)
	}
	return out
}

// alertTypesForCategory returns the alert types belonging to a catalog category, so the
// category filter can be pushed into the SQL query.
func alertTypesForCategory(category string) []string {
	var types []string
	for _, g := range alertTypeCatalog() {
		if g.Category != category {
			continue
		}
		for _, o := range g.Types {
			types = append(types, o.Type)
		}
	}
	return types
}

// alertsQueryString builds the /alerts query string preserving the active filters,
// so each filter control can change one dimension without dropping the others. Changing a
// filter resets pagination (no page param), which is what you want.
func alertsQueryString(status, severity, category string) string {
	return alertsPageQueryString(status, severity, category, 1)
}

// alertsPageQueryString is alertsQueryString plus a page number (omitted for page 1), for
// the pagination links which preserve the active filters.
func alertsPageQueryString(status, severity, category string, page int) string {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if severity != "" {
		q.Set("severity", severity)
	}
	if category != "" {
		q.Set("category", category)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// validAlertType reports whether t is a known dispatchable alert type.
func validAlertType(t string) bool {
	for _, g := range alertTypeCatalog() {
		for _, o := range g.Types {
			if o.Type == t {
				return true
			}
		}
	}
	return false
}

// countAlertTypes is the total number of selectable types (used to collapse a
// fully-selected allowlist back to "all").
func countAlertTypes() int {
	n := 0
	for _, g := range alertTypeCatalog() {
		n += len(g.Types)
	}
	return n
}

type alertFieldView struct {
	Key, Label, Unit string
	Step, Value      float64
}

type alertRuleView struct {
	ID, Type, Name, Desc string
	Enabled              bool
	Fields               []alertFieldView
	Windowed, Recent     bool
	ActiveWindow         string
	DeployedOnly         bool
}

// alertRuleGroup buckets rules by category for the Settings UI, with an enabled count.
type alertRuleGroup struct {
	Category     string
	Rules        []alertRuleView
	EnabledCount int
}

// buildAlertRuleViews merges the seeded alert rules with the field catalog so the
// settings page can render an enable toggle + current thresholds per rule.
func (h *Handler) buildAlertRuleViews(ctx context.Context) []alertRuleGroup {
	rules, err := h.db.ListAlertRules(ctx, false)
	if err != nil {
		return nil
	}
	byType := map[string]db.AlertRule{}
	for _, r := range rules {
		byType[r.Type] = r
	}
	var groups []alertRuleGroup
	idx := map[string]int{} // category -> groups index, preserving first-seen order
	for _, def := range alertRuleDefs {
		r, ok := byType[def.Type]
		if !ok {
			continue
		}
		var p map[string]float64
		_ = json.Unmarshal(r.Params, &p)
		var fields []alertFieldView
		for _, f := range def.Fields {
			val := f.Default
			if v, ok := p[f.Key]; ok {
				val = v
			}
			fields = append(fields, alertFieldView{f.Key, f.Label, f.Unit, f.Step, val})
		}
		aw := r.ActiveWindow
		if aw == "" {
			aw = "always"
		}
		view := alertRuleView{
			ID: r.ID.String(), Type: def.Type, Name: def.Label, Desc: def.Desc,
			Enabled: r.Enabled, Fields: fields,
			Windowed: def.Windowed, Recent: def.Recent, ActiveWindow: aw,
			DeployedOnly: r.DeployedOnly,
		}
		gi, ok := idx[def.Category]
		if !ok {
			gi = len(groups)
			idx[def.Category] = gi
			groups = append(groups, alertRuleGroup{Category: def.Category})
		}
		groups[gi].Rules = append(groups[gi].Rules, view)
		if r.Enabled {
			groups[gi].EnabledCount++
		}
	}
	return groups
}

// WrappedPage renders "Fleet Wrapped" — a playful, full-screen year-in-review of
// the whole fleet (Spotify-Wrapped style). Open to any signed-in role.
func (h *Handler) WrappedPage(w http.ResponseWriter, r *http.Request) {
	me, _ := h.db.UserStats(r.Context(), h.currentUsername(r), h.user)
	topUsers, _ := h.db.TopActors(r.Context(), 5, h.user) // the env admin login is not a person on the team
	wr, err := h.db.GetFleetWrapped(r.Context())
	if err != nil {
		log.Printf("[wrapped] compute: %v", err)
	}
	h.render(w, r, "wrapped.html", map[string]any{
		"Title":      "Fleet Wrapped",
		"W":          wr,
		"Me":         me,
		"TopUsers":   topUsers,
		"OnlineDays": wr.OnlineMinutes / 1440,
		"WorkerDays": int(wr.HardestWorker.Value) / 1440,
	})
}

// AlertConfigView renders the alert-rule configuration read-only. Editing stays
// in Settings (admin-only); this page lets operators and devs see exactly what the
// fleet watches for and the thresholds that trigger each alert.
func (h *Handler) AlertConfigView(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "alert_config.html", map[string]any{
		"Title":      "Alert config",
		"AlertRules": h.buildAlertRuleViews(r.Context()),
		"Peak":       h.peakView(r.Context(), nil),
	})
}

// alertThresholds reads the configurable cutoffs from the alert rules so the fleet
// report judges problems against the same numbers (defaults if a rule is missing).
func (h *Handler) alertThresholds(ctx context.Context) ai.Thresholds {
	t := ai.Thresholds{TempC: 45, MinFullPct: 90, MaxChargeFrac: 0.3, RAMPct: 85}
	rules, err := h.db.ListAlertRules(ctx, false)
	if err != nil {
		return t
	}
	for _, r := range rules {
		var p map[string]float64
		_ = json.Unmarshal(r.Params, &p)
		get := func(k string, d float64) float64 {
			if v, ok := p[k]; ok {
				return v
			}
			return d
		}
		switch r.Type {
		case "overheating":
			t.TempC = get("temp_c", t.TempC)
		case "memory_pressure":
			t.RAMPct = get("ram_pct", t.RAMPct)
		}
	}
	return t
}

// SettingsUpdateAlertRule saves one alert rule's enabled flag and thresholds.
func (h *Handler) SettingsUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid rule ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	typ := r.FormValue("type")
	var def *struct {
		Type, Label, Desc, Category string
		Fields                      []alertParamField
		Windowed, Recent            bool
	}
	for i := range alertRuleDefs {
		if alertRuleDefs[i].Type == typ {
			def = &alertRuleDefs[i]
			break
		}
	}
	if def == nil {
		http.Error(w, "Unknown rule type", http.StatusBadRequest)
		return
	}
	params := map[string]float64{}
	for _, f := range def.Fields {
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(f.Key)), 64); err == nil {
			params[f.Key] = v
		} else {
			params[f.Key] = f.Default
		}
	}
	pj, _ := json.Marshal(params)
	// active_window is only honored for windowed (operational) rules; "" leaves it as-is.
	aw := ""
	if def.Windowed {
		switch r.FormValue("active_window") {
		case "service":
			aw = "service"
		case "overnight":
			aw = "overnight"
		case "peak":
			aw = "peak"
		case "always":
			aw = "always"
		}
	}
	// A windowed rule is deployed-only by nature and has no toggle in the form, so it
	// keeps the flag set; everything else takes the checkbox.
	deployedOnly := def.Windowed || r.FormValue("deployed_only") == "on"
	if err := h.db.UpdateAlertRule(r.Context(), id, r.FormValue("enabled") == "on", pj, aw, deployedOnly); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.rule", typ, "")
	h.settingsRedirect(w, r)
}

// SettingsSetAI saves the Anthropic API key, model, and daily-digest toggle. An
// empty key field is treated as "keep the current key" so saving the model/digest
// never clears a configured key; submit the literal "-" to clear it.
func (h *Handler) SettingsSetAI(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if p := strings.TrimSpace(r.FormValue("ai_provider")); p != "" {
		h.cfg.SetAIProvider(p)
	}
	if k := strings.TrimSpace(r.FormValue("anthropic_api_key")); k != "" {
		if k == "-" {
			k = ""
		}
		h.cfg.SetAnthropicAPIKey(k)
	}
	// Model and base URL are saved verbatim (empty allowed — defaults fill in).
	h.cfg.SetAnthropicModel(strings.TrimSpace(r.FormValue("anthropic_model")))
	h.cfg.SetAIBaseURL(strings.TrimSpace(r.FormValue("ai_base_url")))
	h.cfg.SetAIDigestEnabled(r.FormValue("ai_digest") == "on")
	h.audit(r, "ai.settings", "", "")
	h.settingsRedirect(w, r)
}

// writeAIResult runs an analysis closure under a timeout, records token usage, and
// writes a JSON {"text","model","generated_at","input_tokens","output_tokens"}
// body, or an error JSON on failure.
func (h *Handler) writeAIResult(w http.ResponseWriter, r *http.Request, run func(ctx context.Context, c *ai.Client) (string, ai.Usage, error)) {
	if !h.cfg.AIEnabled() {
		writeJSONError(w, http.StatusServiceUnavailable, "AI analysis is not configured — add an Anthropic API key in Settings.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	client := ai.New(h.cfg.AIProvider(), h.cfg.AnthropicAPIKey(), h.cfg.AnthropicModel(), h.cfg.AIBaseURL())
	text, usage, err := run(ctx, client)
	if err != nil {
		log.Printf("[ai] analysis failed: %v", err)
		writeJSONError(w, http.StatusBadGateway, "Analysis failed: "+err.Error())
		return
	}
	if err := h.db.RecordAIUsage(ctx, usage.InputTokens, usage.OutputTokens); err != nil {
		log.Printf("[ai] record usage: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"text":          text,
		"model":         client.Model(),
		"generated_at":  time.Now().Format(time.RFC3339),
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// DeviceAIAnalysis returns an AI reading of one device's recent trends + open alerts.
func (h *Handler) DeviceAIAnalysis(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Device not found")
		return
	}
	var resultText, resultModel string
	h.writeAIResult(w, r, func(ctx context.Context, c *ai.Client) (string, ai.Usage, error) {
		stats, err := h.db.GetDeviceDailyStats(ctx, device.ID, 30)
		if err != nil {
			return "", ai.Usage{}, err
		}
		// No per-device alert query exists; filter the open list by serial.
		open, _ := h.db.ListAlerts(ctx, "open", 200)
		var devAlerts []db.Alert
		for _, a := range open {
			if a.Serial == serial {
				devAlerts = append(devAlerts, a)
			}
		}
		text, usage, err := c.AnalyzeDevice(ctx, serial, device.DeployedEffective, stats, devAlerts)
		if err == nil {
			resultText, resultModel = text, c.Model()
		}
		return text, usage, err
	})
	if resultText != "" {
		if err := h.db.SaveDeviceAIAnalysis(r.Context(), device.ID, resultModel, resultText); err != nil {
			log.Printf("[ai] save device analysis: %v", err)
		}
	}
	h.audit(r, "ai.device", serial, "")
}

// AlertLogcatAnalyze runs an AI triage over the logs auto-captured for a crash
// alert and returns the analysis as JSON (same shape as the other AI endpoints).
func (h *Handler) AlertLogcatAnalyze(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	lc, ok, err := h.db.GetAlertLogcat(r.Context(), id)
	if err != nil || !ok || strings.TrimSpace(lc.Content) == "" {
		writeJSONError(w, http.StatusNotFound, "No captured logs to analyze yet.")
		return
	}
	h.writeAIResult(w, r, func(ctx context.Context, c *ai.Client) (string, ai.Usage, error) {
		return c.AnalyzeLog(ctx, lc.Content)
	})
	h.audit(r, "ai.logcat.analyze", id.String(), "")
}

// DeviceAIAnalysesList returns a device's stored AI analyses (newest first) as JSON.
func (h *Handler) DeviceAIAnalysesList(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Device not found")
		return
	}
	list, err := h.db.ListDeviceAIAnalyses(r.Context(), device.ID, 20)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load analyses")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"analyses": list})
}

func (h *Handler) SettingsSetSessionTimeout(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sec := 86400
	if n, err := strconv.Atoi(r.FormValue("timeout")); err == nil && n >= 300 {
		sec = n
	}
	h.cfg.SetSessionTimeout(sec)
	h.store.MaxAge(sec)             // apply to cookie + codec at runtime
	h.hxRedirect(w, r, h.settingsDest(r)) // may clamp to default — re-render the stored value
}

func (h *Handler) SettingsLogoutAll(w http.ResponseWriter, r *http.Request) {
	// Delete every server-side session, including this one (GB-04/GB-08).
	h.audit(r, "session.logout_all", "", "")
	if err := h.db.DeleteAllSessions(r.Context()); err != nil {
		log.Printf("[session] logout all: %v", err)
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (h *Handler) SettingsToggleShell(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetShellEnabled(!h.cfg.ShellEnabled())
	h.settingsToggleResponse(w, r, "/settings/shell/toggle", h.cfg.ShellEnabled())
}

func (h *Handler) SettingsToggleRemote(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetRemoteEnabled(!h.cfg.RemoteEnabled())
	h.settingsToggleResponse(w, r, "/settings/remote/toggle", h.cfg.RemoteEnabled())
}

func (h *Handler) SettingsSetCheckinInterval(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sec := 60
	if v := r.FormValue("interval"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 {
			sec = n
		}
	}
	h.cfg.SetCheckinInterval(sec)
	h.hxRedirect(w, r, h.settingsDest(r)) // may clamp to default — re-render the stored value
}

// SettingsSetWlcProducts saves which product keys have a wireless-charging pad
// (one per line; blank resets to the default t7-only list).
func (h *Handler) SettingsSetWlcProducts(w http.ResponseWriter, r *http.Request) {
	var keys []string
	seen := map[string]bool{}
	for _, line := range strings.Split(r.FormValue("products"), "\n") {
		k := strings.ToLower(strings.TrimSpace(line))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if err := h.cfg.SetWlcProducts(keys); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/settings#devices", "WLC products saved", "success")
}

func (h *Handler) SettingsAddColumn(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	key := strings.TrimSpace(r.FormValue("key"))
	label := strings.TrimSpace(r.FormValue("label"))
	if key == "" || label == "" {
		h.settingsRedirect(w, r)
		return
	}
	h.cfg.Add(config.ExtraColumn{Key: key, Label: label})
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsRemoveColumn(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	h.cfg.Remove(key)
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsAddLegacyBuild(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if id := strings.TrimSpace(r.FormValue("build_id")); id != "" {
		h.cfg.AddLegacyBuild(id)
	}
	h.settingsRedirect(w, r)
}

func (h *Handler) SettingsRemoveLegacyBuild(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	h.cfg.RemoveLegacyBuild(strings.TrimSpace(r.FormValue("build_id")))
	h.settingsRedirect(w, r)
}

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

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
		return
	}

	payload := buildPayload(cmdType, r)
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
			if cmdType == "screenshot" || cmdType == "shell" {
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
	if cmdType == "screenshot" || cmdType == "shell" {
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
		h.hxDoneToast(w, r, "/devices/"+serial, "The DPC agent reads its server from its own config, not this property", "error")
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

	// Kiosk mode: single app (classic), multi-app (extra packages allowed in
	// lock-task), or browser (locked browser on a URL). Older callers that don't
	// send a mode keep the classic single-app behaviour.
	mode := r.FormValue("kiosk_mode")
	switch mode {
	case "app", "multi", "browser":
	case "":
		mode = "app"
	default:
		http.Error(w, "Unknown kiosk mode", http.StatusBadRequest)
		return
	}

	// Browser kiosk locks to the agent's built-in browser, not an installed app.
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
			http.Error(w, "Kiosk package is required when enabling kiosk mode", http.StatusBadRequest)
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
	h.pushKioskConfigToDevices(r.Context(), []uuid.UUID{device.ID})
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// ── Packages ──────────────────────────────────────────────────────────────────

// FleetPackages renders the admin app-classification page: every package across the
// fleet, how clients classified it, and an admin override to flag system apps used
// as the fallback when no client reports the flag.
func (h *Handler) FleetPackages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	pkgs, err := h.db.ListPackagesAdmin(r.Context(), q)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "packages.html", map[string]any{
		"Title":    "App classification",
		"Packages": pkgs,
		"Query":    q,
	})
}

// PackageFlag toggles the admin system-app override for a package (admin only).
func (h *Handler) PackageFlag(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimSpace(r.FormValue("package"))
	if pkg == "" {
		http.Error(w, "package required", http.StatusBadRequest)
		return
	}
	flagged := r.FormValue("flagged") == "1"
	if err := h.db.SetPackageSystemOverride(r.Context(), pkg, flagged); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// HTMX posts get a 204 (the row updates its own UI); plain forms redirect back.
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/packages", http.StatusSeeOther)
}

func (h *Handler) DevicePackages(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	pkgs, err := h.db.GetDevicePackages(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Default to app-drawer apps only (see launchableOnly); ?all=1 keeps the raw
	// package dump reachable.
	showAll := r.URL.Query().Get("all") == "1"
	total := len(pkgs)
	if !showAll {
		pkgs = launchableOnly(pkgs)
	}
	h.render(w, r, "device_packages.html", map[string]any{
		"Title":    serial + " — Packages",
		"Device":   device,
		"Packages": pkgs,
		"ShowAll":  showAll,
		"AllCount": total,
	})
}

// ── Productions ───────────────────────────────────────────────────────────────

func (h *Handler) ProductionList(w http.ResponseWriter, r *http.Request) {
	productions, err := h.db.ListProductions(r.Context(), h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "productions.html", map[string]any{
		"Productions": productions,
	})
}

func (h *Handler) ProductionNew(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "production_form.html", nil)
}

func (h *Handler) ProductionCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	name := strings.TrimSpace(r.FormValue("name"))
	productCode := strings.ToUpper(strings.TrimSpace(r.FormValue("product_code")))
	modelCode := strings.TrimSpace(r.FormValue("model_code"))
	variant := r.FormValue("variant")
	if variant == "" {
		variant = "0"
	}
	sku := strings.ToUpper(strings.TrimSpace(r.FormValue("sku")))
	if sku == "" {
		sku = "AA"
	}
	// Model code is the zero-padded integer part of the model size; accept a single
	// digit ("6") and pad it to two ("06") to match the field's documented contract.
	if len(modelCode) == 1 {
		modelCode = "0" + modelCode
	}
	batchMonth, _ := strconv.Atoi(r.FormValue("batch_month"))
	batchYear, _ := strconv.Atoi(r.FormValue("batch_year"))
	startSeq, _ := strconv.Atoi(r.FormValue("start_sequence"))
	quantity, _ := strconv.Atoi(r.FormValue("quantity"))
	notes := r.FormValue("notes")

	if name == "" || productCode == "" || modelCode == "" ||
		batchMonth < 1 || batchMonth > 12 || batchYear < 0 || batchYear > 99 ||
		startSeq < 1 || quantity < 1 {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	// The serial schema is a fixed 9-char prefix + 5-digit sequence = 14 chars, and the
	// device-matching queries hard-code LENGTH(serial)=14. Reject any component length
	// that would produce a serial that can never match a device.
	if len(productCode) != 2 || len(modelCode) != 2 || len(variant) != 1 || len(sku) != 2 {
		http.Error(w, "Product code and SKU must be exactly 2 characters, model code 2 digits, variant 1 character.", http.StatusBadRequest)
		return
	}
	// The batch code normally encodes month+year, but the earliest runs shipped with
	// codes that predate that convention (e.g. "26", which decodes to month 0). Those
	// devices exist, so let an admin type the literal code; month/year still record
	// when the run happened.
	batch := strings.ToUpper(strings.TrimSpace(r.FormValue("batch")))
	if batch == "" {
		batch = db.EncodeBatch(batchMonth, batchYear)
	} else if len(batch) != 2 || strings.IndexFunc(batch, func(c rune) bool {
		return !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z'))
	}) >= 0 {
		http.Error(w, "Batch code must be exactly 2 letters or digits.", http.StatusBadRequest)
		return
	}

	endSeq := startSeq + quantity - 1
	params := db.ProductionParams{
		Name:          name,
		ProductCode:   productCode,
		ModelCode:     modelCode,
		Variant:       variant,
		SKU:           sku,
		Batch:         batch,
		BatchMonth:    batchMonth,
		BatchYear:     batchYear,
		StartSequence: startSeq,
		EndSequence:   endSeq,
		Notes:         notes,
	}
	prod, err := h.db.CreateProduction(r.Context(), params)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/productions/"+prod.ID.String(), http.StatusFound)
}

func (h *Handler) ProductionDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	prod, err := h.db.GetProduction(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Production not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{ProductionID: id}, 0, 2000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "production_detail.html", map[string]any{
		"Production":          prod,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	})
}

func (h *Handler) ProductionExportCSV(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	prod, err := h.db.GetProduction(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Production not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.GetProductionDevices(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Index connected devices by serial for O(1) lookup
	connected := make(map[string]*db.ProductionDevice, len(devices))
	for i := range devices {
		connected[devices[i].Serial] = &devices[i]
	}

	filename := safeCSVFilename(prod.Name, "production")
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	cw.Write([]string{"serial_number", "status", "build_id", "battery_pct", "last_seen_at", "first_seen_at"})

	prefix := prod.SerialPrefix()
	for seq := prod.StartSequence; seq <= prod.EndSequence; seq++ {
		serial := fmt.Sprintf("%s%05d", prefix, seq)
		if dev, ok := connected[serial]; ok {
			lastSeen := ""
			firstSeen := ""
			if !dev.LastSeenAt.IsZero() {
				lastSeen = dev.LastSeenAt.UTC().Format(time.RFC3339)
			}
			if !dev.CreatedAt.IsZero() {
				firstSeen = dev.CreatedAt.UTC().Format(time.RFC3339)
			}
			cw.Write([]string{serial, dev.ConnectionStatus, dev.BuildID, strconv.Itoa(dev.BatteryPct), lastSeen, firstSeen})
		} else {
			cw.Write([]string{serial, "never", "", "", "", ""})
		}
	}
	cw.Flush()
}

func (h *Handler) ProductionDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteProduction(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/productions", http.StatusFound)
}

// ProductionPreviewSerial returns the first/last serial for the given form values (HTMX partial).
func (h *Handler) ProductionPreviewSerial(w http.ResponseWriter, r *http.Request) {
	productCode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("product_code")))
	modelCode := strings.TrimSpace(r.URL.Query().Get("model_code"))
	variant := r.URL.Query().Get("variant")
	if variant == "" {
		variant = "0"
	}
	sku := strings.ToUpper(r.URL.Query().Get("sku"))
	if sku == "" {
		sku = "AA"
	}
	batchMonth, _ := strconv.Atoi(r.URL.Query().Get("batch_month"))
	batchYear, _ := strconv.Atoi(r.URL.Query().Get("batch_year"))
	startSeq, _ := strconv.Atoi(r.URL.Query().Get("start_sequence"))
	quantity, _ := strconv.Atoi(r.URL.Query().Get("quantity"))

	if productCode == "" || modelCode == "" || batchMonth < 1 || batchMonth > 12 || batchYear < 0 || startSeq < 1 || quantity < 1 {
		fmt.Fprint(w, `<span class="muted">—</span>`)
		return
	}

	batch := db.EncodeBatch(batchMonth, batchYear)
	prefix := productCode + modelCode + variant + sku + batch
	first := fmt.Sprintf("%s%05d", prefix, startSeq)
	last := fmt.Sprintf("%s%05d", prefix, startSeq+quantity-1)
	fmt.Fprintf(w, `<code class="serial-preview">%s</code> → <code class="serial-preview">%s</code> &nbsp;<span class="muted small">batch <strong>%s</strong></span>`, first, last, batch)
}

// ── Users ─────────────────────────────────────────────────────────────────────

func (h *Handler) UserList(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	admins, operators, viewers := 0, 0, 0
	for _, u := range users {
		switch u.Role {
		case "admin":
			admins++
		case "operator":
			operators++
		default:
			viewers++
		}
	}
	// The env-configured admin account isn't a DB row, but it always exists —
	// count it so the KPI strip isn't misleadingly "0 admins".
	admins++
	// The env-configured dashboard login (DASHBOARD_USER) has no users row by
	// design, so it would otherwise always show up here as "unlinked".
	summaries, _ := h.db.ActorSummaries(r.Context())
	orphans, _ := h.db.ListOrphanActors(r.Context())
	for i := 0; i < len(orphans); i++ {
		if orphans[i].Username == h.user {
			orphans = append(orphans[:i], orphans[i+1:]...)
			i--
		}
	}
	grantCounts, _ := h.db.CountAccessGrants(r.Context(), sensitiveActionKeys)
	h.render(w, r, "users.html", map[string]any{
		"Users":     users,
		"Orphans":   orphans,
		"Summaries": summaries,
		"Grants":    grantCounts,
		"Roles":     roleOrder,
		"UsersTab":  "people",
		"Assignable": assignableRoles(h.role(r)),
		"Restaurants": func() []db.Restaurant { rs, _ := h.db.ListRestaurants(r.Context()); return rs }(),
		"Admins":    admins,
		"Operators": operators,
		"Viewers":   viewers,
	})
}

// activityActor is one option in the Activity page's "User" filter — value is
// the stable username the row is actually filtered/stored by, label is the
// user's current display name.
type activityActor struct {
	Username string
	Name     string
	Count    int // log entries by this actor (page views excluded)
}

// ActivityPage renders the admin-only activity log — every audit entry, optionally
// filtered to one user, so an admin can see everything at once or drill into one
// person's actions.
func (h *Handler) ActivityPage(w http.ResponseWriter, r *http.Request) {
	// actor is the username selected in the "User" filter dropdown (see below).
	actor := r.URL.Query().Get("actor")
	isAdmin := h.role(r) == "admin"
	// The "Show admin" and "Include page views" toggles are admin-only: everyone
	// else always sees the list without admin actions and without page views.
	showAdmin := isAdmin && r.URL.Query().Get("admin") == "1"
	excludeActor := ""
	if !showAdmin {
		excludeActor = "admin"
	}
	// Fetch broadly (everything but a straight exclude-admin) and apply the actor
	// filter AFTER resolving names below, rather than as a SQL exact-match on the
	// raw column. audit_log.actor is a mix of formats: current rows store a
	// username, but rows logged before a user had a name on file fell back to
	// storing the username too (same value, coincidentally matches), while rows
	// logged after they'd set a name stored that display-name text directly (a
	// pre-this-fix snapshot) — a raw exact-match against "the selected user's
	// username" only ever caught the first case, silently dropping the rest of
	// that person's own history. Matching on the resolved display name instead
	// catches all three shapes.
	showViews := isAdmin && r.URL.Query().Get("views") == "1"
	entries, err := h.db.ListAuditForActivity(r.Context(), excludeActor, showViews, 1000)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The actor filter only lists real team accounts (users with an @aioapp.com
	// email), not raw audit_log.actor noise like "System"/"API key"/scheduled-recipe
	// labels or stale entries from accounts that no longer exist.
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var actors []activityActor
	nameByUsername := make(map[string]string, len(users))
	counts, _ := h.db.AuditCountsByActor(r.Context())
	for _, u := range users {
		nameByUsername[u.Username] = u.DisplayName()
		if u.Email != nil && strings.HasSuffix(strings.ToLower(*u.Email), "@aioapp.com") {
			actors = append(actors, activityActor{Username: u.Username, Name: u.DisplayName(), Count: counts[u.Username]})
		}
	}
	// Most active first; the filter shows the top five and folds the rest (A–Z).
	sort.SliceStable(actors, func(i, j int) bool {
		if actors[i].Count != actors[j].Count {
			return actors[i].Count > actors[j].Count
		}
		return strings.ToLower(actors[i].Name) < strings.ToLower(actors[j].Name)
	})
	topActors, moreActors := actors, []activityActor(nil)
	if len(actors) > 5 {
		topActors, moreActors = actors[:5], append([]activityActor(nil), actors[5:]...)
		sort.SliceStable(moreActors, func(i, j int) bool { return strings.ToLower(moreActors[i].Name) < strings.ToLower(moreActors[j].Name) })
	}
	actorName := actor
	if dn, ok := nameByUsername[actor]; ok {
		actorName = dn
	}
	for i := range entries {
		if dn, ok := nameByUsername[entries[i].Actor]; ok {
			entries[i].Actor = dn
		}
	}
	if actor != "" {
		kept := make([]db.AuditEntry, 0, len(entries))
		for _, e := range entries {
			if e.Actor == actorName {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	// KPI strip: today / this-week counts, distinct users, most common action —
	// computed over the same (already actor-filtered) entry set the table shows.
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := todayStart.AddDate(0, 0, -6)
	entriesToday, entriesWeek := 0, 0
	distinctUsers := map[string]bool{}
	actionCounts := map[string]int{}
	for _, e := range entries {
		if !e.CreatedAt.Before(todayStart) {
			entriesToday++
		}
		if !e.CreatedAt.Before(weekStart) {
			entriesWeek++
		}
		distinctUsers[e.Actor] = true
		actionCounts[e.Action]++
	}
	topAction, topActionN := "—", 0
	for a, n := range actionCounts {
		if n > topActionN || (n == topActionN && a < topAction) {
			topAction, topActionN = a, n
		}
	}

	const pageSize = 40
	total := len(entries)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	var pageEntries []db.AuditEntry
	if total > 0 {
		pageEntries = entries[start:end]
	}

	h.render(w, r, "activity.html", map[string]any{
		"UsersTab":      "activity",
		"Entries":       pageEntries,
		"Total":         total,
		"Page":          page,
		"TotalPages":    totalPages,
		"Actors":        actors,
		"TopActors":     topActors,
		"MoreActors":    moreActors,
		"Actor":         actor,
		"ActorName":     actorName,
		"ShowAdmin":     showAdmin,
		"ShowViews":     showViews,
		"EntriesToday":  entriesToday,
		"EntriesWeek":   entriesWeek,
		"DistinctUsers": len(distinctUsers),
		"TopAction":     topAction,
		"TopActionN":    topActionN,
	})
}

// validUserRole reports whether role is a role an admin may assign to a DB user.
// "admin" is excluded — it is env-configured only, never a DB user.
func validUserRole(role string) bool {
	_, ok := roleLevels[role]
	return ok
}

func (h *Handler) UserCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The account's identity is its email — used as both username (login) and
	// email (forgot-password), same as self-signup. Pre-verified since the admin
	// is vouching for it directly; no confirmation email needed.
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	password := r.FormValue("password")
	role := r.FormValue("role")
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))

	if email == "" || password == "" || !validUserRole(role) {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	if !mayManageUser(h.role(r), "viewer", role) {
		http.Error(w, "You can only create accounts with a role below your own level.", http.StatusForbidden)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	created, err := h.db.CreateUserNamed(r.Context(), email, string(hash), role, &email, true, firstName, lastName)
	if err != nil {
		http.Error(w, "An account with that email already exists, or internal error", http.StatusBadRequest)
		return
	}
	// A restaurant owner sees exactly their venue(s): base deny, allow "view"
	// per selected restaurant, out-of-scope devices hidden.
	if role == "owner" {
		pol := db.AccessPolicy{Base: "deny", HideOutOfScope: true}
		if err := h.db.SetUserAccess(r.Context(), created.ID, pol); err != nil {
			log.Printf("[users] owner policy for %s: %v", created.Username, err)
		}
		for _, rid := range r.Form["restaurants"] {
			id, err := uuid.Parse(rid)
			if err != nil {
				continue
			}
			g := db.AccessGrant{UserID: created.ID, Effect: "allow", ScopeType: "restaurant", ScopeID: &id, Actions: []string{"view"}, Note: "owner's venue", CreatedBy: h.currentUsername(r)}
			if saved, err := h.db.AddAccessGrant(r.Context(), g); err != nil {
				log.Printf("[users] owner venue grant for %s: %v", created.Username, err)
			} else {
				h.audit(r, "user.access.grant", created.Username, describeGrant(*saved))
			}
		}
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserSetName lets an admin set/correct a user's first/last name — e.g. for
// accounts created before names existed, or to fix a typo the user can't self-serve.
func (h *Handler) UserSetName(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	r.ParseForm()
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))
	if err := h.db.UpdateUserName(r.Context(), id, firstName, lastName); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.usersRedirect(w, r)
}

func (h *Handler) UserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	if err := h.db.DeleteUser(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserSetRole changes an existing user's role. Admin is never assignable from
// the UI (the admin account comes from env), matching UserCreate.
func (h *Handler) UserSetRole(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	role := r.FormValue("role")
	if !validUserRole(role) {
		http.Error(w, "Invalid role", http.StatusBadRequest)
		return
	}
	if !mayManageUser(h.role(r), target.Role, role) {
		http.Error(w, "You can only assign roles below your own level.", http.StatusForbidden)
		return
	}
	if err := h.db.UpdateUserRole(r.Context(), id, role); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(target.Username)
	h.usersRedirect(w, r)
}

// UserSetPassword resets a team user's password (admin-only). The built-in admin
// account's password comes from the environment and can't be changed here.
func (h *Handler) UserSetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	password := r.FormValue("password")
	if len(password) < 6 {
		http.Error(w, "Password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserPassword(r.Context(), id, string(hash)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.password_reset", id.String(), "")
	h.usersRedirect(w, r)
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// post registers a state-changing route behind the same-origin (CSRF) guard.
	// Use it for every POST so cross-site requests can't drive an authenticated
	// browser session; GET routes stay on mux.HandleFunc directly.
	post := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, h.enforceSameOrigin(handler))
	}

	mux.HandleFunc("GET /login", h.LoginPage)
	post("POST /login", h.LoginSubmit)
	post("POST /logout", h.Logout)
	mux.HandleFunc("GET /signup", h.SignupPage)
	post("POST /signup", h.SignupSubmit)
	mux.HandleFunc("GET /auth/microsoft", h.MicrosoftLoginStart)
	mux.HandleFunc("GET /auth/microsoft/callback", h.MicrosoftLoginCallback)
	mux.HandleFunc("GET /verify-email", h.VerifyEmailPage)
	post("POST /verify-email", h.VerifyEmailSubmit)
	mux.HandleFunc("GET /forgot-password", h.ForgotPasswordPage)
	post("POST /forgot-password", h.ForgotPasswordSubmit)
	mux.HandleFunc("GET /reset-password", h.ResetPasswordPage)
	post("POST /reset-password", h.ResetPasswordSubmit)

	mux.HandleFunc("GET /sneak-peek", h.SneakPeek)
	mux.HandleFunc("GET /sneak-peek/alerts", h.SneakPeekAlerts)
	mux.HandleFunc("GET /sneak-peek/fleet", h.SneakPeekFleet)

	mux.HandleFunc("GET /{$}", h.Root)
	mux.HandleFunc("GET /devices", h.requireAuth(h.DeviceList))
	mux.HandleFunc("GET /devices/select-serials", h.requireAuth(h.DeviceSelectAllSerials))
	mux.HandleFunc("GET /devices/search", h.requireAuth(h.DeviceSearch))
	mux.HandleFunc("GET /cmdk-index", h.requireAuth(h.CmdkIndex))
	mux.HandleFunc("GET /demo", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/demo/index", http.StatusFound)
	}))
	mux.HandleFunc("GET /demos", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/demo/index", http.StatusFound)
	}))
	// Working target-picker prototype (real fleet data + live counts), plus the static demos.
	mux.HandleFunc("GET /demo/target-live", h.requireAuth(h.TargetLivePage))
	mux.HandleFunc("GET /demo/target-count", h.requireAuth(h.TargetCountJSON))
	// Internal presenter material (system audit + demo runbook): admin/dev only, and
	// registered as explicit routes so they take precedence over /demo/{n} — a viewer
	// or external guest with a login can't reach the candid readiness/security content.
	mux.HandleFunc("GET /demo/audit", h.requireAdmin(h.InternalPage))
	mux.HandleFunc("GET /demo/runbook", h.requireAdmin(h.InternalPage))
	mux.HandleFunc("GET /demo/{n}", h.requireAuth(h.DemoPage))
	mux.HandleFunc("GET /events/devices", h.requireAuth(h.FleetEvents))
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.deviceRoute("view", h.DeviceDetail)))
	mux.HandleFunc("GET /devices/{serial}/history", h.requireAuth(h.deviceRoute("view", h.DeviceHistory)))
	mux.HandleFunc("GET /devices/{serial}/chart-data", h.requireAuth(h.deviceRoute("view", h.DeviceChartData)))
	mux.HandleFunc("GET /devices/{serial}/events", h.requireAuth(h.deviceRoute("view", h.DeviceEvents)))
	mux.HandleFunc("GET /devices/{serial}/ws-status", h.requireAuth(h.deviceRoute("view", h.DeviceOnlineStatus)))
	mux.HandleFunc("GET /devices/{serial}/presence-stream", h.requireAuth(h.deviceRoute("view", h.DevicePresenceStream)))
	mux.HandleFunc("GET /devices/{serial}/stats", h.requireAuth(h.deviceRoute("view", h.DeviceStatsPartial)))
	mux.HandleFunc("GET /devices/{serial}/vitals", h.requireAuth(h.deviceRoute("view", h.DeviceVitalsPartial)))
	mux.HandleFunc("GET /devices/{serial}/panel", h.requireAuth(h.deviceRoute("view", h.DeviceInspectorPanel)))
	mux.HandleFunc("GET /devices/{serial}/battery.csv", h.requireAuth(h.deviceRoute("view", h.DeviceBatteryCSV)))
	mux.HandleFunc("GET /devices/{serial}/daily-stats", h.requireAuth(h.deviceRoute("view", h.DeviceDailyStatsJSON)))
	post("POST /devices/{serial}/ai-analysis", h.requireAuth(h.deviceRoute("query", h.DeviceAIAnalysis)))
	mux.HandleFunc("GET /devices/{serial}/ai-analyses", h.requireAuth(h.deviceRoute("view", h.DeviceAIAnalysesList)))
	mux.HandleFunc("GET /devices/{serial}/shell", h.requireReleaseAdmin(h.deviceRoute("shell", h.DeviceShellPage)))
	mux.HandleFunc("GET /devices/{serial}/commands-status", h.requireAuth(h.deviceRoute("view", h.DeviceCommandsPartial)))
	mux.HandleFunc("GET /devices/{serial}/alerts-panel", h.requireAuth(h.deviceRoute("view", h.DeviceAlertsPanel)))
	mux.HandleFunc("GET /devices/{serial}/queue", h.requireAuth(h.deviceRoute("view", h.DeviceQueuePartial)))
	post("POST /devices/{serial}/queue/clear", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceQueueClear)))
	post("POST /devices/{serial}/queue/{id}/remove", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceQueueRemove)))
	mux.HandleFunc("GET /devices/{serial}/installs", h.requireAuth(h.deviceRoute("view", h.DeviceInstallProgress)))
	post("POST /devices/{serial}/installs/cancel", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceInstallCancel)))
	post("POST /devices/{serial}/commands", h.requireAuth(h.deviceRoute("view", h.DeviceCommandCreate)))
	post("POST /devices/{serial}/poll-interval", h.requireAdmin(h.deviceRoute("view", h.DeviceSetPollInterval)))
	post("POST /devices/{serial}/move-server", h.requireStrictAdmin(h.deviceRoute("shell", h.DeviceMoveServer)))
	post("POST /devices/{serial}/notes", h.requireOperatorOrAdmin(h.deviceRoute("notes", h.DeviceNotesUpdate)))
	post("POST /devices/{serial}/nickname", h.requireOperatorOrAdmin(h.deviceRoute("notes", h.DeviceSetNickname)))
	post("POST /devices/{serial}/kiosk", h.requireAdminOrOperator(h.deviceRoute("kiosk", h.DeviceKioskUpdate)))
	post("POST /devices/{serial}/wlc", h.requireAdminOrOperator(h.deviceRoute("kiosk", h.DeviceWlcUpdate)))
	post("POST /devices/{serial}/offline-code/rotate", h.requireAdmin(h.deviceRoute("kiosk", h.DeviceRotateOfflineCode)))
	mux.HandleFunc("GET /devices/{serial}/offline-code", h.requireOperatorOrAdmin(h.deviceRoute("kiosk", h.DeviceOfflineCode)))
	post("POST /devices/{serial}/hide", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceHide)))
	post("POST /devices/{serial}/unhide", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceUnhide)))
	post("POST /devices/{serial}/clear-ota", h.requireAdmin(h.deviceRoute("view", h.DeviceClearOTA)))
	// Remote screen capture + input injection is highly sensitive (full control of the
	// device), so it is restricted to admins only.
	mux.HandleFunc("GET /devices/{serial}/remote", h.requireAdminOrOperator(h.deviceRoute("remote", h.DeviceRemote)))
	mux.HandleFunc("GET /devices/{serial}/remote/token", h.requireAdminOrOperator(h.deviceRoute("remote", h.DeviceRemoteToken)))
	post("POST /devices/bulk-hide", h.requireStrictAdmin(h.BulkHideDevices))
	post("POST /devices/bulk-unhide", h.requireStrictAdmin(h.BulkUnhideDevices))
	post("POST /devices/bulk-restaurant", h.requireAdminOrOperator(h.BulkAssignRestaurant))
	post("POST /devices/bulk-nickname", h.requireAdminOrOperator(h.BulkNickname))
	post("POST /devices/bulk-class", h.requireStrictAdmin(h.BulkClass))
	post("POST /devices/bulk-retire", h.requireStrictAdmin(h.BulkRetire))
	post("POST /devices/bulk-kiosk", h.requireAdminOrOperator(h.BulkKioskUpdate))
	post("POST /devices/bulk-kiosk-apps", h.requireAdminOrOperator(h.BulkKioskApps))
	mux.HandleFunc("GET /export", h.requireAuth(h.ExportPage))
	post("POST /export/csv", h.requireAuth(h.ExportCSV))
	mux.HandleFunc("GET /export/report/inventory.csv", h.requireAuth(h.ReportInventoryCSV))
	mux.HandleFunc("GET /export/report/compliance.csv", h.requireAuth(h.ReportComplianceCSV))
	mux.HandleFunc("GET /export/report/activity.csv", h.requireAuth(h.ReportActivityCSV))
	// Admin-only for now (see the Overview card's same gate) — loosen to
	// requireAuth if this opens up to other roles later.
	mux.HandleFunc("GET /export/visualize", h.requireAuth(h.ExportVisualizePage))
	mux.HandleFunc("GET /devices/{serial}/packages", h.requireAuth(h.deviceRoute("view", h.DevicePackages)))
	mux.HandleFunc("GET /devices/{serial}/apps-list", h.requireAuth(h.deviceRoute("view", h.DeviceAppsList)))
	mux.HandleFunc("GET /devices/{serial}/ota-progress", h.requireAuth(h.deviceRoute("view", h.DeviceOtaProgress)))
	mux.HandleFunc("GET /packages", h.requireStrictAdmin(h.FleetPackages))
	post("POST /packages/flag", h.requireStrictAdmin(h.PackageFlag))
	mux.HandleFunc("GET /devices/{serial}/logcat/live", h.requireAuth(h.deviceRoute("logcat", h.LogcatLivePage)))
	mux.HandleFunc("GET /devices/{serial}/logcat/stream", h.requireAuth(h.deviceRoute("logcat", h.LogcatStream)))

	mux.HandleFunc("GET /groups/new", h.requireAdminOrOperator(h.GroupNew))
	mux.HandleFunc("GET /groups/new/devices", h.requireAdminOrOperator(h.GroupNewDevices))
	mux.HandleFunc("GET /groups", h.requireAuth(h.GroupList))
	post("POST /groups", h.requireAdminOrOperator(h.GroupCreate))
	mux.HandleFunc("GET /groups/{id}", h.requireAuth(h.GroupDetail))
	mux.HandleFunc("GET /groups/{id}/devices-modal", h.requireAdminOrOperator(h.GroupDevicesModal))
	mux.HandleFunc("GET /groups/{id}/device-search", h.requireAuth(h.GroupDeviceSearch))
	mux.HandleFunc("GET /groups/{id}/members", h.requireAuth(h.GroupMembers))
	mux.HandleFunc("GET /groups/{id}/daily-stats", h.requireAuth(h.GroupDailyStatsJSON))
	mux.HandleFunc("GET /fleet-health", h.requireAuth(h.FleetHealth))
	mux.HandleFunc("GET /map", h.requireAuth(h.MapPage))
	mux.HandleFunc("GET /devices/map.json", h.requireAuth(h.DeviceMapData))
	post("POST /overview/layout", h.requireAuth(h.OverviewLayoutSave))
	post("POST /overview/layout/reset", h.requireAuth(h.OverviewLayoutReset))
	post("POST /tour/done", h.requireAuth(h.TourDone))
	post("POST /tour/reset", h.requireAuth(h.TourReset))
	mux.HandleFunc("GET /sw.js", h.ServiceWorker)
	mux.HandleFunc("GET /reports/alerts-by-restaurant", h.requireAuth(h.ReportAlertsByRestaurant))
	mux.HandleFunc("GET /alerts/newest", h.requireAuth(h.AlertNewest))
	post("POST /ai-summary/refresh", h.requireAuth(h.AISummaryRefresh))
	mux.HandleFunc("GET /alerts", h.requireAuth(h.AlertList))
	mux.HandleFunc("GET /alert-config", h.requireAdmin(h.AlertConfigView))
	// Requires auth: the page exposes real device serials and restaurant/venue names,
	// so it must not be anonymous even though it's a standalone "wrapped" page.
	mux.HandleFunc("GET /wrapped", h.requireAuth(h.WrappedPage))
	mux.HandleFunc("GET /alerts/recent", h.requireAuth(h.AlertsRecent))
	mux.HandleFunc("GET /alerts/events", h.requireAuth(h.AlertEvents))
	post("POST /alerts/bulk", h.requireOperatorOrAdmin(h.AlertBulk))
	post("POST /alerts/ack-all", h.requireOperatorOrAdmin(h.AlertAckAll))
	post("POST /alerts/resolve-all", h.requireOperatorOrAdmin(h.AlertResolveAll))
	post("POST /alerts/clear-all", h.requireAdmin(h.AlertClearAll))
	post("POST /alerts/{id}/ack", h.requireOperatorOrAdmin(h.AlertAck))
	post("POST /alerts/{id}/resolve", h.requireOperatorOrAdmin(h.AlertResolve))
	post("POST /groups/{id}", h.requireAdminOrOperator(h.GroupUpdate))
	post("POST /groups/{id}/delete", h.requireAdminOrOperator(h.GroupDelete))
	post("POST /groups/{id}/devices", h.requireAdminOrOperator(h.GroupAddDevice))

	// Restaurants (venue object). Static sub-paths registered before /{id}.
	mux.HandleFunc("GET /restaurants/new", h.requireAdminOrOperator(h.RestaurantNew))
	mux.HandleFunc("GET /restaurants/new/device-picker", h.requireAdminOrOperator(h.RestaurantNewDevices))
	mux.HandleFunc("GET /restaurants", h.requireAuth(h.RestaurantList))
	post("POST /restaurants", h.requireAdminOrOperator(h.RestaurantCreate))
	mux.HandleFunc("GET /restaurants/{id}", h.requireAuth(h.RestaurantDetail))
	mux.HandleFunc("GET /restaurants/{id}/edit", h.requireAdminOrOperator(h.RestaurantEdit))
	mux.HandleFunc("GET /restaurants/{id}/devices-modal", h.requireAdminOrOperator(h.RestaurantDevicesModal))
	mux.HandleFunc("GET /restaurants/{id}/daily-stats", h.requireAuth(h.RestaurantDailyStatsJSON))
	mux.HandleFunc("GET /restaurants/{id}/members", h.requireAuth(h.RestaurantMembers))
	mux.HandleFunc("GET /restaurants/{id}/report", h.requireAuth(h.RestaurantReport))
	post("POST /restaurants/{id}/report/email", h.requireAdminOrOperator(h.RestaurantReportEmail))
	post("POST /restaurants/{id}", h.requireAdminOrOperator(h.RestaurantUpdate))
	post("POST /restaurants/{id}/rename", h.requireAdminOrOperator(h.RestaurantRename))
	post("POST /restaurants/{id}/delete", h.requireAdminOrOperator(h.RestaurantDelete))
	mux.HandleFunc("GET /restaurants/{id}/device-picker", h.requireAdminOrOperator(h.RestaurantDevicePicker))
	post("POST /restaurants/{id}/devices", h.requireAdminOrOperator(h.RestaurantAssignDevices))
	post("POST /restaurants/{id}/devices/{serial}/remove", h.requireAdminOrOperator(h.RestaurantRemoveDevice))
	post("POST /restaurants/{id}/service-window", h.requireAdminOrOperator(h.RestaurantSetServiceWindow))
	post("POST /devices/{serial}/restaurant", h.requireAdmin(h.deviceRoute("view", h.DeviceSetRestaurant)))
	post("POST /groups/{id}/devices/remove", h.requireAdminOrOperator(h.GroupBulkRemoveDevice))
	post("POST /groups/{id}/devices/{serial}/remove", h.requireAdminOrOperator(h.GroupRemoveDevice))
	post("POST /groups/{id}/commands", h.requireAdminOrOperator(h.GroupCommandCreate))

	// Productions is owned by the test team: admin/dev/operator can list, view,
	// create and export (requireAdminOrOperator). Deletion stays admin/dev-only.
	mux.HandleFunc("GET /productions", h.requireAdminOrOperator(h.ProductionList))
	mux.HandleFunc("GET /productions/new", h.requireAdminOrOperator(h.ProductionNew))
	post("POST /productions", h.requireAdminOrOperator(h.ProductionCreate))
	mux.HandleFunc("GET /productions/preview-serial", h.requireAdminOrOperator(h.ProductionPreviewSerial))
	mux.HandleFunc("GET /productions/{id}", h.requireAdminOrOperator(h.ProductionDetail))
	mux.HandleFunc("GET /productions/{id}/export.csv", h.requireAdminOrOperator(h.ProductionExportCSV))
	post("POST /productions/{id}/delete", h.requireAdmin(h.ProductionDelete))

	mux.HandleFunc("GET /commands", h.requireAuth(h.CommandList))
	mux.HandleFunc("GET /manage", h.requireAuth(h.Manage))
	// Enrollment is admin-only: the page, its QR codes and every profile mutation.
	mux.HandleFunc("GET /enrollment", h.requireStrictAdmin(h.EnrollmentPage))
	mux.HandleFunc("GET /enrollment/profiles/{id}/qr.png", h.requireStrictAdmin(h.EnrollmentProfileQR))
	post("POST /enrollment/profiles", h.requireStrictAdmin(h.EnrollmentProfileCreate))
	post("POST /enrollment/profiles/{id}/revoke", h.requireStrictAdmin(h.EnrollmentProfileRevoke))
	post("POST /enrollment/profiles/{id}/activate", h.requireStrictAdmin(h.EnrollmentProfileActivate))
	post("POST /enrollment/profiles/{id}/delete", h.requireStrictAdmin(h.EnrollmentProfileDelete))
	post("POST /enrollment/profiles/{id}/update", h.requireStrictAdmin(h.EnrollmentProfileUpdate))
	// Device lifecycle: onboarding inbox confirmation, class override, retire/unretire.
	post("POST /devices/{serial}/onboard", h.requireOperatorOrAdmin(h.deviceRoute("notes", h.DeviceOnboard)))
	post("POST /devices/{serial}/class", h.requireOperatorOrAdmin(h.deviceRoute("notes", h.DeviceSetClass)))
	post("POST /devices/{serial}/retire", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceRetire)))
	post("POST /devices/{serial}/unretire", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceUnretire)))
	mux.HandleFunc("GET /updates-policy", h.requireAuth(h.UpdatesPolicyPage))
	post("POST /updates-policy", h.requireStrictAdmin(h.UpdatesPolicySave))
	mux.HandleFunc("GET /compliance", h.requireAuth(h.CompliancePage))
	post("POST /compliance/rules", h.requireAdminOrOperator(h.ComplianceRuleCreate))
	post("POST /compliance/rules/{id}/toggle", h.requireAdminOrOperator(h.ComplianceRuleToggle))
	post("POST /compliance/rules/{id}/delete", h.requireAdminOrOperator(h.ComplianceRuleDelete))
	// Remediation queues real device commands, so it stays admin-only (the
	// template hides the button for everyone else).
	post("POST /compliance/remediate/{serial}", h.requireStrictAdmin(h.ComplianceRemediate))
	mux.HandleFunc("GET /geofencing", h.requireAuth(h.GeofencingPage))
	post("POST /geofencing/location-toggle", h.requireAdminOrOperator(h.GeofencingLocationToggle))
	post("POST /geofencing/fences", h.requireAdminOrOperator(h.GeofenceCreate))
	post("POST /geofencing/fences/{id}/delete", h.requireAdminOrOperator(h.GeofenceDelete))
	mux.HandleFunc("GET /network", h.requireAuth(h.NetworkPage))
	post("POST /network/wifi", h.requireStrictAdmin(h.NetworkWifiAdd))
	post("POST /network/wifi/delete", h.requireStrictAdmin(h.NetworkWifiDelete))
	post("POST /network/ca", h.requireStrictAdmin(h.NetworkCAAdd))
	post("POST /network/ca/delete", h.requireStrictAdmin(h.NetworkCADelete))
	post("POST /network/vpn", h.requireStrictAdmin(h.NetworkVPNSave))
	mux.HandleFunc("GET /manage/policies/new", h.requireAuth(h.ManagePolicyNew))
	mux.HandleFunc("GET /manage/policies/{id}/edit", h.requireAuth(h.ManagePolicyEditPage))
	mux.HandleFunc("POST /manage/policies", h.requireAuth(h.ManagePolicySave))
	mux.HandleFunc("POST /manage/policies/{id}/duplicate", h.requireAuth(h.ManagePolicyDuplicate))
	mux.HandleFunc("POST /manage/policies/{id}/delete", h.requireAuth(h.ManagePolicyDelete))
	mux.HandleFunc("GET /commands/browse-devices", h.requireAuth(h.CommandBrowseDevices))
	mux.HandleFunc("GET /commands/resolve-serials", h.requireAuth(h.CommandResolveSerials))
	mux.HandleFunc("GET /commands/history", h.requireAuth(h.CommandHistory))
	// Static route wins over /commands/{id}, so this is the global live feed the
	// history page subscribes to (not a per-command stream).
	mux.HandleFunc("GET /commands/events", h.requireAuth(h.CommandsFeedEvents))
	post("POST /commands/clear-attention", h.requireOperatorOrAdmin(h.AttentionClear))
	mux.HandleFunc("GET /commands/impact", h.requireAuth(h.CommandImpact))
	mux.HandleFunc("GET /commands/target-packages", h.requireAuth(h.CommandTargetPackages))
	// Recipes live under /recipes (not /commands/recipes) so the {id} delete route
	// doesn't collide with /commands/{id}/resend/{serial} in the wildcard mux.
	post("POST /recipes", h.requireAuth(h.RecipeCreate))
	post("POST /recipes/{id}/delete", h.requireAdmin(h.RecipeDelete))
	mux.HandleFunc("GET /schedules", h.requireAuth(h.ScheduleList))
	post("POST /schedules", h.requireOperatorOrAdmin(h.ScheduleCreate))
	post("POST /schedules/{id}/delete", h.requireOperatorOrAdmin(h.ScheduleDelete))
	post("POST /schedules/{id}/toggle", h.requireOperatorOrAdmin(h.ScheduleToggle))
	post("POST /schedules/{id}/run-now", h.requireOperatorOrAdmin(h.ScheduleRunNow))
	post("POST /commands", h.requireAuth(h.CommandCreate))
	post("POST /alerts/{id}/logcat/analyze", h.requireAuth(h.AlertLogcatAnalyze))
	mux.HandleFunc("GET /commands/{id}", h.requireAuth(h.CommandDetail))
	mux.HandleFunc("GET /commands/{id}/screenshot/{serial}", h.requireAuth(h.CommandScreenshot))
	mux.HandleFunc("GET /commands/{id}/status", h.requireAuth(h.CommandStatusPartial))
	mux.HandleFunc("GET /commands/{id}/events", h.requireAuth(h.CommandEvents))
	post("POST /commands/{id}/delete", h.requireOperatorOrAdmin(h.CommandDelete))
	post("POST /commands/{id}/resend", h.requireAuth(h.CommandResendAll))
	post("POST /commands/{id}/resend/{serial}", h.requireAuth(h.CommandResendDevice))

	mux.HandleFunc("GET /boot-logo", h.requireStrictAdmin(h.BootLogo))
	mux.HandleFunc("GET /settings", h.requireStrictAdmin(h.SettingsPage))
	mux.HandleFunc("GET /settings/google-usage", h.requireStrictAdmin(h.GoogleUsageJSON))
	post("POST /settings/columns/add", h.requireStrictAdmin(h.SettingsAddColumn))
	post("POST /settings/columns/{key}/remove", h.requireStrictAdmin(h.SettingsRemoveColumn))
	post("POST /settings/legacy-builds/add", h.requireStrictAdmin(h.SettingsAddLegacyBuild))
	post("POST /settings/legacy-builds/remove", h.requireStrictAdmin(h.SettingsRemoveLegacyBuild))
	post("POST /settings/legacy-checkin/toggle", h.requireStrictAdmin(h.SettingsToggleLegacyCheckin))
	post("POST /settings/ignore-dpc/toggle", h.requireStrictAdmin(h.SettingsToggleIgnoreDPC))
	post("POST /settings/shell/toggle", h.requireStrictAdmin(h.SettingsToggleShell))
	post("POST /settings/remote/toggle", h.requireStrictAdmin(h.SettingsToggleRemote))
	post("POST /settings/command-expiry", h.requireStrictAdmin(h.SettingsSetCommandExpiry))
	post("POST /settings/max-targets", h.requireStrictAdmin(h.SettingsSetMaxTargets))
	post("POST /settings/queries", h.requireStrictAdmin(h.SettingsQueryCreate))
	post("POST /settings/queries/{id}/edit", h.requireStrictAdmin(h.SettingsQueryEdit))
	post("POST /settings/queries/{id}/delete", h.requireStrictAdmin(h.SettingsQueryDelete))
	post("POST /settings/queries/{id}/toggle", h.requireStrictAdmin(h.SettingsQueryToggle))
	mux.HandleFunc("GET /changelog", h.requireAuth(h.Changelog))
	mux.HandleFunc("GET /changelog/latest", h.requireAuth(h.ChangelogLatest))
	post("POST /settings/require-reason", h.requireStrictAdmin(h.SettingsToggleRequireReason))
	post("POST /settings/agent-apk", h.requireStrictAdmin(h.SettingsAgentAPK))
	post("POST /settings/agent-apk/upload", h.requireStrictAdmin(h.SettingsAgentAPKUpload))
	post("POST /settings/agent-apk/remove", h.requireStrictAdmin(h.SettingsAgentAPKRemove))
	// Public on purpose: a factory-reset phone downloads the agent from the QR.
	mux.HandleFunc("GET /agent/skorra-agent.apk", h.AgentAPKDownload)
	post("POST /settings/maintenance", h.requireStrictAdmin(h.SettingsToggleMaintenance))
	post("POST /settings/legacy-strip-done", h.requireStrictAdmin(h.SettingsLegacyStripDone))
	mux.HandleFunc("GET /maintenance", h.MaintenancePage)
	post("POST /settings/dashboard", h.requireStrictAdmin(h.SettingsSetDashboard))
	post("POST /settings/kiosk-allowlist", h.requireStrictAdmin(h.SettingsSetKioskAllowlist))
	post("POST /settings/ota-support", h.requireStrictAdmin(h.SettingsSetOTASupport))
	post("POST /settings/mdm-servers", h.requireStrictAdmin(h.SettingsSetMDMServers))
	post("POST /settings/ota-min-release", h.requireStrictAdmin(h.SettingsSetOTAMinRelease))
	post("POST /settings/legacy-ota", h.requireStrictAdmin(h.SettingsSetLegacyOTA))
	post("POST /settings/ota-config", h.requireStrictAdmin(h.SettingsSetOTAConfig))
	post("POST /settings/alert-webhook", h.requireStrictAdmin(h.SettingsSetAlertWebhook))
	post("POST /settings/alert-rules/{id}", h.requireStrictAdmin(h.SettingsUpdateAlertRule))
	post("POST /settings/service-window", h.requireStrictAdmin(h.SettingsSetServiceWindow))
	post("POST /settings/alert-channels", h.requireStrictAdmin(h.SettingsSaveChannel))
	post("POST /settings/alert-channels/{id}/test", h.requireStrictAdmin(h.SettingsTestChannel))
	post("POST /settings/alert-channels/{id}/delete", h.requireStrictAdmin(h.SettingsDeleteChannel))
	post("POST /settings/ai", h.requireStrictAdmin(h.SettingsSetAI))
	post("POST /settings/retention", h.requireStrictAdmin(h.SettingsSetRetention))
	post("POST /settings/session-timeout", h.requireStrictAdmin(h.SettingsSetSessionTimeout))
	post("POST /settings/logout-all", h.requireStrictAdmin(h.SettingsLogoutAll))
	post("POST /settings/checkin-interval", h.requireStrictAdmin(h.SettingsSetCheckinInterval))
	post("POST /settings/wlc-products", h.requireStrictAdmin(h.SettingsSetWlcProducts))

	mux.HandleFunc("GET /setup", h.requireAdmin(h.SetupPage))
	// Managed app configurations edit the fleet device_policy, so mutations are
	// strict-admin like the other policy pages; the page itself is admin/dev.
	mux.HandleFunc("GET /setup/managed-configs", h.requireAdmin(h.ManagedConfigsPage))
	post("POST /setup/managed-configs", h.requireStrictAdmin(h.ManagedConfigSave))
	post("POST /setup/managed-configs/delete", h.requireStrictAdmin(h.ManagedConfigDelete))
	post("POST /setup/apps", h.requireAdmin(h.SetupCreateApp))
	post("POST /setup/apps/create", h.requireAdmin(h.SetupCreateAppJSON))
	// S3 APK uploads: presigned direct-to-S3 upload + register + device download proxy.
	mux.HandleFunc("GET /apps", h.requireAppLibrary(h.AppLibraryPage))
	post("POST /apps/upload-url", h.requireAppLibrary(h.AppUploadURL))
	post("POST /apps/register", h.requireAppLibrary(h.AppRegister))
	post("POST /apps/families", h.requireAppLibrary(h.AppFamilyCreate))
	post("POST /apps/families/{id}/rename", h.requireAppLibrary(h.AppFamilyRename))
	post("POST /apps/families/{id}/ungroup", h.requireAppLibrary(h.AppFamilyUngroup))
	post("POST /apps/package/assign", h.requireAppLibrary(h.AppPackageAssign))
	post("POST /settings/app-family-mode", h.requireStrictAdmin(h.SettingsSetAppFamilyMode))
	mux.HandleFunc("GET /apps/{id}/apk", h.AppAPK) // open: device installer fetches by URL
	post("POST /setup/apps/{id}/edit", h.requireAdmin(h.SetupUpdateApp))
	post("POST /setup/apps/{id}/delete", h.requireAdmin(h.SetupDeleteApp)) // removing an APK stays admin-only

	mux.HandleFunc("GET /releases", h.requireAdminOrOperator(h.ReleaseList))
	mux.HandleFunc("GET /releases/events", h.requireAdminOrOperator(h.ReleaseProblemEvents))
	post("POST /releases", h.requireReleaseAdmin(h.ReleaseCreate))
	mux.HandleFunc("GET /releases/{id}", h.requireAdminOrOperator(h.ReleaseDetail))
	post("POST /releases/{id}/packages", h.requireReleaseAdmin(h.ReleaseAddPackage))
	post("POST /releases/{id}/packages/inspect", h.requireReleaseAdmin(h.PackageInspect))
	mux.HandleFunc("GET /releases/{id}/crashes", h.requireAdminOrOperator(h.ReleaseCrashes))
	post("POST /releases/{id}/crashes/group/delete", h.requireReleaseAdmin(h.ReleaseCrashGroupDelete))
	post("POST /releases/{id}/crashes/{eid}/delete", h.requireReleaseAdmin(h.ReleaseCrashDelete))
	post("POST /releases/{id}/packages/{pid}/delete", h.requireReleaseAdmin(h.PackageDelete))
	post("POST /releases/{id}/qfil", h.requireReleaseAdmin(h.ReleaseAddQFIL))
	post("POST /releases/{id}/dev", h.requireReleaseAdmin(h.ReleaseSetDev))
	post("POST /releases/{id}/qfil/{qid}/delete", h.requireReleaseAdmin(h.ReleaseDeleteQFIL))
	post("POST /releases/track", h.requireReleaseAdmin(h.ReleaseTrack))
	post("POST /releases/order", h.requireReleaseAdmin(h.ReorderVersions))
	post("POST /releases/version/hide", h.requireReleaseAdmin(h.VersionHide))
	post("POST /releases/version/unhide", h.requireReleaseAdmin(h.VersionUnhide))
	post("POST /releases/{id}/meta", h.requireReleaseAdmin(h.ReleaseEditMeta))
	post("POST /releases/{id}/rename", h.requireReleaseAdmin(h.ReleaseRename))
	post("POST /releases/{id}/hide", h.requireReleaseAdmin(h.ReleaseSetHidden))
	post("POST /releases/{id}/delete", h.requireReleaseAdmin(h.ReleaseDelete))
	post("POST /releases/{id}/publish", h.requireReleaseAdmin(h.ReleasePublish))
	mux.HandleFunc("GET /updates", h.requireAdminOrOperator(h.UpdatesHub))
	mux.HandleFunc("GET /updates/rollouts", h.requireAdminOrOperator(h.UpdatesRollouts))
	mux.HandleFunc("GET /updates/new", h.requireOTA(h.NewUpdatePage))
	mux.HandleFunc("GET /updates/legacy", h.requireAdminOrOperator(h.LegacyOTAPage))
	mux.HandleFunc("GET /updates/legacy/deployments/{id}", h.requireAdminOrOperator(h.LegacyDeploymentPage))
	post("POST /updates/legacy/push", h.requireOTA(h.LegacyOTAPush))
	post("POST /updates/legacy/deployments/{id}/cancel", h.requireOTA(h.LegacyOTACancel))
	post("POST /updates/legacy/deployments/{id}/devices/{serial}/retry", h.requireOTA(h.LegacyOTARetry))
	post("POST /updates/legacy/deployments/{id}/devices/{serial}/reboot", h.requireOTA(h.LegacyOTAReboot))
	post("POST /updates", h.requireOTA(h.DeployCreate))
	post("POST /releases/{id}/deploy", h.requireOTA(h.ReleaseDeploy))
	post("POST /releases/{id}/sign-off", h.requireDev(h.ReleaseSignOff))
	post("POST /releases/{id}/sign-off/clear", h.requireDev(h.ReleaseClearSignOff))
	post("POST /releases/{id}/branch", h.requireReleaseAdmin(h.ReleaseCreateBranch))
	post("POST /releases/{id}/merge", h.requireReleaseAdmin(h.ReleaseMerge))
	// Problem reports: any operator/operator can file and triage; admins can delete.
	post("POST /releases/{id}/problems", h.requireOperatorOrAdmin(h.ReleaseProblemCreate))
	post("POST /releases/{id}/problems/{pid}", h.requireOperatorOrAdmin(h.ReleaseProblemUpdate))
	post("POST /releases/{id}/problems/{pid}/delete", h.requireReleaseAdmin(h.ReleaseProblemDelete))

	// Test team / QA — base cases are managed inline on the Releases page (admin),
	// operators mark results per-release. No standalone Testing/Test-cases pages.
	post("POST /test-cases", h.requireReleaseAdmin(h.TestCaseCreate))
	post("POST /test-cases/{id}/edit", h.requireReleaseAdmin(h.TestCaseUpdate))
	post("POST /test-cases/{id}/delete", h.requireReleaseAdmin(h.TestCaseDelete))
	mux.HandleFunc("GET /demo/updates", h.requireAdminOrOperator(h.DemoUpdatesIndex))
	mux.HandleFunc("GET /demo/updates/{scenario}", h.requireAdminOrOperator(h.DemoUpdatesScenario))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}", h.requireAdminOrOperator(h.DeploymentDetail))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}/events", h.requireAdminOrOperator(h.DeploymentEvents))
	post("POST /releases/{id}/deployments/{did}/settings", h.requireOTA(h.DeploymentUpdateSettings))
	post("POST /releases/{id}/deployments/{did}/cancel", h.requireOperatorOrAdmin(h.DeploymentCancel))
	post("POST /releases/{id}/deployments/{did}/add-targets", h.requireOTA(h.DeploymentAddTargets))
	post("POST /releases/{id}/deployments/{did}/reboot-all", h.requireOperatorOrAdmin(h.DeploymentRebootAll))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/reboot", h.requireOperatorOrAdmin(h.DeploymentRebootDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/retry", h.requireOTA(h.DeploymentRetryDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/cancel-ota", h.requireOTA(h.DeploymentCancelDeviceOTA))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/remove", h.requireOTA(h.DeploymentRemoveDevice))
	post("POST /releases/{id}/deployments/{did}/delete", h.requireOTA(h.DeploymentDelete))

	mux.HandleFunc("GET /activity", h.requireUserManager(h.ActivityPage))
	mux.HandleFunc("GET /users", h.requireUserManager(h.UserList))
	post("POST /users", h.requireAccountAdmin(h.UserCreate))
	post("POST /users/{id}/role", h.requireUserManager(h.UserSetRole))
	post("POST /users/{id}/name", h.requireUserManager(h.UserSetName))
	mux.HandleFunc("GET /users/{id}/avatar.png", h.requireAuth(h.UserAvatar))
	post("POST /users/{id}/avatar", h.requireAuth(h.UserSetAvatar))
	post("POST /users/{id}/avatar/delete", h.requireAuth(h.UserClearAvatar))
	post("POST /users/{id}/password", h.requireUserManager(h.UserSetPassword))
	post("POST /users/{id}/delete", h.requireAccountAdmin(h.UserDelete))
	post("POST /users/merge", h.requireAccountAdmin(h.UserMergeActor))
	mux.HandleFunc("GET /profile", h.requireAuth(h.ProfilePage))
	mux.HandleFunc("GET /users/{id}/profile", h.requireAuth(h.UserProfilePage))
	mux.HandleFunc("GET /users/access", h.requireUserManager(h.UsersAccessPage))
	mux.HandleFunc("GET /users/access/scope-search", h.requireUserManager(h.AccessScopeSearch))
	mux.HandleFunc("GET /users/{id}/access", h.requireUserManager(h.UserAccessPage))
	mux.HandleFunc("GET /users/{id}/manage", h.requireUserManager(h.UserAccessPage))
	mux.HandleFunc("GET /users/{id}/access/check", h.requireUserManager(h.UserAccessCheck))
	post("POST /users/{id}/access/base", h.requireUserManager(h.UserAccessSetBase))
	post("POST /users/{id}/access/grants", h.requireUserManager(h.UserAccessAddGrant))
	post("POST /users/{id}/access/grants/{gid}/edit", h.requireUserManager(h.UserAccessEditGrant))
	post("POST /users/{id}/access/grants/{gid}/delete", h.requireUserManager(h.UserAccessDeleteGrant))
	mux.HandleFunc("GET /icon/{sha}", h.IconPNG)

	// Command output SSE
	mux.HandleFunc("GET /commands/{id}/output/{serial}/stream", h.requireAuth(h.CommandOutputStream))
}

// CommandOutputStream is an SSE endpoint that streams live output for a
// specific (command, device) pair as the device sends it.
func (h *Handler) CommandOutputStream(w http.ResponseWriter, r *http.Request) {
	cmdID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid command id", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch, unsub := h.shell.SubscribeCommandOutput(cmdID, device.ID)
	defer unsub()

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// LogcatLivePage renders the live logcat console for a device.
func (h *Handler) LogcatLivePage(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	h.render(w, r, "logcat_live.html", map[string]any{
		"Title":  "Realtime logs · " + device.SerialNumber,
		"Device": device,
		"Online": h.hub.IsConnected(device.ID),
	})
}

// LogcatStream opens a Server-Sent Events stream of live `logcat` output from a
// device. It mints a request_id, pushes start_logcat_stream to the device, relays
// each chunk as an SSE `data:` line, and pushes stop_logcat_stream when the browser
// disconnects. Query params: level (V|D|I|W|E), tag, buffer, grep, tail.
func (h *Handler) LogcatStream(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if !h.hub.IsConnected(device.ID) {
		http.Error(w, "device not connected", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	reqID := uuid.NewString()
	ch := h.logs.Open(reqID)
	defer h.logs.Close(reqID)

	start, _ := json.Marshal(map[string]any{
		"type":       "start_logcat_stream",
		"request_id": reqID,
		"level":      logcatLevel(r.URL.Query().Get("level")),
		"tag":        strings.TrimSpace(r.URL.Query().Get("tag")),
		"buffer":     strings.TrimSpace(r.URL.Query().Get("buffer")),
		"grep":       strings.TrimSpace(r.URL.Query().Get("grep")),
		"tail":       logcatTail(r.URL.Query().Get("tail")),
	})
	if !h.hub.Push(device.ID, start) {
		http.Error(w, "failed to reach device", http.StatusServiceUnavailable)
		return
	}
	// Tell the device to stop streaming the moment this browser goes away.
	defer func() {
		stop, _ := json.Marshal(map[string]any{"type": "stop_logcat_stream", "request_id": reqID})
		h.hub.Push(device.ID, stop)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, ": logcat stream %s\n\n", reqID)
	flusher.Flush()

	ka := time.NewTicker(20 * time.Second)
	defer ka.Stop()
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ka.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// logcatLevel clamps a requested min-priority to a valid logcat level char.
func logcatLevel(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "V", "D", "I", "W", "E":
		return strings.ToUpper(strings.TrimSpace(s))
	default:
		return "V"
	}
}

// logcatTail clamps the initial backlog line count to a sane range.
func logcatTail(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 200
	}
	if n > 5000 {
		return 5000
	}
	return n
}

// ── WS push helpers ───────────────────────────────────────────────────────────

func (h *Handler) pushCommand(ctx context.Context, cmd *db.Command, targetType string, targetIDs []uuid.UUID) {
	msg, _ := json.Marshal(map[string]any{
		"type":         "command",
		"id":           cmd.ID,
		"command_type": db.DeviceCommandType(cmd.Type),
		"apk_url":      cmd.ApkURL,
		"payload":      cmd.Payload,
	})

	switch targetType {
	case "all":
		h.hub.Broadcast(msg)
		h.hub.PublishCommandUpdate(cmd.ID)
		return
	case "groups":
		ids, err := h.db.GetDeviceIDsByGroupIDs(ctx, targetIDs)
		if err != nil {
			return
		}
		targetIDs = ids
	}

	var pushed []uuid.UUID
	for _, deviceID := range targetIDs {
		// Surface the new queue entry on that device's own page immediately (its Queue
		// tab listens for this), instead of only on its next 300s poll or a reload —
		// whether the command ends up pushed, held behind an earlier one, or the device
		// is offline and never gets a push at all.
		h.hub.PublishDeviceUpdate(deviceID)
		// The whole queue runs one command at a time per device: if an earlier command for
		// this device is still unfinished, hold this one (it stays queued and is flushed when
		// the current one finishes).
		if blocked, err := h.db.CommandBlocked(ctx, cmd.ID, deviceID); err == nil && blocked {
			continue
		}
		if h.hub.Push(deviceID, msg) {
			pushed = append(pushed, deviceID)
		}
	}
	// One batched status write for all online targets (was a query per device).
	// A pushed command is only 'delivered', never 'completed' here — including reboot.
	// Marking a reboot 'completed' at delivery (as this path used to) is a false
	// positive: hub.Push succeeding only means the frame was queued, not that the device
	// rebooted; a device that never reboots (low battery / declined / half-open socket)
	// then shows a false 'completed'. Reboot flips to 'completed' only when the device
	// actually comes back — CompleteDeliveredReboots on WS connect / next check-in
	// (FW-2026-000033). This matches the api-side pushCommand.
	_ = h.db.SetCommandStatusForDevices(ctx, cmd.ID, pushed, "delivered", false)
	// Surface the new delivery/ack state on the command detail page in real time
	// instead of waiting for its 30s polling fallback.
	h.hub.PublishCommandUpdate(cmd.ID)
}

// otaConfigLastView / otaConfigNextView back the discovery-config card: what the fleet is
// reading right now, and the window the next publish would carry.
func otaConfigLastView(cfg *config.Config) map[string]string {
	body, at := cfg.OTAConfigLast()
	return map[string]string{"Body": body, "At": at}
}

func otaConfigNextView(cfg *config.Config) map[string]any {
	o := cfg.OTAConfigOptions()
	start, end := otaconfig.Window(o, time.Now())
	return map[string]any{"Start": start, "End": end, "TZ": o.Timezone}
}

// deviceReportsBattery reports whether the hardware actually sends battery readings.
// True when there is no evidence either way (a device that has only just enrolled), so
// a new T7 keeps its battery UI until its checkins say otherwise.
func deviceReportsBattery(d *db.Device, recent []db.Checkin) bool {
	if d.BatteryPct > 0 {
		return true
	}
	if len(recent) == 0 {
		return true // nothing to judge by yet: trust the product catalog
	}
	for _, c := range recent {
		if c.BatteryPct > 0 {
			return true
		}
	}
	return false
}
