package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// A person's own Where (picked B, "where on person", 2 Nov 2026; db/access_where.go): on the
// person page, Where replaces the role's default scope for that person (Lab · All · Deployed
// · Groups…), Elsewhere says what they get on the other devices (View only or Hidden), and
// "Copy from…" takes another person's. Their role still decides what they may do.

// effectiveGrants is a person's grants as the evaluator sees them, with scope names: their
// role's (or the ones their Where stands for, SubjectType "where") and their own. w is their
// Where, nil when they follow the role.
func (h *Handler) effectiveGrants(ctx context.Context, role, username string) ([]db.AccessLevel, *db.AccessWhere) {
	roleGs, _ := h.db.RoleAccessLevels(ctx, role)
	own, _ := h.db.UserAccessLevels(ctx, username)
	w, _ := h.db.AccessWhereFor(ctx, username)
	gs := db.EffectiveLevels(append(append([]db.AccessLevel(nil), roleGs...), own...), w, time.Now())
	if w != nil {
		names := h.groupNames(ctx)
		for i := range gs {
			if gs[i].SubjectType == "where" && gs[i].ScopeID != nil {
				gs[i].ScopeName = names[*gs[i].ScopeID]
			}
		}
	}
	return gs, w
}

func (h *Handler) groupNames(ctx context.Context) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	if gs, err := h.db.ListGroups(ctx); err == nil {
		for _, g := range gs {
			out[g.ID] = g.Name
		}
	}
	return out
}

// whereChoice is the person page's Where form, filled from their Where.
type whereChoice struct {
	Set       bool
	Scope     string // lab | all | deployed | groups
	Groups    map[string]bool
	Elsewhere string // view | hidden
	Text      string // "the QA group; elsewhere view only"
	SetBy     string
	SetAt     time.Time
}

func whereChoiceOf(w *db.AccessWhere, names map[uuid.UUID]string) whereChoice {
	c := whereChoice{Groups: map[string]bool{}, Elsewhere: "view"}
	if w == nil {
		return c
	}
	c.Set, c.Scope, c.Elsewhere, c.SetBy, c.SetAt = true, w.Scope, w.Elsewhere, w.SetBy, w.SetAt
	for _, id := range w.GroupIDs {
		c.Groups[id.String()] = true
	}
	c.Text = whereText(w, names)
	return c
}

// whereText names a Where as people read it: "Lab devices", "the QA and POS groups".
func whereText(w *db.AccessWhere, names map[uuid.UUID]string) string {
	switch w.Scope {
	case "all":
		return "every device"
	case "lab":
		return "Lab devices"
	case "deployed":
		return "deployed devices (at a venue)"
	}
	var ns []string
	for _, id := range w.GroupIDs {
		if n := names[id]; n != "" {
			ns = append(ns, n)
		} else {
			ns = append(ns, "a removed group")
		}
	}
	if len(ns) == 1 {
		return "the " + ns[0] + " group"
	}
	return "the " + joinAnd(ns) + " groups"
}

