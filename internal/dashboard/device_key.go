package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
)

// Device keys on the dashboard (device-key plan; the device-key-reset demo, option A).
// The Enrollment card says which key a device uses, and an admin can reset a firmware
// device's own key when the device has lost it (factory reset, data cleared) and is being
// refused. Until it registers again the shared key works for it, so a reset that is not
// followed by a registration within the hour raises a warning.

// SetKeyResetHook lets the device API drop its cached "has its own key" answer after a
// reset, so the device is not refused for the cache's lifetime.
func (h *Handler) SetKeyResetHook(f func(serial string)) { h.onKeyReset = f }

// keyRegisterClient is the first firmware client that registers its own key.
const keyRegisterClient = "1.6.0"

// keyCredential is the Enrollment card's Credential row for a firmware device.
type keyCredential struct {
	Own        bool
	Since      *time.Time
	ResetAt    *time.Time
	CanReg     bool   // its client registers a key by itself
	Client     string // the client version it reports
}

func (h *Handler) keyCredentialFor(ctx context.Context, dev *db.Device) *keyCredential {
	if dev == nil || dev.IsDPC() {
		return nil
	}
	st, err := h.db.GetDeviceKeyState(ctx, dev.ID)
	if err != nil {
		return nil
	}
	c := &keyCredential{Own: st.Own, Since: st.Since, ResetAt: st.ResetAt}
	c.Client = strings.TrimSpace(extraString(dev.LatestExtra, "agent_version"))
	c.CanReg = c.Client != "" && !versionNewer(keyRegisterClient, c.Client)
	if c.ResetAt != nil {
		c.CanReg = true // it had a key of its own, so its client registers one
	}
	return c
}

// DeviceKeyReset is POST /devices/{serial}/key-reset (admin).
func (h *Handler) DeviceKeyReset(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	why := map[string]string{"wiped": "factory reset or data cleared", "reflashed": "firmware reflashed", "other": "other"}[r.FormValue("why")]
	if why == "" {
		why = "other"
	}
	if note := strings.TrimSpace(r.FormValue("note")); note != "" {
		if len(note) > 200 {
			note = note[:200]
		}
		why += ": " + note
	}
	cleared, err := h.db.ResetDeviceKey(r.Context(), serial)
	if err != nil {
		h.hxDoneToast(w, r, "/devices/"+serial, "Could not reset the key", "error")
		return
	}
	if !cleared {
		h.hxDoneToast(w, r, "/devices/"+serial, "This device has no key of its own to reset", "error")
		return
	}
	if h.onKeyReset != nil {
		h.onKeyReset(serial)
	}
	h.audit(r, "device.key_reset", serial, why)
	h.hxDoneToast(w, r, "/devices/"+serial, "Key reset. It registers a new one at its next check-in", "ok")
}

// keyResetGrace is how long a reset device has to register before it is flagged.
const keyResetGrace = time.Hour

// checkKeyResets warns about devices whose key was reset and that have not registered a
// new one within the grace period; the shared key still works for them meanwhile.
func (h *Handler) checkKeyResets(ctx context.Context) {
	overdue, err := h.db.OverdueKeyResets(ctx, keyResetGrace)
	if err != nil || len(overdue) == 0 {
		return
	}
	var created []db.AlertNotification
	for id, serial := range overdue {
		summary := fmt.Sprintf("%s has not registered a new key an hour after its key was reset; the shared key still works for it", serial)
		ok, err := h.db.CreateAlertIfAbsent(ctx, nil, "key_not_registered", id, "warning", summary, map[string]any{"serial": serial})
		if err != nil {
			log.Printf("[device-key] overdue alert for %s: %v", serial, err)
			continue
		}
		if ok {
			created = append(created, db.AlertNotification{Type: "key_not_registered", Severity: "warning", Summary: summary,
				Serial: serial, DeviceID: id, EventAt: time.Now().UTC()})
		}
	}
	if len(created) > 0 {
		h.dispatchAlertNotifications(ctx, created)
		h.hub.PublishAlertUpdate()
	}
}
