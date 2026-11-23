package dashboard

import "net/http"

// Landing renders the public entry page for visitors without a session (LP4 "The
// door", 19 Nov 2026): the product name, one line, Sign in, Microsoft sign-in when it
// is configured, and Create account. The sneak-peek preview it used to link to is gone.
func (h *Handler) Landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.tmpl.ExecuteTemplate(w, "landing.html", map[string]any{
		"Brand":         h.cfg.CustomBrand(),
		"AssetVer":      h.assetVer,
		"SignupEnabled": true,
	})
}

// About renders the public /about page for viewers (picked 23 Nov 2026, "Overview bento"):
// what AIO MDM is, a mock of the Overview with fictional data, the capabilities, how
// devices connect, the two clients and the device lineup. Open to everyone, signed in or
// not; it replaces the retired sneak peek as the page we link people to.
func (h *Handler) About(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	h.tmpl.ExecuteTemplate(w, "about.html", map[string]any{
		"AssetVer": h.assetVer,
	})
}
