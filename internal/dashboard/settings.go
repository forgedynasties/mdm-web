package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/geolocate"
)

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
		"Title":            "Settings",
		"AgentAPKURL":      h.cfg.AgentAPKURL(),
		"AgentAPKChecksum": h.cfg.AgentAPKChecksum(),
		"AgentAPKHosted":   h.cfg.AgentAPKHosted(),
		"AgentAPKHostedInfo": func() map[string]any {
			sha, name, size, at := h.cfg.AgentAPKHostedInfo()
			return map[string]any{"SHA": sha, "Name": name, "Size": size, "At": at, "URL": h.agentAPKURL(r)}
		}(),
		"AgentAPKFromEnv":       h.cfg.AgentAPKURLVal == "" && os.Getenv("AGENT_APK_URL") != "",
		"FleetAdbKey":           h.fleetAdbKeyView(r),
		// Getting-started checklist (General tab): is at least one venue being scanned?
		"AnyVenueScanning": func() bool { ids, _ := h.db.RestaurantsToScan(r.Context()); return len(ids) > 0 }(),
		"Apps":                  repoApps,
		"Families":              setFams,
		"Suggestions":           setSugg,
		"AppFamilyMode":         setMode,
		"Productions":           productions,
		"DeviceQueries":         deviceQueries,
		"BaseCases":             baseCases,
		"ExtraColumns":          h.cfg.Columns(),
		"LegacyCheckin":         h.cfg.LegacyCheckin(),
		"CommandCanary":         h.cfg.CommandCanary(),
		"LegacyBuilds":          h.cfg.LegacyBuilds(),
		"WlcProducts":           h.cfg.WlcProducts(),
		"CheckinInterval":       h.cfg.CheckinInterval(),
		"ShellEnabled":          h.cfg.ShellEnabled(),
		"RemoteEnabled":         h.cfg.RemoteEnabled(),
		"CommandExpiry":         h.cfg.CommandExpiry(),
		"MaxTargets":            h.cfg.MaxTargets(),
		"RequireReason":         h.cfg.RequireReason(),
		"SessionTimeout":        h.cfg.SessionTimeout(),
		"BrandName":             h.cfg.BrandName(),
		"PageSize":              h.cfg.PageSize(),
		"DefaultSort":           h.cfg.DefaultSort(),
		"Density":               h.cfg.Density(),
		"Use24Hour":             h.cfg.Use24Hour(),
		"AlertWebhookSet":       h.cfg.AlertWebhookURL() != "",
		"AlertRules":            h.buildAlertRuleViews(r.Context()),
		"FleetWindow":           fleetWindow,
		"GroupWindows":          groupWindows,
		"AlertChannels":         channels,
		"ChannelTest":           r.URL.Query().Get("channel_test"),
		"AIKeySet":              h.cfg.AIEnabled(),
		"AIProvider":            h.cfg.AIProvider(),
		"AnthropicModel":        h.cfg.AnthropicModel(),
		"AIBaseURL":             h.cfg.AIBaseURL(),
		"AIDigestEnabled":       h.cfg.AIDigestEnabled(),
		"AIUsage":               aiTotals,
		"AIUsageTotal":          aiTotals.InputTokens + aiTotals.OutputTokens,
		"AIUsageDailyJSON":      template.JS(aiDailyJSON),
		"AutoHideDays":          h.cfg.AutoHideDays(),
		"CheckinRetentionDays":  h.cfg.CheckinRetentionDays(),
		"LogcatRetentionDays":   h.cfg.LogcatRetentionDays(),
		"CheckinSampleSec":      h.cfg.CheckinSampleSec(),
		"CheckinDownsampleDays": h.cfg.CheckinDownsampleDays(),
		"CheckinDownsampleSec":  h.cfg.CheckinDownsampleSec(),
		"LegacyStripCursor":     h.cfg.LegacyStripCursor(),
		"LegacyStripPct":        h.legacyStripPct(r.Context()),
		"MaintenanceMode":       h.cfg.MaintenanceMode(),
		"IgnoreDPCCheckins":     h.cfg.IgnoreDPCCheckins(),
		"DBStats":               dbStats,
		"KioskAllowlist":        strings.Join(h.cfg.KioskAllowlist(), "\n"),
		"OTACutoffRows":         h.otaCutoffRows(r.Context()),
		"OTABuildRows":          h.otaBuildRows(r.Context()),
		"MDMServers":            h.cfg.MDMServers(),
		"LegacyOTAMode":         h.cfg.LegacyOTAMode(),
		"LegacyOTAPort":         os.Getenv("LEGACY_OTA_PORT"),
		"LegacyOTAUpstream":     os.Getenv("LEGACY_OTA_UPSTREAM"),
		"OTAConfigManaged":      h.cfg.OTAConfigManaged(),
		"OTAConfigOpts":         h.cfg.OTAConfigOptions(),
		"OTAConfigLast":         otaConfigLastView(h.cfg),
		"OTAConfigNext":         otaConfigNextView(h.cfg),
		"OTAConfigS3":           h.apk != nil,
		"KioskFleetApps":        kioskFleetApps,
		"GoogleUsage":           googleUsage,
		"GoogleUsageJSON":       template.JS(googleUsageJSON),
		"LearnedAPs":            learnedAPs,
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

// SettingsToggleCommandCanary switches the daily control probe on and off. Off by
// default: it is one queued command per device per day, which is cheap but is still a
// decision about the fleet rather than a default.
func (h *Handler) SettingsToggleCommandCanary(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg.SetCommandCanary(!h.cfg.CommandCanary())
	h.audit(r, "settings.command_canary", "", fmt.Sprintf("%t", h.cfg.CommandCanary()))
	h.settingsToggleResponse(w, r, "/settings/command-canary/toggle", h.cfg.CommandCanary())
}

func (h *Handler) SettingsToggleLegacyCheckin(w http.ResponseWriter, r *http.Request) {
	h.cfg.SetLegacyCheckin(!h.cfg.LegacyCheckin())
	h.settingsToggleResponse(w, r, "/settings/legacy-checkin/toggle", h.cfg.LegacyCheckin())
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
		atoiNonNeg(r.FormValue("checkin_downsample_days")),
		atoiNonNeg(r.FormValue("checkin_downsample_sec")),
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

func (h *Handler) SettingsSetSessionTimeout(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sec := 86400
	if n, err := strconv.Atoi(r.FormValue("timeout")); err == nil && n >= 300 {
		sec = n
	}
	h.cfg.SetSessionTimeout(sec)
	h.store.MaxAge(sec)                   // apply to cookie + codec at runtime
	h.hxRedirect(w, r, h.settingsDest(r)) // may clamp to default — re-render the stored value
}

func (h *Handler) SettingsLogoutAll(w http.ResponseWriter, r *http.Request) {
	// Delete every server-side session, including this one (GB-04/GB-08).
	h.audit(r, "session.logout_all", "", "")
	if err := h.db.DeleteAllSessions(r.Context()); err != nil {
		log.Printf("[session] logout all: %v", err)
	}
	redirectLogin(w, r)
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
