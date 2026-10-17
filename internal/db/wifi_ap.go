package db

import (
	"context"
	"time"
)

// ── Learned WiFi AP → location index ────────────────────────────────────────

// WifiAPLoc is one learned access point: its BSSID and the geographic point it
// was last resolved to (via Google), with how many local lookups it has served.
type WifiAPLoc struct {
	BSSID    string    `json:"bssid"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Accuracy float64   `json:"accuracy"`
	Hits     int64     `json:"hits"`
	SeenAt   time.Time `json:"seen_at"`
}

// WifiAPStats summarizes the learned index for the admin panel.
type WifiAPStats struct {
	Total            int64     `json:"total"`             // learned APs (all)
	Fresh            int64     `json:"fresh"`             // APs within the freshness window
	DistinctPlaces   int64     `json:"distinct_places"`   // ~unique points (lat/lon rounded)
	ServedLookups    int64     `json:"served_lookups"`    // SUM(hits) — local lookups served
	LastLearnedAt    time.Time `json:"last_learned_at"`
}

// LookupWifiAPs returns learned locations for the given BSSIDs that are still
// fresh (seen_at >= fresherThan). Used to locate a device from a WiFi scan
// without calling Google.
func (d *DB) LookupWifiAPs(ctx context.Context, bssids []string, fresherThan time.Time) ([]WifiAPLoc, error) {
	if len(bssids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT bssid, lat, lon, accuracy, hits, seen_at
		FROM wifi_ap_locations
		WHERE bssid = ANY($1) AND seen_at >= $2`, bssids, fresherThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WifiAPLoc
	for rows.Next() {
		var a WifiAPLoc
		if err := rows.Scan(&a.BSSID, &a.Lat, &a.Lon, &a.Accuracy, &a.Hits, &a.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LearnWifiAPs records/refreshes the resolved location for a set of BSSIDs (the
// scan Google just resolved). Upsert: existing APs are re-pointed and their
// seen_at refreshed; hits is preserved.
func (d *DB) LearnWifiAPs(ctx context.Context, bssids []string, lat, lon, accuracy float64) error {
	if len(bssids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO wifi_ap_locations (bssid, lat, lon, accuracy, seen_at)
		SELECT unnest($1::text[]), $2, $3, $4, NOW()
		ON CONFLICT (bssid) DO UPDATE SET
			lat = EXCLUDED.lat, lon = EXCLUDED.lon,
			accuracy = EXCLUDED.accuracy, seen_at = NOW()`,
		bssids, lat, lon, accuracy)
	return err
}

// BumpWifiAPHits increments the hit counter for the APs that just served a local
// lookup (best-effort — a failure here must not fail the lookup).
func (d *DB) BumpWifiAPHits(ctx context.Context, bssids []string) error {
	if len(bssids) == 0 {
		return nil
	}
	// Locks are taken in BSSID order, deliberately. A plain multi-row UPDATE acquires
	// its row locks in whatever order it happens to reach the rows, so two of these with
	// overlapping BSSIDs can each hold what the other wants. That is not theoretical:
	// three deadlocked against each other on live during a restart, when every device
	// reconnects and submits its Wi-Fi scan at once.
	//
	// Sorting the array is not on its own enough — nothing promises an UPDATE visits
	// rows in the order of a subquery. The rows are therefore locked explicitly, by an
	// ORDER BY ... FOR UPDATE that completes before the write, which is what actually
	// imposes the total order and makes a lock cycle impossible.
	_, err := d.pool.Exec(ctx, `
		WITH locked AS (
			SELECT bssid FROM wifi_ap_locations
			WHERE bssid = ANY($1) ORDER BY bssid FOR UPDATE
		)
		UPDATE wifi_ap_locations w SET hits = hits + 1
		FROM locked l WHERE w.bssid = l.bssid`, bssids)
	return err
}

// GetWifiAPStats summarizes the learned index. freshWindow bounds the "fresh" count.
func (d *DB) GetWifiAPStats(ctx context.Context, freshWindow time.Duration) (WifiAPStats, error) {
	var s WifiAPStats
	cutoff := time.Now().Add(-freshWindow)
	err := d.pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE seen_at >= $1),
			count(DISTINCT (round(lat::numeric, 4)::text || ',' || round(lon::numeric, 4)::text)),
			COALESCE(SUM(hits), 0),
			COALESCE(MAX(seen_at), 'epoch'::timestamptz)
		FROM wifi_ap_locations`, cutoff).
		Scan(&s.Total, &s.Fresh, &s.DistinctPlaces, &s.ServedLookups, &s.LastLearnedAt)
	return s, err
}

// ListWifiAPsRecent returns the most-recently-learned APs (for the admin panel table).
func (d *DB) ListWifiAPsRecent(ctx context.Context, limit int) ([]WifiAPLoc, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT bssid, lat, lon, accuracy, hits, seen_at
		FROM wifi_ap_locations
		ORDER BY seen_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WifiAPLoc
	for rows.Next() {
		var a WifiAPLoc
		if err := rows.Scan(&a.BSSID, &a.Lat, &a.Lon, &a.Accuracy, &a.Hits, &a.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
