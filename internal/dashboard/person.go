package dashboard

import (
	"net/http"
	"time"

	"mdm/internal/db"
)

// Person page (U1, access by levels, picked 2 Nov 2026): who they are, what they can do in
// sentences, their role's access and their own (with "Give access"), the account controls,
// and for a super admin their recent activity. Replaces I1's two-question editor.

type personGrant struct {
	db.AccessLevel
	From string // "Engineer" (their role) or "" (their own)
}

func (h *Handler) renderPerson(w http.ResponseWriter, r *http.Request, u *db.User) {
	ctx := r.Context()
	now := time.Now()
	var grants []personGrant
	var mine []db.AccessLevel
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
		roleGs, _ := h.db.RoleAccessLevels(ctx, u.Role)
		mine, _ = h.db.UserAccessLevels(ctx, u.Username)
		for _, g := range roleGs {
			grants = append(grants, personGrant{AccessLevel: g, From: roleLabel(u.Role)})
		}
		for _, g := range mine {
			grants = append(grants, personGrant{AccessLevel: g})
		}
		all := append(append([]db.AccessLevel(nil), roleGs...), mine...)
		sentence = accessSentence(first, pronounFor(first), all, now)
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
	rests, _ := h.db.ListRestaurants(ctx)
	h.render(w, r, "person.html", map[string]any{
		"Title":          u.DisplayName(),
		"UsersTab":       "people",
		"User":           u,
		"Bubble":         userBubbleFn(u.Username),
		"Sentence":       sentence,
		"Grants":         grants,
		"CanGive":        u.Role != "admin" && !roleIsCustomer(u.Role),
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
