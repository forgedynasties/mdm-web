package dashboard

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Overview B1 (picked 5 Nov 2026; user: "i dont like the overview page so cluttered", then
// a bento reference): white cards on a soft page. Fleet health as a radar of seven parts
// with a percent pill each; "Today" as one big card showing one thing at a time (Needs
// you · Venues · Rollouts · Crashes, with Next); devices online and crashes as two figures
// beside it; then the waiting-to-place card, devices by type as dials, and online /
// offline at a venue / offline in Lab as one split bar. The banner, trend cards, venue
// strip, pill chart, rollout timeline and rail are gone (their pages keep them).

type ovPart struct {
	N         int
	Name      string
	Pct       int
	Tone      string // g | y | o | r
	Href, Why string
}

type ovRadarLabel struct {
	X, Y string
	N    int
}

type ovRadar struct {
	Rings  []string    // the four grid polygons
	Axes   [][2]string // each axis's outer end
	Area   string
	Dots   [][2]string
	Labels []ovRadarLabel
}

// ovHero is one item of a Today tab: a row in the list (Title, Sub) and its detail
// (T2, picked 5 Nov 2026: list on the left, the picked item explained on the right).
type ovHero struct {
	Tag, Tone    string // Critical / Warning / Info / Running …; r | o | g | y
	List, ListS  string // the list row (defaults to Title / Sub)
	Title, Sub   string
	Action, Href string
	Alt, AltHref string
	Facts        []ovFact
	Devs         []ovHeroDev
	MoreDevs     int
	DevsHead     string // the device table's heading
	Bars         []ovTypeBar
	Split        []ovSplitPart
}

type ovFact struct{ V, L string }

type ovHeroDev struct {
	Serial, Type, Seen, Extra, Href string
	Online                          bool
}

type ovTypeBar struct {
	Label         string
	Online, Total int
	Pct           int
	Tone          string
}

type ovSplitPart struct {
	N           int
	Tone, Label string
}

type ovHeroTab struct {
	Key, Label string
	Items      []ovHero
	Empty      string
}

type ovTypeRing struct {
	Label         string
	Online, Total int
	Dash, Circ    string
	Tone, Href    string
}

type ovBento struct {
	Score               int
	ScoreClass          string
	Parts               []ovPart
	Radar               ovRadar
	Tabs                []ovHeroTab
	Online, Total       int
	OnlinePct           int
	OnlineTone          string
	Crashes, CrashesPrv int
	CrashTag, CrashTone string
	Types               []ovTypeRing
	SplitOn             int
	SplitOffVenue       int
	SplitOffLab         int
	ToPlace             int
}

type ovBentoIn struct {
	Devs                []ovDev
	Active              []db.Alert
	CompPct             int // -1: not measured
	Facts               db.OverviewFacts
	ClientUp, ClientBhd int
	ClassOn             []db.ClassOnline
	Todos               []ovTodo
	Venues              []venueRow
	Rollouts            []db.Rollout
	AppLibrary          bool
	CrashTop            []db.DeviceCrash
	Crashes, CrashesPrv int
	Score               int
	ScoreClass          string
	Now                 time.Time
}

func ovTone(pct int) string {
	switch {
	case pct >= 75:
		return "g"
	case pct >= 55:
		return "y"
	case pct >= 40:
		return "o"
	}
	return "r"
}

func pctOf(n, of int) int {
	if of <= 0 {
		return 0
	}
	return int(math.Round(float64(n) * 100 / float64(of)))
}

func buildOvBento(in ovBentoIn) ovBento {
	b := ovBento{Score: in.Score, ScoreClass: in.ScoreClass, Crashes: in.Crashes, CrashesPrv: in.CrashesPrv, ToPlace: in.Facts.ToPlace}

	withAlert := map[uuid.UUID]bool{}
	for _, a := range in.Active {
		if a.DeviceID != nil {
			withAlert[*a.DeviceID] = true
		}
	}
	var today, batt, battOK, clean int
	for _, d := range in.Devs {
		b.Total++
		switch {
		case d.Online:
			b.Online++
			b.SplitOn++
		case d.Venue != nil:
			b.SplitOffVenue++
		default:
			b.SplitOffLab++
		}
		if in.Now.Sub(d.SeenAt) < 24*time.Hour {
			today++
		}
		if d.HasBatt && in.Now.Sub(d.SeenAt) < 7*24*time.Hour {
			batt++
			if d.Battery >= 20 {
				battOK++
			}
		}
		if !withAlert[d.ID] {
			clean++
		}
	}
	b.OnlinePct = pctOf(b.Online, b.Total)
	b.OnlineTone = ovTone(b.OnlinePct)

	add := func(name string, n, of int, href, why string) {
		if of <= 0 {
			return
		}
		p := pctOf(n, of)
		b.Parts = append(b.Parts, ovPart{N: len(b.Parts) + 1, Name: name, Pct: p, Tone: ovTone(p), Href: href, Why: why})
	}
	add("Online", b.Online, b.Total, "/devices?status=offline", fmt.Sprintf("%d of %d devices online now", b.Online, b.Total))
	add("Checked in today", today, b.Total, "/devices?status=offline", fmt.Sprintf("%d of %d heard from in 24 h", today, b.Total))
	add("Battery", battOK, batt, "/devices?battery=low", fmt.Sprintf("%d of %d battery devices at 20 %% or more", battOK, batt))
	add("No open alerts", clean, b.Total, "/alerts", fmt.Sprintf("%d of %d devices with no open alert", clean, b.Total))
	if in.CompPct >= 0 {
		b.Parts = append(b.Parts, ovPart{N: len(b.Parts) + 1, Name: "Compliance", Pct: in.CompPct, Tone: ovTone(in.CompPct), Href: "/compliance", Why: "devices passing every required check"})
	}
	add("Locked", in.Facts.Locked, in.Facts.AtVenue, "/lock", fmt.Sprintf("%d of %d devices at a venue locked to their app", in.Facts.Locked, in.Facts.AtVenue))
	add("Clients current", in.ClientUp, in.ClientUp+in.ClientBhd, "/clients", fmt.Sprintf("%d of %d on the hosted MDM client", in.ClientUp, in.ClientUp+in.ClientBhd))
	if len(b.Parts) >= 3 {
		b.Radar = ovRadarOf(b.Parts)
	}

	// Today's tabs (T2): each item is a list row and a detail made for its kind.
	bySerial := make(map[string]ovDev, len(in.Devs))
	for _, d := range in.Devs {
		bySerial[d.Serial] = d
	}
	devRow := func(d ovDev) ovHeroDev {
		r := ovHeroDev{Serial: d.Serial, Type: ovPillShort(d.Class), Online: d.Online, Href: "/devices/" + d.Serial}
		if r.Type == "" {
			r.Type = "—"
		}
		if d.Online {
			r.Seen = "online"
		} else if !d.SeenAt.IsZero() {
			r.Seen = agoShort(d.SeenAt)
		}
		if d.HasBatt {
			r.Extra = strconv.Itoa(d.Battery) + " %"
		} else {
			r.Extra = "mains"
		}
		return r
	}
	venueDetail := func(h *ovHero, venue uuid.UUID) {
		types := map[string]*ovTypeBar{}
		var order []string
		var off []ovDev
		for _, d := range in.Devs {
			if d.Venue == nil || *d.Venue != venue {
				continue
			}
			l := ovPillShort(d.Class)
			if l == "" {
				l = "No role"
			}
			if types[l] == nil {
				types[l] = &ovTypeBar{Label: l}
				order = append(order, l)
			}
			types[l].Total++
			if d.Online {
				types[l].Online++
			} else {
				off = append(off, d)
			}
		}
		for _, l := range order {
			t := types[l]
			t.Pct = pctOf(t.Online, t.Total)
			t.Tone = map[bool]string{true: "g", false: "o"}[t.Pct >= 55]
			if t.Pct < 30 {
				t.Tone = "r"
			}
			h.Bars = append(h.Bars, *t)
		}
		sort.Slice(off, func(i, j int) bool { return off[i].SeenAt.Before(off[j].SeenAt) })
		for i, d := range off {
			if i == 6 {
				h.MoreDevs = len(off) - 6
				break
			}
			h.Devs = append(h.Devs, devRow(d))
		}
		if len(off) > 0 {
			h.DevsHead = "Offline here"
		}
	}
	crashDetail := func(h *ovHero) {
		h.Facts = []ovFact{{strconv.Itoa(in.Crashes), "crashes · 24 h"}, {strconv.Itoa(in.CrashesPrv), "the day before"}}
		if len(in.CrashTop) > 0 && in.Crashes > 0 {
			top := in.CrashTop[0]
			h.Facts = append(h.Facts, ovFact{fmt.Sprintf("%d %%", pctOf(top.Count, in.Crashes)), "from " + top.Serial})
			if top.Count*2 > in.Crashes {
				where := top.Restaurant
				if where == "" {
					where = "Lab"
				}
				h.Sub = fmt.Sprintf("%d of them come from one device: %s (%s)", top.Count, top.Serial, where)
			}
		}
		for _, c := range in.CrashTop {
			where := c.Restaurant
			if where == "" {
				where = "Lab"
			}
			h.Devs = append(h.Devs, ovHeroDev{Serial: c.Serial, Type: where, Seen: agoShort(c.LatestAt), Extra: strconv.Itoa(c.Count) + " crashes", Href: "/devices/" + c.Serial})
		}
		if len(h.Devs) > 0 {
			h.DevsHead = "Most crashes"
		}
	}

	needs := ovHeroTab{Key: "needs", Label: "Needs you", Empty: "Nothing needs you right now."}
	for _, t := range in.Todos {
		tag := map[string]string{"r": "Critical", "o": "Warning", "y": "Watch", "g": "Info"}[t.Tone]
		if tag == "" {
			tag, t.Tone = "Info", "g"
		}
		h := ovHero{Tag: tag, Tone: t.Tone, Title: t.Title, Sub: t.Sub, Action: t.Action, Href: t.Href}
		switch t.Kind {
		case "alert":
			if t.Devices > 1 {
				h.Title = fmt.Sprintf("%s · %d devices", t.Title, t.Devices)
			}
			if t.Where != "" {
				h.Title += " at " + t.Where
			}
			h.Facts = append(h.Facts, ovFact{strconv.Itoa(max(t.Devices, len(t.Serials))), "device" + sIfN(max(t.Devices, len(t.Serials)))})
			if !t.Since.IsZero() {
				h.Facts = append(h.Facts, ovFact{agoShort(t.Since), "since"})
			}
			where := t.Where
			if where == "" {
				where = "Lab"
			}
			h.Facts = append(h.Facts, ovFact{where, "where"})
			for i, sn := range t.Serials {
				if i == 6 {
					h.MoreDevs = len(t.Serials) - 6
					break
				}
				if d, ok := bySerial[sn]; ok {
					h.Devs = append(h.Devs, devRow(d))
				} else {
					h.Devs = append(h.Devs, ovHeroDev{Serial: sn, Type: "—", Href: "/devices/" + sn})
				}
			}
			if len(h.Devs) > 0 {
				h.DevsHead = "Affected"
			}
		case "venue":
			if t.VenueID != nil {
				venueDetail(&h, *t.VenueID)
			}
		case "spike":
			crashDetail(&h)
			h.Alt, h.AltHref = "Fleet health", "/fleet-health"
		case "waiting":
			h.Facts = []ovFact{{strconv.Itoa(t.Devices), "waiting for a venue"}}
			if in.Facts.ToPlace > 0 {
				h.Alt, h.AltHref = "Open Setup", "/visits"
			}
		}
		h.List, h.ListS = t.Title, t.Sub
		needs.Items = append(needs.Items, h)
	}

	venues := ovHeroTab{Key: "venues", Label: "Venues", Empty: "Every venue is healthy."}
	vs := append([]venueRow(nil), in.Venues...)
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].H.Score < vs[j].H.Score })
	for _, v := range vs {
		if v.Devices == 0 || (!v.NeedsLook && v.Offline == 0) {
			continue
		}
		tag, tone := "Watch", "y"
		if v.Class == "danger" {
			tag, tone = "At risk", "r"
		}
		h := ovHero{Tag: tag, Tone: tone, Title: v.Name, Action: "Open venue", Href: v.Href,
			ListS: fmt.Sprintf("%d of %d online", v.Online, v.Devices)}
		h.Facts = []ovFact{{fmt.Sprintf("%d/%d", v.Online, v.Devices), "online"}}
		if v.HasH {
			h.Facts = append(h.Facts, ovFact{strconv.Itoa(v.H.Score), "health"})
			if v.H.OpenCritical > 0 {
				h.Facts = append(h.Facts, ovFact{strconv.Itoa(v.H.OpenCritical), "critical alert" + sIfN(v.H.OpenCritical)})
			}
		}
		h.Sub = fmt.Sprintf("%d of %d online · %d offline", v.Online, v.Devices, v.Offline)
		if v.Sub != "" && v.Sub != "no address" {
			h.Sub = v.Sub + " · " + h.Sub
		}
		if id, err := uuid.Parse(v.ID); err == nil {
			venueDetail(&h, id)
			h.Alt, h.AltHref = "Show devices", "/devices?restaurant="+v.ID
		}
		venues.Items = append(venues.Items, h)
		if len(venues.Items) == 10 {
			break
		}
	}

	rolls := ovHeroTab{Key: "rollouts", Label: "Rollouts", Empty: "No rollouts in the last seven days."}
	rs := append([]db.Rollout(nil), in.Rollouts...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Start.After(rs[j].Start) })
	for _, r := range rs {
		name, _ := ovRolloutName(r)
		who, _, _ := ovWho(r.Who)
		waiting := r.Total - r.Done - r.InFlight - r.Failed
		if waiting < 0 {
			waiting = 0
		}
		tag, tone := "Done", "g"
		switch {
		case r.Failed > 0 && r.Done == 0 && r.Failed >= r.Total:
			tag, tone = "Failed", "r"
		case r.Failed > 0 && r.Done+r.Failed >= r.Total:
			tag, tone = "Failed some", "o"
		case r.Done >= r.Total:
		case r.InFlight == 0 && in.Now.Sub(r.Start) > 24*time.Hour:
			tag, tone = "Stalled", "o"
		default:
			tag, tone = "Running", "y"
		}
		h := ovHero{Tag: tag, Tone: tone, Title: name, Action: "Open", Href: ovRolloutHref(r, in.AppLibrary),
			Sub:   fmt.Sprintf("%s · started %s", who, agoShort(r.Start)),
			ListS: fmt.Sprintf("%d / %d", r.Done, r.Total)}
		h.Facts = []ovFact{{strconv.Itoa(r.Total), "devices"}, {fmt.Sprintf("%d %%", pctOf(r.Done, r.Total)), "done"}}
		if r.Failed > 0 {
			h.Facts = append(h.Facts, ovFact{strconv.Itoa(r.Failed), "failed"})
		}
		h.Split = []ovSplitPart{{r.Done, "g", "installed"}, {r.InFlight, "y", "in flight"}, {r.Failed, "r", "failed"}, {waiting, "c", "waiting"}}
		rolls.Items = append(rolls.Items, h)
		if len(rolls.Items) == 10 {
			break
		}
	}

	crash := ovHeroTab{Key: "crashes", Label: "Crashes", Empty: "No crashes in the last 24 h."}
	b.CrashTag, b.CrashTone = "Steady", "g"
	if in.Crashes > in.CrashesPrv && in.Crashes >= 5 {
		b.CrashTag, b.CrashTone = "Spike", "r"
	} else if in.Crashes > 0 {
		b.CrashTag, b.CrashTone = "Watch", "y"
	}
	if in.Crashes > 0 {
		dir := "up"
		if in.Crashes < in.CrashesPrv {
			dir = "down"
		}
		h := ovHero{Tag: b.CrashTag, Tone: b.CrashTone,
			Title: fmt.Sprintf("%d crash%s in 24 h", in.Crashes, map[bool]string{true: "", false: "es"}[in.Crashes == 1]),
			Sub:   fmt.Sprintf("%s from %d the day before", dir, in.CrashesPrv), Action: "See crashes", Href: "/alerts?view=crashes",
			List: "All crashes", ListS: fmt.Sprintf("%d · 24 h", in.Crashes)}
		crashDetail(&h)
		crash.Items = append(crash.Items, h)
		for _, c := range in.CrashTop {
			where := c.Restaurant
			if where == "" {
				where = "Lab"
			}
			ch := ovHero{Tag: "Device", Tone: "o", Title: fmt.Sprintf("%d crashes on %s", c.Count, c.Serial),
				Sub: where + " · latest " + agoShort(c.LatestAt), Action: "Open device", Href: "/devices/" + c.Serial,
				List: c.Serial, ListS: fmt.Sprintf("%d · %s", c.Count, where)}
			ch.Facts = []ovFact{{strconv.Itoa(c.Count), "crashes · 24 h"}, {fmt.Sprintf("%d %%", pctOf(c.Count, in.Crashes)), "of the fleet's"}}
			if c.Kind != "" {
				ch.Facts = append(ch.Facts, ovFact{strings.ReplaceAll(strings.ToLower(c.Kind), "_", " "), "latest kind"})
			}
			if c.BuildID != "" {
				ch.Facts = append(ch.Facts, ovFact{c.BuildID, "build"})
			}
			if d, ok := bySerial[c.Serial]; ok {
				ch.Devs = []ovHeroDev{devRow(d)}
				ch.DevsHead = "Device"
			}
			crash.Items = append(crash.Items, ch)
		}
	}
	for _, t := range []*ovHeroTab{&needs, &venues, &rolls, &crash} {
		for i := range t.Items {
			if t.Items[i].List == "" {
				t.Items[i].List = t.Items[i].Title
			}
			if t.Items[i].ListS == "" {
				t.Items[i].ListS = t.Items[i].Sub
			}
		}
	}
	b.Tabs = []ovHeroTab{needs, venues, rolls, crash}

	// Devices by type: every class as a dial, biggest first (user, 5 Nov 2026: MPOS and the
	// menu boards were cut off at four).
	cs := append([]db.ClassOnline(nil), in.ClassOn...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Total > cs[j].Total })
	circ := 2 * math.Pi * 33
	for _, c := range cs {
		if c.Class == "" || c.Total == 0 {
			continue
		}
		f := float64(c.Online) / float64(c.Total)
		tone := "r"
		if f >= .55 {
			tone = "g"
		} else if f >= .3 {
			tone = "o"
		}
		b.Types = append(b.Types, ovTypeRing{Label: ovPillShort(c.Class), Online: c.Online, Total: c.Total,
			Dash: fmt.Sprintf("%.1f", circ*f), Circ: fmt.Sprintf("%.1f", circ), Tone: tone, Href: "/devices?class=" + c.Class})
	}
	return b
}

