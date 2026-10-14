package dashboard

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Role alerts: the two restaurant-device faults the role model can see from the server
// alone (static/device-role-model-plan.html §2, Alerts). Both clear themselves.
//
//   - role_app_stuck  a deployed device keeps failing to install its role's prod build
//   - role_unlocked   a deployed device was taken out of its role lock on site and nobody
//                     has answered it for a while
//
// "Role app not running" needs the device to report its foreground app; no client does
// yet, so it is not raised here. Devices in a lab session raise nothing (upsertAlert).

const (
	roleStuckAfterFailures = 3
	roleStuckWindow        = 24 * time.Hour
	roleUnlockedAfter      = 2 * time.Hour
)

// RunRoleAlerts raises and clears the role alerts. Safe to call every minute.
func (h *Handler) RunRoleAlerts(ctx context.Context) {
	st, err := h.loadRoleAppState(ctx)
	if err != nil || len(st.Apps) == 0 {
		return
	}
	changed := false

	// Stuck behind prod.
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	if err != nil {
		return
	}
	var bases []string
	for _, b := range st.Apps {
		bases = append(bases, b)
	}
	installed, err := h.db.FamilyVersions(ctx, bases)
	if err != nil {
		return
	}
	var ids []uuid.UUID
	for i := range devices {
		ids = append(ids, devices[i].ID)
	}
	failed, err := h.db.FailedInstalls(ctx, ids, roleStuckWindow)
	if err != nil {
		log.Printf("[role-alerts] failed installs: %v", err)
		return
	}
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() || d.RestaurantID == nil {
			continue
		}
		if _, inLab := st.Labs[d.ID]; inLab {
			continue
		}
		target := st.targetFor(d)
		if target == nil {
			continue
		}
		step := planAppStep(st.Apps[d.Class()], installed[d.ID], target)
		if step.Install == nil {
			if n, _ := h.db.ResolveOpenAlert(ctx, "role_app_stuck", d.ID); n > 0 {
				changed = true
			}
			continue
		}
		if n := failed[d.ID][target.ApkURL]; n >= roleStuckFailures() {
			summary := fmt.Sprintf("%s %s failed to install %d times in the last day. The device is still on %s.",
				target.Name, target.VersionName, n, versionOr(installed[d.ID][target.PackageName], "no build of it"))
			if ins, err := h.db.CreateAlertIfAbsent(ctx, nil, "role_app_stuck", d.ID, "warning", summary,
				map[string]any{"package": target.PackageName, "version": target.VersionName, "failures": n}); err == nil && ins {
				changed = true
			}
		}
	}

	// Unlocked too long.
	_, kdevs, err := h.kioskSnapshot(ctx)
	if err == nil {
		for _, k := range kdevs {
			isRole := k.Rule != nil && k.Rule.ID == roleRuleID(k.Dev.Class())
			if isRole && k.State.ExitedAt != nil && time.Since(*k.State.ExitedAt) > roleUnlockedAfter {
				summary := "Taken out of its lock on site " + roughSince(*k.State.ExitedAt) + " ago and not locked again."
				if ins, err := h.db.CreateAlertIfAbsent(ctx, nil, "role_unlocked", k.Dev.ID, "warning", summary,
					map[string]any{"exited_at": k.State.ExitedAt.UTC()}); err == nil && ins {
					changed = true
				}
				continue
			}
			if k.State.ExitedAt == nil {
				if n, _ := h.db.ResolveOpenAlert(ctx, "role_unlocked", k.Dev.ID); n > 0 {
					changed = true
				}
			}
		}
	}
	if changed {
		h.hub.PublishAlertUpdate()
	}
}

func roleStuckFailures() int { return roleStuckAfterFailures }

func versionOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// roughSince renders how long ago t was, to the hour.
func roughSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
