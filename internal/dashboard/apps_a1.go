package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Apps list A1 (picked 21 Oct 2026): the library in the Overview's language. A to-do banner
// (role apps from Lineup with no APK here, apps whose devices run another version than the
// library's, channels where no device runs the library build), Library / Installs / Versions
// cards, one table of every app with a bar of what the fleet runs, and a rail (installs on
// the library version, installs sent per day, Lineup's role apps against the library, the
// drop zone). The tile modals (versions, Install on…, family tools) stay: a row opens its
// app's modal, so every library action is where it was.
//
// The coverage rule from app_coverage.go holds here: an install whose version the device
// did not report is "no version", never "on the library version", and "another version"
// says nothing about older or newer (version strings here do not compare).

type a1Todo struct {
	Title, Sub string
	Action     string
	Href       string // a link (the app page), or…
	AddApps    bool   // …open Add apps
}

type a1Banner struct {
	Kicker, Headline, Tone string // Tone "" (coral) | calm
	Todos                  []a1Todo
	More                   int
	Calm                   string
}

type a1Chan struct {
	Label   string
	Devices int
}

type a1Row struct {
	Href       string // the app page: the prod variant's, or the package's
	Name       string
	Package    string
	Icon       string
	Role       string // the Lineup role whose app this is, "" if none
	RoleLabel  string
	LibVersion string // library's newest prod-variant version, "" if none
	InLibrary  bool
	Devices    int
	OnLib      int
	Other      int
	Unknown    int
	OnLibPct   int // bar widths, of Devices
	OtherPct   int
	UnknownPct int
	Channels   []a1Chan // variants with devices, biggest first (shown when more than one variant)
	MoreChans  int
	Note       string // what the row's numbers say, in words
	Single     string // the one package when the family has one variant (Install on… in place)
	Look       bool   // something on this row needs a look
	Search     string
}

type a1RoleApp struct {
	Role, Label, Package, Version string
	Missing                       bool
}

type a1Rail struct {
	Pct, Offset           string
	PctN                  int
	Tone                  string
	OnLib, Known, Unknown int
	Bars                  []ovRailBar
	BarsMax               int
	Roles                 []a1RoleApp
	RolesMissing          int
}

type ovRailBar struct {
	Pct   int
	Label string
	Title string
	Cur   bool
}

type appsA1 struct {
	Banner a1Banner
	Cards  []ovStat
	Rows   []a1Row
	Rail   a1Rail
	Hidden int // packages seen on devices, not in the library and not a role app
}

type a1Inputs struct {
	Families  []LibFamily
	Apps      []db.App
	Cov       *coverageView
	RoleApps  []db.RoleApp
	Installs  []db.InstallDay // 14 days, oldest first
	AddedLast int             // APKs added in the last 30 days
	Now       time.Time
}

func a1Pct(a, b int) int {
	if b <= 0 {
		return 0
	}
	return a * 100 / b
}

// variantOf finds the family row a package belongs to.
func a1FamilyFor(fams []LibFamily, pkg string) (*LibFamily, *LibVariant) {
	for i := range fams {
		for j := range fams[i].Variants {
			if fams[i].Variants[j].Package == pkg {
				return &fams[i], &fams[i].Variants[j]
			}
		}
	}
	return nil, nil
}

