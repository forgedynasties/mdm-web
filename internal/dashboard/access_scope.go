package dashboard

import (
	"net/http"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Scope helpers for the security audit's medium findings (26 Oct 2026): pages and
// endpoints that listed every device to people whose access rules hide some.

// visibleSerialSet is the serials this viewer may see, or nil when they see the whole
// fleet (callers treat nil as "keep everything").
func (h *Handler) visibleSerialSet(r *http.Request) map[string]bool {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return nil
	}
	f := db.DeviceFilter{}
	acc.applyFilter(&f)
	devs, err := h.db.ListDevices(r.Context(), f, 0, 100000, "serial", "asc")
	out := map[string]bool{}
	if err != nil {
		return out // fail closed: nothing
	}
	for _, d := range devs {
		out[d.SerialNumber] = true
	}
	return out
}

// keepSerials drops serials outside set (nil set keeps them all).
func keepSerials(serials []string, set map[string]bool) []string {
	if set == nil {
		return serials
	}
	out := serials[:0:0]
	for _, s := range serials {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// venueInScope: may this viewer act on the venue? Everyone who sees the whole fleet may;
// otherwise a venue with a device they can see, or one an allow rule names (so a new,
// empty venue of theirs still counts).
func (a *access) venueInScope(id uuid.UUID) bool {
	if !a.hidesDevices() {
		return true
	}
	// The Ops lead runs every venue, empty ones included; Field the venues of their visits.
	if a.role == "fleet_manager" && a.pol.Base != "deny" {
		return true
	}
	if a.role == "field" && a.onVisitAt(id) {
		return true
	}
	if rests, _ := a.visibleCollections(); rests[id] > 0 {
		return true
	}
	for _, g := range a.pol.Grants {
		if g.Effect == "allow" && g.ScopeType == "restaurant" && g.ScopeID != nil && *g.ScopeID == id {
			return true
		}
	}
	return false
}

// keepVisibleEnrollments drops enrollment rows for devices the viewer can't see.
func keepVisibleEnrollments(rows []db.Enrollment, set map[string]bool) []db.Enrollment {
	if set == nil {
		return rows
	}
	out := rows[:0:0]
	for _, e := range rows {
		if set[e.Serial] {
			out = append(out, e)
		}
	}
	return out
}

// commandHiddenFrom is the command page's visibility rule, shared by every route that
// shows a command's output (the status partial and the live output stream skipped it,
// so shell output reached roles the command page refuses): shell is super admin only,
// commands hidden by the admin-command filters stay hidden.
func (h *Handler) commandHiddenFrom(r *http.Request, cmd db.Command) bool {
	role := h.role(r)
	if cmd.Type == "shell" && hideShellForRole(role) {
		return true
	}
	if len(h.filterHiddenCommands(r, []db.Command{cmd})) == 0 {
		return true
	}
	return hideAdminCommandsForRole(role) && h.isAdminAction(cmd)
}

// seesAllOf reports whether the viewer can see every device matching f. Venue and group
// totals (power and usage, health, trends, daily stats) are computed over the whole
// collection, so they are shown only to someone who sees all of it.
func (h *Handler) seesAllOf(r *http.Request, f db.DeviceFilter) bool {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return true
	}
	devs, err := h.db.ListDevices(r.Context(), f, 0, 100000, "serial", "asc")
	if err != nil {
		return false
	}
	for _, d := range devs {
		if !acc.visible(d.ID) {
			return false
		}
	}
	return true
}
