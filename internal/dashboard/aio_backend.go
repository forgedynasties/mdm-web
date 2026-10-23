package dashboard

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

// AIO backend integration (23 Oct 2026; plan answered the same day). The restaurant
// backend is read through Metabase (the only access there is) and copied nightly; each
// venue links to one restaurant in Prod or Stage (the venue's choice), which the MDM
// suggests and a person confirms. Tablets are listed in the backend by the Nugget app's
// Android ID until a Nugget update makes them send the hardware serial, so devices match
// by serial first and Android ID only where the MDM has one.

const (
	backendSyncEvery = 24 * time.Hour
	// backendRetryAfter spaces attempts after a failure, so a Metabase that is down is
	// not asked every tick.
	backendRetryAfter = 30 * time.Minute
)

// SetBackend gives the dashboard its backend client (nil leaves it off).
func (h *Handler) SetBackend(c *aiobackend.Client) { h.backend = c }

// RunBackendSync copies each environment's restaurants and devices when the copy is a
// day old or someone asked for a sync. Safe to call every few minutes.
func (h *Handler) RunBackendSync(ctx context.Context) {
	if h.backend == nil || !backendSyncBusy.CompareAndSwap(false, true) {
		return
	}
	defer backendSyncBusy.Store(false)
	states, err := h.db.BackendSyncStates(ctx)
	if err != nil {
		log.Printf("[backend-sync] state: %v", err)
		return
	}
	now := time.Now()
	for _, env := range aiobackend.Envs {
		if !backendSyncDue(states[string(env)], now) {
			continue
		}
		if err := h.syncBackendEnv(ctx, env); err != nil {
			log.Printf("[backend-sync] %s: %v", env, err)
			if err := h.db.NoteBackendSyncFailed(ctx, string(env), err.Error()); err != nil {
				log.Printf("[backend-sync] %s: note failure: %v", env, err)
			}
		}
	}
}

// backendSyncDue: never synced, asked for, or a day old — but not within 30 min of a
// failed attempt unless asked.
func backendSyncDue(s db.BackendSyncState, now time.Time) bool {
	if s.Requested {
		return true
	}
	if s.Error != "" && s.AttemptedAt != nil && now.Sub(*s.AttemptedAt) < backendRetryAfter {
		return false
	}
	return s.SyncedAt == nil || now.Sub(*s.SyncedAt) >= backendSyncEvery
}

func (h *Handler) syncBackendEnv(ctx context.Context, env aiobackend.Env) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rs, err := h.backend.Restaurants(ctx, env)
	if err != nil {
		return fmt.Errorf("restaurants: %w", err)
	}
	ds, err := h.backend.Devices(ctx, env)
	if err != nil {
		return fmt.Errorf("devices: %w", err)
	}
	if err := h.db.ReplaceBackendCopy(ctx, string(env), rs, ds); err != nil {
		return err
	}
	log.Printf("[backend-sync] %s: %d restaurants, %d devices", env, len(rs), len(ds))
	h.syncLinkedVenues(ctx, env)
	return nil
}

// backendKey names one backend restaurant.
type backendKey struct {
	Env string
	ID  int64
}

// venueSuggestion is a backend restaurant the MDM thinks a venue is, with why.
type venueSuggestion struct {
	Restaurant db.BackendRestaurant
	Score      float64
	Serials    int     // the venue's devices the backend lists at this restaurant
	Meters     float64 // distance between the two pins; -1 when either has none
	NameMatch  float64 // 0…1 word overlap
}

// Reasons is the suggestion's evidence, strongest first ("2 devices", "same name", "40 m").
func (s venueSuggestion) Reasons() []string {
	var out []string
	if s.Serials > 0 {
		out = append(out, fmt.Sprintf("%d device%s listed there", s.Serials, plural(s.Serials)))
	}
	switch {
	case s.NameMatch >= 0.99:
		out = append(out, "same name")
	case s.NameMatch >= 0.5:
		out = append(out, "similar name")
	}
	if s.Meters >= 0 && s.Meters <= nearMeters {
		out = append(out, fmt.Sprintf("%.0f m away", s.Meters))
	}
	return out
}

// nearMeters is how close two pins must be to count as the same place.
const nearMeters = 200

