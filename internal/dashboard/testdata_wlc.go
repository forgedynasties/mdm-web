package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"mdm/internal/product"
)

// QASetDeviceWlc switches wireless charging on a device's pad for the testing team (POST
// /api/v1/testdata/devices/{serial}/wlc, QA key, {"enabled": bool}; user, 2 Nov 2026: "in
// the test api also add wlc on off endpoint"). The same switch as APISetDeviceWlc, in the
// testdata scope: firmware devices at no venue (Lab), and only a product with a pad.
func (h *Handler) QASetDeviceWlc(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Enabled == nil {
		writeJSONError(w, http.StatusBadRequest, "body must be {\"enabled\": true or false}")
		return
	}
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil || dev == nil || dev.AgentKind != product.KindFirmware || dev.RestaurantID != nil {
		writeJSONError(w, http.StatusNotFound, "device not found")
		return
	}
	key := dev.Product
	if key == "" {
		key = "t7" // firmware clients that report no product are T7s
	}
	if !slices.Contains(h.cfg.WlcProducts(), key) {
		writeJSONError(w, http.StatusConflict, "this device has no wireless charging pad")
		return
	}
	if err := h.db.SetWlcCharging(ctx, dev.ID, *body.Enabled); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.pushKioskConfigToDevices(ctx, []uuid.UUID{dev.ID})
	_ = h.db.InsertAuditDevice(ctx, "qa-api", "device.wlc_charging", dev.SerialNumber, boolWord(*body.Enabled), dev.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"serial": dev.SerialNumber, "enabled": *body.Enabled,
		"connected": h.hub.IsConnected(dev.ID)})
}
