package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// median is the middle of a set of coordinates (the upper middle for an even count).
func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
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
// view model: the needs-attention list, devices by type, the venues and compliance.
// The device wall and the map were removed (19 Oct 2026: the Venues widget replaced the
// wall, and the map left the Overview; /map is unchanged).
func (h *Handler) overviewNewData(r *http.Request, data map[string]any, summary db.Summary, groups []db.GroupHealth, inboxN int, crashStats db.FleetCrashStats) {
	ctx := r.Context()
	acc := h.access(r)
	var (
		active   []db.Alert
		classOn  []db.ClassOnline
		wallDevs []db.WallDevice
		products []compRole
		hygiene  db.FleetHygiene
		hygErr   error
		rollouts []db.Rollout
		wg       sync.WaitGroup
	)
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	connected := h.connectedSlice()
	run(func() { active, _ = h.db.ListActiveAlerts(ctx, 300) })
	run(func() { classOn, _ = h.db.FleetClassOnline(ctx, connected, acc.hidesDPC()) })
	run(func() { wallDevs, _ = h.db.FleetWall(ctx, acc.hidesDPC()) })
	run(func() { products, _ = h.overviewProducts(ctx, acc.hidesDPC()) })
	run(func() { rollouts, _ = h.db.RecentRollouts(ctx, 7) })
	// The clean-up checklist is about the whole fleet list, so it is for viewers who
	// see the whole fleet.
	if !acc.hidesDevices() {
		run(func() { hygiene, hygErr = h.db.GetFleetHygiene(ctx) })
	} else {
		hygErr = errNoHygiene
	}
	wg.Wait()

	// The filter pills (C, 21 Oct 2026): types present in the fleet, venues with devices.
	byCount := append([]db.ClassOnline(nil), classOn...)
	sort.SliceStable(byCount, func(i, j int) bool { return byCount[i].Total > byCount[j].Total })
	var classes []string
	for _, c := range byCount { // biggest first, like the pill chart
		if c.Class != "" && c.Total > 0 {
			classes = append(classes, c.Class)
		}
	}
	var venueOpts []ovVenueOpt
	for _, g := range groups {
		if g.DeviceCount > 0 {
			venueOpts = append(venueOpts, ovVenueOpt{g.GroupID, g.Name})
		}
	}
	sort.SliceStable(venueOpts, func(i, j int) bool {
		return strings.ToLower(venueOpts[i].Name) < strings.ToLower(venueOpts[j].Name)
	})
	sc := parseOvScope(r, "/", classes, venueOpts)
	trend, err := h.db.OverviewTrend(ctx, sc.dbScope(), ovTrendDays(sc.Window))
	if err != nil {
		log.Printf("[overview] trend: %v", err)
	}

	conn := make(map[uuid.UUID]bool, len(connected))
	for _, id := range connected {
		conn[id] = true
	}
	devs := ovDevsFromWall(wallDevs, conn)
	now := time.Now()

	if products != nil {
		data["Products"] = products
	}
	active = ovScopeAlerts(acc.keepVisibleAlerts(active), devs, sc)
	att, attN, attCrit := buildAttention(active, ovScopeInbox(inboxN, devs, sc))
	data["AttentionRows"] = att
	data["AttentionTotal"] = attN
	data["AttentionCritical"] = attCrit
	data["ClassOnline"] = classOnlineRows(classOn)
	if hygErr == nil {
		data["Hygiene"] = hygieneRows(hygiene)
	}
	if summary.Total > 0 {
		data["OnlinePct"] = fmt.Sprintf("%.1f", float64(summary.RecentlyActive)*100/float64(summary.Total))
	}

	// Venues for the banner's "worst venue" row: those inside the scope.
	scopedGroups := groups
	if sc.Class != "" {
		has := map[uuid.UUID]bool{}
		for _, d := range devs {
			if d.Venue != nil && d.Class == sc.Class {
				has[*d.Venue] = true
			}
		}
		scopedGroups = nil
		for _, g := range groups {
			if has[g.GroupID] {
				scopedGroups = append(scopedGroups, g)
			}
		}
	}
	online, total := 0, 0
	for _, d := range devs {
		if sc.keeps(d.Class, d.Venue) {
			total++
			if d.Online {
				online++
			}
		}
	}
	data["Scope"] = sc
	data["OvBanner"] = buildOvBanner(att, attN, scopedGroups, crashStats.ByRestaurant, trend, sc, online, total)
	data["OvStats"] = buildOvStats(trend, devs, sc, now)
	data["OvPills"] = buildOvPills(devs, active, sc)
	data["OvRollouts"] = buildOvRollouts(rollouts, now)

	// The rail greets by first name (O2, 20 Oct).
	if u, err := h.db.GetUserByUsername(ctx, h.currentUsername(r)); err == nil && u != nil {
		data["FirstName"] = u.FirstName
	}
	data["Venues"] = h.overviewVenueData(r, wallDevs, sc)
	data["Compliance"] = h.overviewCompliance(r)
}

// classShort is the tile text for a device class on the Overview's type chips.
func classShort(class string) string {
	switch class {
	case "t7":
		return "T7"
	case "kiosk":
		return "K"
	case "dongle":
		return "MB"
	case "kds":
		return "KDS"
	case "pos":
		return "POS"
	case "mpos":
		return "HH"
	case "payment":
		return "PAY"
	case "tablet":
		return "TAB"
	case "":
		return "?"
	}
	if len(class) > 3 {
		return strings.ToUpper(class[:3])
	}
	return strings.ToUpper(class)
}

// compRole is one slice of the Overview's fleet-composition bar.
type compRole struct {
	Class, Label string
	Count        int
}

// overviewProducts is the fleet composition by product role (Menu board, Tableside
// AI, …), the same axis as the fleet rail, not by hardware model. Most devices
// first; unassigned last. Shared by the classic and the new Overview's hero.
func (h *Handler) overviewProducts(ctx context.Context, excludeDPC bool) ([]compRole, bool) {
	cc, _, _, err := h.db.FleetComposition(ctx, excludeDPC)
	if err != nil {
		return nil, false
	}
	return compRolesFrom(cc), true
}

// compRolesFrom is the composition from per-class counts: most devices first,
// unassigned last. The sneak peek feeds it its sample counts.
func compRolesFrom(cc []db.ClassCount) []compRole {
	var roles []compRole
	unassigned := 0
	for _, c := range cc {
		if c.Class == "" {
			unassigned += c.N
			continue
		}
		roles = append(roles, compRole{c.Class, product.ClassLabel(c.Class), c.N})
	}
	sort.SliceStable(roles, func(i, j int) bool { return roles[i].Count > roles[j].Count })
	if unassigned > 0 {
		roles = append(roles, compRole{"", "Unassigned", unassigned})
	}
	return roles
}
