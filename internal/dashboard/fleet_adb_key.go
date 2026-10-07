package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/fleetkey"
	"mdm/internal/ratelimit"
)

// The fleet adb key: the one key pair the AIO firmware trusts, handed to the AIO Enroll
// Android app after sign-in so it can adb-connect to firmware devices with no pairing
// step. Settings → App library holds the card; the app fetches it from /api/v1/app/adb-key.

// roleCanFetchFleetKey says who may pull the private key through the app. Equal to
// "may enroll" for now, so it can be tightened later without touching the app.
func roleCanFetchFleetKey(role string) bool { return roleCanOperate(role) }

const fleetKeyFetchesPerHour = 10

// AppAdbKey: GET /api/v1/app/adb-key → {"version","fingerprint","private_key_pem","public_key"}.
// Every fetch is audited with who, from where and which phone; 10 per hour per person.
func (h *Handler) AppAdbKey(w http.ResponseWriter, r *http.Request, s *db.Session) {
	if !roleCanFetchFleetKey(s.Role) {
		appErr(w, http.StatusForbidden, "your role cannot fetch the fleet adb key")
		return
	}
	if n, retry := h.fleetKeyFetches.Hit("u:" + s.Username); n > fleetKeyFetchesPerHour {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		appErr(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d key fetches an hour; try again in %d minute(s)", fleetKeyFetchesPerHour, int(retry.Minutes())+1))
		return
	}
	k, err := h.db.GetFleetAdbKey(r.Context())
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if k == nil {
		appErr(w, http.StatusNotFound, "no fleet adb key uploaded")
		return
	}
	pem, err := fleetkey.Open(h.cfg.FleetAdbKeySecret(), k.PrivateSealed)
	if err != nil {
		appErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	device := strings.TrimSpace(r.Header.Get("X-AIO-Device"))
	if device == "" {
		device = "unknown device"
	}
	h.auditAs(r, s.Username, "fleet_adb_key.fetch", k.Fingerprint, "v"+fmt.Sprint(k.Version)+" · "+ratelimit.ClientIP(r)+" · "+device)
	appJSON(w, http.StatusOK, map[string]any{
		"version": k.Version, "fingerprint": k.Fingerprint, "private_key_pem": pem, "public_key": k.PublicKey,
	})
}

// fleetAdbKeyView is what the Settings card shows: metadata and the last fetches, never the key.
func (h *Handler) fleetAdbKeyView(r *http.Request) map[string]any {
	k, _ := h.db.GetFleetAdbKey(r.Context())
	fetches, _ := h.db.ListAuditByAction(r.Context(), "fleet_adb_key.fetch", 20)
	v := map[string]any{"Key": k, "Fetches": fetches, "SecretSet": h.cfg.FleetAdbKeySecret() != ""}
	return v
}

// SettingsFleetAdbKeyUpload: POST /settings/fleet-adb-key (multipart: adbkey, adbkey_pub).
func (h *Handler) SettingsFleetAdbKeyUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		h.hxDoneToast(w, r, "/settings", "Could not read the upload", "error")
		return
	}
	read := func(field string) string {
		if f, _, err := r.FormFile(field); err == nil {
			defer f.Close()
			b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
			return string(b)
		}
		return r.FormValue(field)
	}
	priv, pub := read("adbkey"), read("adbkey_pub")
	if _, err := fleetkey.ValidatePrivate(priv); err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	fp, err := fleetkey.Fingerprint(pub)
	if err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	sealed, err := fleetkey.Seal(h.cfg.FleetAdbKeySecret(), strings.TrimSpace(priv)+"\n")
	if err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	v, err := h.db.PutFleetAdbKey(r.Context(), strings.TrimSpace(pub), sealed, fp, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, "/settings", "Could not store the key", "error")
		return
	}
	h.audit(r, "fleet_adb_key.upload", fp, fmt.Sprintf("version %d", v))
	h.hxDoneToast(w, r, "/settings", fmt.Sprintf("Fleet adb key stored · %s · version %d", fp, v), "success")
}

// SettingsFleetAdbKeyRemove: POST /settings/fleet-adb-key/remove.
func (h *Handler) SettingsFleetAdbKeyRemove(w http.ResponseWriter, r *http.Request) {
	k, _ := h.db.GetFleetAdbKey(r.Context())
	if k == nil {
		h.hxDoneToast(w, r, "/settings", "No fleet adb key to remove", "error")
		return
	}
	if err := h.db.DeleteFleetAdbKey(r.Context()); err != nil {
		h.hxDoneToast(w, r, "/settings", "Could not remove the key", "error")
		return
	}
	h.audit(r, "fleet_adb_key.remove", k.Fingerprint, fmt.Sprintf("version %d", k.Version))
	h.hxDoneToast(w, r, "/settings", "Fleet adb key removed · the apps drop their copy on next sign-in", "success")
}

var _ = time.Now
