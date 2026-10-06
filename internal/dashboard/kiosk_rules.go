package dashboard

import (
	"context"
	"log"
	"sync"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Kiosk rules: the Policies page's kiosk policies, enforced continuously. A device
// follows the first rule (in order) whose target covers it — a restaurant, a group, a
// device, or every device. The enforcer runs every minute and after any rule change,
// so a device that joins a restaurant is locked and one that leaves is released. It
// never touches a device someone overrode on its device page, and it only locks to an
// app the device has installed: lock-task on a missing app fails silently.

// kioskDevice is one device's kiosk position under the rules.
type kioskDevice struct {
	Dev      db.Device
	Rule     *db.KioskPolicy // the rule it follows, nil when none covers it
	Covering int             // how many rules cover it
	State    db.KioskState
	HasApp   bool // the rule's app is installed
	CanLock  bool // its agent can lock the screen
	Online   bool
}

// Status is where the device stands against its rule: locked, waiting (not picked up
// yet), exited (staff used an exit code), missing (app not installed), cant (agent
// can't lock), override (changed by hand), or "" with no rule.
func (k kioskDevice) Status() string {
	switch {
	case k.Rule == nil:
		return ""
	case k.State.ExitedAt != nil:
		return "exited"
	case k.State.Override:
		return "override"
	case !k.CanLock:
		return "cant"
	case !k.HasApp:
		return "missing"
	case !k.State.Enabled || k.State.Rule == nil || *k.State.Rule != k.Rule.ID:
		return "waiting"
	case k.State.Suspended:
		return "exited"
	case k.Online || (k.State.ConfigAt != nil && k.State.LastSeen.After(*k.State.ConfigAt)):
		return "locked"
	}
	return "waiting"
}

// ruleCovers reports whether a rule's target includes the device. The restaurant and
// groups come from the scope map (DeviceScopes): device lists don't carry the
// restaurant id.
func ruleCovers(r db.KioskPolicy, d db.Device, sc db.DeviceScope) bool {
	switch r.TargetType {
	case "all":
		return true
	case "restaurant":
		return r.TargetID != nil && sc.RestaurantID != nil && *sc.RestaurantID == *r.TargetID
	case "group":
		if r.TargetID == nil {
			return false
		}
		for _, g := range sc.Groups {
			if g == *r.TargetID {
				return true
			}
		}
	case "device":
		return r.TargetSerial != "" && r.TargetSerial == d.SerialNumber
	}
	return false
}

// kioskSnapshot places every device in the fleet under the rules.
func (h *Handler) kioskSnapshot(ctx context.Context) ([]db.KioskPolicy, []kioskDevice, error) {
	rules, err := h.db.ListKioskPolicies(ctx)
	if err != nil {
		return nil, nil, err
	}
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "", "")
	if err != nil {
		return nil, nil, err
	}
	scopes, err := h.db.DeviceScopes(ctx)
	if err != nil {
		return nil, nil, err
	}
	states, err := h.db.ListKioskStates(ctx)
	if err != nil {
		return nil, nil, err
	}
	var pkgs []string
	for _, r := range rules {
		pkgs = append(pkgs, r.KioskPackage)
	}
	installed, err := h.db.InstalledPackages(ctx, pkgs)
	if err != nil {
		return nil, nil, err
	}
	online := h.hub.ConnectedIDsForDisplay()
	out := make([]kioskDevice, 0, len(devs))
	for _, d := range devs {
		k := kioskDevice{Dev: d, State: states[d.ID], CanLock: d.Supports("kiosk_set")}
		_, k.Online = online[d.ID]
		for i := range rules {
			if ruleCovers(rules[i], d, scopes[d.ID]) {
				k.Covering++
				if k.Rule == nil {
					k.Rule = &rules[i]
				}
			}
		}
		if k.Rule != nil {
			k.HasApp = installed[d.ID][k.Rule.KioskPackage]
		}
		out = append(out, k)
	}
	return rules, out, nil
}

// kioskReconcileMu keeps two passes (the minute ticker and a rule save) from
// interleaving their writes.
var kioskReconcileMu sync.Mutex

// reconcileKiosk brings every device's kiosk in line with the rules and pushes the
// changes. Returns how many devices changed.
func (h *Handler) reconcileKiosk(ctx context.Context) int {
	kioskReconcileMu.Lock()
	defer kioskReconcileMu.Unlock()
	if in, err := h.db.KioskRulesInUse(ctx); err != nil || !in {
		return 0
	}
	_, devs, err := h.kioskSnapshot(ctx)
	if err != nil {
		log.Printf("[kiosk-rules] snapshot: %v", err)
		return 0
	}
	var changed []uuid.UUID
	for _, k := range devs {
		st := k.State
		if st.Override {
			continue // someone decided this device by hand
		}
		if st.ExitedAt != nil {
			continue // taken out of kiosk on site: stays out until someone answers it
		}
		switch {
		case k.Rule == nil:
			// No rule covers it any more. Release it only if a rule had locked it; a
			// device locked by hand stays as it is.
			if st.Rule != nil {
				if err := h.db.SetKioskByRule(ctx, k.Dev.ID, false, "", nil); err == nil {
					changed = append(changed, k.Dev.ID)
				}
			}
		case !k.CanLock:
			// Nothing to do: the agent can't lock. The page says so.
		case !k.HasApp:
			// Don't lock to an app that isn't there. If an earlier rule had locked it
			// to something else, release it; it locks once the app is installed.
			if st.Rule != nil && st.Enabled {
				if err := h.db.SetKioskByRule(ctx, k.Dev.ID, false, "", &k.Rule.ID); err == nil {
					changed = append(changed, k.Dev.ID)
				}
			}
		default:
			want := k.Rule
			if !st.Enabled || st.Package != want.KioskPackage || st.Rule == nil || *st.Rule != want.ID || (st.Mode != "" && st.Mode != "app") {
				if err := h.db.SetKioskByRule(ctx, k.Dev.ID, true, want.KioskPackage, &want.ID); err == nil {
					changed = append(changed, k.Dev.ID)
				}
			}
			if want.OfflineExit && !st.OfflineExit {
				if _, err := h.db.SetOfflineExit(ctx, k.Dev.ID, true, "", false); err != nil {
					log.Printf("[kiosk-rules] offline exit on %s: %v", k.Dev.SerialNumber, err)
				}
			}
		}
	}
	if len(changed) > 0 {
		h.pushKioskConfigToDevices(ctx, changed)
		log.Printf("[kiosk-rules] updated %d device(s)", len(changed))
	}
	return len(changed)
}

// kioskRuleFor returns the rule that covers one device, if any (for the device page).
func (h *Handler) kioskRuleFor(ctx context.Context, dev db.Device) *db.KioskPolicy {
	rules, err := h.db.ListKioskPolicies(ctx)
	if err != nil || len(rules) == 0 {
		return nil
	}
	scopes, _ := h.db.DeviceScopes(ctx)
	for i := range rules {
		if ruleCovers(rules[i], dev, scopes[dev.ID]) {
			return &rules[i]
		}
	}
	return nil
}

// markKioskManual records a by-hand kiosk change on several devices (bulk kiosk from
// the Actions page or the Devices list): those a rule covers become exceptions to it.
func (h *Handler) markKioskManual(ctx context.Context, ids []uuid.UUID) {
	in := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		in[id] = true
	}
	_, devs, err := h.kioskSnapshot(ctx)
	if err != nil {
		return
	}
	for _, k := range devs {
		if in[k.Dev.ID] {
			_ = h.db.MarkKioskManual(ctx, k.Dev.ID, k.Rule != nil)
		}
	}
}
