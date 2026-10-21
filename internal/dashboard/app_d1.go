package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mdm/internal/db"
)

// App page D1 (picked 21 Oct 2026): one package in the Overview's language. A banner says
// what to do about it (not in the library; devices on another version to reconcile;
// devices that report no version; devices "without it" that run another channel of the
// same app, so a blind install would put a second channel on them), On devices / Library
// version / Library cards, versions in service, every device with Version and Venue
// pills, and a rail: the coverage ring, the library's versions (APK, admin delete), the
// family's channels with device counts, the family tools (moved here from the list's
// dialog) and a drop zone for a new version. Install and Reconcile stay in the header.

type d1Todo struct {
	Title, Sub string
	Action     string
	Href       string // a link, or…
	HxGet      string // …load the Install on… chooser, or…
	AddApps    bool   // …open Add apps
	Ver        string // with Href "#d1-devices": filter the table to this version ("none" = no version)
}

type d1Version struct {
	Version string // "" = no version reported
	Devices int
	Pct     int
	Latest  bool
}

type d1LibVersion struct {
	ID         string
	Version    string
	Added      string
	AddedShort string
	ApkURL     string
	Latest     bool
}

type d1Channel struct {
	Label, Package string
	Devices        int
	Current        bool
	InLibrary      bool
}

type d1Device struct {
	Serial, Version, Venue string
}

type appD1 struct {
	Package, Name, Icon, Initial string
	Art                          int
	FamilyName                   string
	Family                       *LibFamily // nil for a package in no family row (not in the library)
	Variant                      string     // this package's label in its family

	InLibrary  bool
	LibVersion string
	ApkURL     string

	Fleet, Devices, OnLatest, Other, Unknown, Known int

	Banner a1Banner2
	Cards  []ovStat

	Versions    []d1Version
	LibVersions []d1LibVersion
	Channels    []d1Channel
	Rows        []d1Device
	VerOpts     []string // version pill values, as shown
	Venues      []string

	// Install / Reconcile, resolved before anything is sent.
	CanSend           bool
	MissingCount      int
	OtherCount        int
	OtherChannelCount int // of the devices "without it", how many run another channel
	OtherChannels     []d1Channel

	Ring struct {
		Pct    string
		PctN   int
		Offset string
		Tone   string
	}
}

// a1Banner2 is the banner with D1's richer actions.
type a1Banner2 struct {
	Kicker, Headline, Tone string
	Todos                  []d1Todo
	Calm                   string
}

type d1Inputs struct {
	Package    string
	Cov        pkgCoverage
	Fleet      int
	Apps       []db.App
	Families   []LibFamily
	Siblings   map[string]map[string]bool // other channel package → serials that run it
	SiblingCov map[string]pkgCoverage
	Devices    []db.DeviceAppRow
	Missing    []string // serials without this package (unknown-version rule already applied)
	OtherCount int
	CanSend    bool
}

