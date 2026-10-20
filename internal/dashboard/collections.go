package dashboard

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// collMember is one collection (a venue, a group or a release) and its devices, the
// input to the collection table that Venues and Fleet share (venues-demos.html L1; Fleet
// picked the same table for its Restaurants, Groups and Releases views on 15 Oct).
type collMember struct {
	ID, Name, Sub, Href, Text string
	Health                    *db.GroupHealth
	Devices                   []db.Device
}

// collRows builds the table rows for one kind: "restaurants", "groups" or "releases".
func (h *Handler) collRows(r *http.Request, kind string) ([]venueRow, venueSummary) {
	ctx := r.Context()
	devices, _ := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	devices = h.access(r).keepVisible(devices)
	var items []collMember
	switch kind {
	case "restaurants":
		rests, _ := h.db.ListRestaurants(ctx)
		health := map[uuid.UUID]db.GroupHealth{}
		if rhs, err := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7); err == nil {
			for _, rh := range rhs {
				health[rh.GroupID] = rh
			}
		}
		by := map[uuid.UUID][]db.Device{}
		for _, d := range devices {
			if d.RestaurantID != nil {
				by[*d.RestaurantID] = append(by[*d.RestaurantID], d)
			}
		}
		for _, rest := range rests {
			sub := rest.Address
			if sub == "" {
				sub = "no address"
			}
			if rest.Timezone != "" {
				sub += " · " + rest.Timezone
			}
			m := collMember{ID: rest.ID.String(), Name: rest.Name, Sub: sub, Href: "/restaurants/" + rest.ID.String(),
				Text: rest.Name + " " + rest.Address + " " + rest.Timezone, Devices: by[rest.ID]}
			if hh, ok := health[rest.ID]; ok {
				m.Health = &hh
			}
			items = append(items, m)
		}
	case "groups":
		groups, _ := h.db.ListGroups(ctx)
		ids := make([]uuid.UUID, 0, len(groups))
		for _, g := range groups {
			ids = append(ids, g.ID)
		}
		members, _ := h.db.GetGroupDeviceIDsBatch(ctx, ids)
		health := map[uuid.UUID]db.GroupHealth{}
		if ghs, err := h.db.GetGroupHealth(ctx, h.connectedSlice()); err == nil {
			for _, gh := range ghs {
				health[gh.GroupID] = gh
			}
		}
		byID := make(map[uuid.UUID]db.Device, len(devices))
		for _, d := range devices {
			byID[d.ID] = d
		}
		for _, g := range groups {
			m := collMember{ID: g.ID.String(), Name: g.Name, Sub: "group", Href: "/groups/" + g.ID.String(), Text: g.Name}
			for _, id := range members[g.ID] {
				if d, ok := byID[id]; ok {
					m.Devices = append(m.Devices, d)
				}
			}
			if hh, ok := health[g.ID]; ok {
				m.Health = &hh
			}
			items = append(items, m)
		}
	case "releases":
		rels, _ := h.db.ListPublishedReleasesForRail(ctx)
		rels = visibleRail(h.role(r), rels)
		by := map[string][]db.Device{}
		for _, d := range devices {
			by[d.BuildID] = append(by[d.BuildID], d)
		}
		for _, rel := range rels {
			name := rel.Version
			sub := "release"
			if rel.Name != "" && rel.Name != rel.Version {
				name, sub = rel.Name, rel.Version
			}
			if rel.IsDev {
				sub += " · dev"
			}
			items = append(items, collMember{ID: strconv.Itoa(rel.ID), Name: name, Sub: sub, Href: "/releases/" + strconv.Itoa(rel.ID),
				Text: rel.Name + " " + rel.Version, Devices: by[rel.Version]})
		}
	}
	return h.rowsFor(items)
}

// rowsFor turns collections into table rows: a square per device (problems first),
// online / offline / dormant counts, the health score where there is one, and when the
// collection last checked in.
func (h *Handler) rowsFor(items []collMember) ([]venueRow, venueSummary) {
	return collRowsFrom(items, h.onlineMap(), time.Duration(h.cfg.CheckinInterval()*3)*time.Second)
}

// collRowsFrom is rowsFor given the live set and the staleness window, so the sneak
// peek can build the same rows from its sample devices.
func collRowsFrom(items []collMember, online map[uuid.UUID]bool, stale time.Duration) ([]venueRow, venueSummary) {
	rows := make([]venueRow, 0, len(items))
	var sum venueSummary
	for _, it := range items {
		v := venueRow{ID: it.ID, Name: it.Name, Sub: it.Sub, Href: it.Href, Text: strings.TrimSpace(it.Text), Class: "empty"}
		if it.Health != nil {
			v.H, v.HasH = *it.Health, true
		}
		var pips []string
		for _, d := range it.Devices {
			if d.LastSeenAt.After(v.Last) {
				v.Last = d.LastSeenAt
			}
			switch age := time.Since(d.LastSeenAt); {
			case online[d.ID] || age < stale:
				v.Online++
				pips = append(pips, "on")
			case age > 14*24*time.Hour:
				v.Dormant++
				pips = append(pips, "dormant")
			default:
				v.Offline++
				pips = append(pips, "off")
			}
		}
		v.Devices = len(it.Devices)
		sort.SliceStable(pips, func(i, j int) bool { return pipRank[pips[i]] < pipRank[pips[j]] })
		if len(pips) > 20 {
			v.More, pips = len(pips)-20, pips[:20]
		}
		v.Pips = pips
		if v.Devices > 0 && v.HasH {
			switch {
			case v.H.Score >= 80:
				v.Class = "ok"
			case v.H.Score >= 50:
				v.Class = "warn"
			default:
				v.Class = "danger"
			}
		}
		// No health score (releases): a collection needs a look when a device is down.
		v.NeedsLook = v.Devices > 0 && ((v.HasH && v.H.Score < 80) || (!v.HasH && v.Offline > 0))
		sum.Devices += v.Devices
		sum.Offline += v.Offline
		if v.NeedsLook {
			sum.NeedLook++
		}
		if v.Offline > 0 {
			sum.WithOffline++
		}
		if v.Devices == 0 {
			sum.Empty++
		}
		if v.HasH {
			sum.Critical += v.H.OpenCritical
		}
		rows = append(rows, v)
	}
	return rows, sum
}
