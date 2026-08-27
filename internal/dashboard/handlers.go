package dashboard

import (
	"bytes"
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
	"mdm/internal/ai"
	"mdm/internal/alerts"
	"mdm/internal/apkmeta"
	"mdm/internal/apkstore"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/geolocate"
	"mdm/internal/logstream"
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
	w.Header().Set("HX-Trigger", string(b))
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
		w.Header().Set("HX-Trigger", string(b))
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

	// lastDigestDay is the YYYY-MM-DD of the most recent AI fleet digest sent, so
	// housekeeping posts it at most once per day. Touched only from the single
	// housekeeping goroutine.
	lastDigestDay string
}

var logcatSeverityRe = regexp.MustCompile(`\b([EWIDV])\/|\s([EWIDV])\s`)

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
		HasBattery:   dev.HasBattery(),
		RowClasses:   deviceRowClasses(dev),
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

	funcMap := template.FuncMap{
		// atoi parses a string to int (0 on failure) for arithmetic in templates.
		"atoi": atoi,
		// isLegacyBuild reports whether a build ID is on the configured legacy (WS-incapable)
		// firmware list — such devices only HTTP check-in and never hold a live WebSocket.
		"isLegacyBuild": cfg.IsLegacyBuild,
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
		"canAdmin": func(role string) bool { return role == "admin" || role == "dev" },
		// canAdminOrTester mirrors requireAdminOrTester: admin/dev plus the test team,
		// for group/restaurant creation and kiosk mode. Used to show those controls.
		"canAdminOrTester": func(role string) bool {
			return role == "admin" || role == "dev" || role == "tester"
		},
		// canAct gates only the Actions dock link. Every authenticated role may
		// open the Actions page — viewers see it read-only (the builder, recipes,
		// resend and delete controls are all separately gated by canOperate /
		// canAdmin, so a viewer sees history but no way to act).
		"canAct": func(role string) bool {
			return role == "admin" || role == "dev" || role == "tester" || role == "viewer"
		},
		// canOperate reports action-level UI power: the test team plus admin/dev.
		// Mirrors requireOperatorOrAdmin on the server so the dashboard shows the
		// same actions those roles can actually perform.
		"canOperate": func(role string) bool {
			return role == "admin" || role == "dev" || role == "tester"
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
		"nowUTC": func() time.Time {
			return time.Now().UTC()
		},
		"timeSince": func(t time.Time) string {
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
		"productLabel": product.Label,
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
		"notStale": func(t time.Time, thresholdSecs int) bool {
			if thresholdSecs <= 0 {
				return true
			}
			return time.Since(t) <= time.Duration(thresholdSecs)*time.Second
		},
		"batteryTemp": func(raw json.RawMessage) string {
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
			case "DOWNLOAD_ERROR":
				return "Download failed — the device couldn't fetch the OTA package (check the URL is reachable from the device)."
			case "UPDATE_ENGINE_BIND_ERROR":
				return "Couldn't reach the device's system update service (update_engine)."
			}
			if n, ok := strings.CutPrefix(code, "UPDATE_ERROR_"); ok {
				if txt := updateEngineErrors[n]; txt != "" {
					return txt
				}
				return "update_engine error code " + n + "."
			}
			return code
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
		publicOrigins: parseOrigins(os.Getenv("PUBLIC_ORIGIN")),
		loginFails:    ratelimit.New(15 * time.Minute),
		assetVer:      assetVersion("static/style.css"),
	}
}

// assetVersion returns a short cache-busting token for a static asset, derived
// from its modification time and size. Falls back to the build version if the
// file can't be stat'd, so the URL is always well-formed.
func assetVersion(path string) string {
	if fi, err := os.Stat(path); err == nil {
		return fmt.Sprintf("%x-%x", fi.ModTime().UnixNano(), fi.Size())
	}
	return version.Current()
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
func (h *Handler) currentSession(r *http.Request) (*db.Session, bool) {
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

// audit records an admin action (best-effort; never blocks the request).
func (h *Handler) audit(r *http.Request, action, target, detail string) {
	actor := h.currentUsername(r)
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAudit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[audit] insert failed: %v", err)
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
	data["CurrentUser"] = h.currentUsername(r)
	data["Brand"] = h.cfg.CustomBrand()
	data["Use24Hour"] = h.cfg.Use24Hour()
	data["Version"] = version.Current()
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
	case path == "/":
		data["ActivePage"] = "overview"
	case strings.HasPrefix(path, "/devices") || path == "/export" || path == "/packages":
		data["ActivePage"] = "devices"
	case strings.HasPrefix(path, "/groups"):
		data["ActivePage"] = "groups"
	case strings.HasPrefix(path, "/restaurants"):
		data["ActivePage"] = "restaurants"
	case strings.HasPrefix(path, "/productions"):
		data["ActivePage"] = "productions"
	case strings.HasPrefix(path, "/commands"):
		data["ActivePage"] = "commands"
	case strings.HasPrefix(path, "/releases"):
		data["ActivePage"] = "releases"
	case strings.HasPrefix(path, "/updates"):
		data["ActivePage"] = "updates"
	case strings.HasPrefix(path, "/setup"):
		data["ActivePage"] = "setup"
	case strings.HasPrefix(path, "/settings"):
		data["ActivePage"] = "settings"
	case strings.HasPrefix(path, "/users"):
		data["ActivePage"] = "users"
	case strings.HasPrefix(path, "/changelog"):
		data["ActivePage"] = "changelog"
	}
	// The unified Fleet surface (Devices/Restaurants/Groups tabs) needs all three
	// counts for its tab strip; fetch them only on those pages.
	if ap, _ := data["ActivePage"].(string); ap == "devices" || ap == "groups" || ap == "restaurants" {
		if fc, err := h.db.FleetCounts(r.Context()); err == nil {
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
	"commands.html": true, "releases.html": true,
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

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isLoggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
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
		if s.Role != "admin" && s.Role != "dev" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireAdminOrTester extends requireAdmin (admin/dev) to also allow testers, for
// the fleet-organisation tasks the test team owns: creating groups and restaurants
// and setting kiosk mode. Viewers and operators still can't reach these.
func (h *Handler) requireAdminOrTester(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "admin" && s.Role != "dev" && s.Role != "tester" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireStrictAdmin guards the two areas a dev must never reach: settings and
// user management. Only the env-configured "admin" passes.
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
		if s.Role != "admin" && s.Role != "dev" && s.Role != "tester" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireTester gates QA result-marking to the tester role only. Managing test
// cases stays admin-only; recording pass/fail is the tester's job and nobody
// else's (viewer/operator/admin all see results read-only).
func (h *Handler) requireTester(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if s.Role != "tester" {
			http.Error(w, "Forbidden", http.StatusForbidden)
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
	http.Redirect(w, r, "/login", http.StatusFound)
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

	// Build filter
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
	activeThresholdLabel := fmt.Sprintf("%d min", activeThreshold/60)
	// The "Retired" view (hidden=only) is admin-only; everyone else only ever sees
	// active devices. Any other value collapses to active-only (there is no mixed view).
	role := h.role(r)
	hiddenParam := ""
	if r.URL.Query().Get("hidden") == "only" && (role == "admin" || role == "dev") {
		hiddenParam = "only"
	}
	filter := db.DeviceFilter{
		Search:              q,
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
		Hidden:              hiddenParam,
		ActiveThresholdSecs: activeThreshold,
		// Online/offline is live WebSocket presence: the status filter and the pill
		// counts (GetSummaryFiltered) resolve it against this connected set.
		Connected: h.connectedSlice(),
	}

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
		return err
	})
	run(func() error {
		var err error
		prodCounts, err = h.db.CountDevicesByProduct(r.Context())
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

	connected := h.hub.ConnectedIDs()
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
	for _, k := range []string{"status", "production", "build", "battery", "kiosk", "charging", "timezone"} {
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
		selectedCollection, selectedCount = product.Label(pk), total
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
	for _, p := range product.All() {
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

	data := map[string]any{
		"Title":                "Devices",
		"Devices":              devices,
		"Flapping":             flapping,
		"Total":                total,
		"FleetTotal":           fleetTotal,
		"FilterCount":          filterCount,
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
		"Products":             product.All(),
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
		"FilterHidden":         hiddenParam,
		"ActiveThresholdSecs":  activeThreshold,
		"ActiveThresholdLabel": activeThresholdLabel,
		"Density":              h.cfg.Density(),
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
	ctx := r.Context()
	activeSecs := h.cfg.CheckinInterval() * 3
	summary, err := h.db.GetSummary(ctx, h.connectedSlice())
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
		groups     []db.GroupHealth
		hot        int
		d14        []db.FleetDailyStat
		openAlerts []db.Alert
		audit      []db.AuditEntry
		openCount  int
		crashStats db.FleetCrashStats
	)
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { groups, _ = h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7) })
	run(func() { hot, _ = h.db.CountHotDevices(ctx) })
	run(func() { d14, _ = h.db.GetFleetDailyStats(ctx, 14) })
	run(func() { openAlerts, _ = h.db.ListAlerts(ctx, "open", 5) })
	run(func() {
		// The audit page itself is admin-only; keep the activity feed consistent.
		if h.role(r) == "admin" {
			audit, _ = h.db.ListAudit(ctx, 6)
		}
	})
	run(func() { openCount, _ = h.db.CountOpenAlerts(ctx) })
	run(func() { crashStats, _ = h.db.GetFleetCrashStats(ctx, 4) })
	wg.Wait()

	daily := d14
	if n := len(d14); n > 7 {
		daily = d14[n-7:]
	}

	// Fleet score: device-weighted mean of the per-group health scores. Without
	// groups, fall back to an online-ratio penalty so the ring still means something.
	score := 100
	if summary.Total > 0 {
		if num, den := 0, 0; len(groups) > 0 {
			for _, g := range groups {
				num += g.Score * g.DeviceCount
				den += g.DeviceCount
			}
			if den > 0 {
				score = num / den
			}
		} else {
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
		actPts = append(actPts, map[string]any{"label": ds.Day.Format("Mon"), "val": ds.Active})
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
	// Crash surfaces (24h rollup): signal tile, per-restaurant chips, top devices.
	// (openCount, crashStats already fetched concurrently above.)
	crashRows := make([]crashRow, 0, len(crashStats.ByDevice))
	for _, c := range crashStats.ByDevice {
		label, class := crashKindBadge(c.Kind)
		crashRows = append(crashRows, crashRow{
			Serial: c.Serial, Restaurant: c.Restaurant, KindLabel: label, KindClass: class,
			BuildID: c.BuildID, Ago: agoShort(c.LatestAt), Count: c.Count,
		})
	}
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

	data := map[string]any{
		"Title": "Overview",
		// withRole overwrites this on success; the default keeps the template's
		// numeric comparison safe if the alerts count query fails.
		"AlertsOpenCount": 0,
		"Summary":         summary,
		"Offline":         offline,
		"Hot":             hot,
		"Attention":       attention,
		"Score":           score,
		"ScoreClass":      scoreClass,
		// 2π·r44 = 276.5; the ring template animates to this offset.
		"RingOffset":           fmt.Sprintf("%.1f", 276.5*float64(100-score)/100),
		"Verdict":              verdict,
		"Greeting":             greeting,
		"DateLine":             time.Now().Format("Monday, January 2"),
		"GroupsCount":          len(groups),
		"SparkActive":          sparkPoints(actS),
		"SparkOff":             sparkPoints(offS),
		"SparkLow":             sparkPoints(lowS),
		"SparkHot":             sparkPoints(hotS),
		"ActivityBars":         activityBars,
		"ActivityJSON":         activityJSON,
		"ActivityPeak":         peak,
		"Audit":                audit,
		"OpenAlerts":           openAlerts,
		"ActiveThresholdLabel": fmt.Sprintf("%d min", activeSecs/60),
		// Command-center fields.
		"RestVerdict":       restVerdict,
		"StatusWord":        statusWord,
		"ScoreDelta":        scoreDelta,
		"HeroSentence":      heroSentence,
		"AttentionRest":     attentionRest,
		"WorstGroups":       worst,
		"RestaurantCrashes": crashStats.ByRestaurant,
		"Crashes24h":        crashStats.Total24h,
		"CrashDevices24h":   crashStats.Devices24h,
		"CrashList":         crashRows,
		"CrashSpark":        sparkPoints(crS),
		"OnlineDelta":       tileDelta(onToday, onYest, false),
		"OfflineDelta":      tileDelta(offToday, offYest, true),
		"LowDelta":          tileDelta(lowToday, lowYest, true),
		"CrashDelta":        tileDelta(crToday, crYest, true),
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
	}

	// Fleet map: every device with a resolved location from the geolocation pipeline.
	locs, locCount := h.deviceLocationsJSON(ctx)
	data["DeviceLocations"] = locs
	data["DeviceMapCount"] = locCount
	data["MapsEmbedKey"] = h.mapsEmbedKey

	h.render(w, r, "overview.html", data)
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
		"Device":            device,
		"Role":              h.role(r),
		"InstalledPackages": installedPkgs,
		"PendingInstalls":   pendingInstallRows(commands, apps, installedPkgs, apkPkg),
		"Uninstalling":      pendingUninstallPkgs(commands),
	})
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
		deviceCrashRaw  []db.CrashEvent
		deviceAlertsRaw []db.Alert
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
		chartCheckins = downsampleCheckins(cc, 2500)
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
		// The per-device command queue (non-terminal commands, FIFO) for the Queue tab.
		q, _ := h.db.GetDeviceQueue(ctx, device.ID)
		redactDeviceCommandURLs(role, q)
		queue = filterShellDeviceCommands(role, q)
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
		// endpoint accepts (admin/dev/tester, i.e. canAdminOrTester); gating this on
		// admin alone left a tester the button and popover but an empty group list,
		// so "+ group" did nothing.
		if role == "admin" || role == "dev" || role == "tester" {
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
		// Charger-fault detection: charging toggles per minute recently. A high rate
		// means the charger/dock connection is dropping in and out (faulty
		// hardware). >10/min is the flapping threshold (matches the
		// charger_flapping alert default).
		flapRate, _ = h.db.DeviceChargerFlapRate(ctx, device.ID, 5)
	})
	run(func() { deviceCrashRaw = mustCrashes(h.db.ListDeviceCrashes(ctx, device.ID, 50)) })
	run(func() { deviceAlertsRaw, _ = h.db.ListDeviceActiveAlerts(ctx, device.ID, 50) })

	wg.Wait()
	if firstErr != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// In-flight installs: install_apk commands not yet completed/failed, so the
	// Applications list can show them as "installing" until the device reports them
	// (skipping any whose app the device already has present).
	pendingInstalls := pendingInstallRows(commands, apps, installedPkgs, apkPkg)

	// Kiosk unlock code (everyone except viewers) — shown inside the Kiosk section so an
	// operator/tester/admin can read it to a technician who needs to leave kiosk on-device.
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

	// Kiosk locked-app choices: this device's installed apps filtered by the kiosk
	// allowlist (empty allowlist = all apps). Keeps the picker to policy-approved apps.
	kioskApps := make([]db.DevicePackage, 0, len(installedPkgs))
	for _, p := range installedPkgs {
		if h.cfg.KioskAppAllowed(p.PackageName) {
			kioskApps = append(kioskApps, p)
		}
	}

	// Alerts tab: this device's crash/ANR events plus its active alerts, so a crash
	// deep-link from the fleet views lands on something that actually shows crashes.
	deviceCrashes := toCrashCards(deviceCrashRaw)
	var deviceAlerts []humanAlert
	canAct := role == "admin" || role == "dev" || role == "tester"
	for _, a := range deviceAlertsRaw {
		ha := humanizeAlert(a)
		ha.CanAct = canAct
		deviceAlerts = append(deviceAlerts, ha)
	}
	h.resolveAppIcons(ctx, [][]humanAlert{deviceAlerts}, deviceCrashes)

	h.render(w, r, "device.html", map[string]any{
		"Title":               device.SerialNumber,
		"Device":              device,
		"DeviceCrashes":       deviceCrashes,
		"DeviceAlerts":        deviceAlerts,
		"OfflinePeriod":       totp.DefaultPeriod,
		"OfflineDigits":       totp.DefaultDigits,
		"OfflineCode":         offlineCode,
		"OfflineCodeSecs":     offlineSecs,
		"ChargerFlapRate":     flapRate,
		"ChargerFlapping":     flapRate > 10,
		"Notes":               notes,
		"Release":             release,
		"Online":              h.hub.IsConnected(device.ID),
		"ChartCheckins":       chartCheckins,
		"ChartFocus":          focusParam,
		"Commands":            commands,
		"Queue":               queue,
		"ExtraColumns":        h.cfg.Columns(),
		"Apps":                apps,
		"InstalledPackages":   installedPkgs,
		"KioskApps":           kioskApps,
		"PendingInstalls":     pendingInstalls,
		"Uninstalling":        pendingUninstallPkgs(commands),
		"InstalledSet":        pkgNameSet(installedPkgs),
		"KioskConfig":         kioskCfg,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ShellEnabled":        h.cfg.ShellEnabled(),
		"RemoteEnabled":       h.cfg.RemoteEnabled(),
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
		// Single-use, short-lived token instead of the admin API key (which must
		// never reach the browser). The control WebSocket redeems it server-side.
		"Token": h.remote.IssueToken(device.ID, 2*time.Minute),
	})
}

// DeviceChartData returns a device's battery / temperature / RAM series for an
// on-demand window [from,until] (epoch-ms), so the chart's custom-range and 7d/30d
// controls can pull OLDER history instead of only filtering the ~48h baked into the
// page. Older builds' data was always in the checkins table (battery + temp are
// recorded for every build) — it just was never fetched, so widening the range showed
// nothing before the initial window. Down-sampled to keep the payload small.
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
	// These devices can check in every few seconds, so a multi-day pull is huge — keep
	// at most maxPoints evenly-strided points per series.
	const maxPoints = 2500
	stride := 1
	if n > maxPoints {
		stride = (n + maxPoints - 1) / maxPoints
	}
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
	battery := make([]bpt, 0, maxPoints)
	temp := make([]pt, 0, maxPoints)
	ram := make([]pt, 0, maxPoints)
	for i := 0; i < n; i += stride {
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"battery": battery, "temp": temp, "ram": ram})
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
	online := h.hub.IsConnected(device.ID)
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

	writeEvent(h.hub.IsConnected(device.ID))

	sub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

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
			writeEvent(ev.Online)
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

	sendRow := func(deviceID uuid.UUID) {
		dev, err := h.db.GetDeviceByID(r.Context(), deviceID)
		if err != nil {
			return
		}
		row := deviceToRowJSON(*dev, h.hub.IsConnected(deviceID), activeThreshold)
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

// OverviewAlerts renders the overview's live "Open alerts" list as an htmx
// fragment, refetched on every mdm:alerts-update so the card tracks alerts live.
func (h *Handler) OverviewAlerts(w http.ResponseWriter, r *http.Request) {
	alerts, _ := h.db.ListAlerts(r.Context(), "open", 5)
	total, _ := h.db.CountOpenAlerts(r.Context())
	h.tmpl.ExecuteTemplate(w, "overview-alerts", map[string]any{
		"OpenAlerts": alerts, "AlertsOpenCount": total,
	})
}

// ReportAlertsByRestaurant renders the Daily Report's per-restaurant open-alert
// breakdown as an htmx fragment. The card re-fetches it on every mdm:alerts-update
// (fired by the global /alerts/events stream) so the breakdown tracks new, acked,
// and resolved alerts live without its own SSE connection.
func (h *Handler) ReportAlertsByRestaurant(w http.ResponseWriter, r *http.Request) {
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

	writePresence(h.hub.IsConnected(device.ID))
	writeDeviceUpdate()

	presenceSub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(presenceSub)
	updateSub := h.hub.SubscribeDeviceUpdates()
	defer h.hub.UnsubscribeDeviceUpdates(updateSub)

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
		case ev, ok := <-presenceSub:
			if !ok {
				return
			}
			if ev.DeviceID == device.ID {
				writePresence(ev.Online)
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
	canAct := role == "admin" || role == "dev" || role == "tester"

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
	var crashEvents []db.CrashEvent
	crashTotal := 0
	if deviceID != nil {
		crashEvents, crashTotal, _ = h.db.ListDeviceCrashesPage(r.Context(), *deviceID, crashPageSize, (page-1)*crashPageSize)
	} else {
		crashEvents, crashTotal, _ = h.db.ListRecentCrashEventsPage(r.Context(), 7, crashPageSize, (page-1)*crashPageSize)
	}
	// Clamp a past-the-end page back to the last real page so a stale ?page= link
	// lands on content, not an empty list.
	crashPages := (crashTotal + crashPageSize - 1) / crashPageSize
	if crashPages > 0 && page > crashPages {
		page = crashPages
		if deviceID != nil {
			crashEvents, crashTotal, _ = h.db.ListDeviceCrashesPage(r.Context(), *deviceID, crashPageSize, (page-1)*crashPageSize)
		} else {
			crashEvents, crashTotal, _ = h.db.ListRecentCrashEventsPage(r.Context(), 7, crashPageSize, (page-1)*crashPageSize)
		}
	}
	crashes := toCrashCards(crashEvents)
	h.resolveAppIcons(r.Context(), [][]humanAlert{crit, watch}, crashes)
	// Preserve the device scope on the pager links.
	crashPageBase := "/alerts?view=crashes"
	if deviceSerial != "" {
		crashPageBase += "&device=" + url.QueryEscape(deviceSerial)
	}
	h.render(w, r, "alerts.html", map[string]any{
		"Title":         "Alerts",
		"Summary":       summary,
		"View":          view,
		"Critical":      crit,
		"Watching":      watch,
		"NeedsCount":    len(crit),
		"WatchCount":    len(watch),
		"ActiveCount":   len(crit) + len(watch),
		"Crashes":       crashes,
		"CrashCount":    crashTotal,
		"DeviceFilter":  deviceSerial,
		"CrashPage":     page,
		"CrashPages":    crashPages,
		"CrashPageBase": crashPageBase,
	})
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
	alerts, _ := h.db.ListActiveAlerts(r.Context(), 1)
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
	alerts, err := h.db.ListActiveAlerts(r.Context(), 6)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "alerts-recent", map[string]any{"Alerts": humanizeAll(alerts)})
}

// AlertBulk applies an action (acknowledge|resolve) to the alert IDs selected via
// checkboxes on the alerts page.
func (h *Handler) AlertBulk(w http.ResponseWriter, r *http.Request) {
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
	h.setAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolve(w http.ResponseWriter, r *http.Request) {
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
	h.bulkAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolveAll(w http.ResponseWriter, r *http.Request) {
	h.bulkAlertStatus(w, r, "resolved")
}

// AlertClearAll deletes every alert row (admin only). Rules are untouched, so any
// alert whose condition still holds re-fires on the next evaluation.
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
		if num, den := 0, 0; len(groups) > 0 {
			for _, g := range groups {
				num += g.Score * g.DeviceCount
				den += g.DeviceCount
			}
			if den > 0 {
				score = num / den
			}
		} else {
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
	// Show the same cached fleet report as the main page (latest of hourly or manual).
	if s, err := h.db.GetAISummary(r.Context(), "fleet"); err == nil && s.Summary != "" {
		data["AISummary"] = s.Summary
		data["AISummaryAt"] = s.GeneratedAt.UTC().Format(time.RFC3339)
		data["AISummaryPreview"] = summarizePreview(s.Summary)
		data["ReportRestaurants"] = restaurantLinksJSON(groups)
	}
	h.render(w, r, "health.html", data)
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
}

// deviceLocationsJSON returns every device with a resolved lat/lon in its latest
// telemetry, as JSON for the overview fleet map, plus the count. Coordinates come
// from the geolocation pipeline (WiFi scan -> lat/lon merged into the device extra).
func (h *Handler) deviceLocationsJSON(ctx context.Context) (template.JS, int) {
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 5000, "", "")
	if err != nil {
		return template.JS("[]"), 0
	}
	online := h.hub.ConnectedIDs()
	pts := make([]deviceMapPoint, 0, 16)
	for _, dv := range devs {
		var m map[string]json.RawMessage
		if len(dv.LatestExtra) == 0 || json.Unmarshal(dv.LatestExtra, &m) != nil {
			continue
		}
		var lat, lon float64
		if json.Unmarshal(m["latitude"], &lat) != nil || json.Unmarshal(m["longitude"], &lon) != nil {
			continue
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
		pts = append(pts, deviceMapPoint{
			Serial: dv.SerialNumber, Name: name, Lat: lat, Lon: lon, Address: addr, Online: isOn,
		})
	}
	b, err := json.Marshal(pts)
	if err != nil {
		return template.JS("[]"), 0
	}
	return template.JS(b), len(pts)
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
	serials := r.URL.Query().Get("serials")
	if serials == "" {
		http.Redirect(w, r, "/devices", http.StatusFound)
		return
	}
	serialList := strings.Split(serials, ",")

	h.render(w, r, "export.html", map[string]any{
		"Title":   "Export Data",
		"Serials": serialList,
		"From":    backPath(r),
	})
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

func (h *Handler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	serials := r.Form["serials"]
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

	header := []string{"serial_number", "timestamp"}
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
		ts := row.Timestamp
		if cycles {
			// Grid marks read naturally in the requester's wall clock (13:00, 14:00…).
			ts = ts.In(loc)
		}
		rec := []string{
			row.SerialNumber,
			ts.Format(time.RFC3339),
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
		"Online":              h.hub.IsConnected(device.ID),
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
		"Online":   h.hub.IsConnected(device.ID),
	})
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
	queue = filterShellDeviceCommands(h.role(r), queue)
	h.renderCachedHTML(w, r, "device-queue", map[string]any{
		"Device": device,
		"Queue":  queue,
		"Online": h.hub.IsConnected(device.ID),
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
	connected := h.hub.ConnectedIDs()
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
	h.render(w, r, "group_detail.html", map[string]any{
		"Title":               g.Name,
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
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
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (h *Handler) GroupCreate(w http.ResponseWriter, r *http.Request) {
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

// GroupUpdate renames a group. A group has only a name, so this is its full "edit";
// the fleet toolbar's inline pencil posts here (and returns to the roster).
func (h *Handler) GroupUpdate(w http.ResponseWriter, r *http.Request) {
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
		"Title":     "New restaurant",
		"Timezones": restaurantTimezones,
	})
}

func (h *Handler) RestaurantCreate(w http.ResponseWriter, r *http.Request) {
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
	win, hasOwn, _ := h.db.GetRestaurantServiceWindow(r.Context(), id)
	h.render(w, r, "restaurant_detail.html", map[string]any{
		"Title":               rest.Name,
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ServiceWindow":       windowView(id.String(), rest.Name, win, hasOwn),
		"PeakWindows":         h.peakView(r.Context(), &id),
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

// RestaurantSetPeakWindows replaces (or resets to fleet default) this restaurant's peak
// ranges from repeated peak_start[]/peak_end[] form fields.
func (h *Handler) RestaurantSetPeakWindows(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	if r.FormValue("action") == "reset" {
		_ = h.db.SetPeakWindows(r.Context(), &id, nil) // clears own rows → inherits fleet
		h.audit(r, "restaurant.peak_windows", id.String(), "reset")
		http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
		return
	}
	starts, ends := r.Form["peak_start"], r.Form["peak_end"]
	var ranges []db.PeakWindow
	for i := range starts {
		s := strings.TrimSpace(starts[i])
		var e string
		if i < len(ends) {
			e = strings.TrimSpace(ends[i])
		}
		if s == "" || e == "" {
			continue // skip blank rows
		}
		ranges = append(ranges, db.PeakWindow{StartMin: parseHHMM(s, -1), EndMin: parseHHMM(e, -1)})
	}
	// Drop any row that failed to parse (parseHHMM returned the -1 sentinel).
	valid := ranges[:0]
	for _, rg := range ranges {
		if rg.StartMin >= 0 && rg.EndMin >= 0 {
			valid = append(valid, rg)
		}
	}
	if err := h.db.SetPeakWindows(r.Context(), &id, valid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.peak_windows", id.String(), "")
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
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
	// Loaded into the fleet-page drawer via htmx: drop the page chrome (header/footer/
	// back-link) and post the save back to the roster instead of the venue page.
	embed := r.Header.Get("HX-Request") == "true"
	h.render(w, r, "restaurant_form.html", map[string]any{
		"Title":      "Edit " + rest.Name,
		"Restaurant": rest,
		"Timezones":  restaurantTimezones,
		"Embed":      embed,
		"RedirectTo": "/devices?restaurant=" + id.String(),
	})
}

func (h *Handler) RestaurantUpdate(w http.ResponseWriter, r *http.Request) {
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
	// The fleet drawer posts a "redirect" back to the roster; the standalone edit
	// page posts none and falls back to the venue page.
	localRedirect(w, r, "/restaurants/"+id.String())
}

func (h *Handler) RestaurantDelete(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "restaurant-members", h.withRole(r, map[string]any{
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
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
// CmdkIndex returns the navigable entities (groups, restaurants, releases) as JSON, so
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
	if rels, err := h.db.ListReleases(r.Context()); err == nil {
		for _, rel := range rels {
			label := rel.Version
			if rel.Name != "" {
				label = rel.Version + " · " + rel.Name
			}
			out = append(out, entry{label, "Release", fmt.Sprintf("/releases/%d", rel.ID), "Release"})
		}
	}
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "group-members", h.withRole(r, map[string]any{
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
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

	cmd, err := h.db.CreateCommand(r.Context(), cmdType, apkURL, payload, "groups", []uuid.UUID{id})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, "groups", []uuid.UUID{id})
	detail := "target=groups, group=" + id.String()
	if reason != "" {
		detail += ", reason=" + reason
	}
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

// BulkAssignRestaurant assigns the selected devices to a restaurant from the devices-page
// bulk-selection bar.
func (h *Handler) BulkAssignRestaurant(w http.ResponseWriter, r *http.Request) {
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

func (h *Handler) ReleaseList(w http.ResponseWriter, r *http.Request) {
	releases, err := h.db.ListReleases(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
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
			row.Status, row.Hidden = rel.Status, rel.Hidden
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
			Status:     rel.Status, Hidden: rel.Hidden,
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
	// "N hidden" count) to operators/testers.
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

	data := map[string]any{
		"Title":           "Releases",
		"Versions":        active,
		"HiddenReleases":  hidden,
		"UntrackedBuilds": untrackedBuilds,
		"TrackedCount":    trackedCount,
		"NotTrackedCount": notTrackedCount,
		"FleetTotal":      fleetTotal,
		"ActiveRelease":   activeRel,
		"ActiveQA":        activeQA,
		"ActiveProblems":  activeProblems,
		"ActiveCarried":   activeCarried,
		"ProblemBoard":    problemBoard,
		"GlobalProblems":  globalProblems,
		"ReleaseTrain":    releaseTrain,
		"Graph":           buildReleaseGraph(releases),
		"Products":        product.All(),
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
		if n, err := h.db.CountDevicesByVersion(r.Context(), rel.Version); err == nil && n > 0 {
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

// ReleaseQAPage keeps the legacy /releases/{id}/qa URL working (bookmarks + the non-JS
// POST-redirect fallbacks) by deep-linking into the QA tab of the unified workspace.
func (h *Handler) ReleaseQAPage(w http.ResponseWriter, r *http.Request) {
	h.renderReleaseWorkspace(w, r, "qa")
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
	devicesCount, _ := h.db.CountDevicesByVersion(ctx, rel.Version)
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
	// sourceBuilds: current builds an active incremental can update FROM. Used to mark
	// devices in the push picker that have no applicable artifact (incremental-only
	// release + a device on some other build → nothing to send it).
	sourceBuilds := map[string]bool{}
	for _, p := range packages {
		if p.Status != "active" {
			continue
		}
		canPush = true
		if p.Type == "full" {
			hasFull = true
		} else if p.SourceBuildID != "" {
			sourceBuilds[p.SourceBuildID] = true
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
	// SourceBuilds + HasFull let the push picker mark devices with no applicable artifact
	// (incremental-only release, device not on a source build) as ineligible.
	data["SourceBuilds"] = sourceBuilds
	data["DevicesCount"] = devicesCount
	data["SourceReleases"] = sourceReleases
	// Fleet build distribution for this product — guides which source builds are worth an
	// incremental (make deltas from the builds a meaningful chunk of the fleet runs).
	fleetBuilds, _ := h.db.FleetBuildDistribution(ctx, rel.Product, rel.Version)
	data["FleetBuilds"] = fleetBuilds
	data["CrashGroups"] = crashGroups
	data["CrashGroupTotal"] = crashTotal
	data["DevicesOnVersion"] = devs
	// Approach A — inline "push to devices" panel: every device of this release's product
	// (productWhere folds legacy/empty -> t7), so the picker is auto-scoped and a
	// wrong-product device can never be selected. The template marks devices already on
	// this build as "up to date". ActiveThresholdSecs drives the online dot.
	pushDevices, _ := h.db.ListDevices(ctx, db.DeviceFilter{Product: rel.Product}, 0, 500, "serial", "asc")
	data["PushDevices"] = pushDevices
	data["ActiveThresholdSecs"] = h.cfg.CheckinInterval() * 3
	// Devices already mid-OTA (pending/downloading on an active deployment) — the picker
	// marks them "updating" and disables them, since their build_id still shows the old
	// version until they reboot.
	updating, _ := h.db.SerialsUpdating(ctx)
	data["DevicesUpdating"] = updating
	// Devices already on a strictly newer release of this product — the picker disables
	// them and labels them "newer installed" (OTAing an older release onto them is blocked).
	blocked, _ := h.db.SerialsOnNewerRelease(ctx, rel.ID)
	data["DevicesBlocked"] = blocked
	// Groups and restaurants power the push pane's target selector (deploy to a whole
	// group/venue, mirroring the Actions target picker).
	if groups, err := h.db.ListGroups(ctx); err == nil {
		data["Groups"] = groups
	}
	if rests, err := h.db.ListRestaurants(ctx); err == nil {
		data["Restaurants"] = rests
	}
	return data
}

// ReleaseProblemCreate files a new problem report against a release. Any operator
// or tester (canOperate) can report; the build is snapshotted from the release.
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

// ReleaseTestingDone marks a release's testing complete — the finish line that moves it
// from active to inactive (it drops out of the hub's active slot). Admin/dev/tester (see
// requireAdminOrTester) — the test team closes out their own testing. Advisory, not gated
// on QA/sign-off state.
func (h *Handler) ReleaseTestingDone(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseTestingDone(r.Context(), id, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.testing_done", strconv.Itoa(id), "")
	h.hub.PublishProblemUpdate() // the hub's active release changed — refresh open hubs
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// ReleaseReopenTesting reopens a finished release, making it active again. Admin/dev.
func (h *Handler) ReleaseReopenTesting(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.ClearReleaseTestingDone(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.testing_reopen", strconv.Itoa(id), "")
	h.hub.PublishProblemUpdate()
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
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

// ReleaseSetTestResult records a tester's outcome for one case on a release.
func (h *Handler) ReleaseSetTestResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	caseID, err := uuid.Parse(r.FormValue("test_case_id"))
	if err != nil {
		http.Error(w, "Invalid test case", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if !validTestStatuses[status] {
		http.Error(w, "Invalid status", http.StatusBadRequest)
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if err := h.db.SetTestResult(r.Context(), id, caseID, status, notes, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Keep the Problems list in sync with QA (two-way): a failed case opens a
	// linked problem; passing it later resolves that problem. Best-effort.
	switch status {
	case "fail":
		// A "Verify fix" case (linked to a manual bug via problem_id) reopens that
		// bug directly — no separate QA-source problem needed, or we'd show a
		// duplicate on the board. For a plain QA case (no problem_id link), the
		// usual UpsertQAProblem creates/opens the QA-source record.
		if reopened, err := h.db.ReopenProblemForCase(r.Context(), caseID); err != nil {
			log.Printf("[qa] reopen linked problem on fail: %v", err)
		} else if !reopened {
			if err := h.db.UpsertQAProblem(r.Context(), id, caseID, notes, h.currentUsername(r)); err != nil {
				log.Printf("[qa] upsert problem from fail: %v", err)
			}
		}
	case "pass":
		if err := h.db.ResolveQAProblem(r.Context(), id, caseID); err != nil {
			log.Printf("[qa] resolve problem on pass: %v", err)
		}
		// A "Verify fix" case (linked to a carried-over bug) verifies that bug when it passes.
		if err := h.db.VerifyProblemForCase(r.Context(), caseID, id); err != nil {
			log.Printf("[qa] verify linked problem on pass: %v", err)
		}
	}
	h.audit(r, "testresult.set", fmt.Sprintf("release %d / %s = %s", id, caseID, status), "")
	h.hub.PublishProblemUpdate() // QA marks can create/resolve problems + shift readiness
	if hxReq(r) {
		h.writeReleaseQAResponse(w, r, id, &caseID)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/qa", id), http.StatusSeeOther)
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
	// Carry-forward is a dev/admin concern — testers/operators don't see it.
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
		"CanRecord":      role == "tester",
		"CanReport":      role == "admin" || role == "dev" || role == "tester",
		"CanAct":         role == "admin" || role == "dev", // act on existing problems (fixed/verified/wontfix) — testers file only
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

// ReleaseSetSkipBase toggles whether this release's QA skips the base (standard)
// test cases and only checks its release-specific cases.
func (h *Handler) ReleaseSetSkipBase(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	skip := r.FormValue("skip_base_tests") == "on"
	if err := h.db.SetReleaseSkipBaseTests(r.Context(), id, skip); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	state := "base+specific"
	if skip {
		state = "specific-only"
	}
	h.audit(r, "release.qa_scope", strconv.Itoa(id), state)
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/qa", id), http.StatusSeeOther)
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
// release and its targets. Testers/operators can view the list but not deploy.
func (h *Handler) UpdatesHub(w http.ResponseWriter, r *http.Request) {
	role := h.role(r)
	deployments, _ := h.db.ListDeployments(r.Context())
	deployable, _ := h.db.ListDeployableReleases(r.Context())
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

	canDeploy := role == "admin" || role == "dev"
	var devices []db.Device
	if canDeploy {
		devices, _ = h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "", "")
	}
	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for cid := range connected {
		online[cid] = true
	}
	h.render(w, r, "updates.html", map[string]any{
		"Title":         "Updates",
		"Deployments":   deployments,
		"Releases":      deployable,
		"Devices":       devices,
		"Groups":        groups,
		"Online":        online,
		"CanDeploy":     canDeploy,
		"PreRelease":    r.URL.Query().Get("release"),
		"Products":      product.All(),
		"FilterProduct": filterProduct,
	})
}

// NewUpdatePage is the standalone "push an update" screen reached from the Updates
// page's "+ New Update" button. Step 1: pick a publishable release. Step 2 (once
// ?release= is set): pick the specific devices, reboot and delivery mode, then POST
// to /updates (DeployCreate). Device selection is scoped to the release's product.
func (h *Handler) NewUpdatePage(w http.ResponseWriter, r *http.Request) {
	deployable, _ := h.db.ListDeployableReleases(r.Context())

	data := map[string]any{
		"Title":               "New Update",
		"Releases":            deployable,
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
				data["PushDevices"] = pushDevices

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

				updating, _ := h.db.SerialsUpdating(r.Context())
				data["DevicesUpdating"] = updating
				blocked, _ := h.db.SerialsOnNewerRelease(r.Context(), relID)
				data["DevicesBlocked"] = blocked
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

	deployment, err := h.db.CreateReleaseUpdate(r.Context(), relID, rebootBehavior, scheduledTime)
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
	if len(eligible) > 0 {
		if err := h.db.SendUpdateToDevices(r.Context(), deployment.ID, eligible, forceFull); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		// Nudge online targets to check in NOW so the OTA resolves immediately instead of
		// waiting out the device's periodic check-in (otherwise it sits "pending" for
		// minutes). Offline devices pick it up on their next check-in as before.
		h.nudgeCheckin(eligible)
	}

	h.audit(r, "release.deploy", rel.Version, strconv.Itoa(len(eligible)))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, deployment.ID), http.StatusSeeOther)
}

func (h *Handler) DeploymentDetail(w http.ResponseWriter, r *http.Request) {
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
	targets, _ := h.db.GetUpdateTargets(r.Context(), did)
	upd.Targets = targets

	otaProgress := make(map[string]any)
	counts := make(map[string]int)
	done := 0
	durSum, durCount := 0, 0
	for _, t := range targets {
		if p := h.shell.GetOTAProgress(t.DeviceID); p != nil {
			otaProgress[t.DeviceID.String()] = p
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
	// Ordered, non-zero status buckets for the rollup line (map iteration order
	// is unstable, so build a fixed-order slice for the template).
	var summary []map[string]any
	for _, s := range []string{"pending", "downloading", "installing", "installed", "awaiting_reboot", "reboot_sent", "failed"} {
		if counts[s] > 0 {
			summary = append(summary, map[string]any{"Status": s, "Count": counts[s]})
		}
	}
	pct := 0
	if len(targets) > 0 {
		pct = done * 100 / len(targets)
	}

	data := map[string]any{
		"Title":        fmt.Sprintf("Deployment #%d", did),
		"Deployment":   upd,
		"Release":      upd.Release,
		"OTAProgress":  otaProgress,
		"Summary":      summary,
		"SummaryDone":  done,
		"SummaryTotal": len(targets),
		"SummaryPct":   pct,
		"AvgDuration":  avgDuration, // seconds, or -1 if no device finished yet
		"DoneCount":    durCount,    // devices with a measured duration
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
	canOp := role == "admin" || role == "dev" || role == "tester"
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
		eligible = append(eligible, d)
	}
	devices = eligible

	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for cid := range connected {
		online[cid] = true
	}
	data["Devices"] = devices
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
	h.audit(r, "deployment.cancel", strconv.Itoa(did), "")
	h.hub.PublishDeploymentUpdate()
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
	if err := h.db.ClearPendingOTACommands(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Retry with the full image: a failed device has almost always tripped on an
	// incremental that can't apply to its source build, and the full package is
	// guaranteed-applicable. Pinning here also recovers devices that failed before
	// the auto-fallback existed.
	_ = h.db.SetUpdateDeviceForceFull(r.Context(), did, device.ID)
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
	cmd, err := h.db.CreateCommand(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.SetUpdateDeviceStatus(r.Context(), did, device.ID, "reboot_sent")
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
		cmd, err := h.db.CreateCommand(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{deviceID})
		if err != nil {
			log.Printf("[deployment-reboot-all] create reboot error device=%s: %v", deviceID, err)
			continue
		}
		_ = h.db.SetUpdateDeviceStatus(r.Context(), did, deviceID, "reboot_sent")
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
		cmds = filterShellCommands(h.role(r), c) // testers never see shell history
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
	canOp := role == "admin" || role == "dev" || role == "tester"
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

	// Collections for the scope-rail target picker (restaurants + releases with counts).
	scopeRestaurants, _ := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), 7)
	scopeReleases, _ := h.db.ListPublishedReleasesForRail(r.Context())

	// Diagnostics catalog for the Actions builder (admin/dev/tester run them; the
	// pill is hidden for viewers and when the catalog is empty).
	var actionQueries []db.DeviceQuery
	if role := h.role(r); role == "admin" || role == "dev" || role == "tester" {
		actionQueries, _ = h.db.ListEnabledDeviceQueries(r.Context())
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
		"TargetSerials":    targetSerials,
		"ShellRecent":      shellRecent,
		"ShellPopular":     shellPopular,
		"AIEnabled":        h.cfg.AIEnabled(),
		"Prefill":          prefill,
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
	cmds = filterShellCommands(h.role(r), cmds) // testers never see shell history
	summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), 0) // full history
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
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
	if rid := r.URL.Query().Get("restaurant"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.RestaurantID = id
		}
	}
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
	h.tmpl.ExecuteTemplate(w, "cmd-device-browser", map[string]any{
		"Devices": devices,
		"Online":  online,
	})
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
		if skipped > 0 {
			s, verb := "", "doesn't"
			if skipped != 1 {
				s, verb = "s", "don't"
			}
			warn = fmt.Sprintf("%d targeted device%s %s report this package — they'll be skipped automatically.", skipped, s, verb)
		}
	}

	h.tmpl.ExecuteTemplate(w, "cmd-impact", map[string]any{
		"Type":       cmdType,
		"Total":      total,
		"Effective":  effective,
		"Online":     onlineEff,
		"Offline":    offlineEff,
		"Skipped":    skipped,
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
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" {
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

// advanceQueue delivers the next queued command (any type) for a device now that the
// current one finished or was removed — the whole queue runs one at a time and the delivery
// gate holds the rest. No-op if the device is offline (it flushes on reconnect) or nothing
// is eligible. The gate makes GetPendingCommandsForDevice return only the single oldest
// deliverable command, so pushing the first result advances the queue by exactly one.
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
		break // gate ensures at most one command is deliverable
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
	// Admin/dev can delete anything. Tester can only delete a command still In progress
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
	newCmd, err := h.db.CreateCommand(r.Context(), cmd.Type, cmd.ApkURL, cmd.Payload, "devices", deviceIDs)
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
	newCmd, err := h.db.CreateCommand(r.Context(), cmd.Type, cmd.ApkURL, cmd.Payload, cmd.TargetType, targetIDs)
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
	// Testers must not see raw shell command detail (text/output) — hide its
	// existence, mirroring the shell filtering applied to every history listing.
	if cmd.Type == "shell" && hideShellForRole(h.role(r)) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !canSeeCommandURLs(h.role(r)) {
		cmd.ApkURL = ""
	}
	h.render(w, r, "command_detail.html", map[string]any{
		"Title":      "Action " + id.String()[:8],
		"Command":    cmd,
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
// Testers carry the everyday action set (screenshot/install/uninstall/reboot) plus
// their exclusive QA recording elsewhere, so they're listed alongside admin/dev on
// those types — but never on the admin/dev-only types (shell, ota, update_splash).
// Raw shell is limited to admin/dev; testers reach vetted commands via the device
// queries catalog instead (the "diagnostic" action on the Actions page).
var commandRoles = map[string][]string{
	"screenshot":    {"admin", "dev", "tester", "viewer"},
	"install_apk":   {"admin", "dev", "tester"},
	"uninstall":     {"admin", "dev", "tester"},
	"reboot":        {"admin", "dev", "tester"},
	"shell":         {"admin", "dev"},
	// "query" is a read-only diagnostic; its command text is admin-vetted (chosen by
	// query_id from the catalog, never user-supplied), so testers may issue it.
	"query":         {"admin", "dev", "tester"},
	"ota":           {"admin", "dev"},
	"update_splash": {"admin", "dev"},
	"logcat":        {"admin", "dev"},
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
	case "update_splash":
		return "Boot logo"
	case "logcat":
		return "Log capture"
	case "ota":
		return "OTA Update"
	default:
		return cmdType
	}
}

// canSeeCommandURLs reports whether a role may be shown internal APK/OTA URLs,
// which embed the object-store bucket name. Only operational roles need them;
// viewers must not see them, so the bucket name is not disclosed via command
// reads (GB-05).
func canSeeCommandURLs(role string) bool {
	return role == "admin" || role == "dev" || role == "tester"
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
// history. Testers cannot run shell (commandRoles limits it to admin/dev) and must
// not browse the shell history either — its command text and captured output can
// expose sensitive operational detail. admin/dev/viewer see history unchanged.
func hideShellForRole(role string) bool { return role == "tester" }

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
	case "all":
		return h.db.GetAllDeviceIDs(r.Context())
	default:
		// No target chosen yet (the builder's default, empty state) — the impact
		// preview should show 0, not silently preview the whole fleet.
		return nil, nil
	}
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

// Manage renders the standing device-configuration page (kiosk lock today; more
// policy types — e.g. charging-pad — land here later). Unlike Actions, nothing on
// this page is a queued command: kiosk config writes directly to device_config and
// is pushed on the target's next check-in (see applyKioskForTargets).
func (h *Handler) Manage(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 2000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	restaurants, _ := h.db.ListRestaurants(r.Context())
	groups, _ := h.db.ListGroups(r.Context())
	fleetPackages, _ := h.db.SearchFleetPackages(r.Context(), "")
	pkgNames := make(map[string]string, len(fleetPackages))
	for _, p := range fleetPackages {
		if p.AppName != "" {
			pkgNames[p.PackageName] = p.AppName
		}
	}

	type manageVenue struct {
		Name    string
		Count   int
		Serials string // comma-joined, feeds target_serials directly
	}
	type manageState struct {
		Label   string
		Enabled bool
		Package string
		Count   int
		Venues  []manageVenue
		Serials string // every device in this state, comma-joined
	}

	// Group by (kiosk_enabled, kiosk_package) first — devices sharing a state
	// collapse into one card instead of one row each, since kiosk config is
	// almost always uniform across most of a fleet. Within a state, sub-group by
	// restaurant so the card still shows WHERE, not just a device-serial dump; a
	// restaurant with devices split across two states legitimately appears under
	// both cards with its partial count in each — no separate "mixed" flag needed,
	// the split is just visible as-is.
	type stateKey struct {
		enabled bool
		pkg     string
	}
	byState := make(map[stateKey][]db.Device)
	var order []stateKey
	for _, d := range devices {
		// Package is meaningless while unlocked, but a device can carry a stale
		// kiosk_package value from before it was last unlocked (its own reported
		// state can drift from what the dashboard set) — ignore it for grouping so
		// "Unlocked" doesn't fracture into multiple cards over a value nobody reads.
		pkg := d.KioskPackage
		if !d.KioskEnabled {
			pkg = ""
		}
		k := stateKey{d.KioskEnabled, pkg}
		if _, ok := byState[k]; !ok {
			order = append(order, k)
		}
		byState[k] = append(byState[k], d)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].enabled != order[j].enabled {
			return order[i].enabled // locked states first
		}
		return len(byState[order[i]]) > len(byState[order[j]])
	})
	states := make([]manageState, 0, len(order))
	for _, k := range order {
		devs := byState[k]
		label := "Unlocked"
		if k.enabled {
			name := k.pkg
			if n, ok := pkgNames[k.pkg]; ok {
				name = n
			}
			label = "Locked — " + name
		}
		byVenue := make(map[string][]string)
		var venueOrder []string
		var allSerials []string
		for _, d := range devs {
			venue := d.RestaurantName
			if venue == "" {
				venue = "Lab (unassigned)"
			}
			if _, ok := byVenue[venue]; !ok {
				venueOrder = append(venueOrder, venue)
			}
			byVenue[venue] = append(byVenue[venue], d.SerialNumber)
			allSerials = append(allSerials, d.SerialNumber)
		}
		sort.Strings(venueOrder)
		venues := make([]manageVenue, 0, len(venueOrder))
		for _, v := range venueOrder {
			venues = append(venues, manageVenue{Name: v, Count: len(byVenue[v]), Serials: strings.Join(byVenue[v], ",")})
		}
		states = append(states, manageState{
			Label: label, Enabled: k.enabled, Package: k.pkg,
			Count: len(devs), Venues: venues, Serials: strings.Join(allSerials, ","),
		})
	}

	role := h.role(r)
	h.render(w, r, "manage.html", map[string]any{
		"Title":         "Manage",
		"States":        states,
		"DeviceCount":   len(devices),
		"Restaurants":   restaurants,
		"Groups":        groups,
		"FleetPackages": fleetPackages,
		"CanEdit":       role == "admin" || role == "dev" || role == "tester",
	})
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
	// the old bulk-kiosk action). Gate it directly (admin/dev/tester), not via the
	// command-role machinery the real commands use.
	if cmdType == "set_kiosk" {
		if role := h.role(r); role != "admin" && role != "dev" && role != "tester" {
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
	// admin/dev/tester may issue it without raw-shell rights: whatever shell_cmd the
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
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" {
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
	skippedSet := map[string]bool{}
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
		cmd, err := h.db.CreateCommand(r.Context(), cmdType, it.apkURL, payload, targetType, ids)
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
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" {
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
	cmd, err := h.db.CreateCommand(ctx, rec.Type, rec.ApkURL, rec.Payload, "devices", ids)
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
	return t == "reboot" || t == "ota" || t == "update_splash"
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
	pkg, name, icon := req.Package, req.Name, req.Icon
	if pkg == "" {
		// Fallback: no client metadata — parse the object server-side (slower).
		meta, err := h.apk.Parse(r.Context(), req.Key)
		if err != nil {
			log.Printf("apk parse %s: %v", req.Key, err)
			writeJSONError(w, http.StatusBadRequest, "could not parse APK — is it a valid .apk?")
			return
		}
		pkg, name, icon = meta.Package, meta.Label, meta.IconPNGB64
	}
	if len(icon) > 128*1024 { // safety cap; a 96px PNG is far smaller
		icon = ""
	}
	if name == "" {
		name = pkg
	}
	// apk_url must be an absolute, device-reachable URL (the device fetches it directly).
	// Prefer PUBLIC_ORIGIN; fall back to the request host.
	base := ""
	if len(h.publicOrigins) > 0 {
		base = h.publicOrigins[0]
	}
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	app, err := h.db.CreateS3App(r.Context(), name, pkg, req.Key, base)
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
	http.Redirect(w, r, "/setup", http.StatusFound)
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (h *Handler) SettingsPage(w http.ResponseWriter, r *http.Request) {
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
	kioskFleetApps, _ := h.db.ListPackagesAdmin(r.Context(), "")
	googleUsage := h.buildGoogleUsage(r.Context())
	googleUsageJSON, _ := json.Marshal(googleUsage)
	learnedAPs, _ := h.db.ListWifiAPsRecent(r.Context(), 25)
	deviceQueries, _ := h.db.ListDeviceQueries(r.Context())
	repoApps, _ := h.db.ListApps(r.Context())
	productions, _ := h.db.ListProductions(r.Context(), h.connectedSlice())
	h.render(w, r, "settings.html", map[string]any{
		"Title":                "Settings",
		"Apps":                 repoApps,
		"Productions":          productions,
		"DeviceQueries":        deviceQueries,
		"BaseCases":            baseCases,
		"ExtraColumns":         h.cfg.Columns(),
		"LegacyCheckin":        h.cfg.LegacyCheckin(),
		"LegacyBuilds":         h.cfg.LegacyBuilds(),
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
		"DBStats":              dbStats,
		"KioskAllowlist":       strings.Join(h.cfg.KioskAllowlist(), "\n"),
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
		"actions-stepper", "actions-accordion", "actions-drawer",
		"actions-pick-pills", "actions-pick-spotlight", "actions-pick-toolbar",
		"actions-panes-studio", "actions-panes-console", "actions-panes-guided",
		"actions-target", "actions-target-rail", "actions-target-audience", "actions-target-split",
		"actions-install-picker", "actions-kiosk-config", "manage-table",
		"action-types", "action-rollout", "action-cockpit",
		"export", "export-builder", "export-compact", "timesel",
		"health-pulse", "health-triage", "health-grid",
		"health-command", "health-reliability", "health-stream", "health-icons",
		"overview-command",
		"alerts-inbox", "alerts-grouped", "notifications",
		"release-pipeline", "release-cockpit", "ota-flow":
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
// and so they're never exposed to a viewer/operator/tester or an external guest login.
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
	h.hxRedirect(w, r, "/settings")
}

func (h *Handler) SettingsSetMaxTargets(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	n := 0
	if v, err := strconv.Atoi(r.FormValue("max_targets")); err == nil && v >= 0 {
		n = v
	}
	h.cfg.SetMaxTargets(n)
	h.hxRedirect(w, r, "/settings") // may clamp to 0/default — re-render the stored value
}

// ── Device diagnostics catalog ───────────────────────────────────────────────
// Admins curate a set of read-only device queries (label + shell command). They're
// surfaced on the device page as "retrieve property" buttons; because the command
// text comes only from this catalog (never user input), testers may run them even
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
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
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
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Handler) SettingsQueryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.db.DeleteDeviceQuery(r.Context(), id)
	h.audit(r, "query.delete", strconv.Itoa(id), "")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Handler) SettingsQueryToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.db.SetDeviceQueryEnabled(r.Context(), id, r.FormValue("enabled") == "1")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
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
const inactiveAfterDays = 10

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
	if !h.cfg.AIDigestEnabled() || !h.cfg.AIEnabled() {
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
	client := ai.New(h.cfg.AIProvider(), h.cfg.AnthropicAPIKey(), h.cfg.AnthropicModel(), h.cfg.AIBaseURL())
	text, usage, err := client.AnalyzeFleet(cctx, groups, summary.Total, summary.RecentlyActive, openAlerts, deployed, lab, alerts, h.alertThresholds(cctx))
	if err != nil {
		log.Printf("[digest] analyze: %v", err)
		return
	}
	if err := h.db.RecordAIUsage(cctx, usage.InputTokens, usage.OutputTokens); err != nil {
		log.Printf("[digest] record usage: %v", err)
	}
	// The report is structured JSON; flatten it to readable text for the webhook.
	msg := text
	if rep, ok := ai.ParseReport(text); ok {
		msg = rep.Text()
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
	)
	// Prunes can delete many rows, so run them in the background to keep Save snappy.
	go h.applyPrunes(context.Background())
	h.hxRedirect(w, r, "/settings") // retention fields may be normalized — re-render them
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsSetAlertWebhook(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The form masks the stored URL (a bearer secret) and never re-sends it, so a blank
	// submission means "keep the existing value" rather than "clear it".
	if u := strings.TrimSpace(r.FormValue("alert_webhook_url")); u != "" {
		h.cfg.SetAlertWebhookURL(u)
	}
	h.audit(r, "alerts.webhook", "", "")
	h.hxDoneToast(w, r, "/settings", "Settings saved", "success")
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
		http.Redirect(w, r, "/settings", http.StatusFound)
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	wr, err := h.db.GetFleetWrapped(r.Context())
	if err != nil {
		log.Printf("[wrapped] compute: %v", err)
	}
	h.render(w, r, "wrapped.html", map[string]any{
		"Title":      "Fleet Wrapped",
		"W":          wr,
		"OnlineDays": wr.OnlineMinutes / 1440,
		"WorkerDays": int(wr.HardestWorker.Value) / 1440,
	})
}

// AlertConfigView renders the alert-rule configuration read-only. Editing stays
// in Settings (admin-only); this page lets testers and devs see exactly what the
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
	if err := h.db.UpdateAlertRule(r.Context(), id, r.FormValue("enabled") == "on", pj, aw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.rule", typ, "")
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	http.Redirect(w, r, "/settings", http.StatusFound)
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
	h.hxRedirect(w, r, "/settings") // may clamp to default — re-render the stored value
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
	h.hxRedirect(w, r, "/settings") // may clamp to default — re-render the stored value
}

func (h *Handler) SettingsAddColumn(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	key := strings.TrimSpace(r.FormValue("key"))
	label := strings.TrimSpace(r.FormValue("label"))
	if key == "" || label == "" {
		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}
	h.cfg.Add(config.ExtraColumn{Key: key, Label: label})
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsRemoveColumn(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	h.cfg.Remove(key)
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsAddLegacyBuild(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if id := strings.TrimSpace(r.FormValue("build_id")); id != "" {
		h.cfg.AddLegacyBuild(id)
	}
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsRemoveLegacyBuild(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	h.cfg.RemoveLegacyBuild(strings.TrimSpace(r.FormValue("build_id")))
	http.Redirect(w, r, "/settings", http.StatusFound)
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

	cmd, err := h.db.CreateCommand(r.Context(), cmdType, apkURL, payload, "devices", []uuid.UUID{device.ID})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	detail := "device=" + serial
	if reason != "" {
		detail += ", reason=" + reason
	}
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

// DeviceNotesUpdate saves freeform operator notes for a device and returns the
// updated notes card for an htmx swap (falls back to a redirect without JS).
func (h *Handler) DeviceNotesUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
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

	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))

	if enabled && pkg == "" {
		http.Error(w, "Kiosk package is required when enabling kiosk mode", http.StatusBadRequest)
		return
	}

	// Only allow locking to an app the device actually reports as installed. Enabling
	// kiosk for a package that isn't on the device stores an unenforceable policy: the
	// client can't launch it, so lock-task never engages and the device sits unlocked
	// while the dashboard shows kiosk "on". Reject it here so the state stays truthful.
	if enabled {
		pkgs, perr := h.db.GetDevicePackages(r.Context(), device.ID)
		if perr != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		installed := false
		for _, p := range pkgs {
			if p.PackageName == pkg {
				installed = true
				break
			}
		}
		if !installed {
			http.Error(w, "That app is not installed on this device. Install it first, then enable kiosk.", http.StatusBadRequest)
			return
		}
	}

	if err := h.db.SetKioskConfig(r.Context(), device.ID, enabled, pkg, 0); err != nil {
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
	h.render(w, r, "device_packages.html", map[string]any{
		"Title":    serial + " — Packages",
		"Device":   device,
		"Packages": pkgs,
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

	endSeq := startSeq + quantity - 1
	params := db.ProductionParams{
		Name:          name,
		ProductCode:   productCode,
		ModelCode:     modelCode,
		Variant:       variant,
		SKU:           sku,
		Batch:         db.EncodeBatch(batchMonth, batchYear),
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
	h.render(w, r, "users.html", map[string]any{
		"Users": users,
	})
}

// validUserRole reports whether role is a role an admin may assign to a DB user.
// "admin" is excluded — it is env-configured only, never a DB user.
func validUserRole(role string) bool {
	switch role {
	case "viewer", "tester", "dev":
		return true
	}
	return false
}

func (h *Handler) UserCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")

	if username == "" || password == "" || !validUserRole(role) {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if _, err := h.db.CreateUser(r.Context(), username, string(hash), role); err != nil {
		http.Error(w, "Username already exists or internal error", http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}

func (h *Handler) UserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
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
	role := r.FormValue("role")
	if !validUserRole(role) {
		http.Error(w, "Invalid role", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateUserRole(r.Context(), id, role); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserSetPassword resets a team user's password (admin-only). The built-in admin
// account's password comes from the environment and can't be changed here.
func (h *Handler) UserSetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
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
	http.Redirect(w, r, "/users", http.StatusFound)
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

	mux.HandleFunc("GET /{$}", h.requireAuth(h.Overview))
	mux.HandleFunc("GET /devices", h.requireAuth(h.DeviceList))
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
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.DeviceDetail))
	mux.HandleFunc("GET /devices/{serial}/history", h.requireAuth(h.DeviceHistory))
	mux.HandleFunc("GET /devices/{serial}/chart-data", h.requireAuth(h.DeviceChartData))
	mux.HandleFunc("GET /devices/{serial}/events", h.requireAuth(h.DeviceEvents))
	mux.HandleFunc("GET /devices/{serial}/ws-status", h.requireAuth(h.DeviceOnlineStatus))
	mux.HandleFunc("GET /devices/{serial}/presence-stream", h.requireAuth(h.DevicePresenceStream))
	mux.HandleFunc("GET /devices/{serial}/stats", h.requireAuth(h.DeviceStatsPartial))
	mux.HandleFunc("GET /devices/{serial}/vitals", h.requireAuth(h.DeviceVitalsPartial))
	mux.HandleFunc("GET /devices/{serial}/panel", h.requireAuth(h.DeviceInspectorPanel))
	mux.HandleFunc("GET /devices/{serial}/battery.csv", h.requireAuth(h.DeviceBatteryCSV))
	mux.HandleFunc("GET /devices/{serial}/daily-stats", h.requireAuth(h.DeviceDailyStatsJSON))
	post("POST /devices/{serial}/ai-analysis", h.requireAuth(h.DeviceAIAnalysis))
	mux.HandleFunc("GET /devices/{serial}/ai-analyses", h.requireAuth(h.DeviceAIAnalysesList))
	mux.HandleFunc("GET /devices/{serial}/shell", h.requireAdmin(h.DeviceShellPage))
	mux.HandleFunc("GET /devices/{serial}/commands-status", h.requireAuth(h.DeviceCommandsPartial))
	mux.HandleFunc("GET /devices/{serial}/queue", h.requireAuth(h.DeviceQueuePartial))
	post("POST /devices/{serial}/queue/clear", h.requireOperatorOrAdmin(h.DeviceQueueClear))
	post("POST /devices/{serial}/queue/{id}/remove", h.requireOperatorOrAdmin(h.DeviceQueueRemove))
	mux.HandleFunc("GET /devices/{serial}/installs", h.requireAuth(h.DeviceInstallProgress))
	post("POST /devices/{serial}/installs/cancel", h.requireOperatorOrAdmin(h.DeviceInstallCancel))
	post("POST /devices/{serial}/commands", h.requireAuth(h.DeviceCommandCreate))
	post("POST /devices/{serial}/poll-interval", h.requireAdmin(h.DeviceSetPollInterval))
	post("POST /devices/{serial}/notes", h.requireOperatorOrAdmin(h.DeviceNotesUpdate))
	post("POST /devices/{serial}/kiosk", h.requireAdminOrTester(h.DeviceKioskUpdate))
	post("POST /devices/{serial}/wlc", h.requireAdminOrTester(h.DeviceWlcUpdate))
	post("POST /devices/{serial}/offline-code/rotate", h.requireAdmin(h.DeviceRotateOfflineCode))
	mux.HandleFunc("GET /devices/{serial}/offline-code", h.requireOperatorOrAdmin(h.DeviceOfflineCode))
	post("POST /devices/{serial}/hide", h.requireAdmin(h.DeviceHide))
	post("POST /devices/{serial}/unhide", h.requireAdmin(h.DeviceUnhide))
	post("POST /devices/{serial}/clear-ota", h.requireAdmin(h.DeviceClearOTA))
	// Remote screen capture + input injection is highly sensitive (full control of the
	// device), so it is restricted to admins only.
	mux.HandleFunc("GET /devices/{serial}/remote", h.requireAdminOrTester(h.DeviceRemote))
	post("POST /devices/bulk-hide", h.requireAdmin(h.BulkHideDevices))
	post("POST /devices/bulk-unhide", h.requireAdmin(h.BulkUnhideDevices))
	post("POST /devices/bulk-restaurant", h.requireAdmin(h.BulkAssignRestaurant))
	post("POST /devices/bulk-kiosk", h.requireAdminOrTester(h.BulkKioskUpdate))
	post("POST /devices/bulk-kiosk-apps", h.requireAdminOrTester(h.BulkKioskApps))
	mux.HandleFunc("GET /export", h.requireAuth(h.ExportPage))
	post("POST /export/csv", h.requireAuth(h.ExportCSV))
	mux.HandleFunc("GET /devices/{serial}/packages", h.requireAuth(h.DevicePackages))
	mux.HandleFunc("GET /devices/{serial}/apps-list", h.requireAuth(h.DeviceAppsList))
	mux.HandleFunc("GET /packages", h.requireStrictAdmin(h.FleetPackages))
	post("POST /packages/flag", h.requireStrictAdmin(h.PackageFlag))
	mux.HandleFunc("GET /devices/{serial}/logcat/live", h.requireAuth(h.LogcatLivePage))
	mux.HandleFunc("GET /devices/{serial}/logcat/stream", h.requireAuth(h.LogcatStream))

	mux.HandleFunc("GET /groups/new", h.requireAdminOrTester(h.GroupNew))
	mux.HandleFunc("GET /groups/new/devices", h.requireAdminOrTester(h.GroupNewDevices))
	mux.HandleFunc("GET /groups", h.requireAuth(h.GroupList))
	post("POST /groups", h.requireAdminOrTester(h.GroupCreate))
	mux.HandleFunc("GET /groups/{id}", h.requireAuth(h.GroupDetail))
	mux.HandleFunc("GET /groups/{id}/device-search", h.requireAuth(h.GroupDeviceSearch))
	mux.HandleFunc("GET /groups/{id}/members", h.requireAuth(h.GroupMembers))
	mux.HandleFunc("GET /groups/{id}/daily-stats", h.requireAuth(h.GroupDailyStatsJSON))
	mux.HandleFunc("GET /fleet-health", h.requireAuth(h.FleetHealth))
	mux.HandleFunc("GET /reports/alerts-by-restaurant", h.requireAuth(h.ReportAlertsByRestaurant))
	mux.HandleFunc("GET /overview/alerts", h.requireAuth(h.OverviewAlerts))
	mux.HandleFunc("GET /alerts/newest", h.requireAuth(h.AlertNewest))
	post("POST /ai-summary/refresh", h.requireAuth(h.AISummaryRefresh))
	mux.HandleFunc("GET /alerts", h.requireAuth(h.AlertList))
	mux.HandleFunc("GET /alert-config", h.requireAdminOrTester(h.AlertConfigView))
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
	post("POST /groups/{id}", h.requireAdminOrTester(h.GroupUpdate))
	post("POST /groups/{id}/delete", h.requireAdminOrTester(h.GroupDelete))
	post("POST /groups/{id}/devices", h.requireAdminOrTester(h.GroupAddDevice))

	// Restaurants (venue object). Static sub-paths registered before /{id}.
	mux.HandleFunc("GET /restaurants/new", h.requireAdminOrTester(h.RestaurantNew))
	mux.HandleFunc("GET /restaurants/new/device-picker", h.requireAdminOrTester(h.RestaurantNewDevices))
	mux.HandleFunc("GET /restaurants", h.requireAuth(h.RestaurantList))
	post("POST /restaurants", h.requireAdminOrTester(h.RestaurantCreate))
	mux.HandleFunc("GET /restaurants/{id}", h.requireAuth(h.RestaurantDetail))
	mux.HandleFunc("GET /restaurants/{id}/edit", h.requireAdminOrTester(h.RestaurantEdit))
	mux.HandleFunc("GET /restaurants/{id}/daily-stats", h.requireAuth(h.RestaurantDailyStatsJSON))
	mux.HandleFunc("GET /restaurants/{id}/members", h.requireAuth(h.RestaurantMembers))
	post("POST /restaurants/{id}", h.requireAdminOrTester(h.RestaurantUpdate))
	post("POST /restaurants/{id}/delete", h.requireAdminOrTester(h.RestaurantDelete))
	mux.HandleFunc("GET /restaurants/{id}/device-picker", h.requireAdminOrTester(h.RestaurantDevicePicker))
	post("POST /restaurants/{id}/devices", h.requireAdminOrTester(h.RestaurantAssignDevices))
	post("POST /restaurants/{id}/devices/{serial}/remove", h.requireAdminOrTester(h.RestaurantRemoveDevice))
	post("POST /restaurants/{id}/service-window", h.requireAdminOrTester(h.RestaurantSetServiceWindow))
	post("POST /restaurants/{id}/peak-windows", h.requireAdminOrTester(h.RestaurantSetPeakWindows))
	post("POST /devices/{serial}/restaurant", h.requireAdmin(h.DeviceSetRestaurant))
	post("POST /groups/{id}/devices/remove", h.requireAdminOrTester(h.GroupBulkRemoveDevice))
	post("POST /groups/{id}/devices/{serial}/remove", h.requireAdminOrTester(h.GroupRemoveDevice))
	post("POST /groups/{id}/commands", h.requireAdminOrTester(h.GroupCommandCreate))

	// Productions is owned by the test team: admin/dev/tester can list, view,
	// create and export (requireAdminOrTester). Deletion stays admin/dev-only.
	mux.HandleFunc("GET /productions", h.requireAdminOrTester(h.ProductionList))
	mux.HandleFunc("GET /productions/new", h.requireAdminOrTester(h.ProductionNew))
	post("POST /productions", h.requireAdminOrTester(h.ProductionCreate))
	mux.HandleFunc("GET /productions/preview-serial", h.requireAdminOrTester(h.ProductionPreviewSerial))
	mux.HandleFunc("GET /productions/{id}", h.requireAdminOrTester(h.ProductionDetail))
	mux.HandleFunc("GET /productions/{id}/export.csv", h.requireAdminOrTester(h.ProductionExportCSV))
	post("POST /productions/{id}/delete", h.requireAdmin(h.ProductionDelete))

	mux.HandleFunc("GET /commands", h.requireAuth(h.CommandList))
	mux.HandleFunc("GET /manage", h.requireAuth(h.Manage))
	mux.HandleFunc("GET /commands/browse-devices", h.requireAuth(h.CommandBrowseDevices))
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
	post("POST /settings/dashboard", h.requireStrictAdmin(h.SettingsSetDashboard))
	post("POST /settings/kiosk-allowlist", h.requireStrictAdmin(h.SettingsSetKioskAllowlist))
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

	mux.HandleFunc("GET /setup", h.requireAdmin(h.SetupPage))
	post("POST /setup/apps", h.requireAdmin(h.SetupCreateApp))
	post("POST /setup/apps/create", h.requireAdmin(h.SetupCreateAppJSON))
	// S3 APK uploads: presigned direct-to-S3 upload + register + device download proxy.
	post("POST /apps/upload-url", h.requireAdmin(h.AppUploadURL))
	post("POST /apps/register", h.requireAdmin(h.AppRegister))
	mux.HandleFunc("GET /apps/{id}/apk", h.AppAPK) // open: device installer fetches by URL
	post("POST /setup/apps/{id}/edit", h.requireAdmin(h.SetupUpdateApp))
	post("POST /setup/apps/{id}/delete", h.requireAdmin(h.SetupDeleteApp))

	mux.HandleFunc("GET /releases", h.requireAdminOrTester(h.ReleaseList))
	mux.HandleFunc("GET /releases/events", h.requireAdminOrTester(h.ReleaseProblemEvents))
	post("POST /releases", h.requireAdmin(h.ReleaseCreate))
	mux.HandleFunc("GET /releases/{id}", h.requireAdminOrTester(h.ReleaseDetail))
	mux.HandleFunc("GET /releases/{id}/qa", h.requireAdminOrTester(h.ReleaseQAPage))
	post("POST /releases/{id}/packages", h.requireAdmin(h.ReleaseAddPackage))
	post("POST /releases/{id}/packages/inspect", h.requireAdmin(h.PackageInspect))
	mux.HandleFunc("GET /releases/{id}/crashes", h.requireAdminOrTester(h.ReleaseCrashes))
	post("POST /releases/{id}/crashes/group/delete", h.requireAdmin(h.ReleaseCrashGroupDelete))
	post("POST /releases/{id}/crashes/{eid}/delete", h.requireAdmin(h.ReleaseCrashDelete))
	post("POST /releases/{id}/packages/{pid}/delete", h.requireAdmin(h.PackageDelete))
	post("POST /releases/{id}/qfil", h.requireAdmin(h.ReleaseAddQFIL))
	post("POST /releases/{id}/qfil/{qid}/delete", h.requireAdmin(h.ReleaseDeleteQFIL))
	post("POST /releases/track", h.requireAdmin(h.ReleaseTrack))
	post("POST /releases/order", h.requireAdmin(h.ReorderVersions))
	post("POST /releases/version/hide", h.requireAdmin(h.VersionHide))
	post("POST /releases/version/unhide", h.requireAdmin(h.VersionUnhide))
	post("POST /releases/{id}/meta", h.requireAdmin(h.ReleaseEditMeta))
	post("POST /releases/{id}/rename", h.requireAdmin(h.ReleaseRename))
	post("POST /releases/{id}/qa-base", h.requireAdmin(h.ReleaseSetSkipBase))
	post("POST /releases/{id}/hide", h.requireAdmin(h.ReleaseSetHidden))
	post("POST /releases/{id}/delete", h.requireAdmin(h.ReleaseDelete))
	post("POST /releases/{id}/publish", h.requireAdmin(h.ReleasePublish))
	mux.HandleFunc("GET /updates", h.requireAdminOrTester(h.UpdatesHub))
	mux.HandleFunc("GET /updates/new", h.requireAdmin(h.NewUpdatePage))
	post("POST /updates", h.requireAdmin(h.DeployCreate))
	post("POST /releases/{id}/deploy", h.requireAdmin(h.ReleaseDeploy))
	post("POST /releases/{id}/sign-off", h.requireDev(h.ReleaseSignOff))
	post("POST /releases/{id}/sign-off/clear", h.requireDev(h.ReleaseClearSignOff))
	post("POST /releases/{id}/testing-done", h.requireAdminOrTester(h.ReleaseTestingDone))
	post("POST /releases/{id}/testing-done/clear", h.requireAdminOrTester(h.ReleaseReopenTesting))
	post("POST /releases/{id}/branch", h.requireAdmin(h.ReleaseCreateBranch))
	post("POST /releases/{id}/merge", h.requireAdmin(h.ReleaseMerge))
	post("POST /releases/{id}/test-results", h.requireTester(h.ReleaseSetTestResult))
	// Problem reports: any operator/tester can file and triage; admins can delete.
	post("POST /releases/{id}/problems", h.requireOperatorOrAdmin(h.ReleaseProblemCreate))
	post("POST /releases/{id}/problems/{pid}", h.requireOperatorOrAdmin(h.ReleaseProblemUpdate))
	post("POST /releases/{id}/problems/{pid}/delete", h.requireAdmin(h.ReleaseProblemDelete))

	// Test team / QA — base cases are managed inline on the Releases page (admin),
	// testers mark results per-release. No standalone Testing/Test-cases pages.
	post("POST /test-cases", h.requireAdmin(h.TestCaseCreate))
	post("POST /test-cases/{id}/edit", h.requireAdmin(h.TestCaseUpdate))
	post("POST /test-cases/{id}/delete", h.requireAdmin(h.TestCaseDelete))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}", h.requireAdminOrTester(h.DeploymentDetail))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}/events", h.requireAdminOrTester(h.DeploymentEvents))
	post("POST /releases/{id}/deployments/{did}/settings", h.requireOperatorOrAdmin(h.DeploymentUpdateSettings))
	post("POST /releases/{id}/deployments/{did}/cancel", h.requireOperatorOrAdmin(h.DeploymentCancel))
	post("POST /releases/{id}/deployments/{did}/add-targets", h.requireOperatorOrAdmin(h.DeploymentAddTargets))
	post("POST /releases/{id}/deployments/{did}/reboot-all", h.requireOperatorOrAdmin(h.DeploymentRebootAll))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/reboot", h.requireOperatorOrAdmin(h.DeploymentRebootDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/retry", h.requireOperatorOrAdmin(h.DeploymentRetryDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/remove", h.requireOperatorOrAdmin(h.DeploymentRemoveDevice))
	post("POST /releases/{id}/deployments/{did}/delete", h.requireAdmin(h.DeploymentDelete))

	mux.HandleFunc("GET /users", h.requireStrictAdmin(h.UserList))
	post("POST /users", h.requireStrictAdmin(h.UserCreate))
	post("POST /users/{id}/role", h.requireStrictAdmin(h.UserSetRole))
	post("POST /users/{id}/password", h.requireStrictAdmin(h.UserSetPassword))
	post("POST /users/{id}/delete", h.requireStrictAdmin(h.UserDelete))

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
