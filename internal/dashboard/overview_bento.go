package dashboard

import (
	"fmt"
	"math"
	"sort"
	"strconv"
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

type ovHero struct {
	Tag, Tone    string // Critical / Warning / Info / Running …; r | o | g | y
	Title, Sub   string
	Action, Href string
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
	Rollouts            ovRollouts
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

	// Today's tabs.
	needs := ovHeroTab{Key: "needs", Label: "Needs you", Empty: "Nothing needs you right now."}
	for _, t := range in.Todos {
		tag := map[string]string{"r": "Critical", "o": "Warning", "y": "Watch", "g": "Info"}[t.Tone]
		if tag == "" {
			tag, t.Tone = "Info", "g"
		}
		needs.Items = append(needs.Items, ovHero{Tag: tag, Tone: t.Tone, Title: t.Title, Sub: t.Sub, Action: t.Action, Href: t.Href})
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
		sub := fmt.Sprintf("%d of %d online", v.Online, v.Devices)
		if v.HasH {
			sub += " · health " + strconv.Itoa(v.H.Score)
		}
		venues.Items = append(venues.Items, ovHero{Tag: tag, Tone: tone, Title: v.Name, Sub: sub, Action: "Open venue", Href: v.Href})
		if len(venues.Items) == 8 {
			break
		}
	}
	rolls := ovHeroTab{Key: "rollouts", Label: "Rollouts", Empty: "No rollouts in the last seven days."}
	for _, r := range in.Rollouts.Bars {
		tag, tone := "Done", "g"
		switch r.Tone {
		case "warn":
			tag, tone = "Stalled", "o"
		case "run":
			tag, tone = "Running", "y"
		case "q":
			tag, tone = "Scheduled", "y"
		}
		rolls.Items = append(rolls.Items, ovHero{Tag: tag, Tone: tone, Title: r.Name, Sub: r.Sub + " · " + r.Count, Action: "Open", Href: r.Href})
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
		crash.Items = append(crash.Items, ovHero{Tag: b.CrashTag, Tone: b.CrashTone,
			Title: fmt.Sprintf("%d crash%s in 24 h", in.Crashes, map[bool]string{true: "", false: "es"}[in.Crashes == 1]),
			Sub:   fmt.Sprintf("%s from %d the day before", dir, in.CrashesPrv), Action: "See crashes", Href: "/alerts?view=crashes"})
	}
	b.Tabs = []ovHeroTab{needs, venues, rolls, crash}

	// Devices by type: the four biggest classes as dials.
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
		if len(b.Types) == 4 {
			break
		}
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
