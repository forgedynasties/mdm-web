package dashboard

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

// Role firmware: for a hardware model whose firmware mode is prod, a device at a venue
// stays on the model's prod release by itself. It is the firmware half of the role model
// (static/device-role-model-plan.html, decisions of 14 Oct):
//
//   - only models set to prod (the T7 by default; kiosks are lab only, stock is off)
//   - only devices at a venue, not in a lab session, present, inside the window
//   - only within the device's firmware line: a v2.1.x device follows a v2.1 prod; a
//     device on an older line (the v2.0 T7s) is left alone and shown as such. Moving a
//     line is a migration someone watches, done with ordinary rollouts — there is no
//     bridge logic here on purpose
//   - every filter the Deploy button applies (firmware client, product, nothing active
//     already, no downgrade, an applicable package, an MDM-capable build)
//
// Devices are added to one standing deployment per model and release, so the rollout
// shows up on Updates like any other and its progress is tracked the same way.

const roleFirmwareBatch = 10

// firmwareLine is the major.minor a build belongs to ("v2.1.030" → "2.1"), "" when the
// build id does not start with two numbers.
func firmwareLine(build string) string {
	parts := splitVersion(build)
	if len(parts) < 2 {
		return ""
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return ""
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// firmwareBehind reports whether a device on build should move to prod: same line, and
// older. A device on the prod build, ahead of it, or on another line is left alone.
func firmwareBehind(build, prodVersion string) bool {
	l := firmwareLine(prodVersion)
	if l == "" || firmwareLine(build) != l {
		return false
	}
	return compareVersionNames(build, prodVersion) < 0
}

// RunRoleFirmware adds behind devices to each prod model's standing deployment. Safe to
// call every minute; it does nothing until a model has a prod release set.
func (h *Handler) RunRoleFirmware(ctx context.Context) {
	chans, err := h.db.ListFirmwareChannels(ctx)
	if err != nil {
		log.Printf("[role-firmware] channels: %v", err)
		return
	}
	if len(chans) == 0 {
		return
	}
	pol := h.cfg.RoleRollout(config.RolloutFirmware)
	if !pol.Active() {
		return
	}
	modes, err := h.db.FirmwareModes(ctx)
	if err != nil {
		return
	}
	labs, err := h.db.LabSessions(ctx)
	if err != nil {
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second
	for _, ch := range chans {
		if ch.ReleaseID == nil || ch.Version == "" || db.FirmwareModeOf(modes, ch.Model) != db.FirmwareProd {
			continue
		}
		rel, err := h.db.GetRelease(ctx, *ch.ReleaseID)
		if err != nil || rel.Status != "published" {
			continue
		}
		devs, err := h.db.ListDevices(ctx, db.DeviceFilter{Product: ch.Model}, 0, 100000, "serial", "asc")
		if err != nil {
			log.Printf("[role-firmware] %s devices: %v", ch.Model, err)
			continue
		}
		var cand []uuid.UUID
		for i := range devs {
			d := &devs[i]
			if d.Hidden || d.Retired() || d.IsDPC() || d.RestaurantID == nil || modelKey(d) != ch.Model {
				continue
			}
			if _, inLab := labs[d.ID]; inLab {
				continue
			}
			if time.Since(d.LastSeenAt) > seenWithin || !firmwareBehind(d.BuildID, rel.Version) {
				continue
			}
			if !pol.Allows(deviceNow(d)) {
				continue
			}
			cand = append(cand, d.ID)
		}
		if len(cand) == 0 {
			continue
		}
		var eligible []uuid.UUID
		for _, id := range cand {
			if has, err := h.db.DeviceHasActiveUpdate(ctx, id); err == nil && !has {
				eligible = append(eligible, id)
			}
		}
		if eligible, err = h.db.RemoveDowngradeTargets(ctx, rel.ID, eligible); err != nil {
			continue
		}
		if eligible, err = h.db.RemoveInapplicableTargets(ctx, rel.ID, eligible, false); err != nil {
			continue
		}
		eligible, _ = h.splitOTACapable(ctx, eligible) // legacy-path builds are a manual rollout
		if len(eligible) == 0 {
			continue
		}
		if len(eligible) > roleFirmwareBatch {
			eligible = eligible[:roleFirmwareBatch]
		}
		depID, err := h.standingDeployment(ctx, ch, rel.ID)
		if err != nil {
			log.Printf("[role-firmware] %s deployment: %v", ch.Model, err)
			continue
		}
		if err := h.db.SendUpdateToDevices(ctx, depID, eligible, false); err != nil {
			log.Printf("[role-firmware] %s send: %v", ch.Model, err)
			continue
		}
		h.pushOTAToConnected(ctx, eligible)
		h.nudgeCheckin(eligible)
		log.Printf("[role-firmware] %s: %s queued for %d device(s)", ch.Model, rel.Version, len(eligible))
	}
}

// standingDeployment returns the model's open deployment for the release, opening a new
// one when there is none or the last has finished.
func (h *Handler) standingDeployment(ctx context.Context, ch db.FirmwareChannel, releaseID int) (int, error) {
	if ch.DeploymentID != nil {
		if u, err := h.db.GetUpdate(ctx, *ch.DeploymentID); err == nil && u.ReleaseID == releaseID && u.Status != "complete" && u.Status != "cancelled" {
			return u.ID, nil
		}
	}
	// Reboot right after install: the job only runs inside the quiet window.
	u, err := h.db.CreateReleaseUpdate(ctx, releaseID, "immediate", nil, "prod firmware")
	if err != nil {
		return 0, err
	}
	if err := h.db.SetFirmwareDeployment(ctx, ch.Model, u.ID); err != nil {
		return 0, err
	}
	return u.ID, nil
}
