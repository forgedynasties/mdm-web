package dashboard

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Apps a device should have (db/app_wants.go; plan picked 17 Nov 2026 from FW-2026-000066):
// Add app records a want and this job installs the library's newest build whenever the
// device is online and lacks it; Remove records the opposite. Nothing expires, so an
// offline device gets the app within a minute of coming back. No version tracking: once
// the app is there, it updates itself.

// wantJob is one install or uninstall the job sends.
type wantJob struct {
	Device *db.Device
	Pkg    string
	App    *db.App // the build to install; nil for a removal
}

// wantPick is what pickWants decides from: each device's wants, the library's newest build
// per package, what each device has installed, which devices reported an app list, which
// have an install or uninstall on its way, and what each was sent lately (back-off).
type wantPick struct {
	wants    map[uuid.UUID][]db.AppWant
	lib      map[string]*db.App
	have     map[uuid.UUID]map[string]bool
	reported map[uuid.UUID]bool
	busy     map[uuid.UUID]bool
	recent   map[uuid.UUID]map[string]bool // install urls sent lately
	recentUn map[uuid.UUID]map[string]bool // packages asked to uninstall lately
	batch    int
	force    bool // a person just asked: skip the back-off
}

// pickWants returns at most one job per device (its oldest unmet want), online devices only.
func pickWants(devices []db.Device, p wantPick, seenWithin time.Duration, now time.Time) []wantJob {
	var out []wantJob
	for i := range devices {
		if p.batch > 0 && len(out) >= p.batch {
			break
		}
		d := &devices[i]
		if d.Retired() || now.Sub(d.LastSeenAt) > seenWithin || !p.reported[d.ID] || p.busy[d.ID] {
			continue
		}
		for _, w := range p.wants[d.ID] {
			has := p.have[d.ID][w.Package]
			if w.Want == db.WantAbsent {
				if has && (p.force || !p.recentUn[d.ID][w.Package]) {
					out = append(out, wantJob{Device: d, Pkg: w.Package})
					break
				}
				continue
			}
			if has {
				continue
			}
			app := p.lib[w.Package]
			if app == nil || (!p.force && p.recent[d.ID][app.ApkURL]) {
				continue
			}
			out = append(out, wantJob{Device: d, Pkg: w.Package, App: app})
			break
		}
	}
	return out
}

// RunAppWants is the minute job.
func (h *Handler) RunAppWants(ctx context.Context) {
	h.applyWants(ctx, nil, false)
}

// applyWants sends what the wants of these devices (nil = every device) call for now.
// force skips the back-off: Add app, Remove and Try again act at once.
func (h *Handler) applyWants(ctx context.Context, only []uuid.UUID, force bool) {
	var all []db.AppWant
	var err error
	if len(only) == 1 {
		all, err = h.db.AppWantsOf(ctx, only[0])
	} else {
		all, err = h.db.AllAppWants(ctx)
	}
	if err != nil {
		log.Printf("[app-wants] wants: %v", err)
		return
	}
	keep := map[uuid.UUID]bool{}
	for _, id := range only {
		keep[id] = true
	}
	wants := map[uuid.UUID][]db.AppWant{}
	pkgSet := map[string]bool{}
	var ids []uuid.UUID
	for _, w := range all {
		if len(only) > 0 && !keep[w.DeviceID] {
			continue
		}
		if wants[w.DeviceID] == nil {
			ids = append(ids, w.DeviceID)
		}
		wants[w.DeviceID] = append(wants[w.DeviceID], w)
		pkgSet[w.Package] = true
	}
	if len(ids) == 0 {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		log.Printf("[app-wants] library: %v", err)
		return
	}
	lib := newestBuilds(apps, pkgSet)
	var pkgs, urls []string
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}
	for _, a := range lib {
		urls = append(urls, a.ApkURL)
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{OnlyIDs: ids}, 0, len(ids)+10, "serial", "asc")
	if err != nil {
		log.Printf("[app-wants] devices: %v", err)
		return
	}
	have, err := h.db.DevicePackagesOf(ctx, ids, pkgs)
	if err != nil {
		log.Printf("[app-wants] installed: %v", err)
		return
	}
	reported, err := h.db.PackagesReported(ctx, ids)
	if err != nil {
		return
	}
	busy, err := h.db.InFlightInstalls(ctx, ids)
	if err != nil {
		return
	}
	recent, err := h.db.RecentInstallsOf(ctx, ids, urls, roleAppInstallRetry, roleAppInstallRetrySoon)
	if err != nil {
		return
	}
	recentUn, err := h.db.RecentUninstallsOf(ctx, ids)
	if err != nil {
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second
	p := wantPick{wants: wants, lib: lib, have: have, reported: reported, busy: busy, recent: recent, recentUn: recentUn,
		batch: h.appRolloutBatch(), force: force}
	for _, j := range pickWants(devices, p, seenWithin, time.Now()) {
		if j.App == nil {
			if err := h.queueUninstall(ctx, j.Device, j.Pkg, "app-wants"); err != nil {
				log.Printf("[app-wants] remove %s on %s: %v", j.Pkg, j.Device.SerialNumber, err)
			} else {
				log.Printf("[app-wants] remove %s → %s", j.Pkg, j.Device.SerialNumber)
			}
			continue
		}
		if err := h.queueAppInstall(ctx, j.Device, j.App, "app-wants"); err != nil {
			log.Printf("[app-wants] add %s on %s: %v", j.Pkg, j.Device.SerialNumber, err)
			continue
		}
		log.Printf("[app-wants] add %s %s → %s", j.Pkg, j.App.VersionName, j.Device.SerialNumber)
	}
}

// roleAppPackages is the device's role app: its product's base package and the build the
// venue runs (Venue settings › Host app). These can't be removed (user, 17 Nov 2026).
func (h *Handler) roleAppPackages(ctx context.Context, d *db.Device) map[string]bool {
	out := map[string]bool{}
	ras, err := h.db.ListRoleApps(ctx)
	if err != nil {
		return out
	}
	base := ""
	for _, ra := range ras {
		if ra.Role == d.Class() {
			base = ra.BasePackage
		}
	}
	if base == "" {
		return out
	}
	out[base] = true
	if d.RestaurantID != nil {
		if venueCh, err := h.db.VenueHostChannels(ctx); err == nil {
			out[hostAppTarget(base, venueCh[*d.RestaurantID][d.Class()])] = true
		}
	}
	return out
}

// chosenChannelOf reports, per device, the base packages one of whose channels someone
// added (aio.app.pos.uatv2 → aio.app.pos): the role app's auto-install leaves those devices
// on what they were given (a QA tablet on UAT is not handed prod).
func chosenChannelOf(wants []db.AppWant) map[uuid.UUID][]string {
	out := map[uuid.UUID][]string{}
	for _, w := range wants {
		if w.Want == db.WantPresent {
			out[w.DeviceID] = append(out[w.DeviceID], w.Package)
		}
	}
	return out
}

// choseOtherChannel: the device was given another channel of base than target.
func choseOtherChannel(pkgs []string, base, target string) bool {
	for _, p := range pkgs {
		if p != target && (p == base || strings.HasPrefix(p, base+".")) {
			return true
		}
	}
	return false
}
