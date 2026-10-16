package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Battery data for the testing team (QA key): what the dashboard's /battery page shows —
// the devices sending firmware client 1.10.1's battery log, each one's raw rows, and its
// charge climbs — as JSON (or CSV) for their own tools. Same scope as the other testdata
// routes: firmware devices only, narrowed by ?group= / ?restaurant=.
//
// Times in the rows are the DEVICE's clock, as stored; t_fix_ms is how far the server's
// clock was ahead of the device's when the batch arrived (add it to t_ms for server time).

const (
	qaBatteryListWindow = 7 * 24 * time.Hour
	qaBatteryMaxSpan    = 31 * 24 * time.Hour
	qaBatteryClimbsMax  = 500
)

var qaBatteryRanges = map[string]time.Duration{"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

type qaBatteryDevice struct {
	SerialNumber   string    `json:"serial_number"`
	RestaurantName string    `json:"restaurant_name"`
	Rows           int       `json:"rows"`
	FirstAt        time.Time `json:"first_at"`
	LastAt         time.Time `json:"last_at"`
	LastMV         *int32    `json:"last_mv"`
	LastPct        *int16    `json:"last_pct"`
	Climbs         int       `json:"climbs"`
}

type qaBatteryRow struct {
	T      time.Time `json:"t"`
	TMs    int64     `json:"t_ms"`
	UpS    int64     `json:"up_s"`
	Ev     string    `json:"ev"`
	MV     *int32    `json:"mv"`
	Pct    *int16    `json:"pct"`
	Status *int16    `json:"status"`
	Plug   *int16    `json:"plug"`
	TempC  *float32  `json:"temp_c"`
	Screen *bool     `json:"screen"`
	IChgMA *int32    `json:"ichg_ma"`
	IInMA  *int32    `json:"iin_ma"`
	Flap   *bool     `json:"flap"`
	Wlc    *int16    `json:"wlc"`
	WlcOn  *int16    `json:"wlc_on"`
	TFixMs int64     `json:"t_fix_ms"`
}

// ListTestDataBattery is GET /api/v1/testdata/battery: the devices that sent battery rows
// in the last 7 days, most rows first.
func (h *Handler) ListTestDataBattery(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.qaDeviceFilter(w, r)
	if !ok {
		return
	}
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 10000, "", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	inScope := make(map[uuid.UUID]bool, len(devices))
	for _, d := range devices {
		inScope[d.ID] = true
	}
	all, err := h.db.BatteryDevices(r.Context(), time.Now().Add(-qaBatteryListWindow))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]qaBatteryDevice, 0, len(all))
	for _, b := range all {
		if !inScope[b.DeviceID] {
			continue
		}
		out = append(out, qaBatteryDevice{
			SerialNumber: b.Serial, RestaurantName: b.Venue, Rows: b.Rows,
			FirstAt: b.First.UTC(), LastAt: b.Last.UTC(), LastMV: b.LastMV, LastPct: b.LastPct, Climbs: b.Climbs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// GetTestDataDeviceBattery is GET /api/v1/testdata/devices/{serial}/battery: the device's
// rows, oldest first. ?range=6h|24h|7d|30d (default 24h), or ?start=&end= (RFC3339; the
// span is capped at 31 days). ?format=csv answers with the dashboard's CSV columns.
func (h *Handler) GetTestDataDeviceBattery(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.qaFirmwareDevice(w, r)
	if !ok {
		return
	}
	from, to, err := qaBatteryWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "battery-"+dev.SerialNumber+".csv"))
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"serial", "t_ms", "up_s", "ev", "mv", "pct", "status", "plug", "temp_c", "screen", "ichg_ma", "iin_ma", "flap", "wlc", "wlc_on", "t_fix_ms"})
		_ = h.db.EachBatterySample(r.Context(), &dev.ID, from, to, func(x db.BatteryExportRow) error {
			return cw.Write([]string{x.Serial, strconv.FormatInt(x.T.UnixMilli(), 10), strconv.FormatInt(x.UpS, 10), x.Ev,
				qaI32(x.MV), qaI16(x.Pct), qaI16(x.Status), qaI16(x.Plug), qaTemp(x.TempC), qaBool(x.Screen), qaI32(x.IChgMA),
				qaI32(x.IInMA), qaBool(x.Flap), qaI16(x.Wlc), qaI16(x.WlcOn), strconv.FormatInt(x.TFixMs, 10)})
		})
		cw.Flush()
		return
	}
	rows := make([]qaBatteryRow, 0, 1024)
	err = h.db.EachBatterySample(r.Context(), &dev.ID, from, to, func(x db.BatteryExportRow) error {
		rows = append(rows, qaBatteryRow{
			T: x.T.UTC(), TMs: x.T.UnixMilli(), UpS: x.UpS, Ev: x.Ev, MV: x.MV, Pct: x.Pct, Status: x.Status,
			Plug: x.Plug, TempC: x.TempC, Screen: x.Screen, IChgMA: x.IChgMA, IInMA: x.IInMA, Flap: x.Flap,
			Wlc: x.Wlc, WlcOn: x.WlcOn, TFixMs: x.TFixMs,
		})
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serial_number": dev.SerialNumber,
		"start":         from.UTC(),
		"end":           to.UTC(),
		"count":         len(rows),
		"rows":          rows,
	})
}

