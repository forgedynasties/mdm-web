package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/version"
)

func (h *Handler) withRole(r *http.Request, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	role := h.role(r)
	data["Role"] = role
	// Fleet cards have one layout, the serial-led one (Classic removed 15 Oct); an old
	// fleet_layout cookie is ignored. The keys stay for the templates that pass them on.
	data["FleetLayout"] = "serial"
	data["ClassicCards"] = false
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
		if h.access(r).hidesDevices() {
			data["AlertsOpenCount"], _ = h.visibleAlertCounts(r)
		} else if n, err := h.db.CountOpenAlerts(r.Context(), h.access(r).hidesIdentityAlerts()); err == nil {
			data["AlertsOpenCount"] = n
		}
	}

	path := r.URL.Path
	// ActivePage is deliberately coarse — several routes share one dock entry
	// (/packages and /export both read as "devices", /releases as "updates"). The
	// "More" page directory lists those routes individually and has to highlight
	// the exact one you are on, so it matches on the path instead.
	data["ActivePath"] = path
	// The dock's three shortcuts: the pages this person opens most, from the page-view
	// audit the dashboard already writes. Cached per user for a few minutes — the dock
	// is rendered on every navigation and these habits move over days, not seconds.
	if role != "" && role != "owner" {
		if b, err := json.Marshal(h.frequentPaths(r)); err == nil {
			// template.JS so it lands as raw JSON inside the script tag rather than
			// as an escaped string the page would have to unwrap twice.
			data["FrequentPathsJSON"] = template.JS(b)
		}
	}
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
	case strings.HasPrefix(path, "/compliance"):
		data["ActivePage"] = "compliance"
	case strings.HasPrefix(path, "/lock"), strings.HasPrefix(path, "/kiosk"), strings.HasPrefix(path, "/manage"):
		data["ActivePage"] = "manage"
	case strings.HasPrefix(path, "/commands"):
		data["ActivePage"] = "commands"
	case strings.HasPrefix(path, "/releases"):
		// One dock entry ("Updates") covers releases and rollouts alike.
		data["ActivePage"] = "updates"
	case strings.HasPrefix(path, "/updates"), strings.HasPrefix(path, "/rollouts"), strings.HasPrefix(path, "/packages"):
		// Rollouts has its own dock entry (16 Oct); the pages it used to share tabs with
		// light it up too.
		data["ActivePage"] = "updates"
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
	"commands.html": true, "releases.html": true, "compliance.html": true,
	// Child-rail destinations: moving within a section is the most common
	// navigation in the dashboard, and the rail warms the next page on hover.
	"alerts.html": true, "alert_config.html": true, "report_alerts.html": true,
	"apps.html": true, "app_detail.html": true, "jobs.html": true,
	"command_history.html": true, "manage.html": true,
	"enrollment.html": true, "groups.html": true, "restaurants.html": true,
	"settings.html": true,
	"packages.html": true, "users.html": true, "activity.html": true,
	"users_access.html": true,
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
// frequentPaths backs the dock's three personal shortcuts. It is deliberately a
// cached read: a miss costs one indexed GROUP BY, a hit costs nothing, and the
// answer changing five minutes late is not something anyone can perceive.
//
// The four fixed dock destinations are excluded here so the dock can never show a
// page twice. Whether the viewer may actually OPEN one of these is decided in the
// template, not here — the page index carries the role guards, and the dock
// resolves these paths through it, so a path the role cannot reach renders nothing.
var (
	freqMu    sync.Mutex
	freqCache = map[string]freqEntry{}
)

type freqEntry struct {
	at    time.Time
	paths []string
}

const freqTTL = 5 * time.Minute

func (h *Handler) frequentPaths(r *http.Request) []string {
	user := h.currentUsername(r)
	if user == "" {
		return nil
	}
	freqMu.Lock()
	if e, ok := freqCache[user]; ok && time.Since(e.at) < freqTTL {
		freqMu.Unlock()
		return e.paths
	}
	freqMu.Unlock()

	// Ten candidates, not three. Plenty of people's most-opened paths are device
	// or release detail pages (/devices/AT070AABU00044), which are not navigation
	// destinations and have no entry in the page index — the dock drops those when
	// it resolves them, and asking for exactly three would leave empty slots.
	// Jobs left the dock on 15 Oct and is not to come back as a shortcut either; it
	// stays in More.
	paths, err := h.db.TopPagesForUser(r.Context(), user, 30*24*time.Hour, 10,
		[]string{"/", "/devices", "/commands", "/alerts", "/jobs", "/releases", "/rollouts", "/updates"})
	if err != nil {
		return nil
	}
	freqMu.Lock()
	freqCache[user] = freqEntry{at: time.Now(), paths: paths}
	freqMu.Unlock()
	return paths
}

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

// trimTrace caps one stack trace for an inline card. A tombstone runs to tens of
// thousands of lines; twelve of them on the device page's Alerts tab came to half a
// megabyte, and colorizeLogcat turns every line into its own span — enough DOM to
// leave the tab sitting on "Loading alerts…" long after the response arrived. The
// head of a trace holds the exception and the frames that matter; the rest is one
// click away on the Alerts page.
func trimTrace(trace string) string {
	const maxLines, maxBytes = 200, 24 * 1024
	if len(trace) <= maxBytes && strings.Count(trace, "\n") < maxLines {
		return trace
	}
	lines := strings.Split(trace, "\n")
	kept, n := lines, 0
	if len(kept) > maxLines {
		kept, n = kept[:maxLines], len(lines)-maxLines
	}
	out := strings.Join(kept, "\n")
	for len(out) > maxBytes && len(kept) > 1 {
		kept = kept[:len(kept)*3/4]
		n = len(lines) - len(kept)
		out = strings.Join(kept, "\n")
	}
	if n > 0 {
		out += fmt.Sprintf("\n… %d more line(s) — open the Alerts page for the full trace.", n)
	}
	return out
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

// pickerScopesJSON is the restaurants + groups list the shared device picker
// offers as one-click shortcuts (templates/_picker.html).
func (h *Handler) pickerScopesJSON(r *http.Request) template.JS {
	ctx := r.Context()
	type scope struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := []scope{}
	if rests, err := h.db.ListRestaurants(ctx); err == nil {
		for _, x := range h.visibleRestaurantList(r, rests) {
			out = append(out, scope{Kind: "restaurant", ID: x.ID.String(), Name: x.Name, Count: x.DeviceCount})
		}
	}
	if groups, err := h.db.ListGroups(ctx); err == nil {
		for _, g := range h.visibleGroupList(r, groups) {
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

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
