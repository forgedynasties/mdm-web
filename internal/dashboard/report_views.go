package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// The weekly report's plain-language views. The standard report is written for us —
// uptime hours out of 168, drain in %/min, serial numbers. These are written for the
// people it goes to: owners, GMs and floor managers, who want to know whether the
// tablets worked while they were open, whether guests used them, and what to do.
// All six read the same venueStory; see static/restaurant-report-demos.html for what
// each is good and bad at.

// reportView is one entry in the page's view picker. Key "" is the standard report,
// which stays the default so every existing link, PDF and email is unchanged.
type reportView struct {
	Key, Label string
	Selected   bool
}

var reportViewList = []reportView{
	{Key: "", Label: "Standard report"},
	{Key: "scorecard", Label: "Scorecard"},
	{Key: "letter", Label: "Letter"},
	{Key: "tablets", Label: "Tablets"},
	{Key: "service", Label: "Service hours"},
	{Key: "guests", Label: "Guests"},
	{Key: "summary", Label: "Two-page summary"},
}

// pickReportView resolves ?view= against the list; anything unknown is the standard.
func pickReportView(key string) (string, []reportView) {
	out := make([]reportView, len(reportViewList))
	copy(out, reportViewList)
	pick := 0
	for i, v := range out {
		if v.Key == key {
			pick = i
		}
	}
	out[pick].Selected = true
	return out[pick].Key, out
}

// Thresholds, in one place because each is a judgement a venue may ask us to explain.
const (
	storyFlatPct       = 5  // at or below this, a tablet has run flat
	storyRedReadyPct   = 85 // ready for less of opening hours than this: it let guests down
	storyAmberReadyPct = 95 // ...less than this: worth a look
	storyRestartsAmber = 3  // restarts on its own in a week before it is worth a look
)

// storyIssue is one thing wrong with a tablet, and what the venue can do about it.
type storyIssue struct {
	Text, Fix string
	Red       bool
}

// tabletStory is one tablet as a venue sees it: by the name of where it sits.
type tabletStory struct {
	DeviceID uuid.UUID
	Name     string // nickname, else the serial
	Serial   string
	Named    bool
	Status   string // g | a | r
	Issues   []storyIssue

	ReadyPct     int
	HasReady     bool
	PoweredHours float64
	PluggedPct   int
	Sessions     int
	GuestHours   float64
	Restarts     int
}

// Reason is the tablet's one line: its worst issue, or a good word when it has none.
func (t tabletStory) Reason() string {
	if len(t.Issues) > 0 {
		return t.Issues[0].Text
	}
	if t.Sessions > 0 {
		return fmt.Sprintf("%d guest charge%s", t.Sessions, plural(t.Sessions))
	}
	return "Fine all week"
}

// Fix is what to do about the worst issue.
func (t tabletStory) Fix() string {
	if len(t.Issues) > 0 {
		return t.Issues[0].Fix
	}
	return ""
}

// Also lists the tablet's other issues after the first, for the attention list.
func (t tabletStory) Also() string {
	if len(t.Issues) < 2 {
		return ""
	}
	var parts []string
	for _, i := range t.Issues[1:] {
		parts = append(parts, lowerFirst(i.Text))
	}
	return "Also: " + strings.Join(parts, "; ") + "."
}

type storyCell struct {
	Class string // g | a | r | "" (no data or not yet)
	Pct   int    // colour strength, for the heat map
	Title string
	Flag  bool
}

type storyRow struct {
	Label string
	Cells []storyCell
}

type storyMoment struct {
	At   time.Time
	When string
	Name string
	Text string
}

type storyDelta struct {
	Text  string // "▲ 5 pts", "▼ 3"
	Class string // up (better) | dn (worse) | flat
	Prev  string // "from 92%"
}

type storySpot struct {
	Name  string
	Count int
}

