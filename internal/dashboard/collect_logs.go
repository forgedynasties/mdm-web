package dashboard

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Collect logs: one fixed script a firmware device runs as a shell command, its output
// kept as the command's result and downloaded as a text file per device, or a zip for
// a run across many. The text is the server's, never the caller's, which is why
// operators may run it without shell rights (see product.Command "collect_logs").
//
// The client returns at most 1 MB of output, so the small sections come first and the
// long logcat last: if anything is cut off it is the oldest log lines.
const collectLogsScript = `s(){ echo; echo "===== $1 ====="; }
s "device"; date; uptime; getprop ro.serialno; getprop ro.build.display.id; getprop ro.build.fingerprint
s "battery"; dumpsys battery
s "storage"; df -h /data /cache 2>/dev/null
s "memory"; dumpsys meminfo 2>/dev/null | head -n 45
s "wifi"; dumpsys wifi 2>/dev/null | grep -E "mWifiInfo|Supplicant state|mNetworkInfo" | head -n 20
s "ota (update_engine)"; logcat -d -s update_engine 2>/dev/null | tail -n 150
s "crash buffer"; logcat -d -b crash -v threadtime 2>/dev/null | tail -n 400
s "getprop"; getprop
s "logcat (last 3000 lines)"; logcat -d -b main,system -v threadtime -t 3000 2>/dev/null`

func collectLogsPayload() json.RawMessage {
	b, _ := json.Marshal(map[string]string{"cmd": collectLogsScript})
	return json.RawMessage(b)
}

// canReadLogs is the gate for downloading a bundle: the same as sending one, per device.
func (h *Handler) canReadLogs(r *http.Request, dev uuid.UUID) bool {
	return h.authorizeCommand(h.role(r), "collect_logs") == cmdAuthzOK && h.access(r).canDevice(policyActionForCommand("collect_logs"), dev)
}

// writeLogBundle writes one device's bundle: a header, the device's output, and the
// crashes the server already holds for it.
func (h *Handler) writeLogBundle(w io.Writer, r *http.Request, cmdID uuid.UUID, l db.CommandLog) {
	fmt.Fprintf(w, "AIO MDM log bundle\nDevice:     %s\nRestaurant: %s\nBuild:      %s\nCollected:  %s\nCommand:    %s\n",
		l.Serial, orDash(l.Restaurant), orDash(l.BuildID), l.At.UTC().Format(time.RFC3339), cmdID)
	io.WriteString(w, l.Output)
	if !strings.HasSuffix(l.Output, "\n") {
		io.WriteString(w, "\n")
	}
	id := l.DeviceID
	crashes, _, _ := h.db.ListRecentCrashGroupsPage(r.Context(), &id, 7, 20, 0)
	fmt.Fprintf(w, "\n===== crashes reported to the MDM (7 days): %d =====\n", len(crashes))
	for _, c := range crashes {
		fmt.Fprintf(w, "\n--- %s · %s · build %s · x%d\n%s\n", c.OccurredAt.UTC().Format(time.RFC3339), c.Kind, orDash(c.BuildID), c.EventCount, c.Summary)
		if c.Detail != "" {
			io.WriteString(w, c.Detail)
			io.WriteString(w, "\n")
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func logBundleName(serial string, at time.Time) string {
	return fmt.Sprintf("logs-%s-%s.txt", serial, at.UTC().Format("20060102-1504"))
}

// CommandLogs downloads one device's bundle: GET /commands/{id}/logs/{serial}.
func (h *Handler) CommandLogs(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.logsCommand(w, r)
	if !ok {
		return
	}
	logs, err := h.db.ListCommandLogs(r.Context(), id, r.PathValue("serial"))
	if err != nil || len(logs) == 0 {
		http.Error(w, "No logs from this device yet", http.StatusNotFound)
		return
	}
	l := logs[0]
	if !h.canReadLogs(r, l.DeviceID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	disp := "inline"
	if r.URL.Query().Get("download") != "" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, logBundleName(l.Serial, l.At)))
	h.audit(r, "logs.download", l.Serial, "cmd="+id.String())
	h.writeLogBundle(w, r, id, l)
}

// CommandLogsZip downloads every device's bundle from one run: GET /commands/{id}/logs.zip.
func (h *Handler) CommandLogsZip(w http.ResponseWriter, r *http.Request) {
	id, cmd, ok := h.logsCommand(w, r)
	if !ok {
		return
	}
	logs, err := h.db.ListCommandLogs(r.Context(), id, "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var keep []db.CommandLog
	for _, l := range logs {
		if h.canReadLogs(r, l.DeviceID) {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		http.Error(w, "No logs collected yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "logs-"+cmd.CreatedAt.UTC().Format("20060102-1504")+".zip"))
	zw := zip.NewWriter(w)
	for _, l := range keep {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: logBundleName(l.Serial, l.At), Method: zip.Deflate, Modified: l.At})
		if err != nil {
			break
		}
		h.writeLogBundle(f, r, id, l)
	}
	zw.Close()
	h.audit(r, "logs.download", "zip", fmt.Sprintf("cmd=%s devices=%d", id, len(keep)))
}

func (h *Handler) logsCommand(w http.ResponseWriter, r *http.Request) (uuid.UUID, *db.Command, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return id, nil, false
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return id, nil, false
	}
	if cmd.Type != "collect_logs" {
		http.Error(w, "Not a log collection", http.StatusBadRequest)
		return id, nil, false
	}
	return id, cmd, true
}
