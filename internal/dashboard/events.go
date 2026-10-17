package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"time"

	"mdm/internal/db"
	"mdm/internal/ws"

	"github.com/google/uuid"
)

// DeviceOnlineStatus returns a tiny HTML fragment with the current connection state.
// Kept for initial page render; live updates come from DevicePresenceStream.
func (h *Handler) DeviceOnlineStatus(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	online := h.hub.IsConnectedForDisplay(device.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if online {
		fmt.Fprint(w, `<span id="ws-badge" class="ws-badge online" title="WebSocket connected"><span class="ws-dot"></span>online</span>`)
	} else {
		fmt.Fprint(w, `<span id="ws-badge" class="ws-badge offline" title="No WebSocket connection"><span class="ws-dot"></span>offline</span>`)
	}
}

// DevicePresenceStream streams SSE events for a single device's connection state.
// Emits an initial snapshot, then pushes "online"/"offline" on change.
func (h *Handler) DevicePresenceStream(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		fmt.Fprintf(w, "event: presence\ndata: %s\n\n", state)
		flusher.Flush()
	}

	writeEvent(h.hub.IsConnectedForDisplay(device.ID))

	sub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// A real disconnect doesn't push "offline" straight away — it arms a
	// presenceGrace timer instead, so a brief blip never flashes the badge.
	// Reconnecting within the window cancels the timer. Going offline pushes
	// only if the device is still disconnected once the timer fires.
	var offlineTimer *time.Timer
	fireOffline := make(chan struct{}, 1)
	defer func() {
		if offlineTimer != nil {
			offlineTimer.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if ev.DeviceID != device.ID {
				continue
			}
			if ev.Online {
				if offlineTimer != nil {
					offlineTimer.Stop()
					offlineTimer = nil
				}
				writeEvent(true)
			} else if offlineTimer == nil {
				offlineTimer = time.AfterFunc(ws.PresenceGrace, func() {
					select {
					case fireOffline <- struct{}{}:
					default:
					}
				})
			}
		case <-fireOffline:
			offlineTimer = nil
			if !h.hub.IsConnected(device.ID) {
				writeEvent(false)
			}
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// FleetEvents streams per-device SSE updates for the fleet dashboard.
// Each event carries a JSON payload so the browser can patch only the affected row.
func (h *Handler) FleetEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	activeThreshold := time.Duration(h.cfg.CheckinInterval()*3) * time.Second

	acc := h.access(r)
	sendRow := func(deviceID uuid.UUID) {
		if !acc.visible(deviceID) {
			return
		}
		dev, err := h.db.GetDeviceByID(r.Context(), deviceID)
		if err != nil {
			return
		}
		row := deviceToRowJSON(*dev, h.hub.IsConnectedForDisplay(deviceID), activeThreshold)
		// The live row-patch repaints the battery chip, so it must carry the flap state
		// too — otherwise it overwrites the server-rendered fault glyph with a plain bolt.
		if rate, _ := h.db.DeviceChargerFlapRate(r.Context(), deviceID, 5); rate > 10 {
			row.Flapping, row.FlapRate = true, rate
		}
		b, err := json.Marshal(row)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: device-update\ndata: %s\n\n", b)
		flusher.Flush()
	}

	presenceSub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(presenceSub)
	updateSub := h.hub.SubscribeDeviceUpdates()
	defer h.hub.UnsubscribeDeviceUpdates(updateSub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// A row's own SSE patch reflects IsConnectedForDisplay already, so a disconnect
	// event itself is harmless (the grace window holds it "online"). But nothing
	// else re-patches that row once the grace window lapses, so schedule one
	// grace-delayed re-send per device that goes offline, to actually flip it.
	offlineTimers := map[uuid.UUID]*time.Timer{}
	fireOffline := make(chan uuid.UUID, 64)
	defer func() {
		for _, t := range offlineTimers {
			t.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-presenceSub:
			if !ok {
				return
			}
			sendRow(ev.DeviceID)
			if ev.Online {
				if t, ok := offlineTimers[ev.DeviceID]; ok {
					t.Stop()
					delete(offlineTimers, ev.DeviceID)
				}
			} else if _, ok := offlineTimers[ev.DeviceID]; !ok {
				id := ev.DeviceID
				offlineTimers[id] = time.AfterFunc(ws.PresenceGrace, func() {
					select {
					case fireOffline <- id:
					default:
					}
				})
			}
		case id := <-fireOffline:
			delete(offlineTimers, id)
			sendRow(id)
		case ev, ok := <-updateSub:
			if !ok {
				return
			}
			sendRow(ev.DeviceID)
		}
	}
}

type deviceEventPayload struct {
	TsMs       int64    `json:"ts_ms"`
	BuildID    string   `json:"build_id,omitempty"`
	BatteryPct int      `json:"battery_pct"`
	Wlc        *int     `json:"wlc"`      // nil = no data
	TempC      *float64 `json:"temp_c"`   // nil = no data
	RamPct     *float64 `json:"ram_pct"`  // nil = no data
	Charging   *bool    `json:"charging"` // nil = no data
	// ChargerFlapping is the firmware client's own flap flag (1.4.5+), so the live strip
	// turns yellow without waiting for a reload.
	ChargerFlapping bool     `json:"charger_flapping,omitempty"`
	ScreenOn        *bool    `json:"screen_on"` // nil = firmware does not report it
	Latitude        *float64 `json:"latitude,omitempty"`
	Longitude       *float64 `json:"longitude,omitempty"`
	// KioskEnabled is the device's current kiosk state (from device_config, not the
	// checkin) so the device page reflects an on-device offline exit live. Pointer so
	// it is only sent when the SSE writer looked it up.
	KioskEnabled *bool `json:"kiosk_enabled,omitempty"`
	// KioskSuspended rides the check-in: the device is out of lock-task after a local
	// unlock with the static exit PIN, while the stored config still says kiosk is on.
	// Reported so the page stops claiming a locked device the moment the client says
	// otherwise, rather than at the next full page load.
	KioskSuspended *bool `json:"kiosk_suspended,omitempty"`
}

func buildDeviceEventPayload(c *db.Checkin) deviceEventPayload {
	p := deviceEventPayload{
		TsMs:       c.CreatedAt.UnixMilli(),
		BuildID:    c.BuildID,
		BatteryPct: c.BatteryPct,
	}
	if len(c.Extra) > 0 {
		var extra map[string]json.RawMessage
		if json.Unmarshal(c.Extra, &extra) == nil {
			if v, ok := extra["wlc_status"]; ok {
				var n int
				if json.Unmarshal(v, &n) == nil {
					p.Wlc = &n
				}
			}
			if v, ok := extra["kiosk_suspended"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					p.KioskSuspended = &b
				}
			}
			if v, ok := extra["charging"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					p.Charging = &b
				}
			}
			p.ChargerFlapping = string(extra["charger_flapping"]) == "true"
			if v, ok := extra["screen_on"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					p.ScreenOn = &b
				}
			}
			if v, ok := extra["ram_usage_mb"]; ok {
				var ram map[string]int
				if json.Unmarshal(v, &ram) == nil && ram["total"] > 0 {
					pct := float64(ram["used"]) * 100 / float64(ram["total"])
					if pct > 100 {
						pct = 100
					}
					p.RamPct = &pct
				}
			}
			if v, ok := extra["latitude"]; ok {
				var lat float64
				if json.Unmarshal(v, &lat) == nil {
					p.Latitude = &lat
				}
			}
			if v, ok := extra["longitude"]; ok {
				var lon float64
				if json.Unmarshal(v, &lon) == nil {
					p.Longitude = &lon
				}
			}
		}
	}
	if temp, _, ok := deviceTempC(c.Extra); ok {
		p.TempC = &temp
	}
	return p
}