// ovRadarOf lays the parts out on a radar 86 units across, centred on 0,0.
func ovRadarOf(parts []ovPart) ovRadar {
	const R = 86.0
	n := len(parts)
	ang := func(i int) float64 { return -math.Pi/2 + float64(i)*2*math.Pi/float64(n) }
	pt := func(i int, r float64) (string, string) {
		return fmt.Sprintf("%.1f", math.Cos(ang(i))*r), fmt.Sprintf("%.1f", math.Sin(ang(i))*r)
	}
	var out ovRadar
	for k := 1; k <= 4; k++ {
		s := ""
		for i := 0; i < n; i++ {
			x, y := pt(i, R*float64(k)/4)
			s += x + "," + y + " "
		}
		out.Rings = append(out.Rings, s)
	}
	area := ""
	for i, p := range parts {
		x, y := pt(i, R)
		out.Axes = append(out.Axes, [2]string{x, y})
		lx, ly := pt(i, R+14)
		lyf, _ := strconv.ParseFloat(ly, 64)
		out.Labels = append(out.Labels, ovRadarLabel{X: lx, Y: fmt.Sprintf("%.1f", lyf+4), N: p.N})
		dx, dy := pt(i, R*float64(p.Pct)/100)
		area += dx + "," + dy + " "
		out.Dots = append(out.Dots, [2]string{dx, dy})
	}
	out.Area = area
	return out
}
