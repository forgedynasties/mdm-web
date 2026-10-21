package dashboard

import (
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/mailer"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ── Users ─────────────────────────────────────────────────────────────────────

// userRow is one person on the Users page (U1, picked 20 Oct): a table line with what
// they can reach in words, and for customers their venues.
type userRow struct {
	db.User
	Access    string     // AIO team: "Every device", "3 venues", "Custom rules", …
	Sensitive []string   // sensitive extras their rules add ("remote", "OTA")
	Venues    []venueRef // customers: their venues
	LastAt    *time.Time
	Invited   *time.Time // invite sent and not used yet
	Rules     int        // access rules of their own (allow or deny)
	Activity  string     // p1Activity bucket, for the Activity filter
	CanManage bool
	Rank      int
}

type venueRef struct {
	ID   uuid.UUID
	Name string
}

func (h *Handler) UserList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := h.db.ListUsers(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The old built-in login (actor "admin") has no users row, so its history shows up
	// as "unlinked" here, to be merged into a person (the existing Merge on this page).
	summaries, _ := h.db.ActorSummaries(ctx)
	orphans, _ := h.db.ListOrphanActors(ctx)
	invites, _ := h.db.PendingInvites(ctx)
	profiles, _ := h.db.ListAccessProfiles(ctx)
	actor := h.role(r)
	// People who signed up but can't sign in yet (email not verified). Only the super
	// admin can vouch for them, so only the super admin sees them singled out.
	var waiting []db.User
	var team, customers []userRow
	for _, u := range users {
		if actor == "admin" && u.Email != nil && u.EmailVerifiedAt == nil {
			if _, inv := invites[u.ID]; !inv {
				waiting = append(waiting, u)
				continue
			}
		}
		row := userRow{User: u, Rank: rank(u.Role), CanManage: mayManageUser(actor, u.Role, "")}
		if sm, ok := summaries[u.Username]; ok {
			row.LastAt = sm.LastAt
		}
		if at, ok := invites[u.ID]; ok {
			row.Invited = &at
		}
		pol, _ := h.db.GetUserAccess(ctx, u.Username)
		row.Rules = len(pol.Grants)
		row.Activity = p1Activity(row, time.Now())
		for _, g := range pol.Grants {
			if g.Effect != "allow" {
				continue
			}
			if roleIsCustomer(u.Role) {
				if g.ScopeType == "restaurant" && g.ScopeID != nil && g.Has("view") {
					row.Venues = append(row.Venues, venueRef{*g.ScopeID, g.ScopeName})
				}
				continue
			}
			for _, k := range sensitiveActionKeys {
				if g.Has(k) && !sensitiveByDefault(u.Role, k) && !containsStr(row.Sensitive, accessShort(k)) {
					row.Sensitive = append(row.Sensitive, accessShort(k))
				}
			}
		}
		if roleIsCustomer(u.Role) {
			customers = append(customers, row)
			continue
		}
		row.Access = accessPhrase(u.Role, pol, profiles)
		team = append(team, row)
	}
	byRank := func(rows []userRow) {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Rank != rows[j].Rank {
				return rows[i].Rank > rows[j].Rank
			}
			return strings.ToLower(rows[i].DisplayName()) < strings.ToLower(rows[j].DisplayName())
		})
	}
	byRank(team)
	byRank(customers)
	tab := "team"
	if r.URL.Query().Get("tab") == "customers" {
		tab = "customers"
	}
	var teamRoles, customerRoles []string
	for _, d := range roleDefs {
		if d.Customer {
			customerRoles = append(customerRoles, d.Key)
		} else {
			teamRoles = append(teamRoles, d.Key)
		}
	}
	days, _ := h.db.PeopleActivityDaily(ctx, 8)
	activity := r.URL.Query().Get("activity")
	switch activity {
	case "week", "month", "quiet", "invited":
	default:
		activity = ""
	}
	h.render(w, r, "users.html", map[string]any{
		"P1":            buildUsersP1(p1Inputs{Team: team, Customers: customers, Waiting: len(waiting), Orphans: orphans, Days: days, Now: time.Now()}),
		"Activity":      activity,
		"Waiting":       waiting,
		"Team":          team,
		"Customers":     customers,
		"Invites":       len(invites),
		"Tab":           tab,
		"TeamRoles":     teamRoles,
		"CustomerRoles": customerRoles,
		"Orphans":       orphans,
		"AllUsers":      users,
		"UsersTab":      "people",
		"Assignable":    assignableRoles(actor),
		"InviteFailed":  r.URL.Query().Get("invite_failed"),
		"Restaurants":   func() []db.Restaurant { rs, _ := h.db.ListRestaurants(ctx); return rs }(),
	})
}

// accessPhrase says in a few words what a team member can reach.
func accessPhrase(role string, pol db.AccessPolicy, profiles []db.AccessProfile) string {
	if role == "admin" {
		return "Everything"
	}
	sa := deriveSimple(role, pol, profiles, nil)
	switch {
	case !sa.OK:
		return "Custom rules"
	case sa.Every && (sa.Do == "role" || role == "viewer"):
		return "Every device"
	case sa.Every:
		return "Every device, limited actions"
	case len(sa.Places) == 0:
		return "Nothing yet"
	case len(sa.Places) == 1:
		return sa.Places[0].Name
	}
	return fmt.Sprintf("%d places", len(sa.Places))
}

