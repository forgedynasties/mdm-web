package dashboard

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── The venue's device-security report (compliance P4, R1, picked 17 Oct) ──────
//
// What a restaurant owner reads about security, in three plain statements rather than
// our seventeen checks: locked to its app, debug access closed, software up to date. It
// goes on its own page of the weekly venue PDF and is the whole of the monthly report.
// Both reach a customer only while Settings › customer compliance reports is on, which
// ships off: the wording below is a draft and needs a non-engineering sign-off first.
//
// It is rebuilt from the compliance engine's own record — each device's current result
// and its state changes — never from a separate log, so the report and /compliance
// cannot disagree. Unknown and n/a never count either way; a device that a statement's
// checks never measured is left out of that statement and the fine print says so.

// compStatement is one line of the report and the checks it folds together.
type compStatement struct {
	Key    string
	Checks []string
	// Ok is the line when every measured device stayed clean, Partial when some did not.
	Ok, Partial string
	// Lapse is how an exception reads after the device's name: "Table 9 " + Lapse.
	Lapse string
	// FixType is the system command that fixes this statement's failures (compliance
	// auto-fix or client auto-update), "" when nothing does it automatically.
	FixType, FixBy, Fixed string
}

// compStatements is the report, in order. Draft wording (17 Oct) — see the note above.
var compStatements = []compStatement{
	{Key: "locked", Checks: []string{"locked"},
		Ok: "Every device stayed locked to its app", Partial: "Devices stayed locked to their app",
		Lapse: "was not locked to its app"},
	{Key: "closed", Checks: []string{"adb_off", "wireless_adb_off", "dev_options_off"},
		Ok: "No debugging or remote access was left open", Partial: "Debugging and remote access stayed closed",
		Lapse: "had debugging or remote access open", FixType: "adb_tcp", FixBy: "compliance", Fixed: "Closed automatically."},
	{Key: "current", Checks: []string{"firmware_approved", "client_current", "security_patch_recent"},
		Ok: "Software stayed up to date", Partial: "Software stayed up to date",
		Lapse: "ran older software", FixType: "app_update", FixBy: "auto-update", Fixed: "Updated automatically."},
}

func compReportChecks() []string {
	var out []string
	for _, s := range compStatements {
		out = append(out, s.Checks...)
	}
	return out
}

// compLine is one statement's result over the period.
type compLine struct {
	Statement compStatement
	Measured  int // devices measured at some point in the period
	Clean     int // of those, devices with no failure in the period
	// Days the statement held for every measured device, of the days anything was measured
	// (venue days, clipped to now).
	DaysClean, DaysMeasured int
}

// Text is the line as printed.
func (l compLine) Text() string {
	if l.Measured > 0 && l.Clean == l.Measured {
		return l.Statement.Ok
	}
	return l.Statement.Partial
}

// AllClean is whether the line gets a tick.
func (l compLine) AllClean() bool { return l.Measured > 0 && l.Clean == l.Measured }

// compException is one failure on one device, its checks merged for the statement.
type compException struct {
	DeviceID  uuid.UUID
	Name      string
	Statement compStatement
	Start     time.Time
	End       *time.Time // nil while it is still open
	AutoFixed bool
	Carried   bool // it began before the period (Start is the period's start)
}

// compWeek is one week of the monthly bars: the share of its measured days on which every
// statement held for every device.
type compWeek struct {
	From         time.Time
	Days, Clean  int
	Pct          int
	AllDaysClean bool
}

// venueSecurity is the report for one venue and period.
type venueSecurity struct {
	From, To   time.Time // the period, To clipped to now
	Loc        *time.Location
	Devices    int
	Lines      []compLine
	Exceptions []compException // oldest first
	Weeks      []compWeek
	// Unmeasured is how many devices had at least one statement with nothing measured.
	Unmeasured int
}

// compSeg is one stretch of one check's state on one device.
type compSeg struct {
	start, end time.Time // end is the zero time while it is still the current state
	state      string
	level      string
}

func (s compSeg) endOr(t time.Time) time.Time {
	if s.end.IsZero() {
		return t
	}
	return s.end
}

// overlaps: a stretch that touches [from, to). A failure recorded and cleared within the
// same instant (an event with no successor and no current result) still counts when it
// falls inside.
func (s compSeg) overlaps(from, to, now time.Time) bool {
	end := s.endOr(now)
	if end.Equal(s.start) {
		return !s.start.Before(from) && s.start.Before(to)
	}
	return s.start.Before(to) && end.After(from)
}

