package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/geolocate"
)

// ── Lost-device reporting on the dashboard (18 Oct; A+B+C+D) ──────────────────
//
// The device page's lost band and the Location tab's "Since it went missing" card (H1,
// picked 18 Oct), the venue's self-lock switch (S1) and the job that places the Wi-Fi
// scans a device kept while out of reach. The storage and the alerts are in
// internal/db/lost_reporting.go and alert_feature_rules.go.

const (
	lostTrailPerTick   = 60 // scans tried per run (learned-index hits are free)
	lostTrailGoogleMax = 5  // Google lookups per run: the resolver allows one per 10 s
	selfLockDefault    = 10 // minutes, when the venue switch goes on without a time (S1)
)

// RunLostTrail places the Wi-Fi scans lost or departed devices kept while out of reach
// (db.PendingTrailScans decides which are worth it). The resolver answers from its cache
// and learned index for free and lets one Google call through per 10 s server-wide, so a
// run waits out the cooldown a few times and leaves the rest for the next minute.
func (h *Handler) RunLostTrail(ctx context.Context) {
	if h.geo == nil {
		return
	}
	pend, err := h.db.PendingTrailScans(ctx, lostTrailPerTick)
	if err != nil {
		log.Printf("[lost-trail] pending: %v", err)
		return
	}
	waits := 0
	for _, p := range pend {
		var aps []geolocate.WifiAP
		if json.Unmarshal(p.Scan, &aps) != nil || len(aps) == 0 {
			_ = h.db.ResolveTrailScan(ctx, p.ID, 0, 0, 0, false)
			continue
		}
		lat, lon, acc, err := h.geo.Resolve(ctx, aps)
		for errors.Is(err, geolocate.ErrCooldown) && waits < lostTrailGoogleMax {
			waits++
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			lat, lon, acc, err = h.geo.Resolve(ctx, aps)
		}
		if errors.Is(err, geolocate.ErrCooldown) {
			return // the rest wait for the next run
		}
		if e := h.db.ResolveTrailScan(ctx, p.ID, lat, lon, acc, err == nil); e != nil {
			log.Printf("[lost-trail] resolve %d: %v", p.ID, e)
		}
	}
}

// lostReport is what the device page shows of a lost device's time out of reach: the
// band's "Last heard … by …" line and the Location tab's timeline.
type lostReport struct {
	LastHeard *db.BLESighting // the most recent neighbour that heard it, nil when none
	Signal    string          // LastHeard's strength in words ("very close" …)
	Bars      int             // the same as 1–4 bars, for the band's signal glyph
	Timeline  []db.LostEvent  // oldest first: left the venue, marked lost, heard, Wi-Fi diary, lifted
	Since     time.Time
	Beacon    *lostBeaconState
}

// lostBeaconState is what the lost device itself said about its beacon (extra.lost_beacon).
type lostBeaconState struct {
	OK  bool   `json:"ok"`
	Why string `json:"why"`
}

// lostReportFor builds the device page's lost report, or nil when there is nothing to
// show (never lost and no recent departure).
func (h *Handler) lostReportFor(ctx context.Context, d *db.Device) *lostReport {
	tl, since, err := h.db.LostTimeline(ctx, d.ID)
	if err != nil {
		log.Printf("[lost] timeline %s: %v", d.SerialNumber, err)
		return nil
	}
	if len(tl) == 0 {
		return nil
	}
	r := &lostReport{Timeline: tl, Since: since}
	if heard, err := h.db.LostSightings(ctx, d.ID, since); err == nil {
		for i := range heard {
			if r.LastHeard == nil || heard[i].LastAt.After(r.LastHeard.LastAt) {
				r.LastHeard = &heard[i]
			}
		}
		if r.LastHeard != nil {
			r.Signal = db.SignalWord(r.LastHeard.RSSI)
			r.Bars = map[string]int{"very close": 4, "close": 3, "in range": 2}[r.Signal]
			if r.Bars == 0 {
				r.Bars = 1
			}
		}
	}
	var x struct {
		B *lostBeaconState `json:"lost_beacon"`
	}
	if json.Unmarshal(d.LatestExtra, &x) == nil {
		r.Beacon = x.B
	}
	return r
}

// RestaurantSelfLock sets a venue's self-lock (S1): POST self_lock = on|off and
// self_lock_minutes (5–120, 10 when on without a time). Admin only, audited; the venue's
// devices are asked to check in so they have it at once.
//
//	POST /restaurants/{id}/self-lock
func (h *Handler) RestaurantSelfLock(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	minutes := 0
	if on := r.FormValue("self_lock"); on == "on" || on == "1" || on == "true" {
		minutes = selfLockDefault
		if v := r.FormValue("self_lock_minutes"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 5 || n > 120 {
				http.Error(w, "Self-lock time must be between 5 and 120 minutes.", http.StatusBadRequest)
				return
			}
			minutes = n
		}
	}
	if err := h.db.SetRestaurantSelfLock(r.Context(), id, minutes); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	detail := "off"
	if minutes > 0 {
		detail = strconv.Itoa(minutes) + " min"
	}
	h.audit(r, "restaurant.self_lock", id.String(), detail)
	if devs, err := h.db.ListRestaurantDevices(r.Context(), id); err == nil {
		ids := make([]uuid.UUID, 0, len(devs))
		for _, d := range devs {
			ids = append(ids, d.ID)
		}
		h.nudgeCheckin(ids)
	}
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
}

// Newest is the timeline newest first, as the Location tab lists it.
func (r *lostReport) Newest() []db.LostEvent {
	out := make([]db.LostEvent, len(r.Timeline))
	for i, e := range r.Timeline {
		out[len(r.Timeline)-1-i] = e
	}
	return out
}

// HasPoints reports whether any timeline event has a place to put on the map.
func (r *lostReport) HasPoints() bool {
	for _, e := range r.Timeline {
		if e.Lat != nil && e.Lon != nil {
			return true
		}
	}
	return false
}

// PointsJSON is the timeline's placed events, oldest first, for the trail map.
func (r *lostReport) PointsJSON() template.JS {
	type pt struct {
		Lat   float64 `json:"lat"`
		Lon   float64 `json:"lon"`
		Acc   float64 `json:"acc"`
		Kind  string  `json:"kind"`
		Title string  `json:"title"`
		At    int64   `json:"at"`
	}
	pts := []pt{}
	for _, e := range r.Timeline {
		if e.Lat != nil && e.Lon != nil {
			pts = append(pts, pt{*e.Lat, *e.Lon, e.Accuracy, e.Kind, e.Title, e.At.UnixMilli()})
		}
	}
	b, _ := json.Marshal(pts)
	return template.JS(b)
}