func buildAppsA1(in a1Inputs) appsA1 {
	var v appsA1
	cov := coverageView{ByPkg: map[string]pkgCoverage{}}
	if in.Cov != nil {
		cov = *in.Cov
	}
	roleOf := map[string]string{} // base package → role
	for _, ra := range in.RoleApps {
		if ra.BasePackage != "" {
			roleOf[ra.BasePackage] = ra.Role
		}
	}
	inLib := map[string]bool{}
	for _, a := range in.Apps {
		inLib[a.PackageName] = true
	}
	famHas := func(base string) *LibFamily {
		for i := range in.Families {
			f := &in.Families[i]
			if f.BasePackage == base {
				return f
			}
			for _, vr := range f.Variants {
				if vr.Package == base || strings.HasPrefix(vr.Package, base+".") {
					return f
				}
			}
		}
		return nil
	}

	type issue struct {
		todo    a1Todo
		devices int
	}
	var otherIssues, chanIssues []issue
	totOnLib, totKnown, totUnknown, totInstalls := 0, 0, 0, 0

	for i := range in.Families {
		f := &in.Families[i]
		r := a1Row{Href: "/apps/" + f.Prod().Package, Name: f.Name, Package: f.BasePackage, Icon: f.Icon, InLibrary: true,
			LibVersion: f.Prod().Latest.VersionName}
		if role, ok := roleOf[f.BasePackage]; ok {
			r.Role, r.RoleLabel = role, prod.ClassLabel(role)
		}
		if len(f.Variants) == 1 {
			r.Single = f.Variants[0].Package
		}
		famOther, famOtherUnknown := 0, 0
		for _, vr := range f.Variants {
			c := cov.Of(vr.Package)
			r.Devices += c.Devices
			r.OnLib += c.OnLatest
			r.Unknown += c.Unknown
			r.Other += c.Known - c.OnLatest
			if c.Devices > 0 {
				r.Channels = append(r.Channels, a1Chan{Label: vr.Label, Devices: c.Devices})
			}
			// A channel where no device runs the library build for it.
			if c.Known > 0 && c.LibLatest != "" && c.OnLatest == 0 {
				name := f.Name
				if len(f.Variants) > 1 {
					name = f.Name + " · " + vr.Label
				}
				builds := countKnownVersions(c)
				sub := fmt.Sprintf("%d on %s", c.Known, dp1Plural(builds, "other build", "other builds"))
				if c.Unknown > 0 {
					sub += fmt.Sprintf(", %d report no version", c.Unknown)
				}
				chanIssues = append(chanIssues, issue{a1Todo{
					Title:  fmt.Sprintf("%s: none of its %d devices run %s", name, c.Devices, c.LibLatest),
					Sub:    sub,
					Action: "Open", Href: "/apps/" + vr.Package}, c.Devices})
				r.Look = true
				continue
			}
			famOther += c.Known - c.OnLatest
			famOtherUnknown += c.Unknown
		}
		if famOther > 0 && r.LibVersion != "" {
			sub := "The library holds " + r.LibVersion
			if famOtherUnknown > 0 {
				sub += fmt.Sprintf("; %d more report no version and are left out", famOtherUnknown)
			}
			otherIssues = append(otherIssues, issue{a1Todo{
				Title:  fmt.Sprintf("%s: %s on another version", f.Name, dp1Plural(famOther, "device", "devices")),
				Sub:    sub,
				Action: "Reconcile", Href: "/apps/" + f.Prod().Package}, famOther})
			r.Look = true
		}
		sort.SliceStable(r.Channels, func(a, b int) bool { return r.Channels[a].Devices > r.Channels[b].Devices })
		if len(f.Variants) < 2 {
			r.Channels = nil
		} else if len(r.Channels) > 3 {
			r.MoreChans = len(r.Channels) - 3
			r.Channels = r.Channels[:3]
		}
		totOnLib += r.OnLib
		totKnown += r.OnLib + r.Other
		totUnknown += r.Unknown
		totInstalls += r.Devices
		v.Rows = append(v.Rows, r)
	}

	// Role apps from Lineup, against the library.
	missing := []string{}
	for _, cl := range prod.Classes() {
		var base string
		for _, ra := range in.RoleApps {
			if ra.Role == cl {
				base = ra.BasePackage
			}
		}
		if base == "" {
			continue
		}
		ra := a1RoleApp{Role: cl, Label: prod.ClassLabel(cl), Package: base}
		if f := famHas(base); f != nil {
			ra.Version = f.Prod().Latest.VersionName
		} else {
			ra.Missing = true
			missing = append(missing, prod.ClassLabel(cl))
			// A row for it: what the fleet runs, though nothing here can install it.
			r := a1Row{Href: "/apps/" + base, Name: prod.ClassLabel(cl) + " app", Package: base, Role: cl, RoleLabel: prod.ClassLabel(cl), Look: true}
			var variants []string
			for pkg, c := range cov.ByPkg {
				if pkg == base || strings.HasPrefix(pkg, base+".") {
					r.Devices += c.Devices
					r.Other += c.Known
					r.Unknown += c.Unknown
					if pkg == base && c.AppName != "" {
						r.Name = c.AppName
					}
					if pkg != base {
						variants = append(variants, strings.TrimPrefix(pkg, base+"."))
					}
				}
			}
			sort.Strings(variants)
			if r.Devices > 0 {
				r.Note = "can't be installed from here"
				if len(variants) > 0 {
					r.Note = "incl. " + strings.Join(variants, ", ")
				}
			} else {
				r.Note = "on no device"
			}
			v.Rows = append(v.Rows, r)
		}
		v.Rail.Roles = append(v.Rail.Roles, ra)
	}
	v.Rail.RolesMissing = len(missing)
	// Packages the fleet runs that are neither in the library nor a role app's.
	for pkg, c := range cov.ByPkg {
		if inLib[pkg] || c.Devices == 0 {
			continue
		}
		role := false
		for base := range roleOf {
			if pkg == base || strings.HasPrefix(pkg, base+".") {
				role = true
			}
		}
		if f, _ := a1FamilyFor(in.Families, pkg); f == nil && !role {
			v.Hidden++
		}
	}

	for i := range v.Rows {
		r := &v.Rows[i]
		r.OnLibPct, r.OtherPct, r.UnknownPct = a1Pct(r.OnLib, r.Devices), a1Pct(r.Other, r.Devices), a1Pct(r.Unknown, r.Devices)
		if r.Note == "" {
			switch {
			case r.Devices == 0:
				r.Note = "on no device"
			default:
				bits := []string{}
				if r.OnLib > 0 {
					if r.Single != "" {
						bits = append(bits, fmt.Sprintf("%d on %s", r.OnLib, r.LibVersion))
					} else {
						bits = append(bits, fmt.Sprintf("%d on library builds", r.OnLib)) // each channel's own
					}
				}
				if r.Other > 0 {
					bits = append(bits, fmt.Sprintf("%d other", r.Other))
				}
				if r.Unknown > 0 {
					bits = append(bits, fmt.Sprintf("%d no version", r.Unknown))
				}
				r.Note = strings.Join(bits, " · ")
			}
		}
		r.Search = strings.ToLower(r.Name + " " + r.Package + " " + r.LibVersion)
		for _, c := range r.Channels {
			r.Search += " " + strings.ToLower(c.Label)
		}
	}
	sort.SliceStable(v.Rows, func(a, b int) bool {
		if v.Rows[a].Devices != v.Rows[b].Devices {
			return v.Rows[a].Devices > v.Rows[b].Devices
		}
		return strings.ToLower(v.Rows[a].Name) < strings.ToLower(v.Rows[b].Name)
	})

	// Banner: role apps missing, then other versions, then channels, biggest first.
	var todos []a1Todo
	if n := len(missing); n > 0 {
		todos = append(todos, a1Todo{
			Title:  dp1Plural(n, "role app has", "role apps have") + " no APK here",
			Sub:    joinAnd(missing) + ": set on Host apps, no APK in the library",
			Action: "Add apps", AddApps: true})
	}
	sort.SliceStable(otherIssues, func(a, b int) bool { return otherIssues[a].devices > otherIssues[b].devices })
	sort.SliceStable(chanIssues, func(a, b int) bool { return chanIssues[a].devices > chanIssues[b].devices })
	for _, x := range otherIssues {
		todos = append(todos, x.todo)
	}
	for _, x := range chanIssues {
		todos = append(todos, x.todo)
	}
	pctLib := a1Pct(totOnLib, totKnown)
	b := a1Banner{Kicker: "App library"}
	if totKnown > 0 {
		b.Kicker += " · " + strconv.Itoa(pctLib) + "% of installs on the library version"
	}
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	if n := len(todos) + b.More; n > 0 {
		b.Headline = dp1Plural(n, "thing", "things") + " to look at"
	} else {
		b.Tone, b.Headline = "calm", "The library and the fleet agree"
		b.Calm = "Every role app has an APK here, and devices that report a version run the library's."
	}
	v.Banner = b

	// Cards.
	appsN := len(in.Families)
	never := 0
	for _, r := range v.Rows {
		if r.InLibrary && r.Devices == 0 {
			never++
		}
	}
	lib := ovStat{Title: "Library", Href: "#a1-apps",
		A: ovMetric{Value: strconv.Itoa(appsN), Label: "apps · " + dp1Plural(len(in.Apps), "APK", "APKs"), Tone: "good"},
		B: ovMetric{Value: strconv.Itoa(never), Label: "never installed", Tone: "flat"}}
	if in.AddedLast > 0 {
		lib.A.Delta = "▲ " + strconv.Itoa(in.AddedLast) + " this month"
	}
	week, prevWeek, failed := 0, 0, 0
	n := len(in.Installs)
	for i, d := range in.Installs {
		if i >= n-7 {
			week += d.Sent
			failed += d.Failed
		} else if i >= n-14 {
			prevWeek += d.Sent
		}
	}
	inst := ovStat{Title: "Installs", Href: "#a1-rail",
		A: ovMetric{Value: strconv.Itoa(week), Label: "sent, last 7 days", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(failed), Label: "failed", Tone: "good"}}
	if n >= 14 && week != prevWeek {
		d := week - prevWeek
		if d > 0 {
			inst.A.Delta = fmt.Sprintf("▲ %d vs last week", d)
		} else {
			inst.A.Delta = fmt.Sprintf("▼ %d vs last week", -d)
		}
	}
	if failed > 0 {
		inst.B.Tone = "bad"
	}
	if n >= 7 {
		s := make([]float64, 0, 7)
		for _, d := range in.Installs[n-7:] {
			s = append(s, float64(d.Sent))
		}
		inst.LineA, _, inst.Fill = ovLines(s, nil)
	}
	ver := ovStat{Title: "Versions", Href: "#a1-apps",
		A: ovMetric{Value: strconv.Itoa(totOnLib), Label: "on library version", Delta: fmt.Sprintf("of %d reporting", totKnown), Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(totUnknown), Label: "report no version", Tone: "flat"}}
	v.Cards = []ovStat{lib, inst, ver}

	// Rail.
	rl := &v.Rail
	rl.PctN, rl.OnLib, rl.Known, rl.Unknown = pctLib, totOnLib, totKnown, totUnknown
	rl.Pct = strconv.Itoa(pctLib) + "%"
	if totKnown == 0 {
		rl.Pct = "—"
	}
	rl.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-pctLib)/100)
	switch {
	case totKnown == 0:
	case pctLib >= 80:
	case pctLib >= 50:
		rl.Tone = "warn"
	default:
		rl.Tone = "danger"
	}
	last7 := in.Installs
	if len(last7) > 7 {
		last7 = last7[len(last7)-7:]
	}
	for _, d := range last7 {
		if d.Sent > rl.BarsMax {
			rl.BarsMax = d.Sent
		}
	}
	for i, d := range last7 {
		h := 4
		if rl.BarsMax > 0 && d.Sent > 0 {
			h = d.Sent * 100 / rl.BarsMax
			if h < 8 {
				h = 8
			}
		}
		rl.Bars = append(rl.Bars, ovRailBar{Pct: h, Label: d.Day.Format("2"), Cur: i == len(last7)-1,
			Title: d.Day.Format("Mon 2 Jan") + ": " + dp1Plural(d.Sent, "install sent", "installs sent")})
	}
	_ = totInstalls
	return v
}
