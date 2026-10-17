package dashboard

import (
	"encoding/json"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"

	"mdm/internal/db"
)

// BootLogo renders the Boot logo config page: a dedicated splash-upload + target
// picker whose submit runs through the normal update_splash command path (POST
// /commands), plus the status of the most recently applied splash. update_splash
// is hidden from the Actions/history/device command lists (see ListCommandsSince /
// GetDeviceCommands), so this page is the single place boot logos are managed.
func (h *Handler) BootLogo(w http.ResponseWriter, r *http.Request) {
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var (
		last       *db.Command
		stats      deliveryStats
		deliveries []db.CommandDelivery
		splashURL  string
	)
	if c, err := h.db.GetLatestCommandByType(r.Context(), "update_splash"); err == nil && c != nil {
		last = c
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		splashURL = p.URL
		if ds, err := h.db.GetCommandDeliveries(r.Context(), c.ID, h.cfg.CommandExpiry()); err == nil {
			ds = h.keepVisibleDeliveries(r, ds)
			h.markDeliveryPresence(ds)
			deliveries = ds
			stats = computeDeliveryStats(ds)
		}
	}
	h.render(w, r, "boot_logo.html", map[string]any{
		"Title":      "Boot logo",
		"Groups":     groups,
		"Last":       last,
		"SplashURL":  splashURL,
		"Stats":      stats,
		"Deliveries": deliveries,
	})
}
