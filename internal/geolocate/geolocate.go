package geolocate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	cache  map[string]cachedLocation
	mu     sync.RWMutex
	client *http.Client
}

// New creates a Resolver with a 5s timeout and an in-memory cache (5 min TTL).
func New() *Resolver {
	return &Resolver{
		cache: make(map[string]cachedLocation),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Resolve resolves a set of WiFi APs to a latitude/longitude using BeaconDB.
// Returns zero values and an error on failure. Results are cached for 5
// minutes keyed by the top-3 strongest BSSIDs.
func (r *Resolver) Resolve(ctx context.Context, aps []WifiAP) (lat, lon, accuracy float64, err error) {
	if len(aps) == 0 {
		return 0, 0, 0, fmt.Errorf("no access points provided")
	}

	key := cacheKey(aps)

	r.mu.RLock()
	if c, ok := r.cache[key]; ok && time.Now().Before(c.ExpiresAt) {
		r.mu.RUnlock()
		return c.Lat, c.Lon, c.Accuracy, nil
	}
	r.mu.RUnlock()

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
	sorted := make([]WifiAP, len(aps))
	copy(sorted, aps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RSSI > sorted[j].RSSI })
	n := 3
	if len(sorted) < n {
		n = len(sorted)
	}
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, sorted[i].BSSID)
	}
	return strings.Join(parts, ",")
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

func (r *Resolver) query(ctx context.Context, aps []WifiAP) (float64, float64, float64, error) {
	reqBody := beaconDBRequest{}
	for _, ap := range aps {
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
		return 0, 0, 0, fmt.Errorf("beacondb returned %d", resp.StatusCode)
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
