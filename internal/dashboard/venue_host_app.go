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
// land per venue, override, defwault is prod, but allow setting variant in venue"), one
// channel per product since H1 (27 Oct: "venue config for host app should be per host
// app"): '' = prod (aio.app.nugget), "uatv2" = that product's .uatv2 build.
// RunRoleAppInstalls installs it where it is missing and Lock rules lock to it once
// installed. Each product's choices are the channels its units here and the library have.

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
	Have       int  // units of this product here that have its app in this channel
	NoBuild    bool // no library build of the product's app in this channel
	On         bool
}

// venueHostProduct is one product at the venue (H1, 27 Oct 2026): its app and the
// channel its units here land.
type venueHostProduct struct {
	Role, Label, Base string
	Channel, ChLabel  string // chosen ('' = prod)
	Target            string // the package that lands: Base or Base.<channel>
	Total             int    // units of this product here
	Have              int    // of them, with Target installed
	NoBuild           bool   // Target has no build in the library
	Opts              []venueHostOpt
}

type venueHostAppView struct {
	Products     []venueHostProduct
	Total        int    // units here whose product has an app
	Summary, Sub string // "TSAI on UAT", "others prod" | "Prod", "default"
	NoBuild      int    // units missing their app with no build to send
}

func (h *Handler) venueHostAppView(ctx context.Context, venueID uuid.UUID, devices []db.Device) venueHostAppView {
	var v venueHostAppView
	chans, _ := h.db.VenueHostChannels(ctx)
	mine := chans[venueID]
	bases := h.db.RoleAppBases(ctx)
	units := map[string][]*db.Device{} // role → its units here
	var baseList []string
	for i := range devices {
		d := &devices[i]
		role := d.Class()
		if b := bases[role]; b != "" && !d.Hidden && !d.Retired() {
			if len(units[role]) == 0 {
				baseList = append(baseList, b)
			}
			units[role] = append(units[role], d)
			v.Total++
		}
	}
	if v.Total == 0 {
		return v
	}
	installed, _ := h.db.FamilyVersions(ctx, baseList)
	library, _ := h.db.ListApps(ctx)
	var off []string
	prodN := 0
	for _, role := range prod.Classes() {
		us := units[role]
		if len(us) == 0 {
			continue
		}
		base := bases[role]
		p := venueHostProduct{Role: role, Label: prod.ClassLabel(role), Base: base, Channel: mine[role], Total: len(us)}
		p.ChLabel = hostChannelLabel(p.Channel)
		// The channels this product has: on its units here or in the library.
		seen := map[string]bool{"": true, p.Channel: true}
		addSuffix := func(pkg string) {
			if sfx := strings.TrimPrefix(pkg, base+"."); sfx != pkg && hostChannelRe.MatchString(sfx) {
				seen[sfx] = true
			}
		}
		for _, d := range us {
			for pkg := range installed[d.ID] {
				addSuffix(pkg)
			}
		}
		for _, a := range library {
			if a.ApkURL != "" {
				addSuffix(a.PackageName)
			}
		}
		want := map[string]bool{}
		for ch := range seen {
			want[hostAppTarget(base, ch)] = true
		}
		lib := newestBuilds(library, want)
		for ch := range seen {
			t := hostAppTarget(base, ch)
			o := venueHostOpt{Key: ch, Label: hostChannelLabel(ch), On: ch == p.Channel, NoBuild: lib[t] == nil}
			for _, d := range us {
				if _, ok := installed[d.ID][t]; ok {
					o.Have++
				}
			}
			if o.On {
				p.Target, p.Have, p.NoBuild = t, o.Have, o.NoBuild
				if o.NoBuild {
					v.NoBuild += p.Total - o.Have
				}
			}
			p.Opts = append(p.Opts, o)
		}
		sort.SliceStable(p.Opts, func(i, j int) bool {
			a, b := p.Opts[i], p.Opts[j]
			if (a.Key == "") != (b.Key == "") {
				return a.Key == ""
			}
			if a.Have != b.Have {
				return a.Have > b.Have
			}
			return a.Key < b.Key
		})
		if p.Channel != "" {
			off = append(off, shortClassLabel(role)+" on "+p.ChLabel)
		} else {
			prodN++
		}
		v.Products = append(v.Products, p)
	}
	switch {
	case len(off) == 0:
		v.Summary, v.Sub = "Prod", "default"
	case prodN == 0:
		v.Summary = strings.Join(off, ", ")
	default:
		v.Summary, v.Sub = strings.Join(off, ", "), "others prod"
	}
	return v
}

// shortClassLabel is a product's name as a summary line uses it ("TSAI", "Kiosk").
func shortClassLabel(role string) string {
	switch role {
	case prod.ClassT7:
		return "TSAI"
	case prod.ClassKiosk:
		return "Kiosk"
	case prod.ClassDongle:
		return "Menu board"
	}
	return prod.ClassLabel(role)
}

// RestaurantHostAppSave is POST /restaurants/{id}/host-app: one channel per product,
// fields ch_<role> ("" or "prod" for prod).
func (h *Handler) RestaurantHostAppSave(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	chans := map[string]string{}
	var parts []string
	for key, vals := range r.PostForm {
		role, ok := strings.CutPrefix(key, "ch_")
		if !ok || len(vals) == 0 || !prod.IsClass(role) {
			continue
		}
		ch := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(vals[0])), ".")
		if ch == "prod" {
			ch = ""
		}
		if ch != "" && !hostChannelRe.MatchString(ch) {
			http.Error(w, "A channel is letters and digits, like uatv2", http.StatusBadRequest)
			return
		}
		chans[role] = ch
		parts = append(parts, prod.ClassLabel(role)+": "+hostChannelLabel(ch))
	}
	sort.Strings(parts)
	if err := h.db.SetVenueHostChannels(r.Context(), id, chans); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.host_app_channel", id.String(), strings.Join(parts, ", "))
	http.Redirect(w, r, fmt.Sprintf("/restaurants/%s?open=hostapp#vn-hostapp", id), http.StatusSeeOther)
}
