package dashboard

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── Overview widget layout ────────────────────────────────────────────────────
//
// The Overview is a set of widgets. Each signed-in user can hide widgets, drag them
// between the two columns, or switch to a preset view; the arrangement is saved
// per user (user_layouts) and rendered server-side so there is no reflow on load.

// overviewWidget is one widget's identity and its home slot in the default layout.
type overviewWidget struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Slot  string `json:"slot"` // "top" (full width, fixed order) | "left" | "right"
}

// overviewWidgets is the catalogue, in default order. "top" widgets keep their
// source order and can only be hidden; column widgets can be reordered/moved.
var overviewWidgets = []overviewWidget{
	{"hero", "Fleet health", "top"},
	{"kpis", "Signal strip", "top"},
	{"inbox", "Onboarding inbox", "left"},
	{"sites", "Sites", "left"},
	{"map", "Fleet map", "left"},
	{"activity", "Fleet activity", "left"},
	{"report", "Daily report", "top"},
	{"rollouts", "Rollouts", "right"},
	{"adoption", "Release adoption", "right"},
	{"vitals", "Fleet vitals", "right"},
}

// widgetSize is a widget's footprint on the home-screen grid: W columns (1 = half,
// 2 = full width) and H (1 = compact, body scrolls; 2 = natural; 3 = tall).
type widgetSize struct {
	W int `json:"w"`
	H int `json:"h"`
}

// overviewLayout is what gets stored per user and handed to the template.
//
// The grid is a single two-column canvas ("home screen"): Order lists the column
// widgets in reading order and Sizes gives each its footprint. Left/Right are the
// pre-grid two-column model, kept so saved layouts and presets keep working: when
// Order is empty it is derived by zipping the two columns.
type overviewLayout struct {
	Preset string                `json:"preset"` // default | operations | releases | minimal | custom
	Hidden []string              `json:"hidden"`
	Left   []string              `json:"left,omitempty"`
	Right  []string              `json:"right,omitempty"`
	Order  []string              `json:"order"`
	Sizes  map[string]widgetSize `json:"sizes"`
}

// Size is the template accessor: a widget's footprint with defaults applied. The
// three headline widgets (health hero, signal strip, daily report) default to full
// width; everything else to half.
func (l overviewLayout) Size(id string) widgetSize {
	if s, ok := l.Sizes[id]; ok {
		if s.W < 1 || s.W > 2 {
			s.W = 1
		}
		if s.H < 1 || s.H > 3 {
			s.H = 2
		}
		return s
	}
	if w, ok := overviewWidgetByID(id); ok && w.Slot == "top" {
		return widgetSize{W: 2, H: 2}
	}
	return widgetSize{W: 1, H: 2}
}

// overviewPreset is a named, built-in arrangement.
type overviewPreset struct {
	ID     string
	Label  string
	Blurb  string
	Layout overviewLayout
}

var overviewPresets = []overviewPreset{
	{"default", "Default", "Everything, balanced across two columns.", overviewLayout{
		Preset: "default",
		Left:   []string{"inbox", "sites", "map", "activity"},
		Right:  []string{"rollouts", "adoption", "vitals"},
		Sizes:  map[string]widgetSize{"map": {W: 2, H: 2}},
	}},
	{"operations", "Operations", "Sites, map and vitals for the on-call desk.", overviewLayout{
		Preset: "operations",
		Hidden: []string{"rollouts", "adoption"},
		Left:   []string{"inbox", "sites", "map"},
		Right:  []string{"vitals", "activity"},
	}},
	{"releases", "Releases", "Rollouts and adoption front and centre.", overviewLayout{
		Preset: "releases",
		Hidden: []string{"map", "inbox"},
		Left:   []string{"rollouts", "adoption"},
		Right:  []string{"sites", "vitals", "activity"},
	}},
	{"minimal", "Minimal", "Score, signals and sites only.", overviewLayout{
		Preset: "minimal",
		Hidden: []string{"map", "activity", "rollouts", "adoption", "vitals", "inbox"},
		Left:   []string{"sites"},
		Right:  []string{},
	}},
}

func overviewPresetByID(id string) (overviewPreset, bool) {
	for _, p := range overviewPresets {
		if p.ID == id {
			return p, true
		}
	}
	return overviewPreset{}, false
}

func overviewWidgetByID(id string) (overviewWidget, bool) {
	for _, w := range overviewWidgets {
		if w.ID == id {
			return w, true
		}
	}
	return overviewWidget{}, false
}

