package geolocate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Geocoder reverse-geocodes latitude/longitude to a human-readable street
// address using the Google Geocoding API. It is deliberately separate from
// Resolver: the two use different Google APIs (and, in this deployment,
// different API keys). Safe for concurrent use.
//
// A device located from its WiFi neighbourhood barely moves, so results are
// cached aggressively keyed on coordinates rounded to ~11m, with a long TTL —
// the Geocoding API is billed per request and the address for a fixed point is
// effectively static.
type Geocoder struct {
	apiKey string
	client *http.Client

	mu    sync.RWMutex
	cache map[string]cachedAddress
	meter *Meter
}

// Stats returns a snapshot of this geocoder's Google Geocoding API usage.
func (g *Geocoder) Stats() MeterSnapshot { return g.meter.Snapshot() }

type cachedAddress struct {
	Address   string
	ExpiresAt time.Time
}

// NewGeocoder creates a Geocoder bound to a Google Geocoding API key. apiKey
// must be non-empty.
func NewGeocoder(apiKey string) *Geocoder {
	return &Geocoder{
		apiKey: apiKey,
		client: &http.Client{Timeout: 15 * time.Second},
		cache:  make(map[string]cachedAddress),
		meter:  newMeter(),
	}
}

// geocodeResponse matches the fields we read from the Google Geocoding API
// success body. status is "OK" on success; anything else (ZERO_RESULTS,
// OVER_QUERY_LIMIT, REQUEST_DENIED, …) is treated as no address.
type geocodeResponse struct {
	Status  string `json:"status"`
	Results []struct {
		FormattedAddress string `json:"formatted_address"`
	} `json:"results"`
	ErrorMessage string `json:"error_message"`
}

const googleGeocodeURL = "https://maps.googleapis.com/maps/api/geocode/json"

// addrCacheKey rounds coordinates to 4 decimal places (~11m) so tiny WiFi
// geolocation jitter around a fixed device doesn't churn the cache or waste
// paid lookups.
func addrCacheKey(lat, lon float64) string {
	return fmt.Sprintf("%.4f,%.4f", math.Round(lat*1e4)/1e4, math.Round(lon*1e4)/1e4)
}

// Reverse resolves lat/lon to a formatted street address. Returns an empty
// string (and nil error) when Google has no address for the point; returns an
// error only on transport/decoding failure. Results are cached for 6 hours.
func (g *Geocoder) Reverse(ctx context.Context, lat, lon float64) (string, error) {
	key := addrCacheKey(lat, lon)

	g.mu.RLock()
	if c, ok := g.cache[key]; ok && time.Now().Before(c.ExpiresAt) {
		g.mu.RUnlock()
		g.meter.MarkHit()
		return c.Address, nil
	}
	g.mu.RUnlock()

	endpoint := googleGeocodeURL +
		"?latlng=" + url.QueryEscape(key) +
		"&key=" + url.QueryEscape(g.apiKey)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		g.meter.MarkError(err)
		return "", err
	}

	resp, err := g.client.Do(req)
	if err != nil {
		g.meter.MarkError(err)
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// The URL carries the key, so log only status + response body, never the URL.
		log.Printf("[geocode] google %d — response: %s", resp.StatusCode, string(body))
		gerr := fmt.Errorf("google geocode returned %d", resp.StatusCode)
		g.meter.MarkError(gerr)
		return "", gerr
	}

	var gResp geocodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		g.meter.MarkError(err)
		return "", err
	}

	var addr string
	if gResp.Status == "OK" && len(gResp.Results) > 0 {
		addr = gResp.Results[0].FormattedAddress
		g.meter.MarkSuccess()
	} else if gResp.Status == "ZERO_RESULTS" {
		// A valid, billable response — Google simply has no address for the point.
		g.meter.MarkSuccess()
	} else {
		// Quota/permission problems (REQUEST_DENIED, OVER_QUERY_LIMIT, …). Surface in
		// the log and count as an error so the usage panel flags it; still cache the
		// empty result briefly below so a broken key doesn't hammer the API.
		log.Printf("[geocode] google status=%s: %s", gResp.Status, gResp.ErrorMessage)
		g.meter.MarkError(fmt.Errorf("geocode status %s: %s", gResp.Status, gResp.ErrorMessage))
	}

	ttl := 6 * time.Hour
	if addr == "" {
		// Don't cache an empty answer for long — a transient error or an
		// out-of-coverage point may resolve later once the device moves.
		ttl = 5 * time.Minute
	}
	g.mu.Lock()
	g.cache[key] = cachedAddress{Address: addr, ExpiresAt: time.Now().Add(ttl)}
	if len(g.cache) > 2000 {
		for k := range g.cache {
			delete(g.cache, k)
			break
		}
	}
	g.mu.Unlock()

	return addr, nil
}
