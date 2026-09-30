package dashboard

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

// Verifying people by hand (user-verify demo, option C). Someone who signs up cannot
// sign in until they click the emailed link; when that email never arrives they are
// stuck. The super admin can vouch for the address instead: from the "can't sign in
// yet" strip on People, or from the person's Manage page.

// UserVerifyEmail is POST /users/{id}/verify-email (super admin).
func (h *Handler) UserVerifyEmail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if u.Email == nil || u.EmailVerifiedAt != nil {
		h.usersRedirect(w, r)
		return
	}
	if err := h.db.SetUserEmailVerified(r.Context(), u.ID); err != nil {
		log.Printf("[users] verify %s by hand: %v", u.Username, err)
		http.Error(w, "Could not verify", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.verify_email", u.Username, "verified by hand")
	h.usersRedirect(w, r)
}
