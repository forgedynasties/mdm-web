package dashboard

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── Battery data (/battery; D1, picked 15 Oct) ────────────────────────────────
//
// What firmware client 1.10.1 collects for the battery health model, readable before the
// model exists: the devices that are sending, one device's curve (cell mV and % against
// charging, plug, screen and temperature), its raw rows, its charge climbs, and a CSV of
// any of it. Times are the device's own clock, as stored.

const (
	batteryListWindow = 7 * 24 * time.Hour
	batteryChartMax   = 4000 // points drawn; more are thinned, state changes kept
	batteryRawRows    = 300
)

var batteryRanges = map[string]time.Duration{"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

func batteryRoleOK(role string) bool { return role == "admin" || role == "dev" || role == "super_op" }

func batteryRange(r *http.Request) (string, time.Time, time.Time) {
	key := r.URL.Query().Get("range")
	d, ok := batteryRanges[key]
	if !ok {
		key, d = "24h", batteryRanges["24h"]
	}
	now := time.Now()
	// The rows carry the device's clock, which can run a little ahead of ours.
	return key, now.Add(-d), now.Add(time.Hour)
}

type batteryClimbView struct {
	At                time.Time
	End, Plug         string
	MV0, Step, N, Gap int
	Rungs             int
	IChg              string
	Temp              string
	Raw               string
}

func (h *Handler) BatteryPage(w http.ResponseWriter, r *http.Request) {
	if !batteryRoleOK(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	acc := h.access(r)
	all, err := h.db.BatteryDevices(ctx, time.Now().Add(-batteryListWindow))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var devices []db.BatteryDeviceSummary
	for _, d := range all {
		if acc.visible(d.DeviceID) {
			devices = append(devices, d)
		}
	}
	rangeKey, from, to := batteryRange(r)
	data := map[string]any{"Title": "Battery data", "Devices": devices, "Range": rangeKey, "Ev": r.URL.Query().Get("ev")}

	serial := r.URL.Query().Get("serial")
	var sel *db.BatteryDeviceSummary
	for i := range devices {
		if devices[i].Serial == serial || (serial == "" && i == 0) {
			sel = &devices[i]
			break
		}
	}
	if sel == nil && serial != "" {
		// A device with nothing in the list window (or nothing at all yet) still opens.
		if dev, err := h.db.GetDevice(ctx, serial); err == nil && acc.visible(dev.ID) {
			sel = &db.BatteryDeviceSummary{DeviceID: dev.ID, Serial: dev.SerialNumber, Venue: dev.RestaurantName}
		}
	}
	if sel != nil {
		data["Sel"] = sel
		rows, err := h.db.BatterySamplesFor(ctx, sel.DeviceID, from, to)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		data["Rows"] = len(rows)
		if len(rows) > 0 {
			first, last := rows[0], rows[len(rows)-1]
			data["Span"] = compAgo(last.T.Sub(first.T))
			lo, hi, edges := int32(1<<30), int32(0), 0
			for _, s := range rows {
				if s.MV != nil {
					lo, hi = min(lo, *s.MV), max(hi, *s.MV)
				}
				if s.Ev == "s" {
					edges++
				}
			}
			data["MVLow"], data["MVHigh"], data["Edges"] = lo, hi, edges
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].MV != nil {
					data["MVNow"] = *rows[i].MV
					break
				}
			}
		}
		b, _ := json.Marshal(batteryChartPoints(rows))
		data["Chart"] = template.JS(b)
		// Raw rows, newest first, optionally one event type.
		ev := r.URL.Query().Get("ev")
		var raw []db.BatterySample
		for i := len(rows) - 1; i >= 0 && len(raw) < batteryRawRows; i-- {
			if ev == "" || rows[i].Ev == ev {
				raw = append(raw, rows[i])
			}
		}
		data["Raw"] = raw
		climbs, _ := h.db.BatteryClimbsFor(ctx, sel.DeviceID, 50)
		data["Climbs"] = batteryClimbViews(climbs)
	}
	h.render(w, r, "battery.html", data)
}

