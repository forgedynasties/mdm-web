package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Host app per venue (V1, picked 23 Oct 2026; user: "setting which variant of host app to
// land per venue, override, defwault is prod, but allow setting variant in venue"). One
// channel per venue for every product: '' = prod (aio.app.nugget), "uatv2" = each
// product's .uatv2 build. RunRoleAppInstalls installs it where it is missing and Lock
// rules lock to it once installed. The choices are the channels the venue's units and
// the library already have.

var hostChannelRe = regexp.MustCompile(`^[a-z0-9]{1,24}$`)

// hostChannelLabel names a channel for people.
func hostChannelLabel(ch string) string {
	switch ch {
	case "":
		return "Prod"
	case "uatv2", "uat":
		return "UAT"
	case "stagev2", "stage":
		return "Stage"
	case "qa":
		return "QA"
	case "internal":
		return "Internal"
	}
	return "." + ch
}

type venueHostOpt struct {
	Key, Label string
	Have       int      // units here that have their app in this channel
	NoBuild    []string // products here with no library build in this channel
	On         bool
}

type venueHostAppView struct {
	Channel, Label string
	Total          int // units here whose product has an app
	Opts           []venueHostOpt
	Missing        int // units without the chosen channel's app
	NoBuild        []string
}

func (h *Handler) venueHostAppView(ctx context.Context, venueID uuid.UUID, devices []db.Device) venueHostAppView {
	chans, _ := h.db.VenueHostChannels(ctx)
	v := venueHostAppView{Channel: chans[venueID]}
	v.Label = hostChannelLabel(v.Channel)
	bases := h.db.RoleAppBases(ctx)
	var units []*db.Device
	present := map[string]bool{} // base packages of the products here
	var baseList []string
	for i := range devices {
		d := &devices[i]
		if b := bases[d.Class()]; b != "" && !d.Hidden && !d.Retired() {
			units = append(units, d)
			if !present[b] {
				present[b] = true
				baseList = append(baseList, b)
			}
		}
	}
	v.Total = len(units)
	installed, _ := h.db.FamilyVersions(ctx, baseList)
	library, _ := h.db.ListApps(ctx)
	seen := map[string]bool{"": true, v.Channel: true}
	addSuffix := func(base, pkg string) {
		if sfx := strings.TrimPrefix(pkg, base+"."); sfx != pkg && hostChannelRe.MatchString(sfx) {
			seen[sfx] = true
		}
	}
	for _, d := range units {
		b := bases[d.Class()]
		for p := range installed[d.ID] {
			addSuffix(b, p)
		}
	}
	for _, a := range library {
		for b := range present {
			if a.ApkURL != "" {
				addSuffix(b, a.PackageName)
			}
		}
	}
	want := map[string]bool{}
	for ch := range seen {
		for b := range present {
			want[hostAppTarget(b, ch)] = true
		}
	}
	lib := newestBuilds(library, want)
	for ch := range seen {
		o := venueHostOpt{Key: ch, Label: hostChannelLabel(ch), On: ch == v.Channel}
		for _, d := range units {
			if _, ok := installed[d.ID][hostAppTarget(bases[d.Class()], ch)]; ok {
				o.Have++
			}
		}
		for _, role := range prod.Classes() {
			if b := bases[role]; present[b] && lib[hostAppTarget(b, ch)] == nil {
				o.NoBuild = append(o.NoBuild, prod.ClassLabel(role))
			}
		}
		if o.On {
			v.Missing, v.NoBuild = v.Total-o.Have, o.NoBuild
		}
		v.Opts = append(v.Opts, o)
	}
	sort.SliceStable(v.Opts, func(i, j int) bool {
		a, b := v.Opts[i], v.Opts[j]
		if (a.Key == "") != (b.Key == "") {
			return a.Key == ""
		}
		if a.Have != b.Have {
			return a.Have > b.Have
		}
		return a.Key < b.Key
	})
	return v
}

// RestaurantHostAppSave is POST /restaurants/{id}/host-app (channel = "" for prod).
func (h *Handler) RestaurantHostAppSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	ch := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(r.FormValue("channel"))), ".")
	if ch == "prod" {
		ch = ""
	}
	if ch != "" && !hostChannelRe.MatchString(ch) {
		http.Error(w, "A channel is letters and digits, like uatv2", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVenueHostChannel(r.Context(), id, ch); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.host_app_channel", id.String(), hostChannelLabel(ch))
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=hostapp#vn-hostapp", id), http.StatusSeeOther)
}
