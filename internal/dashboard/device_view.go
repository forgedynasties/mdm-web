package dashboard

import "net/http"

// Device page views (16 Oct): the classic page as it was before the 14 Oct revamp, and
// the new one. New is the default since 27 Oct 2026; a ?view=new or ?view=classic link switches and the
// choice is kept on the user, so it holds across sessions and browsers.
func (h *Handler) deviceViewTemplate(r *http.Request) string {
	if h.deviceViewName(r) == "classic" {
		return "device_classic.html"
	}
	return "device.html" // "new" and "bento" are the same page; bento tiles its Overview
}

// deviceViewName is the device page the person chose: "new" (the default), "classic" or
// "bento" (the Overview as tiles, 18 Nov 2026; never the default, picked from the page's
// ⋯ menu and remembered on the user like the others).
func (h *Handler) deviceViewName(r *http.Request) string {
	user := h.currentUsername(r)
	// New is the default for everyone, new accounts included (27 Oct 2026); Classic is
	// kept only where someone chose it ("classic"). Before that day '' meant Classic.
	view := ""
	switch q := r.URL.Query().Get("view"); q {
	case "new", "classic", "bento":
		view = q
		if user != "" {
			_ = h.db.SetDeviceView(r.Context(), user, view)
		}
	default:
		if user != "" {
			view, _ = h.db.DeviceView(r.Context(), user)
		}
	}
	switch view {
	case "classic", "bento":
		return view
	}
	return "new"
}

// fleetView is how device lists (Fleet, venue, group and production pages) are drawn:
// "" = the table. The two-line cards were a per-person choice until 30 Oct 2026.
func (h *Handler) fleetView(r *http.Request) string {
	// The table is the only view (user, 30 Oct 2026: "list is the only view"); a stored
	// "cards" choice is ignored.
	return ""
}
