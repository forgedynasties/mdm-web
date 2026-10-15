package dashboard

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/apkmeta"
	"mdm/internal/config"
	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Role apps: every role runs one AIO app, and a device at a venue stays on that app's
// prod build by itself (static/device-role-model-plan.html).
//
// The job below is the app half of that promise. It is built like client auto-update,
// which already works in the field: only devices that are present, only inside their own
// quiet window, a bounded batch per tick, and never a second command while one is on
// its way. It acts only for roles whose prod build is set, so an empty Releases page
// moves nothing.
//
// What a device should run:
//   - in a lab session that chose a package → that package's channel build
//   - in a lab session that chose nothing   → left alone
//   - at a venue (deployed), not in lab      → the role's prod build (its base package)
//   - not at a venue, not in lab             → left alone: a bench unit is somebody's
//
// One package of the family at a time: the target is installed first, and only once it
// is on the device at the right version are the family's other packages removed, so a
// device is never left with no app. A device already on a newer build of the target
// package is left alone ("ahead"): Android refuses an in-place downgrade, and variants
// being separate packages is what makes going back to prod a swap, not a downgrade.

const (
	roleAppBatch = 20
	// roleAppSeenFactor × the check-in interval is "present", as for client auto-update.
	roleAppSeenFactor = 3
)

// appStep is what one device needs next.
type appStep struct {
	Install   *db.App  // install this build (nil = nothing to install)
	Uninstall []string // remove these packages of the family (only once Install is nil)
	Ahead     bool     // the device has a newer build of the target than the channel
}

// planAppStep works out the next step for one device. base is the family's base package,
// installed is every package of the family on the device with its version, and target is
// the build the device should run (nil = leave it alone).
func planAppStep(base string, installed map[string]string, target *db.App) appStep {
	if target == nil || target.PackageName == "" {
		return appStep{}
	}
	have, ok := installed[target.PackageName]
	if !ok || (have != target.VersionName && target.VersionName != "") {
		if ok && compareVersionNames(have, target.VersionName) > 0 {
			return appStep{Ahead: true, Uninstall: others(base, installed, target.PackageName)}
		}
		return appStep{Install: target}
	}
	return appStep{Uninstall: others(base, installed, target.PackageName)}
}

