package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/venueicon"
)

// The venue is edited in place (E1, picked 19 Oct 2026): the venue page's side column has
// a Profile card (icon, name, contact email, notes) and a Location card (search an
// address, drag the pin, or take where the venue's devices are; the timezone follows
// what they report). They open like Service hours and Device lock. The old Edit page
// redirects here.

// venueLocation is what the Location card shows and its map draws.
type venueLocation struct {
	Address   string
	Lat, Lon  *float64
	Pin       string // "37.38712, -122.03055" when set
	Timezone  string
	TZLabel   string
	Devices   []venuePoint // the venue's devices that report a position (last day)
	Centre    *venuePoint  // their middle, when any
	SpreadM   int          // how far the farthest is from that middle, in metres
	DeviceTZ  string       // the timezone most of its devices report
	MapsKey   string
	CanSearch bool // the server has a geocoding key
	// AIO backend (B1): which fields follow it, and its values for the ones set here.
	Linked              bool
	Follow              map[string]bool
	BAddress, BPin, BTZ string
}

type venuePoint struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Serial string  `json:"serial,omitempty"`
}

// PointsJSON is the devices for the map script.
func (v venueLocation) PointsJSON() string {
	b, _ := json.Marshal(v.Devices)
	return string(b)
}

func (h *Handler) venueLocationView(rest *db.Restaurant, devices []db.Device) venueLocation {
	v := venueLocation{Address: rest.Address, Lat: rest.Latitude, Lon: rest.Longitude, Timezone: rest.Timezone,
		MapsKey: h.mapsEmbedKey, CanSearch: h.geocoder != nil}
	v.TZLabel = tzLabel(rest.Timezone)
	if v.Lat != nil && v.Lon != nil {
		v.Pin = fmt.Sprintf("%.5f, %.5f", *v.Lat, *v.Lon)
	}
	tzCount := map[string]int{}
	var lats, lons []float64
	for _, d := range devices {
		var x struct {
			Lat *float64 `json:"latitude"`
			Lon *float64 `json:"longitude"`
			TZ  string   `json:"timezone"`
		}
		if json.Unmarshal(d.LatestExtra, &x) != nil {
			continue
		}
		if x.TZ != "" {
			tzCount[x.TZ]++
		}
		if x.Lat == nil || x.Lon == nil || time.Since(d.LastSeenAt) > 24*time.Hour {
			continue
		}
		v.Devices = append(v.Devices, venuePoint{Lat: *x.Lat, Lon: *x.Lon, Serial: d.SerialNumber})
		lats, lons = append(lats, *x.Lat), append(lons, *x.Lon)
	}
	best := 0
	for tz, n := range tzCount {
		if n > best || (n == best && tz < v.DeviceTZ) {
			v.DeviceTZ, best = tz, n
		}
	}
	if len(lats) > 0 {
		// The median, so one device geolocated across town doesn't drag the middle.
		c := venuePoint{Lat: median(lats), Lon: median(lons)}
		v.Centre = &c
		for _, p := range v.Devices {
			if m := int(distanceM(c.Lat, c.Lon, p.Lat, p.Lon)); m > v.SpreadM {
				v.SpreadM = m
			}
		}
	}
	return v
}

// distanceM is the great-circle distance in metres.
func distanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// tzLabel names a zone the way the timezone list does, or the zone itself.
func tzLabel(tz string) string {
	for _, o := range restaurantTimezones {
		if o.Value == tz {
			return o.Label
		}
	}
	return tz
}

// venueBack sends the browser back to the venue page with one toast and, optionally, a
// card left open.
func venueBack(w http.ResponseWriter, r *http.Request, id uuid.UUID, open, msg, typ string) {
	q := url.Values{}
	if open != "" {
		q.Set("open", open)
	}
	if msg != "" {
		q.Set("flash", msg)
		q.Set("flash_type", typ)
	}
	u := "/restaurants/" + id.String()
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	localRedirect(w, r, u)
}

