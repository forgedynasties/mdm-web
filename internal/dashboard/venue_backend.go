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
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Venue ↔ AIO backend (picked 23 Oct 2026: L2 + U1). Venue settings has an AIO backend row:
// unlinked, it lists the MDM's best guesses with their evidence, a Prod / Stage switch and
// a search; linked, the restaurant, its environment and the last sync with Sync now,
// Change and Unlink. An unlinked venue with a strong guess also gets a banner line "Link
// to X in the AIO backend?" with Link in it (L2). A person always confirms. The venue's
// device table lists the backend's devices the MDM doesn't manage as Unenrolled rows after
// its own (U1), counted in the type tabs.

type backendOpt struct {
	Value, Label string
}

type venueBackendList struct {
	Env, Label string
	Opts       []backendOpt
}

type venueBackendView struct {
	On          bool // the server reads the backend, or holds a copy of it
	Linked      bool
	Link        db.VenueBackendLink
	R           *db.BackendRestaurant // nil when linked to a restaurant the copy no longer has
	EnvLabel    string
	SyncedAt    *time.Time
	SyncErr     string
	Suggestions []venueSuggestion
	Lists       []venueBackendList
	Strong      bool // the first suggestion is good enough for the banner
}

// strongSuggestion: a device listed there, the same name, or a similar name a pin's
// throw away.
func strongSuggestion(s venueSuggestion) bool {
	near := s.Meters >= 0 && s.Meters <= nearMeters
	return s.Serials > 0 || s.NameMatch >= 0.99 || (near && s.NameMatch >= 0.5)
}

func (h *Handler) venueBackendView(ctx context.Context, rest *db.Restaurant, devices []db.Device) venueBackendView {
	var v venueBackendView
	states, err := h.db.BackendSyncStates(ctx)
	if err != nil {
		return v
	}
	v.On = h.backend != nil || len(states) > 0
	if !v.On {
		return v
	}
	link, linked, err := h.db.VenueBackendLinkOf(ctx, rest.ID)
	if err != nil {
		return v
	}
	rs, err := h.db.ListBackendRestaurants(ctx)
	if err != nil {
		return v
	}
	if linked {
		v.Linked, v.Link = true, link
		v.EnvLabel = aiobackend.Env(link.Env).Label()
		st := states[link.Env]
		v.SyncedAt, v.SyncErr = st.SyncedAt, st.Error
		for i := range rs {
			if rs[i].Env == link.Env && rs[i].ID == link.RestaurantID {
				v.R = &rs[i]
				break
			}
		}
	} else {
		bds, err := h.db.ListBackendDevices(ctx)
		if err == nil {
			v.Suggestions = suggestVenueLinks([]db.Restaurant{*rest}, devices, rs, bds)[rest.ID]
			v.Strong = len(v.Suggestions) > 0 && strongSuggestion(v.Suggestions[0])
		}
	}
	for _, env := range aiobackend.Envs {
		l := venueBackendList{Env: string(env), Label: env.Label()}
		for _, r := range rs {
			if r.Env != string(env) {
				continue
			}
			label := r.City
			if r.Status != "" && r.Status != "open" {
				label = strings.TrimSpace(label + " · " + r.Status)
			}
			l.Opts = append(l.Opts, backendOpt{Value: fmt.Sprintf("%s · #%d", r.Name, r.ID), Label: label})
		}
		sort.Slice(l.Opts, func(i, j int) bool { return strings.ToLower(l.Opts[i].Value) < strings.ToLower(l.Opts[j].Value) })
		v.Lists = append(v.Lists, l)
	}
	return v
}

