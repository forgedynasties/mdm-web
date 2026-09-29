package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// wallDormantAfter is when an offline device stops counting as today's outage and
// shows grey on the wall, the same two weeks GroupHealth.DormantCount uses.
const wallDormantAfter = 14 * 24 * time.Hour

// wallSquare is one device on the Overview's device wall.
type wallSquare struct {
	Serial string
	State  string // on | warn | off | dormant
	Tip    string
}

// wallBlock is one restaurant's squares, or one device type's devices with no restaurant.
type wallBlock struct {
	ID      string // restaurant id; "" for the devices with no restaurant
	Name    string
	Health  string // ok | warn | bad; "" for the no-restaurant block
	Online  int
	Total   int
	Cols    int
	Squares []wallSquare
}

// mapSite is one restaurant dot on the Overview map.
type mapSite struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
	Total  int     `json:"total"`
	Online int     `json:"online"`
	Health string  `json:"health"`
	Issue  string  `json:"issue,omitempty"`
}

// wallHealth maps a GroupHealth score class onto the wall's three states.
func wallHealth(scoreClass string) string {
	switch scoreClass {
	case "danger":
		return "bad"
	case "warn":
		return "warn"
	}
	return "ok"
}

// buildWall groups every device into restaurant blocks, worst restaurant first,
// with the devices that have no restaurant last. A device is off when it is not
// in the connected set, warn when it has an active critical or warning alert.
func buildWall(devs []db.WallDevice, connected map[uuid.UUID]struct{}, alerts []db.Alert, groups []db.GroupHealth, names map[uuid.UUID]string, now time.Time) []wallBlock {
	// The worst active alert per device, for its colour and tooltip.
	type devAlert struct {
		sev   int
		issue string
	}
	byDev := map[uuid.UUID]devAlert{}
	for _, a := range alerts {
		if a.DeviceID == nil {
			continue
		}
		ha := humanizeAlert(a)
		if ha.Muted {
			continue
		}
		sev := map[string]int{"critical": 2, "warning": 1}[a.Severity]
		if sev == 0 {
			continue
		}
		if cur, ok := byDev[*a.DeviceID]; !ok || sev > cur.sev {
			byDev[*a.DeviceID] = devAlert{sev, attentionIssue(ha)}
		}
	}
	health := map[uuid.UUID]db.GroupHealth{}
	for _, g := range groups {
		if g.Deployed {
			health[g.GroupID] = g
		}
	}

	blocks := map[uuid.UUID]*wallBlock{}
	unplaced := map[string]*wallBlock{} // by device type label
	for _, d := range devs {
		sq := wallSquare{Serial: d.Serial, State: "on"}
		label := "Unassigned type"
		if d.Class != "" {
			label = product.ClassLabel(d.Class)
		}
		status := "online"
		_, online := connected[d.ID]
		switch {
		case !online && now.Sub(d.LastSeenAt) > wallDormantAfter:
			sq.State, status = "dormant", "not seen for "+minsAgo(now.Sub(d.LastSeenAt).Minutes())
		case !online:
			sq.State, status = "off", "offline · seen "+minsAgo(now.Sub(d.LastSeenAt).Minutes())+" ago"
		}
		if a, ok := byDev[d.ID]; ok {
			if sq.State == "on" {
				sq.State = "warn"
			}
			status += " · " + a.issue
		}
		sq.Tip = d.Serial + " · " + label + " · " + status

		var b *wallBlock
		if d.RestaurantID == nil {
			if b = unplaced[label]; b == nil {
				b = &wallBlock{Name: "No restaurant · " + label}
				unplaced[label] = b
			}
		} else {
			b = blocks[*d.RestaurantID]
			if b == nil {
				b = &wallBlock{ID: d.RestaurantID.String(), Name: names[*d.RestaurantID], Health: "ok"}
				if g, ok := health[*d.RestaurantID]; ok {
					b.Health = wallHealth(g.ScoreClass)
				}
				blocks[*d.RestaurantID] = b
			}
		}
		b.Squares = append(b.Squares, sq)
		b.Total++
		if sq.State == "on" || sq.State == "warn" {
			b.Online++
		}
	}

	rank := map[string]int{"bad": 0, "warn": 1, "ok": 2}
	out := make([]wallBlock, 0, len(blocks)+1)
	for _, b := range blocks {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.Health] != rank[b.Health] {
			return rank[a.Health] < rank[b.Health]
		}
		// Within a state, most devices down first.
		if da, dbn := a.Total-a.Online, b.Total-b.Online; da != dbn {
			return da > dbn
		}
		return a.Name < b.Name
	})
	rest := make([]wallBlock, 0, len(unplaced))
	for _, b := range unplaced {
		rest = append(rest, *b)
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Total != rest[j].Total {
			return rest[i].Total > rest[j].Total
		}
		return rest[i].Name < rest[j].Name
	})
	out = append(out, rest...)
	for i := range out {
		out[i].Cols = int(math.Max(3, math.Ceil(math.Sqrt(float64(out[i].Total)*1.5))))
		// Offline before alerting before online, so a block's trouble reads as one clump.
		order := map[string]int{"off": 0, "warn": 1, "dormant": 2, "on": 3}
		sort.SliceStable(out[i].Squares, func(a, b int) bool {
			return order[out[i].Squares[a].State] < order[out[i].Squares[b].State]
		})
	}
	return out
}

