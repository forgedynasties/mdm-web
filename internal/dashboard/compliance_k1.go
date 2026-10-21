package dashboard

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Compliance K1 (picked 21 Oct 2026, from the Compliance demos): C1's checks-first page
// in the Overview's language. A banner says what to fix first and who fixes it (auto-fix,
// a rollout, or a person); Compliant / Auto-fix / Measurable cards say what changed since
// last week; the check table keeps C1's order and drill-down, grouped as the registry is,
// with an auto-fix chip per check; the rail has the ring, a 14-day trend, the venues
// behind and the venue report. Venues and Devices stay tabs. The trend and the deltas are
// replayed from the stored state changes (compliance_events), the auto-fix counts from
// the commands the server sent by itself.

// k1Day is one day of the replayed history: the devices held to a required check that
// end of day, how many met them all, and how many were measured on server checks only.
type k1Day struct {
	Day                        time.Time
	InScope, Compliant, Partly int
}

func (d k1Day) Pct() int {
	if d.InScope == 0 {
		return 0
	}
	return d.Compliant * 100 / d.InScope
}

// compTrend replays the last `days` days for the given devices: a device/check's state at
// a moment is the last stored change at or before it (ComplianceHistory includes the
// last change before the period, so the state at its start is known). A device with no
// change yet at that moment was not scored then. deviceCheck says which checks the
// device reports (for "partly measurable").
func compTrend(ids []uuid.UUID, events []db.ComplianceEvent, deviceCheck map[string]bool, now time.Time, days int) []k1Day {
	type change struct {
		at         time.Time
		state, lvl string
	}
	per := map[uuid.UUID]map[string][]change{}
	want := map[uuid.UUID]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, e := range events {
		if !want[e.DeviceID] {
			continue
		}
		if per[e.DeviceID] == nil {
			per[e.DeviceID] = map[string][]change{}
		}
		per[e.DeviceID][e.CheckID] = append(per[e.DeviceID][e.CheckID], change{e.At, e.To, e.Level})
	}
	for _, m := range per {
		for _, cs := range m {
			sort.SliceStable(cs, func(i, j int) bool { return cs[i].at.Before(cs[j].at) })
		}
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := make([]k1Day, 0, days)
	for i := days - 1; i >= 0; i-- {
		start := dayStart.AddDate(0, 0, -i)
		at := start.AddDate(0, 0, 1)
		if at.After(now) {
			at = now
		}
		d := k1Day{Day: start}
		for _, id := range ids {
			checks := per[id]
			required, failing := false, false
			devChecks, devUnknown := 0, 0
			for cid, cs := range checks {
				st, lvl := "", ""
				for _, c := range cs {
					if c.at.After(at) {
						break
					}
					st, lvl = c.state, c.lvl
				}
				if st == "" {
					continue
				}
				if lvl == db.ComplianceRequired {
					required = true
					if st == db.ComplianceFail {
						failing = true
					}
				}
				if deviceCheck[cid] && st != db.ComplianceNA {
					devChecks++
					if st == db.ComplianceUnknown {
						devUnknown++
					}
				}
			}
			if !required {
				continue
			}
			d.InScope++
			if !failing {
				d.Compliant++
			}
			if devChecks > 0 && devUnknown == devChecks {
				d.Partly++
			}
		}
		out = append(out, d)
	}
	return out
}

// ── Scope: the Venue and Type pills, and the Level pill on the checks ─────────

type k1Scope struct {
	Venue, VenueName string
	Class, ClassName string
	Level            string // "" | required | recommended
	Tab              string
	Venues, Types    []ovOpt
	Levels           []ovOpt
	On               bool
}

// q is the page's query for a tab with the current filters, plus extra pairs.
func (s k1Scope) q(tab string, extra ...string) string {
	v := url.Values{}
	if tab != "" && tab != "checks" {
		v.Set("tab", tab)
	}
	if s.Venue != "" {
		v.Set("venue", s.Venue)
	}
	if s.Class != "" {
		v.Set("type", s.Class)
	}
	if s.Level != "" && (tab == "" || tab == "checks") {
		v.Set("level", s.Level)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	if len(v) == 0 {
		return "/compliance"
	}
	return "/compliance?" + v.Encode()
}

// TabHref keeps the venue and type filters across the tabs.
func (s k1Scope) TabHref(tab string) string { return s.q(tab) }

func parseK1Scope(q url.Values, tab string, venues map[string]string, classes []string) k1Scope {
	s := k1Scope{Tab: tab}
	if v := q.Get("venue"); venues[v] != "" {
		s.Venue, s.VenueName = v, venues[v]
	}
	if t := q.Get("type"); t != "" {
		for _, c := range classes {
			if c == t {
				s.Class, s.ClassName = c, product.ClassLabel(c)
			}
		}
	}
	if l := q.Get("level"); l == db.ComplianceRequired || l == db.ComplianceRecommended {
		s.Level = l
	}
	s.On = s.Venue != "" || s.Class != ""
	with := func(mod func(*k1Scope)) string {
		c := s
		mod(&c)
		return c.q(tab)
	}
	s.Venues = append(s.Venues, ovOpt{"All venues", with(func(c *k1Scope) { c.Venue = "" }), s.Venue == ""})
	type kv struct{ id, name string }
	var vs []kv
	for id, n := range venues {
		vs = append(vs, kv{id, n})
	}
	sort.Slice(vs, func(i, j int) bool { return strings.ToLower(vs[i].name) < strings.ToLower(vs[j].name) })
	for _, v := range vs {
		v := v
		s.Venues = append(s.Venues, ovOpt{v.name, with(func(c *k1Scope) { c.Venue = v.id }), s.Venue == v.id})
	}
	s.Types = append(s.Types, ovOpt{"All types", with(func(c *k1Scope) { c.Class = "" }), s.Class == ""})
	for _, cl := range classes {
		cl := cl
		s.Types = append(s.Types, ovOpt{product.ClassLabel(cl), with(func(c *k1Scope) { c.Class = cl }), s.Class == cl})
	}
	for _, l := range [][2]string{{"", "All levels"}, {db.ComplianceRequired, "Required"}, {db.ComplianceRecommended, "Recommended"}} {
		l := l
		s.Levels = append(s.Levels, ovOpt{l[1], with(func(c *k1Scope) { c.Level = l[0] }), s.Level == l[0]})
	}
	return s
}

// ── Banner ────────────────────────────────────────────────────────────────────

type k1Todo struct {
	Title, Sub, Who string
	Action, Href    string
}

type k1Banner struct {
	Tone, Kicker, Headline string
	Todos                  []k1Todo
	More                   int
	Calm                   string
}

const k1TodoRows = 3

// k1FirmwareSplit counts failing devices on another firmware line (they need a rollout
// someone watches) and those behind on their line (prod moves them in their window),
// from the check's detail "<build>; prod is <version>".
func k1FirmwareSplit(fails []compDeviceFail) (otherLine, behind int) {
	for _, f := range fails {
		build, prodV, ok := strings.Cut(f.Detail, "; prod is ")
		if ok && firmwareLine(build) != "" && firmwareLine(build) != firmwareLine(prodV) {
			otherLine++
		} else {
			behind++
		}
	}
	return
}

// k1TopVenues is "5 at Mom's Biryani · 2 at Harbour Grill" over failing devices.
func k1TopVenues(fails []compDeviceFail, n int) string {
	count := map[string]int{}
	for _, f := range fails {
		v := f.Venue
		if v == "" {
			v = "no venue"
		}
		count[v]++
	}
	type kv struct {
		v string
		n int
	}
	var vs []kv
	for v, c := range count {
		vs = append(vs, kv{v, c})
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].n != vs[j].n {
			return vs[i].n > vs[j].n
		}
		return vs[i].v < vs[j].v
	})
	var parts []string
	for i, v := range vs {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%d at %s", v.n, v.v))
	}
	return strings.Join(parts, " · ")
}