// normalizeOverviewLayout drops unknown ids, de-duplicates, keeps "top" widgets out
// of the columns, and appends any catalogue widget the stored layout doesn't mention
// (a widget added after the user saved) to its home column so nothing silently
// disappears. Hidden takes precedence over column membership.
func normalizeOverviewLayout(in overviewLayout) overviewLayout {
	out := overviewLayout{Preset: in.Preset}
	if _, ok := overviewPresetByID(out.Preset); !ok && out.Preset != "custom" {
		out.Preset = "default"
	}
	seen := map[string]bool{}
	hidden := map[string]bool{}
	for _, id := range in.Hidden {
		if _, ok := overviewWidgetByID(id); ok && !hidden[id] {
			hidden[id] = true
			out.Hidden = append(out.Hidden, id)
		}
	}
	take := func(ids []string) []string {
		col := []string{}
		for _, id := range ids {
			_, ok := overviewWidgetByID(id)
			if !ok || seen[id] || hidden[id] {
				continue
			}
			seen[id] = true
			col = append(col, id)
		}
		return col
	}
	// Order: the saved grid order, else the two columns zipped (L0 R0 L1 R1 …) so a
	// pre-grid layout keeps roughly the same picture. Anything the layout does not
	// mention (a widget added later) goes at the end.
	if len(in.Order) > 0 {
		out.Order = take(in.Order)
	} else {
		l, r := in.Left, in.Right
		var zipped []string
		for i := 0; i < len(l) || i < len(r); i++ {
			if i < len(l) {
				zipped = append(zipped, l[i])
			}
			if i < len(r) {
				zipped = append(zipped, r[i])
			}
		}
		out.Order = take(zipped)
	}
	// Widgets the layout doesn't mention: the headline ones (hero, signals, report)
	// go first — that is where a pre-grid layout had them — the rest at the end.
	var lead []string
	for _, w := range overviewWidgets {
		if seen[w.ID] || hidden[w.ID] {
			continue
		}
		if w.Slot == "top" {
			lead = append(lead, w.ID)
		} else {
			out.Order = append(out.Order, w.ID)
		}
	}
	out.Order = append(lead, out.Order...)
	if out.Order == nil {
		out.Order = []string{}
	}
	out.Sizes = map[string]widgetSize{}
	for id, s := range in.Sizes {
		if _, ok := overviewWidgetByID(id); !ok {
			continue
		}
		if s.W < 1 || s.W > 2 {
			s.W = 1
		}
		if s.H < 1 || s.H > 3 {
			s.H = 2
		}
		def := widgetSize{W: 1, H: 2}
		if w, _ := overviewWidgetByID(id); w.Slot == "top" {
			def.W = 2
		}
		if s != def {
			out.Sizes[id] = s
		}
	}
	if out.Hidden == nil {
		out.Hidden = []string{}
	}
	return out
}

// overviewLayoutFor loads the user's saved layout (or the default preset).
func (h *Handler) overviewLayoutFor(r *http.Request) overviewLayout {
	def, _ := overviewPresetByID("default")
	user := h.currentUsername(r)
	if user == "" {
		return normalizeOverviewLayout(def.Layout)
	}
	raw, err := h.db.GetUserLayout(r.Context(), user, "overview")
	if err != nil || len(raw) == 0 {
		return normalizeOverviewLayout(def.Layout)
	}
	var l overviewLayout
	if json.Unmarshal(raw, &l) != nil {
		return normalizeOverviewLayout(def.Layout)
	}
	return normalizeOverviewLayout(l)
}

// overviewLayoutData is the template-facing bundle: the effective layout, the
// widget catalogue (for the customise menu), the presets, and which widgets are
// hidden (title-resolved, for the "show again" list).
func (h *Handler) overviewLayoutData(r *http.Request) map[string]any {
	l := h.overviewLayoutFor(r)
	// The onboarding inbox is admin-only (enrollment is): non-admins never get
	// it rendered, listed in the customise menu, or offered under "show again".
	widgets := overviewWidgets
	if h.access(r).role != "admin" {
		l = dropOverviewWidget(l, "inbox")
		widgets = widgets[:0:0]
		for _, w := range overviewWidgets {
			if w.ID != "inbox" {
				widgets = append(widgets, w)
			}
		}
	}
	hidden := map[string]bool{}
	for _, id := range l.Hidden {
		hidden[id] = true
	}
	var hiddenW []overviewWidget
	for _, w := range widgets {
		if hidden[w.ID] {
			hiddenW = append(hiddenW, w)
		}
	}
	lj, _ := json.Marshal(l)
	wj, _ := json.Marshal(widgets)
	return map[string]any{
		"Layout":        l,
		"LayoutJSON":    template.JS(lj),
		"WidgetsJSON":   template.JS(wj),
		"HiddenWidgets": hiddenW,
		"Presets":       overviewPresets,
		"IsHidden":      hidden,
		"IsCustomized":  l.Preset != "default" || len(l.Hidden) > 0,
	}
}

