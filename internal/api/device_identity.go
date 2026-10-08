package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/middleware"
	"mdm/internal/ratelimit"
)

// Device identity, phase 1 of the device-key plan (static/device-key-plan.html).
//
// The firmware client's shared key is public (it ships in the public client source),
// so the shared key proves nothing about which device is calling. Two things follow
// without any client change:
//
//   - A serial that has its own per-device key no longer accepts the shared key, on
//     any device endpoint including the WebSocket. Each device that migrates to its own
//     key is closed to impersonation from that moment.
//   - The same serial connected from two addresses at once raises an alert: a device
//     only ever holds one socket, so a second one elsewhere is someone using its serial.

// ownKeyTTL bounds how stale the "has its own key" answer may be. Keys are issued
// rarely; a minute of lag only delays the lock for a freshly migrated device.
const ownKeyTTL = time.Minute

var ownKeyCache struct {
	sync.Mutex
	m map[string]ownKeyEntry
}

type ownKeyEntry struct {
	has bool
	at  time.Time
}

func (h *Handler) serialHasOwnKey(ctx context.Context, serial string) bool {
	ownKeyCache.Lock()
	if e, ok := ownKeyCache.m[serial]; ok && time.Since(e.at) < ownKeyTTL {
		ownKeyCache.Unlock()
		return e.has
	}
	ownKeyCache.Unlock()
	has, err := h.db.DeviceHasOwnKey(ctx, serial)
	if err != nil {
		return false // unknown serial or DB trouble: the other checks still apply
	}
	ownKeyCache.Lock()
	if ownKeyCache.m == nil {
		ownKeyCache.m = map[string]ownKeyEntry{}
	}
	ownKeyCache.m[serial] = ownKeyEntry{has, time.Now()}
	ownKeyCache.Unlock()
	return has
}

// requireDeviceIdentity decides whether the caller may act as serial. A per-device key
// must name that serial; the shared key is accepted only for serials that have no key
// of their own. Writes a 403 and returns false otherwise.
func (h *Handler) requireDeviceIdentity(w http.ResponseWriter, r *http.Request, serial string) bool {
	// No Retry-After: a real outage doesn't send one, and a client told to wait the
	// whole outage wakes once and keeps one reading instead of one every 5 minutes.
	if _, ok := simulatedOffline(serial); ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return false
	}
	if bound := middleware.BoundSerial(r); bound != "" {
		if bound != serial {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "serial does not match device credential"})
			return false
		}
		h.noteOwnKeyIP(r.Context(), serial, ratelimit.ClientIP(r))
		return true
	}
	if serial != "" && h.serialHasOwnKey(r.Context(), serial) {
		ip := ratelimit.ClientIP(r)
		if h.recoverLostKey(r.Context(), serial, ip) {
			return true
		}
		log.Printf("[device-auth] refused the shared key for %s (it has its own key) from %s", serial, ip)
		h.raiseIdentityAlert(r.Context(), serial, "The shared device key was used for this device, which has its own key", ip, "")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this device uses its own key"})
		return false
	}
	return true
}

// wsPeers remembers the address of each device's current socket, and when the device
// last moved its socket from one address to another while the old one was still open.
var wsPeers struct {
	sync.Mutex
	ip    map[uuid.UUID]string
	flips map[uuid.UUID][]time.Time
}

// A device that changes networks (Wi-Fi to another uplink, a dual-WAN site switching
// lines) opens its new socket before the server notices the old one is dead, so one
// move between addresses is normal. Two devices using the same serial are different:
// each new socket makes the server drop the other, which reconnects within seconds, so
// the serial keeps flipping between two addresses. Alert on that, not on a single move.
const (
	socketFlipWindow = 10 * time.Minute
	socketFlipAlert  = 3
)

