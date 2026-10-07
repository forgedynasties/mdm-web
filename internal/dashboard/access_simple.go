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

// ── Access editor, the two questions (30 Sep) ───────────────────────────────
//
// "Which devices can they see?" and "what can they do there?" replace the starting
// point plus allow/deny rules as the way to set someone up. The answer is stored with
// the same engine: "every device, everything their role can" is the plain default
// (base allow, no rules); anything else is base deny plus one allow rule per place,
// all with the same actions. A setup the questions can't express — exceptions, a
// different set of actions per place — is shown as such and edited in the rules
// editor below it.

// roleExtras are the sensitive actions a role holds by default (see decide), which a
// place-limited "everything a <role> can" rule has to name explicitly.
func roleExtras(role string) []string {
	switch role {
	case "super_op":
		return []string{"remote", "ota"}
	case "user_manager":
		return []string{"remote"}
	}
	return nil
}

func roleActions(role string) []string { return append([]string{"*"}, roleExtras(role)...) }

func sameActions(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// simpleAccess is a user's access as the two questions answer it.
type simpleAccess struct {
	OK      bool   // the questions can express it
	Why     string // why not, when !OK
	Every   bool   // every device, now and later
	Places  []scopeOption
	Do      string   // "profile:<id>" | "role" | "custom"
	Actions []string // for custom
	Expires *time.Time
	Note    string
}

// placeCounts counts devices per restaurant and per group.
func placeCounts(scopes map[uuid.UUID]db.DeviceScope) (rests, groups map[uuid.UUID]int) {
	rests, groups = map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for _, sc := range scopes {
		if sc.RestaurantID != nil {
			rests[*sc.RestaurantID]++
		}
		for _, g := range sc.Groups {
			groups[g]++
		}
	}
	return
}

func placeOption(g db.AccessGrant, rests, groups map[uuid.UUID]int) scopeOption {
	o := scopeOption{Type: g.ScopeType, Name: g.ScopeName}
	if g.ScopeID != nil {
		o.ID = g.ScopeID.String()
		switch g.ScopeType {
		case "restaurant":
			o.Meta = fmt.Sprintf("restaurant · %d devices", rests[*g.ScopeID])
		case "group":
			o.Meta = fmt.Sprintf("group · %d devices", groups[*g.ScopeID])
		default:
			o.Meta = "device"
		}
	}
	return o
}

// lookOnly is the profile a new, limited account starts on.
func lookOnly(profiles []db.AccessProfile) string {
	for _, p := range profiles {
		if p.Builtin && sameActions(p.Actions, []string{"view"}) {
			return "profile:" + p.ID.String()
		}
	}
	return "custom"
}

// deriveSimple reads a stored policy back into the two questions.
func deriveSimple(role string, pol db.AccessPolicy, profiles []db.AccessProfile, scopes map[uuid.UUID]db.DeviceScope) simpleAccess {
	s := simpleAccess{OK: true}
	grants := pol.Grants
	baseAllow := pol.Base != "deny" && role != "owner"
	if baseAllow {
		// Allow rules that only repeat what the role already has (a super op's
		// "remote control everywhere") change nothing.
		has := map[string]bool{}
		for _, a := range roleActions(role) {
			has[a] = true
		}
		for _, g := range grants {
			if g.Effect == "deny" {
				return simpleAccess{Why: "They start from everything, with exceptions."}
			}
			for _, a := range g.Actions {
				if !has[a] && (a == "*" || accessActionByKey[a].Sensitive) {
					return simpleAccess{Why: "They start from everything, with " + lowerFirst(accessActionByKey[a].Label) + " added on top."}
				}
			}
		}
		s.Every, s.Do = true, "role"
		return s
	}
	if len(grants) == 0 {
		s.Do = lookOnly(profiles)
		if s.Do == "custom" {
			s.Actions = []string{"view"}
		}
		return s
	}
	first := grants[0]
	var profile *uuid.UUID = first.ProfileID
	rests, groups := placeCounts(scopes)
	for _, g := range grants {
		switch {
		case g.Effect != "allow":
			return simpleAccess{Why: "They have exceptions (deny rules)."}
		case !sameActions(g.Actions, first.Actions):
			return simpleAccess{Why: "They can do different things in different places."}
		case (g.ExpiresAt == nil) != (first.ExpiresAt == nil) || (g.ExpiresAt != nil && g.ExpiresAt.Sub(*first.ExpiresAt).Abs() > time.Minute):
			return simpleAccess{Why: "Their rules end at different times."}
		}
		if (g.ProfileID == nil) != (profile == nil) || (g.ProfileID != nil && *g.ProfileID != *profile) {
			profile = nil
		}
		if g.ScopeType == "all" {
			if len(grants) > 1 {
				return simpleAccess{Why: "They have a rule for every device and more rules besides."}
			}
			s.Every = true
			continue
		}
		s.Places = append(s.Places, placeOption(g, rests, groups))
	}
	s.Expires, s.Note = first.ExpiresAt, first.Note
	for _, p := range profiles {
		if profile != nil && p.ID == *profile && sameActions(p.Actions, first.Actions) {
			s.Do = "profile:" + p.ID.String()
			return s
		}
	}
	for _, p := range profiles {
		if sameActions(p.Actions, first.Actions) {
			s.Do = "profile:" + p.ID.String()
			return s
		}
	}
	if role != "owner" && sameActions(first.Actions, roleActions(role)) {
		s.Do = "role"
		return s
	}
	s.Do, s.Actions = "custom", first.Actions
	return s
}

// profileFits reports whether every action in the profile is within the role.
func profileFits(p db.AccessProfile, role string) bool {
	c := roleCeiling(role)
	for _, a := range p.Actions {
		if c != nil && !c[a] {
			return false
		}
	}
	return true
}

// simpleProfileView is one choice under "what can they do there?".
type simpleProfileView struct {
	Value, Name, Desc string
	Fits              bool
}

func (h *Handler) simpleChoices(role string, profiles []db.AccessProfile) []simpleProfileView {
	var out []simpleProfileView
	for _, p := range profiles {
		out = append(out, simpleProfileView{Value: "profile:" + p.ID.String(), Name: p.Name, Desc: p.Description, Fits: profileFits(p, role)})
	}
	if role != "owner" {
		out = append(out, simpleProfileView{Value: "role", Name: "Everything " + roleArticle(role) + " can", Desc: roleEverything(role), Fits: true})
	}
	return out
}

func roleEverything(role string) string {
	switch role {
	case "super_op":
		return "Every action, including remote control and firmware updates."
	case "user_manager":
		return "Every action, including remote control."
	case "viewer":
		return "See devices and take screenshots."
	}
	return "Every action except remote control, which needs Custom."
}

// parseSimple turns the form into the policy it describes. r.Form must be parsed.
func (h *Handler) parseSimple(ctx context.Context, r *http.Request, u *db.User) (base string, grants []db.AccessGrant, err error) {
	var actions []string
	var profileID *uuid.UUID
	do := r.FormValue("do")
	switch {
	case do == "role":
		if u.Role == "owner" {
			return "", nil, fmt.Errorf("pick what they can do")
		}
		actions = roleActions(u.Role)
		// The option says "including remote control" in its own words.
		r.Form.Set("confirm_sensitive", "1")
	case strings.HasPrefix(do, "profile:"):
		id, perr := uuid.Parse(strings.TrimPrefix(do, "profile:"))
		if perr != nil {
			return "", nil, fmt.Errorf("unknown profile")
		}
		profiles, _ := h.db.ListAccessProfiles(ctx)
		for _, p := range profiles {
			if p.ID == id {
				if !profileFits(p, u.Role) {
					return "", nil, fmt.Errorf("\"%s\" is more than %s can do", p.Name, roleArticle(u.Role))
				}
				actions, profileID = p.Actions, &id
			}
		}
		if profileID == nil {
			return "", nil, fmt.Errorf("that profile no longer exists")
		}
	default:
		seen := map[string]bool{"view": true}
		actions = []string{"view"} // they have to see a device to act on it
		for _, a := range r.Form["actions"] {
			if isAccessAction(a) && !seen[a] {
				seen[a] = true
				actions = append(actions, a)
			}
		}
	}
	expires, err := parseExpires(r)
	if err != nil {
		return "", nil, err
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > 200 {
		note = note[:200]
	}
	every := r.FormValue("see") == "all"
	if every && do == "role" && expires == nil && u.Role != "owner" {
		return "allow", nil, nil // the plain default: no rules at all
	}
	mk := func(typ string, id *uuid.UUID) db.AccessGrant {
		return db.AccessGrant{UserID: u.ID, Effect: "allow", ScopeType: typ, ScopeID: id, Actions: actions, Note: note, ExpiresAt: expires, CreatedBy: h.currentUsername(r), ProfileID: profileID}
	}
	if every {
		return "deny", []db.AccessGrant{mk("all", nil)}, nil
	}
	seen := map[string]bool{}
	for _, s := range r.Form["scopes"] {
		parts := strings.SplitN(s, ":", 2)
		if len(parts) != 2 || seen[s] || (parts[0] != "restaurant" && parts[0] != "group" && parts[0] != "device") {
			continue
		}
		id, perr := uuid.Parse(parts[1])
		if perr != nil {
			continue
		}
		seen[s] = true
		grants = append(grants, mk(parts[0], &id))
	}
	return "deny", grants, nil
}

// checkSimple applies the delegation rules to a whole new setup: starting from
// everything, or dropping an exception, widens it; every rule must be one the actor
// could grant by hand.
func (h *Handler) checkSimple(r *http.Request, u *db.User, base string, grants []db.AccessGrant) error {
	if base == "allow" {
		if err := h.canWiden(r, u); err != nil {
			return err
		}
	}
	if cur, err := h.db.GetUserAccess(r.Context(), u.Username); err == nil {
		for _, g := range cur.Grants {
			if g.Effect == "deny" {
				if err := h.canWiden(r, u); err != nil {
					return err
				}
				break
			}
		}
	}
	for _, g := range grants {
		if err := h.checkGrantAllowed(r, u, g); err != nil {
			return err
		}
	}
	return nil
}

// UserAccessSimplePreview answers the live preview: how many devices they would see,
// what they could do, and whether the actor may save it (JSON).
func (h *Handler) UserAccessSimplePreview(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	r.ParseForm()
	resp := map[string]any{}
	base, grants, err := h.parseSimple(ctx, r, u)
	if err != nil {
		resp["error"] = err.Error()
	} else {
		if err := h.checkSimple(r, u, base, grants); err != nil {
			resp["error"] = err.Error()
		}
		scopes, _ := h.db.DeviceScopes(ctx)
		a := &access{h: h, ctx: ctx, role: u.Role, pol: db.AccessPolicy{Base: base, Grants: grants}, scopes: scopes}
		a.once.Do(func() {})
		var vis []uuid.UUID
		for id := range scopes {
			if a.canDevice("view", id) {
				vis = append(vis, id)
			}
		}
		var can, cannot []string
		for _, act := range grantableActions(u.Role) {
			if act.Key == "view" {
				continue
			}
			yes := false
			if act.Fleet {
				yes = a.can(act.Key, nil)
			} else {
				for _, id := range vis {
					if a.canDevice(act.Key, id) {
						yes = true
						break
					}
				}
			}
			if yes {
				can = append(can, lowerFirst(act.Label))
			} else {
				cannot = append(cannot, lowerFirst(act.Label))
			}
		}
		resp["devices"], resp["total"], resp["can"], resp["cannot"] = len(vis), len(scopes), can, cannot
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// UserAccessSimpleSave replaces the user's access with the two answers.
func (h *Handler) UserAccessSimpleSave(w http.ResponseWriter, r *http.Request) {
	u, ok := h.loadAccessTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	r.ParseForm()
	base, grants, err := h.parseSimple(ctx, r, u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.checkSimple(r, u, base, grants); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := h.db.ReplaceUserAccess(ctx, u.ID, base, grants); err != nil {
		log.Printf("[access] simple save for %s: %v", u.Username, err)
		http.Error(w, "Could not save (does the restaurant, group or device still exist?)", http.StatusBadRequest)
		return
	}
	invalidatePolicy(u.Username)
	detail := "every device, everything the role can"
	if base == "deny" {
		var parts []string
		for _, g := range grants {
			parts = append(parts, describeGrant(g))
		}
		detail = "only: " + strings.Join(parts, "; ")
		if len(grants) == 0 {
			detail = "no devices"
		}
	}
	h.audit(r, "user.access.set", u.Username, detail)
	h.hxDoneToast(w, r, "/users/"+u.ID.String()+"/manage", "Access saved", "ok")
}

// ── Access profiles: /users/access/profiles ─────────────────────────────────

// canEditProfiles: changing a profile changes the access of everyone on it, so only
// someone who could give any of them anything may — the super admin, or an account
// whose own access is unlimited (see canWiden).
func (h *Handler) canEditProfiles(r *http.Request) bool {
	if h.role(r) == "admin" {
		return true
	}
	if !roleManagesUsers(h.role(r)) {
		return false
	}
	a := h.accessFor(r.Context(), h.role(r), h.currentUsername(r))
	if a.pol.Base == "deny" {
		return false
	}
	for _, g := range a.pol.Grants {
		if g.Effect == "deny" {
			return false
		}
	}
	return true
}

// profileActions is the part of the catalogue a profile can hold: the ordinary
// actions. Remote control, shell and OTA are named per person, never in a profile.
func profileActions() []accessAction {
	var out []accessAction
	for _, a := range accessActions {
		if !a.Sensitive {
			out = append(out, a)
		}
	}
	return out
}

func (h *Handler) AccessProfilesPage(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.db.ListAccessProfiles(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	type row struct {
		db.AccessProfile
		Labels []string
		Keys   string
		People []string
	}
	names := map[string]string{}
	if us, err := h.db.ListUsers(r.Context()); err == nil {
		for _, u := range us {
			names[u.Username] = u.DisplayName()
		}
	}
	var rows []row
	for _, p := range profiles {
		rw := row{AccessProfile: p, Keys: strings.Join(p.Actions, ",")}
		for _, a := range p.Actions {
			if act, ok := accessActionByKey[a]; ok {
				rw.Labels = append(rw.Labels, act.Label)
			}
		}
		for _, u := range p.Users {
			if n := names[u]; n != "" {
				rw.People = append(rw.People, n)
			} else {
				rw.People = append(rw.People, u)
			}
		}
		rows = append(rows, rw)
	}
	h.render(w, r, "access_profiles.html", map[string]any{
		"Title":    "Access profiles",
		"Profiles": rows,
		"Actions":  profileActions(),
		"CanEdit":  h.canEditProfiles(r),
		"UsersTab": "access",
	})
}

// AccessProfileSave creates (no {pid}) or updates a profile.
func (h *Handler) AccessProfileSave(w http.ResponseWriter, r *http.Request) {
	if !h.canEditProfiles(r) {
		http.Error(w, "Changing a profile changes everyone on it, so it takes unlimited access of your own.", http.StatusForbidden)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 60 {
		http.Error(w, "Give the profile a name (up to 60 characters).", http.StatusBadRequest)
		return
	}
	desc := strings.TrimSpace(r.FormValue("description"))
	if len(desc) > 200 {
		desc = desc[:200]
	}
	mine := roleCeiling(h.role(r))
	actions := []string{"view"}
	seen := map[string]bool{"view": true}
	for _, a := range r.Form["actions"] {
		act, ok := accessActionByKey[a]
		if !ok || act.Sensitive || seen[a] {
			continue
		}
		if mine != nil && !mine[a] {
			http.Error(w, fmt.Sprintf("Your role can't grant %s.", lowerFirst(act.Label)), http.StatusForbidden)
			return
		}
		seen[a] = true
		actions = append(actions, a)
	}
	var id *uuid.UUID
	if s := r.PathValue("pid"); s != "" {
		pid, err := uuid.Parse(s)
		if err != nil {
			http.Error(w, "Invalid profile", http.StatusBadRequest)
			return
		}
		id = &pid
	}
	if _, err := h.db.SaveAccessProfile(r.Context(), id, name, desc, actions, h.currentUsername(r)); err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			http.Error(w, "A profile with that name already exists.", http.StatusBadRequest)
			return
		}
		log.Printf("[access] save profile %q: %v", name, err)
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	policyCache.Range(func(k, _ any) bool { policyCache.Delete(k); return true })
	h.audit(r, "access.profile.save", name, strings.Join(actions, ","))
	h.hxDoneToast(w, r, "/users/access/profiles", "Profile saved", "ok")
}

func (h *Handler) AccessProfileDelete(w http.ResponseWriter, r *http.Request) {
	if !h.canEditProfiles(r) {
		http.Error(w, "Only an account with unlimited access can remove a profile.", http.StatusForbidden)
		return
	}
	pid, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid profile", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteAccessProfile(r.Context(), pid); err != nil {
		http.Error(w, "Could not remove", http.StatusInternalServerError)
		return
	}
	h.audit(r, "access.profile.delete", pid.String(), "")
	h.hxDoneToast(w, r, "/users/access/profiles", "Profile removed; the people on it keep their access", "ok")
}
