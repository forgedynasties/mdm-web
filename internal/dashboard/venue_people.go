package dashboard

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ── Venue people (U3, picked 20 Oct 2026) ───────────────────────────────────
//
// A venue's customer accounts — its owner(s) and staff — shown where people think of
// them: a People row in the venue's settings card for the AIO team, and a Team card on
// the owner's home. Owners invite and remove their own staff here; nothing else about
// accounts is open to them, and every action is checked against their own venues.

type venuePerson struct {
	ID       uuid.UUID
	Username string
	Name     string
	Initial  string
	Email    string
	Role     string
	Invited  bool
	LastAt   *time.Time
	Venues   []string // venue names, for the owner's card when they have several
}

// VenueList is the venue names in one line.
func (p venuePerson) VenueList() string { return joinAnd(p.Venues) }

// venuePeople lists the customer accounts of the given venues, owners first.
func (h *Handler) venuePeople(ctx context.Context, venueIDs []uuid.UUID) []venuePerson {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, rid := range venueIDs {
		us, err := h.db.RestaurantCustomers(ctx, rid)
		if err != nil {
			log.Printf("[people] venue %s: %v", rid, err)
			continue
		}
		for _, id := range us {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	invites, _ := h.db.PendingInvites(ctx)
	summaries, _ := h.db.ActorSummaries(ctx)
	var out []venuePerson
	for _, id := range ids {
		u, err := h.db.GetUser(ctx, id)
		if err != nil || u == nil {
			continue
		}
		p := venuePerson{ID: u.ID, Username: u.Username, Name: u.DisplayName(), Role: u.Role}
		for _, c := range p.Name {
			p.Initial = strings.ToUpper(string(c))
			break
		}
		if u.Email != nil {
			p.Email = *u.Email
		}
		_, p.Invited = invites[u.ID]
		if sm, ok := summaries[u.Username]; ok {
			p.LastAt = sm.LastAt
		}
		for _, v := range h.customerVenues(ctx, u.Username) {
			p.Venues = append(p.Venues, v.Name)
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Role) > rank(out[j].Role) })
	return out
}

// customerVenues is the venues a customer's rules give them.
func (h *Handler) customerVenues(ctx context.Context, username string) []venueRef {
	pol, err := h.db.GetUserAccess(ctx, username)
	if err != nil {
		return nil
	}
	var out []venueRef
	for _, g := range pol.Grants {
		if g.Effect == "allow" && g.ScopeType == "restaurant" && g.ScopeID != nil && g.Has("view") && !g.Expired {
			out = append(out, venueRef{*g.ScopeID, g.ScopeName})
		}
	}
	return out
}

// ownerTarget loads the account an owner wants to act on and checks it is staff whose
// every venue is one of the owner's. Writes the error and returns nil otherwise.
func (h *Handler) ownerTarget(w http.ResponseWriter, r *http.Request) *db.User {
	if h.role(r) != "owner" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return nil
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return nil
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil || u.Role != "staff" {
		http.Error(w, "Not one of your staff.", http.StatusNotFound)
		return nil
	}
	mine := map[uuid.UUID]bool{}
	for _, v := range h.customerVenues(r.Context(), h.currentUsername(r)) {
		mine[v.ID] = true
	}
	theirs := h.customerVenues(r.Context(), u.Username)
	for _, v := range theirs {
		if !mine[v.ID] {
			http.Error(w, "They also work at a venue that isn't yours. Ask AIO to change their account.", http.StatusForbidden)
			return nil
		}
	}
	if len(theirs) == 0 {
		http.Error(w, "Not one of your staff.", http.StatusNotFound)
		return nil
	}
	return u
}

// TeamInvite is an owner inviting a staff member to one or more of their own venues.
func (h *Handler) TeamInvite(w http.ResponseWriter, r *http.Request) {
	if h.role(r) != "owner" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, "Enter their email address.", http.StatusBadRequest)
		return
	}
	mine := h.customerVenues(r.Context(), h.currentUsername(r))
	var venues []string
	if len(mine) == 1 {
		venues = []string{mine[0].ID.String()}
	} else {
		ok := map[string]bool{}
		for _, v := range mine {
			ok[v.ID.String()] = true
		}
		for _, v := range r.Form["restaurants"] {
			if ok[v] {
				venues = append(venues, v)
			}
		}
	}
	if len(venues) == 0 {
		http.Error(w, "Pick which of your venues they work at.", http.StatusBadRequest)
		return
	}
	pw, _ := newToken()
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if pw == "" || err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	created, err := h.db.CreateUserNamed(r.Context(), email, string(hash), "staff", &email, true,
		strings.TrimSpace(r.FormValue("first_name")), strings.TrimSpace(r.FormValue("last_name")))
	if err != nil {
		http.Error(w, "That email already has an account. Ask AIO to add them to your venue.", http.StatusBadRequest)
		return
	}
	h.grantVenues(r, created, venues)
	h.audit(r, "user.create", created.Username, "Staff")
	to := "/?team=invited"
	if err := h.sendInvite(r, created); err != nil {
		log.Printf("[people] owner invite %s: %v", created.Username, err)
		to = "/?team=invite_failed"
	}
	http.Redirect(w, r, to+"#team", http.StatusFound)
}

// TeamResendInvite sends a staff member's invite again.
func (h *Handler) TeamResendInvite(w http.ResponseWriter, r *http.Request) {
	u := h.ownerTarget(w, r)
	if u == nil {
		return
	}
	if err := h.sendInvite(r, u); err != nil {
		log.Printf("[people] owner resend %s: %v", u.Username, err)
		http.Error(w, "Couldn't send the invite. Try again in a minute.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/?team=invited#team", http.StatusFound)
}

// TeamRemove deletes a staff member's account.
func (h *Handler) TeamRemove(w http.ResponseWriter, r *http.Request) {
	u := h.ownerTarget(w, r)
	if u == nil {
		return
	}
	if err := h.db.DeleteUser(r.Context(), u.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	invalidatePolicy(u.Username)
	h.audit(r, "user.delete", u.Username, "removed by their venue's owner")
	http.Redirect(w, r, "/?team=removed#team", http.StatusFound)
}
