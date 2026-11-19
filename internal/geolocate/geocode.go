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

	store  AddrStore // shared, persistent address cache and daily call counter (may be nil)

	mu     sync.RWMutex
	cache  map[string]cachedAddress
	places map[string]cachedPlaces // forward lookups, by query
	meter  *Meter

	dailyCap int
}

// AddrStore is the persistence behind the reverse-geocode cache: it survives restarts and is
// shared by every server copy, and it carries the day's paid-request count.
type AddrStore interface {
	GetAddr(ctx context.Context, cell string, maxAgeDays int) (string, bool, error)
	PutAddr(ctx context.Context, cell, address string) error
	// CountCall adds one paid request to today's total and returns it.
	CountCall(ctx context.Context) (int, error)
}

// SetStore attaches the persistent cache; dailyCap bounds the paid requests per UTC day
// (0 = no cap).
func (g *Geocoder) SetStore(s AddrStore, dailyCap int) { g.store, g.dailyCap = s, dailyCap }

// Tunables: a ~110 m cell, a month in the cache. The address of a fixed point is static, and a
// device that really moves lands in another cell.
const (
	addrCacheTTL    = 30 * 24 * time.Hour
	addrCacheDays   = 30
	addrEmptyTTL    = time.Hour
	addrCacheMaxMem = 5000
)

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

// addrCacheKey rounds coordinates to 3 decimal places (~110 m) so Wi-Fi geolocation jitter
// around a fixed device doesn't churn the cache or buy paid lookups (it was 4 places, ~11 m,
// which that jitter kept missing).
func addrCacheKey(lat, lon float64) string {
	return fmt.Sprintf("%.3f,%.3f", math.Round(lat*1e3)/1e3, math.Round(lon*1e3)/1e3)
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

	// The shared store: another copy, or an earlier run, already paid for this cell.
	if g.store != nil {
		if addr, ok, err := g.store.GetAddr(ctx, key, addrCacheDays); err == nil && ok {
			g.remember(key, addr, addrCacheTTL)
			g.meter.MarkLocalHit()
			return addr, nil
		}
		// Daily ceiling on paid requests across all copies: past it the address is simply
		// left off until tomorrow, never bought.
		if g.dailyCap > 0 {
			if n, err := g.store.CountCall(ctx); err == nil && n > g.dailyCap {
				if n == g.dailyCap+1 {
					log.Printf("[geocode] daily cap of %d Google requests reached: no more addresses today", g.dailyCap)
				}
				return "", nil
			}
		}
	}

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
		err = withoutKey(err)
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

	ttl := addrCacheTTL
	if addr == "" {
		// Don't cache an empty answer for long — a transient error or an
		// out-of-coverage point may resolve later once the device moves.
		ttl = addrEmptyTTL
	} else if g.store != nil {
		_ = g.store.PutAddr(ctx, key, addr)
	}
	g.remember(key, addr, ttl)

	return addr, nil
}

// Place is one forward-geocoding answer: a formatted address and its point.
type Place struct {
	Address string  `json:"address"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

type forwardResponse struct {
	Status  string `json:"status"`
	Results []struct {
		FormattedAddress string `json:"formatted_address"`
		Geometry         struct {
			Location struct {
				Lat float64 `json:"lat"`
				Lng float64 `json:"lng"`
			} `json:"location"`
		} `json:"geometry"`
	} `json:"results"`
	ErrorMessage string `json:"error_message"`
}

// Forward looks up an address or place name ("103 E Fremont Ave, Sunnyvale") and returns
// up to five matches, for the venue page's Location card. Answers are cached for a day
// by the exact query, since people retype the same search.
func (g *Geocoder) Forward(ctx context.Context, query string) ([]Place, error) {
	key := "q:" + query
	g.mu.RLock()
	if c, ok := g.places[key]; ok && time.Now().Before(c.expires) {
		g.mu.RUnlock()
		g.meter.MarkHit()
		return c.places, nil
	}
	g.mu.RUnlock()

	endpoint := googleGeocodeURL + "?address=" + url.QueryEscape(query) + "&key=" + url.QueryEscape(g.apiKey)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		err = withoutKey(err)
		g.meter.MarkError(err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		gerr := fmt.Errorf("google geocode returned %d", resp.StatusCode)
		g.meter.MarkError(gerr)
		return nil, gerr
	}
	var fr forwardResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		g.meter.MarkError(err)
		return nil, err
	}
	switch fr.Status {
	case "OK", "ZERO_RESULTS":
		g.meter.MarkSuccess()
	default:
		log.Printf("[geocode] forward status=%s: %s", fr.Status, fr.ErrorMessage)
		gerr := fmt.Errorf("geocode status %s", fr.Status)
		g.meter.MarkError(gerr)
		return nil, gerr
	}
	var out []Place
	for _, r := range fr.Results {
		out = append(out, Place{Address: r.FormattedAddress, Lat: r.Geometry.Location.Lat, Lon: r.Geometry.Location.Lng})
		if len(out) == 5 {
			break
		}
	}
	g.mu.Lock()
	if g.places == nil || len(g.places) > 500 {
		g.places = map[string]cachedPlaces{}
	}
	g.places[key] = cachedPlaces{places: out, expires: time.Now().Add(24 * time.Hour)}
	g.mu.Unlock()
	return out, nil
}

type cachedPlaces struct {
	places  []Place
	expires time.Time
}

// remember keeps an address in the in-memory cache.
func (g *Geocoder) remember(key, addr string, ttl time.Duration) {
	g.mu.Lock()
	g.cache[key] = cachedAddress{Address: addr, ExpiresAt: time.Now().Add(ttl)}
	if len(g.cache) > addrCacheMaxMem {
		for k := range g.cache {
			delete(g.cache, k)
			break
		}
	}
	g.mu.Unlock()
}