// venueStory is everything the six views draw.
type venueStory struct {
	Current   bool
	OpenLabel string // "11am–11pm"
	HourLabel []string

	TabletCount int
	ReadyPct    int
	HasReady    bool
	ReadyDelta  *storyDelta

	Sessions      int
	SessionsDelta *storyDelta
	GuestHours    float64
	AvgSessionMin int
	TopSpots      []storySpot
	PeakLabel     string // "Fri 7pm"

	Flat      int
	FlatDelta *storyDelta
	FlatNames []string

	PlugInPct int
	HasPlugIn bool

	Tablets   []tabletStory
	Attention []tabletStory
	Fine      int
	Amber     int
	Red       int

	Grade, GradeWord, GradeNote string
	GradeClass                  string // g | a | r
	Headline                    string

	Timeline      []storyRow
	AllReadyHours int
	OpenHourSlots int
	Heat          []storyRow
	Moments       []storyMoment

	Letter []template.HTML
}

// storyCore is the handful of venue figures computed for both the week and the week
// before, so every "vs last week" compares like with like.
type storyCore struct {
	readyHours, openHours int
	sessions              int
	flat                  int
}

func (c storyCore) readyPct() (int, bool) {
	if c.openHours == 0 {
		return 0, false
	}
	return c.readyHours * 100 / c.openHours, true
}

// storyInput is what buildVenueStory needs; the handler fills it.
type storyInput struct {
	Win      reportWindow
	Loc      *time.Location
	OpenMin  int
	CloseMin int
	Now      time.Time
	Devices  []db.DeviceWeek
	Metrics  *db.SiteMetrics
	Ins      db.VenueInsights
	Prev     db.VenueInsights
	PrevFrom time.Time // local midnight the week before starts
}

// openHours lists the local clock hours the venue is open, in the order they happen:
// an hour counts when the window covers its middle. A window that wraps past midnight
// runs on into the small hours.
func openHours(openMin, closeMin int) []int {
	covers := func(m int) bool {
		if openMin == closeMin {
			return true // no window: the whole day
		}
		if openMin < closeMin {
			return m >= openMin && m < closeMin
		}
		return m >= openMin || m < closeMin
	}
	var out []int
	start := openMin / 60
	for i := 0; i < 24; i++ {
		h := (start + i) % 24
		if covers(h*60 + 30) {
			out = append(out, h)
		}
	}
	return out
}

func hourLabel(h int) string {
	switch {
	case h == 0:
		return "12am"
	case h < 12:
		return fmt.Sprintf("%dam", h)
	case h == 12:
		return "12pm"
	}
	return fmt.Sprintf("%dpm", h-12)
}

func clockLabel(t time.Time) string {
	h, m := t.Hour(), t.Minute()
	suffix := "am"
	if h >= 12 {
		suffix = "pm"
	}
	h12 := h % 12
	if h12 == 0 {
		h12 = 12
	}
	if m == 0 {
		return fmt.Sprintf("%s %d%s", t.Format("Mon"), h12, suffix)
	}
	return fmt.Sprintf("%s %d:%02d%s", t.Format("Mon"), h12, m, suffix)
}

type devHour struct {
	dev  uuid.UUID
	date string
	hour int
}

// coreFor measures readiness, guest sessions and flat episodes over the given days.
// A tablet is counted only on the days it reported at all, so one not yet on the floor
// does not read as a tablet that failed.
func coreFor(ins db.VenueInsights, ids []uuid.UUID, dayStart time.Time, days int, hours []int, now time.Time) (storyCore, map[uuid.UUID]storyCore, map[uuid.UUID][]time.Time) {
	present := map[devHour]int{}
	reported := map[uuid.UUID]map[string]bool{}
	for _, h := range ins.Hours {
		k := devHour{h.DeviceID, h.Hour.Format("2006-01-02"), h.Hour.Hour()}
		present[k] = h.MinBattery
		if reported[h.DeviceID] == nil {
			reported[h.DeviceID] = map[string]bool{}
		}
		reported[h.DeviceID][k.date] = true
	}
	var total storyCore
	per := map[uuid.UUID]storyCore{}
	flats := map[uuid.UUID][]time.Time{}
	for _, id := range ids {
		var c storyCore
		for d := 0; d < days; d++ {
			day := dayStart.AddDate(0, 0, d)
			date := day.Format("2006-01-02")
			if !reported[id][date] {
				continue
			}
			wasFlat := false
			for _, h := range hours {
				at := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, day.Location())
				if h < hours[0] { // a window past midnight: these hours belong to the next day
					at = at.AddDate(0, 0, 1)
				}
				if !at.Before(now) {
					continue
				}
				bat, ok := present[devHour{id, at.Format("2006-01-02"), h}]
				c.openHours++
				flat := ok && bat <= storyFlatPct
				if ok && !flat {
					c.readyHours++
				}
				if flat && !wasFlat {
					c.flat++
					flats[id] = append(flats[id], at)
				}
				wasFlat = flat
			}
		}
		per[id] = c
		total.readyHours += c.readyHours
		total.openHours += c.openHours
		total.flat += c.flat
	}
	for _, s := range ins.Sessions {
		c := per[s.DeviceID]
		c.sessions++
		per[s.DeviceID] = c
		total.sessions++
	}
	return total, per, flats
}

