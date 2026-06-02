package geolocate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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

// Resolver resolves WiFi AP data to geographic coordinates using BeaconDB.
// Safe for concurrent use.
type Resolver struct {
	cache    map[string]cachedLocation
	mu       sync.RWMutex
	client   *http.Client
	lastCall time.Time
	callMu   sync.Mutex
}

// New creates a Resolver with a 5s timeout and an in-memory cache (5 min TTL).
func New() *Resolver {
	return &Resolver{
		cache: make(map[string]cachedLocation),
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ErrCooldown is returned when Resolve is called within the cooldown period.
var ErrCooldown = fmt.Errorf("beacondb cooldown")

// Resolve resolves a set of WiFi APs to a latitude/longitude using BeaconDB.
// Returns zero values and an error on failure. Results are cached for 5
// minutes keyed by all BSSIDs (sorted alphabetically, stable against RSSI drift).
func (r *Resolver) Resolve(ctx context.Context, aps []WifiAP) (lat, lon, accuracy float64, err error) {
	if len(aps) == 0 {
		return 0, 0, 0, fmt.Errorf("no access points provided")
	}

	// Global cooldown — don't call BeaconDB more than once per 10s regardless of
	// whether the AP set changed. RSSI drift can cause cache-key churn otherwise.
	r.callMu.Lock()
	if elapsed := time.Since(r.lastCall); elapsed < 10*time.Second {
		r.callMu.Unlock()
		return 0, 0, 0, ErrCooldown
	}
	r.callMu.Unlock()

	key := cacheKey(aps)

	r.mu.RLock()
	if c, ok := r.cache[key]; ok && time.Now().Before(c.ExpiresAt) {
		r.mu.RUnlock()
		return c.Lat, c.Lon, c.Accuracy, nil
	}
	r.mu.RUnlock()

	// Mark call time right before the HTTP call so concurrent goroutines wait.
	r.callMu.Lock()
	r.lastCall = time.Now()
	r.callMu.Unlock()

	lat, lon, accuracy, err = r.query(ctx, aps)
	if err != nil {
		return 0, 0, 0, err
	}

	r.mu.Lock()
	r.cache[key] = cachedLocation{
		Lat: lat, Lon: lon, Accuracy: accuracy,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	// Keep cache bounded
	if len(r.cache) > 2000 {
		for k := range r.cache {
			delete(r.cache, k)
			break
		}
	}
	r.mu.Unlock()

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

type beaconDBRequest struct {
	WifiAccessPoints []beaconDBWifiAP `json:"wifiAccessPoints"`
}

type beaconDBWifiAP struct {
	MacAddress     string `json:"macAddress"`
	SignalStrength int    `json:"signalStrength,omitempty"`
}

type beaconDBResponse struct {
	Location struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"location"`
	Accuracy float64 `json:"accuracy"`
}

const beaconDBURL = "https://beacondb.net/v1/geolocate"

// validMAC reports whether s is a well-formed colon-separated MAC address
// (six hex octets). BeaconDB rejects the entire request with a deserialize
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
	reqBody := beaconDBRequest{}
	for _, ap := range aps {
		if !validMAC(ap.BSSID) {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(ap.BSSID), "02:00:00") {
			continue
		}
		reqBody.WifiAccessPoints = append(reqBody.WifiAccessPoints, beaconDBWifiAP{
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

	req, err := http.NewRequestWithContext(ctx, "POST", beaconDBURL, bytes.NewReader(body))
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
		log.Printf("[geolocate] beaconDB %d — request: %s — response: %s", resp.StatusCode, string(body), string(respBody))
		return 0, 0, 0, fmt.Errorf("beacondb returned %d: %s", resp.StatusCode, string(respBody))
	}

	var bdbResp beaconDBResponse
	if err := json.NewDecoder(resp.Body).Decode(&bdbResp); err != nil {
		return 0, 0, 0, err
	}

	if bdbResp.Location.Lat == 0 && bdbResp.Location.Lng == 0 {
		return 0, 0, 0, fmt.Errorf("beacondb returned no location")
	}

	return bdbResp.Location.Lat, bdbResp.Location.Lng, bdbResp.Accuracy, nil
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
