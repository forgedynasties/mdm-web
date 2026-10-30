package dashboard

import "net/http"

// Device page views (16 Oct): the classic page as it was before the 14 Oct revamp, and
// the new one. New is the default since 27 Oct 2026; a ?view=new or ?view=classic link switches and the
// choice is kept on the user, so it holds across sessions and browsers.
func (h *Handler) deviceViewTemplate(r *http.Request) string {
	user := h.currentUsername(r)
	// New is the default for everyone, new accounts included (27 Oct 2026); Classic is
	// kept only where someone chose it ("classic"). Before that day '' meant Classic.
	view := ""
	switch q := r.URL.Query().Get("view"); q {
	case "new", "classic":
		view = q
		if user != "" {
			_ = h.db.SetDeviceView(r.Context(), user, view)
		}
	default:
		if user != "" {
			view, _ = h.db.DeviceView(r.Context(), user)
		}
	}
	if view == "classic" {
		return "device_classic.html"
	}
	return "device.html"
}

// fleetView is how device lists (Fleet, venue, group and production pages) are drawn:
// "" = the table. The two-line cards were a per-person choice until 30 Oct 2026.
func (h *Handler) fleetView(r *http.Request) string {
	// The table is the only view (user, 30 Oct 2026: "list is the only view"); a stored
	// "cards" choice is ignored.
	return ""
}