// betterDelta describes a change where up is good (readiness, guest charges).
func betterDelta(now, prev int, unit string) *storyDelta {
	d := now - prev
	switch {
	case d > 0:
		return &storyDelta{Text: fmt.Sprintf("▲ %d%s", d, unit), Class: "up"}
	case d < 0:
		return &storyDelta{Text: fmt.Sprintf("▼ %d%s", -d, unit), Class: "dn"}
	}
	return &storyDelta{Text: "no change", Class: "flat"}
}

// fewerDelta describes a change where down is good (tablets running flat).
func fewerDelta(now, prev int) *storyDelta {
	d := now - prev
	switch {
	case d < 0:
		return &storyDelta{Text: fmt.Sprintf("▼ %d", -d), Class: "up"}
	case d > 0:
		return &storyDelta{Text: fmt.Sprintf("▲ %d", d), Class: "dn"}
	}
	return &storyDelta{Text: "no change", Class: "flat"}
}

var gradeSteps = []string{"A", "A−", "B+", "B", "C", "D"}

// gradeFor is the scorecard's grade: readiness sets it, and a tablet that let guests
// down during service costs one step. Written out here and on the page, because a grade
// nobody can reproduce is an argument waiting to happen.
func gradeFor(ready int, red int) (grade, word, class string) {
	i := 5
	switch {
	case ready >= 97:
		i = 0
	case ready >= 95:
		i = 1
	case ready >= 93:
		i = 2
	case ready >= 90:
		i = 3
	case ready >= 80:
		i = 4
	}
	if red > 0 && i < 5 {
		i++
	}
	switch {
	case i <= 1:
		word, class = "A good week", "g"
	case i <= 3:
		word, class = "A steady week", "a"
	case i == 4:
		word, class = "A mixed week", "r"
	default:
		word, class = "A tough week", "r"
	}
	return gradeSteps[i], word, class
}

const gradeRule = "Grade from the share of opening hours the tablets were ready: A 97%+, A− 95%+, B+ 93%+, B 90%+, C 80%+, D below; one step lower if any tablet let guests down during service."

