package dashboard

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

// Enrollment › From the AIO backend, B1 (picked 3 Nov 2026; user: "the ui is really messy and
// cluttered"). Replaces the tab's two stacked parts (the "not here yet" card and the Prod coverage
// view with its pills, banner, cards, ring and 162-row table) with ONE table of backend
// restaurants, in Fleet's language: views To act on (lists your devices, no venue here) · All ·
// Linked · Ignored, Prod / Stage / real-restaurant / has-devices-here chips, search, sort. A row:
// name with city and backend #, the devices the backend lists there, how many are in the MDM,
// the venue it is linked to, one button (Create venue, or Open) and ⋯ (Link to…, Ignore or List
// again). /coverage for other fleet-wide team members keeps the old view (coverage.html).

// bkB1Row is one backend restaurant.
type bkB1Row struct {
	Env, EnvLabel string
	ID            int64
	Name, City    string
	Test          bool
	C             covCounts // its manageable devices: placed, elsewhere, missing, by Android ID
	VenueID       *uuid.UUID
	VName         string
	Here          []placementDev // MDM devices it lists that have no venue link to follow
	Ignored       bool
	Flags, Text   string
}

// HereText says what of yours is there: "3 devices here, no venue".
func (r bkB1Row) HereText() string { return placementSummary(r.Here) }

// HereSerials are those devices' serials, for the tooltip.
func (r bkB1Row) HereSerials() string {
	var s []string
	for _, d := range r.Here {
		s = append(s, d.Serial)
	}
	return strings.Join(s, ", ")
}

// PlacedPct and ElsePct are the In the MDM bar's segments (of the devices the backend lists).
func (r bkB1Row) PlacedPct() int { return pctInt(r.C.Placed, r.C.Total()) }
func (r bkB1Row) ElsePct() int   { return pctInt(r.C.Elsewhere, r.C.Total()) }

func pctInt(n, of int) int {
	if of <= 0 {
		return 0
	}
	return n * 100 / of
}

// bkB1 is the tab's data.
type bkB1 struct {
	Rows                                []bkB1Row
	Act, All, Linked, Ignored           int
	Prod, Stage, Real, HereN            int
	ProdIn, ProdTotal, ProdLinked, Rest int
	Synced                              *time.Time
	Unlinked                            []db.Restaurant // venues not linked yet, for Link to…
}

// buildBackendB1 joins both backends' coverage rows, the placements (devices with no venue)
// and the ignores into one list.
func buildBackendB1(in covInputs, p placements, ignored map[string]bool, venues []db.Restaurant) bkB1 {
	var out bkB1
	rows := map[string]*bkB1Row{}
	var order []string
	for _, env := range []aiobackend.Env{aiobackend.Prod, aiobackend.Stage} {
		in.Env, in.All = string(env), true
		cov := buildProdCoverage(in)
		if env == aiobackend.Prod {
			out.ProdIn, out.ProdTotal = cov.C.InMDM(), cov.C.Total()
		}
		for _, r := range cov.Rows {
			k := bkKey(r.R.Env, r.R.ID)
			rows[k] = &bkB1Row{Env: r.R.Env, EnvLabel: env.Label(), ID: r.R.ID, Name: r.R.Name, City: r.R.City, Test: r.Test,
				C: r.C, VenueID: r.Venue, VName: r.VName}
			order = append(order, k)
		}
	}
	for _, pr := range p.Rows {
		k := bkKey(pr.R.Env, pr.R.ID)
		row := rows[k]
		if row == nil {
			row = &bkB1Row{Env: pr.R.Env, EnvLabel: pr.EnvLabel(), ID: pr.R.ID, Name: pr.R.Name, City: pr.R.City, Test: pr.Test}
			rows[k] = row
			order = append(order, k)
		}
		row.Here = pr.Devs
	}
	linked := map[uuid.UUID]bool{}
	for _, k := range order {
		r := rows[k]
		r.Ignored = ignored[k]
		var f []string
		f = append(f, "env-"+r.Env)
		switch r.Env {
		case string(aiobackend.Prod):
			out.Prod++
		default:
			out.Stage++
		}
		if r.Ignored {
			f = append(f, "ignored")
			out.Ignored++
		} else {
			f = append(f, "all")
			out.All++
			if r.VenueID == nil && len(r.Here) > 0 {
				f = append(f, "act")
				out.Act++
			}
			if r.VenueID != nil {
				f = append(f, "linked")
				out.Linked++
				linked[*r.VenueID] = true
			}
		}
		if !r.Test {
			f = append(f, "real")
			out.Real++
		}
		if len(r.Here) > 0 {
			f = append(f, "here")
			out.HereN++
		}
		if r.Env == string(aiobackend.Prod) && r.VenueID != nil {
			out.ProdLinked++
		}
		var serials []string
		for _, d := range r.Here {
			serials = append(serials, d.Serial)
		}
		r.Flags = strings.Join(f, " ")
		r.Text = strings.Join([]string{r.Name, r.City, r.EnvLabel, "#" + strconv.FormatInt(r.ID, 10), r.VName, strings.Join(serials, " ")}, " ")
		out.Rows = append(out.Rows, *r)
	}
	for _, r := range out.Rows {
		if r.Env == string(aiobackend.Prod) {
			out.Rest++
		}
	}
	// Work first: devices of yours with no venue, then the most devices in the backend.
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if (len(a.Here) > 0) != (len(b.Here) > 0) {
			return len(a.Here) > 0
		}
		if a.Test != b.Test {
			return !a.Test
		}
		if a.C.Total() != b.C.Total() {
			return a.C.Total() > b.C.Total()
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, v := range venues {
		if !linked[v.ID] {
			out.Unlinked = append(out.Unlinked, v)
		}
	}
	sort.SliceStable(out.Unlinked, func(i, j int) bool { return strings.ToLower(out.Unlinked[i].Name) < strings.ToLower(out.Unlinked[j].Name) })
	return out
}

// backendB1For loads what the tab needs (super admin only; nil when the backend was never copied).
func (h *Handler) backendB1For(r *http.Request) *bkB1 {
	ctx := r.Context()
	in := covInputs{Base: "/enrollment?tab=backend", Now: time.Now()}
	var err error
	if in.Rs, err = h.db.ListBackendRestaurants(ctx); err != nil || len(in.Rs) == 0 {
		return nil
	}
	if in.Bds, err = h.db.ListBackendDevices(ctx); err != nil {
		return nil
	}
	if in.Devs, err = h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc"); err != nil {
		return nil
	}
	if in.Links, err = h.db.VenueBackendLinks(ctx); err != nil {
		return nil
	}
	venues, _ := h.db.ListRestaurants(ctx)
	in.Names = map[uuid.UUID]string{}
	for _, v := range venues {
		in.Names[v.ID] = v.Name
	}
	p, _, err := h.loadPlacements(ctx)
	if err != nil {
		return nil
	}
	ignored, _ := h.db.BackendIgnores(ctx)
	v := buildBackendB1(in, p, ignored, venues)
	if st, err := h.db.BackendSyncStates(ctx); err == nil {
		v.Synced = st[string(aiobackend.Prod)].SyncedAt
	}
	return &v
}
