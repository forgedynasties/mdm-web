package dashboard

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
	"mdm/internal/ratelimit"
	"mdm/internal/venueicon"
)

// New venue N1 (picked 4 Nov 2026): the page opens with one search over Google Maps and the
// AIO backend's restaurants. A Google place and a backend restaurant that are the same place
// (pins within 150 m, the same phone, or the same name in the same city) are one row with both
// badges. Picking a row fills the venue (name, address, pin, timezone), links it to the
// backend restaurant (which fills hours and follows the backend, as Venue settings › AIO
// backend does), offers the devices the backend lists there, and takes the backend's logo
// (else the website's icon) as the venue icon.

type vnGoogle struct {
	ID      string  `json:"id"`
	Rating  float64 `json:"rating,omitempty"`
	Reviews int     `json:"reviews,omitempty"`
	Phone   string  `json:"phone,omitempty"`
	Website string  `json:"website,omitempty"`
}

type vnDev struct {
	Serial string `json:"serial"`
	Label  string `json:"label"`
	Venue  string `json:"venue,omitempty"` // where it is now ("" = no venue)
}

type vnBackend struct {
	Env      string  `json:"env"`
	EnvLabel string  `json:"env_label"`
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Total    int     `json:"total"`   // devices the backend lists there
	Devices  []vnDev `json:"devices"` // of those, the ones in the MDM (visible to the viewer)
}

type vnVenue struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type vnHit struct {
	Name    string     `json:"name"`
	Address string     `json:"address"`
	Lat     *float64   `json:"lat,omitempty"`
	Lng     *float64   `json:"lng,omitempty"`
	TZ      string     `json:"tz,omitempty"`
	Color   string     `json:"color"`
	Google  *vnGoogle  `json:"google,omitempty"`
	Backend *vnBackend `json:"backend,omitempty"`
	Venue   *vnVenue   `json:"venue,omitempty"` // already a venue: open it instead
	Why     string     `json:"why,omitempty"`
	Test    bool       `json:"test,omitempty"`
}

var vnSearchHits = ratelimit.New(time.Minute)

