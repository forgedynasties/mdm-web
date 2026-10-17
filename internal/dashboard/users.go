package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"

	"mdm/internal/db"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ── Users ─────────────────────────────────────────────────────────────────────

func (h *Handler) UserList(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	admins, operators, viewers := 0, 0, 0
	for _, u := range users {
		switch u.Role {
		case "admin":
			admins++
		case "operator":
			operators++
		default:
			viewers++
		}
	}
	// The old built-in login (actor "admin") has no users row, so its history shows up
	// as "unlinked" here, to be merged into a person (the existing Merge on this page).
	summaries, _ := h.db.ActorSummaries(r.Context())
	orphans, _ := h.db.ListOrphanActors(r.Context())
	grantCounts, _ := h.db.CountAccessGrants(r.Context(), sensitiveActionKeys)
	// People who signed up but can't sign in yet (email not verified). Only the super
	// admin can vouch for them, so only the super admin sees them singled out.
	var waiting []db.User
	if h.role(r) == "admin" {
		for _, u := range users {
			if u.Email != nil && u.EmailVerifiedAt == nil {
				waiting = append(waiting, u)
			}
		}
	}
	h.render(w, r, "users.html", map[string]any{
		"Waiting":     waiting,
		"Users":       users,
		"Orphans":     orphans,
		"Summaries":   summaries,
		"Grants":      grantCounts,
		"Roles":       roleOrder,
		"UsersTab":    "people",
		"Assignable":  assignableRoles(h.role(r)),
		"Restaurants": func() []db.Restaurant { rs, _ := h.db.ListRestaurants(r.Context()); return rs }(),
		"Admins":      admins,
		"Operators":   operators,
		"Viewers":     viewers,
	})
}

// validUserRole reports whether role is a role an admin may assign to a DB user.
// "admin" is excluded — it is env-configured only, never a DB user.
func validUserRole(role string) bool {
	_, ok := roleLevels[role]
	return ok
}

func (h *Handler) UserCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The account's identity is its email — used as both username (login) and
	// email (forgot-password), same as self-signup. Pre-verified since the admin
	// is vouching for it directly; no confirmation email needed.
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	password := r.FormValue("password")
	role := r.FormValue("role")
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))

	if email == "" || password == "" || !validUserRole(role) {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	if !mayManageUser(h.role(r), "viewer", role) {
		http.Error(w, "You can only create accounts with a role below your own level.", http.StatusForbidden)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	created, err := h.db.CreateUserNamed(r.Context(), email, string(hash), role, &email, true, firstName, lastName)
	if err != nil {
		http.Error(w, "An account with that email already exists, or internal error", http.StatusBadRequest)
		return
	}
	// A new account's default is "everything the role can do". Someone whose own access
	// is limited can't hand that out, so theirs start from "nothing" and get allow rules,
	// which are delegation-checked (30 Sep access audit).
	if role != "owner" && role != "admin" && h.canWiden(r, created) != nil {
		if err := h.db.SetUserAccess(r.Context(), created.ID, db.AccessPolicy{Base: "deny"}); err != nil {
			log.Printf("[users] starting policy for %s: %v", created.Username, err)
		}
	}
	// A restaurant owner sees exactly their venue(s): base deny, allow "view"
	// per selected restaurant, out-of-scope devices hidden.
	if role == "owner" {
		pol := db.AccessPolicy{Base: "deny", HideOutOfScope: true}
		if err := h.db.SetUserAccess(r.Context(), created.ID, pol); err != nil {
			log.Printf("[users] owner policy for %s: %v", created.Username, err)
		}
		for _, rid := range r.Form["restaurants"] {
			id, err := uuid.Parse(rid)
			if err != nil {
				continue
			}
			g := db.AccessGrant{UserID: created.ID, Effect: "allow", ScopeType: "restaurant", ScopeID: &id, Actions: []string{"view"}, Note: "owner's venue", CreatedBy: h.currentUsername(r)}
			if saved, err := h.db.AddAccessGrant(r.Context(), g); err != nil {
				log.Printf("[users] owner venue grant for %s: %v", created.Username, err)
			} else {
				h.audit(r, "user.access.grant", created.Username, describeGrant(*saved))
			}
		}
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserSetName lets an admin set/correct a user's first/last name — e.g. for
// accounts created before names existed, or to fix a typo the user can't self-serve.
func (h *Handler) UserSetName(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	r.ParseForm()
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))
	if err := h.db.UpdateUserName(r.Context(), id, firstName, lastName); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.usersRedirect(w, r)
}

func (h *Handler) UserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	if err := h.db.DeleteUser(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// UserSetRole changes an existing user's role. Admin is never assignable from
// the UI (the admin account comes from env), matching UserCreate.
func (h *Handler) UserSetRole(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	role := r.FormValue("role")
	if !validUserRole(role) {
		http.Error(w, "Invalid role", http.StatusBadRequest)
		return
	}
	if !mayManageUser(h.role(r), target.Role, role) {
		http.Error(w, "You can only assign roles below your own level.", http.StatusForbidden)
		return
	}
	// A role with more powers widens whatever their policy allows; with a base of
	// "everything" that is the whole fleet, which only someone who holds it may give.
	if widens(target.Role, role) {
		if err := h.canWiden(r, &db.User{Role: role}); err != nil {
			if pol, perr := h.db.GetUserAccess(r.Context(), target.Username); perr != nil || pol.Base != "deny" {
				http.Error(w, "Can't raise this role: "+err.Error()+". Set their access to start from nothing first.", http.StatusForbidden)
				return
			}
		}
	}
	if err := h.db.UpdateUserRole(r.Context(), id, role); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Rules the new role makes pointless go with the change: left behind, an
	// operator's "remote control" grant kept showing on a super op's page as if it
	// still added something. Also runs when the role is saved unchanged, which is
	// how an account promoted before this existed gets tidied.
	if pol, err := h.db.GetUserAccess(r.Context(), target.Username); err == nil {
		for _, g := range pol.Grants {
			if grantRedundant(role, pol.Base, g) {
				if err := h.db.DeleteAccessGrant(r.Context(), g.ID); err == nil {
					h.audit(r, "user.access.grant_redundant", target.Username, grantSentence(g))
				}
			}
		}
	}
	invalidatePolicy(target.Username)
	h.usersRedirect(w, r)
}

// widens reports whether moving from one role to another adds any power.
func widens(from, to string) bool {
	a, b := roleCeiling(from), roleCeiling(to)
	if b == nil {
		return a != nil
	}
	if a == nil {
		return false
	}
	for k := range b {
		if !a[k] {
			return true
		}
	}
	return false
}

// UserSetPassword resets a team user's password (admin-only). The built-in admin
// account's password comes from the environment and can't be changed here.
func (h *Handler) UserSetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil || target == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageUser(h.role(r), target.Role, "") {
		http.Error(w, "You can only manage accounts below your own level.", http.StatusForbidden)
		return
	}
	password := r.FormValue("password")
	if len(password) < 6 {
		http.Error(w, "Password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserPassword(r.Context(), id, string(hash)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.password_reset", id.String(), "")
	h.usersRedirect(w, r)
}