// compSegments replays one device's check from its events and current result.
//
//   - The state at the start of the record is the first event's from-state, or, with no
//     events at all, the current result since its `since`.
//   - After the last event the state holds while a current result exists. With no current
//     result the check was switched off or the device left the venue at a time nobody
//     recorded, so the stretch ends at that last event.
func compSegments(events []db.ComplianceEvent, cur *db.ComplianceResult) []compSeg {
	if len(events) == 0 {
		if cur == nil {
			return nil
		}
		return []compSeg{{start: cur.Since, state: cur.State, level: cur.Level}}
	}
	var segs []compSeg
	if first := events[0]; first.From != "" {
		// A state from before the record (its event was pruned): it held up to the first
		// change we have, from a time we no longer know.
		segs = append(segs, compSeg{end: first.At, state: first.From, level: first.Level})
	}
	for i, e := range events {
		seg := compSeg{start: e.At, state: e.To, level: e.Level}
		if i+1 < len(events) {
			seg.end = events[i+1].At
		} else if cur == nil {
			seg.end = e.At
		}
		segs = append(segs, seg)
	}
	return segs
}

// buildVenueSecurity is the report for [from, to) at now, in the venue's time zone.
// devices are the venue's devices with their display names; results and events are
// what ComplianceHistory returned; fixes are the system commands sent since from.
func buildVenueSecurity(devices []db.ComplianceVenueDevice, names map[uuid.UUID]string, results []db.ComplianceResult,
	events []db.ComplianceEvent, fixes []db.SystemFix, from, to, now time.Time, loc *time.Location) venueSecurity {
	if loc == nil {
		loc = time.UTC
	}
	if to.After(now) {
		to = now
	}
	v := venueSecurity{From: from, To: to, Loc: loc, Devices: len(devices)}

	type key struct {
		dev   uuid.UUID
		check string
	}
	cur := map[key]*db.ComplianceResult{}
	for i := range results {
		cur[key{results[i].DeviceID, results[i].CheckID}] = &results[i]
	}
	evs := map[key][]db.ComplianceEvent{}
	for _, e := range events {
		k := key{e.DeviceID, e.CheckID}
		evs[k] = append(evs[k], e)
	}
	for k := range evs {
		sort.SliceStable(evs[k], func(i, j int) bool { return evs[k][i].At.Before(evs[k][j].At) })
	}
	days := compDays(from, to, loc)

	unmeasured := map[uuid.UUID]bool{}
	dayBad := make([]bool, len(days)) // any statement failed that day
	dayAny := make([]bool, len(days))
	for _, st := range compStatements {
		line := compLine{Statement: st}
		stBad := make([]bool, len(days))
		stAny := make([]bool, len(days))
		for _, d := range devices {
			measured, clean := false, true
			var fails []compSeg
			for _, c := range st.Checks {
				k := key{d.ID, c}
				for _, s := range compSegments(evs[k], cur[k]) {
					if s.level == db.ComplianceOff || (s.state != db.CompliancePass && s.state != db.ComplianceFail) {
						continue
					}
					if !s.overlaps(from, to, now) {
						continue
					}
					measured = true
					for i, dd := range days {
						if s.overlaps(dd[0], dd[1], now) {
							stAny[i] = true
							if s.state == db.ComplianceFail {
								stBad[i] = true
							}
						}
					}
					if s.state == db.ComplianceFail {
						clean = false
						fails = append(fails, s)
					}
				}
			}
			if !measured {
				unmeasured[d.ID] = true
				continue
			}
			line.Measured++
			if clean {
				line.Clean++
			}
			for _, f := range mergeFails(fails) {
				ex := compException{DeviceID: d.ID, Name: names[d.ID], Statement: st, Start: f.start}
				if ex.Start.IsZero() || ex.Start.Before(from) {
					// It began before the period; the period is what the report speaks for.
					ex.Start, ex.Carried = from, true
				}
				if ex.Name == "" {
					ex.Name = d.Serial
				}
				if !f.end.IsZero() {
					end := f.end
					ex.End = &end
					ex.AutoFixed = autoFixedDuring(fixes, d.ID, st, f.start, end)
				}
				v.Exceptions = append(v.Exceptions, ex)
			}
		}
		for i := range days {
			if stAny[i] {
				line.DaysMeasured++
				dayAny[i] = true
				if !stBad[i] {
					line.DaysClean++
				} else {
					dayBad[i] = true
				}
			}
		}
		v.Lines = append(v.Lines, line)
	}
	v.Unmeasured = len(unmeasured)
	sort.SliceStable(v.Exceptions, func(i, j int) bool { return v.Exceptions[i].Start.Before(v.Exceptions[j].Start) })

	// Weeks: Monday-based, the month's first and last ones partial.
	for i, dd := range days {
		wd := (int(dd[0].Weekday()) + 6) % 7
		if i == 0 || wd == 0 {
			v.Weeks = append(v.Weeks, compWeek{From: dd[0]})
		}
		w := &v.Weeks[len(v.Weeks)-1]
		if dayAny[i] {
			w.Days++
			if !dayBad[i] {
				w.Clean++
			}
		}
	}
	for i := range v.Weeks {
		w := &v.Weeks[i]
		if w.Days > 0 {
			w.Pct = w.Clean * 100 / w.Days
			w.AllDaysClean = w.Clean == w.Days
		}
	}
	return v
}

