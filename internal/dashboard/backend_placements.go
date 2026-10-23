package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

// Devices the AIO backend places elsewhere (M2, picked 23 Oct 2026). The Venues page has a
// card "In the AIO backend, not here yet": a row per backend restaurant that lists some
// of the MDM's devices but has no venue, with Create venue (named after it, linked, its
// profile and hours from the backend, those devices assigned), Link to an existing venue,
// or Ignore (test restaurants). A restaurant that has a venue puts "N devices the backend
// lists here are elsewhere → Move here" on that venue's banner, and the device page says
// where the backend places a device. Nothing moves until someone clicks (Q4).

// placementDev is one MDM device a backend restaurant lists.
type placementDev struct {
	Serial, Label string
	Venue         string // its MDM venue, "" = none
}

// placementRow is a backend restaurant with no venue that lists MDM devices.
type placementRow struct {
	R     db.BackendRestaurant
	Devs  []placementDev
	Total int // devices the backend lists there
	Test  bool
}

// EnvLabel is "Prod" / "Stage".
func (p placementRow) EnvLabel() string { return aiobackend.Env(p.R.Env).Label() }

// Summary is "2 kiosks here have no venue", "1 menu board here is at AIO Lab", or a mix.
func (p placementRow) Summary() string { return placementSummary(p.Devs) }

func placementSummary(ds []placementDev) string {
	if len(ds) == 0 {
		return ""
	}
	none, elsewhere := 0, map[string]int{}
	for _, d := range ds {
		if d.Venue == "" {
			none++
		} else {
			elsewhere[d.Venue]++
		}
	}
	noun := "device"
	if same := ds[0].Label; same != "" {
		allSame := true
		for _, d := range ds {
			allSame = allSame && d.Label == same
		}
		if allSame {
			noun = strings.ToLower(same)
		}
	}
	var parts []string
	if none > 0 {
		parts = append(parts, fmt.Sprintf("%s here %s no venue", countNoun(none, noun), map[bool]string{true: "has", false: "have"}[none == 1]))
	}
	var vs []string
	for v := range elsewhere {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s at %s", countNoun(elsewhere[v], noun), v))
	}
	return strings.Join(parts, " · ")
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "s") {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

var testRestaurantRe = regexp.MustCompile(`(?i)\b(qa|test|testing|demo)\b`)

func bkKey(env string, id int64) string { return env + "/" + strconv.FormatInt(id, 10) }

// placements is what the Venues page, the venue banner and the device page read.
type placements struct {
	Rows  []placementRow
	Moves map[uuid.UUID][]placementDev // venue → its restaurant's devices that are elsewhere
	// Of each device (by serial): the linked venue the backend places it at, when that is
	// not where it is ("" when none), and the unlinked restaurants listing it.
	DeviceVenue map[string]uuid.UUID
	DeviceRest  map[string][]db.BackendRestaurant
}

// buildPlacements matches the MDM's devices (serial, or Nugget Android ID) to the backend
// restaurants listing them.
func buildPlacements(devices []db.Device, venueNames map[uuid.UUID]string, links map[uuid.UUID]db.VenueBackendLink,
	rs []db.BackendRestaurant, bds []db.BackendDevice, ignored map[string]bool) placements {
	out := placements{Moves: map[uuid.UUID][]placementDev{}, DeviceVenue: map[string]uuid.UUID{}, DeviceRest: map[string][]db.BackendRestaurant{}}
	venueOf := map[string]uuid.UUID{}
	for v, l := range links {
		venueOf[bkKey(l.Env, l.RestaurantID)] = v
	}
	rest := map[string]db.BackendRestaurant{}
	for _, r := range rs {
		rest[bkKey(r.Env, r.ID)] = r
	}
	where := map[string][]string{}
	total := map[string]int{}
	for _, d := range bds {
		if d.Revoked {
			continue
		}
		k := bkKey(d.Env, d.RestaurantID)
		total[k]++
		if s := strings.ToUpper(strings.TrimSpace(d.Serial)); s != "" {
			where[s] = append(where[s], k)
		}
	}
	rows := map[string]*placementRow{}
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() {
			continue
		}
		pd := placementDev{Serial: d.SerialNumber, Label: d.ClassLabel()}
		if d.RestaurantID != nil {
			pd.Venue = venueNames[*d.RestaurantID]
		}
		seen := map[string]bool{}
		for _, id := range deviceBackendIDs(d) {
			for _, k := range where[id] {
				if seen[k] {
					continue
				}
				seen[k] = true
				if v, ok := venueOf[k]; ok {
					if d.RestaurantID == nil || *d.RestaurantID != v {
						out.Moves[v] = append(out.Moves[v], pd)
						out.DeviceVenue[d.SerialNumber] = v
					}
					continue
				}
				r, ok := rest[k]
				if !ok || ignored[k] {
					continue
				}
				out.DeviceRest[d.SerialNumber] = append(out.DeviceRest[d.SerialNumber], r)
				row := rows[k]
				if row == nil {
					row = &placementRow{R: r, Total: total[k], Test: testRestaurantRe.MatchString(r.Name)}
					rows[k] = row
				}
				row.Devs = append(row.Devs, pd)
			}
		}
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, *r)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.Test != b.Test {
			return !a.Test
		}
		if len(a.Devs) != len(b.Devs) {
			return len(a.Devs) > len(b.Devs)
		}
		return a.R.Name < b.R.Name
	})
	return out
}

