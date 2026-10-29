package dashboard

import (
	"context"
	"slices"
	"sort"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Lineup (static/lineup-merge-demos.html, M1, picked 14 Oct) is Releases and Hardware in
// one page: the roles in a side list, and for each role one pane that says what it runs
// (its app and builds, its firmware) and what it is (the models attached to it). The
// Pipeline handler renders it; this file builds the hardware half.

// hwModel is one hardware model on the Lineup page.
type hwModel struct {
	Key, Display, Reported, Name, Role string
	// Roles: every role the model is attached to (a model can serve several, 29 Oct
	// 2026). In a role's pane Role is that role, Devices counts its devices in it, Also
	// lists the model's other roles and NoRole its devices still waiting for a pick.
	Roles                        []string
	Also                         []roleCount
	NoRole                       int
	Known, CanFw                 bool // Known: our own board, built in to its role
	Devices, Firmware, DPC, Lite int
	FwMode                       string // prod | lab | off
	Runs                         int    // production runs linked to the model
}

// roleCount is one of a model's other roles and how many of its devices are in it.
type roleCount struct {
	Role string
	N    int
}

// hwData is every model, grouped by the role it is attached to.
type hwData struct {
	ByRole      map[string][]hwModel
	Devices     map[string]int // devices per role, over its models
	None        []hwModel      // models attached to no (current) role
	NoneDevices int
	Stock       []hwModel // every model that can be attached (not one of ours)
}

// lineupHardware lists every model the dashboard knows: our catalog boards (also with
// no device yet), every model the fleet reports, and models attached ahead of their
// first device.
func (h *Handler) lineupHardware(ctx context.Context, productions []db.Production) hwData {
	out := hwData{ByRole: map[string][]hwModel{}, Devices: map[string]int{}}
	rows, err := h.db.ProductOverview(ctx)
	if err != nil {
		return out
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
	runs := map[string]int{}
	for _, p := range productions {
		if p.Model != "" {
			runs[p.Model]++
		}
	}
	counts, _ := h.db.ProductClassCounts(ctx)
	modes, _ := h.db.FirmwareModes(ctx)
	order := map[string]int{}
	for i, cp := range product.All() {
		order[cp.Key] = i
	}
	for _, p := range rows {
		m := hwModel{
			Key: p.Key, Known: product.IsKnown(p.Key), Reported: modelLabel(p.Manufacturer, p.Model),
			Devices: p.Devices, Firmware: p.Firmware, DPC: p.DPC, Lite: p.Lite, Name: p.Name, Role: p.Role,
			FwMode: db.FirmwareModeOf(modes, p.Key), Runs: runs[p.Key],
		}
		if m.Known {
			cp, _ := product.Resolve(p.Key)
			m.Role, m.CanFw = cp.Class, true
		}
		m.Display = productLabel(p.Key)
		if m.Known {
			m.Roles = []string{m.Role}
		} else {
			m.Roles = p.Roles
		}
		if !m.Known {
			out.Stock = append(out.Stock, m)
		}
		if len(m.Roles) == 0 || !product.IsClass(m.Roles[0]) {
			out.None = append(out.None, m)
			out.NoneDevices += m.Devices
			continue
		}
		if len(m.Roles) == 1 {
			out.ByRole[m.Role] = append(out.ByRole[m.Role], m)
			out.Devices[m.Role] += m.Devices
			continue
		}
		// Several roles: the card shows in each role's pane with that role's devices.
		for _, role := range m.Roles {
			rm := m
			rm.Role, rm.Devices, rm.NoRole, rm.Also = role, counts[p.Key][role], counts[p.Key][""], nil
			for _, o := range m.Roles {
				if o != role {
					rm.Also = append(rm.Also, roleCount{o, counts[p.Key][o]})
				}
			}
			out.ByRole[role] = append(out.ByRole[role], rm)
			out.Devices[role] += rm.Devices
		}
	}
	// Built-in boards first in catalog order, then attached models by device count.
	for role, ms := range out.ByRole {
		sort.SliceStable(ms, func(a, b int) bool {
			if ms[a].Known != ms[b].Known {
				return ms[a].Known
			}
			if ms[a].Known {
				return order[ms[a].Key] < order[ms[b].Key]
			}
			return ms[a].Devices > ms[b].Devices
		})
		out.ByRole[role] = ms
	}
	return out
}

// hwCandidates are the models that can be attached to role: every stock model not
// already in it (attaching adds the role; the model keeps its others).
func (d hwData) hwCandidates(role string) []hwModel {
	var out []hwModel
	for _, m := range d.Stock {
		if !slices.Contains(m.Roles, role) {
			out = append(out, m)
		}
	}
	return out
}
