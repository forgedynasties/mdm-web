package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Access by levels: the pages and helpers (picked 2 Nov 2026: L3 · G3 · R1 · U1 · A1 · V1).
// The evaluator is in access.go (deviceLevel); the grants are db/access_levels.go.
//
//   Access page (/users/access, A1): one card per role (its grants, "+ Give … access") and
//   "Who can act on" a group or venue.
//   Person page: their access in sentences, their role's grants and their own, "Give access".
//   Fleet rail (R1): a lock on each group or venue the viewer can only look at.
//   Device page: a view-only device says why and who can operate it.

// accessRoles are the team roles that hold grants on the Access page, in ladder order.
// The super admin manages everything and has no card; customers keep their venue walls.
func accessRoles() []string {
	var out []string
	for _, d := range roleDefs {
		if d.Customer || d.Key == "admin" {
			continue
		}
		out = append(out, d.Key)
	}
	return out
}

// scopeText names a grant's scope as people read it.
func scopeText(g db.AccessLevel) string {
	switch g.ScopeType {
	case "all":
		return "every device"
	case "state":
		switch g.State {
		case db.StateLab:
			return "Lab devices"
		case db.StateSettingUp:
			return "devices being set up"
		case db.StateInService:
			return "devices in service"
		}
	case "group":
		if g.ScopeName != "" {
			return "the " + g.ScopeName + " group"
		}
		return "a removed group"
	case "venue":
		if g.ScopeName != "" {
			return g.ScopeName
		}
		return "a removed venue"
	case "visit":
		return "the venues of their setup visits"
	}
	return "—"
}

// levelVerb is what a level lets someone do, as a verb: "look at", "operate", "manage".
func levelVerb(l int) string {
	switch l {
	case db.LevelView:
		return "look at"
	case db.LevelOperate:
		return "operate"
	case db.LevelManage:
		return "manage"
	}
	return ""
}

// accessSentence says what grants give, highest level first: "Engineers can operate Lab
// devices and the QA group, and look at every other device." subject starts the sentence
// ("Engineers", "Abdullah", "You"); they is "They" or "You" (plural verb) or the person's name.
func accessSentence(subject, they string, gs []db.AccessLevel, now time.Time) string {
	by := map[int][]string{}
	seen := map[string]bool{}
	for _, g := range gs {
		if g.Expired(now) {
			continue
		}
		t := scopeText(g)
		if g.Until != nil {
			t += " (until " + g.Until.Local().Format("2 Jan") + ")"
		}
		if k := fmt.Sprint(g.Rank(), t); !seen[k] {
			seen[k] = true
			by[g.Rank()] = append(by[g.Rank()], t)
		}
	}
	var parts []string
	for _, l := range []int{db.LevelManage, db.LevelOperate, db.LevelView} {
		items := by[l]
		if len(items) == 0 {
			continue
		}
		if len(parts) > 0 {
			for i, t := range items {
				if t == "every device" {
					items[i] = "every other device"
				}
			}
		}
		parts = append(parts, levelVerb(l)+" "+joinAnd(items))
	}
	if len(parts) == 0 {
		if they == "They" || they == "You" {
			return subject + " have no access to devices yet."
		}
		return subject + " has no access to devices yet."
	}
	s := parts[0]
	for i, p := range parts[1:] {
		sep := ", "
		if i == len(parts)-2 {
			sep = " and "
			if strings.Contains(s, " and ") {
				sep = ", and "
			}
		}
		s += sep + p
	}
	return subject + " can " + s + "."
}

// ── The device page: why it is view-only, who can operate it ─────────────────

type lookView struct {
	Why  string   // "Engineers operate Lab devices and the QA group; this device is in Software."
	Lock string   // the short reason on locked controls
	Who  []string // who can operate it
}

// lookViewFor explains a view-only device to the person looking at it.
func (h *Handler) lookViewFor(ctx context.Context, acc *access, dev *db.Device) lookView {
	now := time.Now()
	var op, mine, own []string
	gs, w := h.effectiveGrants(ctx, acc.role, acc.username)
	for _, g := range gs {
		if g.Expired(now) || g.Rank() < db.LevelOperate {
			continue
		}
		switch g.SubjectType {
		case "role":
			op = append(op, scopeText(g))
		case "where":
			mine = append(mine, scopeText(g))
		default:
			own = append(own, scopeText(g))
		}
	}
	where := "at no venue (Lab)"
	if dev.RestaurantName != "" {
		where = "at " + dev.RestaurantName
	}
	if names := h.deviceGroupNames(ctx, dev.ID); len(names) > 0 {
		where += ", in " + joinAnd(names)
	}
	var why string
	switch {
	case w != nil && len(mine) > 0:
		why = "You operate " + joinAnd(mine) + " (set for you)"
	case w != nil:
		why = "You look at devices but operate none"
	case len(op) > 0:
		why = rolePlural(acc.role) + " operate " + joinAnd(op)
	default:
		why = rolePlural(acc.role) + " look at devices but operate none"
	}
	if len(own) > 0 {
		why += "; you also operate " + joinAnd(own)
	}
	why += ". This device is " + where + "."
	return lookView{Why: why, Lock: "View only here · screenshots and logs are fine", Who: h.whoCanOperate(ctx, dev)}
}

