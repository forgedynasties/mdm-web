package dashboard

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// ── Action catalogue ────────────────────────────────────────────────────────
//
// Roles are ceilings: what can ever be granted. Grants (access_grants) say where.
// See docs/access-control-plan.md for the model and docs/AUTH_MODEL.md for the
// roles.

type accessAction struct {
	Key, Label, Group, Help string
	Fleet                   bool // fleet-level: evaluated against "all" grants only
	Sensitive               bool // needs an explicit allow; "*" never covers it
	DevOnly                 bool // in the dev ceiling only, never grantable to operators
}

var accessActions = []accessAction{
	{Key: "view", Label: "See device", Group: "Visibility", Help: "The device shows up in lists, the map, alerts and history."},
	{Key: "screenshot", Label: "Screenshot", Group: "Commands"},
	{Key: "install_apk", Label: "Install app", Group: "Commands"},
	{Key: "uninstall", Label: "Uninstall app", Group: "Commands"},
	{Key: "reboot", Label: "Reboot", Group: "Commands"},
	{Key: "query", Label: "Run diagnostic query", Group: "Commands"},
	{Key: "logcat", Label: "Request logcat", Group: "Commands"},
	{Key: "kiosk", Label: "Kiosk settings", Group: "Device"},
	{Key: "notes", Label: "Device notes", Group: "Device"},
	{Key: "queue", Label: "Cancel / clear queue", Group: "Device"},
	{Key: "remote", Label: "Remote control", Group: "Sessions", Sensitive: true, Help: "Live screen and touch. Must be named explicitly; \"any action\" never includes it."},
	{Key: "shell", Label: "Shell", Group: "Sessions", Sensitive: true, DevOnly: true, Help: "Raw shell on the device. Dev accounts only."},
	{Key: "ota", Label: "OTA updates", Group: "Updates", Sensitive: true, DevOnly: true, Help: "May target these devices in a firmware deployment. Dev accounts only."},
	{Key: "alerts", Label: "Acknowledge / resolve alerts", Group: "Fleet", Fleet: true},
	{Key: "groups", Label: "Manage groups & venues", Group: "Fleet", Fleet: true},
	{Key: "deploy", Label: "Cancel deployments", Group: "Fleet", Fleet: true},
}

var accessActionByKey = func() map[string]accessAction {
	m := map[string]accessAction{}
	for _, a := range accessActions {
		m[a.Key] = a
	}
	return m
}()

// deviceActions are evaluated against a device's scope; the rest are fleet-level.
var deviceActions = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range accessActions {
		if !a.Fleet {
			m[a.Key] = true
		}
	}
	return m
}()

func isAccessAction(k string) bool { _, ok := accessActionByKey[k]; return ok }

// sensitiveActionKeys are the actions the overview flags with an amber dot.
var sensitiveActionKeys = []string{"remote", "shell", "ota"}

// roleCeiling lists what a role can ever do; nil means unrestricted (admin).
func roleCeiling(role string) map[string]bool {
	switch role {
	case "admin":
		return nil
	case "viewer":
		return map[string]bool{"view": true, "screenshot": true}
	case "owner":
		return map[string]bool{"view": true}
	}
	m := map[string]bool{}
	for _, a := range accessActions {
		// DevOnly actions belong to the dev ceiling; the OTA admin gets exactly one of
		// them, the firmware push.
		if a.DevOnly && role != "dev" && !(role == "ota_admin" && a.Key == "ota") {
			continue
		}
		m[a.Key] = true
	}
	if role == "operator" || role == "user_manager" || role == "ota_admin" || role == "dev" {
		return m
	}
	return map[string]bool{} // unknown role: nothing
}

// grantableActions is the catalogue filtered to a role's ceiling, for the editor.
func grantableActions(role string) []accessAction {
	c := roleCeiling(role)
	var out []accessAction
	for _, a := range accessActions {
		if c == nil || c[a.Key] {
			out = append(out, a)
		}
	}
	return out
}

