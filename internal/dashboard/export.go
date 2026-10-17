package dashboard

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// safeCSVFilename returns a filename safe for use in a Content-Disposition
// header: control chars, quotes, and path separators are replaced with '_'.
// Falls back to the provided default if the result would be empty.
func safeCSVFilename(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f, r == '"', r == '\\', r == '/', r == ':', r == '\r', r == '\n':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		s = fallback
	}
	return s + ".csv"
}

// ── Export ────────────────────────────────────────────────────────────────────

func (h *Handler) ExportPage(w http.ResponseWriter, r *http.Request) {
	var serialList []string
	if serials := r.URL.Query().Get("serials"); serials != "" {
		serialList = strings.Split(serials, ",")
	}
	// Opened without a selection (the Overview's Export button, or a bare /export):
	// render with nothing chosen and let the page's picker build the list, rather
	// than bouncing back to Fleet or silently exporting everything.
	fleetTotal := 0
	if n, err := h.db.CountDevices(r.Context(), db.DeviceFilter{}); err == nil {
		fleetTotal = n
	}

	h.render(w, r, "export.html", map[string]any{
		"Title":      "Export Data",
		"Serials":    serialList,
		"FleetTotal": fleetTotal,
		"ScopesJSON": h.pickerScopesJSON(r),
		"From":       backPath(r),
	})
}

// maxExportRange caps the time window for a single CSV export to keep
// memory and download size bounded. 90 days is well past any reasonable
// dashboard use; bump it if a real workflow needs more.
const maxExportRange = 90 * 24 * time.Hour

// exportColumns is the whole CSV, in order. The export used to be 15 checkboxes
// with presets on top; every sheet anyone actually asked for was this set, and a
// picker that can produce an empty or useless CSV is a way to get the export wrong.
// Network and Meta columns (SSID, IP, last_seen) are gone with it — they identify
// how to reach a device rather than how it is behaving, which is why they were
// admin-only in the first place.
var exportColumns = []string{
	"battery_pct",
	"battery_temp_c",
	"charging",
	"wlc_state",
	"charging_pad",
	"ram_used_pct",
	"storage_free_pct",
}

// storageFreePct is free space as a percentage of the volume, for devices that
// report the volume size (Lite and the DPC agent send storage_total_bytes). The
// firmware client only sends storage_free_gb, so on a T7 or a kiosk this column is
// blank rather than a percentage of a total nobody knows.
func storageFreePct(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	freeGB, err := strconv.ParseFloat(extraFloat(raw, "storage_free_gb"), 64)
	if err != nil {
		return ""
	}
	totalBytes := extraInt64(raw, "storage_total_bytes")
	if totalBytes <= 0 {
		return ""
	}
	totalGB := float64(totalBytes) / (1024 * 1024 * 1024)
	pct := freeGB * 100 / totalGB
	if pct > 100 {
		pct = 100
	}
	return strconv.FormatFloat(pct, 'f', 1, 64)
}

// wlcStateWord spells out a wireless-charging status code for the CSV: the number
// alone (0/1/2) told nobody anything, and 2 in particular — the pad the device has
// slipped off — is the one people export for (FW-2026-000052).
func wlcStateWord(code string) string {
	switch code {
	case "0":
		return "vacant"
	case "1":
		return "charging"
	case "2":
		return "pad misplaced"
	case "":
		return ""
	}
	return "unknown (" + code + ")"
}

