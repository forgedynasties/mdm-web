package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// ── Access editor: /users/{id}/access ───────────────────────────────────────

// scopeOption is one pick in the scope search (venue, group or device).
type scopeOption struct {
	Type string `json:"type"` // "all" | "restaurant" | "group" | "device"
	ID   string `json:"id"`
	Name string `json:"name"`
	Meta string `json:"meta"` // "12 devices", "venue · Front counter"
}

// loadAccessTarget resolves the user being edited and checks the actor may
// manage them. Writes the error response itself.
func (h *Handler) loadAccessTarget(w http.ResponseWriter, r *http.Request) (*db.User, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return nil, false
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return nil, false
	}
	if !mayManageUser(h.role(r), u.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return nil, false
	}
	return u, true
}

// grantView is a grant decorated for the template.
type grantView struct {
	db.AccessGrant
	ActionLabels  []string
	AnyAction     bool
	Sensitive     bool
	OutsideRole   bool   // names an action the user's role can no longer hold (role was lowered)
	ExpiresIn     string // "in 6 days", "expired"
	CreatedByName string
	ScopeKind     string // "Whole fleet" | "Venue" | "Group" | "Device"
	Sentence      string
}

func (h *Handler) grantViews(ctx context.Context, u *db.User, grants []db.AccessGrant) []grantView {
	ceiling := roleCeiling(u.Role)
	out := make([]grantView, 0, len(grants))
	for _, g := range grants {
		v := grantView{AccessGrant: g, CreatedByName: g.CreatedBy}
		if g.CreatedBy != "" && g.CreatedBy != "system" {
			if u, err := h.db.GetUserByUsername(ctx, g.CreatedBy); err == nil && u != nil {
				v.CreatedByName = u.DisplayName()
			}
		}
		for _, a := range g.Actions {
			if a == "*" {
				v.AnyAction = true
				continue
			}
			act, ok := accessActionByKey[a]
			if !ok {
				continue
			}
			v.ActionLabels = append(v.ActionLabels, act.Label)
			if act.Sensitive && g.Effect == "allow" {
				v.Sensitive = true
			}
			if ceiling != nil && !ceiling[a] {
				v.OutsideRole = true
			}
		}
		switch g.ScopeType {
		case "restaurant":
			v.ScopeKind = "Venue"
		case "group":
			v.ScopeKind = "Group"
		case "device":
			v.ScopeKind = "Device"
		default:
			v.ScopeKind = "Whole fleet"
		}
		if g.ExpiresAt != nil {
			if g.Expired {
				v.ExpiresIn = "expired " + humanSince(*g.ExpiresAt)
			} else {
				v.ExpiresIn = "expires " + humanUntil(*g.ExpiresAt)
			}
		}
		v.Sentence = grantSentence(g)
		out = append(out, v)
	}
	return out
}