// ── Per-request access context ──────────────────────────────────────────────

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
	visOnce  sync.Once
	vis      []uuid.UUID
}

// policyCache avoids a users lookup on every request; entries live 20s so a
// saved grant applies almost immediately (saves also invalidate directly).
var policyCache sync.Map // username -> policyEntry

type policyEntry struct {
	pol db.AccessPolicy
	at  time.Time
}

func invalidatePolicy(username string) { policyCache.Delete(username) }

// access resolves the caller's role and policy. Cheap; the device scope map is
// loaded lazily on the first device-level check.
func (h *Handler) access(r *http.Request) *access {
	return h.accessFor(r.Context(), h.role(r), h.currentUsername(r))
}

// accessFor builds the context for any user (used by the editor's preview and
// by delegation checks, which evaluate the granter's own access).
func (h *Handler) accessFor(ctx context.Context, role, username string) *access {
	a := &access{h: h, ctx: ctx, role: role, username: username}
	if a.role == "admin" || a.username == "" {
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

// unrestricted: nothing is ever filtered for this user. Only super admin.
func (a *access) unrestricted() bool { return a.role == "admin" }

func (a *access) loadScopes() {
	a.once.Do(func() { a.scopes, a.scopeErr = a.h.db.DeviceScopes(a.ctx) })
}

// scopeContains reports whether a grant's scope covers the device (nil device =
// fleet-level question, which only "all" answers).
func (a *access) scopeContains(g db.AccessGrant, dev *uuid.UUID) bool {
	switch g.ScopeType {
	case "", "all":
		return true
	case "device":
		return dev != nil && g.ScopeID != nil && *g.ScopeID == *dev
	case "restaurant", "group":
		if dev == nil || g.ScopeID == nil {
			return false
		}
		a.loadScopes()
		sc, ok := a.scopes[*dev]
		if !ok {
			return false
		}
		if g.ScopeType == "restaurant" {
			return sc.RestaurantID != nil && *sc.RestaurantID == *g.ScopeID
		}
		for _, gid := range sc.Groups {
			if gid == *g.ScopeID {
				return true
			}
		}
	}
	return false
}

// grantCovers: the grant names the action (a "*" never reaches a sensitive one)
// and its scope contains the device.
func (a *access) grantCovers(g db.AccessGrant, action string, dev *uuid.UUID) bool {
	named := false
	for _, x := range g.Actions {
		// "*" never hands out a sensitive action, but a deny of "*" takes it away too.
		if x == action || (x == "*" && (g.Effect == "deny" || !accessActionByKey[action].Sensitive)) {
			named = true
			break
		}
	}
	return named && a.scopeContains(g, dev)
}

// decision explains one evaluation, for the editor preview and the audit line.
type decision struct {
	Allowed bool
	Reason  string          // short sentence
	Grant   *db.AccessGrant // the grant that decided it, when one did
}

// decide is the single evaluation path (see docs/access-control-plan.md §2).
func (a *access) decide(action string, dev *uuid.UUID) decision {
	if a.role == "admin" {
		return decision{true, "Super admin", nil}
	}
	act, known := accessActionByKey[action]
	if !known {
		return decision{false, "Unknown action", nil}
	}
	if c := roleCeiling(a.role); !c[action] {
		return decision{false, roleLabel(a.role) + " accounts never get " + lowerFirst(act.Label), nil}
	}
	var deny, allow *db.AccessGrant
	for i := range a.pol.Grants {
		g := &a.pol.Grants[i]
		if !a.grantCovers(*g, action, dev) {
			continue
		}
		if g.Effect == "deny" {
			if deny == nil {
				deny = g
			}
		} else if allow == nil {
			allow = g
		}
	}
	if deny != nil {
		return decision{false, "Denied by a rule", deny}
	}
	if allow != nil {
		return decision{true, "Allowed by a rule", allow}
	}
	if a.role == "owner" {
		return decision{false, "Owners only see what a rule grants", nil}
	}
	if act.Sensitive && a.role != "dev" {
		return decision{false, act.Label + " needs an explicit allow rule", nil}
	}
	if action == "view" && a.role == "viewer" {
		return decision{true, "Viewers see everything unless a rule hides it", nil}
	}
	if a.pol.Base == "deny" {
		return decision{false, "Base is \"allow nothing\" and no rule includes it", nil}
	}
	return decision{true, "Base allows everything the role can do", nil}
}

// can decides one action, optionally for one device.
func (a *access) can(action string, dev *uuid.UUID) bool { return a.decide(action, dev).Allowed }

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
// lists (vs shown with actions disabled). Viewers and owners always hide: there
// is nothing to disable for them.
func (a *access) hidesDevices() bool {
	if a.unrestricted() {
		return false
	}
	if a.role == "owner" {
		return true
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
	for _, g := range a.pol.Grants {
		if g.Effect == "deny" && g.Has("view") {
			return true
		}
	}
	return false
}

// visibleIDs is the device-id allowlist for list queries, or nil for "no filter".
// Computed once per request.
func (a *access) visibleIDs() []uuid.UUID {
	if !a.hidesDevices() {
		return nil
	}
	a.visOnce.Do(func() {
		a.loadScopes()
		ids := make([]uuid.UUID, 0, len(a.scopes))
		for id := range a.scopes {
			if a.canDevice("view", id) {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		a.vis = ids
	})
	return a.vis
}

// anyDeviceAction reports whether the user may take any action on a device
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

// holdsAnywhere reports whether some device exists on which the user may take
// the action (drives whether a nav entry / button is shown at all).
func (a *access) holdsAnywhere(action string) bool {
	if a.unrestricted() {
		return true
	}
	if !roleCeiling(a.role)[action] {
		return false
	}
	a.loadScopes()
	for id := range a.scopes {
		if a.canDevice(action, id) {
			return true
		}
	}
	return false
}

// requireDeviceAction is the common guard for single-device operator handlers.
// Writes a 403 and returns false when the policy withholds the action.
func (h *Handler) requireDeviceAction(w http.ResponseWriter, r *http.Request, action string, dev uuid.UUID) bool {
	d := h.access(r).decide(action, &dev)
	if d.Allowed {
		return true
	}
	h.denied(r, action, &dev, d)
	http.Error(w, "Your access policy does not allow this action on this device.", http.StatusForbidden)
	return false
}

// requireFleetAction guards fleet-level operator actions (QA, groups, alerts…).
func (h *Handler) requireFleetAction(w http.ResponseWriter, r *http.Request, action string) bool {
	d := h.access(r).decide(action, nil)
	if d.Allowed {
		return true
	}
	h.denied(r, action, nil, d)
	http.Error(w, "Your access policy does not allow this action.", http.StatusForbidden)
	return false
}

// enforceCommandTargets applies the policy to a command's targets: it drops the
// devices the user may not touch (403 if nothing is left). For a group target
// it resolves the members, drops what is out of scope, and returns the kept
// device ids with targetType "devices" so a partial group still sends. Returns
// the ids, the target type to store, and false when it already wrote a response.
func (h *Handler) enforceCommandTargets(w http.ResponseWriter, r *http.Request, action, targetType string, ids []uuid.UUID) ([]uuid.UUID, string, bool) {
	acc := h.access(r)
	if acc.unrestricted() {
		return ids, targetType, true
	}
	if targetType == "groups" {
		devIDs, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), ids)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return nil, "", false
		}
		kept, dropped := acc.filterDevices(action, devIDs)
		if dropped == 0 {
			return ids, targetType, true
		}
		if len(kept) == 0 {
			h.denied(r, action, nil, decision{Reason: "no device in the group is in scope"})
			http.Error(w, "Your access policy does not allow this command on any device in that group.", http.StatusForbidden)
			return nil, "", false
		}
		log.Printf("[access] %s: %s on group narrowed to %d of %d device(s)", acc.username, action, len(kept), len(devIDs))
		return kept, "devices", true
	}
	kept, dropped := acc.filterDevices(action, ids)
	if len(kept) == 0 && len(ids) > 0 {
		h.denied(r, action, nil, decision{Reason: "no selected device is in scope"})
		http.Error(w, "Your access policy does not allow this command on the selected devices.", http.StatusForbidden)
		return nil, "", false
	}
	if dropped > 0 {
		log.Printf("[access] %s: %s skipped %d device(s) outside their policy", acc.username, action, dropped)
	}
	return kept, targetType, true
}

// denied records a refused attempt: a log line always, an audit row at most
// once per user+action per minute so a scripted loop cannot flood the log.
var deniedRecent sync.Map // username+action -> time.Time

func (h *Handler) denied(r *http.Request, action string, dev *uuid.UUID, d decision) {
	user := h.currentUsername(r)
	target := action
	if dev != nil {
		target += " on " + dev.String()
	}
	log.Printf("[access] denied user=%s %s: %s", user, target, d.Reason)
	key := user + "|" + action
	if t, ok := deniedRecent.Load(key); ok && time.Since(t.(time.Time)) < time.Minute {
		return
	}
	deniedRecent.Store(key, time.Now())
	h.audit(r, "access.denied", target, d.Reason)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	// "OTA updates" stays as is; "Reboot" → "reboot"
	if b[0] >= 'A' && b[0] <= 'Z' && (len(b) < 2 || b[1] < 'A' || b[1] > 'Z') {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

// ── Visibility helpers for list handlers ────────────────────────────────────

// visible reports whether a device may appear in this user's lists.
func (a *access) visible(id uuid.UUID) bool {
	return !a.hidesDevices() || a.canDevice("view", id)
}

// applyFilter narrows a DeviceFilter to the user's visible devices.
func (a *access) applyFilter(f *db.DeviceFilter) {
	if ids := a.visibleIDs(); ids != nil {
		f.OnlyIDs = ids
	}
}

// keepVisible drops devices the user may not see.
func (a *access) keepVisible(devs []db.Device) []db.Device {
	if !a.hidesDevices() {
		return devs
	}
	out := devs[:0:0]
	for _, d := range devs {
		if a.canDevice("view", d.ID) {
			out = append(out, d)
		}
	}
	return out
}

// keepVisibleAlerts drops alerts about devices the user may not see. Fleet
// alerts (no device) stay.
func (a *access) keepVisibleAlerts(alerts []db.Alert) []db.Alert {
	if !a.hidesDevices() {
		return alerts
	}
	out := alerts[:0:0]
	for _, x := range alerts {
		if x.DeviceID == nil || a.canDevice("view", *x.DeviceID) {
			out = append(out, x)
		}
	}
	return out
}

// filterHiddenCommands drops commands none of whose targets the user may see.
func (h *Handler) filterHiddenCommands(r *http.Request, cmds []db.Command) []db.Command {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return cmds
	}
	out := cmds[:0:0]
	for _, c := range cmds {
		ids, err := h.db.GetCommandTargetIDs(r.Context(), c.ID)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if acc.canDevice("view", id) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// deviceRoute guards a /devices/{serial}/... route. A device the user may not
// see answers 404 (so hidden devices do not leak by URL); a device shown
// read-only accepts GETs and refuses writes; any other action is checked
// against the policy. Admins skip all of it.
func (h *Handler) deviceRoute(action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acc := h.access(r)
		if acc.unrestricted() {
			next(w, r)
			return
		}
		dev, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
		if err != nil || dev == nil {
			next(w, r) // let the handler produce its own 404
			return
		}
		if !acc.canDevice("view", dev.ID) {
			if acc.hidesDevices() {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet || action != "view" {
				h.denied(r, action, &dev.ID, decision{Reason: "device is read-only for this user"})
				http.Error(w, "Your access policy does not allow this on this device.", http.StatusForbidden)
				return
			}
			next(w, r)
			return
		}
		if action != "view" {
			if d := acc.decide(action, &dev.ID); !d.Allowed {
				h.denied(r, action, &dev.ID, d)
				http.Error(w, "Your access policy does not allow this action on this device.", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}
