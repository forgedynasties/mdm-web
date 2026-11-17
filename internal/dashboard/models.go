package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Models (M3, picked 17 Nov 2026; replaces Lineup's All products). User: "lineup is pretty
// much useless, instead … in the left rail should be all the models, each model can be for
// one role fixed or for multiple roles, set at onboarding", "each model is either standard or
// firmware", and "since a model can have multiple roles its not good to classify rail based
// on" roles. The rail is one flat list of every model, biggest first, with chips (Firmware,
// Standard, Needs a role, Several roles); the pane is the picked model: its role setting
// (super admin and Ops lead), devices by role, MDM client, its roles' apps and, for our own
// boards, firmware. A device picks only among its model's roles (device_role.go).

// modelRow is one model in the rail.
type modelRow struct {
	Key, Label, Reported string
	Firmware             bool     // one of our boards (T7, Kiosk 18/22/27): role fixed by the firmware
	Roles                []string // the roles it serves
	Devices, Online      int
	NoRole               int // devices of a several-role model still waiting for a pick
}

// Kind is Firmware or Standard.
func (m modelRow) Kind() string {
	if m.Firmware {
		return "Firmware"
	}
	return "Standard"
}

// RoleText names its roles ("Kitchen display, Self-order kiosk"), "Needs a role" for none.
func (m modelRow) RoleText() string {
	if len(m.Roles) == 0 {
		return "Needs a role"
	}
	labels := make([]string, len(m.Roles))
	for i, r := range m.Roles {
		labels[i] = product.ClassLabel(r)
	}
	return strings.Join(labels, ", ")
}

// modelRows lists every model: those devices report, our catalog boards and models given a
// role ahead of their first device, biggest first.
func (h *Handler) modelRows(ctx context.Context, devices []db.Device) []modelRow {
	rows, err := h.db.ProductOverview(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, p := range rows {
		seen[p.Key] = true
	}
	for _, cp := range product.All() {
		if !seen[cp.Key] {
			rows = append(rows, db.ProductRow{Key: cp.Key})
			seen[cp.Key] = true
		}
	}
	if attached, err := h.db.AttachedModels(ctx); err == nil {
		for _, p := range attached {
			if !seen[p.Key] {
				rows = append(rows, p)
				seen[p.Key] = true
			}
		}
	}
	counts, _ := h.db.ProductClassCounts(ctx)
	online := map[string]int{}
	for i := range devices {
		if d := &devices[i]; !d.Hidden && !d.Retired() && h.deviceOnline(d) {
			online[modelKey(d)]++
		}
	}
	out := make([]modelRow, 0, len(rows))
	for _, p := range rows {
		m := modelRow{Key: p.Key, Label: productLabel(p.Key), Reported: modelLabel(p.Manufacturer, p.Model),
			Devices: p.Devices, Online: online[p.Key]}
		if product.IsKnown(p.Key) {
			cp, _ := product.Resolve(p.Key)
			m.Firmware, m.Roles = true, []string{cp.Class}
		} else {
			m.Roles = p.Roles
			if len(m.Roles) != 1 {
				m.NoRole = counts[p.Key][""]
			}
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Devices != out[b].Devices {
			return out[a].Devices > out[b].Devices
		}
		return out[a].Label < out[b].Label
	})
	return out
}

// modelPane is the picked model's sections.
type modelPane struct {
	M         modelRow
	ByRole    []roleCount // devices per role, the model's roles first
	CanEdit   bool        // its role setting: super admin and Ops lead, Standard models only
	Moving    map[string]int
	Client    *ClientSlotView
	ClientOn  luBar
	Apps      []modelApp
	Fw        *pipeModel
	FwOnIt    luBar
	FwSince   string
	CanFw     bool
	AllRoles  []string
	FwRoleKey string
}

// modelApp is one of its roles' apps and how many of the model's devices have it.
type modelApp struct {
	Role, Label, Base string
	Have, Of          int
}

// ModelsPage is GET /models[?model=<key>].
func (h *Handler) ModelsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if roleIsCustomer(h.role(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	devFilter := db.DeviceFilter{}
	h.access(r).applyFilter(&devFilter)
	devices, err := h.db.ListDevices(ctx, devFilter, 0, 100000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	models := h.modelRows(ctx, devices)
	pick := product.Normalize(r.URL.Query().Get("model"))
	var cur *modelRow
	for i := range models {
		if models[i].Key == pick || (pick == "" && cur == nil) {
			cur = &models[i]
		}
	}
	data := map[string]any{"Title": "Models", "Models": models, "Total": len(models)}
	needs, several := 0, 0
	for _, m := range models {
		if len(m.Roles) == 0 {
			needs++
		}
		if len(m.Roles) > 1 {
			several++
		}
	}
	data["NeedsN"], data["SeveralN"] = needs, several
	if cur != nil {
		data["Pane"] = h.modelPaneOf(r, *cur, devices)
	}
	h.render(w, r, "models.html", data)
}

func (h *Handler) modelPaneOf(r *http.Request, m modelRow, devices []db.Device) modelPane {
	ctx := r.Context()
	p := modelPane{M: m}
	role := h.role(r)
	p.CanEdit = !m.Firmware && (role == "admin" || role == "fleet_manager")
	for _, c := range product.Classes() {
		if perSerialClass(c) {
			p.AllRoles = append(p.AllRoles, c)
		}
	}
	var mine []db.Device

	by := map[string]int{}
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() || modelKey(d) != m.Key {
			continue
		}
		mine = append(mine, *d)

		by[d.Class()]++
	}
	for _, rl := range m.Roles {
		p.ByRole = append(p.ByRole, roleCount{rl, by[rl]})
		delete(by, rl)
	}
	var rest []string
	for rl := range by {
		rest = append(rest, rl)
	}
	sort.Strings(rest)
	for _, rl := range rest {
		p.ByRole = append(p.ByRole, roleCount{rl, by[rl]})
	}
	// What "One role: X" would move, for the confirmation.
	if p.CanEdit {
		p.Moving = map[string]int{}
		for _, c := range p.AllRoles {
			n := 0
			for i := range mine {
				if mine[i].Class() != c {
					n++
				}
			}
			p.Moving[c] = n
		}
	}
	if views, _ := h.clientSlotSummaries(r, mine); len(views) > 0 {
		for _, slot := range clientSlotOrder {
			if v := views[slot]; v != nil && v.Devices > 0 {
				p.Client, p.ClientOn = v, mkLuBar(v.UpToDate, v.Devices)
				break
			}
		}
	}
	if ras, err := h.db.ListRoleApps(ctx); err == nil {
		base := map[string]string{}
		for _, a := range ras {
			base[a.Role] = a.BasePackage
		}
		for _, rl := range m.Roles {
			a := modelApp{Role: rl, Label: product.ClassLabel(rl), Base: base[rl]}
			if a.Base != "" {
				var roleIDs []uuid.UUID
				for i := range mine {
					if mine[i].Class() == rl {
						roleIDs = append(roleIDs, mine[i].ID)
					}
				}
				a.Of = len(roleIDs)
				if fv, err := h.db.FamilyVersions(ctx, []string{a.Base}); err == nil {
					for _, id := range roleIDs {
						if len(fv[id]) > 0 {
							a.Have++
						}
					}
				}
			}
			p.Apps = append(p.Apps, a)
		}
	}
	if m.Firmware && len(m.Roles) == 1 {
		if st, err := h.loadRoleAppState(ctx); err == nil {
			for _, pm := range h.pipelineModels(ctx, m.Roles[0], devices, st) {
				if pm.Key == m.Key {
					pm := pm
					p.Fw, p.FwRoleKey = &pm, m.Roles[0]
					p.FwOnIt = mkLuBar(pm.On, pm.Total)
					if pm.Channel != nil {
						p.FwSince = "since " + pm.Channel.SetAt.Format("2 Jan")
						if by := pm.Channel.SetBy; by != "" {
							if i := strings.IndexByte(by, '@'); i > 0 {
								by = by[:i]
							}
							p.FwSince += " · " + by
						}
					}
				}
			}
		}
		p.CanFw = h.canFleetFirmware(r)
	}
	return p
}

// ModelSetRoles is POST /models/{key}/roles {mode: one|several, role (one), roles (several)}.
func (h *Handler) ModelSetRoles(w http.ResponseWriter, r *http.Request) {
	role := h.role(r)
	if role != "admin" && role != "fleet_manager" {
		http.Error(w, "Only a super admin or an Ops lead changes a model's roles.", http.StatusForbidden)
		return
	}
	key := product.Normalize(r.PathValue("key"))
	back := "/models?model=" + key
	if key == "" || product.IsKnown(key) {
		h.hxDoneToast(w, r, back, "This model's role is set by its firmware", "error")
		return
	}
	r.ParseForm()
	var roles []string
	switch r.FormValue("mode") {
	case "one":
		if c := r.FormValue("role"); perSerialClass(c) {
			roles = []string{c}
		}
	case "several":
		for _, c := range r.Form["roles"] {
			if perSerialClass(c) && !slices.Contains(roles, c) {
				roles = append(roles, c)
			}
		}
	}
	if len(roles) == 0 {
		h.hxDoneToast(w, r, back, "Pick a role", "error")
		return
	}
	if r.FormValue("mode") == "several" && len(roles) < 2 {
		h.hxDoneToast(w, r, back, "Several roles means at least two", "error")
		return
	}
	n, err := h.db.SetProductRoles(r.Context(), key, roles, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	labels := make([]string, len(roles))
	for i, c := range roles {
		labels[i] = product.ClassLabel(c)
	}
	h.audit(r, "model.roles", key, strings.Join(roles, ","))
	msg := productLabel(key) + ": " + strings.Join(labels, ", ")
	if n > 0 {
		msg += fmt.Sprintf(" · %d device(s) changed role", n)
	}
	h.hxRedirect(w, r, back+"&flash="+url.QueryEscape(msg))
}