func (h *Handler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	// The device picker posts one newline-separated field; the Fleet selection and
	// the device page post one value per serial. parseSerialsField takes both.
	serials := h.keepVisibleSerials(r, parseSerialsField(r.Form["serials"]))
	if len(serials) == 0 {
		http.Error(w, "No devices selected", http.StatusBadRequest)
		return
	}

	// Parse time range
	startStr := r.FormValue("start")
	endStr := r.FormValue("end")
	if startStr == "" || endStr == "" {
		http.Error(w, "Start and end time required", http.StatusBadRequest)
		return
	}
	// datetime-local inputs are wall-clock in the user's timezone. Prefer an
	// explicit tz_offset (minutes east of UTC, as sent by JS via
	// new Date().getTimezoneOffset()*-1) when present; otherwise fall back to
	// the server's local time. Either way, normalize to UTC for the DB query.
	loc := time.Local
	if v := r.FormValue("tz_offset"); v != "" {
		if mins, errOff := strconv.Atoi(v); errOff == nil {
			loc = time.FixedZone("client", mins*60)
		}
	}
	// The window the user typed is read in their own wall clock (above); every CSV
	// *prints* UTC. One fixed zone, no picker: exports get mailed around and merged
	// with each other, and a sheet whose timestamps depended on whoever downloaded it
	// could not be compared with the next one. The device graph is where you look at
	// the same data in your own or the device's zone.
	outLoc, outZone := time.UTC, "UTC"
	start, err := time.ParseInLocation("2006-01-02T15:04", startStr, loc)
	if err != nil {
		http.Error(w, "Invalid start time", http.StatusBadRequest)
		return
	}
	end, err := time.ParseInLocation("2006-01-02T15:04", endStr, loc)
	if err != nil {
		http.Error(w, "Invalid end time", http.StatusBadRequest)
		return
	}
	if !end.After(start) {
		http.Error(w, "End time must be after start time", http.StatusBadRequest)
		return
	}
	if end.Sub(start) > maxExportRange {
		http.Error(w, "Time range too large (max 90 days). Narrow the window.", http.StatusBadRequest)
		return
	}

	// Every row is a check-in the device actually sent, at its own timestamp. The
	// resampling grid (interval / cycles mode) is gone from the form: it repeated the
	// last value for as long as nothing changed, which reads as data the device never
	// reported. The stream functions keep the parameters, so nothing else changes.
	const intervalSec = 0

	// Resolve serials to device IDs
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
	if err != nil || len(deviceIDs) == 0 {
		http.Error(w, "No matching devices", http.StatusBadRequest)
		return
	}

	filename := "mdm_export.csv"
	if len(serials) == 1 {
		filename = safeCSVFilename(serials[0]+"_export", "mdm_export")
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	colOrder := exportColumns

	// One timestamp column, UTC, offset included — the old timestamp / timestamp_utc /
	// sample_at trio only invited "which one is real?" (FW-2026-000052). Rows are raw
	// check-ins now, so the timestamp *is* the moment of the sample.
	header := append([]string{"serial_number", "timestamp (" + outZone + ")"}, colOrder...)
	cw := csv.NewWriter(w)
	// Excel splits a .csv on the list separator of the machine's locale, which is ";"
	// across most of Europe and elsewhere — the whole row then lands in cell A1. The
	// "sep=" preamble is Excel's own opt-out; it is off by default because it is not
	// CSV and other tools would read it as a data row.
	if r.FormValue("excel") == "1" {
		if _, err := io.WriteString(w, "sep=,\n"); err != nil {
			return
		}
	}
	if err := cw.Write(header); err != nil {
		return
	}
	// Per-device charging-pad setting, read once: it is configuration, not telemetry,
	// so it does not ride along on the rows.
	padOn := map[string]bool{}
	if m, padErr := h.db.WlcEnabledBySerial(r.Context(), deviceIDs); padErr == nil {
		padOn = m
	}

	writeRow := func(row db.ExportRow) error {
		rec := []string{
			row.SerialNumber,
			row.Timestamp.In(outLoc).Format(time.RFC3339),
		}
		for _, c := range colOrder {
			if row.Empty {
				rec = append(rec, "")
				continue
			}
			switch c {
			case "battery_pct":
				rec = append(rec, strconv.Itoa(row.BatteryPct))
			case "battery_temp_c":
				rec = append(rec, extraFloat(row.Extra, "battery_temp_c"))
			case "charging":
				rec = append(rec, extraBoolAsInt(row.Extra, "charging"))
			case "wlc_state":
				rec = append(rec, wlcStateWord(extraInt(row.Extra, "wlc_status")))
			case "charging_pad":
				if padOn[row.SerialNumber] {
					rec = append(rec, "enabled")
				} else {
					rec = append(rec, "disabled")
				}
			case "ram_used_pct":
				if pct, ok := ramPctFromExtra(row.Extra); ok {
					rec = append(rec, strconv.FormatFloat(pct, 'f', 1, 64))
				} else {
					rec = append(rec, "")
				}
			case "storage_free_pct":
				rec = append(rec, storageFreePct(row.Extra))
			}
		}
		return cw.Write(rec)
	}
	// The shaped tables cover every device's whole history since the checkins table was
	// retired (samples backfilled to the first check-in, state events derived from the
	// old snapshots), so they answer every export.
	err = h.db.StreamExportShaped(r.Context(), deviceIDs, start.UTC(), end.UTC(), intervalSec, false, writeRow)
	cw.Flush()
	if err != nil {
		// Headers already written; the partial CSV is the best we can do.
		// Log so operators can see disconnects / DB errors.
		log.Printf("export: stream error: %v", err)
	}
}

// ExportVisualizePage renders the "upload a CSV and visualize it" tool.
// Everything after that — reading the file, parsing it, building the table
// and charts — happens entirely client-side (see export_visualize.html): the
// file never gets sent to the server, so this handler has nothing to do but
// serve the static page.
func (h *Handler) ExportVisualizePage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "export_visualize.html", map[string]any{"Title": "Import & Visualize Export"})
}
