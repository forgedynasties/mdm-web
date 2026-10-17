package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strings"
)

// ── Packages ──────────────────────────────────────────────────────────────────

// FleetPackages renders the admin app-classification page: every package across the
// fleet, how clients classified it, and an admin override to flag system apps used
// as the fallback when no client reports the flag.
func (h *Handler) FleetPackages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	pkgs, err := h.db.ListPackagesAdmin(r.Context(), q)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "packages.html", map[string]any{
		"Title":    "App classification",
		"Packages": pkgs,
		"Query":    q,
	})
}

// PackageFlag toggles the admin system-app override for a package (admin only).
func (h *Handler) PackageFlag(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimSpace(r.FormValue("package"))
	if pkg == "" {
		http.Error(w, "package required", http.StatusBadRequest)
		return
	}
	flagged := r.FormValue("flagged") == "1"
	if err := h.db.SetPackageSystemOverride(r.Context(), pkg, flagged); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// HTMX posts get a 204 (the row updates its own UI); plain forms redirect back.
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/packages", http.StatusSeeOther)
}

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
