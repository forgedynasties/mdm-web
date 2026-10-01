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
	{Key: "app_control", Label: "App control (reload, restart, clear cache, update check)", Group: "Commands", Help: "MDM Lite devices: control the menu board app itself."},
	{Key: "query", Label: "Run diagnostic query", Group: "Commands"},
	{Key: "logcat", Label: "Request logcat", Group: "Commands"},
	{Key: "kiosk", Label: "Kiosk settings", Group: "Device"},
	{Key: "notes", Label: "Device notes", Group: "Device"},
	{Key: "queue", Label: "Cancel / clear queue", Group: "Device"},
	{Key: "remote", Label: "Remote control", Group: "Sessions", Sensitive: true, Help: "Live screen and touch. Dev, super op and access admin have it by default; an operator needs a rule that names it (\"any action\" never includes it)."},
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
		// DevOnly actions belong to the dev ceiling; the super op gets exactly one of
		// them, the firmware push.
		if a.DevOnly && role != "dev" && !(role == "super_op" && a.Key == "ota") {
			continue
		}
		m[a.Key] = true
	}
	if role == "operator" || role == "user_manager" || role == "super_op" || role == "dev" {
		return m
	}
	return map[string]bool{} // unknown role: nothing
}

// sensitiveByDefault: the sensitive actions a role holds without a rule — dev all of
// them, the super op firmware pushes, and remote control for the super op and the
// access admin. Everyone else needs an allow rule that names the action.
func sensitiveByDefault(role, action string) bool {
	return role == "dev" || (role == "super_op" && action == "ota") ||
		(action == "remote" && (role == "super_op" || role == "user_manager"))
}

// grantRedundant reports an allow rule that gives nothing the role does not already
// have: with a base of "everything", every action in it is within the role and
// either not sensitive or one the role holds by default. An operator's "remote
// control everywhere" becomes one of these the moment they are made a super op.
func grantRedundant(role, base string, g db.AccessGrant) bool {
	if g.Effect != "allow" || base == "deny" || role == "owner" || len(g.Actions) == 0 {
		return false
	}
	c := roleCeiling(role)
	for _, a := range g.Actions {
		if a == "*" || c == nil {
			continue
		}
		if !c[a] || (accessActionByKey[a].Sensitive && !sensitiveByDefault(role, a)) {
			return false
		}
	}
	return true
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
		// Fail closed, and don't cache it: a zero policy means "allow everything the
		// role can do" and would have dropped every deny rule for 20 seconds.
		log.Printf("[access] policy for %q: %v (denying)", a.username, err)
		a.pol = db.AccessPolicy{Base: "deny"}
		return a
	}
	policyCache.Store(a.username, policyEntry{pol: pol, at: time.Now()})
	a.pol = pol
	return a
}

// unrestricted: nothing is ever filtered for this user. Only super admin.
func (a *access) unrestricted() bool { return a.role == "admin" }

// hidesDPC: DPC-managed (outsourced) devices used to be admin/super-op only, which
// left every other role seeing just our own firmware hardware. They are ordinary fleet
// devices now — every role sees them and the access policy alone decides what may be
// done to them — so nothing is hidden on agent grounds any more.
func (a *access) hidesDPC() bool { return false }

