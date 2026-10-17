package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"math"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// deviceMapPoint is one device pin on the overview fleet map.
type deviceMapPoint struct {
	Serial  string  `json:"serial"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Address string  `json:"address,omitempty"`
	Online  bool    `json:"online"`
	// Extra facets for the dedicated /map page's filters and list.
	Product      string `json:"product"`
	Battery      int    `json:"battery"`
	Kiosk        bool   `json:"kiosk"`
	Build        string `json:"build,omitempty"`
	RestaurantID string `json:"restaurant_id,omitempty"`
	Restaurant   string `json:"restaurant,omitempty"`
	LastSeen     string `json:"last_seen"`
	// Site groups devices that stand in the same place: the restaurant when there is one,
	// else the position snapped to a ~30 m grid. The fleet map draws one blip per site.
	Site string `json:"site"`
	// Region is the country a device's address ends in, for the zoomed-out region chips.
	Region string `json:"region,omitempty"`
	// Where the venue is and how far the device is from it, when the venue has a position.
	// Away is true past the configured limit; such a device is its own site, since its
	// restaurant's blip would otherwise be dragged towards it.
	VenueLat *float64 `json:"venue_lat,omitempty"`
	VenueLon *float64 `json:"venue_lon,omitempty"`
	AwayM    *int     `json:"away_m,omitempty"`
	Away     bool     `json:"away,omitempty"`
}

// distanceMeters is the great-circle distance between two positions.
func distanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

// mapRegion names the area an address is in: its last comma-separated part, which for a
// reverse-geocoded address is the country ("..., San Jose, CA 95120, USA"). "" when the
// address has no country on the end.
func mapRegion(addr string) string {
	parts := strings.Split(addr, ",")
	if len(parts) < 2 {
		return ""
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	switch strings.ToUpper(last) {
	case "USA", "US", "UNITED STATES OF AMERICA":
		return "United States"
	}
	// "CA 95120" is a state and a postcode with the country missing, not a region.
	for _, r := range last {
		if r >= '0' && r <= '9' {
			return ""
		}
	}
	return last
}

// mapSiteKey is the key devices share on the fleet map. A restaurant wins over position,
// since wifi-derived fixes for its tablets differ by a few metres.
func mapSiteKey(restaurantID *uuid.UUID, lat, lon float64) string {
	if restaurantID != nil {
		return "r:" + restaurantID.String()
	}
	const grid = 0.0003 // ~33 m of latitude
	return fmt.Sprintf("g:%d,%d", int64(math.Floor(lat/grid)), int64(math.Floor(lon/grid)))
}

// deviceLocationsJSON returns every device with a resolved lat/lon in its latest
// telemetry, as JSON for the overview fleet map, plus the count. Coordinates come
// from the geolocation pipeline (WiFi scan -> lat/lon merged into the device extra).
func (h *Handler) deviceLocationsJSON(ctx context.Context, excludeDPC bool) (template.JS, int) {
	f := db.DeviceFilter{}
	if excludeDPC {
		f.AgentKind = "firmware"
	}
	pts := h.devicePoints(ctx, f)
	b, err := json.Marshal(pts)
	if err != nil {
		return template.JS("[]"), 0
	}
	return template.JS(b), len(pts)
}

// DeviceMapData is the Fleet page's map view: the located devices matching the
// same query string the roster uses (collection, quick view, filters, search),
// as JSON {points:[…], total:N}.
func (h *Handler) DeviceMapData(w http.ResponseWriter, r *http.Request) {
	filter := h.deviceFilterFromRequest(r)
	h.access(r).applyFilter(&filter)
	pts := h.devicePoints(r.Context(), filter)
	total, _ := h.db.CountDevices(r.Context(), filter)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"points": pts, "total": total, "away_limit_m": h.cfg.AwayLimitM()})
}

// devicePoints returns every device matching filter that has a resolved lat/lon.
func (h *Handler) devicePoints(ctx context.Context, filter db.DeviceFilter) []deviceMapPoint {
	devs, err := h.db.ListDevices(ctx, filter, 0, 5000, "", "")
	if err != nil {
		return []deviceMapPoint{}
	}
	online := h.hub.ConnectedIDsForDisplay()
	pts := make([]deviceMapPoint, 0, 16)
	type venuePos struct{ lat, lon float64 }
	venues := map[uuid.UUID]venuePos{}
	if rs, err := h.db.ListRestaurants(ctx); err == nil {
		for _, r := range rs {
			if r.Latitude != nil && r.Longitude != nil {
				venues[r.ID] = venuePos{*r.Latitude, *r.Longitude}
			}
		}
	}
	awayLimit := float64(h.cfg.AwayLimitM())
	for _, dv := range devs {
		var m map[string]json.RawMessage
		if len(dv.LatestExtra) == 0 || json.Unmarshal(dv.LatestExtra, &m) != nil {
			continue
		}
		// Prefer the agent's precise GPS fix (extra.location_lat/lon, reported while
		// fleet policy location_enabled is on) over the WiFi-scan-derived coordinates.
		var lat, lon float64
		if json.Unmarshal(m["location_lat"], &lat) != nil || json.Unmarshal(m["location_lon"], &lon) != nil {
			if json.Unmarshal(m["latitude"], &lat) != nil || json.Unmarshal(m["longitude"], &lon) != nil {
				continue
			}
		}
		name := dv.RestaurantName
		if name == "" {
			name = dv.SerialNumber
		}
		var addr string
		if len(m["location_address"]) > 0 {
			_ = json.Unmarshal(m["location_address"], &addr)
		}
		_, isOn := online[dv.ID]
		p := deviceMapPoint{
			Serial: dv.SerialNumber, Name: name, Lat: lat, Lon: lon, Address: addr, Online: isOn,
			Product: dv.ProductLabel(), Battery: dv.BatteryPct, Kiosk: dv.KioskEnabled, Build: dv.BuildID,
			Restaurant: dv.RestaurantName, LastSeen: dv.LastSeenAt.UTC().Format(time.RFC3339),
		}
		if dv.RestaurantID != nil {
			p.RestaurantID = dv.RestaurantID.String()
		}
		p.Site = mapSiteKey(dv.RestaurantID, lat, lon)
		p.Region = mapRegion(addr)
		if dv.RestaurantID != nil {
			if v, ok := venues[*dv.RestaurantID]; ok {
				d := int(math.Round(distanceMeters(lat, lon, v.lat, v.lon)))
				p.VenueLat, p.VenueLon, p.AwayM = &v.lat, &v.lon, &d
				if float64(d) > awayLimit {
					p.Away = true
					p.Site = "a:" + dv.SerialNumber
				}
			}
		}
		pts = append(pts, p)
	}
	return pts
}
