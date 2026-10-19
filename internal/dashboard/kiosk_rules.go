package dashboard

import (
	"context"
	"log"
	"sort"
	"sync"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Lock rules: the Policies page's kiosk policies, enforced continuously. A device
// follows the first rule (in order) whose target covers it — a restaurant, a group, a
// device, or every device. The enforcer runs every minute and after any rule change,
// so a device that joins a restaurant is locked and one that leaves is released. It
// never touches a device someone overrode on its device page, and it only locks to an
// app the device has installed: lock-task on a missing app fails silently.

// A rule names no app (19 Oct 2026): it locks each device to its role's app on Lineup,
// the build of it the device has (lockPackageFor). A rule saved before then still names
// its package and locks to exactly that.

// roleAppWords is how a rule that names no app says what it locks to.
const roleAppWords = "its role's app"

// kioskDevice is one device's kiosk position under the rules.
type kioskDevice struct {
	Dev      db.Device
	Rule     *db.KioskPolicy // the rule it follows, nil when none covers it
	Covering int             // how many rules cover it
	State    db.KioskState
	Package  string // what the rule locks this device to ("" = nothing it can lock to)
	HasApp   bool   // that app is installed
	CanLock  bool   // its agent can lock the screen
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

// Role lock (static/device-role-model-plan.html): a device at a venue is locked to its
// role's app, ahead of any rule on the Lock page, once that role has a prod build. Each
// role gets a virtual rule with a stable id, so the enforcer below does the work it
// already does for rules — wait for the app, skip agents that can't lock, leave by-hand
// overrides and staff exits alone, release a device that stops being covered. A device
// in a lab session is covered by no rule at all, which is what releases it.

// roleRuleNS namespaces the virtual rule ids.
var roleRuleNS = uuid.MustParse("6f1e5d1c-3b0a-4c55-9a43-2f7d1c0b9e61")

// roleRuleID is the stable id of a role's virtual lock rule.
func roleRuleID(role string) uuid.UUID { return uuid.NewSHA1(roleRuleNS, []byte("role-lock:"+role)) }

// roleLockRules returns one virtual rule per role that has an app with a prod build.
func roleLockRules(st *roleAppState) map[string]db.KioskPolicy {
	out := map[string]db.KioskPolicy{}
	for role, base := range st.Apps {
		if st.Channels[role][base] == nil {
			continue
		}
		out[role] = db.KioskPolicy{ID: roleRuleID(role), Name: "Role: " + prod.ClassLabel(role), KioskPackage: base, TargetType: "role", Priority: -1}
	}
	return out
}

// IsRoleRule reports whether a device's recorded rule is a role's virtual rule.
func IsRoleRule(id *uuid.UUID, role string) bool { return id != nil && *id == roleRuleID(role) }

// kioskSnapshot places every device in the fleet under the rules.
func (h *Handler) kioskSnapshot(ctx context.Context) ([]db.KioskPolicy, []kioskDevice, error) {
	rules, err := h.db.ListKioskPolicies(ctx)
	if err != nil {
		return nil, nil, err
	}
	roleSt, err := h.loadRoleAppState(ctx)
	if err != nil {
		return nil, nil, err
	}
	roleRules := roleLockRules(roleSt)
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
	for _, r := range roleRules {
		pkgs = append(pkgs, r.KioskPackage)
	}
	installed, err := h.db.InstalledPackages(ctx, pkgs)
	if err != nil {
		return nil, nil, err
	}
	bases := h.db.RoleAppBases(ctx)
	var famList []string
	for _, b := range bases {
		if b != "" {
			famList = append(famList, b)
		}
	}
	fam, err := h.db.FamilyVersions(ctx, famList)
	if err != nil {
		return nil, nil, err
	}
	// place fills in what a device locks to under its rule.
	place := func(k *kioskDevice) {
		if k.Rule.KioskPackage != "" {
			k.Package = k.Rule.KioskPackage
			k.HasApp = installed[k.Dev.ID][k.Package]
			return
		}
		k.Package = lockPackageFor(k.Dev, bases[k.Dev.Class()], fam[k.Dev.ID], k.State)
		k.HasApp = k.Package != ""
	}
	online := h.hub.ConnectedIDsForDisplay()
	out := make([]kioskDevice, 0, len(devs))
	for _, d := range devs {
		k := kioskDevice{Dev: d, State: states[d.ID], CanLock: d.Supports("kiosk_set")}
		_, k.Online = online[d.ID]
		if _, inLab := roleSt.Labs[d.ID]; inLab {
			out = append(out, k) // in lab: no rule covers it, so a rule's lock is released
			continue
		}
		if rr, ok := roleRules[d.Class()]; ok && d.RestaurantID != nil {
			rr := rr
			k.Rule, k.Covering = &rr, 1
			place(&k)
			out = append(out, k)
			continue
		}
		for i := range rules {
			if ruleCovers(rules[i], d, scopes[d.ID]) {
				k.Covering++
				if k.Rule == nil {
					k.Rule = &rules[i]
				}
			}
		}
		if k.Rule != nil {
			place(&k)
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
	in, err := h.db.KioskRulesInUse(ctx)
	if err != nil {
		return 0
	}
	if !in {
		// Role locks count as rules in use.
		if st, err := h.loadRoleAppState(ctx); err != nil || len(roleLockRules(st)) == 0 {
			return 0
		}
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
			continue // unlocked on the device: stays out until someone answers it
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
			if !st.Enabled || st.Package != k.Package || st.Rule == nil || *st.Rule != want.ID || (st.Mode != "" && st.Mode != "app") {
				if err := h.db.SetKioskByRule(ctx, k.Dev.ID, true, k.Package, &want.ID); err == nil {
					changed = append(changed, k.Dev.ID)
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

// lockPackageFor is the build of its role's app (base) a rule locks a device to: the one
// it is already locked to, if that is still installed, so the choice never flips back
// and forth; else the build on screen; else prod; else the first by name. "" when the
// role has no app or none of it is installed.
func lockPackageFor(d db.Device, base string, installed map[string]string, st db.KioskState) string {
	if base == "" || len(installed) == 0 {
		return ""
	}
	if st.Enabled && inFamily(base, st.Package) {
		if _, ok := installed[st.Package]; ok {
			return st.Package
		}
	}
	screen := screenPackages(d.LatestExtra)
	var pkgs []string
	for p := range installed {
		if inFamily(base, p) {
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) == 0 {
		return ""
	}
	sort.Slice(pkgs, func(i, j int) bool {
		a, b := pkgs[i], pkgs[j]
		if screen[a] != screen[b] {
			return screen[a]
		}
		if (a == base) != (b == base) {
			return a == base
		}
		return a < b
	})
	return pkgs[0]
}

// RunLockRules is the minute tick for the enforcer above. It was documented as running
// every minute but was only ever called after a rule change, so a device that joined a
// restaurant was not locked until someone next saved a rule. Role locks depend on it.
func (h *Handler) RunLockRules(ctx context.Context) { h.reconcileKiosk(ctx) }

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
