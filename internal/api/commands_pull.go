package api

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"mdm/internal/command"
	"mdm/internal/ratelimit"
)

// PendingCommands is the pull side of command delivery: GET /api/v1/commands/pending.
//
// Until this existed, every delivery path needed the server to hold a working socket to
// the device — push at creation, flush on connect, and two per-minute re-push sweeps that
// skip anything not currently connected. A device whose socket had died therefore could
// not be commanded at all, indefinitely, while its HTTP check-ins kept arriving and
// everything on the dashboard looked alive (AT070AABU00077, 9 Oct 2026: 15 hours, with a
// shell command sitting at 'delivered').
//
// Inverting it fixes the class rather than the instance: the device asks, because the
// device is the only party that always knows whether it can be reached. A socket then only
// decides how soon it asks, not whether it can be told anything.
//
// What is next is NOT decided here — this serves exactly what GetPendingCommandsForDevice
// decides, so the one-hour window, the per-device FIFO gate and the OTA exemptions stay in
// one place and both transports obey them identically.
func (h *Handler) PendingCommands(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial query parameter required"})
		return
	}
	if !validSerial(serial) {
		h.noteRefusedSerial(serial, ratelimit.ClientIP(r), "")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if h.deviceRateLimited(w, serial) {
		return
	}
	if !h.requireDeviceIdentity(w, r, serial) {
		return
	}
	ctx := r.Context()
	device, err := h.db.GetDevice(ctx, serial)
	if err != nil || device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	// A client only polls when it believes it has no usable socket, so a poll is evidence
	// about this device's command channel — and the one kind of evidence that arrives even
	// when nothing else does. Record it, which also resolves the alert when it recovers.
	h.noteCommandChannel(ctx, device.ID, serial, device.LatestExtra)

	out := make([]map[string]any, 0, 4)
	cmds, err := h.db.GetPendingCommandsForDevice(ctx, device.ID)
	if err != nil {
		log.Printf("[pull] %s GetPendingCommandsForDevice: %v", serial, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for _, cmd := range cmds {
		spec := command.For(cmd.Type)
		// A live session (screen capture) is meaningless off a socket: handing it to a
		// polling device would start a capture with nowhere to send frames.
		if spec.LiveOnly {
			continue
		}
		// The lease is the type's full work-time budget, not the short push lease: this
		// hand-over is confirmed by the 200 the device is about to receive, where a WS
		// push is only confirmed by a 'received' ack that may never come.
		if _, err := h.db.LeaseCommand(ctx, cmd.ID, device.ID, spec.Lease, "http"); err != nil {
			log.Printf("[pull] %s lease %s: %v", serial, cmd.ID, err)
			continue
		}
		out = append(out, map[string]any{
			"id": cmd.ID,
			// Both key names: "command_type" is what the WS frame uses, so a client can
			// feed this straight into the handler it already has; "type" is what the DPC
			// agent reads first (CommandExecutor.kt) and what older clients know.
			"type":         deviceCommandType(cmd.Type),
			"command_type": deviceCommandType(cmd.Type),
			"apk_url":      cmd.ApkURL,
			"payload":      cmd.Payload,
		})
		h.hub.PublishCommandUpdate(cmd.ID)
		if cmd.Type == "reboot" {
			// A reboot ends this device's attention: anything after it would be handed to a
			// device that is about to go away, and would be re-delivered anyway.
			break
		}
	}
	if len(out) > 0 {
		log.Printf("[pull] %s took %d command(s) over HTTP", serial, len(out))
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": out})
}

// leaseForPush takes the short, unconfirmed lease that a WebSocket delivery gets. Kept
// next to the pull path so the two halves of "who holds this command" are read together.
func (h *Handler) leaseForPush(ctx context.Context, commandID, deviceID uuid.UUID) {
	if _, err := h.db.LeaseCommand(ctx, commandID, deviceID, command.PushLease, "ws"); err != nil {
		log.Printf("[push] lease %s for %s: %v", commandID, deviceID, err)
	}
}

// releaseLeasesOnConnect makes a reconnecting device's commands immediately deliverable
// again. A fresh socket is proof that whatever was pushed down the old one is lost, which
// is the assumption the WS flush has always been built on — without this, a device that
// reconnects would have to wait out a lease taken for a delivery that can no longer land.
func (h *Handler) releaseLeasesOnConnect(ctx context.Context, deviceID uuid.UUID, serial string) {
	n, err := h.db.ReleaseDeviceLeases(ctx, deviceID)
	if err != nil {
		log.Printf("[push] release leases for %s: %v", serial, err)
		return
	}
	if n > 0 {
		log.Printf("[push] %s reconnected — released %d held command lease(s)", serial, n)
	}
}
