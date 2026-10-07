package api

import (
	"log"
	"net/http"
	"strings"

	"mdm/internal/adbtunnel"
	"mdm/internal/ratelimit"
	"mdm/internal/ws"
)

// SetAdbTunnels wires the tunnel manager (kept off NewHandler's already long signature).
func (h *Handler) SetAdbTunnels(m *adbtunnel.Manager) { h.tunnels = m }

// ConnectAdbTunnel is the device's leg of one adb tunnel stream
// (GET /api/v1/adb-tunnel/{session}/{stream}?serial=…, device auth). The server asked
// for it with an adb_tunnel_open frame on the command socket; the agent dialled
// 127.0.0.1:5555 and now carries that socket here as binary frames. The handler blocks
// until the stream ends on either side. See internal/adbtunnel.
func (h *Handler) ConnectAdbTunnel(w http.ResponseWriter, r *http.Request) {
	if h.tunnels == nil || !h.tunnels.Enabled() {
		http.Error(w, "adb tunnels are not enabled", http.StatusNotFound)
		return
	}
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" || !validSerial(serial) {
		http.Error(w, errInvalidSerial, http.StatusBadRequest)
		return
	}
	if !h.requireDeviceIdentity(w, r, serial) {
		return
	}
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	sessionID, streamID := r.PathValue("session"), r.PathValue("stream")
	s, ok := h.tunnels.Get(sessionID)
	if !ok || s.DeviceID != device.ID {
		// A session that is gone (or another device's) gets a 404 so the agent drops
		// the attempt instead of retrying.
		http.Error(w, "no such tunnel session", http.StatusNotFound)
		return
	}
	upgrader := ws.Upgrader()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[adbtunnel] upgrade failed for %s from %s: %v", serial, ratelimit.ClientIP(r), err)
		return
	}
	done, err := h.tunnels.AttachDevice(sessionID, streamID, device.ID, conn)
	if err != nil {
		_ = conn.WriteMessage(1, []byte(`{"error":"`+err.Error()+`"}`))
		conn.Close()
		return
	}
	<-done
	conn.Close()
}