// loadPlacements gathers everything buildPlacements needs (whole fleet).
func (h *Handler) loadPlacements(ctx context.Context) (placements, []db.Restaurant, error) {
	var p placements
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		return p, nil, err
	}
	venues, err := h.db.ListRestaurants(ctx)
	if err != nil {
		return p, nil, err
	}
	names := map[uuid.UUID]string{}
	for _, v := range venues {
		names[v.ID] = v.Name
	}
	links, err := h.db.VenueBackendLinks(ctx)
	if err != nil {
		return p, nil, err
	}
	rs, err := h.db.ListBackendRestaurants(ctx)
	if err != nil {
		return p, nil, err
	}
	bds, err := h.db.ListBackendDevices(ctx)
	if err != nil {
		return p, nil, err
	}
	ign, err := h.db.BackendIgnores(ctx)
	if err != nil {
		return p, nil, err
	}
	return buildPlacements(devices, names, links, rs, bds, ign), venues, nil
}

// venuesPlacementView is the Venues page card: the rows, and the venues that can still be
// linked (for Link to…).
type venuesPlacementView struct {
	Rows     []placementRow
	Devices  int
	Unlinked []db.Restaurant
}

func (h *Handler) venuesPlacementView(ctx context.Context) *venuesPlacementView {
	p, venues, err := h.loadPlacements(ctx)
	if err != nil || len(p.Rows) == 0 {
		return nil
	}
	links, _ := h.db.VenueBackendLinks(ctx)
	v := &venuesPlacementView{Rows: p.Rows}
	seen := map[string]bool{}
	for _, r := range p.Rows {
		for _, d := range r.Devs {
			if !seen[d.Serial] {
				seen[d.Serial] = true
				v.Devices++
			}
		}
	}
	for _, x := range venues {
		if _, linked := links[x.ID]; !linked {
			v.Unlinked = append(v.Unlinked, x)
		}
	}
	return v
}

// venueMovesTodo is the banner line on a linked venue whose restaurant lists devices that
// are elsewhere in the MDM.
func venueMovesTodo(devs []placementDev) (ovTodo, bool) {
	if len(devs) == 0 {
		return ovTodo{}, false
	}
	title := fmt.Sprintf("%d devices the AIO backend lists here are elsewhere", len(devs))
	if len(devs) == 1 {
		title = "1 device the AIO backend lists here is elsewhere"
	}
	var bits []string
	for i, d := range devs {
		if i == 3 {
			bits = append(bits, fmt.Sprintf("+%d more", len(devs)-3))
			break
		}
		at := "no venue"
		if d.Venue != "" {
			at = "at " + d.Venue
		}
		bits = append(bits, d.Serial+" "+at)
	}
	return ovTodo{Title: title, Sub: strings.Join(bits, " · "), Action: "Move here", Post: "backend-move",
		Fields: [][2]string{{"all", "1"}}}, true
}

