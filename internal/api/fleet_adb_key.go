package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"mdm/internal/db"
	"mdm/internal/fleetkey"
)

// Fleet adb keys — admin API (X-API-Key). A private key goes in once and never comes
// back out through here; the apps fetch them through /api/v1/app/adb-key with a signed-in
// session (internal/dashboard/app_api.go).
//
// There is one key per vendor, named by a label, because a key baked into a vendor's
// image can only be changed by a firmware release — so one key for everybody means one
// leak reopens the whole fleet. "default" is the key the AIO firmware carries, and is
// what the unlabelled routes act on, so anything written against the old single-key API
// keeps working.
//
//	PUT    /api/v1/fleet-adb-key          {"private_key_pem","public_key","label"?} → {"label","version","fingerprint"}
//	GET    /api/v1/fleet-adb-key          → {"label","version","fingerprint","uploaded_by","uploaded_at"}  (404 when none)
//	DELETE /api/v1/fleet-adb-key
//	PUT    /api/v1/fleet-adb-key/{label}  — the same, for one vendor's key
//	GET    /api/v1/fleet-adb-key/{label}
//	DELETE /api/v1/fleet-adb-key/{label}
//	GET    /api/v1/fleet-adb-keys         → {"keys":[…]}  (metadata only, never a private key)

// label is the one in the path, else the one in the body, else "default".
func fleetKeyLabel(r *http.Request, fromBody string) (string, error) {
	l := r.PathValue("label")
	if l == "" {
		l = fromBody
	}
	return db.CleanFleetAdbKeyLabel(l)
}

func (h *Handler) PutFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PrivateKeyPEM string `json:"private_key_pem"`
		PublicKey     string `json:"public_key"`
		Label         string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	label, err := fleetKeyLabel(r, body.Label)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	v, err := h.db.PutFleetAdbKeyByLabel(r.Context(), label, strings.TrimSpace(body.PublicKey), sealed, fp, "API key")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "API key", "fleet_adb_key.upload", fp, label+" · version "+itoa(v))
	writeJSON(w, http.StatusOK, map[string]any{"label": label, "version": v, "fingerprint": fp})
}

func (h *Handler) GetFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	label, err := fleetKeyLabel(r, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	k, err := h.db.GetFleetAdbKeyByLabel(r.Context(), label)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if k == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fleet adb key uploaded for " + label})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"label": k.Label, "version": k.Version, "fingerprint": k.Fingerprint,
		"uploaded_by": k.UploadedBy, "uploaded_at": k.UploadedAt,
	})
}

// ListFleetAdbKeys: every key's metadata, so an operator can see which vendors are covered.
func (h *Handler) ListFleetAdbKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.db.ListFleetAdbKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"label": k.Label, "version": k.Version, "fingerprint": k.Fingerprint,
			"public_key": k.PublicKey, "uploaded_by": k.UploadedBy, "uploaded_at": k.UploadedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (h *Handler) DeleteFleetAdbKey(w http.ResponseWriter, r *http.Request) {
	label, err := fleetKeyLabel(r, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	k, err := h.db.GetFleetAdbKeyByLabel(r.Context(), label)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if k == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fleet adb key uploaded for " + label})
		return
	}
	if err := h.db.DeleteFleetAdbKeyByLabel(r.Context(), label); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "API key", "fleet_adb_key.remove", k.Fingerprint, label+" · version "+itoa(k.Version))
	writeJSON(w, http.StatusOK, map[string]any{"label": label, "removed": true, "version": k.Version})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
