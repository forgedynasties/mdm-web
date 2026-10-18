package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/geolocate"
)

// Offline check-ins (1 Oct; plan: static/offline-queue-plan.html). A client (1.7.0+)
// that couldn't reach the MDM kept a reading every few minutes; this takes them when it
// can. They go into the history as late readings: no alerts, no change to last-seen.

const (
	backfillMaxBatch = 500
	backfillMaxAge   = 8 * 24 * time.Hour
)

type backfillReading struct {
	AtMs          int64    `json:"at_ms"`      // the device's wall clock when read
	ElapsedMs     int64    `json:"elapsed_ms"` // time since boot when read
	BootID        string   `json:"boot_id"`
	BatteryPct    *int16   `json:"battery_pct"`
	TempC         *float64 `json:"temp_c"`
	WifiRSSI      *int16   `json:"wifi_rssi"`
	RAMUsedMB     *int32   `json:"ram_used_mb"`
	RAMTotalMB    *int32   `json:"ram_total_mb"`
	StorageFreeGB *float64 `json:"storage_free_gb"`
	// 1.7.1+: the states the charge strip draws, as the check-in reports them, and the
	// crashes recorded since the previous kept reading.
	Charging  json.RawMessage `json:"charging"`
	WlcStatus json.RawMessage `json:"wlc_status"`
	// Lost-device reporting (firmware client 1.13.0): the strongest Wi-Fi APs it could see,
	// when they changed since the previous kept reading (C), and lost mode as it stood —
	// a self-lock (D) happens while out of reach, so it arrives here.
	WifiScan json.RawMessage `json:"wifi_scan"`
	LostMode *struct {
		On      bool  `json:"on"`
		Self    bool  `json:"self"`
		SinceMs int64 `json:"since_ms"`
	} `json:"lost_mode"`
	Crashes []struct {
		Kind    string `json:"kind"`
		TimeMs  int64  `json:"time_ms"`
		Summary string `json:"summary"`
		Trace   string `json:"trace"`
	} `json:"crashes"`
}

// stateText renders a reported state value the way the event table stores it
// ("true", "2"), matching the live check-in path; absent is "".
func stateText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// placeReading turns a reading's device times into server time. In the boot it is being
// sent from, time-since-boot is exact whatever the device's clock says; from an earlier
// boot only the wall clock is left, corrected by how far the device's clock is off now,
// and a clock that can't be right (before 2025: the RTC was reset) loses the reading.
func placeReading(rd backfillReading, serverNow time.Time, devNowMs, devNowElapsed int64, bootID string) (time.Time, bool) {
	var at time.Time
	switch {
	case bootID != "" && rd.BootID == bootID && devNowElapsed > 0 && rd.ElapsedMs > 0 && rd.ElapsedMs <= devNowElapsed:
		at = serverNow.Add(-time.Duration(devNowElapsed-rd.ElapsedMs) * time.Millisecond)
	case rd.AtMs > time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() && devNowMs > 0:
		skew := serverNow.UnixMilli() - devNowMs
		at = time.UnixMilli(rd.AtMs + skew)
	default:
		return time.Time{}, false
	}
	if at.After(serverNow) || at.Before(serverNow.Add(-backfillMaxAge)) {
		return time.Time{}, false
	}
	return at.Truncate(time.Millisecond), true
}

// backfillRequest is the body of POST /api/v1/checkins/backfill.
type backfillRequest struct {
	Serial       string            `json:"serial"`
	NowMs        int64             `json:"now_ms"`
	NowElapsedMs int64             `json:"now_elapsed_ms"`
	BootID       string            `json:"boot_id"`
	Dropped      int               `json:"dropped"` // readings the device threw away (its queue was full)
	Readings     []backfillReading `json:"readings"`
}

// skewMs is how far the device's clock is behind ours (0 when it did not say).
func (r backfillRequest) skewMs(now time.Time) int64 {
	if r.NowMs <= 0 {
		return 0
	}
	return now.UnixMilli() - r.NowMs
}

// backfillMaxScans caps the Wi-Fi scans one batch adds to a device's trail: the client
// keeps one only when its surroundings changed, so a real trip is far below it.
const backfillMaxScans = 200