// deviceBackendLine is the device page's line about where the backend places it.
type deviceBackendLine struct {
	Text, Sub string
	VenueID   string // a linked venue to move/assign it to ("" = none)
	Action    string // "Move to X" / "Assign to X"
}

func (h *Handler) deviceBackendLine(ctx context.Context, d *db.Device) *deviceBackendLine {
	bds, err := h.db.BackendDevicesFor(ctx, deviceBackendIDs(d))
	if err != nil || len(bds) == 0 {
		return nil
	}
	links, err := h.db.VenueBackendLinks(ctx)
	if err != nil {
		return nil
	}
	rs, err := h.db.ListBackendRestaurants(ctx)
	if err != nil {
		return nil
	}
	venues, _ := h.db.ListRestaurants(ctx)
	names := map[uuid.UUID]string{}
	for _, v := range venues {
		names[v.ID] = v.Name
	}
	ign, _ := h.db.BackendIgnores(ctx)
	p := buildPlacements([]db.Device{*d}, names, links, rs, bds, ign)
	noun := strings.ToLower(d.ClassLabel())
	if noun == "" {
		noun = "device"
	}
	if v, ok := p.DeviceVenue[d.SerialNumber]; ok {
		l := deviceBackendLine{VenueID: v.String(), Text: fmt.Sprintf("AIO backend says this %s is at %s", noun, names[v])}
		if d.RestaurantID == nil {
			l.Action, l.Sub = "Assign to "+names[v], "it has no venue here"
		} else {
			l.Action, l.Sub = "Move to "+names[v], "it is at "+names[*d.RestaurantID]+" here"
		}
		return &l
	}
	if rr := p.DeviceRest[d.SerialNumber]; len(rr) > 0 {
		r := rr[0]
		sub := fmt.Sprintf("%s #%d", aiobackend.Env(r.Env).Label(), r.ID)
		if r.City != "" {
			sub += " · " + r.City
		}
		sub += " · no venue in the MDM yet: create or link it on Venues"
		return &deviceBackendLine{Text: fmt.Sprintf("AIO backend says this %s is at %s", noun, r.Name), Sub: sub}
	}
	return nil
}

// assignToVenue puts devices in a venue the way the device page does it (audit, live
// update, the guest frame).
func (h *Handler) assignToVenue(r *http.Request, serials []string, venue uuid.UUID) int {
	n := 0
	for _, s := range serials {
		if err := h.db.AssignDeviceToRestaurant(r.Context(), s, &venue); err != nil {
			log.Printf("[backend-place] assign %s: %v", s, err)
			continue
		}
		n++
		if d, err := h.db.GetDevice(r.Context(), s); err == nil {
			h.auditDev(r, "device.restaurant", d.ID, s, venue.String()+" (AIO backend)")
			h.hub.PublishDeviceUpdate(d.ID)
			h.pushGuestToDevices(r.Context(), []uuid.UUID{d.ID})
		}
	}
	return n
}

// linkAndFill links a venue to a backend restaurant and fills it from there (P2), then
// assigns the restaurant's MDM devices to it.
func (h *Handler) linkAndFill(r *http.Request, venue uuid.UUID, env string, rid int64, serials []string) int {
	ctx := r.Context()
	if err := h.db.SetVenueBackendLink(ctx, venue, env, rid, h.currentUsername(r)); err != nil {
		log.Printf("[backend-place] link %s: %v", venue, err)
		return 0
	}
	if err := h.refreshBackendHours(ctx, db.VenueBackendLink{Env: env, RestaurantID: rid}); err != nil {
		log.Printf("[backend-place] hours: %v", err)
	}
	if _, err := h.applyBackendToVenue(ctx, venue); err != nil {
		log.Printf("[backend-place] apply: %v", err)
	}
	return h.assignToVenue(r, serials, venue)
}