// venueBackendTodo is the L2 banner line for an unlinked venue with a strong guess.
func venueBackendTodo(v venueBackendView) (ovTodo, bool) {
	if v.Linked || !v.Strong {
		return ovTodo{}, false
	}
	s := v.Suggestions[0]
	sub := []string{fmt.Sprintf("%s #%d", aiobackend.Env(s.Restaurant.Env).Label(), s.Restaurant.ID)}
	sub = append(sub, s.Reasons()...)
	return ovTodo{
		Title:   "Link to " + s.Restaurant.Name + " in the AIO backend?",
		Sub:     strings.Join(sub, " · "),
		Action:  "Link",
		Post:    "backend",
		Fields:  [][2]string{{"action", "link"}, {"env", s.Restaurant.Env}, {"rid", strconv.FormatInt(s.Restaurant.ID, 10)}},
		Alt:     "Other…",
		AltHref: "?open=backend#vn-backend",
	}, true
}

// unenrolledRow is a backend device the MDM doesn't manage, as the venue table shows it.
type unenrolledRow struct {
	Serial, Class, Label string
	Onboarded            bool
	Help                 string
}

type venueUnenrolled struct {
	Rows    []unenrolledRow
	ByClass map[string]int // class key → unenrolled rows
	AIDNote string         // "8 tablets and 3 POS apps", listed by Android ID
}

func unenrolledHelp(class string) string {
	if class == prod.ClassT7 {
		return "Tableside AI tablets run our firmware and enrol themselves on first boot from an image with the enrollment key."
	}
	return "Enrol it with the AIO Enroll app over adb, or let a firmware device at this venue find it on the Wi-Fi (Venue settings › Auto-enrol)."
}

// backendTypeNoun names a count of backend devices of one type ("8 tablets").
func backendTypeNoun(t string, n int) string {
	nouns := map[string][2]string{
		"nugget": {"tablet", "tablets"}, "pos": {"POS app", "POS apps"}, "mpos": {"handheld", "handhelds"},
		"kds": {"kitchen display", "kitchen displays"}, "kdx": {"kitchen display", "kitchen displays"},
		"kdc": {"kitchen display", "kitchen displays"}, "kiosk": {"kiosk", "kiosks"}, "tv": {"menu board", "menu boards"},
	}
	w, ok := nouns[t]
	if !ok {
		w = [2]string{t + " device", t + " devices"}
	}
	if n == 1 {
		return "1 " + w[0]
	}
	return fmt.Sprintf("%d %s", n, w[1])
}

// venueUnenrolledView loads the U1 rows for a linked venue (nil when unlinked).
func (h *Handler) venueUnenrolledView(ctx context.Context, venueID uuid.UUID) *venueUnenrolled {
	link, linked, err := h.db.VenueBackendLinkOf(ctx, venueID)
	if err != nil || !linked {
		return nil
	}
	bds, err := h.db.BackendDevicesAt(ctx, link.Env, link.RestaurantID)
	if err != nil || len(bds) == 0 {
		return nil
	}
	known, err := h.db.KnownBackendIDs(ctx)
	if err != nil {
		log.Printf("[venue-backend] known ids: %v", err)
		return nil
	}
	return buildVenueUnenrolled(bds, known)
}

func buildVenueUnenrolled(bds []db.BackendDevice, known map[string]bool) *venueUnenrolled {
	rows, byAID := unenrolledAt(bds, known)
	if len(rows) == 0 && len(byAID) == 0 {
		return nil
	}
	u := &venueUnenrolled{ByClass: map[string]int{}}
	for _, d := range rows {
		c := backendTypeClass(d.Type)
		u.Rows = append(u.Rows, unenrolledRow{Serial: d.Serial, Class: c, Label: prod.ClassLabel(c), Onboarded: d.Onboarded, Help: unenrolledHelp(c)})
		u.ByClass[c]++
	}
	sort.SliceStable(u.Rows, func(i, j int) bool {
		if u.Rows[i].Label != u.Rows[j].Label {
			return u.Rows[i].Label < u.Rows[j].Label
		}
		return u.Rows[i].Serial < u.Rows[j].Serial
	})
	var types []string
	for t := range byAID {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		return byAID[types[i]] > byAID[types[j]] || (byAID[types[i]] == byAID[types[j]] && types[i] < types[j])
	})
	var parts []string
	for _, t := range types {
		parts = append(parts, backendTypeNoun(t, byAID[t]))
	}
	switch len(parts) {
	case 0:
	case 1:
		u.AIDNote = parts[0]
	default:
		u.AIDNote = strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	return u
}

