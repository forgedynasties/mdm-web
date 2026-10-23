package dashboard

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

// Profile and hours from the AIO backend (P2, picked 23 Oct 2026: H1 + B1). A linked
// venue's address, pin, timezone and service hours follow its backend restaurant until
// someone edits them in the MDM ("Set here"); "Use the backend's" puts a field back.
// Linking never overwrites a value someone typed: a field already set and different
// starts as Set here. Name and contact email stay the MDM's own; the backend's phone is
// shown, not stored. Every change a sync makes goes in the audit log as aio-backend.

// Fields that can follow the backend.
const (
	followAddress  = "address"
	followPin      = "pin"
	followTimezone = "timezone"
	followHours    = "hours"
)

var followFields = []string{followAddress, followPin, followTimezone, followHours}

func followLabel(f string) string {
	switch f {
	case followPin:
		return "pin"
	case followHours:
		return "service hours"
	}
	return f
}

// backendWeek turns the backend's per-day rows into a Week: the earliest slot start to the
// latest end per day (a lunch gap counts as open), a day with no row closed, an open day
// with no slots open all day. nil when the backend has no hours or no open day.
func backendWeek(days []aiobackend.DayHours) *db.Week {
	if len(days) == 0 {
		return nil
	}
	idx := map[string]int{"monday": 0, "tuesday": 1, "wednesday": 2, "thursday": 3, "friday": 4, "saturday": 5, "sunday": 6}
	var wk db.Week
	for i := range wk {
		wk[i].Closed = true
	}
	for _, d := range days {
		i, ok := idx[strings.ToLower(d.Day)]
		if !ok || !d.Open {
			continue
		}
		if len(d.Slots) == 0 {
			wk[i] = db.DayHours{Open: 0, Close: 1440}
			continue
		}
		open, close := -1, -1
		for _, s := range d.Slots {
			o, c := slotMin(s[0]), slotMin(s[1])
			if o < 0 || c < 0 {
				continue
			}
			if c <= o {
				c += 1440
			}
			if open < 0 || o < open {
				open = o
			}
			if c > close {
				close = c
			}
		}
		if open >= 0 {
			if close > 1440 {
				close -= 1440
			}
			wk[i] = db.DayHours{Open: open, Close: close}
		}
	}
	if _, _, any := wk.Span(); !any {
		return nil
	}
	return &wk
}

// venueValues are the fields B1 manages, as the MDM has them or as the backend says.
type venueValues struct {
	Address  string
	Lat, Lon *float64
	Timezone string
	Week     *db.Week
	OwnHours bool // the venue has its own service window (MDM side only)
}

func samePin(a, b venueValues) bool {
	return a.Lat != nil && b.Lat != nil && a.Lon != nil && b.Lon != nil &&
		math.Abs(*a.Lat-*b.Lat) < 1e-5 && math.Abs(*a.Lon-*b.Lon) < 1e-5
}

func sameWeek(a, b *db.Week) bool {
	return a != nil && b != nil && *a == *b
}

// mergeBackend applies B1: a followed field takes the backend's value; an empty one is
// filled and starts following; an equal one starts following; a different one is left
// (Set here). It returns the new values, the fields changed and the new follow set.
func mergeBackend(cur, b venueValues, follow map[string]bool) (venueValues, []string, map[string]bool) {
	out := cur
	nf := map[string]bool{}
	for k, v := range follow {
		nf[k] = v
	}
	var changed []string
	decide := func(field string, has, empty, same bool, set func()) {
		if !has {
			return
		}
		switch {
		case nf[field] && !same, !nf[field] && empty:
			set()
			changed = append(changed, field)
			nf[field] = true
		case same:
			nf[field] = true
		}
	}
	decide(followAddress, b.Address != "", strings.TrimSpace(cur.Address) == "", strings.EqualFold(strings.TrimSpace(cur.Address), b.Address),
		func() { out.Address = b.Address })
	decide(followPin, b.Lat != nil && b.Lon != nil, cur.Lat == nil || cur.Lon == nil, samePin(cur, b),
		func() { out.Lat, out.Lon = b.Lat, b.Lon })
	tzOK := false
	if b.Timezone != "" {
		_, err := time.LoadLocation(b.Timezone)
		tzOK = err == nil
	}
	decide(followTimezone, tzOK, cur.Timezone == "", cur.Timezone == b.Timezone, func() { out.Timezone = b.Timezone })
	decide(followHours, b.Week != nil, !cur.OwnHours, cur.OwnHours && sameWeek(cur.Week, b.Week),
		func() { out.Week, out.OwnHours = b.Week, true })
	return out, changed, nf
}

// refreshBackendHours copies a linked restaurant's hours (one small query).
func (h *Handler) refreshBackendHours(ctx context.Context, l db.VenueBackendLink) error {
	if h.backend == nil {
		return nil
	}
	r, ok, err := h.db.BackendRestaurantOf(ctx, l.Env, l.RestaurantID)
	if err != nil || !ok {
		return err
	}
	days, err := h.backend.Hours(ctx, aiobackend.Env(l.Env), r.BusinessID, r.ID)
	if err != nil {
		return err
	}
	return h.db.SetBackendHours(ctx, l.Env, l.RestaurantID, backendWeek(days))
}

