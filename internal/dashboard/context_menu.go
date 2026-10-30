package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"

	"mdm/internal/product"
)

// Right-click menus (R1, picked 30 Oct 2026): a device, venue, group, app or person
// right-clicked anywhere in <main> opens our menu (static/js/layout/context-menu.js);
// Shift + right-click keeps the browser's. The menu only offers what the viewer may do:
// ctxCaps is that, printed once by the layout; each route still checks for itself.

// ctxCaps is what the viewer's role may do from a right-click menu, as JSON for the
// layout's data-ctx-caps.
func ctxCaps(role string) string {
	may := func(cmd string) bool { return slices.Contains(commandRoles[cmd], role) }
	caps := map[string]any{
		"team":       role != "" && !roleIsCustomer(role),
		"screenshot": may("screenshot"),
		"reboot":     may("reboot"),
		"remote":     roleCanOperate(role),
		"lock":       roleCanSupport(role),
		"group":      roleCanOperate(role),
		// Engineers never move devices to a venue (engineerNeverWrites).
		"venue": roleCanOperate(role) && role != "engineer",
		"role":  roleCanOperate(role),
		"admin": role == "admin",
		// An engineer only looks at, screenshots and collects logs from devices at a venue.
		"venueReadOnly": role == "engineer",
	}
	// Role… lists what a Standard-client device may be set to by serial (perSerialClass).
	var roles [][2]string
	for _, c := range product.Classes() {
		if perSerialClass(c) {
			roles = append(roles, [2]string{c, product.ClassLabel(c)})
		}
	}
	caps["roles"] = roles
	b, _ := json.Marshal(caps)
	return string(b)
}

type ctxTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CtxTargets: GET /ctx/targets → {"groups":[…],"venues":[…]}, the pickers of the menu's
// Add to group… and Move to venue…. Venues are the ones the viewer may act on; hidden
// groups stay out, as in every other picker.
func (h *Handler) CtxTargets(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Groups []ctxTarget `json:"groups"`
		Venues []ctxTarget `json:"venues"`
	}{Groups: []ctxTarget{}, Venues: []ctxTarget{}}
	if groups, err := h.db.ListGroups(r.Context()); err == nil {
		for _, g := range groups {
			out.Groups = append(out.Groups, ctxTarget{g.ID.String(), g.Name})
		}
	}
	if rests, err := h.db.ListRestaurants(r.Context()); err == nil {
		acc := h.access(r)
		for _, v := range rests {
			if acc.venueInScope(v.ID) {
				out.Venues = append(out.Venues, ctxTarget{v.ID.String(), v.Name})
			}
		}
	}
	byName := func(s []ctxTarget) {
		sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	}
	byName(out.Groups)
	byName(out.Venues)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
