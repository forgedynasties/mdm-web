package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

// When devices move, per venue (V2, picked 22 Oct 2026; user: "add when devices move
// setting override per venue or pause option"). The Venue settings card has an Updates
// row: Fleet window (Lineup's, the default), While the venue is closed (outside its
// service hours, so it follows the hours when they change), Own window, or Paused for
// 24 h / 3 days / 7 days / until resumed. It gates both things that move by themselves:
// prod firmware (RunRoleFirmware) and MDM client auto-update (RunClientAutoUpdate). A
// fleet-wide pause still stops everything, and a deployment someone sends by hand is not
// held: that is "now, and I am watching".

// venueGate answers "may this device move now" for the update jobs, loaded once a tick.
type venueGate struct {
	pols    map[uuid.UUID]db.VenueUpdatePolicy
	windows map[uuid.UUID]db.ServiceWindow // venues with their own service hours
	fleetSW db.ServiceWindow
}

func (h *Handler) loadVenueGate(ctx context.Context) venueGate {
	g := venueGate{}
	var err error
	if g.pols, err = h.db.VenueUpdatePolicies(ctx); err != nil {
		log.Printf("[venue-updates] policies: %v", err)
	}
	if len(g.pols) == 0 {
		return g
	}
	if g.windows, err = h.db.ListRestaurantServiceWindows(ctx); err != nil {
		log.Printf("[venue-updates] service windows: %v", err)
	}
	g.fleetSW, _ = h.db.GetFleetServiceWindow(ctx)
	return g
}

// allows is fleet.Allows with the device's venue setting applied. The caller has already
// checked that the fleet policy is active.
func (g venueGate) allows(d *db.Device, fleet config.ClientAutoUpdate, now time.Time) bool {
	local := deviceNow(d)
	if d.RestaurantID == nil {
		return fleet.Allows(local)
	}
	p, ok := g.pols[*d.RestaurantID]
	if !ok {
		return fleet.Allows(local)
	}
	if p.PausedAt(now) {
		return false
	}
	switch p.Mode {
	case db.VenueUpdateOwn:
		return config.ClientAutoUpdate{WindowStart: p.Start, WindowEnd: p.End}.Allows(local)
	case db.VenueUpdateClosed:
		w, own := g.windows[*d.RestaurantID]
		if !own {
			w = g.fleetSW
		}
		if w.OpenMin == w.CloseMin { // no service hours to be closed outside of
			return fleet.Allows(local)
		}
		return !db.ServiceOpenAt(w, extraString(d.LatestExtra, "timezone"), now)
	}
	return fleet.Allows(local)
}

// venueUpdatesView is the Updates row on the Venue settings card.
type venueUpdatesView struct {
	Mode         string // "" | closed | own
	Start, End   string
	Paused       bool   // the pause holds now
	PausedSub    string // "resumes Fri 24 Oct, 09:00" | "until someone resumes"
	By           string // "by ali · 22 Oct"
	FleetWindow  string // "02:00 – 05:00" | "any time"
	FleetPaused  bool
	ClosedWindow string // "23:00 – 11:00", "" when the venue has no closed hours
	Summary      string
	SummarySub   string
	TZ           string
}

func hhmmRange(a, b string) string {
	if a == "" || b == "" {
		return "any time"
	}
	return a + " – " + b
}

