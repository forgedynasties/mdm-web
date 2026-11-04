package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"mdm/internal/ratelimit"
)

// OnboardingDemo: GET /restaurant-onboarding-demo, the restaurant owner sign-up demo (O1 "AI
// concierge" with O3's layout choice, picked 4 Nov 2026), open to anyone with the link: no
// sign-in (user: "no sign in gate"). A static page; nothing is saved. The restaurant search is
// Google Places (New) through the two endpoints below, so the key stays on the server
// (GOOGLE_PLACES_API_KEY); without a key the page falls back to a sample list.
func (h *Handler) OnboardingDemo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Robots-Tag", "noindex")
	http.ServeFile(w, r, "static/restaurant-onboarding-demo.html")
}

var (
	placesHits   = ratelimit.New(time.Minute)
	placesClient = &http.Client{Timeout: 8 * time.Second}
)

const placesPerMinute = 60

func placesKey() string { return strings.TrimSpace(os.Getenv("GOOGLE_PLACES_API_KEY")) }

// placesGate answers 404 without a key (the page then uses its sample list) and 429 past
// placesPerMinute calls a minute from one address: the endpoints are public.
func placesGate(w http.ResponseWriter, r *http.Request) bool {
	if placesKey() == "" {
		http.Error(w, "places not configured", http.StatusNotFound)
		return false
	}
	if n, _ := placesHits.Hit(ratelimit.ClientIP(r)); n > placesPerMinute {
		http.Error(w, "too many searches, try again in a minute", http.StatusTooManyRequests)
		return false
	}
	return true
}

func placesCall(ctx context.Context, method, endpoint string, body any, fieldMask string, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", placesKey())
	if fieldMask != "" {
		req.Header.Set("X-Goog-FieldMask", fieldMask)
	}
	resp, err := placesClient.Do(req)
	if err != nil {
		return fmt.Errorf("places: %s", strings.ReplaceAll(err.Error(), placesKey(), "…"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("places returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// OnboardingPlacesSearch: GET /restaurant-onboarding-demo/places?q=&s=<session>, restaurants,
// cafés and bars matching what the owner typed (Places Autocomplete, New).
func (h *Handler) OnboardingPlacesSearch(w http.ResponseWriter, r *http.Request) {
	if !placesGate(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 120 {
		writeJSONResp(w, map[string]any{"places": []any{}})
		return
	}
	body := map[string]any{
		"input":                q,
		"includedPrimaryTypes": []string{"restaurant", "cafe", "bar", "bakery", "meal_takeaway"},
	}
	if s := r.URL.Query().Get("s"); s != "" && len(s) <= 64 {
		body["sessionToken"] = s
	}
	var res struct {
		Suggestions []struct {
			PlacePrediction *struct {
				PlaceID          string `json:"placeId"`
				StructuredFormat struct {
					MainText      struct{ Text string } `json:"mainText"`
					SecondaryText struct{ Text string } `json:"secondaryText"`
				} `json:"structuredFormat"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	if err := placesCall(r.Context(), "POST", "https://places.googleapis.com/v1/places:autocomplete", body, "", &res); err != nil {
		http.Error(w, "search failed", http.StatusBadGateway)
		return
	}
	type place struct {
		ID      string `json:"id"`
		Name    string `json:"n"`
		Address string `json:"a"`
	}
	out := []place{}
	for _, s := range res.Suggestions {
		if p := s.PlacePrediction; p != nil && len(out) < 6 {
			out = append(out, place{p.PlaceID, p.StructuredFormat.MainText.Text, p.StructuredFormat.SecondaryText.Text})
		}
	}
	writeJSONResp(w, map[string]any{"places": out})
}

// OnboardingPlaceDetails: GET /restaurant-onboarding-demo/place?id=&s=, what the restaurant
// card shows: name, address, pin, rating, kind, today's hours, phone, website, and a photo
// (its googleusercontent address, resolved here so the key never reaches the page).
func (h *Handler) OnboardingPlaceDetails(w http.ResponseWriter, r *http.Request) {
	if !placesGate(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 300 || strings.ContainsAny(id, "/?#") {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	endpoint := "https://places.googleapis.com/v1/places/" + url.PathEscape(id)
	if s := r.URL.Query().Get("s"); s != "" && len(s) <= 64 {
		endpoint += "?sessionToken=" + url.QueryEscape(s)
	}
	var p struct {
		DisplayName            struct{ Text string } `json:"displayName"`
		FormattedAddress       string                `json:"formattedAddress"`
		Location               struct{ Latitude, Longitude float64 }
		Rating                 float64 `json:"rating"`
		UserRatingCount        int     `json:"userRatingCount"`
		PrimaryTypeDisplayName struct{ Text string } `json:"primaryTypeDisplayName"`
		NationalPhoneNumber    string                `json:"nationalPhoneNumber"`
		WebsiteURI             string                `json:"websiteUri"`
		RegularOpeningHours    struct {
			WeekdayDescriptions []string `json:"weekdayDescriptions"`
		} `json:"regularOpeningHours"`
		Photos []struct {
			Name string `json:"name"`
		} `json:"photos"`
	}
	mask := "displayName,formattedAddress,location,rating,userRatingCount,primaryTypeDisplayName,nationalPhoneNumber,websiteUri,regularOpeningHours.weekdayDescriptions,photos"
	if err := placesCall(r.Context(), "GET", endpoint, nil, mask, &p); err != nil {
		http.Error(w, "lookup failed", http.StatusBadGateway)
		return
	}
	hours := ""
	if ds := p.RegularOpeningHours.WeekdayDescriptions; len(ds) == 7 {
		// Google lists Monday first; today's line, without the day's name.
		d := ds[(int(time.Now().Weekday())+6)%7]
		if i := strings.Index(d, ": "); i > 0 {
			d = d[i+2:]
		}
		hours = "Today " + d
	}
	photo := ""
	if len(p.Photos) > 0 && strings.HasPrefix(p.Photos[0].Name, "places/") {
		var m struct {
			PhotoURI string `json:"photoUri"`
		}
		if placesCall(r.Context(), "GET", "https://places.googleapis.com/v1/"+p.Photos[0].Name+"/media?maxWidthPx=240&skipHttpRedirect=true", nil, "", &m) == nil {
			photo = m.PhotoURI
		}
	}
	writeJSONResp(w, map[string]any{
		"n": p.DisplayName.Text, "a": p.FormattedAddress, "lat": p.Location.Latitude, "lng": p.Location.Longitude,
		"r": p.Rating, "rc": p.UserRatingCount, "t": p.PrimaryTypeDisplayName.Text, "h": hours,
		"p": p.NationalPhoneNumber, "w": p.WebsiteURI, "photo": photo,
	})
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
