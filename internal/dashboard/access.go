package dashboard

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// Actions an operator's policy can grant or withhold. Admin/dev-only command
// types (shell, ota, update_splash, logcat) are not here: the role allowlist in
// commandRoles keeps them out of operators' reach regardless of policy.
var accessActions = []struct{ Key, Label, Group string }{
	{"view", "See device", "Visibility"},
	{"screenshot", "Screenshot", "Commands"},
	{"install_apk", "Install app", "Commands"},
	{"uninstall", "Uninstall app", "Commands"},
	{"reboot", "Reboot", "Commands"},
	{"query", "Run query", "Commands"},
	{"kiosk", "Kiosk settings", "Device"},
	{"notes", "Device notes", "Device"},
	{"queue", "Queue: cancel / clear", "Device"},
	{"alerts", "Acknowledge / resolve alerts", "Fleet"},
	{"groups", "Manage groups & venues", "Fleet"},
	{"qa", "Record QA results", "Fleet"},
	{"deploy", "Cancel deployments", "Fleet"},
}

// deviceActions are evaluated against a device's scope; the rest are fleet-level.
var deviceActions = map[string]bool{"view": true, "screenshot": true, "install_apk": true, "uninstall": true, "reboot": true, "query": true, "kiosk": true, "notes": true, "queue": true}

func isAccessAction(k string) bool {
	for _, a := range accessActions {
		if a.Key == k {
			return true
		}
	}
	return false
}

// access is one request's resolved authorization context.
type access struct {
	h        *Handler
	ctx      context.Context
	role     string
	username string
	pol      db.AccessPolicy
	scopes   map[uuid.UUID]db.DeviceScope
	scopeErr error
	once     sync.Once
}

// policyCache avoids a users lookup on every request; entries live 20s so a
// saved policy applies almost immediately.
var policyCache sync.Map // username -> policyEntry

type policyEntry struct {
	pol db.AccessPolicy
	at  time.Time
}

func invalidatePolicy(username string) { policyCache.Delete(username) }

// access resolves the caller's role and policy. Cheap; the device scope map is
// loaded lazily on the first device-level check.
func (h *Handler) access(r *http.Request) *access {
	a := &access{h: h, ctx: r.Context(), role: h.role(r), username: h.currentUsername(r)}
	if a.role == "admin" || a.role == "dev" || a.username == "" {
		return a
	}
	if e, ok := policyCache.Load(a.username); ok && time.Since(e.(policyEntry).at) < 20*time.Second {
		a.pol = e.(policyEntry).pol
		return a
	}
	pol, err := h.db.GetUserAccess(a.ctx, a.username)
	if err != nil {
		log.Printf("[access] policy for %q: %v", a.username, err)
	}
	policyCache.Store(a.username, policyEntry{pol: pol, at: time.Now()})
	a.pol = pol
	return a
}

func (a *access) unrestricted() bool { return a.role == "admin" || a.role == "dev" }

func (a *access) loadScopes() {
	a.once.Do(func() { a.scopes, a.scopeErr = a.h.db.DeviceScopes(a.ctx) })
}

