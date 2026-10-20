package dashboard

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Compliance widget (C1, picked 19 Oct 2026): Overview's compliance card is a score, then
// what to fix. The ring is the share of devices passing every required check, over the
// devices the viewer can see that are held to at least one required check (the
// Compliance page's own number, from the same per-device evaluation, compDeviceViews).
// Under it, the three checks failing on the most devices, each with where it is
// concentrated and one action. There is no manual fix endpoint: the one auto-fix (wireless
// adb off) runs by itself in the compliance job, so the row says "auto-fix on" and every
// action is Show, onto the Compliance page's device list for that check.

const compRingR = 34 // the ring's radius in the widget's 80×80 SVG

type compWidget struct {
	InScope, Compliant, Failing int
	Unknown                     int // compliant, but some check could not be measured
	Partly                      int // no device check measurable at all (e.g. v2.0 images)
	Pct                         int
	// The ring's three arcs as stroke-dasharray/offset pairs, in SVG units.
	OkDash, FailDash, UnkDash string
	OkOff, FailOff, UnkOff    string
	HasDelta                  bool
	Delta                     int // percentage points against a week ago
	Top                       []compWidgetCheck
	MoreChecks                int // failing checks past the top three
}

type compWidgetCheck struct {
	ID, Name, Line, Href string
	Count                int
}

// overviewCompliance builds the widget; nil for viewers the Compliance page refuses, or
// when the data can't be read.
func (h *Handler) overviewCompliance(r *http.Request) *compWidget {
	if roleIsCustomer(h.role(r)) {
		return nil
	}
	ctx := r.Context()
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		return nil
	}
	results, err := h.db.ListComplianceResults(ctx)
	if err != nil {
		return nil
	}
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	if err != nil {
		return nil
	}
	devices, checkViews, _ := compDeviceViews(rules, results, devs, h.access(r).visible)
	w := buildCompWidget(devices, checkViews, rules)
	if w.InScope > 0 {
		ids := make([]uuid.UUID, 0, len(devices))
		for _, v := range devices {
			ids = append(ids, v.ID)
		}
		if pct, ok := h.compPctAt(r, ids, time.Now().Add(-7*24*time.Hour)); ok {
			w.HasDelta, w.Delta = true, w.Pct-pct
		}
	}
	return &w
}

// buildCompWidget is the widget from the evaluated devices and checks.
func buildCompWidget(devices []compDeviceView, checkViews map[string]*compCheckView, rules compRules) compWidget {
	var w compWidget
	for _, v := range devices {
		if !v.Required {
			continue
		}
		w.InScope++
		switch {
		case !v.Compliant:
			w.Failing++
		case v.Partly || v.Unknown > 0:
			w.Compliant++
			w.Unknown++
		default:
			w.Compliant++
		}
		if v.Partly {
			w.Partly++
		}
	}
	if w.InScope > 0 {
		w.Pct = w.Compliant * 100 / w.InScope
		c := 2 * math.Pi * compRingR
		arc := func(n, before int) (string, string) {
			l := c * float64(n) / float64(w.InScope)
			return fmt.Sprintf("%.2f %.2f", l, c-l), fmt.Sprintf("%.2f", -c*float64(before)/float64(w.InScope))
		}
		ok := w.Compliant - w.Unknown
		w.OkDash, w.OkOff = arc(ok, 0)
		w.UnkDash, w.UnkOff = arc(w.Unknown, ok)
		w.FailDash, w.FailOff = arc(w.Failing, ok+w.Unknown)
	}

	// Checks by how many devices fail them at the required level.
	var failing []compWidgetCheck
	for _, c := range complianceChecks {
		cv := checkViews[c.ID]
		if cv == nil {
			continue
		}
		byVenue := map[string]int{}
		n := 0
		for _, f := range cv.Failing {
			if f.Level != db.ComplianceRequired {
				continue
			}
			n++
			byVenue[f.Venue]++
		}
		if n == 0 {
			continue
		}
		top, topN := "", 0
		for v, k := range byVenue {
			if k > topN || (k == topN && v < top) {
				top, topN = v, k
			}
		}
		line := ""
		switch {
		case top == "":
		case len(byVenue) == 1:
			line = "all at " + top
		default:
			line = fmt.Sprintf("%d at %s", topN, top)
		}
		if c.Fix != "" && rules.autoFix(c) {
			if line != "" {
				line += " · "
			}
			line += "auto-fix on"
		}
		failing = append(failing, compWidgetCheck{ID: c.ID, Name: c.Name, Line: line, Count: n,
			Href: "/compliance?tab=devices&show=failing&check=" + url.QueryEscape(c.ID)})
	}
	sort.SliceStable(failing, func(i, j int) bool { return failing[i].Count > failing[j].Count })
	if len(failing) > 3 {
		w.MoreChecks, failing = len(failing)-3, failing[:3]
	}
	w.Top = failing
	return w
}

// compPctAt replays the stored compliance events to the share of devices that passed
// every required check at an earlier moment, over the same devices. ok is false when
// nothing was scored that far back (the engine started on 15 Oct 2026), so the widget
// shows no change rather than an invented one.
func (h *Handler) compPctAt(r *http.Request, ids []uuid.UUID, at time.Time) (int, bool) {
	checks := make([]string, 0, len(complianceChecks))
	for _, c := range complianceChecks {
		checks = append(checks, c.ID)
	}
	_, events, err := h.db.ComplianceHistory(r.Context(), ids, checks, at)
	if err != nil {
		return 0, false
	}
	pct, ok := compPctFromEvents(events, at)
	return pct, ok
}

// compPctFromEvents is the state each device and check was in at `at` — the last event
// before it — reduced to the share of devices held to a required check that failed none.
func compPctFromEvents(events []db.ComplianceEvent, at time.Time) (int, bool) {
	type key struct {
		dev   uuid.UUID
		check string
	}
	last := map[key]db.ComplianceEvent{}
	for _, e := range events {
		if !e.At.Before(at) {
			continue
		}
		k := key{e.DeviceID, e.CheckID}
		if p, ok := last[k]; !ok || e.At.After(p.At) {
			last[k] = e
		}
	}
	required, failing := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for k, e := range last {
		if e.Level != db.ComplianceRequired {
			continue
		}
		required[k.dev] = true
		if e.To == db.ComplianceFail {
			failing[k.dev] = true
		}
	}
	if len(required) == 0 {
		return 0, false
	}
	return (len(required) - len(failing)) * 100 / len(required), true
}