// others lists the family's packages on the device other than keep, sorted.
func others(base string, installed map[string]string, keep string) []string {
	var out []string
	for p := range installed {
		if p != keep && inFamily(base, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// inFamily reports whether pkg is the base package or one of its variants.
func inFamily(base, pkg string) bool {
	return pkg == base || strings.HasPrefix(pkg, base+".")
}

// compareVersionNames compares two versionName strings by their numeric dotted parts
// ("4.10.0" > "4.9.2"); a part that is not a number compares as text. -1, 0 or 1.
func compareVersionNames(a, b string) int {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x == y {
			continue
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		switch {
		case ex == nil && ey == nil:
			if nx < ny {
				return -1
			}
			return 1
		case x == "":
			// "4.3.0" vs "4.3.0-rc2": a trailing word is a pre-release, which comes
			// before the plain version; a trailing number ("4.3" vs "4.3.1") comes after.
			if ey != nil {
				return 1
			}
			return -1
		case y == "":
			if ex != nil {
				return -1
			}
			return 1
		case x < y:
			return -1
		default:
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == ' ' })
}

// roleAppState is everything the job and the Releases page read about role apps, loaded
// once per tick or request.
type roleAppState struct {
	Apps     map[string]string             // role → base package
	Channels map[string]map[string]*db.App // role → package → build (nil = cleared)
	Labs     map[uuid.UUID]db.LabSession
}

// loadRoleAppState returns the lab sessions only. Roles no longer carry an app (decided
// 14 Oct 2026: Lineup is firmware, MDM clients and hardware; apps are installed by hand),
// so Apps and Channels stay empty and everything keyed on them (the role app job, the
// role lock, the role app alerts) does nothing. The role_apps and app_channels tables
// are left in place, unread.
func (h *Handler) loadRoleAppState(ctx context.Context) (*roleAppState, error) {
	st := &roleAppState{Apps: map[string]string{}, Channels: map[string]map[string]*db.App{}}
	var err error
	if st.Labs, err = h.db.LabSessions(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

// targetFor is the build a device should run, or nil to leave its app alone.
func (st *roleAppState) targetFor(d *db.Device) *db.App {
	role := d.Class()
	base := st.Apps[role]
	if base == "" {
		return nil
	}
	if s, inLab := st.Labs[d.ID]; inLab {
		if s.Package == "" || !inFamily(base, s.Package) {
			return nil
		}
		return st.Channels[role][s.Package]
	}
	if d.RestaurantID == nil {
		return nil
	}
	return st.Channels[role][base]
}

// RunRoleApps moves deployed devices onto their role's prod build and lab devices onto
// the package their session chose. Safe to call every minute.
func (h *Handler) RunRoleApps(ctx context.Context) {
	pol := h.cfg.RoleRollout(config.RolloutApps)
	st, err := h.loadRoleAppState(ctx)
	if err != nil {
		log.Printf("[role-apps] load: %v", err)
		return
	}
	if len(st.Apps) == 0 {
		return
	}
	bases := make([]string, 0, len(st.Apps))
	for _, b := range st.Apps {
		bases = append(bases, b)
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		log.Printf("[role-apps] ListDevices: %v", err)
		return
	}
	installed, err := h.db.FamilyVersions(ctx, bases)
	if err != nil {
		log.Printf("[role-apps] inventory: %v", err)
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second

	type job struct {
		d    *db.Device
		step appStep
	}
	var jobs []job
	var ids []uuid.UUID
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() || time.Since(d.LastSeenAt) > seenWithin {
			continue
		}
		target := st.targetFor(d)
		if target == nil {
			continue
		}
		_, inLab := st.Labs[d.ID]
		// Lab moves happen when someone asks for them; prod waits for the quiet window
		// and for the policy to be on.
		if !inLab && (!pol.Active() || !pol.Allows(deviceNow(d))) {
			continue
		}
		step := planAppStep(st.Apps[d.Class()], installed[d.ID], target)
		if step.Install == nil && len(step.Uninstall) == 0 {
			continue
		}
		if step.Install != nil && !d.Supports("install_apk") {
			continue
		}
		if step.Install == nil && !d.Supports("uninstall") {
			continue
		}
		jobs = append(jobs, job{d, step})
		ids = append(ids, d.ID)
	}
	if len(jobs) == 0 {
		return
	}
	busy, err := h.db.InFlightInstalls(ctx, ids)
	if err != nil {
		log.Printf("[role-apps] in-flight: %v", err)
		return
	}
	sent := 0
	for _, j := range jobs {
		if sent >= roleAppBatch {
			break
		}
		if busy[j.d.ID] {
			continue
		}
		if j.step.Install != nil {
			if err := h.queueAppInstall(ctx, j.d, j.step.Install, "role-apps"); err != nil {
				log.Printf("[role-apps] install %s on %s: %v", j.step.Install.PackageName, j.d.SerialNumber, err)
				continue
			}
		} else {
			// One package per tick: the next one goes once this one is done.
			if err := h.queueUninstall(ctx, j.d, j.step.Uninstall[0], "role-apps"); err != nil {
				log.Printf("[role-apps] uninstall %s on %s: %v", j.step.Uninstall[0], j.d.SerialNumber, err)
				continue
			}
		}
		sent++
	}
	if sent > 0 {
		log.Printf("[role-apps] queued %d command(s)", sent)
	}
}

// queueAppInstall queues an install of a library build on one device and pushes it if
// the device is connected.
func (h *Handler) queueAppInstall(ctx context.Context, d *db.Device, app *db.App, by string) error {
	raw := apkmeta.Augment(ctx, app.ApkURL, json.RawMessage("{}"))
	cmd, err := h.db.CreateCommandBy(ctx, "install_apk", app.ApkURL, raw, "devices", []uuid.UUID{d.ID}, by)
	if err != nil {
		return err
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{d.ID})
	return nil
}

// queueUninstall queues the removal of one package on one device.
func (h *Handler) queueUninstall(ctx context.Context, d *db.Device, pkg, by string) error {
	payload, _ := json.Marshal(map[string]string{"package": pkg})
	cmd, err := h.db.CreateCommandBy(ctx, "uninstall", "", payload, "devices", []uuid.UUID{d.ID}, by)
	if err != nil {
		return err
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{d.ID})
	return nil
}

// modelKey is the hardware model a device's firmware setting is looked up by.
func modelKey(d *db.Device) string {
	if k := prod.Normalize(d.Product); k != "" {
		return k
	}
	return prod.DefaultKey
}