// noteSocketPeer records a new socket's address and alerts when the device keeps
// moving between addresses while its previous socket is still open.
func (h *Handler) noteSocketPeer(ctx context.Context, deviceID uuid.UUID, serial, ip string) {
	wsPeers.Lock()
	if wsPeers.ip == nil {
		wsPeers.ip = map[uuid.UUID]string{}
		wsPeers.flips = map[uuid.UUID][]time.Time{}
	}
	prev := wsPeers.ip[deviceID]
	wsPeers.ip[deviceID] = ip
	flips := 0
	if prev != "" && prev != ip && h.hub.IsConnected(deviceID) {
		var kept []time.Time
		for _, t := range wsPeers.flips[deviceID] {
			if time.Since(t) < socketFlipWindow {
				kept = append(kept, t)
			}
		}
		kept = append(kept, time.Now())
		wsPeers.flips[deviceID] = kept
		flips = len(kept)
	}
	wsPeers.Unlock()
	if flips == 0 {
		return
	}
	log.Printf("[device-auth] %s connected from %s while connected from %s (%d in %s)", serial, ip, prev, flips, socketFlipWindow)
	if flips >= socketFlipAlert {
		h.raiseIdentityAlert(ctx, serial, fmt.Sprintf("Connected from two addresses in turn, %d times in %d minutes", flips, int(socketFlipWindow.Minutes())), ip, prev)
	}
}

// identityAlertEvery keeps a repeated refusal or double connection to one alert per
// device per quarter hour.
const identityAlertEvery = 15 * time.Minute

var identityAlerted sync.Map // serial -> time.Time

// raiseIdentityAlert opens (or refreshes) an "identity_conflict" alert on the device
// and notifies the alert channels.
func (h *Handler) raiseIdentityAlert(ctx context.Context, serial, what, ip, prevIP string) {
	if t, ok := identityAlerted.Load(serial); ok && time.Since(t.(time.Time)) < identityAlertEvery {
		return
	}
	identityAlerted.Store(serial, time.Now())
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		dev, err := h.db.GetDevice(ctx, serial)
		if err != nil || dev == nil {
			return
		}
		summary := fmt.Sprintf("Possible impersonation: %s (from %s", what, ip)
		if prevIP != "" {
			summary += " and " + prevIP
		}
		summary += ")"
		detail := map[string]any{"ip": ip, "previous_ip": prevIP, "what": what}
		created, err := h.db.CreateAlertIfAbsent(ctx, nil, "identity_conflict", dev.ID, "critical", summary, detail)
		if err != nil {
			log.Printf("[device-auth] alert for %s: %v", serial, err)
			return
		}
		if created {
			h.alerts.Dispatch(ctx, []db.AlertNotification{{Type: "identity_conflict", Severity: "critical", Summary: summary,
				Serial: serial, DeviceID: dev.ID, EventAt: time.Now().UTC()}})
		}
		h.hub.PublishAlertUpdate()
	}()
}

// ── A device that lost its own key ──────────────────────────────────────────────
//
// A device can lose its key without anyone doing anything wrong: 1.6.0/1.6.1 kept it
// encrypted with an Android Keystore key, and a firmware update made that unreadable on
// three devices on 30 Sep; clearing the client's data loses it too; and in this fleet
// the commonest cause by far is that we reflashed it. The device then calls with the
// shared key, which Phase 1 refuses for a serial that has its own key.
//
// Until 2026-10-08 it was let back in only after three refusals, from the address it
// last used its key from, once a day — otherwise locked out until an admin pressed
// Reset. The user asked for the opposite default: let it straight back in, from
// anywhere, on the first call, and raise an alert so a person can look. A device in a
// restaurant that is dark because of a check we could not act on costs more than an
// alert that turns out to be a reflash. The only gate left is a per-device ceiling on
// how often this can happen in a day, past which the old refusal and the critical
// "Possible impersonation" alert come back — the one case where "let it in" would be
// a loop or a real attack.
//
// What the shared key can and cannot do is unchanged by this: it gets a serial's
// record back onto the shared key, where the next check-in registers a new per-device
// key; it does not read anything from the device or the server.

const (
	lostKeyWindow = 24 * time.Hour
	lostKeyMaxDay = 6 // auto-resets per device per day before the refusal returns
)

var ownKeyIPs sync.Map // serial -> ip last written, to skip the database when unchanged

func (h *Handler) noteOwnKeyIP(ctx context.Context, serial, ip string) {
	if ip == "" {
		return
	}
	if v, ok := ownKeyIPs.Load(serial); ok && v.(string) == ip {
		return
	}
	if err := h.db.NoteOwnKeyIP(ctx, serial, ip); err == nil {
		ownKeyIPs.Store(serial, ip)
	}
}