// CommandEvents streams SSE notifications for a command detail page.
func (h *Handler) CommandEvents(w http.ResponseWriter, r *http.Request) {
	cmdID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid command id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeCommandUpdates()
	defer h.hub.UnsubscribeCommandUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if ev.CommandID == cmdID {
				fmt.Fprint(w, "event: command-update\ndata: refresh\n\n")
				flusher.Flush()
			}
		}
	}
}

// CommandsFeedEvents streams SSE notifications for the action history page: it
// fires on every command update (no per-id filter), so an in-flight action that
// completes moves from "In progress" to "Completed" without a manual refresh —
// the same live behavior the device page has for its command list.
func (h *Handler) CommandsFeedEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeCommandUpdates()
	defer h.hub.UnsubscribeCommandUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: command-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// AlertEvents streams SSE notifications to the alerts page so an ack/resolve by
// any user refreshes every open alerts view in real time.
func (h *Handler) AlertEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeAlertUpdates()
	defer h.hub.UnsubscribeAlertUpdates(sub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()

	// emitCount sends the current open-alert count so every connected client (the
	// global nav badge and the alerts list) can update without each re-querying.
	hideIdentity := h.access(r).hidesIdentityAlerts()
	emitCount := func() {
		n, err := h.db.CountOpenAlerts(ctx, hideIdentity)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: alert-update\ndata: %d\n\n", n)
		flusher.Flush()
	}

	// Send the count once on connect so a freshly-loaded page syncs its badge
	// immediately rather than waiting for the next change.
	emitCount()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			emitCount()
		}
	}
}

// DeploymentEvents streams SSE notifications to open deployment pages so OTA
// progress and operator actions (retry/cancel/edit) refresh live without polling.
func (h *Handler) DeploymentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeDeploymentUpdates()
	defer h.hub.UnsubscribeDeploymentUpdates(sub)
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: deployment-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// ReleaseProblemEvents streams SSE notifications to the releases hub and any open
// release workspace, so the cross-release problems board and the workspace fragments
// refresh live when a problem (or a QA result that creates/resolves one) changes
// anywhere — no polling. Payload-less; each listener re-fetches its own scoped fragment.
func (h *Handler) ReleaseProblemEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub := h.hub.SubscribeProblemUpdates()
	defer h.hub.UnsubscribeProblemUpdates(sub)
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case _, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: problem-update\ndata: refresh\n\n")
			flusher.Flush()
		}
	}
}