// suggestVenueLinks ranks backend restaurants for each venue: devices the backend lists
// there (by serial, or the Nugget Android ID), a pin within 200 m, and the name. A venue
// gets at most three suggestions, and only ones with some evidence.
func suggestVenueLinks(venues []db.Restaurant, devices []db.Device, rs []db.BackendRestaurant, bds []db.BackendDevice) map[uuid.UUID][]venueSuggestion {
	where := map[string][]backendKey{} // upper(serial or Android ID) → restaurants
	for _, d := range bds {
		if s := strings.ToUpper(strings.TrimSpace(d.Serial)); s != "" {
			where[s] = append(where[s], backendKey{d.Env, d.RestaurantID})
		}
	}
	hits := map[uuid.UUID]map[backendKey]int{}
	for i := range devices {
		d := &devices[i]
		if d.RestaurantID == nil {
			continue
		}
		seen := map[backendKey]bool{}
		for _, id := range deviceBackendIDs(d) {
			for _, k := range where[id] {
				if seen[k] {
					continue
				}
				seen[k] = true
				if hits[*d.RestaurantID] == nil {
					hits[*d.RestaurantID] = map[backendKey]int{}
				}
				hits[*d.RestaurantID][k]++
			}
		}
	}
	out := map[uuid.UUID][]venueSuggestion{}
	for _, v := range venues {
		var list []venueSuggestion
		vWords := nameWords(v.Name)
		for _, r := range rs {
			s := venueSuggestion{Restaurant: r, Meters: -1}
			s.Serials = hits[v.ID][backendKey{r.Env, r.ID}]
			s.NameMatch = wordOverlap(vWords, nameWords(r.Name))
			if v.Latitude != nil && v.Longitude != nil && r.Latitude != nil && r.Longitude != nil {
				s.Meters = haversineMeters(*v.Latitude, *v.Longitude, *r.Latitude, *r.Longitude)
			}
			near := s.Meters >= 0 && s.Meters <= nearMeters
			if s.Serials == 0 && !near && s.NameMatch < 0.5 {
				continue
			}
			s.Score = float64(s.Serials)*10 + s.NameMatch*4
			if near {
				s.Score += 5
			}
			list = append(list, s)
		}
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Score != list[j].Score {
				return list[i].Score > list[j].Score
			}
			// Same evidence in both environments: Prod first.
			return list[i].Restaurant.Env == string(aiobackend.Prod) && list[j].Restaurant.Env != string(aiobackend.Prod)
		})
		if len(list) > 3 {
			list = list[:3]
		}
		if len(list) > 0 {
			out[v.ID] = list
		}
	}
	return out
}

// deviceBackendIDs are the ids the backend may list a device under: its serial and, for
// tablets until the Nugget app sends serials, the Android IDs its Nugget builds see.
func deviceBackendIDs(d *db.Device) []string {
	ids := []string{strings.ToUpper(d.SerialNumber)}
	for _, n := range nuggetAndroidIDs(d.LatestExtra) {
		ids = append(ids, strings.ToUpper(n.AndroidID))
	}
	return ids
}

// nameStop are words that say nothing about which restaurant it is.
var nameStop = map[string]bool{"the": true, "restaurant": true, "food": true, "and": true, "of": true, "cafe": true, "kitchen": true, "grill": true, "bar": true}

func nameWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 && !nameStop[w] {
			out[w] = true
		}
	}
	return out
}

// wordOverlap is the share of the shorter name's words found in the other (1 = all).
func wordOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// unenrolledAt splits a restaurant's backend devices that the MDM does not manage (no
// device anywhere with that serial or Nugget Android ID; known from db.KnownBackendIDs)
// into rows with a hardware serial and a count per type of rows keyed by an Android ID,
// which can't be told apart from the MDM's own tablets until the Nugget app sends
// serials (user, 23 Oct: "can still show up in the venue but should show up as
// unenrolled on mdm"). Printers, drawers and payment terminals are left out.
func unenrolledAt(bds []db.BackendDevice, known map[string]bool) (rows []db.BackendDevice, byAndroidID map[string]int) {
	byAndroidID = map[string]int{}
	for _, d := range bds {
		if d.Revoked || !backendManageable(d.Type) {
			continue
		}
		id := strings.ToUpper(strings.TrimSpace(d.Serial))
		if id == "" || known[id] {
			continue
		}
		if androidIDRe.MatchString(d.Serial) {
			byAndroidID[d.Type]++
			continue
		}
		rows = append(rows, d)
	}
	return rows, byAndroidID
}

// androidIDRe is an Android ID as the backend stores it (16 hex digits) — what a tablet or
// POS app row carries instead of a hardware serial.
var androidIDRe = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)

// backendManageable: device types that run Android and could be enrolled (printers, cash
// drawers and payment terminals can't).
func backendManageable(t string) bool {
	switch t {
	case "pos", "kds", "kdx", "kdc", "kiosk", "mpos", "tv", "nugget":
		return true
	}
	return false
}

// backendTypeClass maps a backend device type to the MDM's class key.
func backendTypeClass(t string) string {
	switch t {
	case "nugget":
		return "t7"
	case "tv":
		return "dongle"
	case "kds", "kdx", "kdc":
		return "kds"
	case "pos":
		return "pos"
	case "mpos":
		return "mpos"
	case "kiosk":
		return "kiosk"
	}
	return ""
}
