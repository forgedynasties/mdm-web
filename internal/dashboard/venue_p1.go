package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Venue page P1 (picked 21 Oct 2026, from the venues demos): the Overview's language for
// one venue. A to-do banner of what is wrong here only (critical alerts, devices dead for
// days, devices offline in service hours, a unit running hot now, the staff PIN still the
// built-in default, warnings), Online / Stability / Battery cards over a window, devices
// by type as the pill chart over the device table, and the Venue settings card (S1) as
// the rail, unchanged.

// vpDev is one of the venue's devices as P1 reads it.
type vpDev struct {
	ovDev
	Serial   string
	Firmware bool // our firmware client: the staff PIN applies to it
}

type vpBanner struct {
	Tone     string // bad | warn | calm
	Kicker   string
	Headline string
	Todos    []ovTodo
	More     int
	Calm     string
}

type venueP1 struct {
	Banner  vpBanner
	Stats   []ovStat
	Pills   ovPillChart
	Tabs    []vpTab // D1: All, then one per type, over the full-width device table
	Window  string
	Windows []ovOpt
}

type vpInputs struct {
	ID       uuid.UUID
	Name     string
	Devices  []vpDev
	Alerts   []db.Alert // open alerts at this venue, already scoped to the viewer
	Health   *db.GroupHealth
	Open     bool   // inside service hours now
	OpenAt   string // HH:MM the window opens
	CloseAt  string // HH:MM it closes
	StaffPIN string // "" = the built-in default
	Updates  venueUpdatesView
	Lost     venueLostView
	Trend    db.ScopedTrend
	Backend  *ovTodo // "Link to X in the AIO backend?" (L2), nil when not offered
	Window   string  // 24h | 7d | 30d
	Now      time.Time
}

const vpTodoRows = 3

// vpWindow reads ?window= for the cards; a venue defaults to a week.
func vpWindow(v string) string {
	switch v {
	case "24h", "30d":
		return v
	}
	return "7d"
}

func buildVenueP1(in vpInputs) venueP1 {
	out := venueP1{Window: in.Window}
	sc := ovScope{Venue: &in.ID, VenueName: in.Name, Window: in.Window}
	sc.Days, sc.WindowLabel, sc.VsLabel = ovWindowOf(in.Window)
	for _, w := range [][2]string{{"24h", "24 h"}, {"7d", "7 days"}, {"30d", "30 days"}} {
		href := "?window=" + w[0]
		out.Windows = append(out.Windows, ovOpt{w[1], href, w[0] == in.Window})
	}
	devs := make([]ovDev, len(in.Devices))
	for i, d := range in.Devices {
		devs[i] = d.ovDev
	}
	out.Stats = buildOvStats(in.Trend, devs, sc, in.Now)
	if len(out.Stats) > 0 {
		out.Stats[0].Title = "Online"
	}
	out.Pills = buildOvPills(devs, in.Alerts, sc)
	out.Tabs = buildVpTabs(out.Pills, devs, in.Alerts)
	out.Banner = buildVpBanner(in)
	return out
}

// vpTab is one type tab over the venue's device table (D1, picked 22 Oct 2026: "Devices by
// type" merged into the device list, full width). Class "" is All. The bar's three parts
// are shares of online + offline + alerts, so a type with alerts shows red; the line under
// it carries the exact numbers.
type vpTab struct {
	Class, Label            string
	Total                   int
	Online, Offline, Alerts int
	Unenrolled              int // AIO backend devices of this type the MDM doesn't manage (U1)
	POn, POff, PAl, PUn     int // bar widths, %
}

func vpTabBar(t *vpTab) {
	n := t.Online + t.Offline + t.Alerts + t.Unenrolled
	if n == 0 {
		return
	}
	t.PAl = t.Alerts * 100 / n
	t.POff = t.Offline * 100 / n
	t.PUn = t.Unenrolled * 100 / n
	t.POn = 100 - t.PAl - t.POff - t.PUn
}

