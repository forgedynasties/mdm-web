package dashboard

import "net/http"

// The device page has one view, Bento (the Overview as tiles), since 25 Nov 2026 (user:
// "make bento view the default. remove the standard view and the classic view"). Old
// ?view= links and the users.device_view column are ignored.
func (h *Handler) deviceViewTemplate(r *http.Request) string {
	return "device.html"
}

// deviceViewName is always "bento"; kept so callers read the same way.
func (h *Handler) deviceViewName(r *http.Request) string {
	return "bento"
}

// fleetView is how device lists (Fleet, venue, group and production pages) are drawn:
// "" = the table. The two-line cards were a per-person choice until 30 Oct 2026.
func (h *Handler) fleetView(r *http.Request) string {
	// The table is the only view (user, 30 Oct 2026: "list is the only view"); a stored
	// "cards" choice is ignored.
	return ""
}
