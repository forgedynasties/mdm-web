package geolocate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// WifiAP represents a single WiFi access point for geolocation lookup.
type WifiAP struct {
	BSSID string `json:"bssid"`
	SSID  string `json:"ssid,omitempty"`
	RSSI  int    `json:"rssi"`
}

type cachedLocation struct {
	Lat       float64
	Lon       float64
	Accuracy  float64
	ExpiresAt time.Time
}

// Resolver resolves WiFi AP data to geographic coordinates using the Google
// Maps Geolocation API. Safe for concurrent use.
type Resolver struct {
	apiKey   string
	cache    map[string]cachedLocation
	mu       sync.RWMutex
	client   *http.Client
	lastCall time.Time
	callMu   sync.Mutex
	meter    *Meter
	store    LocationStore
}

// APLocation is a learned WiFi access point → geographic point mapping.
type APLocation struct {
	BSSID    string
	Lat      float64
	Lon      float64
	Accuracy float64
}

// LocationStore is the persistence for the learned WiFi-AP index. It lets the
// resolver locate a device from a scan that overlaps previously-seen APs without
// calling Google. Implemented by the db layer (adapted in main); may be nil, in
// which case the resolver falls back to the in-memory cache + Google only.
type LocationStore interface {
	// LookupAPs returns learned locations for the given BSSIDs seen at/after fresherThan.
	LookupAPs(ctx context.Context, bssids []string, fresherThan time.Time) ([]APLocation, error)
	// LearnAPs records the resolved point for a scan's BSSIDs (upsert, refresh seen_at).
	LearnAPs(ctx context.Context, bssids []string, lat, lon, accuracy float64) error
	// BumpAPHits increments the hit counter for APs that just served a local lookup.
	BumpAPHits(ctx context.Context, bssids []string) error
}

// Tunables for the learned index.
const (
	// LearnedFreshWindow bounds how old a learned AP may be before it is ignored
	// (APs get moved/replaced). Refreshed whenever Google reconfirms the AP.
	LearnedFreshWindow = 14 * 24 * time.Hour
	// minKnownAPs / minOverlapRatio gate a local estimate: enough of the scan's APs
	// must be known, both in absolute count and as a fraction, to trust the result
	// (guards against a moved device that shares a few APs with an old location).
	minKnownAPs     = 3
	minOverlapRatio = 0.5
	// localEstAccuracyFloor inflates the reported accuracy of a locally-estimated
	// point — a centroid is coarser than a Google fix.
	localEstAccuracyFloor = 50.0
)

// SetStore attaches a learned-index store (called from main after construction).
func (r *Resolver) SetStore(s LocationStore) { r.store = s }

// Stats returns a snapshot of this resolver's Google Geolocation API usage.
func (r *Resolver) Stats() MeterSnapshot { return r.meter.Snapshot() }

// bssidList returns the BSSIDs of a scan (unfiltered).
func bssidList(aps []WifiAP) []string {
	out := make([]string, 0, len(aps))
	for _, ap := range aps {
		if ap.BSSID != "" {
			out = append(out, ap.BSSID)
		}
	}
	return out
}

// estimateFromAPs computes an RSSI-weighted centroid of the scan's APs that are
// present in the learned set. Returns ok=false unless the overlap clears both the
// absolute and fractional thresholds. Also returns the matched BSSIDs (for hit-bumping).
func estimateFromAPs(aps []WifiAP, known []APLocation) (lat, lon, acc float64, matched []string, ok bool) {
	byB := make(map[string]APLocation, len(known))
	for _, k := range known {
		byB[strings.ToUpper(k.BSSID)] = k
	}
	var sumW, sumLat, sumLon, maxAcc float64
	for _, ap := range aps {
		k, found := byB[strings.ToUpper(ap.BSSID)]
		if !found {
			continue
		}
		// RSSI (dBm, negative) → weight: stronger AP counts more. -40→60, -85→15.
		w := float64(ap.RSSI) + 100
		if w < 1 {
			w = 1
		}
		sumW += w
		sumLat += w * k.Lat
		sumLon += w * k.Lon
		if k.Accuracy > maxAcc {
			maxAcc = k.Accuracy
		}
		matched = append(matched, ap.BSSID)
	}
	n := len(matched)
	if n < minKnownAPs || sumW == 0 {
		return 0, 0, 0, nil, false
	}
	if len(aps) == 0 || float64(n)/float64(len(aps)) < minOverlapRatio {
		return 0, 0, 0, nil, false
	}
	acc = maxAcc
	if acc < localEstAccuracyFloor {
		acc = localEstAccuracyFloor
	}
	return sumLat / sumW, sumLon / sumW, acc, matched, true
}