// GetTestDataDeviceBatteryClimbs is GET /api/v1/testdata/devices/{serial}/battery/climbs:
// the device's charge climbs (client 1.10.1's battery_cc), newest first. ?limit= (default
// 50, at most 500). Each payload is the client's own object, passed through as-is.
func (h *Handler) GetTestDataDeviceBatteryClimbs(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.qaFirmwareDevice(w, r)
	if !ok {
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, qaBatteryClimbsMax)
	}
	climbs, err := h.db.BatteryClimbsFor(r.Context(), dev.ID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	type climb struct {
		At         time.Time       `json:"at"`
		ReceivedAt time.Time       `json:"received_at"`
		Payload    json.RawMessage `json:"payload"`
	}
	out := make([]climb, 0, len(climbs))
	for _, c := range climbs {
		out = append(out, climb{At: c.At.UTC(), ReceivedAt: c.Received.UTC(), Payload: c.Payload})
	}
	writeJSON(w, http.StatusOK, out)
}

// qaFirmwareDevice resolves {serial} for a testdata route: firmware devices only, and a
// device outside that scope is "not found" rather than a different answer.
func (h *Handler) qaFirmwareDevice(w http.ResponseWriter, r *http.Request) (*db.Device, bool) {
	dev, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || dev.AgentKind != product.KindFirmware {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return nil, false
	}
	return dev, true
}

func qaBatteryWindow(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	now := time.Now()
	if q.Get("start") != "" || q.Get("end") != "" {
		// The rows carry the device's clock, which can run a little ahead of ours.
		from, to := now.Add(-24*time.Hour), now.Add(time.Hour)
		if v := q.Get("start"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return from, to, fmt.Errorf("start: want RFC3339, e.g. 2026-10-16T00:00:00Z")
			}
			from = t
		}
		if v := q.Get("end"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return from, to, fmt.Errorf("end: want RFC3339, e.g. 2026-10-17T00:00:00Z")
			}
			to = t
		}
		if !to.After(from) {
			return from, to, fmt.Errorf("end must be after start")
		}
		if to.Sub(from) > qaBatteryMaxSpan {
			return from, to, fmt.Errorf("at most 31 days per request")
		}
		return from, to, nil
	}
	d, ok := qaBatteryRanges[q.Get("range")]
	if !ok {
		if q.Get("range") != "" {
			return now, now, fmt.Errorf("range: one of 6h, 24h, 7d, 30d")
		}
		d = qaBatteryRanges["24h"]
	}
	return now.Add(-d), now.Add(time.Hour), nil
}

func qaI32(p *int32) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(int(*p))
}

func qaI16(p *int16) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(int(*p))
}

func qaBool(p *bool) string {
	if p == nil {
		return ""
	}
	if *p {
		return "1"
	}
	return "0"
}

func qaTemp(p *float32) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(float64(*p), 'f', 1, 32)
}
