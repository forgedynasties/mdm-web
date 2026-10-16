package dashboard

import "net/http"

// Device page views (16 Oct): the classic page as it was before the 14 Oct revamp, and
// the new one. Classic is the default; a ?view=new or ?view=classic link switches and the
// choice is kept on the user, so it holds across sessions and browsers.
func (h *Handler) deviceViewTemplate(r *http.Request) string {
	user := h.currentUsername(r)
	view := ""
	switch r.URL.Query().Get("view") {
	case "new":
		view = "new"
		if user != "" {
			_ = h.db.SetDeviceView(r.Context(), user, view)
		}
	case "classic":
		if user != "" {
			_ = h.db.SetDeviceView(r.Context(), user, "")
		}
	default:
		if user != "" {
			view, _ = h.db.DeviceView(r.Context(), user)
		}
	}
	if view == "new" {
		return "device.html"
	}
	return "device_classic.html"
}