// storeInCache writes a resolved/estimated point to the in-memory exact-scan cache.
func (r *Resolver) storeInCache(key string, lat, lon, accuracy float64) {
	r.mu.Lock()
	r.cache[key] = cachedLocation{Lat: lat, Lon: lon, Accuracy: accuracy, ExpiresAt: time.Now().Add(cacheTTL)}
	if len(r.cache) > 2000 {
		for k := range r.cache {
			delete(r.cache, k)
			break
		}
	}
	r.mu.Unlock()
}

// New creates a Resolver bound to a Google Geolocation API key, with a 15s
// timeout and an in-memory cache (5 min TTL). apiKey must be non-empty.
func New(apiKey string) *Resolver {
	return &Resolver{
		apiKey: apiKey,
		cache:  make(map[string]cachedLocation),
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		meter: newMeter(),
	}
}

// ErrCooldown is returned when Resolve is called within the cooldown period.
var ErrCooldown = fmt.Errorf("geolocation cooldown")

// cacheTTL is how long an exact-scan result stays in the in-memory cache. Bumped
// from 5m to 1h: kiosks are stationary, so re-resolving the identical scan every
// few minutes just burned Google calls. The learned index handles partial/overlap
// matches and cross-device sharing on top of this.
const cacheTTL = time.Hour

// Resolve resolves a set of WiFi APs to a latitude/longitude using the Google
// Geolocation API. Returns zero values and an error on failure. Results are
// cached for 5 minutes keyed by all BSSIDs (sorted alphabetically, stable
// against RSSI drift).
func (r *Resolver) Resolve(ctx context.Context, aps []WifiAP) (lat, lon, accuracy float64, err error) {
	if len(aps) == 0 {
		return 0, 0, 0, fmt.Errorf("no access points provided")
	}

	// Global cooldown — don't call the API more than once per 10s regardless of
	// whether the AP set changed. RSSI drift can cause cache-key churn otherwise.
	key := cacheKey(aps)

	// 1. In-memory exact-scan cache — fastest, no I/O. Checked before the cooldown
	//    so a repeat scan is always served instantly even during the cooldown window.
	r.mu.RLock()
	if c, ok := r.cache[key]; ok && time.Now().Before(c.ExpiresAt) {
		r.mu.RUnlock()
		r.meter.MarkHit()
		return c.Lat, c.Lon, c.Accuracy, nil
	}
	r.mu.RUnlock()

	// 2. Learned WiFi-AP index — locate from previously-seen APs without calling
	//    Google. This is the main cost saver: after a location's APs are learned
	//    once, every overlapping scan (same or nearby device) resolves locally.
	if r.store != nil {
		if known, e := r.store.LookupAPs(ctx, bssidList(aps), time.Now().Add(-LearnedFreshWindow)); e == nil && len(known) > 0 {
			if elat, elon, eacc, matched, ok := estimateFromAPs(aps, known); ok {
				r.meter.MarkLocalHit()
				r.storeInCache(key, elat, elon, eacc)
				if err := r.store.BumpAPHits(ctx, matched); err != nil {
					log.Printf("[geolocate] bump hits: %v", err)
				}
				return elat, elon, eacc, nil
			}
		} else if e != nil {
			log.Printf("[geolocate] learned-index lookup: %v", e)
		}
	}

	// 3. Global cooldown — only gates real Google calls now (cache/index hits above
	//    are already served). Don't call the API more than once per 10s server-wide.
	r.callMu.Lock()
	if elapsed := time.Since(r.lastCall); elapsed < 10*time.Second {
		r.callMu.Unlock()
		r.meter.MarkCooldown()
		return 0, 0, 0, ErrCooldown
	}
	r.lastCall = time.Now()
	r.callMu.Unlock()

	// 4. Google Geolocation API.
	lat, lon, accuracy, err = r.query(ctx, aps)
	if err != nil {
		r.meter.MarkError(err)
		return 0, 0, 0, err
	}
	r.meter.MarkSuccess()
	r.storeInCache(key, lat, lon, accuracy)

	// 5. Learn: stamp every AP in this scan with the resolved point so future
	//    overlapping scans resolve locally.
	if r.store != nil {
		if err := r.store.LearnAPs(ctx, bssidList(aps), lat, lon, accuracy); err != nil {
			log.Printf("[geolocate] learn APs: %v", err)
		}
	}

	return lat, lon, accuracy, nil
}

