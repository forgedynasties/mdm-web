package dashboard

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// DeviceChartData returns a device's battery / temperature / RAM series for an
// on-demand window [from,until] (epoch-ms), so the chart's custom-range and 7d/30d
// controls can pull OLDER history instead of only filtering the ~48h baked into the
// page. Older builds' data was always in the checkins table (battery + temp are
// recorded for every build) — it just was never fetched, so widening the range showed
// nothing before the initial window. Down-sampled to keep the payload small.
// ── Chart window cache ────────────────────────────────────────────────────────
// A 48h pull is thousands of rows scattered across a multi-GB checkins table, so
// it is dominated by random heap reads (measured: 2.4s cold for 4.4k rows, ~0.3s
// warm). The same window is asked for repeatedly — the page's background warm-up,
// then a range click, then anyone else opening the device — so memoise the encoded
// response briefly. Windows are keyed on their endpoints rounded to 30s so the
// warm-up and the click that follows it share one entry.
type chartCacheEntry struct {
	body []byte
	at   time.Time
}

var (
	chartCacheMu sync.Mutex
	chartCache   = map[string]chartCacheEntry{}
)

const (
	chartCacheTTL     = 45 * time.Second
	chartCacheMaxSize = 400
)

func chartCacheKey(deviceID uuid.UUID, fromMs, untilMs int64) string {
	const round = 30_000
	return fmt.Sprintf("%s|%d|%d", deviceID, fromMs/round, untilMs/round)
}

func chartCacheGet(key string) ([]byte, bool) {
	chartCacheMu.Lock()
	defer chartCacheMu.Unlock()
	e, ok := chartCache[key]
	if !ok || time.Since(e.at) > chartCacheTTL {
		return nil, false
	}
	return e.body, true
}

func chartCachePut(key string, body []byte) {
	chartCacheMu.Lock()
	defer chartCacheMu.Unlock()
	if len(chartCache) >= chartCacheMaxSize {
		for k, e := range chartCache { // drop expired first, else start fresh
			if time.Since(e.at) > chartCacheTTL {
				delete(chartCache, k)
			}
		}
		if len(chartCache) >= chartCacheMaxSize {
			chartCache = map[string]chartCacheEntry{}
		}
	}
	chartCache[key] = chartCacheEntry{body: body, at: time.Now()}
}

// batteryAnchorShaped fetches the last battery reading before the chart window.
// Best-effort: a chart with no anchor is still correct, it just cannot see a cycle that
// armed off the left edge. Always from device_samples, whichever table answers the chart:
// samples are backfilled to the first check-in (2026-03-18), so they hold every battery
// reading the check-in history does.
func (h *Handler) batteryAnchorShaped(ctx context.Context, deviceID uuid.UUID, from time.Time) *batteryAnchor {
	at, pct, ok, err := h.db.LastBatteryMarkBefore(ctx, deviceID, from)
	if err != nil || !ok {
		return nil
	}
	return &batteryAnchor{X: at.UnixMilli(), Y: pct}
}