func (h *Handler) venueUpdatesView(ctx context.Context, id uuid.UUID, win db.ServiceWindow, tz string, now time.Time) venueUpdatesView {
	p, err := h.db.VenueUpdatePolicy(ctx, id)
	if err != nil {
		log.Printf("[venue-updates] %s: %v", id, err)
	}
	fleet := h.cfg.RoleRollout(config.RolloutFirmware)
	v := venueUpdatesView{Mode: p.Mode, Start: p.Start, End: p.End, Paused: p.PausedAt(now),
		FleetWindow: hhmmRange(fleet.WindowStart, fleet.WindowEnd), FleetPaused: fleet.Paused, TZ: tz}
	if win.OpenMin != win.CloseMin {
		v.ClosedWindow = hhmm(win.CloseMin) + " – " + hhmm(win.OpenMin)
	}
	if p.By != "" && p.At != nil {
		v.By = "by " + shortUser(p.By) + " · " + p.At.Format("2 Jan")
	}
	if v.Paused {
		if p.PausedUntil == nil {
			v.PausedSub = "until someone resumes"
		} else {
			loc := now.Location()
			if l, err := time.LoadLocation(tz); tz != "" && err == nil {
				loc = l
			}
			v.PausedSub = "resumes " + p.PausedUntil.In(loc).Format("Mon 2 Jan, 15:04")
		}
	}
	switch {
	case v.Paused:
		v.Summary, v.SummarySub = "Paused", v.PausedSub
	case p.Mode == db.VenueUpdateClosed && v.ClosedWindow != "":
		v.Summary, v.SummarySub = "While closed", v.ClosedWindow
	case p.Mode == db.VenueUpdateOwn:
		v.Summary, v.SummarySub = "Own window", hhmmRange(p.Start, p.End)
	default:
		v.Summary, v.SummarySub = "Fleet window", v.FleetWindow+" local"
	}
	if fleet.Paused {
		v.SummarySub += " · paused fleet-wide"
	}
	return v
}

// shortUser is the part of a username before "@".
func shortUser(u string) string {
	if i := strings.IndexByte(u, '@'); i > 0 {
		return u[:i]
	}
	return u
}

// venuePauseTodo is the venue banner's line while its updates are paused.
func venuePauseTodo(v venueUpdatesView) (ovTodo, bool) {
	if !v.Paused {
		return ovTodo{}, false
	}
	sub := v.PausedSub
	if v.By != "" {
		sub = v.By + " · " + sub
	}
	return ovTodo{Title: "Updates paused here", Sub: sub + " · firmware and MDM client stay put",
		Action: "Resume", Href: "?open=updates#vn-updates"}, true
}

// venuePauseChoices are the pause lengths on the Updates row, in order.
var venuePauseChoices = []struct{ Value, Label string }{
	{"24h", "24 hours"}, {"3d", "3 days"}, {"7d", "7 days"}, {"until", "Until I resume"},
}

var venuePauseLengths = map[string]time.Duration{"24h": 24 * time.Hour, "3d": 72 * time.Hour, "7d": 7 * 24 * time.Hour}

// RestaurantUpdatesSave is POST /restaurants/{id}/updates: mode = fleet | closed | own |
// paused (with pause = 24h | 3d | 7d | until), or action = resume.
func (h *Handler) RestaurantUpdatesSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") { // "Manage groups & venues", like Service hours
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	cur, err := h.db.VenueUpdatePolicy(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	p := cur
	p.By = h.currentUsername(r)
	what := ""
	switch mode := strings.TrimSpace(r.FormValue("mode")); {
	case r.FormValue("action") == "resume":
		p.Paused, p.PausedUntil, what = false, nil, "resumed"
	case mode == "paused":
		p.Paused, p.PausedUntil = true, nil
		what = "paused until resumed"
		if d, ok := venuePauseLengths[r.FormValue("pause")]; ok {
			t := time.Now().Add(d)
			p.PausedUntil, what = &t, "paused for "+r.FormValue("pause")
		}
	case mode == "fleet" || db.IsVenueUpdateMode(mode):
		if mode == "fleet" {
			mode = db.VenueUpdateFleet
		}
		p.Mode, p.Paused, p.PausedUntil = mode, false, nil
		p.Start, p.End = "", ""
		if mode == db.VenueUpdateOwn {
			p.Start, p.End = strings.TrimSpace(r.FormValue("window_start")), strings.TrimSpace(r.FormValue("window_end"))
			if _, e1 := time.Parse("15:04", p.Start); e1 != nil || p.Start == p.End {
				http.Error(w, "Choose a start and an end time", http.StatusBadRequest)
				return
			}
			if _, e2 := time.Parse("15:04", p.End); e2 != nil {
				http.Error(w, "Choose a start and an end time", http.StatusBadRequest)
				return
			}
		}
		what = map[string]string{"": "fleet window", "closed": "while closed", "own": "own " + p.Start + "–" + p.End}[mode]
	default:
		http.Error(w, "Choose when devices move", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVenueUpdatePolicy(r.Context(), id, p); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.updates", id.String(), what)
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=updates#vn-updates", id), http.StatusSeeOther)
}