// isDPC reports whether the device runs the DPC agent (from the per-request scope map).
func (a *access) isDPC(dev uuid.UUID) bool {
	a.loadScopes()
	return a.scopes[dev].DPC
}

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
	if dev != nil && a.hidesDPC() && a.isDPC(*dev) {
		return decision{false, "DPC-managed devices are limited to super admins and super ops", nil}
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
	// Sensitive actions need an explicit allow rule — except for the roles whose
	// ceiling exists for them: dev (all of them), the super op (firmware pushes),
	// and remote control, which the super op and the access admin have by default.
	// Operators still need a rule that names the device.
	if act.Sensitive && !sensitiveByDefault(a.role, action) {
		return decision{false, act.Label + " needs an explicit allow rule", nil}
	}
	// Base deny is checked before the viewer default: a viewer set to "allow nothing"
	// must not still see the whole fleet (30 Sep access audit).
	if a.pol.Base == "deny" {
		return decision{false, "Base is \"allow nothing\" and no rule includes it", nil}
	}
	if action == "view" && a.role == "viewer" {
		return decision{true, "Viewers see everything unless a rule hides it", nil}
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

// hidesDevices reports whether devices this user cannot view are removed from lists.
// "See device" is the visibility grant, so a device without it is hidden, for every
// role. It used to take a second switch (HideOutOfScope) for operators and devs; with
// it off, a user allowed to see one device was still listed the whole fleet read-only,
// which is not what "See device" says (30 Sep access audit). A device the user may see
// but not act on still shows, read-only.
func (a *access) hidesDevices() bool {
	if a.unrestricted() {
		return false
	}
	if a.role == "owner" {
		return true
	}
	return a.hasViewRestriction()
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
	hideDev, hideDPC := a.hidesDevices(), a.hidesDPC()
	if !hideDev && !hideDPC {
		return nil
	}
	a.visOnce.Do(func() {
		a.loadScopes()
		ids := make([]uuid.UUID, 0, len(a.scopes))
		for id := range a.scopes {
			// Each reason filters on its own criterion. Applying the view policy
			// whenever ANY reason is in play is what made an operator with a base-deny
			// policy and no hide flag see an empty device list: DPC hiding put them on
			// the filter path, and the filter then also enforced the view restriction
			// that was deliberately meant to leave their list alone and only disable
			// the actions.
			if hideDPC && a.isDPC(id) {
				continue
			}
			if hideDev && !a.canDevice("view", id) {
				continue
			}
			ids = append(ids, id)
		}
		// When the only reason to filter was DPC hiding and no DPC device was found,
		// there is no filter to apply, and saying so with nil is not just tidier: a
		// materialised list is a snapshot of the devices that existed when this request
		// loaded its scopes, so returning one would quietly exclude anything enrolled
		// afterwards.
		//
		// Deliberately not done when hideDev is set. Callers do not agree on what nil
		// means — OwnerHome reads it as "no devices" while the list pages read it as "no
		// filter" — so collapsing a policy-filtered result to nil would blank an owner's
		// home page the moment their grants happened to cover the whole fleet.
		if !hideDev && len(ids) == len(a.scopes) {
			a.vis = nil
			return
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
	// "all" carries no ids; for anyone but the super admin it is the fleet narrowed to
	// what they may touch, stored as a device list. Letting it through unfiltered is how
	// a resend could broadcast to the whole fleet.
	if targetType == "all" {
		all, err := h.db.GetAllDeviceIDs(r.Context())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return nil, "", false
		}
		ids, targetType = all, "devices"
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
	if a.hidesDPC() && a.isDPC(id) {
		return false
	}
	return !a.hidesDevices() || a.canDevice("view", id)
}

// applyFilter narrows a DeviceFilter to the user's visible devices.
func (a *access) applyFilter(f *db.DeviceFilter) {
	if a.hidesDPC() {
		f.AgentKind = "firmware"
	}
	if ids := a.visibleIDs(); ids != nil {
		f.OnlyIDs = ids
	}
}

// keepVisible drops devices the user may not see.
func (a *access) keepVisible(devs []db.Device) []db.Device {
	if !a.hidesDevices() && !a.hidesDPC() {
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
	if !acc.hidesDevices() && !acc.hidesDPC() {
		return cmds
	}
	out := cmds[:0:0]
	for _, c := range cmds {
		// Device ids the command reached — command_targets holds group ids for a group
		// command, which never match a device.
		ids, err := h.db.GetCommandDeviceIDs(r.Context(), c.ID)
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

// keepVisibleDeliveries drops the per-device rows of a command (status, output,
// screenshot) for devices the user may not see: a command they can open because one
// target is theirs must not show the others' output.
func (h *Handler) keepVisibleDeliveries(r *http.Request, ds []db.CommandDelivery) []db.CommandDelivery {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return ds
	}
	out := ds[:0:0]
	for _, d := range ds {
		if acc.visible(d.DeviceID) {
			out = append(out, d)
		}
	}
	return out
}

// commandVisible reports whether the user may open a command at all: the super admin
// always, anyone else when at least one device it reached is visible to them.
func (h *Handler) commandVisible(r *http.Request, cmd db.Command) bool {
	return len(h.filterHiddenCommands(r, []db.Command{cmd})) > 0
}

// keepVisibleSerials drops serials of devices the user may not see (and unknown ones,
// for a restricted user).
func (h *Handler) keepVisibleSerials(r *http.Request, serials []string) []string {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return serials
	}
	ids, err := h.db.SerialIDs(r.Context(), serials)
	if err != nil {
		return nil
	}
	out := serials[:0:0]
	for _, s := range serials {
		if id, ok := ids[s]; ok && acc.visible(id) {
			out = append(out, s)
		}
	}
	return out
}

// fleetWide guards a page or endpoint that is about the whole fleet (the device map,
// compliance, exports, productions, AI fleet summary…) and has no per-device filter:
// a user with a visibility limit is sent to their own device list instead (a JSON or
// write request gets a 403). The Overview and Fleet health already do this.
func (h *Handler) fleetWide(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.access(r).hidesDevices() {
			if r.Method != http.MethodGet || strings.Contains(r.Header.Get("Accept"), "application/json") {
				http.Error(w, "This covers the whole fleet, which your access policy does not.", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/devices", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// visibleCollections counts, per restaurant and per group, the devices this user can
// see — the rails and pickers list only collections with at least one, sized by it.
func (a *access) visibleCollections() (rests, groups map[uuid.UUID]int) {
	rests, groups = map[uuid.UUID]int{}, map[uuid.UUID]int{}
	a.loadScopes()
	for id, sc := range a.scopes {
		if !a.visible(id) {
			continue
		}
		if sc.RestaurantID != nil {
			rests[*sc.RestaurantID]++
		}
		for _, g := range sc.Groups {
			groups[g]++
		}
	}
	return rests, groups
}

// scopeHealth keeps the health rows of collections the user can see, with the device
// count set to what they can see.
func scopeHealth(rows []db.GroupHealth, seen map[uuid.UUID]int) []db.GroupHealth {
	out := rows[:0:0]
	for _, g := range rows {
		if n := seen[g.GroupID]; n > 0 {
			if g.DeviceCount > n {
				// Partly visible: only their devices' count, not the venue's others.
				g.DeviceCount, g.OfflineCount, g.DormantCount = n, 0, 0
			}
			out = append(out, g)
		}
	}
	return out
}

// collectionRoute guards /restaurants/{id}/… and /groups/{id}/…: a user with a
// visibility limit reaches a restaurant or group only when they can see at least one of
// its devices; otherwise it does not exist for them (404), as a hidden device does.
func (h *Handler) collectionRoute(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acc := h.access(r)
		if acc.hidesDevices() {
			id, err := uuid.Parse(r.PathValue("id"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			rests, groups := acc.visibleCollections()
			seen := rests
			if kind == "group" {
				seen = groups
			}
			if seen[id] == 0 {
				http.NotFound(w, r)
				return
			}
		}
		next(w, r)
	}
}

// visibleGroupList keeps the groups this user has a device in, sized by it.
func (h *Handler) visibleGroupList(r *http.Request, gs []db.Group) []db.Group {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return gs
	}
	_, seen := acc.visibleCollections()
	out := gs[:0:0]
	for _, g := range gs {
		if n := seen[g.ID]; n > 0 {
			g.DeviceCount = n
			out = append(out, g)
		}
	}
	return out
}

// visibleRestaurantList keeps the restaurants this user has a device in, sized by it.
func (h *Handler) visibleRestaurantList(r *http.Request, rs []db.Restaurant) []db.Restaurant {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return rs
	}
	seen, _ := acc.visibleCollections()
	out := rs[:0:0]
	for _, x := range rs {
		if n := seen[x.ID]; n > 0 {
			x.DeviceCount = n
			out = append(out, x)
		}
	}
	return out
}