// VenueNewSearch is GET /restaurants/new/search?q=.
func (h *Handler) VenueNewSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 120 {
		writeJSONResp(w, map[string]any{"results": []vnHit{}})
		return
	}
	ctx := r.Context()
	src, err := h.loadVenueSearchSources(r)
	if err != nil {
		log.Printf("[venue-search] sources: %v", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	var places []gPlace
	google := "off"
	if placesKey() != "" {
		google = "ok"
		// Google ranks the whole country; when the backend knows a restaurant by that name,
		// look around it first so the two can meet ("ole ole" → Oakland, not Ithaca).
		var near *db.BackendRestaurant
		for _, br := range src.byName(q) {
			if br.Latitude != nil && br.Longitude != nil && !testRestaurantRe.MatchString(br.Name) {
				near = &br
				break
			}
		}
		if n, _ := vnSearchHits.Hit(h.currentUsername(r)); n > 90 {
			google = "busy"
		} else if ps, err := searchPlaces(ctx, q, near); err != nil {
			log.Printf("[venue-search] places: %v", err)
			google = "error"
		} else {
			places = ps
		}
	}
	writeJSONResp(w, map[string]any{"results": src.match(q, places), "google": google})
}

// gPlace is one Places (New) Text Search result.
type gPlace struct {
	ID          string `json:"id"`
	DisplayName struct {
		Text string `json:"text"`
	} `json:"displayName"`
	FormattedAddress string `json:"formattedAddress"`
	Location         *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	TimeZone struct {
		ID string `json:"id"`
	} `json:"timeZone"`
	Rating              float64 `json:"rating"`
	UserRatingCount     int     `json:"userRatingCount"`
	NationalPhoneNumber string  `json:"nationalPhoneNumber"`
	WebsiteURI          string  `json:"websiteUri"`
}

func searchPlaces(ctx context.Context, q string, near *db.BackendRestaurant) ([]gPlace, error) {
	var res struct {
		Places []gPlace `json:"places"`
	}
	body := map[string]any{"textQuery": q, "maxResultCount": 6, "includedType": "restaurant"}
	if near != nil {
		body["locationBias"] = map[string]any{"circle": map[string]any{
			"center": map[string]float64{"latitude": *near.Latitude, "longitude": *near.Longitude}, "radius": 50000.0}}
	}
	mask := "places.id,places.displayName,places.formattedAddress,places.location,places.timeZone,places.rating,places.userRatingCount,places.nationalPhoneNumber,places.websiteUri"
	if err := placesCall(ctx, "POST", "https://places.googleapis.com/v1/places:searchText", body, mask, &res); err != nil {
		return nil, err
	}
	return res.Places, nil
}

// vnSources is what a search matches against: the backend copy, the MDM's venues and links,
// and which of the viewer's devices each backend restaurant lists.
type vnSources struct {
	rests  []db.BackendRestaurant
	venues []db.Restaurant
	linked map[string]uuid.UUID // backend key → venue
	names  map[uuid.UUID]string
	devs   map[string][]vnDev // backend key → devices in the MDM
	total  map[string]int
}

func (h *Handler) loadVenueSearchSources(r *http.Request) (*vnSources, error) {
	ctx := r.Context()
	s := &vnSources{linked: map[string]uuid.UUID{}, names: map[uuid.UUID]string{}, devs: map[string][]vnDev{}, total: map[string]int{}}
	var err error
	if s.rests, err = h.db.ListBackendRestaurants(ctx); err != nil {
		return nil, err
	}
	if s.venues, err = h.db.ListRestaurants(ctx); err != nil {
		return nil, err
	}
	for _, v := range s.venues {
		s.names[v.ID] = v.Name
	}
	links, err := h.db.VenueBackendLinks(ctx)
	if err != nil {
		return nil, err
	}
	for v, l := range links {
		s.linked[bkKey(l.Env, l.RestaurantID)] = v
	}
	bds, err := h.db.ListBackendDevices(ctx)
	if err != nil {
		return nil, err
	}
	where := map[string][]string{}
	for _, d := range bds {
		if d.Revoked {
			continue
		}
		k := bkKey(d.Env, d.RestaurantID)
		s.total[k]++
		if sn := strings.ToUpper(strings.TrimSpace(d.Serial)); sn != "" {
			where[sn] = append(where[sn], k)
		}
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		return nil, err
	}
	for _, d := range h.access(r).keepVisible(devices) {
		if d.Hidden || d.Retired() {
			continue
		}
		seen := map[string]bool{}
		for _, id := range deviceBackendIDs(&d) {
			for _, k := range where[id] {
				if seen[k] {
					continue
				}
				seen[k] = true
				vd := vnDev{Serial: d.SerialNumber, Label: d.ClassLabel()}
				if d.RestaurantID != nil {
					vd.Venue = s.names[*d.RestaurantID]
				}
				s.devs[k] = append(s.devs[k], vd)
			}
		}
	}
	return s, nil
}

// match joins the Google places and the backend restaurants matching q into rows.
func (s *vnSources) match(q string, places []gPlace) []vnHit {
	out := []vnHit{}
	used := map[string]bool{}
	for _, p := range places {
		hit := vnHit{Name: p.DisplayName.Text, Address: trimUSA(p.FormattedAddress), TZ: p.TimeZone.ID,
			Google: &vnGoogle{ID: p.ID, Rating: p.Rating, Reviews: p.UserRatingCount, Phone: p.NationalPhoneNumber, Website: p.WebsiteURI}}
		if p.Location != nil {
			lat, lng := p.Location.Latitude, p.Location.Longitude
			hit.Lat, hit.Lng = &lat, &lng
		}
		if br, why := s.sameAs(p); br != nil {
			k := bkKey(br.Env, br.ID)
			if !used[k] {
				used[k] = true
				hit.Backend = s.backendOf(*br)
				hit.Why = why
				hit.Test = testRestaurantRe.MatchString(br.Name)
				if hit.TZ == "" {
					hit.TZ = br.Timezone
				}
			}
		}
		hit.Venue = s.venueFor(&hit)
		out = append(out, hit)
	}
	// Backend restaurants whose name (or city) matches what was typed, not already joined.
	var extra []vnHit
	for _, br := range s.byName(q) {
		k := bkKey(br.Env, br.ID)
		if used[k] {
			continue
		}
		used[k] = true
		hit := vnHit{Name: br.Name, Address: trimUSA(br.FullAddress()), Lat: br.Latitude, Lng: br.Longitude, TZ: br.Timezone,
			Backend: s.backendOf(br), Test: testRestaurantRe.MatchString(br.Name)}
		if len(places) > 0 {
			hit.Why = "Not found on Google Maps under this name."
		}
		hit.Venue = s.venueFor(&hit)
		extra = append(extra, hit)
	}
	sort.SliceStable(extra, func(i, j int) bool {
		a, b := extra[i], extra[j]
		if a.Test != b.Test {
			return !a.Test
		}
		return len(a.Backend.Devices) > len(b.Backend.Devices)
	})
	out = append(out, extra...)
	if len(out) > 10 {
		out = out[:10]
	}
	for i := range out {
		out[i].Color = vnColor(out[i].Name)
	}
	return out
}

// byName lists the backend restaurants whose name, business or city matches q.
func (s *vnSources) byName(q string) []db.BackendRestaurant {
	nq := vnNorm(q)
	var out []db.BackendRestaurant
	if nq == "" {
		return out
	}
	for _, br := range s.rests {
		if strings.Contains(vnNorm(br.Name), nq) || strings.Contains(vnNorm(br.BusinessName), nq) || vnNorm(br.City) == nq {
			out = append(out, br)
		}
	}
	return out
}

func trimUSA(a string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(a), ", USA"), ", United States")
}