// mapDevice is a located device drawn on its own, because it has no located restaurant.
type mapDevice struct {
	Serial string  `json:"serial"`
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
	Online bool    `json:"online"`
}

// overviewMap is the Overview map's data: restaurants, plus loose devices.
type overviewMap struct {
	Sites   []mapSite   `json:"sites"`
	Devices []mapDevice `json:"devices"`
}

// buildMap places each restaurant on the map: its stored coordinates, or else the
// median of its located devices. Located devices whose restaurant is not on the
// map (or that have none) are drawn one by one. Returns the JSON and how many
// dots it holds.
func buildMap(blocks []wallBlock, restaurants []db.Restaurant, pts []deviceMapPoint, groups []db.GroupHealth) (template.JS, int) {
	coords := map[string][2]float64{}
	for _, r := range restaurants {
		if r.Latitude != nil && r.Longitude != nil && (*r.Latitude != 0 || *r.Longitude != 0) {
			coords[r.ID.String()] = [2]float64{*r.Latitude, *r.Longitude}
		}
	}
	lats, lngs := map[string][]float64{}, map[string][]float64{}
	for _, p := range pts {
		if p.RestaurantID != "" {
			lats[p.RestaurantID] = append(lats[p.RestaurantID], p.Lat)
			lngs[p.RestaurantID] = append(lngs[p.RestaurantID], p.Lon)
		}
	}
	issues := map[string]string{}
	for _, g := range groups {
		if g.Deployed {
			issues[g.GroupID.String()] = mainIssue(whyScore(g))
		}
	}
	sites := []mapSite{}
	for _, b := range blocks {
		if b.ID == "" {
			continue
		}
		c, ok := coords[b.ID]
		if !ok && len(lats[b.ID]) > 0 {
			c, ok = [2]float64{median(lats[b.ID]), median(lngs[b.ID])}, true
		}
		if !ok {
			continue
		}
		sites = append(sites, mapSite{ID: b.ID, Name: b.Name, Lat: c[0], Lng: c[1], Total: b.Total,
			Online: b.Online, Health: b.Health, Issue: issues[b.ID]})
	}
	placed := make(map[string]bool, len(sites))
	for _, st := range sites {
		placed[st.ID] = true
	}
	devs := []mapDevice{}
	for _, p := range pts {
		if !placed[p.RestaurantID] {
			devs = append(devs, mapDevice{Serial: p.Serial, Lat: p.Lat, Lng: p.Lon, Online: p.Online})
		}
	}
	j, err := json.Marshal(overviewMap{sites, devs})
	if err != nil {
		return template.JS("{}"), 0
	}
	return template.JS(j), len(sites) + len(devs)
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// sneakPeekWall builds a wall for the public preview from the synthetic venues:
// each venue's devices, its offline count shown as offline squares.
func sneakPeekWall(groups []db.GroupHealth) []wallBlock {
	now := time.Now()
	var devs []db.WallDevice
	names := map[uuid.UUID]string{}
	connected := map[uuid.UUID]struct{}{}
	n := 0
	for _, g := range groups {
		if !g.Deployed {
			continue
		}
		id := g.GroupID
		names[id] = g.Name
		for i := 0; i < g.DeviceCount; i++ {
			n++
			d := db.WallDevice{ID: uuid.New(), Serial: "T7-" + itoa4(n), Class: "t7", RestaurantID: &id, LastSeenAt: now}
			if i >= g.OfflineCount {
				connected[d.ID] = struct{}{}
			} else {
				d.LastSeenAt = now.Add(-time.Duration(30+i*20) * time.Minute)
			}
			devs = append(devs, d)
		}
	}
	return buildWall(devs, connected, nil, groups, names, now)
}

func itoa4(n int) string {
	b := []byte("0000")
	for i := 3; i >= 0 && n > 0; i-- {
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b)
}

// overviewCookie remembers a super admin's pick between the new Overview and the
// classic one.
const overviewCookie = "mdm_overview"

// overviewChoice is "new" unless the cookie says "classic".
func overviewChoice(r *http.Request) string {
	if c, err := r.Cookie(overviewCookie); err == nil && c.Value == "classic" {
		return "classic"
	}
	return "new"
}

// overviewNewData adds what only the new Overview reads on top of the shared
// view model: the needs-attention list, devices by type, the wall and the map.
func (h *Handler) overviewNewData(r *http.Request, data map[string]any, summary db.Summary, groups []db.GroupHealth, inboxN int) {
	ctx := r.Context()
	acc := h.access(r)
	var (
		active   []db.Alert
		classOn  []db.ClassOnline
		wallDevs []db.WallDevice
		rests    []db.Restaurant
		pts      []deviceMapPoint
		wg       sync.WaitGroup
	)
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { active, _ = h.db.ListActiveAlerts(ctx, 300) })
	run(func() { classOn, _ = h.db.FleetClassOnline(ctx, h.connectedSlice(), acc.hidesDPC()) })
	run(func() { wallDevs, _ = h.db.FleetWall(ctx, acc.hidesDPC()) })
	run(func() { rests, _ = h.db.ListRestaurants(ctx) })
	run(func() {
		f := db.DeviceFilter{}
		if acc.hidesDPC() {
			f.AgentKind = "firmware"
		}
		pts = h.devicePoints(ctx, f)
	})
	wg.Wait()

	active = acc.keepVisibleAlerts(active)
	att, attN, attCrit := buildAttention(active, inboxN)
	data["AttentionRows"] = att
	data["AttentionTotal"] = attN
	data["AttentionCritical"] = attCrit
	data["ClassOnline"] = classOnlineRows(classOn)
	if summary.Total > 0 {
		data["OnlinePct"] = fmt.Sprintf("%.1f", float64(summary.RecentlyActive)*100/float64(summary.Total))
	}

	names := make(map[uuid.UUID]string, len(rests))
	for _, rs := range rests {
		names[rs.ID] = rs.Name
	}
	wall := buildWall(wallDevs, h.hub.ConnectedIDsForDisplay(), active, groups, names, time.Now())
	data["Wall"] = wall
	data["WallTotal"] = len(wallDevs)
	data["MapData"], data["MapDots"] = buildMap(wall, rests, pts, groups)
	data["MapsEmbedKey"] = h.mapsEmbedKey
}
