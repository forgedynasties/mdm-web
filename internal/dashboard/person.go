package dashboard

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Person page (U1, access by levels, picked 2 Nov 2026): who they are, what they can do in
// sentences, their role's access and their own (with "Give access"), the account controls,
// and for a super admin their recent activity. Replaces I1's two-question editor.

type personGrant struct {
	db.AccessLevel
	From  string // "Engineer" (their role) or "" (their own or their Where)
	Where bool   // stands for their own Where (access_where.go)
}

func (h *Handler) renderPerson(w http.ResponseWriter, r *http.Request, u *db.User) {
	ctx := r.Context()
	now := time.Now()
	var grants []personGrant
	var where *db.AccessWhere
	roleWhere, roleLevel := "", db.LevelNone
	sentence := ""
	first := u.FirstName
	if first == "" {
		first = u.DisplayName()
	}
	switch {
	case u.Role == "admin":
		sentence = first + " is a super admin and can do everything, firmware updates and the shell included."
	case roleIsCustomer(u.Role):
		var venues []string
		if pol, err := h.db.GetUserAccess(ctx, u.Username); err == nil {
			for _, g := range pol.Grants {
				if g.Effect == "allow" && g.ScopeType == "restaurant" && g.ScopeName != "" {
					venues = append(venues, g.ScopeName)
				}
			}
		}
		if len(venues) == 0 {
			sentence = first + " is " + roleArticle(u.Role) + " with no venue yet."
		} else {
			sentence = first + " is " + roleArticle(u.Role) + " at " + joinAnd(venues) + ", and sees only that."
		}
	default:
		var all []db.AccessLevel
		all, where = h.effectiveGrants(ctx, u.Role, u.Username)
		for _, g := range all {
			switch g.SubjectType {
			case "role":
				grants = append(grants, personGrant{AccessLevel: g, From: roleLabel(u.Role)})
			case "where":
				grants = append(grants, personGrant{AccessLevel: g, Where: true})
			default:
				grants = append(grants, personGrant{AccessLevel: g})
			}
		}
		sentence = whereSentence(first, all, where, now)
		roleGs, _ := h.db.RoleAccessLevels(ctx, u.Role)
		roleWhere, roleLevel = roleWhereText(roleGs, now)
	}
	var invited *time.Time
	if inv, err := h.db.PendingInvites(ctx); err == nil {
		if at, ok := inv[u.ID]; ok {
			invited = &at
		}
	}
	stats, _ := h.db.UserStats(ctx, u.Username, legacyAdminActor)
	var recent []db.AuditEntry
	seeActivity := h.role(r) == "admin"
	if seeActivity {
		recent, _ = h.db.ListAuditFiltered(ctx, u.Username, 12)
	}
	groups, _ := h.db.ListGroups(ctx)
	gnames := map[uuid.UUID]string{}
	for _, g := range groups {
		gnames[g.ID] = g.Name
	}
	// "Copy from…" offers the other team members who can hold a Where.
	var copyFrom []db.User
	if us, err := h.db.ListUsers(ctx); err == nil {
		for _, x := range us {
			if x.ID != u.ID && x.Role != "admin" && !roleIsCustomer(x.Role) {
				copyFrom = append(copyFrom, x)
			}
		}
	}
	rests, _ := h.db.ListRestaurants(ctx)
	h.render(w, r, "person.html", map[string]any{
		"Title":          u.DisplayName(),
		"UsersTab":       "people",
		"User":           u,
		"Bubble":         userBubbleFn(u.Username),
		"Sentence":       sentence,
		"Grants":         grants,
		"CanGive":        u.Role != "admin" && !roleIsCustomer(u.Role),
		"Where":          whereChoiceOf(where, gnames),
		"CopyFrom":       copyFrom,
		"RoleWhere":      roleWhere,
		"RoleVerb":       levelVerb(roleLevel),
		"Groups":         groups,
		"Venues":         rests,
		"Now":            now,
		"Invited":        invited,
		"Stats":          stats,
		"Recent":         recent,
		"CanSeeActivity": seeActivity,
		"Assignable":     assignableRoles(h.role(r)),
		"CanEditAvatar":  h.mayEditAvatar(r, u),
		"CanDelete":      u.Username != h.currentUsername(r) && roleCreatesUsers(h.role(r)),
		"IsSuperAdmin":   h.role(r) == "admin",
		"Back":           "/users/" + u.ID.String() + "/profile",
		"FirstName":      first,
	})
}

// pronounFor is the second sentence's subject: we don't guess anyone's pronouns.
func pronounFor(first string) string {
	if first == "" {
		return "They"
	}
	return first
}