// sameAs finds the backend restaurant that is the same place as a Google result.
func (s *vnSources) sameAs(p gPlace) (*db.BackendRestaurant, string) {
	var best *db.BackendRestaurant
	bestD := math.MaxFloat64
	if p.Location != nil {
		for i := range s.rests {
			br := &s.rests[i]
			if br.Latitude == nil || br.Longitude == nil {
				continue
			}
			if d := haversineM(p.Location.Latitude, p.Location.Longitude, *br.Latitude, *br.Longitude); d <= 150 && d < bestD {
				best, bestD = br, d
			}
		}
	}
	if best != nil {
		return best, vnNameNote(p.DisplayName.Text, best.Name, "Same pin.")
	}
	if ph := vnDigits(p.NationalPhoneNumber); len(ph) >= 10 {
		for i := range s.rests {
			if vnDigits(s.rests[i].Phone) == ph {
				return &s.rests[i], vnNameNote(p.DisplayName.Text, s.rests[i].Name, "Same phone number.")
			}
		}
	}
	gn := vnNorm(p.DisplayName.Text)
	for i := range s.rests {
		br := &s.rests[i]
		if gn != "" && vnNorm(br.Name) == gn && br.City != "" && strings.Contains(strings.ToLower(p.FormattedAddress), strings.ToLower(br.City)) {
			return br, "Same name in " + br.City + "."
		}
	}
	return nil, ""
}

func vnNameNote(google, backend, base string) string {
	if vnNorm(google) != vnNorm(backend) {
		return base + ` The backend calls it "` + backend + `".`
	}
	return ""
}

func (s *vnSources) backendOf(br db.BackendRestaurant) *vnBackend {
	k := bkKey(br.Env, br.ID)
	return &vnBackend{Env: br.Env, EnvLabel: aiobackend.Env(br.Env).Label(), ID: br.ID, Name: br.Name,
		Total: s.total[k], Devices: append([]vnDev{}, s.devs[k]...)}
}

// venueFor is the existing venue a row already is: the venue linked to its backend
// restaurant, else a venue pinned within 100 m.
func (s *vnSources) venueFor(hit *vnHit) *vnVenue {
	if b := hit.Backend; b != nil {
		if v, ok := s.linked[bkKey(b.Env, b.ID)]; ok {
			return &vnVenue{ID: v.String(), Name: s.names[v]}
		}
	}
	if hit.Lat == nil || hit.Lng == nil {
		return nil
	}
	for _, v := range s.venues {
		if v.Latitude != nil && v.Longitude != nil && haversineM(*hit.Lat, *hit.Lng, *v.Latitude, *v.Longitude) <= 100 {
			return &vnVenue{ID: v.ID.String(), Name: v.Name}
		}
	}
	return nil
}

func vnNorm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func vnDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 10 {
		d = d[len(d)-10:]
	}
	return d
}

// vnColor is a row's initial-tile colour, steady per name.
func vnColor(name string) string {
	pal := []string{"#c53030", "#b7791f", "#2f6db5", "#1d7a43", "#6a3fd0", "#0f5c4c", "#2f9e8f", "#c2410c"}
	f := fnv.New32a()
	f.Write([]byte(name))
	return pal[f.Sum32()%uint32(len(pal))]
}

