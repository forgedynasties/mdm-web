package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Lost devices row (L1, picked 22 Oct 2026; user: "add option to pause device lost per
// venue too"). One row on the Venue settings card for what happens when a tablet leaves
// the venue: Watching (the left_venue alert), Watching and lock it (also self-lock after N
// minutes; moved here from the Device lock row) or Paused for 24 h / 3 days / 7 days /
// until resumed, which stops both (db/venue_lost_pause.go). Anyone who may change the
// venue can pause and resume; turning self-lock on or off stays admin only.

// venueLostView is the Lost devices row.
type venueLostView struct {
	SelfLockMinutes int  // 0 = alerts only
	Paused          bool // the pause holds now
	PausedSub       string
	By              string
	Summary         string
	SummarySub      string
}

func (h *Handler) venueLostView(ctx context.Context, id uuid.UUID, tz string, now time.Time) venueLostView {
	w, err := h.db.VenueLostWatch(ctx, id)
	if err != nil {
		log.Printf("[venue-lost] %s: %v", id, err)
	}
	return lostViewOf(w, tz, now)
}

func lostViewOf(w db.VenueLostWatch, tz string, now time.Time) venueLostView {
	v := venueLostView{SelfLockMinutes: w.SelfLockMinutes, Paused: w.PausedAt(now)}
	if w.By != "" && w.At != nil {
		v.By = "by " + shortUser(w.By) + " · " + w.At.Format("2 Jan")
	}
	if v.Paused {
		if w.PausedUntil == nil {
			v.PausedSub = "until someone resumes"
		} else {
			loc := now.Location()
			if l, err := time.LoadLocation(tz); tz != "" && err == nil {
				loc = l
			}
			v.PausedSub = "resumes " + w.PausedUntil.In(loc).Format("Mon 2 Jan, 15:04")
		}
	}
	switch {
	case v.Paused:
		v.Summary, v.SummarySub = "Paused", v.PausedSub
	case w.SelfLockMinutes > 0:
		v.Summary, v.SummarySub = "Watching", fmt.Sprintf("alerts, and locks itself after %d min", w.SelfLockMinutes)
	default:
		v.Summary, v.SummarySub = "Watching", "alerts when a tablet leaves"
	}
	return v
}

// venueLostPauseTodo is the venue banner's line while lost-device watching is paused.
func venueLostPauseTodo(v venueLostView) (ovTodo, bool) {
	if !v.Paused {
		return ovTodo{}, false
	}
	sub := v.PausedSub
	if v.By != "" {
		sub = v.By + " · " + sub
	}
	return ovTodo{Title: "Lost-device watch paused", Sub: sub + " · no left-venue alerts or self-lock",
		Action: "Resume", Href: "?open=lost#vn-lost"}, true
}

// RestaurantLostSave is POST /restaurants/{id}/lost: mode = watch | lock (with
// self_lock_minutes) | paused (with pause = 24h | 3d | 7d | until), or action = resume.
func (h *Handler) RestaurantLostSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	cur, err := h.db.VenueLostWatch(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	p := cur
	p.By = h.currentUsername(r)
	action, what := "restaurant.lost_pause", ""
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
	case mode == "watch" || mode == "lock":
		p.Paused, p.PausedUntil = false, nil
		p.SelfLockMinutes, what = 0, "watching"
		if mode == "lock" {
			p.SelfLockMinutes = cur.SelfLockMinutes
			if v := r.FormValue("self_lock_minutes"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 5 || n > 120 {
					http.Error(w, "Self-lock time must be between 5 and 120 minutes.", http.StatusBadRequest)
					return
				}
				p.SelfLockMinutes = n
			}
			if p.SelfLockMinutes == 0 {
				p.SelfLockMinutes = selfLockDefault
			}
			what = "watching, self-lock " + strconv.Itoa(p.SelfLockMinutes) + " min"
		}
		if p.SelfLockMinutes != cur.SelfLockMinutes {
			if h.role(r) != "admin" {
				http.Error(w, "Only an admin can turn self-lock on or off.", http.StatusForbidden)
				return
			}
			action = "restaurant.self_lock"
		} else if !cur.PausedAt(time.Now()) {
			http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=lost#vn-lost", id), http.StatusSeeOther)
			return
		}
	default:
		http.Error(w, "Choose what happens when a tablet leaves", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVenueLostWatch(r.Context(), id, p); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, action, id.String(), what)
	if devs, err := h.db.ListRestaurantDevices(r.Context(), id); err == nil {
		ids := make([]uuid.UUID, 0, len(devs))
		for _, d := range devs {
			ids = append(ids, d.ID)
		}
		h.nudgeCheckin(ids)
	}
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=lost#vn-lost", id), http.StatusSeeOther)
}
