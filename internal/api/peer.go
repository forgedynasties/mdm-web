package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/config"
)

// The peer endpoints: what a neighbouring MDM is allowed to ask this one. Three calls,
// all of them about where a serial was last seen, none of them able to change a device,
// create one, or make it do anything. That narrowness is the security model — the peer
// key opens this door and no other, which is why it is not the admin key.
//
// See internal/peers for the client half and why a push and a pull both exist.

// peerAuth identifies the calling peer by the key it presents, and answers it itself
// when it cannot. The key names the peer: there is no separate identity to spoof.
func (h *Handler) peerAuth(w http.ResponseWriter, r *http.Request) (config.Peer, bool) {
	key := strings.TrimSpace(r.Header.Get("X-Peer-Key"))
	p, ok := h.cfg.PeerByKey(key)
	if !ok {
		// Deliberately terse: an unknown key learns nothing about which keys exist.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown peer"})
		return config.Peer{}, false
	}
	return p, true
}

// PeerPing answers "are you there, and who are you?" — the handshake behind the test
// button, and the one call that proves a key works before anything depends on it.
func (h *Handler) PeerPing(w http.ResponseWriter, r *http.Request) {
	p, ok := h.peerAuth(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server": h.cfg.SelfName(),
		"peer":   p.Name,
		"now":    time.Now().UTC(),
	})
}

// peerArrival is the body of an announcement. It mirrors peers.Arrival; the two are
// deliberately separate types so the wire format is something both sides agree on
// rather than an internal struct that can drift.
type peerArrival struct {
	Serial  string    `json:"serial"`
	SeenAt  time.Time `json:"seen_at"`
	BuildID string    `json:"build_id"`
	Product string    `json:"product"`
	From    string    `json:"from"`
	SentAt  time.Time `json:"sent_at"`
}

// PeerDeviceSeen records that a peer has the device. The effect is bounded on purpose:
//
//   - an unknown serial is a 204, never a new device row — a peer cannot enroll
//     hardware into this fleet;
//   - a sighting older than our own last_seen is ignored — we heard from it more
//     recently, so it is ours;
//   - nothing but the custody columns is written.
func (h *Handler) PeerDeviceSeen(w http.ResponseWriter, r *http.Request) {
	p, ok := h.peerAuth(w, r)
	if !ok {
		return
	}
	var body peerArrival
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	serial := strings.TrimSpace(body.Serial)
	if serial == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial is required"})
		return
	}
	seen := body.SeenAt
	if seen.IsZero() {
		seen = time.Now().UTC()
	}
	// A peer whose clock is wildly ahead could otherwise plant a sighting that no
	// later local check-in can beat, pinning a device as "elsewhere" forever. Cap it
	// at now: a sighting cannot be in the future.
	if now := time.Now().UTC(); seen.After(now) {
		seen = now
	}
	id, _, found, err := h.db.DeviceIDBySerial(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !found {
		// Not ours. Nothing to record, and nothing to leak about what is.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	filed, err := h.db.SetDeviceCustody(r.Context(), id, p.Name, peerLink(p), seen, "peer")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if filed {
		if n, err := h.db.CancelPendingForCustody(r.Context(), id); err == nil && n > 0 {
			log.Printf("[peers] dropped %d queued command target(s) for %s", n, serial)
		}
		_ = h.db.InsertAudit(r.Context(), "peer:"+p.Name, "device.custody", serial,
			"reporting to "+p.Name+" as of "+seen.Format(time.RFC3339))
		log.Printf("[peers] %s reports %s (last seen there %s)", p.Name, serial, seen.Format(time.RFC3339))
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": filed})
}

// peerLookupRequest is a batch of serials a peer is asking about.
type peerLookupRequest struct {
	Serials []string `json:"serials"`
	From    string   `json:"from"`
}

// PeerLookup answers "have you seen any of these?" — the pull half, and the reason this
// design self-corrects. A peer that missed an announcement, was added later, or was
// pointed at hardware flashed in the factory still converges, because it asks.
//
// Only serials the caller already named are answered, so this cannot be used to
// enumerate a fleet: you must know a serial to learn anything about it.
func (h *Handler) PeerLookup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.peerAuth(w, r)
	if !ok {
		return
	}
	var body peerLookupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(body.Serials) > 500 {
		body.Serials = body.Serials[:500]
	}
	type seen struct {
		Serial  string    `json:"serial"`
		SeenAt  time.Time `json:"seen_at"`
		BuildID string    `json:"build_id,omitempty"`
	}
	out := make([]seen, 0, len(body.Serials))
	for _, s := range body.Serials {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		d, err := h.db.GetDevice(r.Context(), s)
		if err != nil || d == nil {
			continue
		}
		// Only report a device this server actually holds: one filed as being on
		// another server is not ours to vouch for, and answering with a stale
		// sighting would let two servers hand a device back and forth forever.
		if c, err := h.db.DeviceCustody(r.Context(), d.ID); err == nil && c.Elsewhere() {
			continue
		}
		out = append(out, seen{Serial: d.SerialNumber, SeenAt: d.LastSeenAt.UTC(), BuildID: d.BuildID})
	}
	_ = p
	writeJSON(w, http.StatusOK, map[string]any{
		"server":  h.cfg.SelfName(),
		"now":     time.Now().UTC(),
		"devices": out,
	})
}

// peerLink is where an operator is sent to see a device that lives on that peer.
func peerLink(p config.Peer) string {
	if strings.TrimSpace(p.DashboardURL) != "" {
		return strings.TrimRight(p.DashboardURL, "/")
	}
	return strings.TrimRight(p.URL, "/")
}
