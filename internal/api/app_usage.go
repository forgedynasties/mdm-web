package api

import (
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
)

// POST /api/v1/app-usage: a client's per-app summary for one window (A1, 22 Oct 2026).
//
//	{"serial", "from_ms", "to_ms", "apps": [{"package", "system", "fg_ms", "launches",
//	  "rx_bytes", "tx_bytes", "pss_mb", "pss_max_mb", "cpu_pct", "power_mah"}]}
//
// Window times are the device's clock; the window is added into the hour of its middle,
// read on the server's clock when the device's is off by more than a day. Memory and
// CPU are averages over the window; leave them out when not measured.
type appUsageApp struct {
	Package  string   `json:"package"`
	System   bool     `json:"system"`
	FgMs     int64    `json:"fg_ms"`
	Launches int      `json:"launches"`
	RxBytes  int64    `json:"rx_bytes"`
	TxBytes  int64    `json:"tx_bytes"`
	PssMB    *float64 `json:"pss_mb"`
	PssMaxMB *int     `json:"pss_max_mb"`
	CPUPct   *float64 `json:"cpu_pct"`
	PowerMAh float64  `json:"power_mah"`
}

type appUsageRequest struct {
	Serial string        `json:"serial"`
	FromMs int64         `json:"from_ms"`
	ToMs   int64         `json:"to_ms"`
	Apps   []appUsageApp `json:"apps"`
}

const appUsageMaxApps = 400

func (h *Handler) AppUsage(w http.ResponseWriter, r *http.Request) {
	var req appUsageRequest
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.Serial = strings.TrimSpace(req.Serial)
	if !validSerial(req.Serial) || len(req.Serial) > maxSerialLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if len(req.Apps) > appUsageMaxApps || req.ToMs <= req.FromMs {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at most 400 apps, and to_ms after from_ms"})
		return
	}
	if h.deviceRateLimited(w, req.Serial) {
		return
	}
	if !h.requireDeviceIdentity(w, r, req.Serial) {
		return
	}
	dev, err := h.db.GetDevice(r.Context(), req.Serial)
	if err != nil || dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device: check in first"})
		return
	}
	at := time.UnixMilli(req.FromMs + (req.ToMs-req.FromMs)/2)
	now := time.Now()
	if at.After(now.Add(time.Hour)) || at.Before(now.Add(-8*24*time.Hour)) {
		at = now // a device clock this far off says nothing about when
	}
	apps := make([]db.AppUsageReport, 0, len(req.Apps))
	for _, a := range req.Apps {
		pkg := strings.TrimSpace(a.Package)
		if pkg == "" || len(pkg) > 255 || a.FgMs < 0 || a.RxBytes < 0 || a.TxBytes < 0 || a.PowerMAh < 0 {
			continue
		}
		if a.CPUPct != nil && (*a.CPUPct < 0 || *a.CPUPct > 800) {
			a.CPUPct = nil
		}
		apps = append(apps, db.AppUsageReport{Package: pkg, System: a.System, FgMs: a.FgMs, Launches: a.Launches,
			RxBytes: a.RxBytes, TxBytes: a.TxBytes, PssMB: a.PssMB, PssMaxMB: a.PssMaxMB, CPUPct: a.CPUPct, PowerMAh: a.PowerMAh})
	}
	if err := h.db.AddAppUsage(r.Context(), dev.ID, at, apps); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(apps)})
}
