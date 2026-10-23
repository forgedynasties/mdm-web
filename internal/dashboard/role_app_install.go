package dashboard

import (
	"context"
	"log"
	"regexp"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Role app installs (user, 23 Oct 2026: "when the host app is set for a class, any unit
// that boots up should install the prod app, if the prod is not installed. any unit
// deployed or not"). Every online device whose product names an app on Lineup and that
// lacks its host app is sent the library's newest build of it, at a venue or not, now
// rather than at its next boot (the user's pick). The host app is the prod package (the
// base, e.g. aio.app.nugget) unless the device's venue chose a channel (V1: Venue
// settings › Host app; "uatv2" → aio.app.nugget.uatv2). Another variant doesn't count as
// having it, and nothing is uninstalled or upgraded.
//
// Like client auto-update: present devices only, 20 a minute, never a second install
// while one is on its way, and an APK sent to a device is not sent again for 6 hours, so
// a device that can't install it is not asked every minute — except a transient failure
// (the download was cut by a server restart or a Wi-Fi drop, a 5xx, the stall sweep),
// which is tried again after 15 minutes. A package with no APK in the library gets
// nothing until one is uploaded.

const (
	roleAppInstallBatch     = 20
	roleAppInstallRetry     = 6 * time.Hour
	roleAppInstallRetrySoon = 15 * time.Minute
)

var transientInstallRe = regexp.MustCompile(`(?i)(unexpected end of stream|ProtocolException|SocketException|SocketTimeout|timeout|connection reset|HTTP 5[0-9][0-9])`)

// transientInstall is the Go side of db.TransientInstallSQL.
func transientInstall(output string) bool {
	return output == "" || transientInstallRe.MatchString(output)
}

// hostAppTarget is the package a device should have: base, or base.<channel> when its
// venue chose a channel.
func hostAppTarget(base, channel string) string {
	if base == "" || channel == "" {
		return base
	}
	return base + "." + channel
}

// roleAppInstallBases is role → base package for the roles with auto-install on (Lineup ›
// Apps switch, on by default).
func (h *Handler) roleAppInstallBases(ctx context.Context) map[string]string {
	apps, err := h.db.ListRoleApps(ctx)
	if err != nil {
		log.Printf("[role-app-install] role apps: %v", err)
		return nil
	}
	out := map[string]string{}
	for _, a := range apps {
		if a.AutoInstall && a.BasePackage != "" {
			out[a.Role] = a.BasePackage
		}
	}
	return out
}

type roleAppInstall struct {
	Device *db.Device
	App    *db.App
}

// newestBuilds is the library's newest upload with an APK for each of these packages.
func newestBuilds(apps []db.App, pkgs map[string]bool) map[string]*db.App {
	out := map[string]*db.App{}
	for i := range apps {
		a := &apps[i]
		if !pkgs[a.PackageName] || a.ApkURL == "" {
			continue
		}
		if cur := out[a.PackageName]; cur == nil || a.CreatedAt.After(cur.CreatedAt) {
			out[a.PackageName] = a
		}
	}
	return out
}

// newestProdBuilds is newestBuilds for the roles' base packages.
func newestProdBuilds(apps []db.App, bases map[string]string) map[string]*db.App {
	want := map[string]bool{}
	for _, b := range bases {
		if b != "" {
			want[b] = true
		}
	}
	return newestBuilds(apps, want)
}

// deviceTargets is each device's host app package (devices whose product has no app
// left out).
func deviceTargets(devices []db.Device, bases map[string]string, venueCh map[uuid.UUID]string) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for i := range devices {
		d := &devices[i]
		base := bases[d.Class()]
		if base == "" {
			continue
		}
		ch := ""
		if d.RestaurantID != nil {
			ch = venueCh[*d.RestaurantID]
		}
		out[d.ID] = hostAppTarget(base, ch)
	}
	return out
}

// pickRoleAppInstalls chooses this tick's installs. target is each device's host app
// package, lib the newest build per package, missing per package the devices known not
// to have it; skip holds devices with an install on its way or sent this APK recently.
func pickRoleAppInstalls(devices []db.Device, target map[uuid.UUID]string, lib map[string]*db.App,
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
		pkg := target[d.ID]
		app := lib[pkg]
		if pkg == "" || app == nil || !missing[pkg][d.ID] || !d.Supports("install_apk") {
			continue
		}
		out = append(out, roleAppInstall{Device: d, App: app})
	}
	return out
}

// RunRoleAppInstalls sends each online device its host app where it lacks it. Safe to
// call every minute.
func (h *Handler) RunRoleAppInstalls(ctx context.Context) {
	bases := h.roleAppInstallBases(ctx)
	if len(bases) == 0 {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		log.Printf("[role-app-install] library: %v", err)
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		log.Printf("[role-app-install] ListDevices: %v", err)
		return
	}
	venueCh, err := h.db.VenueHostChannels(ctx)
	if err != nil {
		log.Printf("[role-app-install] venue channels: %v", err)
		return
	}
	target := deviceTargets(devices, bases, venueCh)
	want := map[string]bool{}
	for _, p := range target {
		want[p] = true
	}
	lib := newestBuilds(apps, want)
	if len(lib) == 0 {
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second
	now := time.Now()
	byPkg := map[string][]uuid.UUID{}
	var ids []uuid.UUID
	for i := range devices {
		d := &devices[i]
		pkg := target[d.ID]
		if lib[pkg] == nil || d.Hidden || d.Retired() || now.Sub(d.LastSeenAt) > seenWithin {
			continue
		}
		byPkg[pkg] = append(byPkg[pkg], d.ID)
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		return
	}
	missing := map[string]map[uuid.UUID]bool{}
	var urls []string
	for pkg, pids := range byPkg {
		if missing[pkg], err = h.db.DevicesWithoutPackage(ctx, pids, pkg); err != nil {
			log.Printf("[role-app-install] inventory %s: %v", pkg, err)
			return
		}
		urls = append(urls, lib[pkg].ApkURL)
	}
	skip, err := h.db.InFlightInstalls(ctx, ids)
	if err != nil {
		log.Printf("[role-app-install] in-flight: %v", err)
		return
	}
	recent, err := h.db.RecentInstallsOf(ctx, ids, urls, roleAppInstallRetry, roleAppInstallRetrySoon)
	if err != nil {
		log.Printf("[role-app-install] recent: %v", err)
		return
	}
	for id := range recent {
		skip[id] = true
	}
	for _, j := range pickRoleAppInstalls(devices, target, lib, missing, skip, seenWithin, now) {
		if err := h.queueAppInstall(ctx, j.Device, j.App, "role-app-install"); err != nil {
			log.Printf("[role-app-install] %s on %s: %v", j.App.PackageName, j.Device.SerialNumber, err)
			continue
		}
		log.Printf("[role-app-install] %s %s → %s (host app missing)", j.App.PackageName, j.App.VersionName, j.Device.SerialNumber)
	}
}