// AccessWhereSave: POST /users/{id}/access-where — action set (scope, groups, elsewhere),
// clear (back to the role's defaults) or copy (from: another person's username).
func (h *Handler) AccessWhereSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.ParseForm()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	u, err := h.db.GetUser(ctx, id)
	if err != nil || u == nil {
		h.notFoundPage(w, r, "Person", "There is no account at this address.")
		return
	}
	back := safeBack(r.FormValue("back"), "/users/"+u.ID.String()+"/profile")
	if roleIsCustomer(u.Role) || u.Role == "admin" {
		h.hxDoneToast(w, r, back, "Where is for team members below super admin", "error")
		return
	}
	names := h.groupNames(ctx)
	action := r.FormValue("action")
	if action == "" && r.FormValue("scope") == "" {
		action = "clear" // "Role default"
	}
	switch action {
	case "clear":
		if err := h.db.ClearAccessWhere(ctx, u.Username); err != nil {
			h.hxDoneToast(w, r, back, "Could not save", "error")
			return
		}
		invalidateLevels()
		h.audit(r, "access.where", u.Username, "back to the "+roleLabel(u.Role)+" defaults")
		h.hxDoneToast(w, r, back, "Back to the role's defaults", "success")
		return
	case "copy":
		from := strings.TrimSpace(r.FormValue("from"))
		src, err := h.db.GetUserByUsername(ctx, from)
		if err != nil || src == nil || from == u.Username {
			h.hxDoneToast(w, r, back, "Pick someone to copy from", "error")
			return
		}
		sw, err := h.db.AccessWhereFor(ctx, src.Username)
		if err != nil {
			h.hxDoneToast(w, r, back, "Could not read their Where", "error")
			return
		}
		if sw == nil {
			err = h.db.ClearAccessWhere(ctx, u.Username)
		} else {
			err = h.db.SetAccessWhere(ctx, db.AccessWhere{Username: u.Username, Scope: sw.Scope, GroupIDs: sw.GroupIDs,
				Elsewhere: sw.Elsewhere, SetBy: h.currentUsername(r)})
		}
		if err != nil {
			h.hxDoneToast(w, r, back, "Could not save", "error")
			return
		}
		invalidateLevels()
		detail := "copied from " + src.Username + ": role defaults"
		if sw != nil {
			detail = "copied from " + src.Username + ": " + whereText(sw, names) + ", elsewhere " + sw.Elsewhere
		}
		h.audit(r, "access.where", u.Username, detail)
		h.hxDoneToast(w, r, back, "Copied from "+src.DisplayName(), "success")
		return
	}
	nw := db.AccessWhere{Username: u.Username, Scope: r.FormValue("scope"), Elsewhere: r.FormValue("elsewhere"), SetBy: h.currentUsername(r)}
	if !db.ValidWhereScope(nw.Scope) {
		h.hxDoneToast(w, r, back, "Pick where", "error")
		return
	}
	if nw.Scope == "groups" {
		for _, v := range r.Form["groups"] {
			gid, err := uuid.Parse(v)
			if err != nil || names[gid] == "" {
				continue
			}
			nw.GroupIDs = append(nw.GroupIDs, gid)
		}
		if len(nw.GroupIDs) == 0 {
			h.hxDoneToast(w, r, back, "Pick at least one group", "error")
			return
		}
	}
	if err := h.db.SetAccessWhere(ctx, nw); err != nil {
		h.hxDoneToast(w, r, back, "Could not save", "error")
		return
	}
	invalidateLevels()
	if nw.Elsewhere != "hidden" {
		nw.Elsewhere = "view"
	}
	h.audit(r, "access.where", u.Username, whereText(&nw, names)+", elsewhere "+nw.Elsewhere)
	h.hxDoneToast(w, r, back, "Where saved", "success")
}

// whereSentence is the person page's sentence: what they can do, and that the rest is
// hidden when Elsewhere is Hidden.
func whereSentence(first string, gs []db.AccessLevel, w *db.AccessWhere, now time.Time) string {
	s := accessSentence(first, pronounFor(first), gs, now)
	if w == nil || w.Elsewhere != "hidden" {
		return s
	}
	for _, g := range gs {
		if g.ScopeType == "all" && !g.Expired(now) {
			return s // their own grant shows them everything anyway
		}
	}
	return s + " Other devices are hidden from " + first + "."
}

// roleWhereText is the role's own Where, for "Role default (…)": the scopes of its
// highest level, "Lab devices and the QA group" for engineers.
func roleWhereText(gs []db.AccessLevel, now time.Time) (where string, level int) {
	var top []db.AccessLevel
	for _, g := range gs {
		if g.SubjectType != "role" || g.Expired(now) {
			continue
		}
		switch {
		case g.Rank() > level:
			level, top = g.Rank(), []db.AccessLevel{g}
		case g.Rank() == level:
			top = append(top, g)
		}
	}
	var ts []string
	for _, g := range top {
		ts = append(ts, scopeText(g))
	}
	return joinAnd(ts), level
}
