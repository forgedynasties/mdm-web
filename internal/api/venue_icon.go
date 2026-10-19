package api

import (
	"net/http"
	"strings"

	"mdm/internal/ratelimit"
)

// VenueIcon is GET /api/v1/venue-icon?serial=…: the PNG of the calling device's own
// venue icon, for the firmware client's lost screen (L1, 19 Oct 2026). The device learns
// the icon's version from venue_icon_v (lost_mode payload, config self_lock) and fetches
// it only when that changes, so the image never rides in a WebSocket frame. 404 when the
// device has no venue or its venue has no icon.
func (h *Handler) VenueIcon(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial query parameter required"})
		return
	}
	if !validSerial(serial) {
		h.noteRefusedSerial(serial, ratelimit.ClientIP(r), "")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if h.deviceRateLimited(w, serial) {
		return
	}
	if !h.requireDeviceIdentity(w, r, serial) {
		return
	}
	ctx := r.Context()
	device, err := h.db.GetDevice(ctx, serial)
	if err != nil || device == nil || device.RestaurantID == nil {
		http.NotFound(w, r)
		return
	}
	png, _, _, err := h.db.RestaurantIcon(ctx, *device.RestaurantID)
	if err != nil || png == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// The client keys its cache on venue_icon_v, so a day is plenty.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(png)
}