// buildK1Banner lists the required checks that fail, most devices first, with who deals
// with each: auto-fix (wireless adb, when it is on), a rollout (devices on another
// firmware line), prod (devices behind on their line move by themselves), or a person.
func buildK1Banner(checks []compCheckView, rules compRules, fixesWeek map[string]int, sc k1Scope, pct, failing, inScope, partly int) k1Banner {
	b := k1Banner{Tone: "calm", Kicker: fmt.Sprintf("Compliance · %d%% · %d failing a required check", pct, failing)}
	var rows []compCheckView
	for _, c := range checks {
		if c.Level == db.ComplianceRequired && c.Counts.Fail > 0 {
			rows = append(rows, c)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Counts.Fail > rows[j].Counts.Fail })
	devs := func(id string) string { return sc.q("devices", "show", "all", "check", id) }
	var todos []k1Todo
	for _, c := range rows {
		n := c.Counts.Fail
		t := k1Todo{Who: "a person", Action: "Open", Href: devs(c.ID)}
		switch c.ID {
		case "firmware_approved":
			other, behind := k1FirmwareSplit(c.Failing)
			t.Title = fmt.Sprintf("%d device%s not on the approved firmware", n, sIfN(n))
			var bits []string
			if other > 0 {
				bits = append(bits, fmt.Sprintf("%d on another firmware line need a rollout you watch", other))
			}
			if behind > 0 {
				bits = append(bits, fmt.Sprintf("%d behind on their line: prod moves them in their window", behind))
			}
			t.Sub = strings.Join(bits, " · ")
			t.Who, t.Action, t.Href = "prod, by itself", "Releases", "/releases"
			if other > 0 {
				t.Who = "a rollout"
			}
		case "checked_in":
			hours := rules.baseSetting(c.compCheck).Param
			t.Title = fmt.Sprintf("%d device%s at a venue silent for over %d h", n, sIfN(n), hours)
			t.Sub = k1TopVenues(c.Failing, 2)
			t.Who, t.Action = "someone on site", "See devices"
		case "locked":
			t.Title = fmt.Sprintf("%d not locked as their rule says", n)
			t.Sub = k1TopVenues(c.Failing, 2)
			t.Action, t.Href = "Lock", "/lock"
		default:
			t.Title = fmt.Sprintf("%d fail %q", n, c.Name)
			if len(c.Failing) > 0 && c.Failing[0].Detail != "" {
				t.Sub = c.Failing[0].Detail
				if len(c.Failing) > 1 {
					t.Sub += fmt.Sprintf(" (and %d more)", len(c.Failing)-1)
				}
			}
			if rules.autoFix(c.compCheck) {
				t.Who = "auto-fix"
				fixed := fixesWeek[c.ID]
				t.Title = fmt.Sprintf("%s: %d still failing", c.Name, n)
				t.Sub = fmt.Sprintf("auto-fix %s this week and tries again every %d h; one someone turned on from the adb page is left until its window ends",
					map[bool]string{true: fmt.Sprintf("fixed %d", fixed), false: "fixed none"}[fixed > 0], int(compFixAgain.Hours()))
				if c.Fix == "" {
					t.Sub = ""
				}
			}
			t.Action = fmt.Sprintf("Open the %d", n)
			if n == 1 {
				t.Action = "Open"
			}
		}
		todos = append(todos, t)
	}
	if len(todos) == 0 {
		b.Headline = "Every device meets every required check"
		b.Calm = fmt.Sprintf("%d device%s held to required checks", inScope, sIfN(inScope))
		if partly > 0 {
			b.Calm += fmt.Sprintf(" · %d measured on server checks only (v2.0 images)", partly)
		}
		b.Calm += "."
		return b
	}
	b.Tone = "warn"
	b.Headline = fmt.Sprintf("%d to fix first", len(todos))
	if len(todos) == 1 {
		b.Headline = "1 to fix"
	}
	if len(todos) > k1TodoRows {
		b.More = len(todos) - k1TodoRows
		todos = todos[:k1TodoRows]
	}
	b.Todos = todos
	return b
}