// OverviewLayoutSave stores the caller's layout. Body is either a full layout
// ({hidden,left,right,preset:"custom"}) or {"preset":"<id>"} to adopt a preset.
func (h *Handler) OverviewLayoutSave(w http.ResponseWriter, r *http.Request) {
	user := h.currentUsername(r)
	if user == "" {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var body overviewLayout
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid layout: "+err.Error())
		return
	}
	var l overviewLayout
	if p, ok := overviewPresetByID(strings.TrimSpace(body.Preset)); ok && body.Left == nil && body.Right == nil && body.Hidden == nil && body.Order == nil {
		l = normalizeOverviewLayout(p.Layout)
	} else {
		body.Preset = "custom"
		l = normalizeOverviewLayout(body)
	}
	raw, _ := json.Marshal(l)
	if err := h.db.SetUserLayout(r.Context(), user, "overview", raw); err != nil {
		log.Printf("overview layout save (%s): %v", user, err)
		writeJSONError(w, http.StatusInternalServerError, "could not save layout")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "layout": l})
}

// OverviewLayoutReset deletes the caller's saved layout (back to the default view).
func (h *Handler) OverviewLayoutReset(w http.ResponseWriter, r *http.Request) {
	user := h.currentUsername(r)
	if user == "" {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	if err := h.db.DeleteUserLayout(r.Context(), user, "overview"); err != nil {
		log.Printf("overview layout reset (%s): %v", user, err)
		writeJSONError(w, http.StatusInternalServerError, "could not reset layout")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ── Fleet map page ────────────────────────────────────────────────────────────

// MapPage renders the dedicated full-screen fleet map with view options (status,
// product, site filters, cluster toggle, map style, site list).
func (h *Handler) MapPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	locs, locCount := h.deviceLocationsJSON(ctx, h.access(r).hidesDPC())
	summary, _ := h.db.GetSummary(ctx, h.connectedSlice())
	h.render(w, r, "map.html", map[string]any{
		"Title":           "Fleet map",
		"DeviceLocations": locs,
		"DeviceMapCount":  locCount,
		"MapsEmbedKey":    h.mapsEmbedKey,
		"Summary":         summary,
	})
}

// ── Guided tour (first-run walkthrough) ───────────────────────────────────────
//
// Shown once per user on their next full page load (new and existing users
// alike), skippable, replayable from the account menu. Completion is stored in
// user_layouts under page "tour" and memoised per process so the per-render
// check is a map lookup, not a query.

var tourSeen sync.Map // username -> true

// showTour reports whether the current user still needs the walkthrough.
func (h *Handler) showTour(r *http.Request) bool {
	user := h.currentUsername(r)
	if user == "" {
		return false
	}
	if _, ok := tourSeen.Load(user); ok {
		return false
	}
	raw, err := h.db.GetUserLayout(r.Context(), user, "tour")
	if err != nil {
		return false // don't nag on a DB hiccup
	}
	if len(raw) > 0 {
		tourSeen.Store(user, true)
		return false
	}
	return true
}

// TourDone records that the user finished or skipped the walkthrough.
func (h *Handler) TourDone(w http.ResponseWriter, r *http.Request) {
	user := h.currentUsername(r)
	if user == "" {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var body struct {
		Status string `json:"status"` // completed | skipped
		Step   int    `json:"step"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	if body.Status != "completed" {
		body.Status = "skipped"
	}
	raw, _ := json.Marshal(map[string]any{"status": body.Status, "step": body.Step, "at": time.Now().UTC().Format(time.RFC3339)})
	if err := h.db.SetUserLayout(r.Context(), user, "tour", raw); err != nil {
		log.Printf("tour done (%s): %v", user, err)
		writeJSONError(w, http.StatusInternalServerError, "could not save")
		return
	}
	tourSeen.Store(user, true)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// TourReset clears the record so the walkthrough shows again (used by "Take the
// tour" in the account menu, which then starts it client-side immediately).
func (h *Handler) TourReset(w http.ResponseWriter, r *http.Request) {
	user := h.currentUsername(r)
	if user == "" {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	_ = h.db.DeleteUserLayout(r.Context(), user, "tour")
	tourSeen.Delete(user)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ServiceWorker serves static/sw.js from the site root so its scope covers the
// whole app (a worker served under /static/ could only control /static/).
func (h *Handler) ServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, "static/sw.js")
}

// dropOverviewWidget removes a widget from every list of a layout.
func dropOverviewWidget(l overviewLayout, id string) overviewLayout {
	rm := func(in []string) []string {
		out := in[:0:0]
		for _, x := range in {
			if x != id {
				out = append(out, x)
			}
		}
		return out
	}
	l.Hidden, l.Left, l.Right, l.Order = rm(l.Hidden), rm(l.Left), rm(l.Right), rm(l.Order)
	return l
}
