package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// LogcatLivePage renders the live logcat console for a device.
func (h *Handler) LogcatLivePage(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if !device.Supports("logcat") {
		http.Error(w, "Live logs aren't available on this device's agent ("+device.KindLabel()+").", http.StatusBadRequest)
		return
	}
	h.render(w, r, "logcat_live.html", map[string]any{
		"Title":  "Realtime logs · " + device.SerialNumber,
		"Device": device,
		"Online": h.hub.IsConnected(device.ID),
	})
}

// LogcatStream opens a Server-Sent Events stream of live `logcat` output from a
// device. It mints a request_id, pushes start_logcat_stream to the device, relays
// each chunk as an SSE `data:` line, and pushes stop_logcat_stream when the browser
// disconnects. Query params: level (V|D|I|W|E), tag, buffer, grep, tail.
func (h *Handler) LogcatStream(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if !h.hub.IsConnected(device.ID) {
		http.Error(w, "device not connected", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	reqID := uuid.NewString()
	ch := h.logs.Open(reqID)
	defer h.logs.Close(reqID)

	start, _ := json.Marshal(map[string]any{
		"type":       "start_logcat_stream",
		"request_id": reqID,
		"level":      logcatLevel(r.URL.Query().Get("level")),
		"tag":        strings.TrimSpace(r.URL.Query().Get("tag")),
		"buffer":     strings.TrimSpace(r.URL.Query().Get("buffer")),
		"grep":       strings.TrimSpace(r.URL.Query().Get("grep")),
		"tail":       logcatTail(r.URL.Query().Get("tail")),
	})
	if !h.hub.Push(device.ID, start) {
		http.Error(w, "failed to reach device", http.StatusServiceUnavailable)
		return
	}
	// Tell the device to stop streaming the moment this browser goes away.
	defer func() {
		stop, _ := json.Marshal(map[string]any{"type": "stop_logcat_stream", "request_id": reqID})
		h.hub.Push(device.ID, stop)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, ": logcat stream %s\n\n", reqID)
	flusher.Flush()

	ka := time.NewTicker(20 * time.Second)
	defer ka.Stop()
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ka.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// logcatLevel clamps a requested min-priority to a valid logcat level char.
func logcatLevel(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "V", "D", "I", "W", "E":
		return strings.ToUpper(strings.TrimSpace(s))
	default:
		return "V"
	}
}

// logcatTail clamps the initial backlog line count to a sane range.
func logcatTail(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 200
	}
	if n > 5000 {
		return 5000
	}
	return n
}
