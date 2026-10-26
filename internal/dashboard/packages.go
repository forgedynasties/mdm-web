package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
)

// ── Packages ──────────────────────────────────────────────────────────────────

func (h *Handler) DevicePackages(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	pkgs, err := h.db.GetDevicePackages(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Default to app-drawer apps only (see launchableOnly); ?all=1 keeps the raw
	// package dump reachable.
	showAll := r.URL.Query().Get("all") == "1"
	total := len(pkgs)
	if !showAll {
		pkgs = launchableOnly(pkgs)
	}
	h.render(w, r, "device_packages.html", map[string]any{
		"Title":    serial + " — Packages",
		"Device":   device,
		"Packages": pkgs,
		"ShowAll":  showAll,
		"AllCount": total,
	})
}
