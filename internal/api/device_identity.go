package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
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
	if bound := middleware.BoundSerial(r); bound != "" {
		if bound != serial {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "serial does not match device credential"})
			return false
		}
		return true
	}
	if serial != "" && h.serialHasOwnKey(r.Context(), serial) {
		ip := ratelimit.ClientIP(r)
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