// RestaurantProfileSave is the Profile card: name, contact email, notes.
func (h *Handler) RestaurantProfileSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		venueBack(w, r, id, "profile", "A venue needs a name.", "error")
		return
	}
	contact, err := venueContactEmail(r.FormValue("contact_email"))
	if err != nil {
		venueBack(w, r, id, "profile", err.Error(), "error")
		return
	}
	ctx := r.Context()
	if err := h.db.UpdateRestaurantProfile(ctx, id, name, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetRestaurantContactEmail(ctx, id, contact); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.update", id.String(), name)
	h.pushGuestToRestaurant(ctx, id) // the name is on every table's welcome
	venueBack(w, r, id, "", "Saved.", "success")
}

// RestaurantIconSave is the Profile card's icon: action=fetch (website), upload (file)
// or remove.
func (h *Handler) RestaurantIconSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	if err := r.ParseMultipartForm(3 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		venueBack(w, r, id, "profile", "That file is too large (2 MB at most).", "error")
		return
	}
	ctx := r.Context()
	var icon []byte
	var source, msg string
	switch r.FormValue("action") {
	case "remove":
		msg = "Icon removed."
	case "upload":
		f, _, err := r.FormFile("icon")
		if err != nil {
			venueBack(w, r, id, "profile", "Choose an image to upload.", "error")
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
		if err == nil {
			icon, err = venueicon.Normalize(raw)
		}
		if err != nil {
			venueBack(w, r, id, "profile", "That image can't be used: "+err.Error()+".", "error")
			return
		}
		source, msg = "uploaded", "Icon uploaded."
	default:
		site := r.FormValue("website")
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		icon, source, err = venueicon.Fetch(fctx, site)
		cancel()
		if err != nil {
			venueBack(w, r, id, "profile", "No icon found: "+err.Error()+".", "error")
			return
		}
		msg = "Icon taken from " + strings.TrimSpace(site) + "."
		if strings.HasPrefix(source, "Google") {
			msg = "Icon taken from Google's copy of " + strings.TrimSpace(site) + "'s favicon."
		}
	}
	if err := h.db.SetRestaurantIcon(ctx, id, icon, source); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.icon", id.String(), source)
	venueBack(w, r, id, "profile", msg, "success")
}

// RestaurantLocationSave is the Location card: address, point, timezone.
func (h *Handler) RestaurantLocationSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	lat, lon := parseFloatPtr(r.FormValue("latitude")), parseFloatPtr(r.FormValue("longitude"))
	if (lat == nil) != (lon == nil) || (lat != nil && (math.Abs(*lat) > 90 || math.Abs(*lon) > 180)) {
		venueBack(w, r, id, "location", "That point isn't on the map.", "error")
		return
	}
	tz := strings.TrimSpace(r.FormValue("timezone"))
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			venueBack(w, r, id, "location", "Unknown timezone "+tz+".", "error")
			return
		}
	}
	addr := strings.TrimSpace(r.FormValue("address"))
	// A field someone changes here stops following the AIO backend (B1, "Set here").
	if cur, err := h.db.GetRestaurant(r.Context(), id); err == nil {
		var edited []string
		if addr != strings.TrimSpace(cur.Address) {
			edited = append(edited, followAddress)
		}
		if !samePin(venueValues{Lat: lat, Lon: lon}, venueValues{Lat: cur.Latitude, Lon: cur.Longitude}) && (lat != nil || cur.Latitude != nil) {
			edited = append(edited, followPin)
		}
		if tz != cur.Timezone {
			edited = append(edited, followTimezone)
		}
		if len(edited) > 0 {
			_ = h.db.StopFollowingBackend(r.Context(), id, edited...)
		}
	}
	if err := h.db.UpdateRestaurantLocation(r.Context(), id, addr, lat, lon, tz); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.location", id.String(), r.FormValue("address"))
	venueBack(w, r, id, "", "Location saved.", "success")
}

// RestaurantGeocode answers the Location card's search box.
func (h *Handler) RestaurantGeocode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if h.geocoder == nil || len([]rune(q)) < 4 {
		w.Write([]byte(`{"places":[]}`))
		return
	}
	if len(q) > 200 {
		q = q[:200]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	places, err := h.geocoder.Forward(ctx, q)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "Search isn't answering right now.")
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"places": places})
}