func buildAppD1(in d1Inputs) appD1 {
	c := in.Cov
	v := appD1{Package: in.Package, Name: c.AppName, Art: artSlot(in.Package), Fleet: in.Fleet,
		Devices: c.Devices, OnLatest: c.OnLatest, Unknown: c.Unknown, Known: c.Known,
		Other: c.Known - c.OnLatest, InLibrary: c.InLibrary, LibVersion: c.LibLatest, CanSend: in.CanSend}

	// Library side: this package's versions, newest first, and its name and icon.
	var mine []db.App
	for _, a := range in.Apps {
		if a.PackageName == in.Package {
			mine = append(mine, a)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		if mine[i].VersionName != "" && mine[j].VersionName != "" && mine[i].VersionName != mine[j].VersionName {
			return versionNewer(mine[i].VersionName, mine[j].VersionName)
		}
		return mine[i].CreatedAt.After(mine[j].CreatedAt)
	})
	for i, a := range mine {
		if i == 0 {
			if a.Name != "" {
				v.Name = a.Name
			}
			v.Icon, v.ApkURL = a.Icon, a.ApkURL
		}
		v.LibVersions = append(v.LibVersions, d1LibVersion{ID: a.ID.String(), Version: a.VersionName,
			Added: a.CreatedAt.Format("2 Jan 2006"), AddedShort: a.CreatedAt.Format("2 Jan"), ApkURL: a.ApkURL, Latest: a.VersionName != "" && a.VersionName == c.LibLatest})
	}
	if v.Name == "" {
		v.Name = in.Package
	}
	if v.Icon == "" {
		v.Initial = strings.ToUpper(string([]rune(v.Name)[0:1]))
	}

	// The family and its channels.
	for i := range in.Families {
		for _, vr := range in.Families[i].Variants {
			if vr.Package == in.Package {
				v.Family = &in.Families[i]
				v.Variant = vr.Label
			}
		}
	}
	if v.Family != nil {
		v.FamilyName = v.Family.Name
		for _, vr := range v.Family.Variants {
			ch := d1Channel{Label: vr.Label, Package: vr.Package, InLibrary: true, Current: vr.Package == in.Package}
			if ch.Current {
				ch.Devices = c.Devices
			} else {
				ch.Devices = in.SiblingCov[vr.Package].Devices
			}
			v.Channels = append(v.Channels, ch)
		}
	}

	// Versions in service, biggest share first; "no version" stays its own row.
	for _, vc := range c.Versions {
		row := d1Version{Version: vc.Version, Devices: vc.Devices, Latest: vc.Latest}
		if c.Devices > 0 {
			row.Pct = vc.Devices * 100 / c.Devices
		}
		v.Versions = append(v.Versions, row)
	}

	// Devices, with the pills' values.
	vers, venues := map[string]bool{}, map[string]bool{}
	for _, d := range in.Devices {
		v.Rows = append(v.Rows, d1Device{Serial: d.Serial, Version: d.Version, Venue: d.Venue})
		vers[d.Version] = true
		if d.Venue != "" {
			venues[d.Venue] = true
		}
	}
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].Version != v.Rows[j].Version {
			return v.Rows[i].Version > v.Rows[j].Version
		}
		return v.Rows[i].Serial < v.Rows[j].Serial
	})
	for _, vc := range v.Versions { // same order as the versions card
		if vers[vc.Version] {
			if vc.Version == "" {
				v.VerOpts = append(v.VerOpts, "none")
			} else {
				v.VerOpts = append(v.VerOpts, vc.Version)
			}
		}
	}
	for name := range venues {
		v.Venues = append(v.Venues, name)
	}
	sort.Strings(v.Venues)

	// What Install would reach, and how many of those already run another channel.
	v.MissingCount, v.OtherCount = len(in.Missing), in.OtherCount
	byChan := map[string]int{}
	for _, sn := range in.Missing {
		hit := false
		for pkg, serials := range in.Siblings {
			if serials[sn] {
				byChan[pkg]++
				hit = true
			}
		}
		if hit {
			v.OtherChannelCount++
		}
	}
	for _, ch := range v.Channels {
		if n := byChan[ch.Package]; n > 0 {
			v.OtherChannels = append(v.OtherChannels, d1Channel{Label: ch.Label, Package: ch.Package, Devices: n})
		}
	}
	sort.SliceStable(v.OtherChannels, func(i, j int) bool { return v.OtherChannels[i].Devices > v.OtherChannels[j].Devices })

	// Banner.
	b := a1Banner2{Kicker: v.Name}
	if v.FamilyName != "" && len(v.Channels) > 1 {
		b.Kicker = v.FamilyName + " · " + v.Variant
	}
	if v.Known > 0 {
		b.Kicker += " · " + strconv.Itoa(a1Pct(v.OnLatest, v.Known)) + "% on the library version"
	}
	var todos []d1Todo
	if !v.InLibrary {
		todos = append(todos, d1Todo{Title: "Not in the library",
			Sub:    fmt.Sprintf("On %s, with no APK here: it can't be installed, updated or removed from the dashboard", dp1Plural(v.Devices, "device", "devices")),
			Action: "Add APK", AddApps: true})
	}
	if v.Other > 0 && v.LibVersion != "" {
		var others []string
		for _, vc := range c.Versions {
			if vc.Version != "" && !vc.Latest && len(others) < 3 {
				others = append(others, fmt.Sprintf("%s on %d", vc.Version, vc.Devices))
			}
		}
		t := d1Todo{Title: fmt.Sprintf("Reconcile %s on another version", dp1Plural(v.Other, "device", "devices")),
			Sub: "The library holds " + v.LibVersion + "; " + strings.Join(others, " · ")}
		if v.CanSend {
			t.Action, t.HxGet = "Reconcile", "/apps/install-targets?pkg="+in.Package
		} else {
			t.Action, t.Href, t.Ver = "See them", "#d1-devices", ""
		}
		todos = append(todos, t)
	}
	if v.OtherChannelCount > 0 && v.MissingCount > 0 {
		var bits []string
		for _, ch := range v.OtherChannels {
			bits = append(bits, fmt.Sprintf("%s %d", ch.Label, ch.Devices))
		}
		todos = append(todos, d1Todo{
			Title:  fmt.Sprintf("%d of the %d devices without it run another %s channel", v.OtherChannelCount, v.MissingCount, v.FamilyName),
			Sub:    strings.Join(bits, " · ") + ". Check which channel each should run before installing this one",
			Action: "See channels", Href: "#d1-channels"})
	}
	if v.Unknown > 0 {
		todos = append(todos, d1Todo{Title: dp1Plural(v.Unknown, "device reports", "devices report") + " no version",
			Sub:    "Left out of installs: an update there could be a downgrade. Ask them for a fresh app inventory",
			Action: "See them", Href: "#d1-devices", Ver: "none"})
	}
	b.Todos = todos
	if n := len(todos); n > 0 {
		b.Headline = dp1Plural(n, "thing", "things") + " to look at"
	} else {
		b.Tone, b.Headline = "calm", "Every device runs the library's build"
		if v.Devices == 0 {
			b.Headline, b.Calm = "On no device yet", "Install it from the header when it is ready to go out."
		} else {
			b.Calm = "Each device that has it reports the version the library holds."
		}
	}
	v.Banner = b

	// Cards.
	on := ovStat{Title: "On devices", Href: "#d1-devices",
		A: ovMetric{Value: strconv.Itoa(v.Devices), Label: "of " + strconv.Itoa(v.Fleet) + " devices", Tone: "flat"},
		B: ovMetric{Value: "—", Label: "without it", Tone: "flat"}}
	if v.CanSend {
		on.B.Value = strconv.Itoa(v.MissingCount)
		if v.OtherChannelCount > 0 {
			on.B.Delta, on.B.Tone = strconv.Itoa(v.OtherChannelCount)+" on other channels", "bad"
		}
	}
	ver := ovStat{Title: "Library version", Href: "#d1-versions",
		A: ovMetric{Value: strconv.Itoa(v.OnLatest), Label: "on " + orDash(v.LibVersion), Delta: fmt.Sprintf("of %d reporting", v.Known), Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(v.Unknown), Label: "report no version", Tone: "flat"}}
	if v.Unknown > 0 {
		ver.B.Tone = "bad"
	}
	lib := ovStat{Title: "Library", Href: "#d1-rail",
		A: ovMetric{Value: strconv.Itoa(len(v.LibVersions)), Label: "versions held", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(max(len(v.Channels), 1)), Label: "channels", Tone: "flat"}}
	if len(v.LibVersions) == 1 {
		lib.A.Label = "version held"
	}
	if len(v.LibVersions) > 0 {
		lib.A.Delta = "newest " + v.LibVersions[0].AddedShort
	}
	if len(v.Channels) <= 1 {
		lib.B.Label = "channel"
	}
	v.Cards = []ovStat{on, ver, lib}

	// Ring: installs on the library version, of those that report one.
	p := a1Pct(v.OnLatest, v.Known)
	v.Ring.PctN, v.Ring.Pct = p, strconv.Itoa(p)+"%"
	if v.Known == 0 {
		v.Ring.Pct = "—"
	}
	v.Ring.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-p)/100)
	switch {
	case v.Known == 0, p >= 80:
	case p >= 50:
		v.Ring.Tone = "warn"
	default:
		v.Ring.Tone = "danger"
	}
	return v
}