// ── Cards ─────────────────────────────────────────────────────────────────────

type k1Card struct {
	Title, Href string
	A, B        ovMetric
	LineA, Fill string
}

// buildK1Cards: Compliant (today against a week ago), Auto-fix (fixes this week against
// the week before; checks with auto-fix on) and Measurable (fully measured against a
// week ago; server checks only). A delta shows only where the week-ago day had data.
func buildK1Cards(trend []k1Day, fixesDaily []int, autoFixOn, pct, failing, inScope, partly int, sc k1Scope) []k1Card {
	var today, weekAgo *k1Day
	if n := len(trend); n > 0 {
		today = &trend[n-1]
		if n > 7 && trend[n-8].InScope > 0 {
			weekAgo = &trend[n-8]
		}
	}
	series := func(f func(k1Day) float64) []float64 {
		out := make([]float64, 0, len(trend))
		for _, d := range trend {
			if d.InScope > 0 {
				out = append(out, f(d))
			}
		}
		return out
	}
	comp := k1Card{Title: "Compliant", Href: sc.q("devices", "show", "failing")}
	comp.A = ovMetric{Value: fmt.Sprintf("%d%%", pct), Label: "meet every required check", Tone: "flat"}
	comp.B = ovMetric{Value: fmt.Sprintf("%d", failing), Label: "failing a required check", Tone: "flat"}
	if weekAgo != nil && today != nil {
		comp.A.Delta, comp.A.Tone = ovDelta(float64(today.Pct()-weekAgo.Pct()), "pts", false, 0)
		if comp.A.Delta != "" {
			comp.A.Delta += " vs last week"
		}
		comp.B.Delta, comp.B.Tone = ovDelta(float64((today.InScope-today.Compliant)-(weekAgo.InScope-weekAgo.Compliant)), "", true, 0)
	}
	comp.LineA, _, comp.Fill = ovLines(series(func(d k1Day) float64 { return float64(d.Pct()) }), nil)

	fix := k1Card{Title: "Auto-fix", Href: "/compliance/rules"}
	week, prev := 0, 0
	for i, n := range fixesDaily {
		if i >= len(fixesDaily)-7 {
			week += n
		} else {
			prev += n
		}
	}
	fix.A = ovMetric{Value: fmt.Sprintf("%d", week), Label: "fixed by the system this week", Tone: "flat"}
	if len(fixesDaily) >= 14 {
		fix.A.Delta, fix.A.Tone = ovDelta(float64(week-prev), "", false, 0)
		fix.A.Tone = "flat" // more fixes is neither good nor bad news
	}
	fix.B = ovMetric{Value: fmt.Sprintf("%d", autoFixOn), Label: fmt.Sprintf("check%s with auto-fix on", sIfN(autoFixOn)), Tone: "flat"}
	fd := make([]float64, len(fixesDaily))
	for i, n := range fixesDaily {
		fd[i] = float64(n)
	}
	fix.LineA, _, fix.Fill = ovLines(fd, nil)

	meas := k1Card{Title: "Measurable", Href: sc.q("devices", "show", "unknown")}
	full := inScope - partly
	meas.A = ovMetric{Value: fmt.Sprintf("%d", full), Label: "fully measured", Tone: "flat"}
	meas.B = ovMetric{Value: fmt.Sprintf("%d", partly), Label: "server checks only (v2.0 images)", Tone: "flat"}
	if weekAgo != nil && today != nil {
		meas.A.Delta, meas.A.Tone = ovDelta(float64((today.InScope-today.Partly)-(weekAgo.InScope-weekAgo.Partly)), "", false, 0)
	}
	meas.LineA, _, meas.Fill = ovLines(series(func(d k1Day) float64 { return float64(d.InScope - d.Partly) }), nil)
	return []k1Card{comp, fix, meas}
}