// deviceGroupNames are the names of the groups a device is in.
func (h *Handler) deviceGroupNames(ctx context.Context, dev uuid.UUID) []string {
	byDev, err := h.db.GroupNamesByDevice(ctx)
	if err != nil {
		return nil
	}
	names := byDev[dev]
	sort.Strings(names)
	return names
}

// whoCanOperate lists who may operate the device: super admins, each role and person whose
// grant reaches it with Operate or more.
func (h *Handler) whoCanOperate(ctx context.Context, dev *db.Device) []string {
	scopes, err := h.db.DeviceScopes(ctx)
	if err != nil {
		return nil
	}
	sc := scopes[dev.ID]
	all, err := h.db.ListAccessLevels(ctx)
	if err != nil {
		return nil
	}
	users, _ := h.db.ListUsers(ctx)
	name := map[string]string{}
	for _, u := range users {
		name[u.Username] = u.DisplayName()
	}
	now := time.Now()
	out := []string{"Super admins"}
	seen := map[string]bool{}
	for _, g := range all {
		if g.Expired(now) || g.Rank() < db.LevelOperate || !levelCovers(g, sc) {
			continue
		}
		who := rolePlural(g.Subject)
		if g.SubjectType == "user" {
			who = name[g.Subject]
			if who == "" {
				who = g.Subject
			}
		}
		line := who + " · " + strings.ToLower(levelWord(g.Rank())) + " (" + scopeText(g) + ")"
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	// People with their own Where: what it stands for (their role's level on its scope).
	if wheres, err := h.db.ListAccessWhere(ctx); err == nil && len(wheres) > 0 {
		roleRows := map[string][]db.AccessLevel{}
		for _, g := range all {
			if g.SubjectType == "role" {
				roleRows[g.Subject] = append(roleRows[g.Subject], g)
			}
		}
		gnames := h.groupNames(ctx)
		for _, u := range users {
			wh, ok := wheres[u.Username]
			if !ok || u.Role == "admin" || roleIsCustomer(u.Role) {
				continue
			}
			for _, g := range db.EffectiveLevels(roleRows[u.Role], &wh, now) {
				if g.Rank() < db.LevelOperate || !levelCovers(g, sc) {
					continue
				}
				if g.ScopeID != nil {
					g.ScopeName = gnames[*g.ScopeID]
				}
				line := u.DisplayName() + " · " + strings.ToLower(levelWord(g.Rank())) + " (" + scopeText(g) + ", set for them)"
				if !seen[line] {
					seen[line] = true
					out = append(out, line)
				}
			}
		}
	}
	return out
}

// levelCovers is grantCoversDevice without a request: a "visit" grant (Field) counts for
// any placed device, since whose visit it is depends on the person.
func levelCovers(g db.AccessLevel, sc db.DeviceScope) bool {
	switch g.ScopeType {
	case "all":
		return true
	case "state":
		return g.State == sc.State()
	case "group":
		for _, id := range sc.Groups {
			if g.ScopeID != nil && id == *g.ScopeID {
				return true
			}
		}
	case "venue":
		return g.ScopeID != nil && sc.RestaurantID != nil && *sc.RestaurantID == *g.ScopeID
	case "visit":
		return sc.RestaurantID != nil
	}
	return false
}

// ── Fleet rail and rows (R1) ────────────────────────────────────────────────

// collectionLocks are the venues and groups the viewer can see but only look at (no
// device in them they may operate): the rail draws a lock on each.
func (a *access) collectionLocks() (venues, groups map[string]bool) {
	venues, groups = map[string]bool{}, map[string]bool{}
	if a.unrestricted() || roleIsCustomer(a.role) {
		return
	}
	a.loadScopes()
	seeV, actV := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	seeG, actG := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for id, sc := range a.scopes {
		l, _ := a.deviceLevel(id)
		if l < db.LevelView {
			continue
		}
		if sc.RestaurantID != nil {
			seeV[*sc.RestaurantID] = true
			if l >= db.LevelOperate {
				actV[*sc.RestaurantID] = true
			}
		}
		for _, g := range sc.Groups {
			seeG[g] = true
			if l >= db.LevelOperate {
				actG[g] = true
			}
		}
	}
	for id := range seeV {
		if !actV[id] {
			venues[id.String()] = true
		}
	}
	for id := range seeG {
		if !actG[id] {
			groups[id.String()] = true
		}
	}
	return
}

// restaurantsViewOnly reports whether this person only looks at restaurants: none of their
// grants lets them operate restaurant devices as such (everywhere, a venue, devices being set
// up or in service, their visits). Access through a group that happens to hold a restaurant
// device doesn't count: the rail's Restaurants tab carries the lock and Groups opens first
// (T1, picked 2 Nov 2026: "for engineer the lock icon will be on restaurant tab").
func (a *access) restaurantsViewOnly() bool {
	if a.unrestricted() || roleIsCustomer(a.role) {
		return false
	}
	now := time.Now()
	for _, g := range a.levels() {
		if g.Expired(now) || g.Rank() < db.LevelOperate {
			continue
		}
		switch g.ScopeType {
		case "all", "venue", "visit":
			return false
		case "state":
			if g.State != db.StateLab {
				return false
			}
		}
	}
	return true
}

// markViewOnly sets ViewOnly on the devices the viewer can only look at, for the rows'
// data-ro (the right-click menu shows their actions as read only).
func (a *access) markViewOnly(devs []db.Device) {
	if a.unrestricted() || roleIsCustomer(a.role) {
		return
	}
	for i := range devs {
		if l, _ := a.deviceLevel(devs[i].ID); l < db.LevelOperate {
			devs[i].ViewOnly = true
		}
	}
}

// ── Access page (A1) ────────────────────────────────────────────────────────

type accessRoleCard struct {
	Role, Label, Plural string
	People              int
	Legacy              bool
	Grants              []db.AccessLevel
	Sentence            string
}

type whoRow struct {
	Who, Level, Via string
	People          int
}

// AccessPage is /users/access: a card per role and "Who can act on" a group or venue.
func (h *Handler) AccessPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := h.db.ListAccessLevels(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	users, _ := h.db.ListUsers(ctx)
	count := map[string]int{}
	name := map[string]string{}
	for _, u := range users {
		count[u.Role]++
		name[u.Username] = u.DisplayName()
	}
	now := time.Now()
	byRole := map[string][]db.AccessLevel{}
	var own []db.AccessLevel
	for _, g := range all {
		if g.SubjectType == "role" {
			byRole[g.Subject] = append(byRole[g.Subject], g)
		} else {
			own = append(own, g)
		}
	}
	var cards []accessRoleCard
	for _, k := range accessRoles() {
		d := roleByKey[k]
		if d.Legacy && count[k] == 0 {
			continue
		}
		c := accessRoleCard{Role: k, Label: d.Label, Plural: d.Plural, People: count[k], Legacy: d.Legacy, Grants: byRole[k]}
		c.Sentence = accessSentence(d.Plural, "They", c.Grants, now)
		cards = append(cards, c)
	}
	sel := r.URL.Query().Get("role")
	if sel == "" && r.URL.Query().Get("who") == "" && len(cards) > 0 {
		sel = "engineer"
	}
	groups, _ := h.db.ListAllGroups(ctx)
	rests, _ := h.db.ListRestaurants(ctx)

	// Who can act on a group or venue.
	who := r.URL.Query().Get("who")
	var whoName string
	var whoRows []whoRow
	if kind, id, ok := strings.Cut(who, ":"); ok {
		gid, err := uuid.Parse(id)
		if err == nil {
			for _, g := range groups {
				if kind == "group" && g.ID == gid {
					whoName = "the " + g.Name + " group"
				}
			}
			for _, x := range rests {
				if kind == "venue" && x.ID == gid {
					whoName = x.Name
				}
			}
			admins := count["admin"]
			whoRows = append(whoRows, whoRow{Who: "Super admins", Level: "Manage", Via: "everything", People: admins})
			for _, g := range all {
				if g.Expired(now) {
					continue
				}
				hit := g.ScopeType == "all" || g.ScopeType == "state" ||
					(g.ScopeType == kind && g.ScopeID != nil && *g.ScopeID == gid) || (kind == "venue" && g.ScopeType == "visit")
				if !hit {
					continue
				}
				row := whoRow{Level: levelWord(g.Rank()), Via: scopeText(g)}
				if g.ScopeType == "state" || g.ScopeType == "visit" {
					row.Via += " only"
				}
				if g.SubjectType == "role" {
					row.Who, row.People = rolePlural(g.Subject), count[g.Subject]
				} else {
					row.Who, row.People = name[g.Subject], 1
					if row.Who == "" {
						row.Who = g.Subject
					}
				}
				whoRows = append(whoRows, row)
			}
		}
	}
	h.render(w, r, "users_access.html", map[string]any{
		"Title":     "Access",
		"UsersTab":  "access",
		"Cards":     cards,
		"Sel":       sel,
		"Own":       own,
		"Who":       who,
		"WhoName":   whoName,
		"WhoRows":   whoRows,
		"Groups":    groups,
		"Venues":    rests,
		"UserNames": name,
		"Now":       now,
	})
}

// parseScope reads the "where" field: all, state:<state>, visit, group:<id>, venue:<id>.
func parseScope(where string) (scopeType string, id *uuid.UUID, state string, ok bool) {
	kind, rest, _ := strings.Cut(where, ":")
	switch kind {
	case "all", "visit":
		return kind, nil, "", rest == ""
	case "state":
		switch rest {
		case db.StateLab, db.StateSettingUp, db.StateInService:
			return "state", nil, rest, true
		}
	case "group", "venue":
		if u, err := uuid.Parse(rest); err == nil {
			return kind, &u, "", true
		}
	}
	return "", nil, "", false
}

// AccessLevelSet gives a role or a person a level on a scope (POST /users/access/levels:
// subject_type, subject, where, level, until (YYYY-MM-DD, optional), back).
func (h *Handler) AccessLevelSet(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	back := safeBack(r.FormValue("back"), "/users/access")
	st, subject := r.FormValue("subject_type"), strings.TrimSpace(r.FormValue("subject"))
	switch st {
	case "role":
		if d, ok := roleByKey[subject]; !ok || d.Customer || subject == "admin" {
			h.hxDoneToast(w, r, back, "That role can't hold access", "error")
			return
		}
	case "user":
		u, err := h.db.GetUserByUsername(r.Context(), subject)
		if err != nil || u == nil || roleIsCustomer(u.Role) || u.Role == "admin" {
			h.hxDoneToast(w, r, back, "That person can't hold access", "error")
			return
		}
	default:
		h.hxDoneToast(w, r, back, "Pick who gets it", "error")
		return
	}
	scope, id, state, ok := parseScope(r.FormValue("where"))
	if !ok {
		h.hxDoneToast(w, r, back, "Pick where", "error")
		return
	}
	level := r.FormValue("level")
	if db.LevelOf(level) == db.LevelNone {
		h.hxDoneToast(w, r, back, "Pick a level", "error")
		return
	}
	var until *time.Time
	if v := strings.TrimSpace(r.FormValue("until")); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil || !t.After(time.Now()) {
			h.hxDoneToast(w, r, back, "The end date must be a day after today", "error")
			return
		}
		t = t.Add(24*time.Hour - time.Second) // to the end of that day
		until = &t
	}
	g := db.AccessLevel{SubjectType: st, Subject: subject, ScopeType: scope, ScopeID: id, State: state,
		Level: level, Until: until, CreatedBy: h.currentUsername(r)}
	if err := h.db.SetAccessLevel(r.Context(), g); err != nil {
		h.hxDoneToast(w, r, back, "Could not save the access", "error")
		return
	}
	invalidateLevels()
	g.ScopeName = h.scopeName(r.Context(), g)
	detail := level + " on " + scopeText(g)
	if until != nil {
		detail += " until " + until.Format("2006-01-02")
	}
	h.audit(r, "access.give", st+":"+subject, detail)
	h.hxDoneToast(w, r, back, "Access saved", "success")
}