// BackendPlaceSave is POST /restaurants/backend-place: action create | link (venue) |
// ignore, for one backend restaurant (env + rid).
func (h *Handler) BackendPlaceSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	ctx := r.Context()
	env := r.FormValue("env")
	rid, _ := strconv.ParseInt(r.FormValue("rid"), 10, 64)
	br, ok, err := h.db.BackendRestaurantOf(ctx, env, rid)
	if err != nil || !ok {
		http.Error(w, "That restaurant isn't in the AIO backend copy", http.StatusBadRequest)
		return
	}
	if r.FormValue("action") == "ignore" {
		if err := h.db.IgnoreBackendRestaurant(ctx, env, rid, h.currentUsername(r)); err != nil {
			http.Error(w, "Could not save", http.StatusInternalServerError)
			return
		}
		h.audit(r, "backend.ignore", bkKey(env, rid), br.Name)
		http.Redirect(w, r, flashURL("/coverage", br.Name+" ignored", "success"), http.StatusSeeOther)
		return
	}
	p, _, err := h.loadPlacements(ctx)
	if err != nil {
		http.Error(w, "Could not read the backend copy", http.StatusInternalServerError)
		return
	}
	var serials []string
	for _, row := range p.Rows {
		if row.R.Env == env && row.R.ID == rid {
			for _, d := range row.Devs {
				serials = append(serials, d.Serial)
			}
		}
	}
	var venue uuid.UUID
	switch r.FormValue("action") {
	case "create":
		v, err := h.db.CreateRestaurant(ctx, db.Restaurant{Name: br.Name})
		if err != nil {
			http.Error(w, "Could not create the venue: "+err.Error(), http.StatusBadRequest)
			return
		}
		venue = v.ID
		h.audit(r, "restaurant.create", venue.String(), br.Name+" (from AIO backend)")
	case "link":
		venue, err = uuid.Parse(r.FormValue("venue"))
		if err != nil {
			http.Error(w, "Pick a venue", http.StatusBadRequest)
			return
		}
		if _, linked, _ := h.db.VenueBackendLinkOf(ctx, venue); linked {
			http.Error(w, "That venue is already linked to a backend restaurant", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}
	n := h.linkAndFill(r, venue, env, rid, serials)
	h.audit(r, "restaurant.backend_link", venue.String(), fmt.Sprintf("%s · %s #%d · %d devices assigned", br.Name, aiobackend.Env(env).Label(), rid, n))
	http.Redirect(w, r, flashURL("/restaurants/"+venue.String(), fmt.Sprintf("%s linked · %s assigned", br.Name, countNoun(n, "device")), "success"), http.StatusSeeOther)
}

// RestaurantBackendMove is POST /restaurants/{id}/backend-move: move here the devices this
// venue's backend restaurant lists (all=1, or one serial).
func (h *Handler) RestaurantBackendMove(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	p, _, err := h.loadPlacements(r.Context())
	if err != nil {
		http.Error(w, "Could not read the backend copy", http.StatusInternalServerError)
		return
	}
	one := r.FormValue("serial")
	var serials []string
	for _, d := range p.Moves[id] {
		if one == "" || d.Serial == one {
			serials = append(serials, d.Serial)
		}
	}
	n := h.assignToVenue(r, serials, id)
	back := "/restaurants/" + id.String()
	if one != "" && r.FormValue("from") == "device" {
		back = "/devices/" + one
	}
	http.Redirect(w, r, flashURL(back, countNoun(n, "device")+" moved here", "success"), http.StatusSeeOther)
}

// Initial is the restaurant's first letter, for its tile.
func (p placementRow) Initial() string {
	for _, c := range strings.TrimSpace(p.R.Name) {
		return strings.ToUpper(string(c))
	}
	return "?"
}

// Serials lists the devices' serials for the row's note.
func (p placementRow) Serials() string {
	var s []string
	for _, d := range p.Devs {
		s = append(s, d.Serial)
	}
	return strings.Join(s, ", ")
}
