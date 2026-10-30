package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

const pageSize = 25

// connectedSlice returns the device IDs shown as present, as a slice, for DB queries
// that compute online counts and filters from real presence (the ws.Hub) instead of
// check-in recency: live sockets, plus the display grace and MDM-lite check-in
// presence (ConnectedIDsForDisplay), so a counter never says "0 of 1 online" beside
// a card that says Reporting. Empty slice = nobody online.
func (h *Handler) connectedSlice() []uuid.UUID {
	set := h.hub.ConnectedIDsForDisplay()
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// deviceFilterFromRequest builds the roster DeviceFilter from the same query
// params DeviceList reads (search/group/restaurant/build/battery/etc, plus the
// view=restaurant|group scoping and the admin-only hidden=only override). Shared
// with DeviceSelectAllSerials so "Select all N matching" resolves the exact same
// set the roster is currently showing.
func (h *Handler) deviceFilterFromRequest(r *http.Request) db.DeviceFilter {
	f := h.deviceFilterFromRequestRaw(r)
	if ids := h.access(r).visibleIDs(); ids != nil {
		f.OnlyIDs = ids
	}
	return f
}

func (h *Handler) deviceFilterFromRequestRaw(r *http.Request) db.DeviceFilter {
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
	return db.DeviceFilter{
		Search:       r.URL.Query().Get("q"),
		GroupID:      groupID,
		RestaurantID: restaurantID,
		ProductionID: productionID,
		Online:       r.URL.Query().Get("status"),
		BuildID:      r.URL.Query().Get("build"),
		Battery:      r.URL.Query().Get("battery"),
		Kiosk:        r.URL.Query().Get("kiosk"),
		Charging:     r.URL.Query().Get("charging"),
		RAM:          r.URL.Query().Get("ram"),
		Temp:         r.URL.Query().Get("temp"),
		Timezone:     r.URL.Query().Get("timezone"),
		Product:      r.URL.Query().Get("product"),
		// Mixed-fleet axes (see docs/ux-enrollment-refactor-plan.md §3.2).
		AgentKind:           r.URL.Query().Get("kind"),
		Class:               r.URL.Query().Get("class"),
		Onboarding:          r.URL.Query().Get("onboarding"),
		State:               r.URL.Query().Get("state"),
		EnrolledBy:          r.URL.Query().Get("enrolled_by"),
		Lifecycle:           r.URL.Query().Get("lifecycle"),
		Hygiene:             r.URL.Query().Get("hygiene"),
		ActiveThresholdSecs: activeThreshold,
		// Online/offline is live WebSocket presence: the status filter and the pill
		// counts (GetSummaryFiltered) resolve it against this connected set.
		Connected: h.connectedSlice(),
	}
}

// DeviceSelectAllSerials returns every serial matching the roster's current
// filter (not just the current page), backing the "Select all N matching" link
// that appears once a full page of checkboxes is selected but more devices exist
// beyond it.
func (h *Handler) DeviceSelectAllSerials(w http.ResponseWriter, r *http.Request) {
	filter := h.deviceFilterFromRequest(r)
	h.access(r).applyFilter(&filter)
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 10000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	serials := make([]string, len(devices))
	for i, d := range devices {
		serials[i] = d.SerialNumber
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"serials": serials})
}

// defaultSortDir is the direction DeviceList uses when the URL names no sort: time
// newest first, everything else ascending.
func defaultSortDir(sort string) string {
	if sort == "last_seen" || sort == "created_at" {
		return "desc"
	}
	return "asc"
}

