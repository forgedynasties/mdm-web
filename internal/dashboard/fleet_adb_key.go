package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/fleetkey"
	"mdm/internal/ratelimit"
)

// The fleet adb keys: the key pairs device images trust, handed to the AIO Enroll apps
// after sign-in so they can adb-connect with no pairing step. One per vendor — a key
// baked into an image can only be changed by a firmware release, so a single shared key
// means one leak reopens every device we have. "default" is the key the AIO firmware
// carries. Settings → App library holds the card; the apps fetch them from
// /api/v1/app/adb-key.

// roleCanFetchFleetKey says who may pull the private key through the app. Equal to
// "may enroll" for now, so it can be tightened later without touching the app.
func roleCanFetchFleetKey(role string) bool { return roleCanOperate(role) }

const fleetKeyFetchesPerHour = 10

// AppAdbKey: GET /api/v1/app/adb-key → every key the apps should try, newest version first:
//
//	{"version","keys":[{"label","fingerprint","version","private_key_pem","public_key"},…]}
//
// The default key's fields are repeated at the top level ("fingerprint", "private_key_pem",
// "public_key") because the apps released before there were several read only those.
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
	keys, err := h.db.ListFleetAdbKeys(r.Context())
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(keys) == 0 {
		appErr(w, http.StatusNotFound, "no fleet adb key uploaded")
		return
	}
	secret := h.cfg.FleetAdbKeySecret()
	out := make([]map[string]any, 0, len(keys))
	newest, fps := 0, make([]string, 0, len(keys))
	for _, k := range keys {
		pem, err := fleetkey.Open(secret, k.PrivateSealed)
		if err != nil {
			// One key that will not unseal must not cost the app the others.
			h.auditAs(r, s.Username, "fleet_adb_key.fetch", k.Fingerprint, k.Label+" · could not unseal: "+err.Error())
			continue
		}
		out = append(out, map[string]any{
			"label": k.Label, "version": k.Version, "fingerprint": k.Fingerprint,
			"private_key_pem": pem, "public_key": k.PublicKey,
		})
		if k.Version > newest {
			newest = k.Version
		}
		fps = append(fps, k.Label+"/"+k.Fingerprint)
	}
	if len(out) == 0 {
		// Every key failed to unseal: the secret is wrong or missing, which is the one
		// thing the operator needs told.
		appErr(w, http.StatusServiceUnavailable, "the keys cannot be unsealed on this server")
		return
	}
	// Who asked, in enough detail to recognise a fetch nobody expected: which app and
	// version, and which machine. Both come from the client, so they are clamped to a
	// sane length and are evidence of nothing on their own.
	clip := func(v, fallback string) string {
		v = strings.TrimSpace(v)
		if v == "" {
			return fallback
		}
		if len(v) > 64 {
			v = v[:64]
		}
		return v
	}
	app := clip(r.Header.Get("X-AIO-App"), "unknown app")
	device := clip(r.Header.Get("X-AIO-Device"), "unknown device")
	h.auditAs(r, s.Username, "fleet_adb_key.fetch", out[0]["fingerprint"].(string),
		fmt.Sprint(len(out))+" key(s) · "+strings.Join(fps, ", ")+" · "+ratelimit.ClientIP(r)+" · "+app+" · "+device)

	body := map[string]any{"version": newest, "keys": out}
	// ListFleetAdbKeys puts "default" first, so out[0] is it when it exists.
	for _, f := range []string{"fingerprint", "private_key_pem", "public_key"} {
		body[f] = out[0][f]
	}
	appJSON(w, http.StatusOK, body)
}

// AppAdbKeySeen: POST /api/v1/app/adb-key-seen {"serial","label"} — the app telling us which
// key got it into a device. Cheap, idempotent, and the only way we learn this for a device
// that is not enrolled yet; a leaked key is answered from these rows.
func (h *Handler) AppAdbKeySeen(w http.ResponseWriter, r *http.Request, s *db.Session) {
	var body struct {
		Serial string `json:"serial"`
		Label  string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	serial := strings.TrimSpace(body.Serial)
	label, err := db.CleanFleetAdbKeyLabel(body.Label)
	if err != nil || serial == "" {
		appErr(w, http.StatusBadRequest, "serial and label are required")
		return
	}
	if err := h.db.PutDeviceAdbKey(r.Context(), serial, label, "enroll-app", s.Username); err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	appJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// fleetAdbKeyView is what the Settings card shows: each key's metadata, how many devices are
// known to take it, and the last fetches. Never a private key.
func (h *Handler) fleetAdbKeyView(r *http.Request) map[string]any {
	keys, _ := h.db.ListFleetAdbKeys(r.Context())
	counts, _ := h.db.CountDevicesByAdbKey(r.Context())
	fetches, _ := h.db.ListAuditByAction(r.Context(), "fleet_adb_key.fetch", 20)
	k, _ := h.db.GetFleetAdbKey(r.Context())
	return map[string]any{
		"Keys":      keys,
		"Devices":   counts,
		"Key":       k, // the default one, for the "is this server set up" check
		"Fetches":   fetches,
		"SecretSet": h.cfg.FleetAdbKeySecret() != "",
	}
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
	label, err := db.CleanFleetAdbKeyLabel(r.FormValue("label"))
	if err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	if _, err := fleetkey.ValidatePrivate(priv); err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	fp, err2 := fleetkey.Fingerprint(pub)
	if err2 != nil {
		h.hxDoneToast(w, r, "/settings", err2.Error(), "error")
		return
	}
	sealed, err := fleetkey.Seal(h.cfg.FleetAdbKeySecret(), strings.TrimSpace(priv)+"\n")
	if err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	v, err := h.db.PutFleetAdbKeyByLabel(r.Context(), label, strings.TrimSpace(pub), sealed, fp, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, "/settings", "Could not store the key", "error")
		return
	}
	h.audit(r, "fleet_adb_key.upload", fp, fmt.Sprintf("%s · version %d", label, v))
	h.hxDoneToast(w, r, "/settings", fmt.Sprintf("adb key %s stored · %s · version %d", label, fp, v), "success")
}

// SettingsFleetAdbKeyRemove: POST /settings/fleet-adb-key/remove.
func (h *Handler) SettingsFleetAdbKeyRemove(w http.ResponseWriter, r *http.Request) {
	label, err := db.CleanFleetAdbKeyLabel(r.FormValue("label"))
	if err != nil {
		h.hxDoneToast(w, r, "/settings", err.Error(), "error")
		return
	}
	k, _ := h.db.GetFleetAdbKeyByLabel(r.Context(), label)
	if k == nil {
		h.hxDoneToast(w, r, "/settings", "No "+label+" adb key to remove", "error")
		return
	}
	if err := h.db.DeleteFleetAdbKeyByLabel(r.Context(), label); err != nil {
		h.hxDoneToast(w, r, "/settings", "Could not remove the key", "error")
		return
	}
	h.audit(r, "fleet_adb_key.remove", k.Fingerprint, fmt.Sprintf("%s · version %d", label, k.Version))
	h.hxDoneToast(w, r, "/settings", "adb key "+label+" removed · the apps drop their copy on next sign-in", "success")
}

var _ = time.Now
