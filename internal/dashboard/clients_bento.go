package dashboard

import (
	"fmt"
	"sort"
)

// MDM clients in the bento layout (R1, picked 23 Oct 2026 from the user's reference
// screenshot, in the MDM's own colours): a hero card with the one thing to know, a
// completion card (share on the hosted build, labelled segments, the auto-update
// controls and Update all behind), the versions in use as tall bars with the hosted one
// marked, and the devices as a soft table, behind first. The version table with its
// rollback and per-version updates stays under it as Version history.

type bentoSeg struct {
	Label string
	N     int
	Frac  int // grid fraction (≥1 when N > 0, so a sliver still shows)
	Class string
}

type bentoBar struct {
	Label  string
	N      int
	Height int // % of the tallest
	On     bool
	Href   string
}

type clientBento struct {
	Hero     string
	Pct      int
	Segs     []bentoSeg
	Bars     []bentoBar
	Devices  []ClientDeviceRow
	MoreDevs int
}

func clientBentoOf(v *ClientSlotView) clientBento {
	b := clientBento{}
	if v == nil {
		return b
	}
	if v.Devices > 0 {
		b.Pct = v.UpToDate * 100 / v.Devices
	}
	ver := v.Build.Version
	// Hero: behind devices, and whether they can take the update now.
	behind, behindOff := 0, 0
	for _, d := range v.Rows {
		if d.State == "behind" && d.Elsewhere == "" {
			behind++
			if d.Unreachable {
				behindOff++
			}
		}
	}
	switch {
	case !v.Hosted:
		b.Hero = "Nothing is hosted for " + v.Label + " yet, so no device can update."
	case behind == 0 && v.Unknown == 0:
		b.Hero = fmt.Sprintf("Every device on %s runs %s.", v.Label, ver)
	case behind == 0:
		b.Hero = fmt.Sprintf("No device that reports a version is behind %s. %d run a client too old to report one.", ver, v.Unknown)
	case behindOff == behind:
		b.Hero = fmt.Sprintf("%d device%s %s behind %s. All of them are offline; they update the minute they're back.", behind, plural(behind), isAre(behind), ver)
	default:
		on := behind - behindOff
		when := "at their next check-in"
		if !v.Auto.On || v.Auto.Paused {
			when = "when someone sends the update (auto-update is off)"
		} else if v.Auto.WindowStart != "" {
			when = "inside " + v.Auto.WindowStart + "–" + v.Auto.WindowEnd
		}
		b.Hero = fmt.Sprintf("%d device%s %s behind %s; %d online update %s.", behind, plural(behind), isAre(behind), ver, on, when)
	}
	add := func(label string, n int, cls string) {
		if n > 0 {
			b.Segs = append(b.Segs, bentoSeg{Label: label, N: n, Class: cls})
		}
	}
	add("On "+ver, v.UpToDate, "on")
	add("Behind", v.Behind, "behind")
	add("No version", v.Unknown, "none")
	add("Can't update here", v.NoBuild, "none")
	total := 0
	for _, s := range b.Segs {
		total += s.N
	}
	for i := range b.Segs {
		b.Segs[i].Frac = max(1, b.Segs[i].N*100/max(total, 1))
	}
	// Bars: the three biggest versions, the rest as "older".
	type vc struct {
		v  string
		n  int
		on bool
	}
	var vs []vc
	for _, r := range v.VersionRows {
		if r.Version == "" || len(r.Devices) == 0 {
			continue
		}
		vs = append(vs, vc{r.Version, len(r.Devices), r.Kind == "hosted"})
	}
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].n > vs[j].n })
	rest := 0
	if len(vs) > 3 {
		for _, x := range vs[3:] {
			rest += x.n
		}
		vs = vs[:3]
	}
	tallest := 1
	for _, x := range vs {
		tallest = max(tallest, x.n)
	}
	tallest = max(tallest, rest)
	for _, x := range vs {
		b.Bars = append(b.Bars, bentoBar{Label: x.v, N: x.n, Height: max(12, x.n*100/tallest), On: x.on, Href: "#cv-tbl"})
	}
	if rest > 0 {
		b.Bars = append(b.Bars, bentoBar{Label: "older", N: rest, Height: max(12, rest*100/tallest), Href: "#cv-tbl"})
	}
	// Devices: behind (reachable first), updating, no version, then up to date.
	rank := func(d ClientDeviceRow) int {
		switch {
		case d.State == "behind" && !d.Unreachable:
			return 0
		case d.State == "updating":
			return 1
		case d.State == "behind":
			return 2
		case d.State == "unknown":
			return 3
		}
		return 4
	}
	devs := append([]ClientDeviceRow(nil), v.Rows...)
	sort.SliceStable(devs, func(i, j int) bool {
		if rank(devs[i]) != rank(devs[j]) {
			return rank(devs[i]) < rank(devs[j])
		}
		return devs[i].LastSeen.After(devs[j].LastSeen)
	})
	if len(devs) > 8 {
		b.MoreDevs = len(devs) - 8
		devs = devs[:8]
	}
	b.Devices = devs
	return b
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// hostAppsBento is Host apps in the same layout (R1): a hero with what blocks the most
// units, the chosen product's completion (segments from its card), and on-prod coverage
// per product as bars (each one opens that product).
type hostAppsBento struct {
	Hero string
	Segs []bentoSeg
	Bars []bentoBar
}

func hostAppsBentoOf(cards []luAppCard, pane *luAppsPane) hostAppsBento {
	b := hostAppsBento{}
	noBuild, blocked, missing := 0, 0, 0
	for _, c := range cards {
		if c.Base == "" || c.Total == 0 {
			continue
		}
		left := c.Total - c.OnProd - c.Running
		if c.Build == "" {
			noBuild++
			blocked += c.Total - c.OnProd
		} else if left > 0 {
			missing += left
		}
	}
	switch {
	case noBuild > 0:
		prods := "product has"
		if noBuild != 1 {
			prods = "products have"
		}
		b.Hero = fmt.Sprintf("%d %s no prod build in the library, so %d unit%s can't get %s app yet.", noBuild, prods, blocked, plural(blocked), map[bool]string{true: "its", false: "their"}[blocked == 1])
		if missing > 0 {
			b.Hero += fmt.Sprintf(" %d more get theirs as they come online.", missing)
		}
	case missing > 0:
		b.Hero = fmt.Sprintf("%d unit%s still lack their host app; they get it as they come online.", missing, plural(missing))
	default:
		b.Hero = "Every unit runs its host app."
	}
	if pane != nil {
		labels := map[string]string{"p": "On prod", "g": "Installing", "v": "Another build", "n": "Without it"}
		classes := map[string]string{"p": "on", "g": "run", "v": "behind", "n": "none"}
		total := 0
		for _, s := range pane.Card.Bar {
			total += s.N
		}
		for _, s := range pane.Card.Bar {
			if s.N > 0 {
				b.Segs = append(b.Segs, bentoSeg{Label: labels[s.Tone], N: s.N, Class: classes[s.Tone], Frac: max(1, s.N*100/max(total, 1))})
			}
		}
	}
	for _, c := range cards {
		if c.Total == 0 {
			continue
		}
		pct := c.OnProd * 100 / c.Total
		b.Bars = append(b.Bars, bentoBar{Label: c.Label, N: pct, Height: max(10, pct), On: c.On, Href: "/apps/products?app=" + c.Role})
	}
	return b
}