func buildVenueStory(in storyInput) venueStory {
	loc := in.Loc
	hours := openHours(in.OpenMin, in.CloseMin)
	dayStart := time.Date(in.Win.From.Year(), in.Win.From.Month(), in.Win.From.Day(), 0, 0, 0, 0, loc)
	st := venueStory{Current: in.Win.Current, TabletCount: len(in.Devices)}
	if len(hours) > 0 {
		last := hours[len(hours)-1] + 1
		st.OpenLabel = hourLabel(hours[0]) + "–" + hourLabel(last%24)
		if len(hours) == 24 {
			st.OpenLabel = "all day"
		}
	}
	for _, h := range hours {
		st.HourLabel = append(st.HourLabel, hourLabel(h))
	}

	ids := make([]uuid.UUID, 0, len(in.Devices))
	byID := map[uuid.UUID]db.DeviceWeek{}
	for _, d := range in.Devices {
		ids = append(ids, d.DeviceID)
		byID[d.DeviceID] = d
	}
	nameOf := func(id uuid.UUID) string {
		d := byID[id]
		if d.Nickname != "" {
			return d.Nickname
		}
		return d.Serial
	}

	core, per, flats := coreFor(in.Ins, ids, dayStart, in.Win.Days, hours, in.Now)
	prevCore, _, _ := coreFor(in.Prev, ids, in.PrevFrom, 7, hours, in.Now)
	st.ReadyPct, st.HasReady = core.readyPct()
	if p, ok := prevCore.readyPct(); ok && st.HasReady {
		st.ReadyDelta = betterDelta(st.ReadyPct, p, " pts")
		st.ReadyDelta.Prev = fmt.Sprintf("from %d%%", p)
	}
	st.Sessions, st.Flat = core.sessions, core.flat
	if !in.Win.Current { // counts over a part-week cannot be set against a whole one
		st.SessionsDelta = betterDelta(core.sessions, prevCore.sessions, "")
		st.SessionsDelta.Prev = fmt.Sprintf("from %d", prevCore.sessions)
		st.FlatDelta = fewerDelta(core.flat, prevCore.flat)
		st.FlatDelta.Prev = fmt.Sprintf("from %d", prevCore.flat)
	}
	var sessMin float64
	spots := map[uuid.UUID]int{}
	for _, s := range in.Ins.Sessions {
		sessMin += s.Minutes
		spots[s.DeviceID]++
	}
	st.GuestHours = sessMin / 60
	if st.Sessions > 0 {
		st.AvgSessionMin = int(sessMin/float64(st.Sessions) + 0.5)
	}
	for id, n := range spots {
		st.TopSpots = append(st.TopSpots, storySpot{Name: nameOf(id), Count: n})
	}
	sort.Slice(st.TopSpots, func(i, j int) bool {
		if st.TopSpots[i].Count != st.TopSpots[j].Count {
			return st.TopSpots[i].Count > st.TopSpots[j].Count
		}
		return st.TopSpots[i].Name < st.TopSpots[j].Name
	})
	if len(st.TopSpots) > 3 {
		st.TopSpots = st.TopSpots[:3]
	}
	if in.Metrics != nil && in.Metrics.ConnectCount > 0 {
		st.PlugInPct, st.HasPlugIn = int(in.Metrics.ConnectAvgPct+0.5), true
	}

	// Alerts by device and type.
	alerts := map[uuid.UUID]map[string][]time.Time{}
	for _, a := range in.Ins.Alerts {
		if alerts[a.DeviceID] == nil {
			alerts[a.DeviceID] = map[string][]time.Time{}
		}
		alerts[a.DeviceID][a.Type] = append(alerts[a.DeviceID][a.Type], a.At)
	}
	whenList := func(ts []time.Time) string {
		var s []string
		for i, t := range ts {
			if i == 2 {
				s = append(s, fmt.Sprintf("%d more", len(ts)-2))
				break
			}
			s = append(s, clockLabel(t))
		}
		return strings.Join(s, ", ")
	}

	// Tablets.
	for _, d := range in.Devices {
		c := per[d.DeviceID]
		t := tabletStory{
			DeviceID: d.DeviceID, Name: nameOf(d.DeviceID), Serial: d.Serial, Named: d.Nickname != "",
			PoweredHours: d.PoweredMinutes / 60, Sessions: c.sessions, Restarts: in.Ins.Restarts[d.DeviceID],
		}
		if d.FullWindowMinutes > 0 {
			t.PluggedPct = pctCapped(d.PluggedMinutes, d.FullWindowMinutes)
		}
		t.ReadyPct, t.HasReady = c.readyPct()
		for _, s := range in.Ins.Sessions {
			if s.DeviceID == d.DeviceID {
				t.GuestHours += s.Minutes / 60
			}
		}
		a := alerts[d.DeviceID]
		if off := a["offline_peak"]; len(off) > 0 {
			t.Issues = append(t.Issues, storyIssue{Red: true,
				Text: "Went offline during opening hours (" + whenList(off) + ")",
				Fix:  "Check the Wi-Fi signal where it sits; if it happens again, ask us to look."})
		}
		if t.HasReady && t.ReadyPct < storyRedReadyPct {
			t.Issues = append(t.Issues, storyIssue{Red: true,
				Text: fmt.Sprintf("Ready for only %d%% of opening hours", t.ReadyPct),
				Fix:  "Keep it switched on and charged while you are open."})
		}
		if f := flats[d.DeviceID]; len(f) > 0 {
			t.Issues = append(t.Issues, storyIssue{
				Text: "Ran flat during service (" + whenList(f) + ")",
				Fix:  "Put it back on charge between services."})
		}
		if len(a["charger_flapping"]) > 0 {
			t.Issues = append(t.Issues, storyIssue{
				Text: "Charging cable keeps cutting in and out",
				Fix:  "Try another cable or plug; we can send a replacement."})
		}
		if hot := a["overheating"]; len(hot) > 0 {
			t.Issues = append(t.Issues, storyIssue{
				Text: "Got very hot (" + whenList(hot) + ")",
				Fix:  "Keep it out of direct sun and away from heat lamps."})
		}
		if t.Restarts >= storyRestartsAmber {
			t.Issues = append(t.Issues, storyIssue{
				Text: fmt.Sprintf("Restarted by itself %d times", t.Restarts),
				Fix:  "Nothing for you to do — we are looking into it."})
		}
		if t.HasReady && t.ReadyPct >= storyRedReadyPct && t.ReadyPct < storyAmberReadyPct && len(t.Issues) == 0 {
			t.Issues = append(t.Issues, storyIssue{
				Text: fmt.Sprintf("Ready for %d%% of opening hours", t.ReadyPct),
				Fix:  "Keep it switched on and charged while you are open."})
		}
		t.Status = "g"
		for _, i := range t.Issues {
			if i.Red {
				t.Status = "r"
				break
			}
			t.Status = "a"
		}
		st.Tablets = append(st.Tablets, t)
	}
	rank := map[string]int{"r": 0, "a": 1, "g": 2}
	sort.SliceStable(st.Tablets, func(i, j int) bool {
		a, b := st.Tablets[i], st.Tablets[j]
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		return naturalLess(a.Name, b.Name)
	})
	for _, t := range st.Tablets {
		switch t.Status {
		case "g":
			st.Fine++
		default:
			if t.Status == "r" {
				st.Red++
			} else {
				st.Amber++
			}
			st.Attention = append(st.Attention, t)
		}
	}
	for id, f := range flats {
		if len(f) > 0 {
			st.FlatNames = append(st.FlatNames, nameOf(id))
		}
	}
	sort.Slice(st.FlatNames, func(i, j int) bool { return naturalLess(st.FlatNames[i], st.FlatNames[j]) })

	if st.HasReady {
		st.Grade, st.GradeWord, st.GradeClass = gradeFor(st.ReadyPct, st.Red)
		st.GradeNote = gradeRule
		st.Headline = fmt.Sprintf("%s — tablets were ready for %d%% of opening hours.", st.GradeWord, st.ReadyPct)
	} else {
		st.GradeWord = "Not enough data yet"
		st.Headline = "Not enough data yet for this week."
	}

	// Moments: what happened, in order.
	for id, byType := range alerts {
		for typ, ts := range byType {
			for _, at := range ts {
				m := storyMoment{At: at, When: clockLabel(at), Name: nameOf(id)}
				switch typ {
				case "offline_peak":
					m.Text = "went offline during opening hours."
				case "charger_flapping":
					m.Text = "charging cable started cutting in and out."
				case "overheating":
					m.Text = "got very hot."
				}
				st.Moments = append(st.Moments, m)
			}
		}
	}
	for id, f := range flats {
		for _, at := range f {
			st.Moments = append(st.Moments, storyMoment{At: at, When: clockLabel(at), Name: nameOf(id), Text: "ran flat during service."})
		}
	}
	sort.Slice(st.Moments, func(i, j int) bool { return st.Moments[i].At.Before(st.Moments[j].At) })
	if len(st.Moments) > 10 {
		st.Moments = st.Moments[:10]
	}
	flagged := map[string]bool{}
	for _, m := range st.Moments {
		flagged[m.At.Format("2006-01-02 15")] = true
	}

	// Service-hours grid and guest-charging heat map, one row per day.
	present := map[devHour]int{}
	reported := map[string]map[uuid.UUID]bool{}
	for _, h := range in.Ins.Hours {
		date := h.Hour.Format("2006-01-02")
		present[devHour{h.DeviceID, date, h.Hour.Hour()}] = h.MinBattery
		if reported[date] == nil {
			reported[date] = map[uuid.UUID]bool{}
		}
		reported[date][h.DeviceID] = true
	}
	starts := map[string]int{}
	maxStarts := 0
	peakKey := ""
	for _, s := range in.Ins.Sessions {
		k := s.Start.Format("2006-01-02 15")
		starts[k]++
		if starts[k] > maxStarts {
			maxStarts, peakKey = starts[k], k
		}
	}
	if peakKey != "" {
		if t, err := time.ParseInLocation("2006-01-02 15", peakKey, loc); err == nil {
			st.PeakLabel = clockLabel(t)
		}
	}
	for d := 0; d < in.Win.Days; d++ {
		day := dayStart.AddDate(0, 0, d)
		tl := storyRow{Label: day.Format("Mon")}
		ht := storyRow{Label: day.Format("Mon")}
		for _, h := range hours {
			at := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
			if h < hours[0] {
				at = at.AddDate(0, 0, 1)
			}
			key := at.Format("2006-01-02 15")
			hc := storyCell{}
			if n := starts[key]; n > 0 && maxStarts > 0 {
				hc.Pct = 12 + n*80/maxStarts
				hc.Title = fmt.Sprintf("%s — %d phone%s", clockLabel(at), n, plural(n))
			} else {
				hc.Title = clockLabel(at) + " — none"
			}
			ht.Cells = append(ht.Cells, hc)

			c := storyCell{Title: clockLabel(at) + " — no data"}
			expected := reported[day.Format("2006-01-02")]
			if at.Before(in.Now) && len(expected) > 0 {
				notReady := 0
				for id := range expected {
					bat, ok := present[devHour{id, at.Format("2006-01-02"), h}]
					if !ok || bat <= storyFlatPct {
						notReady++
					}
				}
				st.OpenHourSlots++
				switch notReady {
				case 0:
					c.Class, c.Title = "g", fmt.Sprintf("%s — all %d ready", clockLabel(at), len(expected))
					st.AllReadyHours++
				case 1:
					c.Class, c.Title = "a", clockLabel(at)+" — 1 tablet not ready"
				default:
					c.Class, c.Title = "r", fmt.Sprintf("%s — %d tablets not ready", clockLabel(at), notReady)
				}
				c.Flag = flagged[key]
			}
			tl.Cells = append(tl.Cells, c)
		}
		st.Timeline = append(st.Timeline, tl)
		st.Heat = append(st.Heat, ht)
	}

	st.Letter = storyLetter(st)
	return st
}

