package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Lineup LU1 (picked 30 Oct 2026, "Fleet's twin"): Fleet's layout. The rail lists All
// products, each product and Setup (models without a role, factory runs); All products is
// one table of every product's firmware, MDM client and app; a product is plain sections,
// each a small table: Needs a look, Models, Firmware, MDM client, App. The banner, cards,
// model picture cards, firmware timeline and rail ring/bars of L1 are gone; every form
// and route they used is kept.

// luBar is an "n of m" bar drawn like Fleet's battery bar.
type luBar struct {
	On, Of, Pct int
	Tone        string // battery-ok | battery-mid | battery-low
}

func mkLuBar(on, of int) luBar {
	b := luBar{On: on, Of: of}
	if of > 0 {
		b.Pct = on * 100 / of
	}
	switch {
	case b.Pct >= 80:
		b.Tone = "battery-ok"
	case b.Pct >= 40:
		b.Tone = "battery-mid"
	default:
		b.Tone = "battery-low"
	}
	if b.Pct < 3 && of > 0 {
		b.Pct = 3 // a sliver, so 0 of 9 still reads as a bar
	}
	return b
}

// luRow is one product on All products.
type luRow struct {
	Key, Label    string
	Devices       int
	Models        int
	FwBuild       string // prod build ("" = none set)
	FwNote        string // "maker's firmware" | "lab only" | "no prod firmware"
	Fw            luBar  // deployed devices on prod, of those it governs
	ClientLabel   string
	ClientVersion string
	Client        luBar
	HasClient     bool
	AppName       string
	App           luBar
	HasApp        bool
	Todo          int
	On            bool
}

// luNeed is one Needs a look row: the old banner's to-dos and the rail's looks in one list.
type luNeed struct {
	Tone, Title, Sub string
	Action, Href     string
	Promote          bool
	Model            string
	ReleaseID        int
	Confirm          string
}

// luFwRow is one model of ours in the Firmware section.
type luFwRow struct {
	M        pipeModel
	Since    string // "since 21 Oct · Muhammadali"
	Deployed luBar
}

// luClRow is one MDM client the product's devices run.
type luClRow struct {
	C      *ClientSlotView
	OnIt   luBar
	Auto   string // On | Paused | Off
	Window string
}

// luProductView is a product's sections.
type luProductView struct {
	Needs       []luNeed
	Fw          []luFwRow
	Clients     []luClRow
	App         lineupApp
	AppBar      luBar
	AutoInstall bool
}

// luRoleData is what one product's lineup is built from.
type luRoleData struct {
	Models  []pipeModel
	Clients []*ClientSlotView
	App     lineupApp
	Pane    luPane
}

// lineupRoleData gathers a product's models, MDM clients and app, and its L1 pane (whose
// to-dos and looks feed Needs a look and the All products count).
func (h *Handler) lineupRoleData(ctx context.Context, r *http.Request, role, label string, devices []db.Device, st *roleAppState, hw hwData, canEdit bool) luRoleData {
	var d luRoleData
	d.Models = h.pipelineModels(ctx, role, devices, st)
	var roleDevs []db.Device
	for i := range devices {
		if !devices[i].Hidden && !devices[i].Retired() && devices[i].Class() == role {
			roleDevs = append(roleDevs, devices[i])
		}
	}
	views, _ := h.clientSlotSummaries(r, roleDevs)
	for _, slot := range clientSlotOrder {
		v := views[slot]
		if v == nil || v.Devices == 0 {
			continue
		}
		var behind []string
		for i := range roleDevs {
			dv := &roleDevs[i]
			real := agentSlotFor(dv)
			if clientDisplaySlot(real) != slot {
				continue
			}
			_, code := clientVersionOf(dv)
			if real != slot {
				if h.foldedStanding(dv, real, code) == "behind" {
					behind = append(behind, dv.SerialNumber)
				}
				continue
			}
			if v.Hosted && code > 0 && versionComparable(dv, v.Build.Package) && code < v.Build.VersionCode {
				behind = append(behind, dv.SerialNumber)
			}
		}
		v.BehindSerials = strings.Join(behind, ",")
		d.Clients = append(d.Clients, v)
	}
	d.App = h.lineupApp(ctx, role, devices)
	hasApp := map[uuid.UUID]bool{}
	if d.App.Base != "" {
		if fv, err := h.db.FamilyVersions(ctx, []string{d.App.Base}); err == nil {
			for id, pkgs := range fv {
				if len(pkgs) > 0 {
					hasApp[id] = true
				}
			}
		}
	}
	full, deployed := lineupFullOn(devices, role, d.Models, views, d.App.Base, hasApp,
		func(dv *db.Device) bool { _, in := st.Labs[dv.ID]; return in })
	d.Pane = buildLineupPane(luInputs{RoleKey: role, RoleLabel: label, Models: d.Models, HW: hw.ByRole[role],
		App: d.App, Clients: d.Clients, Full: full, Deployed: deployed, CanEdit: canEdit})
	return d
}