var lostKeyCallsSeen struct {
	sync.Mutex
	m map[string][]time.Time
}

// recoverLostKey forgets a device's own key on a refused shared-key call so the device
// comes straight back, and raises a warning saying so. Reports whether the call may go
// on; false means the daily ceiling is spent (or the write failed) and the caller
// refuses and alerts as a possible impersonation instead.
func (h *Handler) recoverLostKey(ctx context.Context, serial, ip string) bool {
	lostKeyCallsSeen.Lock()
	if lostKeyCallsSeen.m == nil {
		lostKeyCallsSeen.m = map[string][]time.Time{}
	}
	var kept []time.Time
	for _, t := range lostKeyCallsSeen.m[serial] {
		if time.Since(t) < lostKeyWindow {
			kept = append(kept, t)
		}
	}
	n := len(kept)
	if n >= lostKeyMaxDay {
		lostKeyCallsSeen.m[serial] = kept
		lostKeyCallsSeen.Unlock()
		log.Printf("[device-key] %s: %d automatic key resets in %s already — not resetting again", serial, n, lostKeyWindow)
		return false
	}
	kept = append(kept, time.Now())
	lostKeyCallsSeen.m[serial] = kept
	lostKeyCallsSeen.Unlock()
	ok, err := h.db.AutoResetDeviceKeyAnywhere(ctx, serial)
	if err != nil || !ok {
		return false
	}
	forgetOwnKey(serial)
	log.Printf("[device-key] %s used the shared key from %s; its own key was reset automatically (%d of %d today) so it comes back online", serial, ip, n+1, lostKeyMaxDay)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		dev, err := h.db.GetDevice(ctx, serial)
		if err != nil || dev == nil {
			return
		}
		summary := fmt.Sprintf("Key reset automatically: the shared key was used for this device from %s, which had its own key. Usually a reflash or cleared data — it is back online and registers a new key at its next check-in. Look into it if nobody touched this device.", ip)
		detail := map[string]any{"ip": ip, "auto_resets_24h": n + 1}
		created, err := h.db.CreateAlertIfAbsent(ctx, nil, "key_recovered", dev.ID, "warning", summary, detail)
		if err == nil && created {
			h.alerts.Dispatch(ctx, []db.AlertNotification{{Type: "key_recovered", Severity: "warning", Summary: summary,
				Serial: serial, DeviceID: dev.ID, EventAt: time.Now().UTC()}})
		}
		// Any impersonation alert still open on it was this same device coming back.
		_, _ = h.db.ResolveOpenAlert(ctx, "identity_conflict", dev.ID)
		h.hub.PublishAlertUpdate()
	}()
	return true
}

// ── Simulated outage, for testing ───────────────────────────────────────────────
//
// POST /api/v1/devices/{serial}/simulate-offline?minutes=N (admin) makes this server
// refuse one device for N minutes: every device endpoint answers 503 and its socket is
// dropped, so its client behaves exactly as in a real outage (1.7.0 keeps readings and
// sends them afterwards). It ends by itself; nothing on the device changes. Added 1 Oct
// because a system app can't switch its own device's network off to test that.

var simOffline sync.Map // serial -> time.Time

func simulatedOffline(serial string) (time.Time, bool) {
	v, ok := simOffline.Load(serial)
	if !ok {
		return time.Time{}, false
	}
	until := v.(time.Time)
	if time.Now().After(until) {
		simOffline.Delete(serial)
		return time.Time{}, false
	}
	return until, true
}

func (h *Handler) SimulateOffline(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	mins, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if mins < 0 || mins > 120 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "minutes must be 0 to 120 (0 ends it)"})
		return
	}
	dev, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
		return
	}
	if mins == 0 {
		simOffline.Delete(serial)
		log.Printf("[simulate-offline] %s: ended", serial)
		writeJSON(w, http.StatusOK, map[string]any{"serial": serial, "offline": false})
		return
	}
	until := time.Now().Add(time.Duration(mins) * time.Minute)
	simOffline.Store(serial, until)
	h.hub.Close(dev.ID)
	log.Printf("[simulate-offline] %s refused until %s", serial, until.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"serial": serial, "offline_until": until.UTC()})
}
