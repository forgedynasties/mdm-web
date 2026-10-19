package dashboard

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The App section of a role on Lineup (A1, picked 19 Oct 2026): which app the role's
// devices are there to run, and the builds of it they are actually on. It is a name and
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

// lineupBuild is one package of the family across the role's devices, at the version
// most of them run.
type lineupBuild struct {
	Package, Short, Env, EnvTone, Version string
	Versions                              int // distinct versions of this package on them
	Devices                               int
}

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
	type tally struct {
		devices  int
		versions map[string]int
	}
	per := map[string]*tally{}
	for id, pkgs := range installed {
		if !inRole[id] {
			continue
		}
		out.On++
		for pkg, ver := range pkgs {
			t := per[pkg]
			if t == nil {
				t = &tally{versions: map[string]int{}}
				per[pkg] = t
			}
			t.devices++
			t.versions[ver]++
		}
	}
	for pkg, t := range per {
		b := lineupBuild{Package: pkg, Short: pkg, Devices: t.devices, Versions: len(t.versions)}
		suffix := strings.TrimPrefix(strings.TrimPrefix(pkg, out.Base), ".")
		if suffix != "" {
			b.Short = "." + suffix
		}
		b.Env, b.EnvTone = hostAppEnv(suffix)
		top := 0
		for v, n := range t.versions {
			if n > top || (n == top && compareVersionNames(v, b.Version) > 0) {
				b.Version, top = v, n
			}
		}
		out.Builds = append(out.Builds, b)
	}
	sort.Slice(out.Builds, func(i, j int) bool {
		if out.Builds[i].Devices != out.Builds[j].Devices {
			return out.Builds[i].Devices > out.Builds[j].Devices
		}
		return out.Builds[i].Package < out.Builds[j].Package
	})
	return out
}

// appChoiceLabel names a family for the picker: "Nugget · aio.app.nugget".
func appChoiceLabel(base string) string {
	if n := hostAppName(base); n != base {
		return n + " · " + base
	}
	return base
}