// trailScan returns a reading's wifi_scan when it is a non-empty list of APs with a BSSID
// each, trimmed to the 8 strongest the protocol allows; nil otherwise.
func trailScan(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var aps []geolocate.WifiAP
	if json.Unmarshal(raw, &aps) != nil {
		return nil
	}
	out := aps[:0]
	for _, a := range aps {
		if a.BSSID != "" && len(out) < 8 {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

// Backfill is POST /api/v1/checkins/backfill.
func (h *Handler) Backfill(w http.ResponseWriter, r *http.Request) {
	var req backfillRequest
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.Serial = strings.TrimSpace(req.Serial)
	if !validSerial(req.Serial) || len(req.Serial) > maxSerialLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if len(req.Readings) > backfillMaxBatch {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "at most 500 readings per request"})
		return
	}
	if h.deviceRateLimited(w, req.Serial) {
		return
	}
	if !h.requireDeviceIdentity(w, r, req.Serial) {
		return
	}
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, req.Serial)
	if err != nil || dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device: check in first"})
		return
	}
	now := time.Now()
	ss := make([]db.LateSample, 0, len(req.Readings))
	var charging, wlc []db.LateState
	var crashes []db.LateCrash
	var scans []db.TrailScan
	var selfLockAt time.Time
	skipped := 0
	for _, rd := range req.Readings {
		// Crashes carry the device's own time (as live reports do), so they don't
		// depend on placing the reading.
		for _, c := range rd.Crashes {
			crashes = append(crashes, db.LateCrash{Kind: c.Kind, TimeMs: c.TimeMs, Summary: c.Summary, Trace: c.Trace})
		}
		at, ok := placeReading(rd, now, req.NowMs, req.NowElapsedMs, req.BootID)
		if !ok {
			skipped++
			continue
		}
		if v := stateText(rd.Charging); v != "" {
			charging = append(charging, db.LateState{At: at, Val: v})
		}
		if v := stateText(rd.WlcStatus); v != "" {
			wlc = append(wlc, db.LateState{At: at, Val: v})
		}
		if scan := trailScan(rd.WifiScan); scan != nil && len(scans) < backfillMaxScans {
			scans = append(scans, db.TrailScan{At: at, Scan: scan})
		}
		if rd.LostMode != nil && rd.LostMode.On && rd.LostMode.Self && selfLockAt.IsZero() {
			selfLockAt = at
			if rd.LostMode.SinceMs > 0 {
				if t := time.UnixMilli(rd.LostMode.SinceMs + req.skewMs(now)); !t.After(at) {
					selfLockAt = t
				}
			}
		}
		ss = append(ss, db.LateSample{At: at, BatteryPct: rd.BatteryPct, TempC: rd.TempC, WifiRSSI: rd.WifiRSSI,
			RAMUsedMB: rd.RAMUsedMB, RAMTotalMB: rd.RAMTotalMB, StorageFreeGB: rd.StorageFreeGB})
	}
	stored, err := h.db.InsertLateSamples(ctx, dev.ID, ss)
	if err != nil {
		log.Printf("[backfill] %s: %v", req.Serial, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.db.InsertLateStates(ctx, dev.ID, "charging", charging); err != nil {
		log.Printf("[backfill] %s charging: %v", req.Serial, err)
	}
	if err := h.db.InsertLateStates(ctx, dev.ID, "wlc_status", wlc); err != nil {
		log.Printf("[backfill] %s wlc_status: %v", req.Serial, err)
	}
	lateCrashes, err := h.db.InsertLateCrashes(ctx, dev.ID, dev.BuildID, crashes)
	if err != nil {
		log.Printf("[backfill] %s crashes: %v", req.Serial, err)
	}
	if len(scans) > 0 {
		if n, err := h.db.InsertTrailScans(ctx, dev.ID, scans); err != nil {
			log.Printf("[backfill] %s wifi scans: %v", req.Serial, err)
		} else if n > 0 {
			log.Printf("[backfill] %s kept %d Wi-Fi scan(s) for its trail", req.Serial, n)
		}
	}
	if !selfLockAt.IsZero() {
		h.db.RecordSelfLock(ctx, dev.ID, selfLockAt)
	}
	if req.Dropped > 0 {
		h.db.RecordDeviceEvent(ctx, dev.ID, "offline_dropped", "Offline too long: the device's queue was full and it dropped readings")
	}
	log.Printf("[backfill] %s sent %d readings from while offline: %d stored, %d already known, %d with unusable times, %d dropped on the device, %d crash(es) added",
		req.Serial, len(req.Readings), stored, len(ss)-stored, skipped, req.Dropped, lateCrashes)
	// Every reading in the batch is answered (stored, already known, or unusable), so the
	// device can delete them all.
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(req.Readings), "stored": stored})
}
