package dashboard

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// RestaurantAppAutoSave is POST /restaurants/{id}/app-auto: whether the venue's devices
// get their role app installed automatically (on=1) or keep what they run (on=0).
func (h *Handler) RestaurantAppAutoSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	on := r.FormValue("on") == "1"
	if err := h.db.SetVenueAppAutoInstall(r.Context(), id, on); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	state := "off"
	if on {
		state = "on"
	}
	h.audit(r, "venue.app_auto_install", id.String(), state)
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=appauto#vn-appauto", id), http.StatusSeeOther)
}
