package dashboard

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// Auto-enrol per venue (22 Oct 2026; user: "add option to auto enroll yes or no per
// venue"). An Auto-enrol row on the Venue settings card: on, a device the venue's scout
// finds ready (it trusts the fleet adb key, has no owner, no account, one user) is
// enrolled without anyone pressing Enrol on /enrollment; blocked and unauthorized devices
// still wait for a person, and a failed enrolment is not retried by itself. Off by default.

// autoEnrollBy is the approver recorded for an enrolment nobody pressed.
const autoEnrollBy = "auto-enrol"

// RestaurantAutoEnrollSave is POST /restaurants/{id}/auto-enroll (on = 1 | 0).
func (h *Handler) RestaurantAutoEnrollSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	on := r.FormValue("on") == "1"
	if err := h.db.SetRestaurantAutoEnroll(r.Context(), id, on); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	state := "off"
	if on {
		state = "on"
	}
	h.audit(r, "restaurant.auto_enroll", id.String(), state)
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=enrol#vn-enrol", id), http.StatusSeeOther)
}
