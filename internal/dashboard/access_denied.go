package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"mdm/internal/db"
)

// Access denied pages (2 Nov 2026, user: "if access is not granted it should show a proper
// error and not 404"). A page someone may not open answers 403 inside the dashboard: what
// it is, why, who can give access, and a way back. A fragment, fetch or write gets the same
// reason as plain text, which the toast shows. 404 is only for things that don't exist.

// forbidPage refuses a page (or, for anything but a page load, a request) with a reason.
func (h *Handler) forbidPage(w http.ResponseWriter, r *http.Request, what, why string) {
	if !wantsPage(r) {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	h.errorPage(w, r, http.StatusForbidden, what, why)
}

// notFoundPage answers a page whose subject doesn't exist.
func (h *Handler) notFoundPage(w http.ResponseWriter, r *http.Request, what, why string) {
	if !wantsPage(r) {
		http.Error(w, what+" not found", http.StatusNotFound)
		return
	}
	h.errorPage(w, r, http.StatusNotFound, what, why)
}

// wantsPage: a GET page load (typed, linked or boosted), not a fragment, fetch or JSON call.
func wantsPage(r *http.Request) bool {
	return r.Method == http.MethodGet && isNavigation(r) && !strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, code int, what, why string) {
	title := "No access"
	if code == http.StatusNotFound {
		title = "Not found"
	}
	data := map[string]any{"Title": title, "Code": code, "What": what, "Why": why, "Back": backLink(r)}
	if code == http.StatusForbidden {
		data["Who"] = h.accessGivers(r)
		if s, ok := h.currentSession(r); ok {
			data["You"] = roleLabel(s.Role)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// navigation.js swaps a boosted navigation's error answer only when it carries this.
	w.Header().Set("X-MDM-Page", "error")
	w.WriteHeader(code)
	h.render(w, r, "denied.html", data)
}

// accessGivers says who can widen this person's access: super admins for the team; for a
// customer, their AIO contact (and for staff, the venue's owner).
func (h *Handler) accessGivers(r *http.Request) string {
	switch h.role(r) {
	case "staff":
		return "Your venue's owner or your AIO contact can give you access."
	case "owner":
		return "Your AIO contact can add a venue to your account."
	}
	us, err := h.db.ListUsers(r.Context())
	if err != nil {
		return "A super admin can give you access."
	}
	var names []string
	for _, u := range us {
		// Real people only: no demo accounts, no one-letter test names.
		if u.Role == "admin" && !strings.HasPrefix(u.Username, "demo.") && len([]rune(u.DisplayName())) > 2 {
			names = append(names, u.DisplayName())
		}
	}
	switch {
	case len(names) == 0:
		return "A super admin can give you access."
	case len(names) > 4:
		names = append(names[:3], "others")
	}
	return "A super admin can give you access: " + joinAnd(names) + "."
}

// backLink is where "Go back" leads: the page they came from on this site, else home.
func backLink(r *http.Request) string {
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && ref.Path != r.URL.Path && strings.HasPrefix(ref.Path, "/") {
		return ref.RequestURI()
	}
	return "/"
}

// rolesWhere names the current team roles that pass, for "Only Super admins and Ops leads
// can open this."
func rolesWhere(pass func(string) bool) string {
	var out []string
	for _, d := range roleDefs {
		if !d.Customer && !d.Legacy && pass(d.Key) {
			out = append(out, d.Plural)
		}
	}
	if len(out) == 0 {
		return "Nobody"
	}
	return joinAnd(out)
}

// guardRefuse is a role guard's refusal: who the page is for.
func (h *Handler) guardRefuse(w http.ResponseWriter, r *http.Request, pass func(string) bool) {
	who := rolesWhere(pass)
	if !wantsPage(r) {
		http.Error(w, "Only "+who+" can do this.", http.StatusForbidden)
		return
	}
	h.errorPage(w, r, http.StatusForbidden, "This page", "Only "+who+" can open it.")
}

// yourDevices is the person's access in a sentence: "You can look at devices in service."
func (h *Handler) yourDevices(r *http.Request) string {
	acc := h.access(r)
	if roleIsCustomer(acc.role) {
		return "You see your venue's devices only."
	}
	gs, _ := h.db.RoleAccessLevels(r.Context(), acc.role)
	own, _ := h.db.UserAccessLevels(r.Context(), acc.username)
	return accessSentence("You", "You", append(gs, own...), time.Now())
}

// deviceOutsideWhy: why a device is outside someone's access, from its state.
func (h *Handler) deviceOutsideWhy(r *http.Request, dev *db.Device) string {
	acc := h.access(r)
	if roleIsCustomer(acc.role) {
		return "It isn't at your venue."
	}
	acc.loadScopes()
	where := ""
	switch acc.scopes[dev.ID].State() {
	case db.StateLab:
		where = "It is in the Lab (at no venue)."
	case db.StateSettingUp:
		where = "It is being set up" + atVenue(dev) + "."
	default:
		where = "It is in service" + atVenue(dev) + "."
	}
	return where + " " + h.yourDevices(r)
}

func atVenue(dev *db.Device) string {
	if dev.RestaurantName != "" {
		return " at " + dev.RestaurantName
	}
	return ""
}
