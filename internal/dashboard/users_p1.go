package dashboard

import (
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// Users page P1 (picked 21 Oct 2026): the Overview's language on the people list. A
// to-do banner about accounts (sign-ups waiting on verification, invites not used,
// accounts nobody has used for a month, powers beyond a role, activity under a username
// with no account), People / Active this week / Access cards, the people table as
// before (AIO team / Customers, role and activity filters), and a rail: the share of
// accounts active this week, actions by people per day, and how many hold each role.

// p1QuietDays is how long without any recorded activity makes an account "quiet".
const p1QuietDays = 30

type p1Inputs struct {
	Team, Customers []userRow
	Waiting         int // signed up, email not verified (super admin only)
	Orphans         []db.OrphanActor
	Days            []db.PeopleDay // oldest first, today last
	Now             time.Time
}

type p1Role struct {
	Label string
	N     int
	Pct   int
}

type p1Rail struct {
	Pct     int
	Offset  string
	Tone    string
	Active  int
	Total   int
	Bars    []dp1Bar
	Roles   []p1Role
}

type p1View struct {
	Banner dp1Banner
	Cards  []ovStat
	Rail   p1Rail
}

// p1Activity is a row's activity bucket for the Activity filter: invited (invite not
// used), week (did something in 7 days), month (in 30), quiet (longer, or never on an
// account older than 30 days), new (never, but the account is younger than that).
func p1Activity(r userRow, now time.Time) string {
	switch {
	case r.Invited != nil:
		return "invited"
	case r.LastAt != nil && now.Sub(*r.LastAt) <= 7*24*time.Hour:
		return "week"
	case r.LastAt != nil && now.Sub(*r.LastAt) <= p1QuietDays*24*time.Hour:
		return "month"
	case r.LastAt == nil && now.Sub(r.CreatedAt) < p1QuietDays*24*time.Hour:
		return "new"
	}
	return "quiet"
}

func p1Comma(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func buildUsersP1(in p1Inputs) p1View {
	now := in.Now
	all := append(append([]userRow{}, in.Team...), in.Customers...)
	total := len(all)
	var v p1View

	active, invited, joined, withRules, extras := 0, 0, 0, 0, 0
	var quiet, extraRows []userRow
	for _, r := range all {
		switch p1Activity(r, now) {
		case "week":
			active++
		case "invited":
			invited++
		case "quiet":
			quiet = append(quiet, r)
		}
		if r.CreatedAt.Year() == now.Year() && r.CreatedAt.Month() == now.Month() {
			joined++
		}
		if r.Rules > 0 {
			withRules++
		}
		if len(r.Sensitive) > 0 {
			extras++
			extraRows = append(extraRows, r)
		}
	}

	// Banner.
	b := dp1Banner{Kicker: "People · " + dp1Plural(total, "account", "accounts")}
	var todos []dp1Todo
	if in.Waiting > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(in.Waiting, "person can't", "people can't") + " sign in yet",
			Sub: "They signed up but haven't clicked the link we emailed", Action: "Verify", Href: "#waiting-signups"})
	}
	if invited > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(invited, "invite", "invites") + " not used yet",
			Sub: "Send one again from the person's ··· menu if it got lost", Action: "See them", Href: "/users?activity=invited"})
	}
	if n := len(quiet); n > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(n, "account hasn't", "accounts haven't") + " done anything in " + strconv.Itoa(p1QuietDays) + " days",
			Sub: p1QuietSub(quiet) + " · remove the ones who've left", Action: "Review", Href: "/users?activity=quiet"})
	}
	if n := len(extraRows); n > 0 {
		names := []string{}
		for i, r := range extraRows {
			if i == 3 {
				names = append(names, fmt.Sprintf("%d more", n-3))
				break
			}
			names = append(names, r.DisplayName())
		}
		todos = append(todos, dp1Todo{Title: dp1Plural(n, "person has", "people have") + " powers beyond their role",
			Sub: joinAnd(names) + " · a rule adds remote, shell or firmware", Action: "Access control", Href: "/users/access"})
	}
	if n := len(in.Orphans); n > 0 {
		t := dp1Todo{Action: "Merge", Href: "#unlinked"}
		if n == 1 {
			o := in.Orphans[0]
			t.Title = "Activity under a username with no account"
			t.Sub = "“" + o.Username + "” · " + dp1Plural(int(o.Rows), "record", "records") + " · merge it into the person it belongs to"
		} else {
			t.Title = dp1Plural(n, "former username has", "former usernames have") + " activity but no account"
			t.Sub = "Merge each into the person it belongs to"
		}
		todos = append(todos, t)
	}
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	if n := len(todos) + b.More; n > 0 {
		b.Headline = dp1Plural(n, "thing", "things") + " to look at"
	} else {
		b.Tone, b.Headline = "calm", "Every account is in order"
	}
	if in.Waiting == 0 && invited == 0 {
		b.Line = "No invites waiting and nobody stuck at email verification."
	}
	v.Banner = b

	// Cards.
	actions7 := 0
	var people []float64
	for i, d := range in.Days {
		if i >= len(in.Days)-7 {
			actions7 += d.Actions
		}
		people = append(people, float64(d.People))
	}
	pc := ovStat{Title: "People", Href: "#up-card",
		A: ovMetric{Value: strconv.Itoa(len(in.Team)), Label: "on the AIO team", Tone: "good"},
		B: ovMetric{Value: strconv.Itoa(len(in.Customers)), Label: "customer" + p1S(len(in.Customers)), Tone: "flat"}}
	if joined > 0 {
		pc.A.Delta = fmt.Sprintf("▲ %d joined in %s", joined, now.Format("Jan"))
	}
	ac := ovStat{Title: "Active this week", Href: "/users?activity=week",
		A: ovMetric{Value: strconv.Itoa(active), Label: "of " + strconv.Itoa(total) + " did something", Tone: "flat"},
		B: ovMetric{Value: p1Comma(actions7), Label: "actions", Tone: "flat"}}
	ac.LineA, _, ac.Fill = ovLines(people, nil)
	xc := ovStat{Title: "Access", Href: "/users/access",
		A: ovMetric{Value: strconv.Itoa(withRules), Label: "with their own rules", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(extras), Label: "with extras", Tone: "good"}}
	if extras > 0 {
		xc.B.Tone, xc.B.Delta = "bad", "beyond the role"
	}
	v.Cards = []ovStat{pc, ac, xc}

	// Rail.
	rl := p1Rail{Active: active, Total: total, Pct: dp1Pct(active, total)}
	rl.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-rl.Pct)/100)
	if rl.Pct < 25 {
		rl.Tone = "warn"
	}
	maxA := 0
	for _, d := range in.Days {
		if d.Actions > maxA {
			maxA = d.Actions
		}
	}
	for i, d := range in.Days {
		h := 4
		if maxA > 0 && d.Actions > 0 {
			h = d.Actions * 100 / maxA
			if h < 8 {
				h = 8
			}
		}
		rl.Bars = append(rl.Bars, dp1Bar{Pct: h, Cur: i == len(in.Days)-1, Label: template.HTML(d.Day.Format("Mon")),
			Title: d.Day.Format("Mon 2 Jan") + ": " + dp1Plural(d.Actions, "action", "actions") + " by " + dp1Plural(d.People, "person", "people")})
	}
	counts := map[string]int{}
	for _, r := range all {
		counts[r.Role]++
	}
	maxR := 0
	for _, n := range counts {
		if n > maxR {
			maxR = n
		}
	}
	for _, d := range roleDefs {
		if n := counts[d.Key]; n > 0 {
			label := d.Label
			if d.Customer {
				label += " · customer"
			}
			rl.Roles = append(rl.Roles, p1Role{Label: label, N: n, Pct: n * 100 / maxR})
		}
	}
	v.Rail = rl
	return v
}

