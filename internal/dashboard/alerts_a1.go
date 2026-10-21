package dashboard

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// Alerts A1 (picked 21 Oct 2026, from the Alerts demos): the page in the Overview's
// language. A to-do banner names what to handle first (critical before warnings, open
// restaurants before closed ones, oldest first), three count cards (Critical, Warnings,
// Crashes), the problems as one table with severity, owner and state chips, and a rail
// with the share acknowledged, crashes per day, what is snoozed and the release issues.
// Everything the page could do stays: tabs, search, device filter, Acknowledge (with a
// note), Assign, Snooze, Resolve with a reason, Acknowledge all / Resolve all, the crash
// feed and Release issues.

type alTodo struct {
	Title, Sub   string
	Action, Href string
}

type alBanner struct {
	Tone     string // bad | warn | calm
	Headline string
	Todos    []alTodo
	More     int
	Calm     string
}

const alTodoRows = 3

// alHandleOrder ranks problems that nobody has acknowledged: critical first, then those
// raised while the restaurant was open, then the oldest.
func alHandleOrder(ps []problemView) []problemView {
	var out []problemView
	for _, p := range ps {
		if !p.Acked && !p.Snoozed {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Severity == "critical") != (b.Severity == "critical") {
			return a.Severity == "critical"
		}
		if a.AfterHours != b.AfterHours {
			return !a.AfterHours
		}
		return a.Since.Before(b.Since)
	})
	return out
}

func alOwner(p problemView) string {
	switch p.Assignee {
	case "":
		return "nobody assigned"
	case "restaurant":
		return "assigned to the restaurant"
	}
	return "assigned to " + p.Assignee
}

func buildAlertBanner(needs, watching []problemView, now time.Time) alBanner {
	all := append(append([]problemView{}, needs...), watching...)
	todo := alHandleOrder(all)
	b := alBanner{Tone: "calm"}
	if len(todo) == 0 {
		b.Headline = "Nothing waiting on you"
		switch n := len(all); {
		case n == 0:
			b.Calm = "No open problems. Alerts are raised automatically from device telemetry."
		case n == 1:
			b.Calm = "The one open problem is acknowledged and being worked on."
		default:
			b.Calm = fmt.Sprintf("All %d open problems are acknowledged and being worked on.", n)
		}
		return b
	}
	b.Tone = "warn"
	for _, p := range todo {
		if p.Severity == "critical" {
			b.Tone = "bad"
			break
		}
	}
	b.Headline = fmt.Sprintf("%d to handle", len(todo))
	if len(todo) <= alTodoRows {
		b.Headline += " first"
	}
	for i, p := range todo {
		if i == alTodoRows {
			break
		}
		t := alTodo{Title: p.Title, Action: "Open", Href: "#" + alAnchor(p.Key)}
		if p.Restaurant != "" && !strings.Contains(p.Title, p.Restaurant) {
			t.Title += " at " + p.Restaurant
		}
		var bits []string
		switch {
		case p.Serial != "":
			bits = append(bits, p.Serial)
		case p.Devices > 1:
			bits = append(bits, fmt.Sprintf("%d devices", p.Devices))
		}
		if !p.Since.IsZero() {
			bits = append(bits, "since "+alAgo(p.Since, now))
		}
		if p.AfterHours {
			bits = append(bits, "raised after hours")
		}
		bits = append(bits, alOwner(p))
		t.Sub = strings.Join(bits, " · ")
		b.Todos = append(b.Todos, t)
	}
	b.More = len(todo) - len(b.Todos)
	return b
}

type alCard struct {
	Title, Href string
	A, B        ovMetric
}