func (h *Handler) DeviceList(w http.ResponseWriter, r *http.Request) {
	// A ?page_size=N from the main-page selector persists to config (survives
	// restarts) so the choice sticks across sessions and machines. Capped at 200 to
	// match the selector's own largest option (devices.html) — the clamp used to
	// allow up to 500 via a direct URL param even though the UI never offers past
	// 200, and 500 rows means thousands of DOM nodes (several inline SVGs per row)
	// that measurably slow down the page's client-side render.
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 && n != h.effectiveDisplay(r).PageSize {
			h.cfg.SetPageSize(n)
		}
	}
	pageSize := h.effectiveDisplay(r).PageSize // the viewer's own, else the fleet default
	q := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	dir := r.URL.Query().Get("dir")
	if sort == "" {
		sort = h.effectiveDisplay(r).Sort
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

	activeThreshold := h.cfg.CheckinInterval() * 3
	activeThresholdLabel := fmt.Sprintf("%d min", activeThreshold/60)
	filter := h.deviceFilterFromRequest(r)
	// Right-pane mode ("" = roster, "restaurants"/"groups" = collections grid,
	// "restaurant"/"group" = one collection's detail) — filter.RestaurantID/GroupID
	// already carry the view=restaurant|group scoping resolved in deviceFilterFromRequest.
	view := r.URL.Query().Get("view")
	viewID := r.URL.Query().Get("id")
	restaurantID := filter.RestaurantID
	groupID := filter.GroupID

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
		inactiveN   int
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
		railRels = visibleRail(h.role(r), railRels)
		return err
	})
	run(func() error {
		var err error
		prodCounts, err = h.db.CountDevicesByProduct(r.Context())
		return err
	})
	run(func() error {
		var err error
		// Fleet KPI strip: devices marked inactive (hidden), scoped to the same
		// collection/filters as the roster so the tile matches what's on screen.
		inactiveFilter := filter
		inactiveFilter.Hidden = "only"
		inactiveN, err = h.db.CountDevices(r.Context(), inactiveFilter)
		return err
	})

	wg.Wait()
	close(errCh)
	// Hidden groups (super admin only) never sit in the rail, for anyone (30 Oct 2026).
	// groups comes from ListGroups, which leaves them out already.
	shown := make(map[uuid.UUID]bool, len(groups))
	for _, g := range groups {
		shown[g.ID] = true
	}
	keptRail := railGroups[:0:0]
	for _, g := range railGroups {
		if shown[g.GroupID] {
			keptRail = append(keptRail, g)
		}
	}
	railGroups = keptRail
	for err := range errCh {
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}

	// Someone who sees part of the fleet gets rails, filters and counts over their part:
	// only the restaurants and groups they have a device in, the builds and time zones
	// their devices run, and their own total (30 Sep access audit).
	if acc := h.access(r); acc.hidesDevices() {
		seenR, seenG := acc.visibleCollections()
		railRests, railGroups = scopeHealth(railRests, seenR), scopeHealth(railGroups, seenG)
		keptR := restaurants[:0:0]
		for _, x := range restaurants {
			if seenR[x.ID] > 0 {
				keptR = append(keptR, x)
			}
		}
		restaurants = keptR
		keptG := groups[:0:0]
		for _, x := range groups {
			if seenG[x.ID] > 0 {
				x.DeviceCount = seenG[x.ID]
				keptG = append(keptG, x)
			}
		}
		groups = keptG
		productions = nil
		vis, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{OnlyIDs: acc.visibleIDs()}, 0, 10000, "", "")
		fleetTotal = len(vis)
		prodCounts = map[string]int{}
		bset, tset := map[string]int{}, map[string]bool{}
		builds, timezones = nil, nil
		for _, d := range vis {
			prodCounts[d.Product]++
			if d.BuildID != "" {
				if bset[d.BuildID] == 0 {
					builds = append(builds, d.BuildID)
				}
				bset[d.BuildID]++
			}
			var ex struct {
				TZ string `json:"timezone"`
			}
			_ = json.Unmarshal(d.LatestExtra, &ex)
			if tz := ex.TZ; tz != "" && !tset[tz] {
				tset[tz] = true
				timezones = append(timezones, tz)
			}
		}
		// The release filter lists the build most devices are on first.
		slices.SortStableFunc(builds, func(a, b string) int {
			if bset[a] != bset[b] {
				return bset[b] - bset[a]
			}
			return strings.Compare(a, b)
		})
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	connected := h.hub.ConnectedIDsForDisplay()
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
	for _, k := range []string{"status", "production", "build", "battery", "ram", "temp", "kiosk", "charging", "timezone", "kind", "class", "onboarding", "lifecycle"} {
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
		selectedCollection, selectedCount = productLabel(pk), total
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
	for _, p := range h.productFilters(r.Context()) {
		if n := prodCounts[p.Key]; n > 0 {
			railProducts = append(railProducts, railProduct{p.Key, p.Label, n})
		}
	}
	// Product tabs above the roster: one per product (role) the fleet has, counted in the
	// current scope (collection, search, the Filters panel) without the product itself or
	// the Online/Offline quick views, so the tabs stay put while those change. A product
	// with none in this scope keeps its tab at 0 (so picking a restaurant doesn't make the
	// others vanish); one the fleet has no devices of at all is not shown. Order is by the
	// fleet-wide count, so the tabs don't reshuffle as the scope changes.
	var railClasses []db.ClassCount
	{
		scope := filter
		scope.Class, scope.Online, scope.Battery, scope.Kiosk = "", "", "", ""
		fleet := db.DeviceFilter{Hidden: filter.Hidden, Lifecycle: filter.Lifecycle, OnlyIDs: filter.OnlyIDs}
		fleetN := map[string]int{}
		for _, c := range product.Classes() {
			ff := fleet
			ff.Class = c
			total, err := h.db.CountDevices(r.Context(), ff)
			if err != nil || (total == 0 && c != filter.Class) {
				continue
			}
			f := scope
			f.Class = c
			n, err := h.db.CountDevices(r.Context(), f)
			if err != nil {
				continue
			}
			fleetN[c] = total
			railClasses = append(railClasses, db.ClassCount{Class: c, N: n})
		}
		// (stable insertion sort: "sort" is a query parameter in this handler)
		for i := 1; i < len(railClasses); i++ {
			for j := i; j > 0 && fleetN[railClasses[j].Class] > fleetN[railClasses[j-1].Class]; j-- {
				railClasses[j], railClasses[j-1] = railClasses[j-1], railClasses[j]
			}
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

	// Friendly names ("Host stand") so the roster leads with the name and shows the
	// serial as the subtle line.
	nickIDs := make([]uuid.UUID, 0, len(devices))
	for _, d := range devices {
		nickIDs = append(nickIDs, d.ID)
	}
	nicknames, _ := h.db.GetNicknames(r.Context(), nickIDs)
	if nicknames == nil {
		nicknames = map[uuid.UUID]string{}
	}
	// Which of these devices are reporting to another MDM. One query for the page, the
	// same shape as Online: a device that moved is silent here for a reason, and a
	// fleet list that calls that "offline" sends someone to look for working hardware.
	custody, _ := h.db.DeviceCustodyMap(r.Context(), nickIDs)
	enrollers, _ := h.db.ListEnrollers(r.Context())
	data := map[string]any{
		"Title":                "Devices",
		"Devices":              devices,
		"Custody":              custody,
		"Nicknames":            nicknames,
		"Flapping":             flapping,
		"Total":                total,
		"FleetTotal":           fleetTotal,
		"FilterCount":          filterCount,
		"InactiveCount":        inactiveN,
		"RailGroups":           railGroups,
		"RailRestaurants":      railRests,
		"RailReleases":         railRels,
		"RailProducts":         railProducts,
		"RailClasses":          railClasses,
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
		"DefaultSort":          h.effectiveDisplay(r).Sort,
		"DefaultSortDir":       defaultSortDir(h.effectiveDisplay(r).Sort),
		"Online":               online,
		"Groups":               groups,
		"Restaurants":          restaurants,
		"Productions":          productions,
		"Products":             h.productFilters(r.Context()),
		"Builds":               builds,
		"Timezones":            timezones,
		"FilterGroup":          r.URL.Query().Get("group"),
		"FilterProduction":     r.URL.Query().Get("production"),
		"FilterProduct":        r.URL.Query().Get("product"),
		"FilterStatus":         r.URL.Query().Get("status"),
		"FilterBuild":          r.URL.Query().Get("build"),
		"FilterBattery":        r.URL.Query().Get("battery"),
		"FilterRAM":            r.URL.Query().Get("ram"),
		"FilterTemp":           r.URL.Query().Get("temp"),
		"FilterKiosk":          r.URL.Query().Get("kiosk"),
		"FilterCharging":       r.URL.Query().Get("charging"),
		"FilterTimezone":       r.URL.Query().Get("timezone"),
		"FilterKind":           r.URL.Query().Get("kind"),
		"FilterClass":          r.URL.Query().Get("class"),
		"FilterOnboarding":     r.URL.Query().Get("onboarding"),
		"FilterEnrolledBy":     r.URL.Query().Get("enrolled_by"),
		"Enrollers":            enrollers,
		"Names":                h.actorDisplayNames(r.Context()),
		"FilterLifecycle":      r.URL.Query().Get("lifecycle"),
		"FilterHygiene":        db.HygieneJobLabel(r.URL.Query().Get("hygiene")),
		"Classes":              product.Classes(),
		"FilterHidden":         filter.Hidden,
		"ActiveThresholdSecs":  activeThreshold,
		"ActiveThresholdLabel": activeThresholdLabel,
		"Density":              h.effectiveDisplay(r).Density,
		"MapsEmbedKey":         h.mapsEmbedKey,
	}

	// Tablets refused for a corrupted serial in the last day: they are stored nowhere
	// else, so their cards at the top of the roster are the only place they show up.
	if refused, err := h.db.RecentRefusedCheckins(r.Context(), 24*time.Hour); err == nil && len(refused) > 0 {
		data["RefusedSerials"] = h.refusedCards(r.Context(), refused)
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
		rd := h.withRole(r, data)
		h.tmpl.ExecuteTemplate(w, "device-table", rd)
		// Filters and refreshes change the counts too; keep the headline in step.
		h.tmpl.ExecuteTemplate(w, "fleet-headline-oob", rd)
		return
	}
	if view == "restaurants" || view == "groups" || view == "releases" {
		data["CollRows"], data["CollSummary"] = h.collRows(r, view)
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

// ── Saved fleet views ─────────────────────────────────────────────────────────
