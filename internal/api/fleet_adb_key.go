package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"mdm/internal/fleetkey"
)

// Fleet adb key — admin API (X-API-Key). The private key goes in once and never comes
// back out through here; the apps fetch it through /api/v1/app/adb-key with a signed-in
// session (internal/dashboard/app_api.go).
//
//	PUT    /api/v1/fleet-adb-key  {"private_key_pem","public_key"} → {"version","fingerprint"}
//	GET    /api/v1/fleet-adb-key  → {"version","fingerprint","uploaded_by","uploaded_at"}  (404 when none)
//	DELETE /api/v1/fleet-adb-key

func (h *Handler) PutFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PrivateKeyPEM string `json:"private_key_pem"`
		PublicKey     string `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if _, err := fleetkey.ValidatePrivate(body.PrivateKeyPEM); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	fp, err := fleetkey.Fingerprint(body.PublicKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sealed, err := fleetkey.Seal(h.cfg.FleetAdbKeySecret(), strings.TrimSpace(body.PrivateKeyPEM)+"\n")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	v, err := h.db.PutFleetAdbKey(r.Context(), strings.TrimSpace(body.PublicKey), sealed, fp, "API key")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "API key", "fleet_adb_key.upload", fp, "version "+itoa(v))
	writeJSON(w, http.StatusOK, map[string]any{"version": v, "fingerprint": fp})
}

func (h *Handler) GetFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	k, err := h.db.GetFleetAdbKey(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if k == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fleet adb key uploaded"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": k.Version, "fingerprint": k.Fingerprint, "uploaded_by": k.UploadedBy, "uploaded_at": k.UploadedAt,
	})
}

func (h *Handler) DeleteFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	k, err := h.db.GetFleetAdbKey(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if k == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fleet adb key uploaded"})
		return
	}
	if err := h.db.DeleteFleetAdbKey(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "API key", "fleet_adb_key.remove", k.Fingerprint, "version "+itoa(k.Version))
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "version": k.Version})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
