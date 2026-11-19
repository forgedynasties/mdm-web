package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Add app on many devices (D1, picked 17 Nov 2026): the right-click menu (one device or
// ticked Fleet rows), a group and a venue open the same dialog (add-app.js), and the app is
// added to every device in the target now, offline ones included (user: devices in it now,
// not ones that join later). GET /apps/add-to renders the dialog, POST adds.

// addTarget is what the dialog was opened on.
type addTarget struct {
	Label   string
	Query   string // serials=… | group=… | venue=…
	Devices []db.Device
}

func (h *Handler) resolveAddTarget(r *http.Request) (addTarget, error) {
	ctx := r.Context()
	var t addTarget
	switch {
	case r.FormValue("group") != "":
		id, err := uuid.Parse(r.FormValue("group"))
		if err != nil {
			return t, err
		}
		devs, err := h.db.ListDevices(ctx, db.DeviceFilter{GroupID: id}, 0, 5000, "serial", "asc")
		if err != nil {
			return t, err
		}
		name := "the group"
		if g, err := h.db.GetGroup(ctx, id); err == nil && g != nil {
			name = g.Name
		}
		t = addTarget{Label: name, Query: "group=" + id.String(), Devices: devs}
	case r.FormValue("venue") != "":
		id, err := uuid.Parse(r.FormValue("venue"))
		if err != nil {
			return t, err
		}
		devs, err := h.db.ListDevices(ctx, db.DeviceFilter{RestaurantID: id}, 0, 5000, "serial", "asc")
		if err != nil {
			return t, err
		}
		name := "the venue"
		if v, err := h.db.GetRestaurant(ctx, id); err == nil && v != nil {
			name = v.Name
		}
		t = addTarget{Label: name, Query: "venue=" + id.String(), Devices: devs}
	default:
		seen := map[string]bool{}
		var serials []string
		for _, s := range strings.FieldsFunc(r.FormValue("serials"), func(c rune) bool { return c == ',' || c == '\n' || c == ' ' }) {
			if s != "" && !seen[s] && len(serials) < 2000 {
				seen[s] = true
				serials = append(serials, s)
			}
		}
		for _, s := range serials {
			if d, err := h.db.GetDevice(ctx, s); err == nil && d != nil {
				t.Devices = append(t.Devices, *d)
			}
		}
		t.Query = "serials=" + strings.Join(serials, ",")
		if len(t.Devices) == 1 {
			t.Label = t.Devices[0].SerialNumber
		}
	}
	// Only devices this person may install on, whose client can install apps.
	ids := make([]uuid.UUID, 0, len(t.Devices))
	for _, d := range t.Devices {
		ids = append(ids, d.ID)
	}
	keep := map[uuid.UUID]bool{}
	if acc := h.access(r); !acc.unrestricted() {
		kept, _ := acc.filterDevices(policyActionForCommand("install_apk"), ids)
		for _, id := range kept {
			keep[id] = true
		}
	} else {
		for _, id := range ids {
			keep[id] = true
		}
	}
	out := t.Devices[:0:0]
	for _, d := range t.Devices {
		if keep[d.ID] && d.Supports("install_apk") && !d.Retired() {
			out = append(out, d)
		}
	}
	t.Devices = out
	if t.Label == "" {
		t.Label = fmt.Sprintf("%d devices", len(t.Devices))
	}
	return t, nil
}

// addChannel is one channel of an app in the dialog.
type addChannel struct {
	Label   string `json:"label"`
	Package string `json:"pkg"`
	Newest  string `json:"newest"`
	Have    int    `json:"have"` // target devices that already have it
}

// AddAppDialog is GET /apps/add-to: the dialog for a set of devices.
func (h *Handler) AddAppDialog(w http.ResponseWriter, r *http.Request) {
	if !h.commandTypeAllowed(h.role(r), "install_apk") {
		http.Error(w, "Your role can't add apps.", http.StatusForbidden)
		return
	}
	t, err := h.resolveAddTarget(r)
	if err != nil {
		http.Error(w, "Bad target", http.StatusBadRequest)
		return
	}
	fams, _, _ := h.libraryData(r.Context())
	sort.SliceStable(fams, func(i, j int) bool {
		return strings.HasPrefix(fams[i].BasePackage, "aio.app.") && !strings.HasPrefix(fams[j].BasePackage, "aio.app.")
	})
	ids := make([]uuid.UUID, 0, len(t.Devices))
	online := 0
	for i := range t.Devices {
		ids = append(ids, t.Devices[i].ID)
		if h.deviceOnline(&t.Devices[i]) {
			online++
		}
	}
	var pkgs []string
	for _, f := range fams {
		for _, v := range f.Variants {
			pkgs = append(pkgs, v.Package)
		}
	}
	have, _ := h.db.DevicePackagesOf(r.Context(), ids, pkgs)
	count := map[string]int{}
	for _, m := range have {
		for p := range m {
			count[p]++
		}
	}
	type famRow struct {
		Name, Icon, Channels string
		AIO                  bool
	}
	var rows []famRow
	for _, f := range fams {
		var chs []addChannel
		for _, v := range f.Variants {
			if v.Latest.ApkURL == "" {
				continue
			}
			lbl := ""
			if len(f.Variants) > 1 {
				lbl = channelName(v.Label)
				if v.Label == "prod" {
					lbl = "Prod"
				}
			}
			chs = append(chs, addChannel{Label: lbl, Package: v.Package, Newest: v.Latest.VersionName, Have: count[v.Package]})
		}
		if len(chs) == 0 {
			continue
		}
		b, _ := json.Marshal(chs)
		rows = append(rows, famRow{Name: f.Name, Icon: f.Icon, Channels: string(b), AIO: strings.HasPrefix(f.BasePackage, "aio.app.")})
	}
	h.tmpl.ExecuteTemplate(w, "add-app-dialog", map[string]any{
		"Target": t, "N": len(t.Devices), "Online": online, "Offline": len(t.Devices) - online, "Apps": rows,
	})
}

// AddAppTo is POST /apps/add-to {serials|group|venue, package}: adds one app to every
// device of the target that lacks it, and answers {message} for the island.
func (h *Handler) AddAppTo(w http.ResponseWriter, r *http.Request) {
	if !h.commandTypeAllowed(h.role(r), "install_apk") {
		http.Error(w, "Your role can't add apps.", http.StatusForbidden)
		return
	}
	r.ParseForm()
	t, err := h.resolveAddTarget(r)
	if err != nil || len(t.Devices) == 0 {
		http.Error(w, "None of these devices can take an app from you.", http.StatusBadRequest)
		return
	}
	pkgs, names := h.libPackages(r.Context(), r)
	if len(pkgs) != 1 {
		http.Error(w, "Pick one app from the library.", http.StatusBadRequest)
		return
	}
	pkg := pkgs[0]
	ids := make([]uuid.UUID, 0, len(t.Devices))
	for _, d := range t.Devices {
		ids = append(ids, d.ID)
	}
	have, _ := h.db.DevicePackagesOf(r.Context(), ids, []string{pkg})
	by := h.currentUsername(r)
	var added []uuid.UUID
	now, later := 0, 0
	for i := range t.Devices {
		d := &t.Devices[i]
		if have[d.ID][pkg] {
			continue
		}
		if err := h.db.SetAppWant(r.Context(), d.ID, pkg, db.WantPresent, by); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		added = append(added, d.ID)
		if h.deviceOnline(d) {
			now++
		} else {
			later++
		}
	}
	h.audit(r, "app.add", pkg, fmt.Sprintf("%d device(s) via %s", len(added), t.Label))
	if len(added) > 0 {
		h.applyWants(r.Context(), added, true)
	}
	name := names[pkg]
	if name == "" {
		name = pkg
	}
	msg := ""
	switch {
	case len(added) == 0:
		msg = fmt.Sprintf("All %d already have %s", len(t.Devices), name)
	case len(added) == 1 && later == 0:
		msg = "Added " + name + " · installing now"
	case len(added) == 1:
		msg = "Added " + name + " · installs when the device is back online"
	default:
		msg = fmt.Sprintf("Added %s to %d devices · %d installing · %d waiting for the device", name, len(added), now, later)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"message": msg, "added": len(added)})
}