// buildVpTabs: All (every device, typed or not) and the pill chart's types, biggest first.
func buildVpTabs(p ovPillChart, devs []ovDev, alerts []db.Alert) []vpTab {
	if len(devs) == 0 {
		return nil
	}
	alerted := map[uuid.UUID]bool{}
	for _, a := range alerts {
		if a.DeviceID != nil && (a.Severity == "critical" || a.Severity == "warning") {
			alerted[*a.DeviceID] = true
		}
	}
	all := vpTab{Label: "All", Total: len(devs)}
	for _, d := range devs {
		if d.Online {
			all.Online++
		} else {
			all.Offline++
		}
		if alerted[d.ID] {
			all.Alerts++
		}
	}
	vpTabBar(&all)
	out := []vpTab{all}
	for _, c := range p.Cols {
		t := vpTab{Class: c.Class, Label: c.Label, Total: c.Online + c.Offline, Online: c.Online, Offline: c.Offline, Alerts: c.Alerts}
		vpTabBar(&t)
		out = append(out, t)
	}
	return out
}

func vpDays(d time.Duration) string {
	if n := int(d.Hours() / 24); n >= 1 {
		return fmt.Sprintf("%d d", n)
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}

// vpSerials lists up to three serials with how long each has been quiet.
func vpSerials(ds []vpDev, now time.Time, withAge bool) string {
	var parts []string
	for i, d := range ds {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(ds)-3))
			break
		}
		if withAge {
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Serial, vpDays(now.Sub(d.SeenAt))))
		} else {
			parts = append(parts, d.Serial)
		}
	}
	return strings.Join(parts, ", ")
}

