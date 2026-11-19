package dashboard

import (
	"net/http"
	"sort"
	"strings"


	"mdm/internal/db"
)

// Role homes (roles replan, decided 30 Oct 2026): each role signs in to a page built for
// its job (Root redirects). Field → Setup at /setup/mine (the devices they enrolled, waiting
// for a venue); Ops lead → Setup at /setup (the queue to place). Support lands on Today
// (alerts), Viewers on Venues, Engineers on Fleet filtered to Lab.

// MySetup: GET /setup/mine, Field's home ("Setup"). The devices they enrolled and not placed
// yet, each with its role picker, any venue and Deploy; placing one puts it in service and it
// leaves their list (simplified 19 Nov 2026: no Setting up, no Hand over). The Ops lead and
// super admin may open it too (it lists their own enrolments).
func (h *Handler) MySetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	role := h.role(r)
	if role != "field" && !mayManageVisits(role) {
		http.Error(w, "This is the Field home.", http.StatusForbidden)
		return
	}
	queue, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateToPlace, EnrolledBy: h.currentUsername(r)}, 0, 500, "serial", "asc")
	place := h.placeRowsFor(ctx, queue)
	var all []db.Restaurant
	if len(place) > 0 {
		all, _ = h.db.ListRestaurants(ctx)
		sort.SliceStable(all, func(i, j int) bool { return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name) })
	}
	// The others in the queue enrolled by colleagues are not shown: how many, as a line.
	others := 0
	if len(place) > 0 || role != "field" {
		if n, err := h.db.CountDevices(ctx, db.DeviceFilter{State: db.StateToPlace}); err == nil && n > len(place) {
			others = n - len(place)
		}
	}
	h.render(w, r, "my_visits.html", map[string]any{
		"Title": "Setup", "ActivePage": "myvisits",
		"Place": place, "Venues": all, "Others": others, "EnrollAndroid": h.enrollAndroidView(r),
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
	"lineup": true, "models": true, "clients": true, "host-apps": true, "compliance": true, "jobs": true,
	"lock": true, "adb": true, "users": true, "settings": true, "server": true, "enrollment": true,
	"coverage": true, "groups": true, "map": true, "fleet-health": true, "battery": true,
	"activity": true, "alert-config": true, "reports": true, "productions": true,
	"setup": true, "pipeline": true, "boot-logo": true,
	"commands": true, "kiosk": true, "manage": true, "packages": true, "schedules": true,
}

// curatedAllow is what each restricted role may open. Field's "devices" means device
// pages only: their list is My visits. Engineers open groups (access by levels, 2 Nov
// 2026): group access is how they are given more, and the Fleet rail's Groups tab is theirs.
var curatedAllow = map[string]map[string]bool{
	"field":    {"setup": true, "devices": true, "alerts": true},
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
			seg == "restaurants" && strings.HasSuffix(rest, "/icon") || // venue icons show on every page
			seg == "users" && strings.HasSuffix(rest, "/avatar.png") // profile pictures too (3 Nov 2026)
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

// EnrollGuide: GET /setup/mine/enroll, Field's "Enroll" (the Enrollment page is admin only):
// get the app, enroll in three steps (the guide video in a popup), then place what they
// enrolled (option C, picked 19 Nov 2026).
func (h *Handler) EnrollGuide(w http.ResponseWriter, r *http.Request) {
	queue, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{State: db.StateToPlace, EnrolledBy: h.currentUsername(r)}, 0, 500, "serial", "asc")
	waiting := h.placeRowsFor(r.Context(), queue)
	if len(waiting) > 6 {
		waiting = waiting[:6]
	}
	h.render(w, r, "my_enroll.html", map[string]any{
		"Title": "Enroll", "ActivePage": "enroll", "EnrollAndroid": h.enrollAndroidView(r),
		"Waiting": waiting, "WaitingN": len(queue),
	})
}

// oldSetupURL redirects an old Setup address (/visits, /my-visits, /my-visits/enroll) to its
// new home, keeping the query string (19 Nov 2026).
func oldSetupURL(to string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dest := to
		if q := r.URL.RawQuery; q != "" {
			dest += "?" + q
		}
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
	})
}