func (a *access) ruleCovers(rule db.AccessRule, action string, dev *uuid.UUID) bool {
	hit := false
	for _, x := range rule.Actions {
		if x == "*" || x == action {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	switch rule.ScopeType {
	case "", "all":
		return true
	case "group", "restaurant":
		if dev == nil {
			return false // fleet-level action: only fleet-wide rules apply
		}
		a.loadScopes()
		sc := a.scopes[*dev]
		id, err := uuid.Parse(rule.ScopeID)
		if err != nil {
			return false
		}
		if rule.ScopeType == "restaurant" {
			return sc.RestaurantID != nil && *sc.RestaurantID == id
		}
		for _, g := range sc.Groups {
			if g == id {
				return true
			}
		}
	}
	return false
}

// can decides one action, optionally for one device. Role gates (viewers can't
// act, operators can't shell) are applied by the existing wrappers/allowlist;
// this only refines what the role already permits.
func (a *access) can(action string, dev *uuid.UUID) bool {
	if a.unrestricted() {
		return true
	}
	if a.role == "viewer" && action != "view" && action != "screenshot" {
		return false
	}
	deny, allow := false, false
	for _, rule := range a.pol.Rules {
		if !a.ruleCovers(rule, action, dev) {
			continue
		}
		if rule.Effect == "deny" {
			deny = true
		} else {
			allow = true
		}
	}
	if deny {
		return false
	}
	if allow {
		return true
	}
	if action == "view" && a.role == "viewer" {
		return true // a viewer's base is always "see everything" unless denied
	}
	return a.pol.Base != "deny"
}

func (a *access) canDevice(action string, dev uuid.UUID) bool { return a.can(action, &dev) }

// filterDevices keeps the ids the user may perform action on. Returns kept and
// the number dropped.
func (a *access) filterDevices(action string, ids []uuid.UUID) ([]uuid.UUID, int) {
	if a.unrestricted() {
		return ids, 0
	}
	kept := ids[:0:0]
	for _, id := range ids {
		if a.canDevice(action, id) {
			kept = append(kept, id)
		}
	}
	return kept, len(ids) - len(kept)
}

// hidesDevices reports whether devices this user cannot view are removed from
// lists (vs shown with actions disabled). Viewers always hide: there is nothing
// to disable for them.
func (a *access) hidesDevices() bool {
	if a.unrestricted() {
		return false
	}
	if a.role == "viewer" {
		return a.hasViewRestriction()
	}
	return a.pol.HideOutOfScope && a.hasViewRestriction()
}

func (a *access) hasViewRestriction() bool {
	if a.pol.Base == "deny" {
		return true
	}
	for _, r := range a.pol.Rules {
		if r.Effect == "deny" {
			for _, x := range r.Actions {
				if x == "*" || x == "view" {
					return true
				}
			}
		}
	}
	return false
}

// visibleIDs is the device-id allowlist for list queries, or nil for "no filter".
func (a *access) visibleIDs() []uuid.UUID {
	if !a.hidesDevices() {
		return nil
	}
	a.loadScopes()
	ids := make([]uuid.UUID, 0, len(a.scopes))
	for id := range a.scopes {
		if a.canDevice("view", id) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// deviceActionsAllowed reports whether the user may take any action on a device
// (used to render the device page read-only when they may take none).
func (a *access) anyDeviceAction(dev uuid.UUID) bool {
	if a.unrestricted() {
		return true
	}
	for k := range deviceActions {
		if k != "view" && a.canDevice(k, dev) {
			return true
		}
	}
	return false
}

// requireDeviceAction is the common guard for single-device operator handlers.
// Writes a 403 and returns false when the policy withholds the action.
func (h *Handler) requireDeviceAction(w http.ResponseWriter, r *http.Request, action string, dev uuid.UUID) bool {
	if h.access(r).canDevice(action, dev) {
		return true
	}
	http.Error(w, "Your access policy does not allow this action on this device.", http.StatusForbidden)
	return false
}

// enforceCommandTargets applies the policy to a command's targets. For device
// targets it drops what the user may not touch (403 if nothing is left); for a
// group target it refuses when the group contains any device outside the policy,
// since a group command is stored as one unit and can't be partially sent.
// Returns the ids to use and false when it already wrote a response.
func (h *Handler) enforceCommandTargets(w http.ResponseWriter, r *http.Request, action, targetType string, ids []uuid.UUID) ([]uuid.UUID, bool) {
	acc := h.access(r)
	if acc.unrestricted() {
		return ids, true
	}
	if targetType == "groups" {
		devIDs, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), ids)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return nil, false
		}
		if _, dropped := acc.filterDevices(action, devIDs); dropped > 0 {
			http.Error(w, "This group contains devices your access policy does not allow this command on. Pick devices individually from the Actions page.", http.StatusForbidden)
			return nil, false
		}
		return ids, true
	}
	kept, dropped := acc.filterDevices(action, ids)
	if len(kept) == 0 && len(ids) > 0 {
		http.Error(w, "Your access policy does not allow this command on the selected devices.", http.StatusForbidden)
		return nil, false
	}
	if dropped > 0 {
		log.Printf("[access] %s: %s skipped %d device(s) outside their policy", acc.username, action, dropped)
	}
	return kept, true
}

// requireFleetAction guards fleet-level operator actions (QA, groups, alerts…).
func (h *Handler) requireFleetAction(w http.ResponseWriter, r *http.Request, action string) bool {
	if h.access(r).can(action, nil) {
		return true
	}
	http.Error(w, "Your access policy does not allow this action.", http.StatusForbidden)
	return false
}

// ── Admin: edit a user's policy ─────────────────────────────────────────────

// UserSetAccess parses the Access editor form (see profile.html) into a policy.
// Rows are indexed: rule_effect_N, rule_scope_N ("all" | "group:<id>" |
// "restaurant:<id>"), rule_actions_N (repeated checkbox values, "*" = any).
func (h *Handler) UserSetAccess(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	pol := db.AccessPolicy{Base: "allow"}
	if r.FormValue("base") == "deny" {
		pol.Base = "deny"
	}
	pol.HideOutOfScope = r.FormValue("hide_out_of_scope") == "1"
	for i := 0; i < 50; i++ {
		sfx := "_" + itoa(i)
		effect := r.FormValue("rule_effect" + sfx)
		if effect == "" {
			continue
		}
		acts := r.Form["rule_actions"+sfx]
		if len(acts) == 0 {
			continue // an empty row is ignored
		}
		var clean []string
		for _, a := range acts {
			if a == "*" || isAccessAction(a) {
				clean = append(clean, a)
			}
		}
		if len(clean) == 0 {
			continue
		}
		rule := db.AccessRule{Effect: "allow", Actions: clean, ScopeType: "all"}
		if effect == "deny" {
			rule.Effect = "deny"
		}
		if sc := r.FormValue("rule_scope" + sfx); strings.Contains(sc, ":") {
			parts := strings.SplitN(sc, ":", 2)
			if (parts[0] == "group" || parts[0] == "restaurant") && parts[1] != "" {
				if _, err := uuid.Parse(parts[1]); err == nil {
					rule.ScopeType, rule.ScopeID = parts[0], parts[1]
				}
			}
		}
		pol.Rules = append(pol.Rules, rule)
	}
	if err := h.db.SetUserAccess(r.Context(), id, pol); err != nil {
		log.Printf("[access] save for %s: %v", u.Username, err)
		http.Error(w, "Could not save access policy", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(u.Username)
	h.audit(r, "user.access", u.Username, describePolicy(pol))
	h.hxRedirect(w, r, "/users/"+id.String()+"/profile")
}

func describePolicy(p db.AccessPolicy) string {
	if p.IsEmpty() {
		return "full operator access"
	}
	parts := []string{"base=" + p.Base}
	if p.HideOutOfScope {
		parts = append(parts, "hide out-of-scope")
	}
	for _, r := range p.Rules {
		sc := r.ScopeType
		if r.ScopeID != "" {
			sc += ":" + r.ScopeID
		}
		parts = append(parts, r.Effect+" "+strings.Join(r.Actions, ",")+" @"+sc)
	}
	return strings.Join(parts, "; ")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