// batteryChartPoints turns rows into the chart's compact form, [ms, ev, mv, pct, status,
// plug, temp, screen], thinning long ranges to batteryChartMax while keeping every row
// where charging, plug or screen changed (the edges the chart's bands are drawn from).
func batteryChartPoints(rows []db.BatterySample) [][]any {
	keep := make([]bool, len(rows))
	stride := 1
	if len(rows) > batteryChartMax {
		stride = (len(rows) + batteryChartMax - 1) / batteryChartMax
	}
	val := func(p *int16) int16 {
		if p == nil {
			return -1
		}
		return *p
	}
	for i := range rows {
		if i%stride == 0 || i == len(rows)-1 || rows[i].Ev == "s" {
			keep[i] = true
			continue
		}
		p := rows[i-1]
		if val(rows[i].Status) != val(p.Status) || val(rows[i].Plug) != val(p.Plug) {
			keep[i] = true
		}
	}
	out := make([][]any, 0, min(len(rows), batteryChartMax*2))
	for i, s := range rows {
		if !keep[i] {
			continue
		}
		mv, temp, screen := 0, -100.0, -1
		if s.MV != nil {
			mv = int(*s.MV)
		}
		if s.TempC != nil {
			temp = float64(*s.TempC)
		}
		if s.Screen != nil {
			screen = 0
			if *s.Screen {
				screen = 1
			}
		}
		out = append(out, []any{s.T.UnixMilli(), s.Ev, mv, val(s.Pct), val(s.Status), val(s.Plug), temp, screen})
	}
	return out
}

func batteryClimbViews(cs []db.BatteryClimb) []batteryClimbView {
	var out []batteryClimbView
	for _, c := range cs {
		var p struct {
			MV0  int       `json:"mv0"`
			Step int       `json:"step"`
			S    []int     `json:"s"`
			End  string    `json:"end"`
			IChg []int     `json:"ichg_ma"`
			Plug string    `json:"plug"`
			Temp []float64 `json:"temp_c"`
			Gap  int       `json:"gap_s"`
			N    int       `json:"n"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		v := batteryClimbView{At: c.At, End: p.End, Plug: p.Plug, MV0: p.MV0, Step: p.Step, N: p.N, Gap: p.Gap, Rungs: len(p.S), Raw: string(c.Payload)}
		if len(p.IChg) == 2 {
			v.IChg = fmt.Sprintf("%d–%d mA", p.IChg[0], p.IChg[1])
		}
		if len(p.Temp) == 2 {
			v.Temp = fmt.Sprintf("%.1f–%.1f °C", p.Temp[0], p.Temp[1])
		}
		out = append(out, v)
	}
	return out
}

// BatteryExport is the CSV of a device's rows, or every visible device's, over a range.
func (h *Handler) BatteryExport(w http.ResponseWriter, r *http.Request) {
	if !batteryRoleOK(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	acc := h.access(r)
	rangeKey, from, to := batteryRange(r)
	var dev *uuid.UUID
	name := "fleet"
	if serial := r.URL.Query().Get("serial"); serial != "" {
		d, err := h.db.GetDevice(ctx, serial)
		if err != nil || !acc.visible(d.ID) {
			http.Error(w, "Device not found", http.StatusNotFound)
			return
		}
		dev, name = &d.ID, serial
	}
	if dev == nil && acc.hidesDevices() {
		http.Error(w, "Your access covers some devices only: export one device at a time.", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("battery-%s-%s-%s.csv", name, rangeKey, time.Now().Format("20060102-1504"))))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"serial", "t_ms", "up_s", "ev", "mv", "pct", "status", "plug", "temp_c", "screen", "ichg_ma", "iin_ma", "flap", "wlc", "wlc_on", "t_fix_ms"})
	i32 := func(p *int32) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(int(*p))
	}
	i16 := func(p *int16) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(int(*p))
	}
	b := func(p *bool) string {
		if p == nil {
			return ""
		}
		if *p {
			return "1"
		}
		return "0"
	}
	_ = h.db.EachBatterySample(ctx, dev, from, to, func(x db.BatteryExportRow) error {
		temp := ""
		if x.TempC != nil {
			temp = strconv.FormatFloat(float64(*x.TempC), 'f', 1, 32)
		}
		return cw.Write([]string{x.Serial, strconv.FormatInt(x.T.UnixMilli(), 10), strconv.FormatInt(x.UpS, 10), x.Ev,
			i32(x.MV), i16(x.Pct), i16(x.Status), i16(x.Plug), temp, b(x.Screen), i32(x.IChgMA), i32(x.IInMA), b(x.Flap),
			i16(x.Wlc), i16(x.WlcOn), strconv.FormatInt(x.TFixMs, 10)})
	})
	cw.Flush()
}
