package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

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
func (h *Handler) overviewNewData(r *http.Request, data map[string]any, summary db.Summary, groups []db.GroupHealth, inboxN int) {
	ctx := r.Context()
	acc := h.access(r)
	var (
		active   []db.Alert
		classOn  []db.ClassOnline
		wallDevs []db.WallDevice
		products []compRole
		hygiene  db.FleetHygiene
		hygErr   error
		wg       sync.WaitGroup
	)
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { active, _ = h.db.ListActiveAlerts(ctx, 300) })
	run(func() { classOn, _ = h.db.FleetClassOnline(ctx, h.connectedSlice(), acc.hidesDPC()) })
	run(func() { wallDevs, _ = h.db.FleetWall(ctx, acc.hidesDPC()) })
	run(func() { products, _ = h.overviewProducts(ctx, acc.hidesDPC()) })
	// The clean-up checklist is about the whole fleet list, so it is for viewers who
	// see the whole fleet.
	if !acc.hidesDevices() {
		run(func() { hygiene, hygErr = h.db.GetFleetHygiene(ctx) })
	} else {
		hygErr = errNoHygiene
	}
	wg.Wait()

	if products != nil {
		data["Products"] = products
	}
	active = acc.keepVisibleAlerts(active)
	att, attN, attCrit := buildAttention(active, inboxN)
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

	data["Venues"] = h.overviewVenueData(r, wallDevs)
	data["Compliance"] = h.overviewCompliance(r)
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