func buildVpBanner(in vpInputs) vpBanner {
	b := vpBanner{Tone: "calm"}
	var kick []string
	if h := in.Health; h != nil && h.DeviceCount > 0 {
		word := map[string]string{"ok": "Healthy", "warn": "Watch", "danger": "At risk"}[h.ScoreClass]
		kick = append(kick, fmt.Sprintf("Venue health · %d · %s", h.Score, word))
	} else {
		kick = append(kick, "Venue")
	}
	if in.CloseAt != "" {
		if in.Open {
			kick = append(kick, "open now, closes "+in.CloseAt)
		} else {
			kick = append(kick, "closed now, opens "+in.OpenAt)
		}
	}
	b.Kicker = strings.Join(kick, " · ")

	total, online := len(in.Devices), 0
	var dead, quiet, hot []vpDev
	firmware := false
	for _, d := range in.Devices {
		if d.Firmware {
			firmware = true
		}
		switch {
		case d.Online:
			online++
			if ovHotNow(d.ovDev) {
				hot = append(hot, d)
			}
		case in.Now.Sub(d.SeenAt) >= 24*time.Hour:
			dead = append(dead, d)
		default:
			quiet = append(quiet, d)
		}
	}

	att, _, _ := buildAttention(in.Alerts, 0)
	alertTodo := func(a attentionRow) ovTodo {
		var bits []string
		if a.Serial != "" {
			bits = append(bits, a.Serial)
		} else if a.Devices > 0 {
			bits = append(bits, fmt.Sprintf("%d device%s", a.Devices, sIfN(a.Devices)))
		}
		if !a.Since.IsZero() {
			bits = append(bits, "since "+agoShort(a.Since))
		}
		return ovTodo{Title: a.Issue, Sub: strings.Join(bits, " · "), Action: "Open", Href: a.Href}
	}
	// An "offline" alert on a device the banner already lists as offline says it twice.
	listed := map[string]bool{}
	for _, d := range dead {
		listed[d.Serial] = true
	}
	if in.Open { // quiet devices are only listed in service hours
		for _, d := range quiet {
			listed[d.Serial] = true
		}
	}
	var crit, warn []ovTodo
	for _, a := range att {
		if a.Serial != "" && listed[a.Serial] && strings.Contains(strings.ToLower(a.Issue), "offline") {
			continue
		}
		switch a.Severity {
		case "critical":
			crit = append(crit, alertTodo(a))
		case "warning":
			warn = append(warn, alertTodo(a))
		}
	}

	var mine []ovTodo
	if len(dead) > 0 {
		who := fmt.Sprintf("%d devices", len(dead))
		switch {
		case len(dead) == 1:
			who = "1 device"
		case len(dead) == total && total == 2:
			who = "Both devices"
		case len(dead) == total:
			who = fmt.Sprintf("All %d devices", total)
		}
		t := ovTodo{Title: who + " offline for over a day: " + vpSerials(dead, in.Now, true),
			Action: "See devices", Href: "#restaurant-members"}
		low := true
		var pcts []string
		for _, d := range dead {
			if !d.HasBatt || d.Battery >= 15 {
				low = false
				break
			}
			if len(pcts) < 3 {
				pcts = append(pcts, fmt.Sprintf("%d%%", d.Battery))
			}
		}
		if low {
			t.Sub = "last seen at " + strings.Join(pcts, " and ") + " battery: likely off their chargers"
		} else {
			t.Sub = "not heard from since " + dead[0].SeenAt.Format("Mon 2 Jan")
		}
		mine = append(mine, t)
	}
	if len(quiet) > 0 && in.Open {
		mine = append(mine, ovTodo{
			Title:  fmt.Sprintf("%d offline during service hours", len(quiet)),
			Sub:    vpSerials(quiet, in.Now, false) + " · open until " + in.CloseAt,
			Action: "See devices", Href: "#restaurant-members"})
	}
	if len(hot) > 0 {
		t := ovTodo{Title: fmt.Sprintf("%d running hot", len(hot)), Action: "See devices", Href: "#restaurant-members"}
		if len(hot) == 1 {
			t.Title = fmt.Sprintf("%s running hot (%.0f °C)", hot[0].Serial, *hot[0].TempC)
			t.Sub = "online now"
			t.Action, t.Href = "Open device", "/devices/"+hot[0].Serial
		} else {
			t.Sub = vpSerials(hot, in.Now, false)
		}
		mine = append(mine, t)
	}
	if in.Backend != nil {
		mine = append(mine, *in.Backend)
	}
	if firmware && (in.StaffPIN == "" || in.StaffPIN == db.DefaultStaffPIN) {
		mine = append(mine, ovTodo{Title: "Staff PIN is still the default " + db.DefaultStaffPIN,
			Sub:    "anyone who knows it can take a tablet out of lock",
			Action: "Set PIN", Href: "?open=lock#vn-lock"})
	}

	if t, ok := venuePauseTodo(in.Updates); ok {
		mine = append(mine, t)
	}
	if t, ok := venueLostPauseTodo(in.Lost); ok {
		mine = append(mine, t)
	}

	all := append(append(crit, mine...), warn...)
	switch {
	case len(crit) > 0:
		b.Tone = "bad"
	case len(all) > 0:
		b.Tone = "warn"
	}
	if len(all) == 0 {
		if total == 0 {
			b.Headline = "No devices at " + in.Name + " yet"
			b.Calm = "Add devices from the top bar; each device lives at one venue."
			return b
		}
		b.Headline = "Everything is running at " + in.Name
		b.Calm = fmt.Sprintf("%d of %d device%s online.", online, total, sIfN(total))
		if in.CloseAt != "" && in.Open {
			b.Calm += " Open until " + in.CloseAt + "."
		}
		return b
	}
	thing := "things"
	if len(all) == 1 {
		thing = "thing"
	}
	b.Headline = fmt.Sprintf("%d %s to look at at %s", len(all), thing, in.Name)
	if len(all) > vpTodoRows {
		b.More = len(all) - vpTodoRows
		all = all[:vpTodoRows]
	}
	b.Todos = all
	return b
}
