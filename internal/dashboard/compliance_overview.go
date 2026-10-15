package dashboard

import (
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── Compliance overview (C1, picked 15 Oct) ───────────────────────────────────
//
// Checks first: what is failing and on how many devices, each opening onto the devices
// that fail it. Venues and Devices are tabs of the same page. The numbers count devices
// held to at least one required check; devices only held to recommendations (in a lab
// session, not at a venue) are counted apart, and "partly measurable" devices (whose
// client reports none of the device checks, e.g. v2.0 images) are said to be so.

type compFailView struct {
	CheckID, Name, Detail, Level string
	Since                        time.Time
}

type compDeviceView struct {
	ID         uuid.UUID
	Serial     string
	Venue      string
	Required   bool // held to at least one required check
	Compliant  bool
	Partly     bool // every device-reported check is unknown
	Fails      []compFailView
	Warns      []compFailView // recommended checks failing
	Since      time.Time      // oldest failing required check
	Unknown    int
}

type compCheckView struct {
	compCheck
	Level  string // the baseline's level
	Counts compCounts
	Failing []compDeviceFail
}

type compDeviceFail struct {
	Serial, Venue, Detail, Level string
	Since                        time.Time
}

type compVenueView struct {
	Name                      string
	ID                        *uuid.UUID
	Devices, Compliant, Fails int
	Required                  int // devices held to at least one required check
	Partly                    int
	Worst                     string
	Pct                       int // over Required; -1 when none is
}

func (h *Handler) ComplianceIndex(w http.ResponseWriter, r *http.Request) {
	if h.role(r) == "owner" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	results, err := h.db.ListComplianceResults(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	acc := h.access(r)

	byDev := map[uuid.UUID][]db.ComplianceResult{}
	for k, res := range results {
		if acc.visible(k.DeviceID) {
			byDev[k.DeviceID] = append(byDev[k.DeviceID], res)
		}
	}
	checkByID := map[string]compCheck{}
	for _, c := range complianceChecks {
		checkByID[c.ID] = c
	}

	var devices []compDeviceView
	checkViews := map[string]*compCheckView{}
	for _, c := range complianceChecks {
		checkViews[c.ID] = &compCheckView{compCheck: c, Level: rules.baseSetting(c).Level}
	}
	for i := range devs {
		d := &devs[i]
		rs := byDev[d.ID]
		if d.Hidden || d.Retired() || len(rs) == 0 {
			continue
		}
		v := compDeviceView{ID: d.ID, Serial: d.SerialNumber, Venue: d.RestaurantName, Compliant: true}
		deviceChecks, deviceUnknown := 0, 0
		for _, res := range rs {
			c, ok := checkByID[res.CheckID]
			if !ok {
				continue
			}
			cv := checkViews[c.ID]
			cv.Counts.Total++
			switch res.State {
			case db.CompliancePass:
				cv.Counts.Pass++
			case db.ComplianceFail:
				cv.Counts.Fail++
				cv.Failing = append(cv.Failing, compDeviceFail{Serial: d.SerialNumber, Venue: d.RestaurantName, Detail: res.Detail, Level: res.Level, Since: res.Since})
			case db.ComplianceUnknown:
				cv.Counts.Unknown++
				v.Unknown++
			default:
				cv.Counts.NA++
			}
			if res.Level == db.ComplianceRequired {
				v.Required = true
			}
			if c.Device && res.State != db.ComplianceNA {
				deviceChecks++
				if res.State == db.ComplianceUnknown {
					deviceUnknown++
				}
			}
			if res.State == db.ComplianceFail {
				f := compFailView{CheckID: c.ID, Name: c.Name, Detail: res.Detail, Level: res.Level, Since: res.Since}
				if res.Level == db.ComplianceRequired {
					v.Fails = append(v.Fails, f)
					v.Compliant = false
					if v.Since.IsZero() || res.Since.Before(v.Since) {
						v.Since = res.Since
					}
				} else {
					v.Warns = append(v.Warns, f)
				}
			}
		}
		v.Partly = deviceChecks > 0 && deviceUnknown == deviceChecks
		devices = append(devices, v)
	}

	// Headline numbers, over devices held to at least one required check.
	var inScope, compliant, partly, recOnly int
	for _, v := range devices {
		if !v.Required {
			recOnly++
			continue
		}
		inScope++
		if v.Compliant {
			compliant++
		}
		if v.Partly {
			partly++
		}
	}
	pct := 0
	if inScope > 0 {
		pct = compliant * 100 / inScope
	}

	// Checks: failing first (by count), then the registry's order.
	var checks []compCheckView
	for _, c := range complianceChecks {
		cv := checkViews[c.ID]
		if cv.Level == db.ComplianceOff && cv.Counts.Total == 0 {
			continue
		}
		sort.Slice(cv.Failing, func(i, j int) bool { return cv.Failing[i].Since.Before(cv.Failing[j].Since) })
		checks = append(checks, *cv)
	}
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Counts.Fail > checks[j].Counts.Fail })

	// Venues: one row per venue, devices with no venue last.
	vmap := map[string]*compVenueView{}
	var vorder []string
	worst := map[string]map[string]int{}
	devIdx := make(map[uuid.UUID]int, len(devices))
	for i := range devices {
		devIdx[devices[i].ID] = i
	}
	for i := range devs {
		d := &devs[i]
		j, ok := devIdx[d.ID]
		if !ok {
			continue
		}
		v := &devices[j]
		key := ""
		if d.RestaurantID != nil {
			key = d.RestaurantID.String()
		}
		vv := vmap[key]
		if vv == nil {
			vv = &compVenueView{Name: d.RestaurantName, ID: d.RestaurantID}
			if key == "" {
				vv.Name = "Not at a venue"
			}
			vmap[key] = vv
			vorder = append(vorder, key)
			worst[key] = map[string]int{}
		}
		vv.Devices++
		if v.Required {
			vv.Required++
			if v.Compliant {
				vv.Compliant++
			} else {
				vv.Fails++
			}
		}
		if v.Partly {
			vv.Partly++
		}
		for _, f := range v.Fails {
			worst[key][f.Name]++
		}
	}
	var venues []compVenueView
	for _, k := range vorder {
		vv := vmap[k]
		vv.Pct = -1
		if vv.Required > 0 {
			vv.Pct = vv.Compliant * 100 / vv.Required
		}
		best := 0
		for name, n := range worst[k] {
			if n > best || (n == best && name < vv.Worst) {
				vv.Worst, best = name, n
			}
		}
		venues = append(venues, *vv)
	}
	sort.SliceStable(venues, func(i, j int) bool {
		if (venues[i].ID == nil) != (venues[j].ID == nil) {
			return venues[j].ID == nil
		}
		pi, pj := venues[i].Pct, venues[j].Pct
		if pi < 0 {
			pi = 101 // recommendations only: after every scored venue
		}
		if pj < 0 {
			pj = 101
		}
		return pi < pj
	})

	// Devices tab: a filter on the verdict and, optionally, one check.
	filter := r.URL.Query().Get("show")
	checkF := r.URL.Query().Get("check")
	var list []compDeviceView
	counts := map[string]int{}
	for _, v := range devices {
		st := "compliant"
		switch {
		case !v.Compliant:
			st = "failing"
		case v.Partly || v.Unknown > 0:
			st = "unknown"
		}
		if !v.Required {
			st = "recommended"
		}
		counts[st]++
		if filter == "" {
			filter = "failing"
		}
		if filter != "all" && st != filter {
			continue
		}
		if checkF != "" && !compFailsCheck(v, checkF) {
			continue
		}
		list = append(list, v)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if len(list[i].Fails) != len(list[j].Fails) {
			return len(list[i].Fails) > len(list[j].Fails)
		}
		return list[i].Serial < list[j].Serial
	})
	checkName := ""
	if c, ok := checkByID[checkF]; ok {
		checkName = c.Name
	}

	tab := r.URL.Query().Get("tab")
	if tab != "venues" && tab != "devices" {
		tab = "checks"
	}
	h.render(w, r, "compliance.html", map[string]any{
		"Title":     "Compliance",
		"Tab":       tab,
		"Pct":       pct,
		"InScope":   inScope,
		"Compliant": compliant,
		"Failing":   inScope - compliant,
		"Partly":    partly,
		"RecOnly":   recOnly,
		"Checks":    checks,
		"Venues":    venues,
		"List":      list,
		"Show":      filter,
		"CheckF":    checkF,
		"CheckName": checkName,
		"Counts":    counts,
		"Scored":    len(devices),
	})
}