// addUnenrolledTabs counts the U1 rows into the type tabs: a hatched share of each tab,
// and a tab of its own for a type the venue has only unenrolled.
func addUnenrolledTabs(tabs []vpTab, u *venueUnenrolled) []vpTab {
	if u == nil || len(u.Rows) == 0 {
		return tabs
	}
	if len(tabs) == 0 {
		tabs = []vpTab{{Label: "All"}}
	}
	have := map[string]bool{}
	for i := range tabs {
		t := &tabs[i]
		if t.Class == "" {
			t.Unenrolled = len(u.Rows)
		} else {
			t.Unenrolled = u.ByClass[t.Class]
			have[t.Class] = true
		}
	}
	for _, c := range prod.Classes() {
		if n := u.ByClass[c]; n > 0 && !have[c] {
			tabs = append(tabs, vpTab{Class: c, Label: prod.ClassLabel(c), Unenrolled: n})
		}
	}
	for i := range tabs {
		vpTabBar(&tabs[i])
	}
	return tabs
}

var backendPickRe = regexp.MustCompile(`#(\d+)\s*$`)

// backendSyncBusy keeps a Sync now from running beside the job's own sync.
var backendSyncBusy atomic.Bool

// RestaurantBackendSave is POST /restaurants/{id}/backend: action link (env + rid, or env +
// pick from the search), unlink, or sync.
func (h *Handler) RestaurantBackendSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	back := fmt.Sprintf("/restaurants/%s?open=backend#vn-backend", id)
	switch r.FormValue("action") {
	case "unlink":
		if err := h.db.SetVenueBackendLink(r.Context(), id, "", 0, h.currentUsername(r)); err != nil {
			http.Error(w, "Could not save", http.StatusInternalServerError)
			return
		}
		h.audit(r, "restaurant.backend_link", id.String(), "unlinked")
	case "sync":
		if err := h.db.RequestBackendSync(r.Context()); err != nil {
			http.Error(w, "Could not start a sync", http.StatusInternalServerError)
			return
		}
		go h.RunBackendSync(context.WithoutCancel(r.Context()))
		h.audit(r, "restaurant.backend_sync", id.String(), "")
	case "link":
		env := aiobackend.Env(r.FormValue("env"))
		rid, _ := strconv.ParseInt(r.FormValue("rid"), 10, 64)
		if m := backendPickRe.FindStringSubmatch(r.FormValue("pick")); rid == 0 && m != nil {
			rid, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if !env.Valid() || rid <= 0 {
			http.Error(w, "Pick a restaurant from the list", http.StatusBadRequest)
			return
		}
		rs, err := h.db.ListBackendRestaurants(r.Context())
		if err != nil {
			http.Error(w, "Could not read the backend copy", http.StatusInternalServerError)
			return
		}
		name := ""
		for _, x := range rs {
			if x.Env == string(env) && x.ID == rid {
				name = x.Name
			}
		}
		if name == "" {
			http.Error(w, fmt.Sprintf("No restaurant #%d in %s", rid, env.Label()), http.StatusBadRequest)
			return
		}
		if err := h.db.SetVenueBackendLink(r.Context(), id, string(env), rid, h.currentUsername(r)); err != nil {
			http.Error(w, "Could not save", http.StatusInternalServerError)
			return
		}
		h.audit(r, "restaurant.backend_link", id.String(), fmt.Sprintf("%s · %s #%d", name, env.Label(), rid))
		back = fmt.Sprintf("/restaurants/%s?open=backend#vn-backend", id)
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
