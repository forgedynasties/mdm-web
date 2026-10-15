package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// Battery log (firmware client 1.10.1, T7). The client keeps a row for every battery
// broadcast and screen edge (at most one per 30 s while nothing changes) and sends them
// here in batches, oldest first. Storage only for now: the health model is designed from
// a few weeks of this data.
//
// Answers the client relies on: 2xx moves its cursor; 404/405/501 make it back off 6 h and
// keep the rows (21 days); anything else retries in 15 minutes. Idempotent: a resent batch
// dedupes on (device, t_ms, up_s, ev).

const (
	batteryLogSchema  = 1
	batteryLogMaxRows = 2000
)

// Battery-log columns the server reads; others are ignored, these three are required.
var batteryLogRequired = []string{"t_ms", "up_s", "ev"}

// BatteryLog is POST /api/v1/battery-log.
func (h *Handler) BatteryLog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Serial       string `json:"serial"`
		Schema       int    `json:"schema"`
		Columns      string `json:"columns"`
		NowMs        int64  `json:"now_ms"`
		NowElapsedMs int64  `json:"now_elapsed_ms"`
		BootID       string `json:"boot_id"`
		Rows         string `json:"rows"`
	}
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.Serial = strings.TrimSpace(req.Serial)
	if !validSerial(req.Serial) || len(req.Serial) > maxSerialLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	// A schema this server does not know: 501, so the client keeps its rows and waits for
	// a server that does, instead of retrying every 15 minutes.
	if req.Schema != batteryLogSchema {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "battery-log schema " + strconv.Itoa(req.Schema) + " is not supported"})
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
	// Our firmware client on hardware with a battery (the T7); anything else is told the
	// endpoint isn't there for it, which parks its log for 6 h.
	if dev.IsDPC() || !dev.HasBattery() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "battery log is kept for battery-powered firmware devices only"})
		return
	}

	col := map[string]int{}
	for i, c := range strings.Split(req.Columns, ",") {
		col[strings.TrimSpace(c)] = i
	}
	for _, c := range batteryLogRequired {
		if _, ok := col[c]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "columns must include t_ms, up_s and ev"})
			return
		}
	}
	lines := strings.Split(strings.TrimSpace(req.Rows), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > batteryLogMaxRows {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "at most 2000 rows per request"})
		return
	}
	rows := make([]db.BatterySample, 0, len(lines))
	bad := 0
	for _, ln := range lines {
		if s, ok := parseBatteryRow(strings.Split(strings.TrimSpace(ln), ","), col); ok {
			rows = append(rows, s)
		} else {
			bad++
		}
	}
	// How far the device's clock is from ours now, kept beside the rows (t stays the
	// device's own time, so a resend dedupes exactly).
	var fixMs int64
	if req.NowMs > 0 {
		fixMs = time.Now().UnixMilli() - req.NowMs
	}
	stored, err := h.db.InsertBatterySamples(ctx, dev.ID, fixMs, rows)
	if err != nil {
		log.Printf("[battery-log] %s: %v", req.Serial, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	log.Printf("[battery-log] %s: %d rows, %d stored, %d already known, %d unreadable", req.Serial, len(lines), stored, len(rows)-stored, bad)
	// Every row is answered (stored, known or unreadable), so the client can move on.
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(lines), "stored": stored, "unreadable": bad})
}

// parseBatteryRow reads one CSV row by column name. The client's "unknown" values (-1,
// and -100 for temperature) become nulls.
func parseBatteryRow(f []string, col map[string]int) (db.BatterySample, bool) {
	get := func(name string) (string, bool) {
		i, ok := col[name]
		if !ok || i >= len(f) {
			return "", false
		}
		v := strings.TrimSpace(f[i])
		return v, v != ""
	}
	var s db.BatterySample
	tms, ok := get("t_ms")
	if !ok {
		return s, false
	}
	t, err := strconv.ParseInt(tms, 10, 64)
	if err != nil || t <= 0 {
		return s, false
	}
	ups, ok := get("up_s")
	if !ok {
		return s, false
	}
	if s.UpS, err = strconv.ParseInt(ups, 10, 64); err != nil {
		return s, false
	}
	if s.Ev, ok = get("ev"); !ok || len(s.Ev) != 1 {
		return s, false
	}
	s.T = time.UnixMilli(t).UTC()
	i32 := func(name string, unknownBelow int64) *int32 {
		v, ok := get(name)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < unknownBelow {
			return nil
		}
		x := int32(n)
		return &x
	}
	i16 := func(name string) *int16 {
		v := i32(name, 0)
		if v == nil || *v > 32767 {
			return nil
		}
		x := int16(*v)
		return &x
	}
	flag := func(name string) *bool {
		v := i32(name, 0)
		if v == nil {
			return nil
		}
		b := *v != 0
		return &b
	}
	s.MV = i32("mv", 0)
	s.Pct = i16("pct")
	s.Status = i16("status")
	s.Plug = i16("plug")
	s.Screen = flag("screen")
	s.IChgMA = i32("ichg_ma", 0)
	s.IInMA = i32("iin_ma", 0)
	s.Flap = flag("flap")
	s.Wlc = i16("wlc")
	s.WlcOn = i16("wlc_on")
	if v, ok := get("temp_c"); ok {
		if x, err := strconv.ParseFloat(v, 32); err == nil && x > -100 {
			f := float32(x)
			s.TempC = &f
		}
	}
	return s, true
}