// storyLetter writes the week as a short note. Names come from nicknames people type,
// so every dynamic part is escaped; only the <b> tags are ours.
func storyLetter(st venueStory) []template.HTML {
	e := template.HTMLEscapeString
	b := func(s string) string { return "<b>" + e(s) + "</b>" }
	var ps []string
	ps = append(ps, "Hi team,")
	if st.HasReady {
		p := fmt.Sprintf("Your %d tablet%s had %s. They were on and ready for %s",
			st.TabletCount, plural(st.TabletCount), b(strings.ToLower(st.GradeWord)), b(fmt.Sprintf("%d%% of your opening hours", st.ReadyPct)))
		if st.ReadyDelta != nil && st.ReadyDelta.Class != "flat" {
			dir := "up"
			if st.ReadyDelta.Class == "dn" {
				dir = "down"
			}
			p += " — " + dir + " " + e(st.ReadyDelta.Prev) + " the week before"
		}
		p += "."
		for _, m := range st.Moments {
			if strings.HasPrefix(m.Text, "went offline") {
				p += " The biggest gap was " + b(m.When) + ", when " + e(m.Name) + " went offline."
				break
			}
		}
		ps = append(ps, p)
	} else {
		ps = append(ps, "There is not enough data yet to say how your tablets did this week.")
	}
	if st.Sessions > 0 {
		p := fmt.Sprintf("Guests charged their phones on the tablets %s (%.0f hours in all)",
			b(fmt.Sprintf("%d time%s", st.Sessions, plural(st.Sessions))), st.GuestHours)
		if st.PeakLabel != "" {
			p += ", busiest around " + e(st.PeakLabel)
		}
		p += "."
		if len(st.TopSpots) > 0 {
			p += " " + e(st.TopSpots[0].Name) + " was the most popular spot."
		}
		ps = append(ps, p)
	} else {
		ps = append(ps, "No guest phones were charged on the tablets this week.")
	}
	var p string
	if st.HasPlugIn {
		p = "On average, tablets went back on the charger at " + b(fmt.Sprintf("%d%% battery", st.PlugInPct)) + ". "
	}
	if st.Flat == 0 {
		p += "None ran flat while you were open."
	} else {
		p += fmt.Sprintf("%s ran flat during service %s — a top-up between services would help.",
			e(joinNames(st.FlatNames)), b(fmt.Sprintf("%d time%s", st.Flat, plural(st.Flat))))
	}
	ps = append(ps, p)
	var looks []string
	for _, t := range st.Attention {
		if len(looks) == 4 {
			break
		}
		looks = append(looks, b(t.Name)+" ("+e(lowerFirst(t.Reason()))+")")
	}
	if len(looks) > 0 {
		ps = append(ps, "Worth a look this week: "+strings.Join(looks, ", ")+".")
	} else if st.HasReady {
		ps = append(ps, "Nothing needs your attention this week.")
	}
	out := make([]template.HTML, len(ps))
	for i, s := range ps {
		out[i] = template.HTML(s) // every dynamic part above went through e()
	}
	return out
}

