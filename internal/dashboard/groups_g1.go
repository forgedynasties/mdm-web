package dashboard

import (
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Groups list (G1 "Fleet's twin", picked 30 Oct 2026): the Venues table's language, a rail
// of saved views (need a look, critical alerts, offline, empty), search + sort, quick chips
// and one table row per group. Delete lives in the row's ⋯ menu behind a typed name (the
// card grid's one-click × removed the AIO PK group by accident on 30 Oct).

type groupRow struct {
	G                                    db.Group
	H                                    db.GroupHealth
	HasH                                 bool
	Devices, Online, OnPct               int
	Offline, Critical, Warning, Deployed int
	Class                                string // ok | warn | danger | empty
	Why                                  []string
	Flags                                string
}

type groupsPage struct {
	Rows                                    []groupRow
	Look, Critical, Offline, Empty, AtVenue int
	Devices, OfflineDevices                 int
}

func buildGroupsG1(groups []db.Group, health map[uuid.UUID]db.GroupHealth) groupsPage {
	var p groupsPage
	for _, g := range groups {
		h, ok := health[g.ID]
		r := groupRow{G: g, H: h, HasH: ok && h.DeviceCount > 0, Devices: g.DeviceCount, Class: "empty"}
		if r.HasH {
			r.Devices = h.DeviceCount
			r.Offline, r.Critical, r.Warning, r.Deployed = h.OfflineCount, h.OpenCritical, h.OpenWarning, h.DeployedCount
			r.Online = r.Devices - r.Offline
			r.OnPct = r.Online * 100 / r.Devices
			r.Why = whyScore(h)
			switch {
			case h.Score >= 80:
				r.Class = "ok"
			case h.Score >= 50:
				r.Class = "warn"
			default:
				r.Class = "danger"
			}
		}
		var flags []string
		if r.HasH && h.Score < 80 {
			flags = append(flags, "look")
			p.Look++
		}
		if r.Critical > 0 {
			flags = append(flags, "crit")
			p.Critical++
		}
		if r.Offline > 0 {
			flags = append(flags, "offline")
			p.Offline++
		}
		if r.Deployed > 0 {
			flags = append(flags, "venue")
			p.AtVenue++
		}
		if r.Devices == 0 {
			flags = append(flags, "empty")
			p.Empty++
		}
		r.Flags = strings.Join(flags, " ")
		p.Devices += r.Devices
		p.OfflineDevices += r.Offline
		p.Rows = append(p.Rows, r)
	}
	// Worst first, like Venues: lowest score, empty groups last, then by name.
	sort.SliceStable(p.Rows, func(i, j int) bool {
		a, b := p.Rows[i], p.Rows[j]
		if a.HasH != b.HasH {
			return a.HasH
		}
		if a.HasH && a.H.Score != b.H.Score {
			return a.H.Score < b.H.Score
		}
		return strings.ToLower(a.G.Name) < strings.ToLower(b.G.Name)
	})
	return p
}
