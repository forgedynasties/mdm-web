package dashboard

import (
	"context"
	"fmt"
	"sort"

	"mdm/internal/db"
)

// Fleet coverage on the app library: for each package the library knows (and each of our
// own packages the fleet reports), how many devices have it and at which versions.
//
// The library page listed what could be installed and said nothing about what IS
// installed, so "is this app actually out there, and is it the version we think" was
// unanswerable from the dashboard. The first run of this query against the live fleet
// found five things nobody could see: one product installed under five package names on
// 168 devices, half of one channel's installs reporting no version at all, a channel
// running a version the library does not hold, and rows for devices that no longer exist.
//
// The rule throughout: an install whose version the device did not report is counted as
// UNKNOWN, never as "up to date". A coverage bar that rounds unknowns up to fine is the
// same class of lie as a dashboard that calls a dead device Online.

// versionCount is one version of one package, and how many devices run it.
type versionCount struct {
	Version string // "" → the device reported no version
	Devices int
	Latest  bool // matches the newest version the library holds for this package
}

// pkgCoverage is the fleet's view of one package.
type pkgCoverage struct {
	Package   string
	AppName   string
	Devices   int // live devices reporting this package
	Known     int // installs whose version the device reported
	Unknown   int // installs with no reported version
	OnLatest  int // installs matching the library's newest version
	Versions  []versionCount
	InLibrary bool
	LibLatest string // the newest version the library holds, "" if none
}

// Pct is coverage against the fleet, for the bar.
func (c pkgCoverage) Pct(fleet int) int {
	if fleet <= 0 {
		return 0
	}
	return c.Devices * 100 / fleet
}

// coverageFinding is something worth acting on, in plain words. These are what the panel
// above the library shows; an empty list means the fleet agrees with the library, which is
// the best thing this page can say.
type coverageFinding struct {
	Severity string // "warn" | "bad"
	Headline string
	Detail   string
	Devices  int
	Package  string
}

type coverageView struct {
	Totals   db.FleetAppTotals
	ByPkg    map[string]pkgCoverage
	Findings []coverageFinding
}

// Coverage returns one package's row, for the template.
func (v coverageView) Of(pkg string) pkgCoverage { return v.ByPkg[pkg] }

// buildCoverage folds install counts and the library together. Pure, so the rules that
// decide what counts as a finding are testable without a database.
func buildCoverage(installs []db.PackageInstall, apps []db.App, totals db.FleetAppTotals) coverageView {
	// The newest version the library holds per package. buildLibrary already sorts a
	// package's rows newest-first with versionNewer, so do the same here rather than
	// inventing a second opinion about which version is current.
	libLatest := map[string]string{}
	for _, a := range apps {
		cur, ok := libLatest[a.PackageName]
		if !ok || (a.VersionName != "" && versionNewer(a.VersionName, cur)) {
			libLatest[a.PackageName] = a.VersionName
		}
	}

	byPkg := map[string]pkgCoverage{}
	for _, in := range installs {
		c := byPkg[in.PackageName]
		c.Package = in.PackageName
		if c.AppName == "" {
			c.AppName = in.AppName
		}
		c.Devices += in.Devices
		if in.Version == "" {
			c.Unknown += in.Devices
		} else {
			c.Known += in.Devices
		}
		c.Versions = append(c.Versions, versionCount{Version: in.Version, Devices: in.Devices})
		byPkg[in.PackageName] = c
	}
	for pkg, c := range byPkg {
		latest, inLib := libLatest[pkg]
		c.InLibrary = inLib
		c.LibLatest = latest
		for i := range c.Versions {
			if latest != "" && c.Versions[i].Version == latest {
				c.Versions[i].Latest = true
				c.OnLatest += c.Versions[i].Devices
			}
		}
		sort.SliceStable(c.Versions, func(i, j int) bool { return c.Versions[i].Devices > c.Versions[j].Devices })
		byPkg[pkg] = c
	}

	return coverageView{Totals: totals, ByPkg: byPkg, Findings: findings(byPkg)}
}

// findings reads the coverage and says what is wrong, worst first. Each one names a number
// of devices, because "58 devices" is actionable where "version drift" is a mood.
func findings(byPkg map[string]pkgCoverage) []coverageFinding {
	var out []coverageFinding
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	for _, p := range pkgs {
		c := byPkg[p]
		name := c.AppName
		if name == "" {
			name = p
		}
		// An install whose version is unknown cannot be judged against anything: we do not
		// know whether it needs updating, and an update aimed at it may be a downgrade.
		if c.Unknown > 0 {
			sev := "warn"
			if c.Unknown == c.Devices {
				sev = "bad"
			}
			out = append(out, coverageFinding{
				Severity: sev, Devices: c.Unknown, Package: p,
				Headline: fmt.Sprintf("%d install%s of %s report no version", c.Unknown, plural(c.Unknown), name),
				Detail:   "Nothing can be said about whether these are current, and an update aimed at them could be a downgrade. Ask these devices for a fresh app inventory.",
			})
		}
		// More than one version of the same app in service across the fleet.
		if known := countKnownVersions(c); known > 1 {
			out = append(out, coverageFinding{
				Severity: "warn", Devices: c.Known, Package: p,
				Headline: fmt.Sprintf("%s is running %d different versions", name, known),
				Detail:   versionSummary(c),
			})
		}
		// Installed but the library has no such version — so there is nothing to compare
		// against and "Update" would be guesswork.
		if c.InLibrary && c.LibLatest != "" && c.Known > 0 && c.OnLatest == 0 {
			out = append(out, coverageFinding{
				Severity: "warn", Devices: c.Known, Package: p,
				Headline: fmt.Sprintf("No device runs the version the library holds for %s", name),
				Detail:   fmt.Sprintf("The library's newest is %s, which is on no device. Either the fleet is behind, or the library is — version strings here are not comparable, so this needs a human.", c.LibLatest),
			})
		}
		// Seen on devices, absent from the library: it cannot be installed or updated from
		// here at all, which is worth knowing before someone tries.
		if !c.InLibrary {
			out = append(out, coverageFinding{
				Severity: "warn", Devices: c.Devices, Package: p,
				Headline: fmt.Sprintf("%s is on %d device%s and not in the library", name, c.Devices, plural(c.Devices)),
				Detail:   "It can be seen but not managed: nothing here can install, update or remove it until an APK for " + p + " is uploaded.",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == "bad"
		}
		return out[i].Devices > out[j].Devices
	})
	return out
}

func countKnownVersions(c pkgCoverage) int {
	n := 0
	for _, v := range c.Versions {
		if v.Version != "" {
			n++
		}
	}
	return n
}

func versionSummary(c pkgCoverage) string {
	s := ""
	for _, v := range c.Versions {
		if v.Version == "" {
			continue
		}
		if s != "" {
			s += " · "
		}
		s += fmt.Sprintf("%s on %d", v.Version, v.Devices)
	}
	if c.Unknown > 0 {
		s += fmt.Sprintf(" · %d with no version", c.Unknown)
	}
	return s
}

// appCoverage reads the fleet's installs for the library page. Best-effort: the library
// must still render if this fails, since it worked without coverage before.
func (h *Handler) appCoverage(ctx context.Context, apps []db.App) *coverageView {
	installs, err := h.db.AppInstallCounts(ctx)
	if err != nil {
		return nil
	}
	totals, err := h.db.FleetAppTotals(ctx)
	if err != nil {
		return nil
	}
	v := buildCoverage(installs, apps, totals)
	return &v
}