// AccessLevelDelete removes a grant (POST /users/access/levels/{id}/delete).
func (h *Handler) AccessLevelDelete(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	back := safeBack(r.FormValue("back"), "/users/access")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetAccessLevel(r.Context(), id)
	if err != nil {
		h.hxDoneToast(w, r, back, "Already gone", "success")
		return
	}
	if err := h.db.DeleteAccessLevel(r.Context(), id); err != nil {
		h.hxDoneToast(w, r, back, "Could not remove it", "error")
		return
	}
	invalidateLevels()
	h.audit(r, "access.remove", g.SubjectType+":"+g.Subject, g.Level+" on "+scopeText(*g))
	h.hxDoneToast(w, r, back, "Access removed", "success")
}

// scopeName fills a grant's group or venue name.
func (h *Handler) scopeName(ctx context.Context, g db.AccessLevel) string {
	if g.ScopeID == nil {
		return ""
	}
	switch g.ScopeType {
	case "group":
		gs, _ := h.db.ListAllGroups(ctx)
		for _, x := range gs {
			if x.ID == *g.ScopeID {
				return x.Name
			}
		}
	case "venue":
		rs, _ := h.db.ListRestaurants(ctx)
		for _, x := range rs {
			if x.ID == *g.ScopeID {
				return x.Name
			}
		}
	}
	return ""
}

// safeBack keeps a redirect on this site.
func safeBack(v, def string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v
	}
	return def
}

// redirectTo answers with a redirect to a fixed page (old addresses of moved pages).
func redirectTo(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
}
