package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Alerts row on Venue settings (19 Nov 2026; user: pause alerts per venue, for the AIO Lab
// venue with its hundreds of dark devices). Watching / Paused for 24 h, 3 days, 7 days or
// until resumed. While paused no alert is raised or sent for the venue's devices and the
// ones already open are held back from lists and counts (db/venue_alerts_pause.go); they
// return when it ends. Same people as the Updates and Lost devices rows.

// venueAlertsView is the Alerts row.
type venueAlertsView struct {
	Paused    bool
	PausedSub string
	By        string
	Held      int // open alerts the pause is holding back
	Summary   string
}

func (h *Handler) venueAlertsView(ctx context.Context, id uuid.UUID, tz string, now time.Time) venueAlertsView {
	p, err := h.db.VenueAlertsPause(ctx, id)
	if err != nil {
		log.Printf("[venue-alerts] %s: %v", id, err)
	}
	v := alertsViewOf(p, tz, now)
	if v.Paused {
		if n, err := h.db.VenueHeldAlertCount(ctx, id); err == nil {
			v.Held = n
		}
	}
	return v
}

func alertsViewOf(p db.VenueAlertsPause, tz string, now time.Time) venueAlertsView {
	v := venueAlertsView{Paused: p.PausedAt(now), Summary: "Watching"}
	if p.By != "" && p.At != nil {
		v.By = "by " + shortUser(p.By) + " · " + p.At.Format("2 Jan")
	}
	if v.Paused {
		v.Summary = "Paused"
		if p.PausedUntil == nil {
			v.PausedSub = "until someone resumes"
		} else {
			loc := now.Location()
			if l, err := time.LoadLocation(tz); tz != "" && err == nil {
				loc = l
			}
			v.PausedSub = "until " + p.PausedUntil.In(loc).Format("Mon 2 Jan, 15:04")
		}
	}
	return v
}

// venueAlertsPauseTodo is the venue banner's line while alerts are paused.
func venueAlertsPauseTodo(v venueAlertsView) (ovTodo, bool) {
	if !v.Paused {
		return ovTodo{}, false
	}
	sub := "Alerts paused " + v.PausedSub
	if v.Held > 0 {
		sub += fmt.Sprintf(" · %d open alert%s held back", v.Held, sIfN(v.Held))
	}
	return ovTodo{Title: "Alerts paused here", Sub: sub, Action: "Resume", Href: "?open=alerts#vn-alerts"}, true
}

// RestaurantAlertsSave is POST /restaurants/{id}/alerts: mode = watch | paused (with
// pause = 24h | 3d | 7d | until), or action = resume.
func (h *Handler) RestaurantAlertsSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	cur, err := h.db.VenueAlertsPause(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	p := cur
	p.By = h.currentUsername(r)
	action, what := "venue.alerts_pause", ""
	switch mode := strings.TrimSpace(r.FormValue("mode")); {
	case r.FormValue("action") == "resume" || mode == "watch":
		if !cur.Paused {
			http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=alerts#vn-alerts", id), http.StatusSeeOther)
			return
		}
		p.Paused, p.PausedUntil, what = false, nil, "resumed"
		action = "venue.alerts_resume"
	case mode == "paused":
		p.Paused, p.PausedUntil = true, nil
		what = "paused until resumed"
		if d, ok := venuePauseLengths[r.FormValue("pause")]; ok {
			t := time.Now().Add(d)
			p.PausedUntil, what = &t, "paused for "+r.FormValue("pause")
		}
	default:
		http.Error(w, "Choose watching or paused", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVenueAlertsPause(r.Context(), id, p); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, action, id.String(), what)
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=alerts#vn-alerts", id), http.StatusSeeOther)
}
