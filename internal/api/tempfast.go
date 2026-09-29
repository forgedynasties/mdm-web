package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Fast temperature. Normally the client sends its temperature only when it moved 1 °C,
// so the dashboard can sit up to that far behind the device. With firmware v2.1.023
// healthd's reading is smooth, and a device switched on here (for a set number of hours)
// sends it every few seconds; every such reading is kept for a day in device_temp_fast.

const (
	tempFastDefaultSec   = 10
	tempFastMinSec       = 2
	tempFastMaxHours     = 72
	tempFastDefaultHours = 24
)

// recordTempFast keeps the frame's temperature while the device is switched on, and
// returns the interval the device should report at (0 = off) for its config.
func (h *Handler) recordTempFast(ctx context.Context, deviceID uuid.UUID, extra json.RawMessage, tag string) int {
	s, on, err := h.db.TempFast(ctx, deviceID)
	if err != nil {
		log.Printf("[%s] TempFast error: %v", tag, err)
		return 0
	}
	if !on {
		return 0
	}
	var m struct {
		Temp   *float64 `json:"battery_temp_c"`
		Uptime *float64 `json:"uptime_seconds"`
	}
	if len(extra) > 0 && json.Unmarshal(extra, &m) == nil && m.Temp != nil && *m.Temp > -40 {
		var up *int64
		if m.Uptime != nil {
			u := int64(*m.Uptime)
			up = &u
		}
		if err := h.db.InsertTempFast(ctx, deviceID, *m.Temp, up); err != nil {
			log.Printf("[%s] InsertTempFast error: %v", tag, err)
		}
	}
	return s.IntervalSec
}

// SetTempFast: POST /api/v1/devices/{serial}/temp-fast {"hours":24,"interval_sec":10}.
// hours 0 switches it off. The device picks it up in the config its next frame gets back.
func (h *Handler) SetTempFast(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	body := struct {
		Hours       *int `json:"hours"`
		IntervalSec *int `json:"interval_sec"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	hours, sec := tempFastDefaultHours, tempFastDefaultSec
	if body.Hours != nil {
		hours = *body.Hours
	}
	if body.IntervalSec != nil {
		sec = *body.IntervalSec
	}
	if hours < 0 || hours > tempFastMaxHours {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hours must be 0-" + strconv.Itoa(tempFastMaxHours)})
		return
	}
	if sec < tempFastMinSec || sec > 3600 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interval_sec must be " + strconv.Itoa(tempFastMinSec) + "-3600"})
		return
	}
	if err := h.db.SetTempFast(r.Context(), device.ID, sec, hours); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.writeTempFast(w, r, device, time.Time{})
}

// GetTempFast: GET /api/v1/devices/{serial}/temp-fast?since=<unix seconds> — the setting
// and the kept readings (the last TempFastKeepHours when since is absent).
func (h *Handler) GetTempFast(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	since := time.Now().Add(-db.TempFastKeepHours * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be unix seconds"})
			return
		}
		since = time.Unix(0, int64(f*1e9))
	}
	h.writeTempFast(w, r, device, since)
}

// writeTempFast answers with the setting, plus the readings after since unless it is zero.
func (h *Handler) writeTempFast(w http.ResponseWriter, r *http.Request, device *db.Device, since time.Time) {
	out := map[string]any{"serial": device.SerialNumber, "enabled": false}
	s, on, err := h.db.TempFast(r.Context(), device.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if on {
		out["enabled"], out["interval_sec"], out["until"] = true, s.IntervalSec, s.Until
	}
	if !since.IsZero() {
		rows, err := h.db.TempFastSince(r.Context(), device.ID, since)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		out["readings"] = rows
	}
	writeJSON(w, http.StatusOK, out)
}