func (h *Handler) DeviceChartData(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	fromMs, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("from")), 10, 64)
	untilMs, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("until")), 10, 64)
	if untilMs <= 0 {
		untilMs = time.Now().UnixMilli()
	}
	if fromMs <= 0 || fromMs >= untilMs {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	ckey := chartCacheKey(device.ID, fromMs, untilMs)
	if body, ok := chartCacheGet(ckey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return
	}
	// The shaped tables: a wide window is fixed-width rows on a clustered index.
	if pts, ok := h.shapedChartSeries(r.Context(), device.ID, time.UnixMilli(fromMs), time.UnixMilli(untilMs)); ok {
		body, err := buildChartBody(device, pts, h.batteryAnchorShaped(r.Context(), device.ID, time.UnixMilli(fromMs)))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		chartCachePut(ckey, body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return
	}

	// Nothing in the window (the shaped tables cover every device's whole life, so this
	// is a window with no samples in it): an empty chart, not a fallback.
	body, err := json.Marshal(map[string]any{"battery": []any{}, "temp": []any{}, "ram": []any{}, "charge": []any{},
		"battery_before": h.batteryAnchorShaped(r.Context(), device.ID, time.UnixMilli(fromMs))})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	chartCachePut(ckey, body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// decimateExtremes thins a time-ordered series to about maxPoints by splitting it
// into buckets of equal size and keeping each bucket's first point plus its minimum
// and maximum (in time order), so peaks and dips survive the thinning.
func decimateExtremes[T any](pts []T, maxPoints int, y func(T) float64) []T {
	n := len(pts)
	if n <= maxPoints || maxPoints < 3 {
		return pts
	}
	bucket := (n + maxPoints/3 - 1) / (maxPoints / 3)
	out := make([]T, 0, maxPoints+3)
	for i := 0; i < n; i += bucket {
		j := i + bucket
		if j > n {
			j = n
		}
		lo, hi := i, i
		for k := i + 1; k < j; k++ {
			if y(pts[k]) < y(pts[lo]) {
				lo = k
			}
			if y(pts[k]) > y(pts[hi]) {
				hi = k
			}
		}
		idx := []int{i}
		for _, k := range []int{lo, hi} {
			dup := false
			for _, e := range idx {
				if e == k {
					dup = true
				}
			}
			if !dup {
				idx = append(idx, k)
			}
		}
		sort.Ints(idx)
		for _, k := range idx {
			out = append(out, pts[k])
		}
	}
	return out
}

// wlcIntFromExtra returns the wireless-charging status (0..2) from a check-in's extra,
// or nil when absent/out of range — matching the wlcStatus template helper.
func wlcIntFromExtra(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	v, ok := m["wlc_status"]
	if !ok {
		return nil
	}
	var n int
	if json.Unmarshal(v, &n) != nil || n < 0 || n > 2 {
		return nil
	}
	return &n
}

// chargeRun is one span of a device's charging state for the battery chart's charging
// strip: C is nil where the check-ins carried no "charging" key. From/To are the first
// and last check-in in the span. F marks a flapping charger, drawn as a hatched yellow
// run: a bad cable toggling every second or two, which C alone would draw as slivers.
type chargeRun struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	C    *bool `json:"c"`
	F    bool  `json:"f,omitempty"`
}

// sameCharge reports whether a point's state continues run r.
func (r *chargeRun) sameCharge(c *bool, flap bool) bool {
	if r.F || flap {
		return r.F == flap
	}
	return (r.C == nil && c == nil) || (r.C != nil && c != nil && *r.C == *c)
}

// chargingFromExtra returns a check-in's "charging" flag, or nil when absent, and
// whether it says the charger is flapping.
func chargingFromExtra(raw json.RawMessage) (*bool, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	flap := string(m["charger_flapping"]) == "true"
	v, ok := m["charging"]
	if !ok {
		return nil, flap
	}
	var b bool
	if json.Unmarshal(v, &b) != nil {
		return nil, flap
	}
	return &b, flap
}

// chargeRuns folds oldest-first check-ins into charging-state spans. It runs over
// every check-in, before the battery series is thinned, so a charger flip between two
// kept samples still lands where it happened. A silence longer than gapMs ends a span,
// matching where the battery line breaks (the chart's gapThresholdMs).
func chargeRuns(asc []db.Checkin, gapMs int64) []chargeRun {
	runs := []chargeRun{}
	for _, c := range asc {
		x := c.CreatedAt.UnixMilli()
		st, flap := chargingFromExtra(c.Extra)
		if n := len(runs); n > 0 {
			last := &runs[n-1]
			if last.sameCharge(st, flap) && x-last.To <= gapMs {
				last.To = x
				continue
			}
		}
		runs = append(runs, chargeRun{From: x, To: x, C: st, F: flap})
	}
	return runs
}

// chartGapMs mirrors device.html's gapThresholdMs: silence past max(poll×10, 15 min)
// is the device being dark, not batching.
func chartGapMs(pollIntervalMs int) int64 {
	poll := int64(pollIntervalMs)
	if poll <= 0 {
		poll = 30000
	}
	if g := poll * 10; g > 900000 {
		return g
	}
	return 900000
}

// ramPctFromExtra computes RAM used% from a check-in's extra, matching extraRamPct.
func ramPctFromExtra(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	v, ok := m["ram_usage_mb"]
	if !ok {
		return 0, false
	}
	var ram map[string]int
	if json.Unmarshal(v, &ram) != nil {
		return 0, false
	}
	total := ram["total"]
	if total == 0 {
		return 0, false
	}
	pct := float64(ram["used"]) * 100 / float64(total)
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

func (h *Handler) DeviceHistory(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	// The page used to carry a paged table of raw check-ins (a count(*) plus a page query
	// on every view); the template stopped rendering it, and the rows no longer hold
	// anything to show, so neither is fetched.
	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), commands)
	commands = filterShellDeviceCommands(h.role(r), commands)

	h.render(w, r, "device_history.html", map[string]any{
		"Title":    device.SerialNumber + " — History",
		"Device":   device,
		"Commands": commands,
		// device-commands (shared with the device page) reads $.Online to decide which
		// actions to offer; without it the History tab rendered a 500.
		"Online": h.hub.IsConnected(device.ID),
	})
}

func wlcStatusFromExtra(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m["wlc_status"]
	if !ok {
		return ""
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return ""
	}
	switch n {
	case 0:
		return "not_charging"
	case 1:
		return "charging"
	case 2: // gpio27 kept disagreeing instead of settling — a faulty/loose pad, not a fresh guest
		return "pad_flapping"
	case -1: // the sysfs read itself failed; the pad's actual state is unknown
		return "pad_unreadable"
	default:
		return ""
	}
}

func (h *Handler) DeviceBatteryCSV(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	hours := 48
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	from := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	// Empty when the window holds no samples: the CSV is then just its header.
	checkins, _ := h.shapedCheckins(r.Context(), device.ID, from, time.Now().UTC())

	filename := fmt.Sprintf("%s_battery_%dh.csv", serial, hours)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	cw.Write([]string{"timestamp", "battery_pct", "wlc_status"})
	for i := len(checkins) - 1; i >= 0; i-- {
		c := checkins[i]
		cw.Write([]string{
			c.CreatedAt.Format(time.RFC3339),
			strconv.Itoa(c.BatteryPct),
			wlcStatusFromExtra(c.Extra),
		})
	}
	cw.Flush()
}

// DeviceDailyStatsJSON returns the device's rolled-up daily stats as JSON, oldest
// first. ?days=N selects the window (default 30, capped at 365). Drives the Trends tab.
func (h *Handler) DeviceDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}

	stats, err := h.db.GetDeviceDailyStats(r.Context(), device.ID, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.DeviceDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// GroupDailyStatsJSON returns daily stats aggregated across all devices in a group as
// JSON, oldest first. ?days=N selects the window (default 30, capped at 365).
func (h *Handler) GroupDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}

	stats, err := h.db.GetGroupDailyStats(r.Context(), id, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.GroupDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}