// accessShort is the word the Users list flags a sensitive extra with.
func accessShort(k string) string {
	if k == "ota" {
		return "firmware"
	}
	return k
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// validUserRole reports whether role is one of the roles in roleDefs. Who may hand
// it out is mayManageUser's question.
func validUserRole(role string) bool { return roleKnown(role) }

func (h *Handler) UserCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The account's identity is its email — used as both username (login) and
	// email (forgot-password), same as self-signup. Pre-verified since the inviter
	// is vouching for it directly. With no password in the form (the dashboard's
	// Invite never sends one) the person gets an email link and chooses their own.
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	password := r.FormValue("password")
	invite := password == ""
	role := r.FormValue("role")
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))

	if email == "" || !strings.Contains(email, "@") || !validUserRole(role) {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	if roleIsCustomer(role) && len(r.Form["restaurants"]) == 0 {
		http.Error(w, "Pick the venue(s) this "+strings.ToLower(roleLabel(role))+" belongs to.", http.StatusBadRequest)
		return
	}
	if invite {
		// Nobody knows this one: the account can't sign in until the invite is used.
		if password, _ = newToken(); password == "" {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
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
	if !roleIsCustomer(role) && role != "admin" && h.canWiden(r, created) != nil {
		if err := h.db.SetUserAccess(r.Context(), created.ID, db.AccessPolicy{Base: "deny"}); err != nil {
			log.Printf("[users] starting policy for %s: %v", created.Username, err)
		}
	}
	// A customer (owner or staff) sees exactly their venue(s).
	if roleIsCustomer(role) {
		h.grantVenues(r, created, r.Form["restaurants"])
	}
	h.audit(r, "user.create", created.Username, roleLabel(role))
	to := "/users"
	if roleIsCustomer(role) {
		to = "/users?tab=customers"
	}
	if back := r.FormValue("redirect"); strings.HasPrefix(back, "/restaurants/") && !strings.Contains(back, "//") {
		to = back
	}
	if invite {
		if err := h.sendInvite(r, created); err != nil {
			log.Printf("[users] invite %s: %v", created.Username, err)
			to += map[bool]string{true: "&", false: "?"}[strings.Contains(to, "?")] + "invite_failed=" + url.QueryEscape(created.Username)
		}
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// grantVenues sets a customer's access to exactly these venues: base deny, out-of-scope
// devices hidden, one "See device" rule per venue (decideCustomer reads those as the
// venue wall).
func (h *Handler) grantVenues(r *http.Request, u *db.User, venueIDs []string) {
	pol := db.AccessPolicy{Base: "deny", HideOutOfScope: true}
	if err := h.db.SetUserAccess(r.Context(), u.ID, pol); err != nil {
		log.Printf("[users] customer policy for %s: %v", u.Username, err)
	}
	for _, rid := range venueIDs {
		id, err := uuid.Parse(rid)
		if err != nil {
			continue
		}
		g := db.AccessGrant{UserID: u.ID, Effect: "allow", ScopeType: "restaurant", ScopeID: &id, Actions: []string{"view"}, Note: strings.ToLower(roleLabel(u.Role)) + "'s venue", CreatedBy: h.currentUsername(r)}
		if saved, err := h.db.AddAccessGrant(r.Context(), g); err != nil {
			log.Printf("[users] venue grant for %s: %v", u.Username, err)
		} else {
			h.audit(r, "user.access.grant", u.Username, describeGrant(*saved))
		}
	}
	invalidatePolicy(u.Username)
}

// sendInvite emails a 7-day link that lets the person choose their password.
func (h *Handler) sendInvite(r *http.Request, u *db.User) error {
	if u.Email == nil || *u.Email == "" {
		return fmt.Errorf("no email address")
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	if err := h.db.CreateUserToken(r.Context(), token, u.ID, "invite", time.Now().Add(7*24*time.Hour)); err != nil {
		return err
	}
	link := h.baseURL(r) + "/reset-password?token=" + token
	if err := h.mail.Send(r.Context(), *u.Email, "You're invited to AIO MDM", mailer.InviteHTML(link, h.currentDisplayName(r), roleLabel(u.Role))); err != nil {
		return err
	}
	h.audit(r, "user.invite", u.Username, roleLabel(u.Role))
	return nil
}

// UserResendInvite sends a fresh invite to someone who hasn't set a password yet.
func (h *Handler) UserResendInvite(w http.ResponseWriter, r *http.Request) {
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
	if err := h.sendInvite(r, target); err != nil {
		log.Printf("[users] resend invite %s: %v", target.Username, err)
		http.Error(w, "Couldn't send the invite. Try again in a minute.", http.StatusBadGateway)
		return
	}
	h.usersRedirect(w, r)
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
	h.usersRedirect(w, r)
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
	// The two ladders don't trade places: a customer's access is "which venues", a team
	// member's is the fleet narrowed by rules, and neither carries over to the other.
	if roleIsCustomer(target.Role) != roleIsCustomer(role) {
		http.Error(w, "Customer and AIO team accounts can't be switched into each other. Invite them again with the other role.", http.StatusBadRequest)
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
