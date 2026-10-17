package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	existing, _ := h.db.KioskPolicyForRestaurant(ctx, id)

	back := "/restaurants/" + id.String()
	switch {
	case pkg == "" && existing != nil:
		// Clearing the rule releases the venue's devices unless another rule covers them;
		// reconcileKiosk works that out rather than this handler guessing.
		if err := h.db.DeleteKioskPolicy(ctx, existing.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		n := h.reconcileKiosk(ctx)
		h.audit(r, "kiosk_rule.delete", rest.Name, fmt.Sprintf("venue=%s devices_changed=%d", id, n))
		h.hxRedirect(w, r, back+"?flash="+url.QueryEscape("Kiosk cleared for "+rest.Name+".")+"&flash_type=success")
		return
	case pkg == "":
		h.hxRedirect(w, r, back)
		return
	}

	name := rest.Name + " · " + pkg
	if existing != nil {
		err = h.db.UpdateKioskPolicy(ctx, existing.ID, name, pkg, "restaurant", &id, "")
	} else {
		_, err = h.db.CreateKioskPolicy(ctx, name, pkg, "restaurant", &id, "")
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n := h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.save", name, fmt.Sprintf("venue=%s app=%s devices_changed=%d", id, pkg, n))
	h.hxRedirect(w, r, back+"?flash="+
		url.QueryEscape(fmt.Sprintf("%s tablets now lock to %s — %d device(s) changed.", rest.Name, pkg, n))+
		"&flash_type=success")
}

// venueKiosk gathers what the venue page shows: the rule, and what is actually true.
type venueKioskView struct {
	Package  string
	RuleID   string
	Total    int
	Locked   int
	Exited   int
	Choices  []venueKioskChoice
	Disagree bool // a rule is set but devices are not all locked to it
}

type venueKioskChoice struct {
	Package string
	Label   string
}

func (h *Handler) venueKiosk(r *http.Request, restaurantID uuid.UUID) *venueKioskView {
	ctx := r.Context()
	v := &venueKioskView{}
	if p, _ := h.db.KioskPolicyForRestaurant(ctx, restaurantID); p != nil {
		v.Package, v.RuleID = p.KioskPackage, p.ID.String()
	}
	v.Total, v.Locked, v.Exited, _ = h.db.RestaurantKioskState(ctx, restaurantID)
	v.Disagree = v.Package != "" && v.Locked < v.Total

	// Offer what the venue's own devices actually have installed, most common first:
	// locking to an app that is not on the tablet is the one mistake this picker can
	// prevent.
	if pkgs, err := h.db.LaunchablePackagesAtRestaurant(ctx, restaurantID); err == nil {
		for _, p := range pkgs {
			label := p.AppName
			if label == "" {
				label = p.PackageName
			}
			v.Choices = append(v.Choices, venueKioskChoice{
				Package: p.PackageName,
				Label:   fmt.Sprintf("%s — on %d", label, p.Devices),
			})
		}
	}
	return v
}