// DeviceEvents streams SSE notifications for a single device detail page.
func (h *Handler) DeviceEvents(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writePresence := func(online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		fmt.Fprintf(w, "event: presence\ndata: %s\n\n", state)
		flusher.Flush()
	}
	writeDeviceUpdate := func() {
		// The devices row, not the newest check-in: it is written on every report,
		// while a history row is only stored when something changed or the sampling
		// window elapsed.
		c, err := h.db.GetDeviceLive(r.Context(), device.ID)
		if err != nil {
			fmt.Fprint(w, "event: device\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		p := buildDeviceEventPayload(c)
		// Carry live kiosk state so an on-device offline exit flips the toggle here
		// without a reload (kiosk_enabled lives in device_config, not the checkin).
		if cfg, cerr := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID); cerr == nil {
			ke := cfg.KioskEnabled
			p.KioskEnabled = &ke
		}
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "event: device\ndata: %s\n\n", b)
		flusher.Flush()
	}

	writePresence(h.hub.IsConnectedForDisplay(device.ID))
	writeDeviceUpdate()

	presenceSub := h.hub.SubscribePresence()
	defer h.hub.UnsubscribePresence(presenceSub)
	updateSub := h.hub.SubscribeDeviceUpdates()
	defer h.hub.UnsubscribeDeviceUpdates(updateSub)

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	// Same grace-debounce as DevicePresenceStream: a real disconnect arms a
	// presenceGrace timer instead of pushing "offline" straight away.
	var offlineTimer *time.Timer
	fireOffline := make(chan struct{}, 1)
	defer func() {
		if offlineTimer != nil {
			offlineTimer.Stop()
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-presenceSub:
			if !ok {
				return
			}
			if ev.DeviceID == device.ID {
				if ev.Online {
					if offlineTimer != nil {
						offlineTimer.Stop()
						offlineTimer = nil
					}
					writePresence(true)
				} else if offlineTimer == nil {
					offlineTimer = time.AfterFunc(ws.PresenceGrace, func() {
						select {
						case fireOffline <- struct{}{}:
						default:
						}
					})
				}
			}
		case <-fireOffline:
			offlineTimer = nil
			if !h.hub.IsConnected(device.ID) {
				writePresence(false)
			}
		case ev, ok := <-updateSub:
			if !ok {
				return
			}
			if ev.DeviceID == device.ID {
				writeDeviceUpdate()
			}
		}
	}
}

// CommandOutputStream is an SSE endpoint that streams live output for a
// specific (command, device) pair as the device sends it.
func (h *Handler) CommandOutputStream(w http.ResponseWriter, r *http.Request) {
	cmdID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid command id", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || !h.access(r).visible(device.ID) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch, unsub := h.shell.SubscribeCommandOutput(cmdID, device.ID)
	defer unsub()

	// A command can end without streaming anything: the firmware client streams stdout
	// but reports stderr (and the failure) only in its final acknowledgement, and a fast
	// command can finish before this stream subscribes. So the stream also watches the
	// stored outcome, and when the command is over it sends whatever output the stream
	// did not carry, then a done event that says how it ended.
	streamed := false
	finish := func() {
		// The ack can land a moment after the last chunk; give it a beat.
		// The firmware client marks the end over the socket and sends what it printed
		// on stderr in a separate HTTP ack, which can land seconds later. So once it is
		// over, keep waiting (up to 15s) for that output if none was streamed.
		var status, out string
		for i := 0; i < 30; i++ {
			status, out, _ = h.db.CommandOutcome(r.Context(), cmdID, device.ID)
			if isTerminalCommandStatus(status) && (streamed || out != "" || i >= 10 && status == "completed") {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		if !streamed && out != "" {
			data, _ := json.Marshal(out)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		done, _ := json.Marshal(map[string]string{"status": status})
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", done)
		flusher.Flush()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				finish()
				return
			}
			streamed = true
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-tick.C:
			if status, _, err := h.db.CommandOutcome(r.Context(), cmdID, device.ID); err == nil && isTerminalCommandStatus(status) {
				// Give in-flight chunks a moment to arrive before closing.
				time.Sleep(300 * time.Millisecond)
				for drained := false; !drained; {
					select {
					case chunk, open := <-ch:
						if !open {
							drained = true
							break
						}
						streamed = true
						data, _ := json.Marshal(chunk)
						fmt.Fprintf(w, "data: %s\n\n", data)
					default:
						drained = true
					}
				}
				finish()
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// isTerminalCommandStatus reports whether a device's command status is final.
func isTerminalCommandStatus(s string) bool {
	switch s {
	case "completed", "failed", "unsupported", "cancelled", "canceled", "expired", "rejected", "timeout":
		return true
	}
	return false
}
