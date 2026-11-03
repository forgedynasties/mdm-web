package dashboard

import (
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
// layout about setup visits). Support lands on Today (alerts), Viewers on Venues, Engineers
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
		devs, err := h.db.ListDevices(ctx, db.DeviceFilter{RestaurantID: v.RestaurantID}, 0, 2000, "serial", "asc")
		if err != nil {
			continue
		}
		for i := range devs {
			d := &devs[i]
			if d.Hidden {
				continue
			}
			row := visitDeviceRow{Device: *d, Visit: v.Restaurant, Online: h.hub.IsConnectedForDisplay(d.ID)}
			row.State = scopes[d.ID].State()
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
	// Field's own enrolments (K2, 3 Nov 2026): the ones waiting for a venue are their queue,
	// the ones being set up anywhere are in the table with the visits' devices.
	var place []placeRow
	var venues []db.Restaurant
	if role == "field" && sel == "" {
		me := h.currentUsername(r)
		queue, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateToPlace, EnrolledBy: me}, 0, 500, "serial", "asc")
		place = h.placeRowsFor(ctx, queue)
		mine, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateSettingUp, EnrolledBy: me}, 0, 500, "serial", "asc")
		have := map[uuid.UUID]bool{}
		for _, x := range rows {
			have[x.Device.ID] = true
		}
		for i := range mine {
			d := &mine[i]
			if d.Hidden || have[d.ID] {
				continue
			}
			row := visitDeviceRow{Device: *d, Visit: d.RestaurantName, Online: h.hub.IsConnectedForDisplay(d.ID), State: db.StateSettingUp}
			cfg, _ := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
			row.Checks, row.Ready = h.handOverChecks(ctx, d, cfg != nil && cfg.KioskEnabled)
			row.CanHand = h.mayHandOver(r, d)
			setting++
			if row.Ready {
				ready++
			}
			rows = append(rows, row)
		}
		if len(place) > 0 {
			venues, _ = h.db.ListRestaurants(ctx)
			sort.SliceStable(venues, func(i, j int) bool { return strings.ToLower(venues[i].Name) < strings.ToLower(venues[j].Name) })
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
		"Place": place, "Venues": venues,
	})
}

// dockSets is each curated role's dock (roles replan: "each role gets its own home, the
// dock holds only its pages"). Roles not listed keep the full dock as it was.
var dockSets = map[string]map[string]bool{
	"fleet_manager": {"setup": true, "today": true, "venues": true, "fleet": true, "more": true, "search": true},
	"operator":      {"today": true, "venues": true, "fleet": true, "more": true, "search": true},
	"support":       {"today": true, "venues": true, "fleet": true, "more": true, "search": true},
	"engineer":      {"fleet": true, "apps": true, "releases": true, "search": true},
	"field":         {"myvisits": true, "today": true, "enroll": true},
	"viewer":        {"venues": true},
}

// dockShows reports whether the role's dock has the entry.
func dockShows(role, key string) bool {
	set, curated := dockSets[role]
	if !curated {
		switch key {
		case "myvisits", "today", "enroll":
			return false
		case "setup":
			return role == "admin"
		}
		return true
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
// pages only: their list is My visits. Engineers open groups (access by levels, 2 Nov
// 2026): group access is how they are given more, and the Fleet rail's Groups tab is theirs.
var curatedAllow = map[string]map[string]bool{
	"field":    {"my-visits": true, "devices": true, "alerts": true},
	"viewer":   {"restaurants": true},
	"engineer": {"devices": true, "apps": true, "releases": true, "alerts": true, "groups": true},
}

// canOpenPage: may this role open the top-level page seg ("restaurants", "groups"…)?
// Links a curated role can't follow are drawn as plain text.
func canOpenPage(role, seg string) bool {
	allow, curated := curatedAllow[role]
	return !curated || allow[seg]
}

// CurateByRole keeps Field, Viewers and Engineers on their own pages (the server side of the
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
		ok := allow[seg] || seg == "enrollment" && rest == "android-app/qr.png" ||
			seg == "restaurants" && strings.HasSuffix(rest, "/icon") // venue icons show on every page
		if seg == "devices" && h.role(r) == "field" && rest == "" {
			ok = false // the list page; device pages are fine
		}
		if !ok {
			// A page load gets the "You don't have access" page with a way home (it used to
			// redirect home without a word). A fragment or fetch (the header's alerts
			// popover, a card loading itself) gets a plain refusal.
			if isNavigation(r) {
				h.errorPage(w, r, http.StatusForbidden, "This page", "It isn't one of the pages for "+rolePlural(h.role(r))+". Your dock lists yours.")
			} else {
				http.Error(w, "Not on your pages", http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isNavigation: a page load (typed, linked or htmx-boosted), not a fragment or a fetch.
func isNavigation(r *http.Request) bool {
	if r.Header.Get("HX-Request") == "true" {
		return r.Header.Get("HX-Boosted") == "true"
	}
	m := r.Header.Get("Sec-Fetch-Mode")
	return m == "" || m == "navigate"
}

// EnrollGuide: GET /my-visits/enroll, Field's "Enroll" (the Enrollment page is admin only):
// the onboarding video with its steps and the AIO Enroll for Android download.
func (h *Handler) EnrollGuide(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "my_enroll.html", map[string]any{
		"Title": "Enroll", "ActivePage": "enroll", "EnrollAndroid": h.enrollAndroidView(r),
	})
}
