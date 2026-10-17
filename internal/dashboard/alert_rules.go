package dashboard

import (
	"context"
	"encoding/json"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mdm/internal/ai"
	"mdm/internal/db"

	"github.com/google/uuid"
)

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
		{"temp_c", "Limit (off pad)", "°C", 1, 52},
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
	{"slow_charge_night", "Slow overnight charging", "Fires when a device charges overnight but its battery gains at most this much over the window and ends below the cutoff (charging slows on purpose as a battery fills).", "Power", []alertParamField{
		{"max_gain_pct", "Max gain", "%", 1, 15},
		{"window_hours", "Over", "h", 1, 2},
		{"below_pct", "Only below", "%", 5, 80},
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
	add("Security", "identity_conflict", "Possible impersonation")
	add("Security", "key_not_registered", "Key not registered after a reset")
	add("Security", "hardware_serial_changed", "Hardware serial mismatch")
	add("Security", "key_recovered", "Key reset automatically")
	add("Kiosk", "kiosk_exited", "Unlocked on the device")
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
	FalseAlarms          int // resolved as "false alarm" in the last 30 days
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
	falseAlarms, _ := h.db.FalseAlarmCounts(ctx, 30)
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
			DeployedOnly: r.DeployedOnly, FalseAlarms: falseAlarms[def.Type],
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
