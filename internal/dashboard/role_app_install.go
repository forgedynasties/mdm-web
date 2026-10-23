package dashboard

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Role app installs (user, 23 Oct 2026: "when the host app is set for a class, any unit
// that boots up should install the prod app, if the prod is not installed. any unit
// deployed or not"). Every online device whose product names an app on Lineup and that
// lacks that app's prod package (the base package, e.g. aio.app.nugget) is sent the
// library's newest build of it, at a venue or not, now rather than at its next boot
// (the user's pick). A variant (.uatv2, .stagev2…) doesn't count as having it, and
// nothing is uninstalled or upgraded: a device that has the prod package is left alone.
//
// Like client auto-update: present devices only, 20 a minute, never a second install
// while one is on its way, and an APK sent to a device is not sent again for 6 hours,
// so a device that can't install it is not asked every minute. A product with no prod
// APK in the library gets nothing until one is uploaded.

const (
	roleAppInstallBatch = 20
	roleAppInstallRetry = 6 * time.Hour
)

type roleAppInstall struct {
	Device *db.Device
	App    *db.App
}

// newestProdBuilds is the library's newest upload with an APK for each base package.
func newestProdBuilds(apps []db.App, bases map[string]string) map[string]*db.App {
	want := map[string]bool{}
	for _, b := range bases {
		if b != "" {
			want[b] = true
		}
	}
	out := map[string]*db.App{}
	for i := range apps {
		a := &apps[i]
		if !want[a.PackageName] || a.ApkURL == "" {
			continue
		}
		if cur := out[a.PackageName]; cur == nil || a.CreatedAt.After(cur.CreatedAt) {
			out[a.PackageName] = a
		}
	}
	return out
}

// pickRoleAppInstalls chooses this tick's installs. missing is, per base package, the
// devices known not to have it; skip holds devices with an install on its way or sent
// this APK within the retry window.
func pickRoleAppInstalls(devices []db.Device, bases map[string]string, lib map[string]*db.App,
	missing map[string]map[uuid.UUID]bool, skip map[uuid.UUID]bool, seenWithin time.Duration, now time.Time) []roleAppInstall {
	var out []roleAppInstall
	for i := range devices {
		if len(out) >= roleAppInstallBatch {
			break
		}
		d := &devices[i]
		if d.Hidden || d.Retired() || now.Sub(d.LastSeenAt) > seenWithin || skip[d.ID] {
			continue
		}
		base := bases[d.Class()]
		app := lib[base]
		if base == "" || app == nil || !missing[base][d.ID] || !d.Supports("install_apk") {
			continue
		}
		out = append(out, roleAppInstall{Device: d, App: app})
	}
	return out
}

// RunRoleAppInstalls sends the role's prod app to online devices that lack it. Safe to
// call every minute.
func (h *Handler) RunRoleAppInstalls(ctx context.Context) {
	bases := h.db.RoleAppBases(ctx)
	if len(bases) == 0 {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		log.Printf("[role-app-install] library: %v", err)
		return
	}
	lib := newestProdBuilds(apps, bases)
	if len(lib) == 0 {
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		log.Printf("[role-app-install] ListDevices: %v", err)
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second
	now := time.Now()
	byBase := map[string][]uuid.UUID{}
	var ids []uuid.UUID
	for i := range devices {
		d := &devices[i]
		base := bases[d.Class()]
		if lib[base] == nil || d.Hidden || d.Retired() || now.Sub(d.LastSeenAt) > seenWithin {
			continue
		}
		byBase[base] = append(byBase[base], d.ID)
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		return
	}
	missing := map[string]map[uuid.UUID]bool{}
	var urls []string
	for base, bids := range byBase {
		if missing[base], err = h.db.DevicesWithoutPackage(ctx, bids, base); err != nil {
			log.Printf("[role-app-install] inventory %s: %v", base, err)
			return
		}
		urls = append(urls, lib[base].ApkURL)
	}
	skip, err := h.db.InFlightInstalls(ctx, ids)
	if err != nil {
		log.Printf("[role-app-install] in-flight: %v", err)
		return
	}
	recent, err := h.db.RecentInstallsOf(ctx, ids, urls, roleAppInstallRetry)
	if err != nil {
		log.Printf("[role-app-install] recent: %v", err)
		return
	}
	for id := range recent {
		skip[id] = true
	}
	for _, j := range pickRoleAppInstalls(devices, bases, lib, missing, skip, seenWithin, now) {
		if err := h.queueAppInstall(ctx, j.Device, j.App, "role-app-install"); err != nil {
			log.Printf("[role-app-install] %s on %s: %v", j.App.PackageName, j.Device.SerialNumber, err)
			continue
		}
		log.Printf("[role-app-install] %s %s → %s (prod app missing)", j.App.PackageName, j.App.VersionName, j.Device.SerialNumber)
	}
}
