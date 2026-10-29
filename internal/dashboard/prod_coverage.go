package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Prod coverage (C1, picked 23 Oct 2026; user: "a page where we can see progress of prod,
// how much have been ported to mdm"). /coverage measures the AIO backend copy against the
// MDM: every device a backend restaurant lists that the MDM could manage (POS, KDS,
// kiosks, menu boards, tablets; not printers, drawers or payment terminals) is
//   placed    — the MDM has it (serial, or the Nugget Android ID it reports) at the venue
//               linked to its restaurant;
//   elsewhere — the MDM has it anywhere else (no venue, or the restaurant has none);
//   missing   — not in the MDM;
//   unmatched — listed by an Android ID (POS app installs, tablets) the MDM can't match
//               until the apps send serials.
// In the Overview's language: pills, a to-do banner, cards, a restaurant table, a rail.

const (
	covPlaced = iota
	covElsewhere
	covMissing
	covUnmatched
)

// covCounts is the four parts of a set of backend devices.
type covCounts struct {
	Placed, Elsewhere, Missing, Unmatched int
}

func (c *covCounts) add(k int) {
	switch k {
	case covPlaced:
		c.Placed++
	case covElsewhere:
		c.Elsewhere++
	case covMissing:
		c.Missing++
	default:
		c.Unmatched++
	}
}

// Total is every counted device; InMDM the placed and elsewhere ones.
func (c covCounts) Total() int { return c.Placed + c.Elsewhere + c.Missing + c.Unmatched }
func (c covCounts) InMDM() int { return c.Placed + c.Elsewhere }

// Pct is the part of the total in the MDM, one decimal ("0.7").
func (c covCounts) Pct() string {
	if c.Total() == 0 {
		return "0"
	}
	p := float64(c.InMDM()) * 100 / float64(c.Total())
	if p > 0 && p < 10 {
		return strconv.FormatFloat(p, 'f', 1, 64)
	}
	return strconv.Itoa(int(p + 0.5))
}

// Bar is the four parts as % widths for the segment bar.
func (c covCounts) Bar() []covSeg {
	t := c.Total()
	if t == 0 {
		return nil
	}
	seg := func(n int, cls, lbl string) covSeg {
		return covSeg{Class: cls, Pct: float64(n) * 100 / float64(t), N: n, Label: lbl}
	}
	return []covSeg{seg(c.Placed, "g", "placed"), seg(c.Elsewhere, "a", "elsewhere"), seg(c.Missing, "n", "not in the MDM"), seg(c.Unmatched, "h", "by Android ID")}
}

type covSeg struct {
	Class string
	Pct   float64
	N     int
	Label string
}

// covRow is one backend restaurant.
type covRow struct {
	R       db.BackendRestaurant
	Venue   *uuid.UUID
	VName   string
	C       covCounts
	Test    bool
	Classes []string
}

// Work is what is left to do there (not placed and matchable).
func (r covRow) Work() int { return r.C.Missing + r.C.Elsewhere }

// TypesLabel is "KDS, kiosks, POS".
func (r covRow) TypesLabel() string { return strings.Join(r.Classes, ", ") }

// covWeek is one bar of the rail's weekly chart.
type covWeek struct {
	Label, Title string
	Pct          int
	Cur          bool
}

// covClasses are the classes a backend device can map to, in display order.
var covClasses = []string{prod.ClassPOS, prod.ClassKDS, prod.ClassDongle, prod.ClassKiosk, prod.ClassT7, prod.ClassMPOS}

type covType struct {
	Class, Label string
	C            covCounts
}

type prodCoverageView struct {
	Env      string
	EnvLabel string
	Type     string
	All      bool // inactive restaurants counted too
	Synced   *time.Time
	C        covCounts
	Rows     []covRow
	Shown    []covRow // the table's rows (first 30 unless ?rows=all)
	MoreRows int
	Types    []covType
	Linked   int // restaurants with a venue
	WithAny  int // restaurants with any device in the MDM
	Banner   vpBanner
	Weeks    []covWeek
	Offset   float64 // ring
	Pills    covPills
	AllRows  string // the table's "Show all" link
}

type covPills struct {
	Envs, Types, Rests []ovOpt
	TypeLabel          string
}

type covInputs struct {
	Base    string // where the filter links point ("" = /coverage)
	Env     string
	Type    string
	All     bool
	RowsAll bool
	Rs      []db.BackendRestaurant
	Bds     []db.BackendDevice
	Devs    []db.Device
	Names   map[uuid.UUID]string
	Links   map[uuid.UUID]db.VenueBackendLink
	Synced  *time.Time
	Now     time.Time
}

// covHref is a filter link on base ("/coverage", or "/enrollment?tab=backend" inside
// the Enrollment hub).
func covHref(base, env, typ string, all bool) string {
	q := url.Values{}
	if env != string(aiobackend.Prod) {
		q.Set("env", env)
	}
	if typ != "" {
		q.Set("type", typ)
	}
	if !all {
		q.Set("rest", "active")
	}
	e := q.Encode()
	switch {
	case e == "":
		return base
	case strings.Contains(base, "?"):
		return base + "&" + e
	}
	return base + "?" + e
}

func buildProdCoverage(in covInputs) prodCoverageView {
	if in.Base == "" {
		in.Base = "/coverage"
	}
	v := prodCoverageView{Env: in.Env, EnvLabel: aiobackend.Env(in.Env).Label(), Type: in.Type, All: in.All, Synced: in.Synced}
	// The MDM's devices by every id the backend may list them under.
	idx := map[string]*db.Device{}
	for i := range in.Devs {
		d := &in.Devs[i]
		if d.Hidden {
			continue
		}
		for _, id := range deviceBackendIDs(d) {
			idx[id] = d
		}
	}
	venueOf := map[int64]uuid.UUID{}
	for vid, l := range in.Links {
		if l.Env == in.Env {
			venueOf[l.RestaurantID] = vid
		}
	}
	rest := map[int64]db.BackendRestaurant{}
	for _, r := range in.Rs {
		if r.Env == in.Env {
			rest[r.ID] = r
		}
	}
	rows := map[int64]*covRow{}
	types := map[string]*covType{}
	classSeen := map[int64]map[string]bool{}
	matched := map[uuid.UUID]time.Time{}
	for _, b := range in.Bds {
		if b.Env != in.Env || b.Revoked || !backendManageable(b.Type) {
			continue
		}
		r, ok := rest[b.RestaurantID]
		if !ok || (!in.All && strings.EqualFold(r.RecordStatus, "Inactive")) {
			continue
		}
		class := backendTypeClass(b.Type)
		if in.Type != "" && class != in.Type {
			continue
		}
		k := covMissing
		if d := idx[strings.ToUpper(strings.TrimSpace(b.Serial))]; d != nil {
			k = covElsewhere
			if vid, ok := venueOf[b.RestaurantID]; ok && d.RestaurantID != nil && *d.RestaurantID == vid {
				k = covPlaced
			}
			matched[d.ID] = d.CreatedAt
		} else if androidIDRe.MatchString(strings.TrimSpace(b.Serial)) {
			k = covUnmatched
		}
		v.C.add(k)
		row := rows[b.RestaurantID]
		if row == nil {
			row = &covRow{R: r, Test: testRestaurantRe.MatchString(r.Name)}
			if vid, ok := venueOf[r.ID]; ok {
				row.Venue, row.VName = &vid, in.Names[vid]
			}
			rows[b.RestaurantID] = row
			classSeen[b.RestaurantID] = map[string]bool{}
		}
		row.C.add(k)
		classSeen[b.RestaurantID][class] = true
		t := types[class]
		if t == nil {
			t = &covType{Class: class, Label: prod.ClassLabel(class)}
			types[class] = t
		}
		t.C.add(k)
	}
	for id, row := range rows {
		for _, c := range covClasses {
			if classSeen[id][c] {
				row.Classes = append(row.Classes, prod.ClassLabel(c))
			}
		}
		if row.Venue != nil {
			v.Linked++
		}
		if row.C.InMDM() > 0 {
			v.WithAny++
		}
		v.Rows = append(v.Rows, *row)
	}
	sort.Slice(v.Rows, func(i, j int) bool {
		a, b := v.Rows[i], v.Rows[j]
		if a.Test != b.Test {
			return !a.Test
		}
		if a.C.Total() != b.C.Total() {
			return a.C.Total() > b.C.Total()
		}
		return a.R.Name < b.R.Name
	})
	v.Shown = v.Rows
	if !in.RowsAll && len(v.Shown) > 30 {
		v.MoreRows = len(v.Shown) - 30
		v.Shown = v.Shown[:30]
	}
	for _, c := range covClasses {
		if t := types[c]; t != nil {
			v.Types = append(v.Types, *t)
		}
	}
	sort.SliceStable(v.Types, func(i, j int) bool { return v.Types[i].C.Total() > v.Types[j].C.Total() })
	v.Offset = 326.7
	if t := v.C.Total(); t > 0 {
		v.Offset = 326.7 * (1 - float64(v.C.InMDM())/float64(t))
	}
	v.Banner = prodCoverageBanner(v)
	v.Weeks = prodCoverageWeeks(matched, in.Now)
	// Pills.
	for _, e := range aiobackend.Envs {
		v.Pills.Envs = append(v.Pills.Envs, ovOpt{e.Label(), covHref(in.Base, string(e), in.Type, in.All), string(e) == in.Env})
	}
	v.Pills.Types = append(v.Pills.Types, ovOpt{"All types", covHref(in.Base, in.Env, "", in.All), in.Type == ""})
	for _, c := range covClasses {
		v.Pills.Types = append(v.Pills.Types, ovOpt{prod.ClassLabel(c), covHref(in.Base, in.Env, c, in.All), in.Type == c})
		if in.Type == c {
			v.Pills.TypeLabel = prod.ClassLabel(c)
		}
	}
	v.AllRows = covHref(in.Base, in.Env, in.Type, in.All)
	if strings.Contains(v.AllRows, "?") {
		v.AllRows += "&rows=all#pc-rest"
	} else {
		v.AllRows += "?rows=all#pc-rest"
	}
	// All by default (user, 29 Oct 2026: "show all even not active ones"); ?rest=active narrows.
	v.Pills.Rests = []ovOpt{{"All, inactive too", covHref(in.Base, in.Env, in.Type, true), in.All}, {"Active only", covHref(in.Base, in.Env, in.Type, false), !in.All}}
	return v
}

// prodCoverageBanner: the three biggest restaurants (tests last) with work left.
func prodCoverageBanner(v prodCoverageView) vpBanner {
	b := vpBanner{Tone: "calm", Kicker: fmt.Sprintf("AIO %s · %s%% in the MDM", strings.ToLower(v.EnvLabel), v.C.Pct())}
	var todos []ovTodo
	for _, r := range v.Rows {
		if r.Work() == 0 || r.Test {
			continue
		}
		t := ovTodo{}
		if r.Venue != nil {
			t.Title = fmt.Sprintf("%s: linked, %d of %d in the MDM", r.R.Name, r.C.InMDM(), r.C.Total())
			t.Sub = r.TypesLabel() + " · " + countNoun(r.C.Missing, "device") + " still to enrol"
			t.Action, t.Href = "Open venue", "/restaurants/"+r.Venue.String()
		} else {
			if r.C.InMDM() == 0 {
				t.Title = fmt.Sprintf("%s: %d devices, none in the MDM", r.R.Name, r.C.Total())
			} else {
				t.Title = fmt.Sprintf("%s: %d devices, %d in the MDM", r.R.Name, r.C.Total(), r.C.InMDM())
			}
			t.Sub = r.TypesLabel() + " · no venue yet"
			t.Action, t.Post = "Create venue", "/restaurants/backend-place"
			t.Fields = [][2]string{{"action", "create"}, {"env", r.R.Env}, {"rid", strconv.FormatInt(r.R.ID, 10)}}
		}
		todos = append(todos, t)
	}
	if len(todos) == 0 {
		b.Headline = "Everything the backend lists is in the MDM"
		b.Calm = "Every matchable device is placed at its venue."
		return b
	}
	b.Tone = "warn"
	b.Headline = fmt.Sprintf("%d restaurant%s to start with", min(3, len(todos)), plural(min(3, len(todos))))
	if len(todos) > 3 {
		b.More = len(todos) - 3
		todos = todos[:3]
	}
	b.Todos = todos
	return b
}

// prodCoverageWeeks is how many of today's matched devices the MDM had at the end of each of
// the last eight weeks (from their enrolment dates).
func prodCoverageWeeks(matched map[uuid.UUID]time.Time, now time.Time) []covWeek {
	var out []covWeek
	ends := make([]time.Time, 8)
	counts := make([]int, 8)
	for i := 0; i < 8; i++ {
		ends[i] = now.AddDate(0, 0, -7*(7-i))
	}
	max := 1
	for i, e := range ends {
		for _, c := range matched {
			if !c.After(e) {
				counts[i]++
			}
		}
		if counts[i] > max {
			max = counts[i]
		}
	}
	for i, e := range ends {
		lbl := "" // every other week labelled, so the labels fit under the bars
		if i%2 == 1 {
			lbl = e.Format("2/1")
		}
		out = append(out, covWeek{Label: lbl, Pct: counts[i] * 100 / max, Cur: i == 7,
			Title: fmt.Sprintf("%d in the MDM by %s", counts[i], e.Format("2 Jan"))})
	}
	return out
}

// prodCoverageFor builds the coverage view from the request's pills (env, type, rest,
// rows), its filter links pointing at base. ok is false when the backend copy can't be
// read.
func (h *Handler) prodCoverageFor(r *http.Request, base string) (prodCoverageView, bool) {
	ctx := r.Context()
	q := r.URL.Query()
	env := q.Get("env")
	if !aiobackend.Env(env).Valid() {
		env = string(aiobackend.Prod)
	}
	typ := q.Get("type")
	if !prod.IsClass(typ) {
		typ = ""
	}
	in := covInputs{Base: base, Env: env, Type: typ, All: q.Get("rest") != "active", RowsAll: q.Get("rows") == "all", Now: time.Now()}
	var err error
	if in.Rs, err = h.db.ListBackendRestaurants(ctx); err == nil {
		if in.Bds, err = h.db.ListBackendDevices(ctx); err == nil {
			if in.Devs, err = h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc"); err == nil {
				in.Links, err = h.db.VenueBackendLinks(ctx)
			}
		}
	}
	if err != nil {
		return prodCoverageView{}, false
	}
	in.Names = map[uuid.UUID]string{}
	if venues, err := h.db.ListRestaurants(ctx); err == nil {
		for _, v := range venues {
			in.Names[v.ID] = v.Name
		}
	}
	if st, err := h.db.BackendSyncStates(ctx); err == nil {
		in.Synced = st[env].SyncedAt
	}
	return buildProdCoverage(in), true
}

// ProdCoveragePage is GET /coverage. Since E1 (23 Oct 2026) coverage is a tab of the
// Enrollment hub, and since 26 Oct it is super admin only (user: "from the aio backend
// also" super admin only), so everyone who may open it lands on that tab.
func (h *Handler) ProdCoveragePage(w http.ResponseWriter, r *http.Request) {
	if h.role(r) == "admin" {
		to := "/enrollment?tab=backend"
		if e := r.URL.RawQuery; e != "" {
			to += "&" + e
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	v, ok := h.prodCoverageFor(r, "/coverage")
	if !ok {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "coverage.html", map[string]any{"Title": "Prod coverage", "Cov": v, "Configured": h.backend != nil || v.Synced != nil,
		"Placements": h.venuesPlacementView(r.Context())})
}