func humanUntil(t time.Time) string {
	d := time.Until(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %d min", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("in %d h", int(d.Hours()))
	default:
		return fmt.Sprintf("in %d days", int(d.Hours()/24))
	}
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// grantSentence renders a grant as one plain sentence.
func grantSentence(g db.AccessGrant) string {
	var acts []string
	for _, a := range g.Actions {
		if a == "*" {
			acts = append(acts, "take any action")
			continue
		}
		if act, ok := accessActionByKey[a]; ok {
			acts = append(acts, lowerFirst(act.Label))
		}
	}
	verb := "Can "
	if g.Effect == "deny" {
		verb = "Cannot "
	}
	where := " across the whole fleet"
	switch g.ScopeType {
	case "restaurant":
		where = " in venue " + g.ScopeName
	case "group":
		where = " in group " + g.ScopeName
	case "device":
		where = " on device " + g.ScopeName
	}
	s := verb + joinAnd(acts) + where
	if g.ExpiresAt != nil && !g.Expired {
		s += " until " + g.ExpiresAt.Local().Format("2 Jan 15:04")
	}
	return s + "."
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return "nothing"
	case 1:
		return xs[0]
	case 2:
		return xs[0] + " and " + xs[1]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func describeGrant(g db.AccessGrant) string {
	sc := g.ScopeType
	if g.ScopeID != nil {
		sc += ":" + g.ScopeID.String()
		if g.ScopeName != "" {
			sc += " (" + g.ScopeName + ")"
		}
	}
	s := g.Effect + " " + strings.Join(g.Actions, ",") + " @" + sc
	if g.ExpiresAt != nil {
		s += " until " + g.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if g.Note != "" {
		s += " — " + g.Note
	}
	return s
}

// policySummary turns a policy into the sentences shown on the profile and the
// editor's "in plain words" card.
func (h *Handler) policySummary(ctx context.Context, u *db.User, pol db.AccessPolicy) []string {
	if u == nil {
		return nil
	}
	switch u.Role {
	case "admin":
		return []string{"Super admin: everything, everywhere. No rules apply."}
	case "owner":
		var venues []string
		for _, g := range pol.Grants {
			if g.Effect == "allow" && g.ScopeType == "restaurant" && g.Has("view") {
				venues = append(venues, g.ScopeName)
			}
		}
		if len(venues) == 0 {
			return []string{"Sees no devices yet. Add an allow rule for their venue."}
		}
		return []string{"Sees the devices of " + joinAnd(venues) + ", read-only."}
	}
	var out []string
	base := "Starts with everything " + strings.ToLower(roleArticle(u.Role)) + " can do"
	if u.Role == "viewer" {
		base = "Sees the whole fleet, read-only"
	}
	if pol.Base == "deny" {
		base = "Starts with nothing"
	}
	if u.Role == "operator" || u.Role == "user_manager" {
		hasRemote := false
		for _, g := range pol.Grants {
			if g.Effect == "allow" && g.Has("remote") {
				hasRemote = true
			}
		}
		if !hasRemote {
			base += ", except remote control"
		}
	}
	out = append(out, base+".")
	for _, g := range pol.Grants {
		out = append(out, grantSentence(g))
	}
	if pol.HideOutOfScope {
		out = append(out, "Devices they cannot see are hidden from every list.")
	} else if u.Role != "viewer" && (pol.Base == "deny" || len(pol.Grants) > 0) {
		out = append(out, "Devices they cannot see still appear, with actions disabled.")
	}
	return out
}

func roleArticle(role string) string {
	switch role {
	case "dev":
		return "a dev"
	case "user_manager":
		return "an access admin"
	default:
		return "an operator"
	}
}

// usersRedirect returns to the Manage page when the form says so (redirect=
// /users/…), else to the people list.
func (h *Handler) usersRedirect(w http.ResponseWriter, r *http.Request) {
	if to := r.FormValue("redirect"); strings.HasPrefix(to, "/users/") && !strings.Contains(to, "//") {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserAccessPage renders the Manage page: account settings plus the access
// rule editor (also served at /users/{id}/access).
func (h *Handler) UserAccessPage(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	pol, err := h.db.GetUserAccess(ctx, u.Username)
	if err != nil {
		log.Printf("[access] load %s: %v", u.Username, err)
	}
	all, _ := h.db.ListAccessGrants(ctx, u.ID, true)
	views := h.grantViews(ctx, u, all)
	live, expired := 0, 0
	for _, v := range views {
		if v.Expired {
			expired++
		} else {
			live++
		}
	}
	// group the catalogue for the drawer
	type actGroup struct {
		Name    string
		Actions []accessAction
	}
	var groups []actGroup
	idx := map[string]int{}
	for _, a := range grantableActions(u.Role) {
		gi, ok := idx[a.Group]
		if !ok {
			gi = len(groups)
			idx[a.Group] = gi
			groups = append(groups, actGroup{Name: a.Group})
		}
		groups[gi].Actions = append(groups[gi].Actions, a)
	}
	actor := h.accessFor(ctx, h.role(r), h.currentUsername(r))
	h.render(w, r, "user_access.html", map[string]any{
		"Title":        "Manage · " + u.DisplayName(),
		"Assignable":   assignableRoles(h.role(r)),
		"CanEditAvatar": h.mayEditAvatar(r, u),
		"CanDelete":    u.Username != h.user,
		"Self":         u.Username == h.currentUsername(r),
		"User":         u,
		"Bubble":       userBubbleFn(u.Username),
		"Policy":       pol,
		"Grants":       views,
		"Live":         live,
		"ExpiredCount": expired,
		"ActionGroups": groups,
		"Summary":      h.policySummary(ctx, u, pol),
		"RoleSentence": roleCeilingSentence(u.Role),
		"RoleLower":    strings.ToLower(roleLabel(u.Role)),
		"HasBase":      u.Role == "operator" || u.Role == "user_manager" || u.Role == "dev",
		"ActorIsAdmin": actor.unrestricted(),
		"UsersTab":     "access",
	})
}

func roleCeilingSentence(role string) string {
	switch role {
	case "dev":
		return "A dev can do everything an operator can, plus shell and OTA updates. Rules say where."
	case "user_manager":
		return "An access admin can do everything an operator can, and manage users. Rules say where."
	case "viewer":
		return "A viewer only looks. Rules say which devices they see."
	case "owner":
		return "A restaurant owner sees only the venues you allow, read-only."
	case "admin":
		return "Super admin. Nothing to configure."
	}
	return "An operator can act on devices: commands, kiosk, notes, alerts, and remote control when a rule names it. Rules say where."
}

// AccessScopeSearch answers the editor's scope picker: venues, groups and
// devices matching q (JSON).
func (h *Handler) AccessScopeSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var out []scopeOption
	if q == "" || strings.Contains("whole fleet all devices everything", q) {
		out = append(out, scopeOption{Type: "all", ID: "all", Name: "Whole fleet", Meta: "every device, now and later"})
	}
	rs, _ := h.db.ListRestaurants(ctx)
	for _, x := range rs {
		if q == "" || strings.Contains(strings.ToLower(x.Name), q) {
			out = append(out, scopeOption{Type: "restaurant", ID: x.ID.String(), Name: x.Name, Meta: fmt.Sprintf("venue · %d devices", x.DeviceCount)})
		}
		if len(out) > 24 {
			break
		}
	}
	gs, _ := h.db.ListGroups(ctx)
	for _, g := range gs {
		if q == "" || strings.Contains(strings.ToLower(g.Name), q) {
			out = append(out, scopeOption{Type: "group", ID: g.ID.String(), Name: g.Name, Meta: fmt.Sprintf("group · %d devices", g.DeviceCount)})
		}
		if len(out) > 32 {
			break
		}
	}
	if len(q) >= 2 {
		devs, _ := h.db.SearchDevicesBySerial(ctx, q, 8)
		ids := make([]uuid.UUID, len(devs))
		for i, d := range devs {
			ids[i] = d.ID
		}
		nick, _ := h.db.GetNicknames(ctx, ids)
		for _, d := range devs {
			meta := "device"
			if n := nick[d.ID]; n != "" {
				meta += " · " + n
			}
			if d.RestaurantName != "" {
				meta += " · " + d.RestaurantName
			}
			out = append(out, scopeOption{Type: "device", ID: d.ID.String(), Name: d.SerialNumber, Meta: meta})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// UserAccessCheck is the "check a device" preview: every action, allowed or
// not, and the rule that decided it. Fleet actions are included with no device.
func (h *Handler) UserAccessCheck(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	type row struct {
		Key, Label, Group string `json:"-"`
		K                 string `json:"key"`
		L                 string `json:"label"`
		G                 string `json:"group"`
		Allowed           bool   `json:"allowed"`
		Reason            string `json:"reason"`
		Rule              string `json:"rule,omitempty"`
		Sensitive         bool   `json:"sensitive"`
	}
	resp := map[string]any{"serial": serial}
	var dev *db.Device
	if serial != "" {
		d, err := h.db.GetDevice(ctx, serial)
		if err != nil || d == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "No device with serial " + serial})
			return
		}
		dev = d
		nick, _ := h.db.GetNicknames(ctx, []uuid.UUID{d.ID})
		resp["device"] = map[string]any{"serial": d.SerialNumber, "nickname": nick[d.ID], "venue": d.RestaurantName}
	}
	invalidatePolicy(u.Username)
	acc := h.accessFor(ctx, u.Role, u.Username)
	var rows []row
	for _, a := range accessActions {
		if a.Fleet && dev != nil {
			continue
		}
		if !a.Fleet && dev == nil {
			continue
		}
		var did *uuid.UUID
		if dev != nil {
			did = &dev.ID
		}
		d := acc.decide(a.Key, did)
		rw := row{K: a.Key, L: a.Label, G: a.Group, Allowed: d.Allowed, Reason: d.Reason, Sensitive: a.Sensitive}
		if d.Grant != nil {
			rw.Rule = grantSentence(*d.Grant)
		}
		rows = append(rows, rw)
	}
	resp["actions"] = rows
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// UserAccessSetBase saves base + hide flag.
func (h *Handler) UserAccessSetBase(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	r.ParseForm()
	pol := db.AccessPolicy{Base: "allow"}
	if r.FormValue("base") == "deny" {
		pol.Base = "deny"
	}
	pol.HideOutOfScope = r.FormValue("hide_out_of_scope") == "1"
	if err := h.db.SetUserAccess(r.Context(), u.ID, pol); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(u.Username)
	h.audit(r, "user.access.base", u.Username, "base="+pol.Base+fmt.Sprintf(" hide_out_of_scope=%v", pol.HideOutOfScope))
	h.hxDoneToast(w, r, "/users/"+u.ID.String()+"/access", "Saved", "ok")
}

// parseGrantForm reads effect / actions / note / expiry shared by add and edit.
func parseGrantForm(r *http.Request) (effect string, actions []string, note string, expires *time.Time, err error) {
	effect = "allow"
	if r.FormValue("effect") == "deny" {
		effect = "deny"
	}
	seen := map[string]bool{}
	for _, a := range r.Form["actions"] {
		if (a == "*" || isAccessAction(a)) && !seen[a] {
			seen[a] = true
			actions = append(actions, a)
		}
	}
	if len(actions) == 0 {
		return "", nil, "", nil, fmt.Errorf("pick at least one action")
	}
	note = strings.TrimSpace(r.FormValue("note"))
	if len(note) > 200 {
		note = note[:200]
	}
	switch e := strings.TrimSpace(r.FormValue("expires")); e {
	case "", "never":
	case "1d", "7d", "30d", "90d":
		n := map[string]int{"1d": 1, "7d": 7, "30d": 30, "90d": 90}[e]
		t := time.Now().Add(time.Duration(n) * 24 * time.Hour)
		expires = &t
	default:
		t, perr := time.ParseInLocation("2006-01-02T15:04", e, time.Local)
		if perr != nil {
			return "", nil, "", nil, fmt.Errorf("expiry must be a date and time")
		}
		if t.Before(time.Now()) {
			return "", nil, "", nil, fmt.Errorf("expiry is in the past")
		}
		expires = &t
	}
	return
}

// checkGrantAllowed enforces the ceiling and the delegation rule for an allow
// grant. Returns a user-facing reason when refused.
func (h *Handler) checkGrantAllowed(r *http.Request, target *db.User, g db.AccessGrant) error {
	ceiling := roleCeiling(target.Role)
	var concrete []string
	for _, a := range g.Actions {
		if a == "*" {
			for _, x := range accessActions {
				if ceiling[x.Key] && !x.Sensitive {
					concrete = append(concrete, x.Key)
				}
			}
			continue
		}
		act := accessActionByKey[a]
		if ceiling != nil && !ceiling[a] {
			return fmt.Errorf("%s accounts never get %s", roleLabel(target.Role), lowerFirst(act.Label))
		}
		if act.Sensitive && g.Effect == "allow" && r.FormValue("confirm_sensitive") != "1" {
			return fmt.Errorf("tick the confirmation to grant %s", lowerFirst(act.Label))
		}
		concrete = append(concrete, a)
	}
	if g.Effect == "deny" || h.role(r) == "admin" {
		return nil
	}
	// Delegation: the granter must hold every (action, device) they hand out.
	actor := h.accessFor(r.Context(), h.role(r), h.currentUsername(r))
	var ids []uuid.UUID
	switch g.ScopeType {
	case "all":
		actor.loadScopes()
		for id := range actor.scopes {
			ids = append(ids, id)
		}
	case "restaurant":
		ids, _ = h.db.GetDeviceIDsByRestaurantIDs(r.Context(), []uuid.UUID{*g.ScopeID})
	case "group":
		ids, _ = h.db.GetDeviceIDsByGroupIDs(r.Context(), []uuid.UUID{*g.ScopeID})
	case "device":
		ids = []uuid.UUID{*g.ScopeID}
	}
	for _, a := range concrete {
		act := accessActionByKey[a]
		if act.Fleet {
			if !actor.can(a, nil) {
				return fmt.Errorf("you can't grant \"%s\": your own access doesn't include it", lowerFirst(act.Label))
			}
			continue
		}
		if _, dropped := actor.filterDevices(a, ids); dropped > 0 {
			return fmt.Errorf("you can't grant \"%s\" there: your own access doesn't cover %d of those devices", lowerFirst(act.Label), dropped)
		}
	}
	return nil
}

// UserAccessAddGrant creates one grant per selected scope.
func (h *Handler) UserAccessAddGrant(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	r.ParseForm()
	effect, actions, note, expires, err := parseGrantForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Scopes: "all", "restaurant:<id>", "group:<id>", "device:<id>", plus pasted serials.
	type scope struct {
		typ string
		id  *uuid.UUID
	}
	var scopes []scope
	seen := map[string]bool{}
	for _, s := range r.Form["scopes"] {
		if seen[s] {
			continue
		}
		seen[s] = true
		if s == "all" {
			scopes = append(scopes, scope{typ: "all"})
			continue
		}
		parts := strings.SplitN(s, ":", 2)
		if len(parts) != 2 {
			continue
		}
		id, perr := uuid.Parse(parts[1])
		if perr != nil || (parts[0] != "restaurant" && parts[0] != "group" && parts[0] != "device") {
			continue
		}
		scopes = append(scopes, scope{typ: parts[0], id: &id})
	}
	if raw := strings.TrimSpace(r.FormValue("serials")); raw != "" {
		var serials []string
		for _, s := range strings.FieldsFunc(raw, func(c rune) bool { return c == '\n' || c == ',' || c == ' ' || c == ';' || c == '\t' || c == '\r' }) {
			if s = strings.TrimSpace(s); s != "" {
				serials = append(serials, s)
			}
		}
		ids, err := h.db.GetDeviceIDsBySerials(ctx, serials)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if len(ids) != len(serials) {
			known := map[string]bool{}
			devs, _ := h.db.ListAllSerials(ctx)
			for _, s := range devs {
				known[strings.ToUpper(s)] = true
			}
			var unknown []string
			for _, s := range serials {
				if !known[strings.ToUpper(s)] {
					unknown = append(unknown, s)
				}
			}
			http.Error(w, "Unknown serial(s): "+strings.Join(unknown, ", "), http.StatusBadRequest)
			return
		}
		for _, id := range ids {
			id := id
			key := "device:" + id.String()
			if !seen[key] {
				seen[key] = true
				scopes = append(scopes, scope{typ: "device", id: &id})
			}
		}
	}
	if len(scopes) == 0 {
		http.Error(w, "Pick where the rule applies: the whole fleet, a venue, a group, or devices.", http.StatusBadRequest)
		return
	}
	var saved []db.AccessGrant
	for _, sc := range scopes {
		g := db.AccessGrant{UserID: u.ID, Effect: effect, ScopeType: sc.typ, ScopeID: sc.id, Actions: actions, Note: note, ExpiresAt: expires, CreatedBy: h.currentUsername(r)}
		if err := h.checkGrantAllowed(r, u, g); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		out, err := h.db.AddAccessGrant(ctx, g)
		if err != nil {
			log.Printf("[access] add grant for %s: %v", u.Username, err)
			http.Error(w, "Could not save the rule (does the venue / group / device still exist?)", http.StatusBadRequest)
			return
		}
		saved = append(saved, *out)
		h.audit(r, "user.access.grant", u.Username, describeGrant(*out))
	}
	invalidatePolicy(u.Username)
	msg := "Rule added"
	if len(saved) > 1 {
		msg = fmt.Sprintf("%d rules added", len(saved))
	}
	h.hxDoneToast(w, r, "/users/"+u.ID.String()+"/access", msg, "ok")
}

func (h *Handler) loadGrant(w http.ResponseWriter, r *http.Request, u *db.User) (*db.AccessGrant, bool) {
	gid, err := uuid.Parse(r.PathValue("gid"))
	if err != nil {
		http.Error(w, "Invalid rule", http.StatusBadRequest)
		return nil, false
	}
	g, err := h.db.GetAccessGrant(r.Context(), gid)
	if err != nil || g == nil || g.UserID != u.ID {
		http.Error(w, "Rule not found", http.StatusNotFound)
		return nil, false
	}
	return g, true
}

// UserAccessEditGrant rewrites effect / actions / note / expiry; scope is fixed.
func (h *Handler) UserAccessEditGrant(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	g, ok := h.loadGrant(w, r, u)
	if !ok {
		return
	}
	r.ParseForm()
	effect, actions, note, expires, err := parseGrantForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	next := *g
	next.Effect, next.Actions, next.Note, next.ExpiresAt = effect, actions, note, expires
	if err := h.checkGrantAllowed(r, u, next); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := h.db.UpdateAccessGrant(r.Context(), g.ID, effect, actions, note, expires); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(u.Username)
	h.audit(r, "user.access.edit", u.Username, describeGrant(*g)+" → "+describeGrant(next))
	h.hxDoneToast(w, r, "/users/"+u.ID.String()+"/access", "Rule updated", "ok")
}

func (h *Handler) UserAccessDeleteGrant(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	g, ok := h.loadGrant(w, r, u)
	if !ok {
		return
	}
	if err := h.db.DeleteAccessGrant(r.Context(), g.ID); err != nil {
		http.Error(w, "Could not remove", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(u.Username)
	h.audit(r, "user.access.revoke", u.Username, describeGrant(*g))
	h.hxDoneToast(w, r, "/users/"+u.ID.String()+"/access", "Rule removed", "ok")
}

// ── Overview: /users/access ─────────────────────────────────────────────────

// UsersAccessPage lists every account with its rules in plain words.
func (h *Handler) UsersAccessPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := h.db.ListUsers(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	counts, _ := h.db.CountAccessGrants(ctx, sensitiveActionKeys)
	type row struct {
		User      db.User
		Bubble    any
		Summary   []string
		Grants    int
		Sensitive bool
		Custom    bool
		CanEdit   bool
	}
	var rows []row
	custom, sensitive := 0, 0
	for _, u := range users {
		pol, _ := h.db.GetUserAccess(ctx, u.Username)
		c := counts[u.ID]
		rw := row{User: u, Bubble: userBubbleFn(u.Username), Summary: h.policySummary(ctx, &u, pol), Grants: c.Grants, Sensitive: c.Sensitive,
			Custom: u.Role != "admin" && !pol.IsEmpty(), CanEdit: u.Role != "admin" && mayManageUser(h.role(r), u.Role, "")}
		if rw.Custom {
			custom++
		}
		if rw.Sensitive {
			sensitive++
		}
		rows = append(rows, rw)
	}
	order := map[string]int{"operator": 0, "user_manager": 1, "dev": 2, "viewer": 3, "owner": 4, "admin": 5}
	sort.SliceStable(rows, func(i, j int) bool { return order[rows[i].User.Role] < order[rows[j].User.Role] })
	h.render(w, r, "users_access.html", map[string]any{
		"Title":     "Access control",
		"Rows":      rows,
		"Custom":    custom,
		"Sensitive": sensitive,
		"UsersTab":  "access",
	})
}

// whoHasAccess lists, for admins and access admins, every rule that covers a
// device, grouped per user, so the device page can answer "who can touch this?".
type accessHolder struct {
	Username, Name, Role string
	Bubble               any
	Rules                []string
	Sensitive            bool
}

func (h *Handler) whoHasAccess(r *http.Request, deviceID uuid.UUID) []accessHolder {
	if !roleManagesUsers(h.role(r)) {
		return nil
	}
	grants, err := h.db.AccessGrantsForDevice(r.Context(), deviceID)
	if err != nil || len(grants) == 0 {
		return nil
	}
	byUser := map[string]*accessHolder{}
	var order []string
	for _, g := range grants {
		hd, ok := byUser[g.Username]
		if !ok {
			hd = &accessHolder{Username: g.Username, Name: g.Username, Bubble: userBubbleFn(g.Username)}
			if u, err := h.db.GetUserByUsername(r.Context(), g.Username); err == nil && u != nil {
				hd.Name, hd.Role = u.DisplayName(), roleLabel(u.Role)
			}
			byUser[g.Username] = hd
			order = append(order, g.Username)
		}
		hd.Rules = append(hd.Rules, grantSentence(g))
		if g.Effect == "allow" {
			for _, a := range g.Actions {
				if accessActionByKey[a].Sensitive {
					hd.Sensitive = true
				}
			}
		}
	}
	out := make([]accessHolder, 0, len(order))
	for _, u := range order {
		out = append(out, *byUser[u])
	}
	return out
}