func joinNames(ns []string) string {
	switch len(ns) {
	case 0:
		return ""
	case 1:
		return ns[0]
	}
	return strings.Join(ns[:len(ns)-1], ", ") + " and " + ns[len(ns)-1]
}

// naturalLess orders "Table 2" before "Table 10".
func naturalLess(a, b string) bool {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ca, cb := a[ai], b[bi]
		if isDigit(ca) && isDigit(cb) {
			sa := ai
			for ai < len(a) && isDigit(a[ai]) {
				ai++
			}
			sb := bi
			for bi < len(b) && isDigit(b[bi]) {
				bi++
			}
			na, nb := strings.TrimLeft(a[sa:ai], "0"), strings.TrimLeft(b[sb:bi], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		la, lb := strings.ToLower(string(ca)), strings.ToLower(string(cb))
		if la != lb {
			return la < lb
		}
		ai++
		bi++
	}
	return len(a)-ai < len(b)-bi
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// venueStoryFor loads and builds the story for one venue and week. The week before is
// read the same way so every comparison is like for like.
func (h *Handler) venueStoryFor(ctx context.Context, rest *db.Restaurant, v venueReport) (venueStory, error) {
	sw, _, err := h.db.GetRestaurantServiceWindow(ctx, rest.ID)
	if err != nil {
		return venueStory{}, err
	}
	loc := time.UTC
	for _, name := range []string{rest.Timezone, sw.TZ} {
		if name == "" {
			continue
		}
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
			break
		}
	}
	now := time.Now().In(loc)
	from := time.Date(v.Win.From.Year(), v.Win.From.Month(), v.Win.From.Day(), 0, 0, 0, 0, loc)
	to := from.AddDate(0, 0, v.Win.Days)
	if to.After(now) {
		to = now
	}
	// A window past midnight runs into the next morning; read that far.
	readTo := to.Add(12 * time.Hour)
	if readTo.After(now) {
		readTo = now
	}
	ids := make([]uuid.UUID, 0, len(v.DeviceWeeks))
	for _, d := range v.DeviceWeeks {
		ids = append(ids, d.DeviceID)
	}
	ins, err := h.db.VenueInsightsFor(ctx, ids, from, to, readTo, loc)
	if err != nil {
		return venueStory{}, err
	}
	prevFrom := from.AddDate(0, 0, -7)
	prev, err := h.db.VenueInsightsFor(ctx, ids, prevFrom, from, from.Add(12*time.Hour), loc)
	if err != nil {
		return venueStory{}, err
	}
	return buildVenueStory(storyInput{
		Win: v.Win, Loc: loc, OpenMin: sw.OpenMin, CloseMin: sw.CloseMin, Now: now,
		Devices: v.DeviceWeeks, Metrics: v.Metrics, Ins: ins, Prev: prev, PrevFrom: prevFrom,
	}), nil
}