// compDays splits [from, to) into the venue's calendar days, each [start, end).
func compDays(from, to time.Time, loc *time.Location) [][2]time.Time {
	var out [][2]time.Time
	f := from.In(loc)
	day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	for day.Before(to) {
		next := day.AddDate(0, 0, 1)
		start := day
		if start.Before(from) {
			start = from
		}
		end := next
		if end.After(to) {
			end = to
		}
		out = append(out, [2]time.Time{start, end})
		day = next
	}
	return out
}

// mergeFails folds one device's failing stretches across a statement's checks into
// continuous lapses. An open stretch (zero end) stays open.
func mergeFails(segs []compSeg) []compSeg {
	if len(segs) == 0 {
		return nil
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].start.Before(segs[j].start) })
	var out []compSeg
	for _, s := range segs {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.end.IsZero() || !s.start.After(last.end) {
				if last.end.IsZero() || s.end.IsZero() {
					last.end = time.Time{}
				} else if s.end.After(last.end) {
					last.end = s.end
				}
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// autoFixedDuring: the statement's own system fix completed for the device while the
// lapse was open (or within a minute of it closing, as the engine scores once a minute).
func autoFixedDuring(fixes []db.SystemFix, dev uuid.UUID, st compStatement, start, end time.Time) bool {
	if st.FixType == "" {
		return false
	}
	for _, f := range fixes {
		if f.DeviceID != dev || f.Type != st.FixType || f.By != st.FixBy {
			continue
		}
		if f.Status != "completed" && f.Status != "installed" {
			continue
		}
		if !f.At.Before(start) && !f.At.After(end.Add(time.Minute)) {
			return true
		}
	}
	return false
}

// loadVenueSecurity reads and builds the report for a venue. visible narrows the devices
// (nil keeps them all, the tokenised link's view).
func (h *Handler) loadVenueSecurity(ctx context.Context, rest *db.Restaurant, from, to time.Time, visible func(uuid.UUID) bool) (venueSecurity, error) {
	loc := venueLoc(rest)
	all, err := h.db.VenueComplianceDevices(ctx, rest.ID)
	if err != nil {
		return venueSecurity{}, err
	}
	var devs []db.ComplianceVenueDevice
	var ids []uuid.UUID
	for _, d := range all {
		if visible == nil || visible(d.ID) {
			devs = append(devs, d)
			ids = append(ids, d.ID)
		}
	}
	names, _ := h.db.GetNicknames(ctx, ids)
	results, events, err := h.db.ComplianceHistory(ctx, ids, compReportChecks(), from)
	if err != nil {
		return venueSecurity{}, err
	}
	fixes, err := h.db.SystemFixes(ctx, ids, from)
	if err != nil {
		return venueSecurity{}, err
	}
	return buildVenueSecurity(devs, names, results, events, fixes, from, to, time.Now(), loc), nil
}

// venueLoc is the venue's time zone, UTC when it has none or an unknown one.
func venueLoc(rest *db.Restaurant) *time.Location {
	if rest != nil && rest.Timezone != "" {
		if l, err := time.LoadLocation(rest.Timezone); err == nil {
			return l
		}
	}
	return time.UTC
}

// Sentence is the exception as the monthly report lists it, in the venue's time zone:
// "9 Oct: Table 9 ran older software for 2 days. Updated automatically."
func (e compException) Sentence(loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	when := e.Start.In(loc).Format("2 Jan")
	if e.Carried {
		when = "Before " + when
	}
	s := when + ": " + e.Name + " " + e.Statement.Lapse
	if e.End == nil {
		return s + ". Not resolved yet."
	}
	s += " for " + compDuration(e.End.Sub(e.Start)) + "."
	if e.AutoFixed && e.Statement.Fixed != "" {
		return s + " " + e.Statement.Fixed
	}
	return s + " Back to normal on " + e.End.In(loc).Format("2 Jan") + "."
}

// compDuration is a lapse's length in the largest unit that reads naturally.
func compDuration(d time.Duration) string {
	n, unit := 0, ""
	switch {
	case d < time.Hour:
		n, unit = max(1, int(d.Minutes())), "minute"
	case d < 48*time.Hour:
		n, unit = int(d.Hours()), "hour"
	default:
		n, unit = int(d.Hours()/24), "day"
	}
	return strconv.Itoa(n) + " " + unit + plural(n)
}
