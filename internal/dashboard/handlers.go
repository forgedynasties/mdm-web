package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
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

	"mdm/internal/adbtunnel"
	"mdm/internal/alerts"
	"mdm/internal/apkstore"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/ingest"
	"mdm/internal/logstream"
	"mdm/internal/mailer"
	"mdm/internal/otagate"
	"mdm/internal/product"
	"mdm/internal/ratelimit"
	"mdm/internal/remote"
	"mdm/internal/scout"
	"mdm/internal/shell"
	"mdm/internal/version"
	"mdm/internal/ws"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
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

// fetchedByJS reports that the caller is script pulling a fragment or JSON, not
// the browser navigating to a page. Sec-Fetch-Dest is "document" only for a real
// navigation; fetch() and XHR send "empty".
func fetchedByJS(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return true
	}
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest" ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

// redirectLogin sends an unauthenticated caller to the login page.
//
// Only a real navigation may be answered with a 302: XHR follows a redirect
// transparently, so a pane, a dropdown or a poll that hits an expired session
// would receive the full login page as its fragment and paint it into place —
// the login form turning up inside the alerts dropdown. htmx gets HX-Redirect,
// which it acts on by navigating; a bare fetch() gets 401 plus X-Auth-Required,
// which the shim in layout.html turns into a navigation.
func redirectLogin(w http.ResponseWriter, r *http.Request) {
	if hxReq(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	if fetchedByJS(r) {
		// Scripts act on the status and header. The body is for when a person ends up
		// looking at this response anyway (a frame, an in-app browser, a link whose
		// request looks scripted): it sends the whole window to the login page instead
		// of showing the word "Unauthorized".
		w.Header().Set("X-Auth-Required", "1")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginRedirectPage))
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// loginRedirectPage is the body of a signed-out response to a script-looking
// request: harmless to the scripts, and a redirect to /login for anyone who sees it.
const loginRedirectPage = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/login"><title>Sign in</title>` +
	`<script>try{(window.top||window).location.replace('/login')}catch(e){location.replace('/login')}</script></head>` +
	`<body style="font-family:system-ui,sans-serif;padding:24px">Your session has ended. <a href="/login" target="_top">Sign in</a></body></html>`

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
	http.Redirect(w, r, flashURL(redirectURL, msg, typ), http.StatusSeeOther)
}

// flashURL carries a toast across a plain (non-htmx) form post's redirect: the page
// shows ?flash=…&flash_type=… once and strips it (chrome.js). Without it a form with
// hx-boost="false" lands back on its page with no word of what happened — the Enrol
// button on Enrollment › Found looked dead when its error was dropped this way.
func flashURL(redirectURL, msg, typ string) string {
	if msg == "" {
		return redirectURL
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		return redirectURL
	}
	q := u.Query()
	q.Set("flash", msg)
	if typ != "" {
		q.Set("flash_type", typ)
	}
	u.RawQuery = q.Encode()
	return u.String()
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
	http.Redirect(w, r, flashURL(redirectURL, msg, typ), http.StatusSeeOther)
}

