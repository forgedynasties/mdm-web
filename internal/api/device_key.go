package api

import (
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"mdm/internal/middleware"
	"mdm/internal/product"
	"mdm/internal/ratelimit"
)

// Device keys, phase 2 of the device-key plan (static/device-key-plan.html).
//
// A firmware client (1.6.0+) generates its own key on the device and registers it here
// with the shared key, sending only the key's SHA-256. The server accepts one claim per
// serial — the first; after it Phase 1 refuses the shared key for that serial — and only
// when the claim agrees with what it already knows about the device:
//
//   - the device is a firmware device the server has seen;
//   - the caller is the device's current connection: its live socket, or a check-in in
//     the last few minutes, from the same address;
//   - the boot id and build it reports are the ones the device last reported.
//
// A mismatch is refused with 409 so an honest client retries after its next check-in (a
// reboot or an OTA can race the report); repeated mismatches for one serial raise the
// impersonation alert. A device that lost its key is let back in by an admin reset.

// checkinPeers remembers where each serial last checked in over HTTP.
var checkinPeers struct {
	sync.Mutex
	m map[string]peerSeen
}

type peerSeen struct {
	ip string
	at time.Time
}

const checkinPeerFresh = 5 * time.Minute

func noteCheckinPeer(serial, ip string) {
	checkinPeers.Lock()
	if checkinPeers.m == nil {
		checkinPeers.m = map[string]peerSeen{}
	}
	checkinPeers.m[serial] = peerSeen{ip, time.Now()}
	checkinPeers.Unlock()
}

// keyClaimMisses counts refused claims per serial; the third within the window alerts.
var keyClaimMisses struct {
	sync.Mutex
	m map[string][]time.Time
}

const keyClaimMissWindow = 15 * time.Minute

func noteKeyClaimMiss(serial string) int {
	keyClaimMisses.Lock()
	defer keyClaimMisses.Unlock()
	if keyClaimMisses.m == nil {
		keyClaimMisses.m = map[string][]time.Time{}
	}
	var kept []time.Time
	for _, t := range keyClaimMisses.m[serial] {
		if time.Since(t) < keyClaimMissWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, time.Now())
	keyClaimMisses.m[serial] = kept
	return len(kept)
}

// ForgetOwnKey drops the cached "has its own key" answer for a serial (the dashboard
// calls it after resetting a key, so the device can check in again at once).
func (h *Handler) ForgetOwnKey(serial string) { forgetOwnKey(serial) }

func forgetOwnKey(serial string) {
	ownKeyCache.Lock()
	delete(ownKeyCache.m, serial)
	ownKeyCache.Unlock()
}

// RegisterDeviceKey is POST /api/v1/device-key.
func (h *Handler) RegisterDeviceKey(w http.ResponseWriter, r *http.Request) {
	if middleware.BoundSerial(r) != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already using its own key"})
		return
	}
	var req struct {
		Serial    string `json:"serial"`
		KeySHA256 string `json:"key_sha256"`
		BootID    string `json:"boot_id"`
		BuildID   string `json:"build_id"`
	}
	if err := decodeDeviceJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.Serial = strings.TrimSpace(req.Serial)
	req.KeySHA256 = strings.ToLower(strings.TrimSpace(req.KeySHA256))
	if raw, err := hex.DecodeString(req.KeySHA256); err != nil || len(raw) != 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key_sha256 must be 64 hex characters"})
		return
	}
	if !validSerial(req.Serial) || len(req.Serial) > maxSerialLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if h.deviceRateLimited(w, req.Serial) {
		return
	}
	ctx := r.Context()
	ip := ratelimit.ClientIP(r)
	claim, err := h.db.GetDeviceKeyClaim(ctx, req.Serial)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device: check in first"})
		return
	}
	if claim.AgentKind != product.KindFirmware {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only firmware devices register a key here; others enroll"})
		return
	}
	if claim.HasKey && subtle.ConstantTimeCompare([]byte(claim.KeyHash), []byte(req.KeySHA256)) == 1 {
		// A retry of a registration whose reply was lost: same key, already stored.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if claim.HasKey {
		log.Printf("[device-key] %s already has its own key; claim from %s refused", req.Serial, ip)
		h.raiseIdentityAlert(ctx, req.Serial, "Someone tried to register a new key for this device, which already has one", ip, "")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device already has its own key"})
		return
	}
	refuse := func(why string) {
		n := noteKeyClaimMiss(req.Serial)
		log.Printf("[device-key] %s claim from %s refused (%s), %d in %s", req.Serial, ip, why, n, keyClaimMissWindow)
		if n >= 3 {
			h.raiseIdentityAlert(ctx, req.Serial, "Repeated key registrations that don't match the device ("+why+")", ip, "")
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": why + "; check in and try again"})
	}
	// The caller must be the device's current connection.
	wsPeers.Lock()
	wsIP := wsPeers.ip[claim.ID]
	wsPeers.Unlock()
	checkinPeers.Lock()
	seen := checkinPeers.m[req.Serial]
	checkinPeers.Unlock()
	onSocket := wsIP == ip && h.hub.IsConnected(claim.ID)
	recentCheckin := seen.ip == ip && time.Since(seen.at) < checkinPeerFresh
	if !onSocket && !recentCheckin {
		refuse("not the device's current connection")
		return
	}
	if claim.LastBootID != "" && req.BootID != claim.LastBootID {
		refuse("boot id differs from the device's last report")
		return
	}
	if claim.BuildID != "" && req.BuildID != claim.BuildID {
		refuse("build differs from the device's last report")
		return
	}
	stored, afterReset, err := h.db.ClaimDeviceKey(ctx, req.Serial, req.KeySHA256)
	if err != nil {
		log.Printf("[device-key] %s: %v", req.Serial, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !stored {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device already has its own key"})
		return
	}
	forgetOwnKey(req.Serial)
	log.Printf("[device-key] %s registered its own key from %s", req.Serial, ip)
	// Back after an admin reset: the refusals it raised while key-less were it, and the
	// "hasn't registered" warning is answered.
	_, _ = h.db.ResolveOpenAlert(ctx, "key_not_registered", claim.ID)
	_, _ = h.db.ResolveOpenAlert(ctx, "key_recovered", claim.ID)
	if afterReset {
		_, _ = h.db.ResolveOpenAlert(ctx, "identity_conflict", claim.ID)
	}
	h.hub.PublishDeviceUpdate(claim.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ResetDeviceKey is POST /api/v1/devices/{serial}/key-reset (admin): forget a device's
// own key so it can register a new one.
func (h *Handler) ResetDeviceKey(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	cleared, err := h.db.ResetDeviceKey(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !cleared {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no firmware device with its own key by that serial"})
		return
	}
	forgetOwnKey(serial)
	log.Printf("[device-key] %s key reset by admin API", serial)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "serial": serial})
}
