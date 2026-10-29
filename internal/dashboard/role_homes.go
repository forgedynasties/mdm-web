package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Role homes (roles replan, decided 30 Oct 2026): each role signs in to a page built for
// its job (Root redirects). Field → My visits (F2: Fleet's layout, visits in the left list,
// one table with each device's checklist and Hand over); Ops lead → Setup at /visits (the Overview's
// layout about setup visits). Support keeps the Overview, Sales lands on Venues, Engineers
// on Fleet filtered to Lab.

type visitDeviceRow struct {
	Device  db.Device
	State   string
	Checks  []handOverCheck
	Ready   bool
	CanHand bool
	Online  bool
	Visit   string // the visit's purpose (or the venue) it is listed under
}

// MyVisits: GET /my-visits. Field sees their open visits; the Ops lead and super admin
// see every open visit.
func (h *Handler) MyVisits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	role := h.role(r)
	var visits []db.SetupVisit
	var err error
	switch {
	case role == "field":
		visits, err = h.db.OpenVisitsFor(ctx, h.currentUsername(r))
	case mayManageVisits(role):
		visits, err = h.db.OpenVisits(ctx)
	default:
		http.Error(w, "My visits is the Field home.", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	sel := r.URL.Query().Get("visit")
	scopes, _ := h.db.DeviceScopes(ctx)
	seen := map[uuid.UUID]bool{}
	var rows []visitDeviceRow
	setting, ready := 0, 0
	for _, v := range visits {
		if sel != "" && sel != v.ID.String() {
			continue
		}
		if seen[v.RestaurantID] {
			continue
		}
		seen[v.RestaurantID] = true
		devs, err := h.db.ListRestaurantDevices(ctx, v.RestaurantID)
		if err != nil {
			continue
		}
		for i := range devs {
			d := &devs[i]
			if d.Hidden {
				continue
			}
			row := visitDeviceRow{Device: *d, Visit: v.Restaurant, Online: h.hub.IsConnectedForDisplay(d.ID)}
			row.State = db.DeviceState(d.RestaurantID, scopes[d.ID].InService)
			if row.State == db.StateSettingUp {
				cfg, _ := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
				row.Checks, row.Ready = h.handOverChecks(ctx, d, cfg != nil && cfg.KioskEnabled)
				row.CanHand = h.mayHandOver(r, d)
				setting++
				if row.Ready {
					ready++
				}
			}
			rows = append(rows, row)
		}
	}
	// Setting up first (ready ones on top), then in service; serial within.
	rank := func(x visitDeviceRow) int {
		switch {
		case x.State == db.StateSettingUp && x.Ready:
			return 0
		case x.State == db.StateSettingUp:
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank(rows[i]) != rank(rows[j]) {
			return rank(rows[i]) < rank(rows[j])
		}
		return rows[i].Device.SerialNumber < rows[j].Device.SerialNumber
	})
	h.render(w, r, "my_visits.html", map[string]any{
		"Title": "My visits", "ActivePage": "myvisits",
		"Visits": visits, "Sel": sel, "Rows": rows, "SettingUp": setting, "ReadyN": ready, "Now": time.Now(),
	})
}

type setupTodo struct {
	Title, Sub, Action, Href string
}

// SetupHome: GET /visits, the Ops lead's home ("Setup"; /setup is an old redirect to Apps).
func (h *Handler) SetupHome(w http.ResponseWriter, r *http.Request) {
	if !mayManageVisits(h.role(r)) {
		http.Error(w, "Setup is the Ops lead's home.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	visits, _ := h.db.OpenVisits(ctx)
	setting, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateSettingUp}, 0, 2000, "serial", "asc")
	week, _ := h.db.HandedOverSince(ctx, time.Now().AddDate(0, 0, -7))
	onVisit := map[uuid.UUID]bool{}
	people := map[string]bool{}
	endsThisWeek := 0
	for _, v := range visits {
		onVisit[v.RestaurantID] = true
		for _, p := range v.People {
			people[p] = true
		}
		if time.Until(v.EndsAt) < 7*24*time.Hour {
			endsThisWeek++
		}
	}
	type venueAgg struct {
		ID           uuid.UUID
		Name         string
		Total, Ready int
	}
	agg := map[uuid.UUID]*venueAgg{}
	var order []uuid.UUID
	noVisit := 0
	for i := range setting {
		d := &setting[i]
		if d.RestaurantID == nil || d.Hidden {
			continue
		}
		a := agg[*d.RestaurantID]
		if a == nil {
			a = &venueAgg{ID: *d.RestaurantID, Name: d.RestaurantName}
			agg[a.ID] = a
			order = append(order, a.ID)
		}
		a.Total++
		cfg, _ := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
		if _, ok := h.handOverChecks(ctx, d, cfg != nil && cfg.KioskEnabled); ok {
			a.Ready++
		}
		if !onVisit[a.ID] {
			noVisit++
		}
	}
	var todos []setupTodo
	for _, id := range order {
		a := agg[id]
		if !onVisit[id] {
			todos = append(todos, setupTodo{Title: fmt.Sprintf("%s: %d device%s setting up, no visit open", a.Name, a.Total, plural(a.Total)),
				Sub: "nobody can finish them: open a visit or hand them over", Action: "Open a visit", Href: visitBack(id)})
		}
	}
	for _, id := range order {
		if a := agg[id]; a.Ready > 0 {
			todos = append(todos, setupTodo{Title: fmt.Sprintf("%s: %d ready to hand over", a.Name, a.Ready),
				Sub: "the checklist is green", Action: "Hand over", Href: visitBack(id)})
		}
	}
	for _, v := range visits {
		if left := time.Until(v.EndsAt); left < 24*time.Hour {
			todos = append(todos, setupTodo{Title: v.Restaurant + " visit ends within a day",
				Sub: joinAnd(v.People) + " · extend it if the work isn't done", Action: "Extend", Href: visitBack(v.RestaurantID)})
		}
	}
	readyAll, totalAll := 0, 0
	for _, a := range agg {
		readyAll += a.Ready
		totalAll += a.Total
	}
	pct := 100
	if totalAll > 0 {
		pct = readyAll * 100 / totalAll
	}
	h.render(w, r, "ops_setup.html", map[string]any{
		"Title": "Setup", "ActivePage": "setup",
		"Visits": visits, "Todos": todos, "SettingUp": totalAll, "Ready": readyAll, "NoVisit": noVisit,
		"Week": week, "People": len(people), "EndsThisWeek": endsThisWeek, "Agg": agg,
		"Pct": pct, "RingOffset": fmt.Sprintf("%.1f", 326.7*(1-float64(pct)/100)),
	})
}

// dockSets is each curated role's dock (roles replan: "each role gets its own home, the
// dock holds only its pages"). Roles not listed keep the full dock as it was.
var dockSets = map[string]map[string]bool{
	"fleet_manager": {"setup": true, "overview": true, "venues": true, "fleet": true, "more": true, "search": true},
	"operator":      {"overview": true, "venues": true, "fleet": true, "more": true, "search": true},
	"support":       {"overview": true, "venues": true, "fleet": true, "more": true, "search": true},
	"engineer":      {"fleet": true, "apps": true, "releases": true, "search": true},
	"field":         {"myvisits": true},
	"viewer":        {"venues": true},
}

// dockShows reports whether the role's dock has the entry.
func dockShows(role, key string) bool {
	set, curated := dockSets[role]
	if !curated {
		return key != "myvisits" && key != "setup" || role == "admin" && key == "setup"
	}
	return set[key]
}

// curatedPages: the dashboard's top-level pages, by first path segment. A curated role
// that opens one it doesn't have is sent to its home; anything else (static files, the
// APIs, live updates, device sub-pages) passes untouched and keeps its own guards.
var curatedPages = map[string]bool{
	"devices": true, "restaurants": true, "alerts": true, "apps": true, "releases": true,
	"lineup": true, "clients": true, "host-apps": true, "compliance": true, "jobs": true,
	"lock": true, "adb": true, "users": true, "settings": true, "server": true, "enrollment": true,
	"coverage": true, "groups": true, "map": true, "fleet-health": true, "battery": true,
	"activity": true, "alert-config": true, "reports": true, "productions": true,
	"visits": true, "my-visits": true, "pipeline": true, "sneak-peek": true, "boot-logo": true,
	"commands": true, "kiosk": true, "manage": true, "packages": true, "schedules": true,
}

// curatedAllow is what each restricted role may open. Field's "devices" means device
// pages only: their list is My visits.
var curatedAllow = map[string]map[string]bool{
	"field":    {"my-visits": true, "devices": true},
	"viewer":   {"restaurants": true},
	"engineer": {"devices": true, "apps": true, "releases": true, "alerts": true},
}

// CurateByRole keeps Field, Sales and Engineers on their own pages (the server side of the
// curated dock).
func (h *Handler) CurateByRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		seg, rest, _ := strings.Cut(path, "/")
		if !curatedPages[seg] {
			next.ServeHTTP(w, r)
			return
		}
		allow, curated := curatedAllow[h.role(r)]
		if !curated {
			next.ServeHTTP(w, r)
			return
		}
		ok := allow[seg]
		if seg == "devices" && h.role(r) == "field" && rest == "" {
			ok = false // the list page; device pages are fine
		}
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}