func p1S(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// p1QuietSub says who the quiet accounts are: "All viewers" or "9 viewers, 2 operators",
// and when they were last active.
func p1QuietSub(rows []userRow) string {
	by := map[string]int{}
	var order []string
	var first, last *time.Time
	never := 0
	for _, r := range rows {
		if by[r.Role] == 0 {
			order = append(order, r.Role)
		}
		by[r.Role]++
		if r.LastAt == nil {
			never++
			continue
		}
		if first == nil || r.LastAt.Before(*first) {
			first = r.LastAt
		}
		if last == nil || r.LastAt.After(*last) {
			last = r.LastAt
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]] > by[order[j]] })
	var who string
	if len(order) == 1 {
		who = "All " + strings.ToLower(roleLabel(order[0])) + "s"
		if len(rows) == 1 {
			who = rows[0].DisplayName() + ", " + strings.ToLower(roleLabel(order[0]))
		}
	} else {
		parts := []string{}
		for i, k := range order {
			if i == 3 {
				break
			}
			parts = append(parts, dp1Plural(by[k], strings.ToLower(roleLabel(k)), strings.ToLower(roleLabel(k))+"s"))
		}
		who = strings.Join(parts, ", ")
	}
	switch {
	case first != nil && never > 0:
		return who + ", last active " + first.Format("2 Jan") + "–" + last.Format("2 Jan") + " or never"
	case first != nil && first.Format("2 Jan") == last.Format("2 Jan"):
		return who + ", last active " + first.Format("2 Jan")
	case first != nil:
		return who + ", last active " + first.Format("2 Jan") + "–" + last.Format("2 Jan")
	}
	return who + ", never active"
}