func compFailsCheck(v compDeviceView, id string) bool {
	for _, f := range v.Fails {
		if f.CheckID == id {
			return true
		}
	}
	for _, f := range v.Warns {
		if f.CheckID == id {
			return true
		}
	}
	return false
}

// ── Device page: the Compliance tab ───────────────────────────────────────────

type compDeviceRow struct {
	compCheck
	Result db.ComplianceResult
	Scored bool // false: the check is off for this device
}

// DeviceCompliancePartial is the device page's Compliance tab: every check, what this
// device scored, at which level and since when.
func (h *Handler) DeviceCompliancePartial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	device, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	rs, err := h.db.DeviceComplianceResults(ctx, device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	by := map[string]db.ComplianceResult{}
	for _, res := range rs {
		by[res.CheckID] = res
	}
	var rows []compDeviceRow
	compliant, fails, warns, unknown := true, 0, 0, 0
	for _, c := range complianceChecks {
		res, ok := by[c.ID]
		rows = append(rows, compDeviceRow{compCheck: c, Result: res, Scored: ok})
		if !ok {
			continue
		}
		switch {
		case res.State == db.ComplianceFail && res.Level == db.ComplianceRequired:
			fails++
			compliant = false
		case res.State == db.ComplianceFail:
			warns++
		case res.State == db.ComplianceUnknown:
			unknown++
		}
	}
	// Failing first, then unknown, then the rest; registry order within each.
	rank := func(x compDeviceRow) int {
		switch {
		case !x.Scored:
			return 5
		case x.Result.State == db.ComplianceFail && x.Result.Level == db.ComplianceRequired:
			return 0
		case x.Result.State == db.ComplianceFail:
			return 1
		case x.Result.State == db.ComplianceUnknown:
			return 2
		case x.Result.State == db.CompliancePass:
			return 3
		}
		return 4
	}
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i]) < rank(rows[j]) })
	h.renderCachedHTML(w, r, "device-compliance", h.withRole(r, map[string]any{
		"Device":    device,
		"Rows":      rows,
		"Scored":    len(rs) > 0,
		"Compliant": compliant,
		"Fails":     fails,
		"Warns":     warns,
		"Unknown":   unknown,
	}))
}