// ── Check table ───────────────────────────────────────────────────────────────

type k1Row struct {
	compCheckView
	AutoFix      bool // has a fix and it is on
	HasFix       bool
	FixedWeek    int
	Action, Href string
	MoreHref     string
}

type k1Group struct {
	Name string
	Rows []k1Row
}

// k1GroupLabel is the short name of a registry group, as the demo groups them.
func k1GroupLabel(g string) string {
	switch g {
	case compGroupSetup:
		return "Setup"
	case compGroupDebug:
		return "Debugging"
	case compGroupIntact:
		return "Intact"
	}
	return g
}

// buildK1Groups keeps C1's order inside each group (failing first, then the registry's)
// and the groups in the registry's order. The Level pill keeps only that level.
func buildK1Groups(checks []compCheckView, rules compRules, fixesWeek map[string]int, sc k1Scope) []k1Group {
	var order []string
	byGroup := map[string][]k1Row{}
	for _, c := range complianceChecks {
		if _, ok := byGroup[c.Group]; !ok {
			byGroup[c.Group] = nil
			order = append(order, c.Group)
		}
	}
	for _, c := range checks {
		if sc.Level != "" && c.Level != sc.Level {
			continue
		}
		r := k1Row{compCheckView: c, HasFix: c.Fix != "", AutoFix: rules.autoFix(c.compCheck), FixedWeek: fixesWeek[c.ID],
			MoreHref: sc.q("devices", "show", "all", "check", c.ID)}
		if c.Counts.Fail > 0 {
			switch c.ID {
			case "firmware_approved":
				r.Action, r.Href = "Releases", "/releases"
			case "locked":
				r.Action, r.Href = "Lock", "/lock"
			case "client_current":
				r.Action, r.Href = "Clients", "/lineup?view=clients"
			default:
				r.Action, r.Href = "Devices", r.MoreHref
			}
		}
		byGroup[c.Group] = append(byGroup[c.Group], r)
	}
	var out []k1Group
	for _, g := range order {
		if rows := byGroup[g]; len(rows) > 0 {
			out = append(out, k1Group{Name: k1GroupLabel(g), Rows: rows})
		}
	}
	return out
}

