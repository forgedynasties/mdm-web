package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── Fleet "+ Filter" value counts (F2, picked 16 Oct) ─────────────────────────
//
// The Fleet toolbar shows only the filters that are set, as chips; "+ Filter" opens a
// field list and picking a field lists its values with how many devices each would
// show. The counts are asked for one field at a time, when that field is opened, over
// the roster's current scope (collection, search and every other filter) with the
// field's own value left out — so a count is exactly what picking that value shows.

// fleetFilterFields maps a query parameter to the DeviceFilter field it sets.
var fleetFilterFields = map[string]func(f *db.DeviceFilter, v string){
	"status":      func(f *db.DeviceFilter, v string) { f.Online = v },
	"kind":        func(f *db.DeviceFilter, v string) { f.AgentKind = v },
	"product":     func(f *db.DeviceFilter, v string) { f.Product = v },
	"build":       func(f *db.DeviceFilter, v string) { f.BuildID = v },
	"battery":     func(f *db.DeviceFilter, v string) { f.Battery = v },
	"ram":         func(f *db.DeviceFilter, v string) { f.RAM = v },
	"temp":        func(f *db.DeviceFilter, v string) { f.Temp = v },
	"kiosk":       func(f *db.DeviceFilter, v string) { f.Kiosk = v },
	"charging":    func(f *db.DeviceFilter, v string) { f.Charging = v },
	"timezone":    func(f *db.DeviceFilter, v string) { f.Timezone = v },
	"onboarding":  func(f *db.DeviceFilter, v string) { f.Onboarding = v },
	"enrolled_by": func(f *db.DeviceFilter, v string) { f.EnrolledBy = v },
	"lifecycle":   func(f *db.DeviceFilter, v string) { f.Lifecycle = v },
	"production": func(f *db.DeviceFilter, v string) {
		f.ProductionID = uuid.Nil
		if id, err := uuid.Parse(v); err == nil {
			f.ProductionID = id
		}
	},
}

// FleetFilterCounts answers GET /devices/filter-counts?field=build&vals=a,b,c&<roster
// query> with {"a": 12, "b": 0, ...}.
func (h *Handler) FleetFilterCounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set, ok := fleetFilterFields[q.Get("field")]
	if !ok {
		http.Error(w, "Unknown field", http.StatusBadRequest)
		return
	}
	var vals []string
	for _, v := range strings.Split(q.Get("vals"), ",") {
		if v = strings.TrimSpace(v); v != "" && len(vals) < 80 {
			vals = append(vals, v)
		}
	}
	base := h.deviceFilterFromRequest(r)
	h.access(r).applyFilter(&base)
	out := make(map[string]int, len(vals))
	for _, v := range vals {
		f := base
		set(&f, v)
		n, err := h.db.CountDevices(r.Context(), f)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		out[v] = n
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