// buildAlertCards: Critical and Warnings count open problems now, with the alerts raised
// in the last 24 h against the 24 before (nil when the viewer can't see fleet counts);
// Crashes is the last 24 h with today against yesterday (nil when they can't).
func buildAlertCards(needs, watching []problemView, raised *db.AlertsRaised, crash *db.FleetCrashStats, now time.Time) []alCard {
	oldest := func(ps []problemView) string {
		var t time.Time
		for _, p := range ps {
			if !p.Since.IsZero() && (t.IsZero() || p.Since.Before(t)) {
				t = p.Since
			}
		}
		if t.IsZero() {
			return "—"
		}
		return alAgo(t, now)
	}
	acked := 0
	for _, p := range watching {
		if p.Acked {
			acked++
		}
	}
	crit := alCard{Title: "Critical", Href: "/alerts?view=needs",
		A: ovMetric{Value: fmt.Sprintf("%d", len(needs)), Label: "open now", Tone: "flat"},
		B: ovMetric{Value: oldest(needs), Label: "oldest", Tone: "flat"}}
	warn := alCard{Title: "Warnings", Href: "/alerts?view=watching",
		A: ovMetric{Value: fmt.Sprintf("%d", len(watching)), Label: "problems", Tone: "flat"},
		B: ovMetric{Value: fmt.Sprintf("%d", acked), Label: "acknowledged", Tone: "flat"}}
	if raised != nil {
		crit.B = ovMetric{Value: fmt.Sprintf("%d", raised.Critical24), Label: "raised in 24 h", Tone: "flat"}
		crit.B.Delta, crit.B.Tone = ovDelta(float64(raised.Critical24-raised.CriticalPrev), "", true, 0)
		warn.B = ovMetric{Value: fmt.Sprintf("%d", raised.Warning24), Label: "raised in 24 h", Tone: "flat"}
		warn.B.Delta, warn.B.Tone = ovDelta(float64(raised.Warning24-raised.WarningPrev), "", true, 0)
	}
	cards := []alCard{crit, warn}
	if crash != nil {
		c := alCard{Title: "Crashes", Href: "/alerts?view=crashes",
			A: ovMetric{Value: fmt.Sprintf("%d", crash.Total24h), Label: "last 24 h", Tone: "flat"},
			B: ovMetric{Value: fmt.Sprintf("%d", crash.Devices24h), Label: "devices", Tone: "flat"}}
		if n := len(crash.Daily); n >= 2 {
			c.A.Delta, c.A.Tone = ovDelta(float64(crash.Daily[n-1]-crash.Daily[n-2]), "", true, 0)
		}
		cards = append(cards, c)
	}
	return cards
}

type alRail struct {
	Pct           int
	Offset        string
	Tone          string
	Acked, Total  int
	Owned         int
	OldestUnowned string
	Bars          []luMoveBar
	CrashWeek     int
}

func buildAlertRail(needs, watching []problemView, crash *db.FleetCrashStats, now time.Time) alRail {
	var r alRail
	var unowned time.Time
	for _, p := range append(append([]problemView{}, needs...), watching...) {
		r.Total++
		if p.Acked {
			r.Acked++
		}
		if p.Assignee != "" {
			r.Owned++
		} else if !p.Since.IsZero() && (unowned.IsZero() || p.Since.Before(unowned)) {
			unowned = p.Since
		}
	}
	if r.Total > 0 {
		r.Pct = int(math.Round(float64(r.Acked) * 100 / float64(r.Total)))
	} else {
		r.Pct = 100
	}
	r.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-r.Pct)/100)
	switch {
	case r.Pct >= 80:
		r.Tone = "ok"
	case r.Pct >= 40:
		r.Tone = "warn"
	default:
		r.Tone = "danger"
	}
	if !unowned.IsZero() {
		r.OldestUnowned = alAgo(unowned, now)
	}
	if crash != nil && len(crash.Daily) > 0 {
		peak := 0
		for _, v := range crash.Daily {
			if v > peak {
				peak = v
			}
		}
		n := len(crash.Daily)
		for i, v := range crash.Daily {
			day := now.AddDate(0, 0, i-(n-1))
			pct := 4
			if peak > 0 && v > 0 {
				if pct = v * 100 / peak; pct < 8 {
					pct = 8
				}
			}
			r.CrashWeek += v
			r.Bars = append(r.Bars, luMoveBar{Label: day.Format("Mon")[:1], Pct: pct, Val: v, Cur: i == n-1,
				Detail: fmt.Sprintf("%s: %d crash%s", day.Format("Mon 2"), v, map[bool]string{true: "", false: "es"}[v == 1])})
		}
	}
	return r
}

// alAgo is how long ago t was, short ("18m", "3h", "2d"), against now.
func alAgo(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// alAnchor is a row's HTML id: problem keys can hold a restaurant name.
func alAnchor(key string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("p-%08x", h.Sum32())
}

// alAnchors sets each problem's Anchor.
func alAnchors(ps []problemView) {
	for i := range ps {
		ps[i].Anchor = alAnchor(ps[i].Key)
	}
}

// alVenues is the venues the open problems are at, for the Venue filter.
func alVenues(ps ...[]problemView) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range ps {
		for _, p := range list {
			if p.Restaurant != "" && !seen[p.Restaurant] {
				seen[p.Restaurant] = true
				out = append(out, p.Restaurant)
			}
		}
	}
	sort.Strings(out)
	return out
}