// fillVenueIcon gives a venue with no icon the AIO backend's logo for its linked restaurant,
// else its website's icon (user, 4 Nov 2026: "sync icon from backend if you find"). Runs
// after the request, so a slow site never holds up the page.
func (h *Handler) fillVenueIcon(venue uuid.UUID, env string, rid int64, website string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if icon, _, _, err := h.db.RestaurantIcon(ctx, venue); err == nil && len(icon) > 0 {
			return
		}
		if h.backend != nil && env != "" && rid > 0 {
			if u, err := h.backend.Logo(ctx, aiobackend.Env(env), rid); err == nil && u != "" {
				if png, err := venueicon.FetchImage(ctx, u); err == nil {
					if err := h.db.SetRestaurantIcon(ctx, venue, png, "AIO backend logo"); err == nil {
						_ = h.db.InsertAudit(ctx, "aio-backend", "restaurant.icon", venue.String(), "logo from the AIO backend")
					}
					return
				} else {
					log.Printf("[venue-icon] backend logo for %s: %v", venue, err)
				}
			}
		}
		if website != "" {
			if png, source, err := venueicon.Fetch(ctx, website); err == nil {
				if err := h.db.SetRestaurantIcon(ctx, venue, png, source); err == nil {
					_ = h.db.InsertAudit(ctx, "venue-search", "restaurant.icon", venue.String(), "icon from "+source)
				}
			}
		}
	}()
}

// New venue S1 (picked 4 Nov 2026): under the search, the backend restaurants that list the
// viewer's devices but have no venue (one click fills the card, as a search pick does), and
// the venues added lately (so one isn't added twice).
type vnStartRow struct {
	Hit     vnHit
	JSON    string
	Initial string
	Where   string
	InMDM   int // the backend's devices that are in the MDM
	Free    int // of those, at no venue
}

type vnStart struct {
	Waiting []vnStartRow
	Recent  []db.Restaurant
}

func (h *Handler) venueNewStart(r *http.Request) vnStart {
	var out vnStart
	s, err := h.loadVenueSearchSources(r)
	if err != nil {
		log.Printf("[venue-new] start: %v", err)
		return out
	}
	ign, _ := h.db.BackendIgnores(r.Context())
	for _, br := range s.rests {
		k := bkKey(br.Env, br.ID)
		devs := s.devs[k]
		if len(devs) == 0 || ign[k] {
			continue
		}
		if _, linked := s.linked[k]; linked {
			continue
		}
		hit := vnHit{Name: br.Name, Address: trimUSA(br.FullAddress()), Lat: br.Latitude, Lng: br.Longitude, TZ: br.Timezone,
			Backend: s.backendOf(br), Test: testRestaurantRe.MatchString(br.Name), Color: vnColor(br.Name)}
		row := vnStartRow{Hit: hit, InMDM: len(devs), Initial: vnInitial(br.Name)}
		for _, d := range devs {
			if d.Venue == "" {
				row.Free++
			}
		}
		row.Where = strings.TrimSpace(strings.Trim(br.City+", "+usStateAbbr(br.State), ", "))
		if b, err := json.Marshal(hit); err == nil {
			row.JSON = string(b)
		}
		out.Waiting = append(out.Waiting, row)
	}
	sort.SliceStable(out.Waiting, func(i, j int) bool {
		a, b := out.Waiting[i], out.Waiting[j]
		if a.Hit.Test != b.Hit.Test {
			return !a.Hit.Test
		}
		if a.InMDM != b.InMDM {
			return a.InMDM > b.InMDM
		}
		return a.Hit.Name < b.Hit.Name
	})
	if len(out.Waiting) > 6 {
		out.Waiting = out.Waiting[:6]
	}
	recent := append([]db.Restaurant{}, s.venues...)
	sort.Slice(recent, func(i, j int) bool { return recent[i].CreatedAt.After(recent[j].CreatedAt) })
	if len(recent) > 4 {
		recent = recent[:4]
	}
	out.Recent = recent
	return out
}

func vnInitial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

// usStateAbbr shortens a full US state name ("California" → "CA"); anything else is kept.
func usStateAbbr(s string) string {
	for ab, name := range usStates {
		if strings.EqualFold(name, s) {
			return ab
		}
	}
	return s
}