// luNeedsOf merges the pane's to-dos (things to decide) and looks (things to watch).
func luNeedsOf(p luPane, role string) []luNeed {
	var out []luNeed
	for _, t := range p.Banner.Todos {
		n := luNeed{Tone: "warn", Title: t.Title, Sub: t.Sub, Action: t.Action, Href: t.Href,
			Promote: t.Promote, Model: t.Model, ReleaseID: t.ReleaseID, Confirm: t.Confirm}
		if strings.HasPrefix(n.Href, "#fw-") {
			n.Href = "#firmware"
		}
		out = append(out, n)
	}
	for _, l := range p.Rail.Looks {
		act := "Devices"
		if strings.HasPrefix(l.Href, "/clients") {
			act = "MDM clients"
		}
		tone := l.Tone
		if tone == "" || tone == "info" {
			tone = "muted"
		}
		out = append(out, luNeed{Tone: tone, Title: l.Title, Sub: l.Sub, Action: act, Href: l.Href})
	}
	return out
}

// luRowOf is a product's line on All products.
func luRowOf(role, label string, hw hwData, d luRoleData) luRow {
	row := luRow{Key: role, Label: label, Devices: hw.Devices[role], Models: len(hw.ByRole[role])}
	on, total := 0, 0
	for _, m := range d.Models {
		if m.Mode == "prod" && m.Channel != nil && m.Channel.Version != "" {
			if row.FwBuild == "" {
				row.FwBuild = m.Channel.Version
			} else if row.FwBuild != m.Channel.Version {
				row.FwBuild += " · " + m.Channel.Version
			}
			on, total = on+m.On, total+m.Total
		}
	}
	switch {
	case row.FwBuild != "":
		row.Fw = mkLuBar(on, total)
	default:
		row.FwNote = "maker's firmware"
		for _, m := range d.Models {
			if m.Mode == "lab" {
				row.FwNote = "lab only"
			} else if m.Mode == "prod" && row.FwNote != "lab only" {
				row.FwNote = "no prod firmware"
			}
		}
	}
	for _, c := range d.Clients {
		if !c.Hosted {
			continue
		}
		row.HasClient, row.ClientLabel, row.ClientVersion = true, c.Label, c.Build.Version
		row.Client = mkLuBar(c.UpToDate, c.Devices)
		break
	}
	if d.App.Base != "" {
		row.HasApp, row.AppName = true, d.App.Name
		row.App = mkLuBar(d.App.On, d.App.Total)
	}
	row.Todo = len(d.Pane.Banner.Todos) + len(d.Pane.Rail.Looks)
	return row
}

// luProductOf is a product's sections.
func luProductOf(role string, d luRoleData, autoInstall bool) luProductView {
	v := luProductView{Needs: luNeedsOf(d.Pane, role), App: d.App, AppBar: mkLuBar(d.App.On, d.App.Total), AutoInstall: autoInstall}
	for _, m := range d.Models {
		if m.Mode == "off" {
			continue
		}
		fr := luFwRow{M: m}
		if m.Channel != nil {
			fr.Since = "since " + m.Channel.SetAt.Format("2 Jan")
			if m.Channel.SetBy != "" {
				by := m.Channel.SetBy
				if i := strings.IndexByte(by, '@'); i > 0 {
					by = by[:i]
				}
				fr.Since += " · " + by
			}
			fr.Deployed = mkLuBar(m.On, m.Total)
		}
		v.Fw = append(v.Fw, fr)
	}
	for _, c := range d.Clients {
		cr := luClRow{C: c, OnIt: mkLuBar(c.UpToDate, c.Devices), Auto: "Off"}
		if c.Auto.On {
			cr.Auto = "On"
			if c.Auto.Paused {
				cr.Auto = "Paused"
			}
			cr.Window = "any time"
			if c.Auto.WindowStart != "" && c.Auto.WindowEnd != "" {
				cr.Window = fmt.Sprintf("%s–%s local", c.Auto.WindowStart, c.Auto.WindowEnd)
			}
		}
		v.Clients = append(v.Clients, cr)
	}
	return v
}
