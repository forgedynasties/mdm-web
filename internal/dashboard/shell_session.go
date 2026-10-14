package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Interactive shell sessions: one persistent shell on the device for as long as the
// browser keeps the console open, instead of a separate `sh -c` per line. `cd`, shell
// variables and a running program's state carry from one line to the next, and stderr
// arrives with stdout, as it happens.
//
// Wire protocol (over the device WebSocket, keyed like the live logcat stream):
//
//	server → device  {"type":"shell_open","session_id":S}
//	                 {"type":"shell_input","session_id":S,"data":"ls -l\n"}
//	                 {"type":"shell_close","session_id":S}
//	device → server  {"type":"shell_data","request_id":S,"chunk":"..."}
//	                 {"type":"shell_end","request_id":S,"reason":"exited|closed|error","error":"..."}
//
// A client that can do this says so with "shell_session": true in its telemetry; for
// any other client the console keeps the one-command-at-a-time behaviour.
//
// Every line typed is written to the audit log with the device and the user, so a
// session leaves the same trail the per-command shell did.

type shellSessionInfo struct {
	DeviceID uuid.UUID
	Serial   string
	User     string
	Opened   time.Time
}

var shellSessions sync.Map // session id → shellSessionInfo

// supportsShellSession reports whether the device's client runs interactive sessions.
func supportsShellSession(d *db.Device) bool {
	return extraBoolField(d.LatestExtra, "shell_session")
}

// ShellSessionStream opens a session and streams its output as Server-Sent Events. The
// first event, "session", carries the id the browser sends its input to.
func (h *Handler) ShellSessionStream(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if !supportsShellSession(device) {
		http.Error(w, "This device's client does not run interactive sessions yet.", http.StatusBadRequest)
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

	sid := uuid.NewString()
	ch := h.logs.Open(sid)
	defer h.logs.Close(sid)
	open, _ := json.Marshal(map[string]any{"type": "shell_open", "session_id": sid})
	if !h.hub.Push(device.ID, open) {
		http.Error(w, "failed to reach device", http.StatusServiceUnavailable)
		return
	}
	user := h.currentUsername(r)
	shellSessions.Store(sid, shellSessionInfo{DeviceID: device.ID, Serial: device.SerialNumber, User: user, Opened: time.Now()})
	h.audit(r, "shell.session_open", device.SerialNumber, sid)
	defer func() {
		shellSessions.Delete(sid)
		closeMsg, _ := json.Marshal(map[string]any{"type": "shell_close", "session_id": sid})
		h.hub.Push(device.ID, closeMsg)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	sess, _ := json.Marshal(map[string]string{"id": sid})
	fmt.Fprintf(w, "event: session\ndata: %s\n\n", sess)
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

// maxShellInput bounds one write into a session.
const maxShellInput = 8 << 10

// ShellSessionInput writes what the user typed into their open session.
func (h *Handler) ShellSessionInput(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	v, ok := shellSessions.Load(sid)
	if !ok {
		http.Error(w, "That session has ended. Open a new one.", http.StatusGone)
		return
	}
	info := v.(shellSessionInfo)
	if info.Serial != r.PathValue("serial") || info.User != h.currentUsername(r) {
		http.Error(w, "Not your session.", http.StatusForbidden)
		return
	}
	data := r.FormValue("data")
	if len(data) > maxShellInput {
		http.Error(w, "Too much input at once.", http.StatusRequestEntityTooLarge)
		return
	}
	msg, _ := json.Marshal(map[string]any{"type": "shell_input", "session_id": sid, "data": data})
	if !h.hub.Push(info.DeviceID, msg) {
		http.Error(w, "Device not reachable.", http.StatusServiceUnavailable)
		return
	}
	if line := strings.TrimSpace(data); line != "" {
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		h.audit(r, "shell.input", info.Serial, line)
	}
	w.WriteHeader(http.StatusNoContent)
}

// shellQueueItem is one shell command sent through the device's queue, for the console's
// "Queued commands" tab.
type shellQueueItem struct {
	ID        uuid.UUID
	Cmd       string
	Status    string
	Output    string
	CreatedAt time.Time
}

// shellQueue lists the device's most recent queued shell commands, newest first.
func (h *Handler) shellQueue(r *http.Request, deviceID uuid.UUID) []shellQueueItem {
	cmds, err := h.db.GetDeviceCommands(r.Context(), deviceID, h.cfg.CheckinInterval()*3)
	if err != nil {
		return nil
	}
	var out []shellQueueItem
	for _, c := range cmds {
		if c.Type != "shell" {
			continue
		}
		var p struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		out = append(out, shellQueueItem{ID: c.ID, Cmd: p.Cmd, Status: c.Status, Output: c.Output, CreatedAt: c.CreatedAt})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