func cacheKey(aps []WifiAP) string {
	if len(aps) == 0 {
		return ""
	}
	// Use BSSIDs only, sorted alphabetically — stable against RSSI drift.
	ids := make([]string, len(aps))
	for i, ap := range aps {
		ids[i] = ap.BSSID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// googleRequest matches the Google Geolocation API request body. considerIp is
// forced false so a kiosk with no GPS is located from its WiFi neighbourhood
// only — never from the (often wildly wrong) egress IP.
type googleRequest struct {
	ConsiderIP       bool             `json:"considerIp"`
	WifiAccessPoints []googleWifiAP   `json:"wifiAccessPoints"`
}

type googleWifiAP struct {
	MacAddress     string `json:"macAddress"`
	SignalStrength int    `json:"signalStrength,omitempty"`
}

// googleResponse matches the Google Geolocation API success body.
type googleResponse struct {
	Location struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"location"`
	Accuracy float64 `json:"accuracy"`
}

const googleGeolocateURL = "https://www.googleapis.com/geolocation/v1/geolocate"

// validMAC reports whether s is a well-formed colon-separated MAC address
// (six hex octets). The API rejects the entire request with a deserialize
// error if any macAddress is empty or malformed, so we drop bad entries —
// Android wifi scans occasionally return results with blank BSSIDs.
func validMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i := 0; i < 17; i++ {
		c := s[i]
		if i%3 == 2 {
			if c != ':' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func (r *Resolver) query(ctx context.Context, aps []WifiAP) (float64, float64, float64, error) {
	reqBody := googleRequest{ConsiderIP: false}
	for _, ap := range aps {
		if !validMAC(ap.BSSID) {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(ap.BSSID), "02:00:00") {
			continue
		}
		reqBody.WifiAccessPoints = append(reqBody.WifiAccessPoints, googleWifiAP{
			MacAddress:     ap.BSSID,
			SignalStrength: ap.RSSI,
		})
	}
	if len(reqBody.WifiAccessPoints) == 0 {
		return 0, 0, 0, fmt.Errorf("no valid access points after filtering")
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, 0, 0, err
	}

	endpoint := googleGeolocateURL + "?key=" + url.QueryEscape(r.apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Never log the request body — it does not carry the key, but the URL does;
		// keep the key out of logs entirely by logging only status + response.
		log.Printf("[geolocate] google %d — response: %s", resp.StatusCode, string(respBody))
		return 0, 0, 0, fmt.Errorf("google geolocation returned %d", resp.StatusCode)
	}

	var gResp googleResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return 0, 0, 0, err
	}

	if gResp.Location.Lat == 0 && gResp.Location.Lng == 0 {
		return 0, 0, 0, fmt.Errorf("google geolocation returned no location")
	}

	return gResp.Location.Lat, gResp.Location.Lng, gResp.Accuracy, nil
}

// ExtractWifiScan parses the "wifi_scan" array from a raw extra JSONB payload
// into a slice of WifiAP suitable for Resolve. Returns nil if the key is absent
// or unparseable.
func ExtractWifiScan(raw json.RawMessage) []WifiAP {
	if len(raw) == 0 {
		return nil
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return nil
	}
	scanRaw, ok := extra["wifi_scan"]
	if !ok {
		return nil
	}
	var scan []struct {
		BSSID string `json:"bssid"`
		SSID  string `json:"ssid"`
		RSSI  int    `json:"rssi"`
	}
	if err := json.Unmarshal(scanRaw, &scan); err != nil {
		log.Printf("[geolocate] failed to parse wifi_scan: %v", err)
		return nil
	}
	aps := make([]WifiAP, len(scan))
	for i, s := range scan {
		aps[i] = WifiAP{BSSID: s.BSSID, SSID: s.SSID, RSSI: s.RSSI}
	}
	return aps
}
