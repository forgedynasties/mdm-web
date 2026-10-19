package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The App section of a role on Lineup (A1, picked 19 Oct 2026): which app the role's
// devices are there to run, and the builds of it they are actually on — drawn as one
// stacked bar with a legend (B2, picked 19 Oct 2026). It is a name and
// a read-out, nothing more: Lock locks to this app, the firmware client reports on it
// (host_app) and the "not on screen" alert watches it, but nothing installs or moves a
// build by itself. Installs stay by hand, as decided on 14 Oct.

type lineupApp struct {
	Base, Name string
	Choices    []lineupAppChoice
	On, Total  int // role devices with any build of the app, of all the role's devices
	Builds     []lineupBuild
}

type lineupAppChoice struct {
	Package, Label string
}

// lineupBuild is one package of the family across the role's devices. Version is the one
// most of them run ("unknown version" when they report none); Tip names the package and
// its top versions for the bar's tooltip; Pct is its slice of the bar.
type lineupBuild struct {
	Package, Short, Env, EnvTone, Version string
	Versions                              int // distinct versions of this package on them
	Devices                               int
	Pct                                   float64
	Tip                                   string
}

// unknownVersion stands for a device that reports no version name for the package.
const unknownVersion = "unknown version"

// mdmClientFamily is our own MDM client: an aio.app package, never a role's app.
const mdmClientFamily = "aio.app.mdmclient"

func (h *Handler) lineupApp(ctx context.Context, role string, devices []db.Device) lineupApp {
	out := lineupApp{Base: h.db.RoleAppBases(ctx)[role]}
	if out.Base != "" {
		out.Name = hostAppName(out.Base)
	}
	seen := map[string]bool{}
	fams, _ := h.db.FleetAppFamilies(ctx)
	for _, f := range fams {
		if f.Base == mdmClientFamily || seen[f.Base] {
			continue
		}
		seen[f.Base] = true
		out.Choices = append(out.Choices, lineupAppChoice{Package: f.Base, Label: appChoiceLabel(f.Base)})
	}
	if out.Base != "" && !seen[out.Base] {
		out.Choices = append(out.Choices, lineupAppChoice{Package: out.Base, Label: appChoiceLabel(out.Base)})
	}

	inRole := map[uuid.UUID]bool{}
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() || d.Class() != role {
			continue
		}
		out.Total++
		inRole[d.ID] = true
	}
	if out.Base == "" || out.Total == 0 {
		return out
	}
	installed, err := h.db.FamilyVersions(ctx, []string{out.Base})
	if err != nil {
		return out
	}
	out.On, out.Builds = tallyBuilds(out.Base, installed, inRole)
	return out
}

// tallyBuilds counts, over the role's devices, how many run each package of the family
// and at which versions. on is the devices with any of it. Builds come biggest first, and
// their Pct slices add up to 100: a device with two builds counts in both, so the bar
// shows builds, not devices.
func tallyBuilds(base string, installed map[uuid.UUID]map[string]string, inRole map[uuid.UUID]bool) (on int, builds []lineupBuild) {
	type tally struct {
		devices  int
		versions map[string]int
	}
	per := map[string]*tally{}
	for id, pkgs := range installed {
		if !inRole[id] {
			continue
		}
		on++
		for pkg, ver := range pkgs {
			t := per[pkg]
			if t == nil {
				t = &tally{versions: map[string]int{}}
				per[pkg] = t
			}
			t.devices++
			if strings.TrimSpace(ver) == "" {
				ver = unknownVersion
			}
			t.versions[ver]++
		}
	}
	sum := 0
	for pkg, t := range per {
		sum += t.devices
		b := lineupBuild{Package: pkg, Short: pkg, Devices: t.devices, Versions: len(t.versions)}
		suffix := strings.TrimPrefix(strings.TrimPrefix(pkg, base), ".")
		if suffix != "" {
			b.Short = "." + suffix
		}
		b.Env, b.EnvTone = hostAppEnv(suffix)
		// Versions most-run first; a named version wins a tie with "unknown", then the newer.
		vs := make([]string, 0, len(t.versions))
		for v := range t.versions {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool {
			a, c := vs[i], vs[j]
			if t.versions[a] != t.versions[c] {
				return t.versions[a] > t.versions[c]
			}
			if (a == unknownVersion) != (c == unknownVersion) {
				return c == unknownVersion
			}
			return compareVersionNames(a, c) > 0
		})
		b.Version = vs[0]
		parts := make([]string, 0, 4)
		for i, v := range vs {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("%d more", len(vs)-3))
				break
			}
			parts = append(parts, fmt.Sprintf("%s ×%d", v, t.versions[v]))
		}
		b.Tip = pkg + " · " + strings.Join(parts, ", ")
		builds = append(builds, b)
	}
	for i := range builds {
		builds[i].Pct = float64(builds[i].Devices) * 100 / float64(sum)
	}
	sort.Slice(builds, func(i, j int) bool {
		if builds[i].Devices != builds[j].Devices {
			return builds[i].Devices > builds[j].Devices
		}
		return builds[i].Package < builds[j].Package
	})
	return on, builds
}

// appChoiceLabel names a family for the picker: "Nugget · aio.app.nugget".
func appChoiceLabel(base string) string {
	if n := hostAppName(base); n != base {
		return n + " · " + base
	}
	return base
}