// ── Rail ──────────────────────────────────────────────────────────────────────

type k1Bar struct {
	Label string
	Pct   int // bar height
	Title string
	Cur   bool
}

type k1Report struct {
	On        bool
	Next      string
	VenueID   string
	VenueName string
	Venues    int
}

type k1Rail struct {
	Pct       int
	Offset    string
	Tone      string
	Compliant int
	InScope   int
	Partly    int
	Bars      []k1Bar
	HasTrend  bool
	Behind    []compVenueView
	Report    k1Report
}

func buildK1Rail(pct, compliant, inScope, partly int, trend []k1Day, venues []compVenueView, reportsOn bool, now time.Time) k1Rail {
	r := k1Rail{Pct: pct, Compliant: compliant, InScope: inScope, Partly: partly,
		Offset: fmt.Sprintf("%.1f", 326.7*float64(100-pct)/100)}
	switch {
	case pct >= 90:
		r.Tone = "ok"
	case pct >= 70:
		r.Tone = "warn"
	default:
		r.Tone = "danger"
	}
	// Bars scale between the lowest and highest day, so a move from 76% to 82% shows
	// (on a 0–100 scale every bar would look the same height).
	lo, hi := 101, -1
	for _, d := range trend {
		if d.InScope > 0 {
			lo, hi = min(lo, d.Pct()), max(hi, d.Pct())
		}
	}
	for i, d := range trend {
		if d.InScope > 0 {
			r.HasTrend = true
		}
		h := 4
		if d.InScope > 0 {
			h = 60
			if hi > lo {
				h = 20 + (d.Pct()-lo)*80/(hi-lo)
			}
		}
		title := d.Day.Format("Mon 2 Jan") + ": no scored devices"
		if d.InScope > 0 {
			title = fmt.Sprintf("%s: %d%% (%d of %d)", d.Day.Format("Mon 2 Jan"), d.Pct(), d.Compliant, d.InScope)
		}
		r.Bars = append(r.Bars, k1Bar{Label: d.Day.Format("2"), Pct: h, Title: title, Cur: i == len(trend)-1})
	}
	scored := 0
	for _, v := range venues {
		if v.ID == nil || v.Pct < 0 {
			continue
		}
		scored++
		if v.Pct < 100 && len(r.Behind) < 3 {
			r.Behind = append(r.Behind, v)
		}
	}
	r.Report = k1Report{On: reportsOn, Venues: scored}
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)
	r.Report.Next = first.Format("2 Jan")
	if len(r.Behind) > 0 {
		r.Report.VenueID, r.Report.VenueName = r.Behind[0].ID.String(), r.Behind[0].Name
	} else {
		for _, v := range venues {
			if v.ID != nil {
				r.Report.VenueID, r.Report.VenueName = v.ID.String(), v.Name
				break
			}
		}
	}
	return r
}

// k1FixesDaily counts the auto-fix commands that completed, per day over the last
// `days` days (oldest first), and per check this week. Only wireless adb has a fix today
// (adb_tcp, sent by "compliance").
func k1FixesDaily(fixes []db.SystemFix, now time.Time, days int) ([]int, map[string]int) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := dayStart.AddDate(0, 0, -(days - 1))
	out := make([]int, days)
	week := map[string]int{}
	for _, f := range fixes {
		if f.By != compFixActor || f.Status != "completed" || f.At.Before(start) {
			continue
		}
		i := int(f.At.In(now.Location()).Sub(start).Hours() / 24)
		if i < 0 || i >= days {
			continue
		}
		out[i]++
		if i >= days-7 && f.Type == "adb_tcp" {
			week["wireless_adb_off"]++
		}
	}
	return out, week
}

// DevHref is the Devices tab with a verdict filter (and a check, if any), keeping the
// venue and type scope. Built here, not in the template: html/template would escape a
// query suffix pasted into an href.
func (s k1Scope) DevHref(show, check string) string {
	extra := []string{"show", show}
	if check != "" {
		extra = append(extra, "check", check)
	}
	return s.q("devices", extra...)
}