// applyBackendToVenue merges the backend's values into a linked venue (B1) and returns
// what changed.
func (h *Handler) applyBackendToVenue(ctx context.Context, venueID uuid.UUID) ([]string, error) {
	l, linked, err := h.db.VenueBackendLinkOf(ctx, venueID)
	if err != nil || !linked {
		return nil, err
	}
	br, ok, err := h.db.BackendRestaurantOf(ctx, l.Env, l.RestaurantID)
	if err != nil || !ok {
		return nil, err
	}
	rest, err := h.db.GetRestaurant(ctx, venueID)
	if err != nil {
		return nil, err
	}
	follow, err := h.db.VenueBackendFollow(ctx, venueID)
	if err != nil {
		return nil, err
	}
	win, own, err := h.db.GetRestaurantServiceWindow(ctx, venueID)
	if err != nil {
		return nil, err
	}
	wk, err := h.db.BackendHoursOf(ctx, l.Env, l.RestaurantID)
	if err != nil {
		return nil, err
	}
	cur := venueValues{Address: rest.Address, Lat: rest.Latitude, Lon: rest.Longitude, Timezone: rest.Timezone, OwnHours: own}
	if own {
		cur.Week = win.Week
	}
	b := venueValues{Address: br.FullAddress(), Lat: br.Latitude, Lon: br.Longitude, Timezone: br.Timezone, Week: wk}
	out, changed, nf := mergeBackend(cur, b, follow)
	locChanged, hoursChanged := false, false
	for _, f := range changed {
		if f == followHours {
			hoursChanged = true
		} else {
			locChanged = true
		}
	}
	if locChanged {
		if err := h.db.UpdateRestaurantLocation(ctx, venueID, out.Address, out.Lat, out.Lon, out.Timezone); err != nil {
			return nil, err
		}
	}
	if hoursChanged {
		sw := db.ServiceWindow{RestaurantID: &venueID, NightOpenMin: win.NightOpenMin, NightCloseMin: win.NightCloseMin, Week: out.Week}
		if own {
			sw.TZ = win.TZ
		}
		if err := h.db.SetServiceWindow(ctx, sw); err != nil {
			return nil, err
		}
		h.pushGuestToRestaurant(ctx, venueID)
	}
	if err := h.db.SetVenueBackendFollow(ctx, venueID, nf); err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		var names []string
		for _, f := range changed {
			names = append(names, followLabel(f))
		}
		detail := fmt.Sprintf("%s set from AIO backend (%s #%d)", strings.Join(names, ", "), aiobackend.Env(l.Env).Label(), l.RestaurantID)
		if err := h.db.InsertAudit(ctx, "aio-backend", "restaurant.backend_sync", venueID.String(), detail); err != nil {
			log.Printf("[backend-sync] audit: %v", err)
		}
	}
	return changed, nil
}

// syncLinkedVenues refreshes the hours and applies the backend to every venue linked in
// env, after that environment's copy was refreshed.
func (h *Handler) syncLinkedVenues(ctx context.Context, env aiobackend.Env) {
	links, err := h.db.VenueBackendLinks(ctx)
	if err != nil {
		log.Printf("[backend-sync] links: %v", err)
		return
	}
	for id, l := range links {
		if l.Env != string(env) {
			continue
		}
		if err := h.refreshBackendHours(ctx, l); err != nil {
			log.Printf("[backend-sync] hours for %s: %v", id, err)
		}
		if changed, err := h.applyBackendToVenue(ctx, id); err != nil {
			log.Printf("[backend-sync] apply to %s: %v", id, err)
		} else if len(changed) > 0 {
			log.Printf("[backend-sync] %s: %s from the backend", id, strings.Join(changed, ", "))
		}
	}
}

// slotMin reads a backend slot time ("11:00", "24:00"); -1 when it isn't one.
func slotMin(s string) int {
	if strings.TrimSpace(s) == "24:00" {
		return 1440
	}
	return parseHHMM(s, -1)
}

// venueBackendFields is what the Location and Service hours rows show about the backend.
type venueBackendFields struct {
	Linked bool
	Follow map[string]bool
	R      *db.BackendRestaurant
	Week   *db.Week
}

func (h *Handler) venueBackendFieldsOf(ctx context.Context, venueID uuid.UUID) venueBackendFields {
	var f venueBackendFields
	l, linked, err := h.db.VenueBackendLinkOf(ctx, venueID)
	if err != nil || !linked {
		return f
	}
	f.Linked = true
	f.Follow, _ = h.db.VenueBackendFollow(ctx, venueID)
	if r, ok, err := h.db.BackendRestaurantOf(ctx, l.Env, l.RestaurantID); err == nil && ok {
		f.R = &r
	}
	f.Week, _ = h.db.BackendHoursOf(ctx, l.Env, l.RestaurantID)
	return f
}

// decorate fills the B1 parts of the Location and Service hours views.
func (f venueBackendFields) decorate(loc *venueLocation, sw *serviceWindowView) {
	if !f.Linked {
		return
	}
	loc.Linked, loc.Follow = true, f.Follow
	if f.R != nil {
		loc.BAddress, loc.BTZ = f.R.FullAddress(), f.R.Timezone
		if f.R.Latitude != nil && f.R.Longitude != nil {
			loc.BPin = fmt.Sprintf("%.5f, %.5f", *f.R.Latitude, *f.R.Longitude)
		}
	}
	sw.Follows = f.Follow[followHours]
	if !sw.Follows && f.Week != nil {
		sw.BackendDays = weekDays(f.Week, 0, 0)
	}
}
