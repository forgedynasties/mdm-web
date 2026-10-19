package dashboard

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// Kiosk on the venue page.
//
// Locking a tablet to one app is the most visible thing this product does, and it had
// three homes that could each silently disagree: the fleet-wide rules page, the device
// page's own toggle, and the Devices bulk bar. None of them is where the business thinks
// about it — the sentence people actually say is "the tablets at this restaurant run the
// ordering app", and a venue is the only place that sentence fits.
//
// This does not invent a model: kiosk_policies has carried target_type 'restaurant' all
// along. It puts the existing capability where it belongs and shows, beside it, what is
// actually true on the venue's devices — which is not the same thing as what the rule
// says, because a device can be unlocked on the device.

// RestaurantKioskSave sets or clears the venue's lock rule.
func (h *Handler) RestaurantKioskSave(w http.ResponseWriter, r *http.Request) {
	if !roleCanOperate(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	rest, err := h.db.GetRestaurant(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	// The venue's rule locks each tablet to its role's app (Lineup); it names no package.
	lock := r.FormValue("lock") == "on"
	existing, _ := h.db.KioskPolicyForRestaurant(ctx, id)

	back := "/restaurants/" + id.String()
	if r.FormValue("back") == "/lock" { // the Device lock page's venue switch
		back = "/lock"
	}
	switch {
	case !lock && existing != nil:
		// Clearing the rule releases the venue's devices unless another rule covers them;
		// reconcileKiosk works that out rather than this handler guessing.
		if err := h.db.DeleteKioskPolicy(ctx, existing.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		n := h.reconcileKiosk(ctx)
		h.audit(r, "kiosk_rule.delete", rest.Name, fmt.Sprintf("venue=%s devices_changed=%d", id, n))
		h.hxRedirect(w, r, back+"?flash="+url.QueryEscape("Lock cleared for "+rest.Name+".")+"&flash_type=success")
		return
	case !lock:
		h.hxRedirect(w, r, back)
		return
	}

	name := rest.Name + " · locked"
	if existing != nil {
		err = h.db.UpdateKioskPolicy(ctx, existing.ID, name, "", "restaurant", &id, "")
	} else {
		_, err = h.db.CreateKioskPolicy(ctx, name, "", "restaurant", &id, "")
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n := h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.save", name, fmt.Sprintf("venue=%s app=role devices_changed=%d", id, n))
	h.hxRedirect(w, r, back+"?flash="+
		url.QueryEscape(fmt.Sprintf("%s devices now lock to their app: %d changed.", rest.Name, n))+
		"&flash_type=success")
}

// venueKiosk gathers what the venue page shows: the rule, and what is actually true.
type venueKioskView struct {
	On       bool   // the venue has a lock rule
	Package  string // a rule saved before 19 Oct names its app; "" = each device's role app
	RuleID   string
	Total    int
	Locked   int
	Exited   int
	Disagree bool // a rule is set but devices are not all locked to it
}

func (h *Handler) venueKiosk(r *http.Request, restaurantID uuid.UUID) *venueKioskView {
	ctx := r.Context()
	v := &venueKioskView{}
	if p, _ := h.db.KioskPolicyForRestaurant(ctx, restaurantID); p != nil {
		v.On, v.Package, v.RuleID = true, p.KioskPackage, p.ID.String()
	}
	v.Total, v.Locked, v.Exited, _ = h.db.RestaurantKioskState(ctx, restaurantID)
	v.Disagree = v.On && v.Locked < v.Total
	return v
}