type Handler struct {
	ingestStats func() ingest.Pipeline // the check-in pipeline, for the Server page (SetIngestStats)
	onKeyReset  func(serial string)    // tells the device API a key was reset (SetKeyResetHook)
	scout       *scout.Service         // scout enrolment (SetScout); nil until wired in main
	db          *db.DB
	hub         *ws.Hub
	shell       *shell.Manager
	remote      *remote.Manager
	tunnels     *adbtunnel.Manager
	logs        *logstream.Manager
	store       *sessions.CookieStore
	tmpl        *template.Template
	cfg         *config.Config
	adminAPIKey string
	// reportSecret signs the unguessable weekly-report PDF links.
	reportSecret string
	alerts       *alerts.Dispatcher
	otaGate      *otagate.Gate // which builds can take an MDM OTA (the rest go legacy)

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

	// fleetKeyFetches throttles /api/v1/app/adb-key per person (fleet_adb_key.go).
	fleetKeyFetches *ratelimit.Counter

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

// extraBoolField reads a boolean check-in field, false when absent or not a boolean.
func extraBoolField(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	return json.Unmarshal(v, &b) == nil && b
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

	if dev.HasBattery() && dev.BatteryPct < 20 {
		classes = append(classes, "row-alert-battery")
	}
	if lvl := deviceTempClass(dev.LatestExtra); lvl != "" && lvl != "ok" {
		classes = append(classes, "row-alert-temp")
	}
	if deviceLost(dev.LatestExtra) {
		classes = append(classes, "row-lost")
	}

	return strings.Join(classes, " ")
}

// DeviceRowJSON holds the pre-computed, JSON-serialisable data for one fleet table row.
type DeviceRowJSON struct {
	Serial       string `json:"serial"`
	BuildID      string `json:"build_id"`
	Online       bool   `json:"online"`
	BatteryPct   int    `json:"battery_pct"`
	BatteryClass string `json:"battery_class"`
	BatteryWidth string `json:"battery_width"`
	RamPct       int    `json:"ram_pct"` // 0 = no data
	HasRam       bool   `json:"has_ram"`
	TempStr      string `json:"temp_str"` // "" = no data
	TempClass    string `json:"temp_class"`
	LastSeenISO  string `json:"last_seen_iso"` // RFC3339, empty if zero
	TimeSince    string `json:"time_since"`
	PollInterval int    `json:"poll_interval_ms"`
	KioskEnabled bool   `json:"kiosk_enabled"`
	// KioskSuspended: the device reported that it is OUT of lock-task after a local
	// unlock (the static exit PIN), while the stored config still says kiosk is on.
	// Without it the dashboard showed "Kiosk on" over a device sitting on the launcher.
	KioskSuspended bool   `json:"kiosk_suspended"`
	KioskPackage   string `json:"kiosk_package"`
	Hidden         bool   `json:"hidden"`      // true once hidden; tells the live row patch to drop the row
	HasBattery     bool   `json:"has_battery"` // false = wall-powered (kiosk, dongle); live patch shows mains, not 0%
	// BatteryMissing: product has a battery but the device reports the pack absent
	// (NTC fault / unplugged). The live patch shows a "None" chip, never a percentage.
	BatteryMissing bool    `json:"battery_missing"`
	Charging       bool    `json:"charging"`
	Flapping       bool    `json:"flapping"`  // charger toggling >10×/min — show the fault glyph
	FlapRate       int     `json:"flap_rate"` // observed toggles/min, for the tooltip
	RowClasses     string  `json:"row_classes"`
	Latitude       float64 `json:"latitude,omitempty"`
	Longitude      float64 `json:"longitude,omitempty"`
}

func deviceToRowJSON(dev db.Device, online bool, staleThreshold time.Duration) DeviceRowJSON {
	r := DeviceRowJSON{
		Serial:         dev.SerialNumber,
		BuildID:        dev.BuildID,
		Online:         online,
		BatteryPct:     dev.BatteryPct,
		BatteryWidth:   fmt.Sprintf("%d%%", dev.BatteryPct),
		PollInterval:   dev.PollIntervalMs,
		KioskEnabled:   dev.KioskEnabled,
		KioskSuspended: extraBoolField(dev.LatestExtra, "kiosk_suspended"),
		KioskPackage:   dev.KioskPackage,
		Hidden:         dev.Hidden,
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

	if temp, src, ok := deviceTempC(dev.LatestExtra); ok {
		r.TempStr = fmt.Sprintf("%.1f°C", temp)
		r.TempClass = tempLevel(temp, src)
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

func NewHandler(d *db.DB, hub *ws.Hub, shellMgr *shell.Manager, remoteMgr *remote.Manager, logMgr *logstream.Manager, sessionSecret string, cfg *config.Config, adminAPIKey, mapsEmbedKey string, geo *geolocate.Resolver, geocoder *geolocate.Geocoder) *Handler {
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
		uuMu  sync.Mutex
		uuAt  time.Time
		uuMap map[string]string
	)
	// userBubble resolves a username or display name (as snapshotted on commands)
	// to what the avatar bubble needs: initial, display name, and the picture URL
	// (empty when none). Cached like userURL.
	type bubble struct {
		Initial, Name, URL string
		Admin              bool // author has the admin/dev role (or is the env dashboard admin)
		System             bool // the server's own work (auto-update, the admin API, a peer server)
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
		if label, ok := db.SystemActorLabel(name); ok {
			return bubble{Initial: "⚙", Name: label, System: true}
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
	// One stamp for every /static/ URL (see assetVersion). Templates read it through the
	// assetVer func rather than .AssetVer so partials rendered with their own data (htmx
	// fragments) can version the scripts they load too.
	assetVer := assetVersion("static")
	funcMap := template.FuncMap{
		"assetVer": func() string { return assetVer },
		// isCards: the user draws device lists as cards (fleet_view), safe on pages that never set it.
		"isCards":           func(v any) bool { s, _ := v.(string); return s == "cards" },
		"venueIcon":         func(id any, name, size string) template.HTML { return venueIconHTML(d, id, name, size) },
		"venuePauseChoices": func() []struct{ Value, Label string } { return venuePauseChoices },
		// withNoHead is the page's data with NoHead set: the venue page's device card has
		// its own header and type tabs over restaurant-members (D1).
		"withNoHead": func(data map[string]any) map[string]any {
			c := make(map[string]any, len(data)+1)
			for k, v := range data {
				c[k] = v
			}
			c["NoHead"] = true
			return c
		},
		"userURL":    userURL,
		"userBubble": userBubble,
		// systemActorLabel names a sender the server writes itself ("Control probe",
		// "Auto-update"), "" for a person.
		"systemActorLabel": func(n string) string { l, _ := db.SystemActorLabel(n); return l },
		"auditSentence":    humanizeAudit,
		"authorIsAdmin":    authorIsAdmin,
		// msLoginEnabled reports whether Microsoft sign-in is configured, so
		// login.html only shows the button when it'll actually work.
		"msLoginEnabled": func() bool { return msLoginEnabled },
		// atoi parses a string to int (0 on failure) for arithmetic in templates.
		"atoi": atoi,
		// isLegacyBuild reports whether a build ID is on the configured legacy (WS-incapable)
		// firmware list — such devices only HTTP check-in and never hold a live WebSocket.
		"isLegacyBuild": cfg.IsLegacyBuild,
		// legacyCheckingIn: a legacy device counts as checking in (yellow dot) while its
		// last HTTP check-in is within legacyWindowSecs (3 check-in intervals).
		"legacyCheckingIn": func(t time.Time) bool {
			return !t.IsZero() && time.Since(t) < time.Duration(3*cfg.CheckinInterval())*time.Second
		},
		"legacyWindowSecs": func() int { return 3 * cfg.CheckinInterval() },
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
		// swatchHue is a stable colour for a user-typed name — a venue, a group. Two
		// venues are told apart in a rail by their mark before their name is read, and
		// the same venue keeps the same colour on every page and every session, because
		// it is derived from the name rather than stored anywhere.
		"swatchHue": func(name string) int {
			var hsum uint32 = 2166136261 // FNV-1a: short, stable, and spreads short strings
			for i := 0; i < len(name); i++ {
				hsum ^= uint32(name[i])
				hsum *= 16777619
			}
			return int(hsum % 360)
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
		// the super admin only (the dev role was retired 2026-10-07). Used to gate
		// operational buttons/links.
		"canAdmin": func(role string) bool { return role == "admin" },
		// whyScore: the penalties behind a venue/group score, for the "?" popover.
		"whyScore": whyScore,
		// canRelease: release / OTA / deployment controls — admin or dev.
		"canRelease": func(role string) bool { return role == "admin" },
		// canSeeDev: who sees releases still marked dev — everyone but viewers/owners.
		"canSeeDev": roleSeesDev,
		"canOTA":    roleCanOTA,
		// canAppLibrary: who may open /apps and upload to the library (admin, dev, super op).
		"canAppLibrary": roleCanAppLibrary,
		// canManageUsers: the Users pages (roster, activity, access control).
		"canManageUsers": roleManagesUsers,
		"canCreateUsers": roleCreatesUsers,
		"roleLabel":      roleLabel,
		"roleLevel":      roleLevel,
		"roleDesc":       roleDesc,
		// ringOffset: stroke-dashoffset for a score ring of circumference c (Overview rail).
		"ringOffset": func(score int, c float64) string {
			if score < 0 {
				score = 0
			} else if score > 100 {
				score = 100
			}
			return fmt.Sprintf("%.1f", c*float64(100-score)/100)
		},
		// classShort: the two-or-three-letter tile for a device class (Overview chips).
		"classShort": classShort,
		"rolePill":   rolePill,
		// isCustomer: an owner or staff account (venue-walled, owner home only).
		"isCustomer": roleIsCustomer,
		// canSupport: the safe device actions — screenshot, reboot, lock, lost mode,
		// alerts, notes. Support and up; mirrors requireSupport.
		"canSupport": roleCanSupport,
		// canAdminOrOperator mirrors requireAdminOrOperator: admin/dev plus the test team,
		// for group/restaurant creation and locked mode. Used to show those controls.
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
		// otaErrorText renders 'stalled' differently depending on whether the device was
		// still checking in when the sweep gave up on it: a live device whose download
		// died is a different fault from a device that vanished, and the operator does
		// different things about each.
		"stallKind": func(t UpdateTargetLike) string {
			if time.Since(t.GetLastSeenAt()) > 15*time.Minute {
				return "the device stopped checking in"
			}
			return "the download stopped while the device stayed online"
		},
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
			case "downloading", "installing", "running":
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
		"productName":  productName, // admin-set display name for a stock product, "" if none
		"modelPic":     modelPic,    // a model's product shot, nil if none
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
		// sizeLabel renders a byte count as "412 KB" / "1.1 MB" (log bundles).
		"sizeLabel": func(n int) string {
			if n >= 1<<20 {
				return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
			}
			return fmt.Sprintf("%d KB", (n+1023)/1024)
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
		// cmdTitle / cmdHint / cmdVerb: one naming per command from type and payload
		// (cmdtitle.go). They take a db.Command or a db.DeviceCommand.
		"cmdTitle": func(c any) string { t, _ := cmdTitleOf(shapeOf(c)); return t },
		// batchSerials: the union of target serials over a batch's members.
		"batchSerials": func(members []db.Command, serials map[uuid.UUID][]string) []string {
			var out []string
			seen := map[string]bool{}
			for _, m := range members {
				for _, s := range serials[m.ID] {
					if !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
				}
			}
			return out
		},
		"cmdHint": func(c any) string { _, h := cmdTitleOf(shapeOf(c)); return h },
		"cmdVerb": func(c any) string { return cmdVerbOf(shapeOf(c)) },
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
		"vitalCols":   vitalColumns, // the device pane's vitals as columns
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
		"tempClass":  deviceTempClass,
		"tempSource": deviceTempSrc,
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
			temp, _, ok := deviceTempC(raw)
			if !ok {
				return "null"
			}
			return template.JS(fmt.Sprintf("%.1f", temp))
		},
		"rowClasses": func(dev db.Device) string {
			return deviceRowClasses(dev)
		},
		// deviceIcon: the fleet card's type-icon key for a device's class. The retired
		// panel class draws as a kiosk; unknown or unset classes get the generic chip.
		"deviceIcon": func(dev db.Device) string {
			switch c := dev.Class(); c {
			case "t7", "kiosk", "tablet", "mpos", "pos", "dongle", "kds", "payment":
				return c
			case "panel":
				return "kiosk"
			}
			return "other"
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
		"supports": func(d db.Device, cmdType string) bool { return d.Supports(cmdType) },
		"degraded": func(d db.Device, cmdType string) bool { return d.Degraded(cmdType) },
		// joinLines renders a serial list for the shared picker's hidden field.
		"joinLines":  func(v []string) string { return strings.Join(v, "\n") },
		"classLabel": product.ClassLabel,
		// The scout's eight enrolment steps, by 1-based number — the same names the
		// firmware client (NetScout.STEPS) and the Enroll apps use, so a badge that says
		// "step 3/8 · Installing the agent" matches what the device is logging.
		// seq a b → [a..b], for templates that draw a fixed number of steps.
		"seq": func(a, b int) []int {
			if b < a {
				return nil
			}
			out := make([]int, 0, b-a+1)
			for i := a; i <= b; i++ {
				out = append(out, i)
			}
			return out
		},
		"scoutStep": func(n int) string {
			steps := []string{"Checking the device", "Getting a token", "Installing the agent", "Setting Device Owner",
				"Granting permissions", "Starting the agent", "Registering with the MDM", "First check-in"}
			if n >= 1 && n <= len(steps) {
				return steps[n-1]
			}
			return ""
		},
		// serialHead / serialTail split a serial for the fleet layout that leads with it:
		// the batch prefix stays quiet and the last few characters — the part support
		// reads out loud, and the part that differs between two units of the same model —
		// carry the weight. One rule, so both the card and any future view agree.
		// misreportedVersion: the device sent the framework's version, not its own —
		// the pre-1.0.2 client bug. Templates ask before printing the number.
		"misreportedVersion": frameworkVersionReported,
		"serialHead":         func(s string) string { return s[:len(s)-serialTailLen(s)] },
		"serialTail":         func(s string) string { return s[len(s)-serialTailLen(s):] },
		"classes":            product.Classes,
		// extraBool reads a boolean from latest_extra (false when absent or not a bool).
		"extraBool": func(raw []byte, key string) bool {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				return false
			}
			var b bool
			return json.Unmarshal(m[key], &b) == nil && b
		},
		// extraSub reads obj.key from latest_extra (e.g. host_app.version_name); "" when absent.
		"extraSub": func(raw []byte, obj, key string) string {
			var m map[string]map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil || m[obj] == nil {
				return ""
			}
			v := m[obj][key]
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
			return strings.Trim(string(v), `"`)
		},
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
		// Battery wear (battery_wear.go): a climb's seconds as "5 h 32 m", the "faster than
		// fleet" line as a percent, and who may open /battery.
		"fmtClimb":      fmtClimb,
		"wearFastPct":   func() int { return wearFastPct },
		"canSeeBattery": batteryRoleOK,
		// Lost mode as the device reports it (lost_mode.go): the fleet's red "Lost" tag.
		"deviceLost": deviceLost,
		"staffMode":  staffMode,
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
		// perDev turns a figure summed across a venue's devices into the per-device
		// average the cards actually want to show. Fleet totals read as impressively
		// large numbers that mean nothing without knowing the device count.
		"perDev": func(total float64, n int) float64 {
			if n <= 0 {
				return 0
			}
			return total / float64(n)
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
	// The plain-language report views (report_views.go): a reason read mid-sentence, and
	// the top spot, which may not exist.
	funcMap["lowerFirst"] = lowerFirst
	funcMap["guestTime"] = guestTime
	funcMap["index0"] = func(s []storySpot) *storySpot {
		if len(s) == 0 {
			return nil
		}
		return &s[0]
	}
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
	if d != nil {
		if m, err := d.ProductNames(context.Background()); err == nil {
			productNames.Store(m)
		}
	}

	return &Handler{
		db:              d,
		hub:             hub,
		apk:             apkStore,
		shell:           shellMgr,
		remote:          remoteMgr,
		logs:            logMgr,
		store:           store,
		tmpl:            tmpl,
		cfg:             cfg,
		adminAPIKey:     adminAPIKey,
		reportSecret:    sessionSecret,
		mapsEmbedKey:    mapsEmbedKey,
		geo:             geo,
		geocoder:        geocoder,
		startedAt:       time.Now(),
		alerts:          alerts.NewDispatcher(d, cfg),
		otaGate:         otagate.New(d, cfg),
		publicOrigins:   parseOrigins(os.Getenv("PUBLIC_ORIGIN")),
		loginFails:      ratelimit.New(15 * time.Minute),
		fleetKeyFetches: ratelimit.New(time.Hour),
		signupAttempts:  ratelimit.New(time.Hour),
		resetAttempts:   ratelimit.New(time.Hour),
		mail:            mustMailer(),
		msClientID:      os.Getenv("MS_CLIENT_ID"),
		msClientSecret:  os.Getenv("MS_CLIENT_SECRET"),
		msTenantID:      os.Getenv("MS_TENANT_ID"),
		assetVer:        assetVer,
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
// publicBase is baseURL with no request to read: what a background job must use when it
// puts a URL in a command. Without a configured PUBLIC_ORIGIN there is nothing truthful
// to say, so it falls back to the loopback the box itself can always reach.
func (h *Handler) publicBase() string {
	if len(h.publicOrigins) > 0 {
		return h.publicOrigins[0]
	}
	return "http://127.0.0.1:8080"
}

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
