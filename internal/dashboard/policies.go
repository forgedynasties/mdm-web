package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// The Device lock page (K1 below) and the rule editor: three questions (where, what it
// locks to, how staff get out) with a live preview. Since 19 Oct a rule
// names no app: each device locks to its role's app from Lineup. Enforcement lives in
// kiosk_rules.go.

// kioskRuleView is one row of the rules list.
type kioskRuleView struct {
	db.KioskPolicy
	AppName     string
	AppIcon     string
	TargetLabel string
	TargetHref  string
	Devices     int // devices following this rule
	Locked      int
	Waiting     int
	Exited      int
	Missing     int
	Cant        int
	Override    int
	Higher      int // devices in its target that follow a rule above it
}

func kioskTargetHref(p db.KioskPolicy) string {
	switch p.TargetType {
	case "restaurant":
		if p.TargetID != nil {
			return "/devices?view=restaurant&id=" + p.TargetID.String()
		}
	case "group":
		if p.TargetID != nil {
			return "/devices?view=group&id=" + p.TargetID.String()
		}
	case "device":
		return "/devices/" + url.PathEscape(p.TargetSerial)
	}
	return "/devices"
}

// Device lock (K1, picked 19 Oct 2026): since rules name no app (each device locks to its
// role's app), the page is about where devices are locked and what needs a person. Four
// counts, then every venue with an on/off switch (the venue's own rule, the same one its
// page sets), then devices taken out on site, then the few other rules (groups, single
// devices, every device, and venue rules from before 19 Oct that still name a package).
// Rule order is kept as stored and still decides overlaps; nothing reorders it any more.

// lockVenueRow is one venue on the Lock page.
type lockVenueRow struct {
	ID      uuid.UUID
	Name    string
	On      bool   // the venue has a lock rule
	Package string // that rule names a package (saved before 19 Oct)
	Devices int
	Locked  int
	Exited  int // taken out on site
	Missing int // their app isn't installed (or their role has none)
	Cant    int // their agent can't lock
	Pct     int // locked share, for the bar
}

// Problems says whether the venue needs a look, for sorting and the tags.
func (v lockVenueRow) Problems() bool { return v.Exited+v.Missing+v.Cant > 0 }

// lockedNow reports whether a device is locked on the device right now, whatever locked it.
func lockedNow(k kioskDevice) bool {
	return k.State.Enabled && !k.State.Suspended && k.State.ExitedAt == nil
}

// Manage renders the Device lock page.
func (h *Handler) Manage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, devs, err := h.kioskSnapshot(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	acc := h.access(r)
	restaurants, _ := h.db.ListRestaurants(ctx)
	groups, _ := h.db.ListGroups(ctx)
	apps := h.fleetAppIndex(r)

	// Venues: every one the viewer can see, with its rule if it has one.
	venues := map[uuid.UUID]*lockVenueRow{}
	var order []*lockVenueRow
	for _, rest := range h.visibleRestaurantList(r, restaurants) {
		v := &lockVenueRow{ID: rest.ID, Name: rest.Name}
		venues[rest.ID] = v
		order = append(order, v)
	}
	venueRule := map[uuid.UUID]bool{} // rule ids that are a venue's own switch
	for _, p := range rules {
		if p.TargetType == "restaurant" && p.TargetID != nil {
			if v := venues[*p.TargetID]; v != nil {
				v.On, v.Package = true, p.KioskPackage
				if p.KioskPackage == "" {
					venueRule[p.ID] = true
				}
			}
		}
	}

	// The other rules, as compact rows with what happened on their devices.
	views := []kioskRuleView{}
	idx := map[uuid.UUID]int{}
	for _, p := range rules {
		if venueRule[p.ID] {
			continue
		}
		v := kioskRuleView{KioskPolicy: p, AppName: p.KioskPackage,
			TargetLabel: manageTargetLabel(p, restaurants, groups), TargetHref: kioskTargetHref(p)}
		if p.KioskPackage == "" {
			v.AppName = roleAppWords
		}
		if a, ok := apps[p.KioskPackage]; ok && p.KioskPackage != "" && a.AppName != "" {
			v.AppName = a.AppName
		}
		idx[p.ID] = len(views)
		views = append(views, v)
	}

	locked, unlocked, exited, cant, notCovered, byHand, noVenue := 0, 0, 0, 0, 0, 0, 0
	seenRule := map[uuid.UUID]bool{}
	for _, k := range devs {
		if !acc.visible(k.Dev.ID) || k.Dev.Hidden || k.Dev.Retired() {
			continue
		}
		// The four counts add up to the devices shown: locked, taken out on site, can't
		// lock (their rule's app is missing or their agent has no lock), or unlocked.
		st := k.Status()
		switch {
		case lockedNow(k):
			locked++
		case k.State.ExitedAt != nil || k.State.Suspended:
			exited++
		case st == "missing" || st == "cant":
			cant++
		default:
			unlocked++
		}
		if k.Dev.RestaurantID == nil {
			noVenue++
		}
		if k.Dev.RestaurantID != nil {
			if v := venues[*k.Dev.RestaurantID]; v != nil {
				v.Devices++
				if lockedNow(k) {
					v.Locked++
				}
				switch st {
				case "exited":
					v.Exited++
				case "missing":
					v.Missing++
				case "cant":
					v.Cant++
				}
			}
		}
		if k.Rule == nil {
			notCovered++
			if k.State.Enabled {
				byHand++
			}
			continue
		}
		ri, listed := idx[k.Rule.ID]
		if !listed {
			continue
		}
		rv := &views[ri]
		seenRule[k.Rule.ID] = true
		rv.Devices++
		switch st {
		case "locked":
			rv.Locked++
		case "waiting":
			rv.Waiting++
		case "exited":
			rv.Exited++
		case "missing":
			rv.Missing++
		case "cant":
			rv.Cant++
		case "override":
			rv.Override++
		}
	}
	if acc.hidesDevices() {
		kept := views[:0:0]
		for _, v := range views {
			if seenRule[v.ID] {
				kept = append(kept, v)
			}
		}
		views = kept
	}
	for _, v := range order {
		if v.Devices > 0 {
			v.Pct = v.Locked * 100 / v.Devices
		}
	}
	// Venues that need a look first, then by name.
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Problems() != order[j].Problems() {
			return order[i].Problems()
		}
		return strings.ToLower(order[i].Name) < strings.ToLower(order[j].Name)
	})

	var exits []kioskExitView
	if all, err := h.db.ListKioskExits(ctx); err == nil {
		for _, e := range all {
			if acc.visible(e.DeviceID) {
				exits = append(exits, kioskExitView{KioskExit: e, AppName: appNameOf(apps, e.Package)})
			}
		}
	}
	h.render(w, r, "manage.html", map[string]any{
		"St":         h.movedSettings(r), // Settings cards that live here since S1 (24 Oct 2026)
		"Title":      "Device lock",
		"Venues":     order,
		"Exits":      exits,
		"Rules":      views,
		"Locked":     locked,
		"Unlocked":   unlocked,
		"Exited":     exited,
		"Cant":       cant,
		"NotCovered": notCovered,
		"NoVenue":    noVenue,
		"ByHand":     byHand,
		"CanEdit":    roleCanOperate(h.role(r)),
		"Flash":      r.URL.Query().Get("flash"),
	})
}

// fleetAppIndex maps package name to its app name and icon.
func (h *Handler) fleetAppIndex(r *http.Request) map[string]db.FleetPackage {
	pk, _ := h.db.SearchFleetPackages(r.Context(), "")
	out := make(map[string]db.FleetPackage, len(pk))
	for _, p := range pk {
		out[p.PackageName] = p
	}
	return out
}

// ManagePolicyNew / ManagePolicyEditPage render the three-question editor.
func (h *Handler) ManagePolicyNew(w http.ResponseWriter, r *http.Request) {
	if !roleCanOperate(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	data := h.kioskEditorData(r)
	data["Title"] = "New lock rule"
	h.render(w, r, "manage_policy_form.html", data)
}

func (h *Handler) ManagePolicyEditPage(w http.ResponseWriter, r *http.Request) {
	if !roleCanOperate(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetKioskPolicy(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := h.kioskEditorData(r)
	data["Title"] = "Edit lock rule"
	data["Policy"] = p
	h.render(w, r, "manage_policy_form.html", data)
}

func (h *Handler) kioskEditorData(r *http.Request) map[string]any {
	ctx := r.Context()
	rests, _ := h.db.ListRestaurants(ctx)
	groups, _ := h.db.ListGroups(ctx)
	acc := h.access(r)
	f := db.DeviceFilter{}
	acc.applyFilter(&f)
	devs, _ := h.db.ListDevices(ctx, f, 0, 10000, "serial", "asc")
	serials := make([]string, 0, len(devs))
	for _, d := range devs {
		serials = append(serials, d.SerialNumber)
	}
	return map[string]any{
		"Restaurants": h.visibleRestaurantList(r, rests),
		"Groups":      h.visibleGroupList(r, groups),
		"Serials":     serials,
		"FleetTotal":  len(devs),
		"AllowAll":    !acc.hidesDevices(),
		"CanEdit":     roleCanOperate(h.role(r)),
		"RoleApps":    h.roleAppList(ctx),
	}
}

// roleAppLine is one role and its app, for the rule editor's "what it locks to".
type roleAppLine struct {
	Role, Label, App, Package string
}

// roleAppList is every role with its app on Lineup, in Lineup's order.
func (h *Handler) roleAppList(ctx context.Context) []roleAppLine {
	bases := h.db.RoleAppBases(ctx)
	var out []roleAppLine
	for _, c := range prod.Classes() {
		l := roleAppLine{Role: c, Label: prod.ClassLabel(c), Package: bases[c]}
		if l.Package != "" {
			l.App = hostAppName(l.Package)
		}
		out = append(out, l)
	}
	return out
}

// kioskTargetFromForm reads the target fields shared by save and preview.
func kioskTargetFromForm(r *http.Request) (targetType string, targetID *uuid.UUID, serial string, ok bool) {
	targetType = r.FormValue("target_type")
	if s := r.FormValue("target_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			targetID = &id
		}
	}
	serial = strings.TrimSpace(r.FormValue("target_serial"))
	switch targetType {
	case "all":
		return targetType, nil, "", true
	case "restaurant", "group":
		return targetType, targetID, "", targetID != nil
	case "device":
		return targetType, nil, serial, serial != ""
	}
	return targetType, nil, "", false
}

// ManagePolicyPreview answers the editor as it is filled in: how many devices the
// target reaches and how many are online, which apps they have installed (with how
// many have each), and for the chosen app, how many would lock, lack it, can't lock,
// or follow a rule above this one.
func (h *Handler) ManagePolicyPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tt, tid, serial, ok := kioskTargetFromForm(r)
	resp := map[string]any{"devices": 0}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	ids, _ := h.resolvePolicyTargetIDs(ctx, tt, tid, serial)
	acc := h.access(r)
	ids, _ = acc.filterDevices("view", ids)
	in := map[uuid.UUID]bool{}
	for _, id := range ids {
		in[id] = true
	}
	rules, devs, err := h.kioskSnapshot(ctx)
	if err != nil {
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	var editing uuid.UUID
	if s := r.FormValue("id"); s != "" {
		editing, _ = uuid.Parse(s)
	}
	// A new or edited rule names no app: each device locks to its role's app. A rule saved
	// before 19 Oct still names one.
	pkg := r.FormValue("kiosk_package")
	installed, _ := h.db.InstalledPackages(ctx, []string{pkg})
	bases := h.db.RoleAppBases(ctx)
	var famList []string
	for _, b := range bases {
		if b != "" {
			famList = append(famList, b)
		}
	}
	fam, _ := h.db.FamilyVersions(ctx, famList)
	scopes, _ := h.db.DeviceScopes(ctx)
	online, cant, missing, lock := 0, 0, 0, 0
	higher := map[string]int{}
	for _, k := range devs {
		if !in[k.Dev.ID] {
			continue
		}
		if k.Online {
			online++
		}
		// A rule above this one that also covers the device wins. A new rule goes to
		// the top (nothing above it), except an all-devices rule, which goes to the
		// bottom (everything is above it).
		above := ""
		if editing != uuid.Nil || tt == "all" {
			for _, rr := range rules {
				if rr.ID == editing {
					break
				}
				if ruleCovers(rr, k.Dev, scopes[k.Dev.ID]) {
					above = rr.Name
					break
				}
			}
		}
		switch {
		case above != "":
			higher[above]++
		case !k.CanLock:
			cant++
		case pkg != "" && !installed[k.Dev.ID][pkg]:
			missing++
		case pkg == "" && lockPackageFor(k.Dev, bases[k.Dev.Class()], "", fam[k.Dev.ID], k.State) == "":
			missing++
		default:
			lock++
		}
	}
	appIdx := h.fleetAppIndex(r)
	pk, _ := h.db.KioskAppsForDevices(ctx, ids)
	type app struct {
		Pkg   string `json:"pkg"`
		Name  string `json:"name"`
		Icon  string `json:"icon"`
		Count int    `json:"count"`
	}
	var list []app
	for _, p := range pk {
		a := app{Pkg: p.PackageName, Name: p.AppName, Count: p.DeviceCount}
		if f, ok := appIdx[p.PackageName]; ok {
			a.Icon = f.Icon
			if a.Name == "" {
				a.Name = f.AppName
			}
		}
		if a.Name == "" {
			a.Name = p.PackageName
		}
		list = append(list, a)
	}
	resp = map[string]any{
		"devices": len(ids), "online": online, "apps": list,
		"lock": lock, "missing": missing, "cant": cant, "higher": higher,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ManagePolicySave creates or updates a rule, then enforces the rules at once.
func (h *Handler) ManagePolicySave(w http.ResponseWriter, r *http.Request) {
	if !roleCanOperate(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	ctx := r.Context()
	tt, tid, serial, ok := kioskTargetFromForm(r)
	// Rules lock to each device's role app (Lineup): the form no longer names a package.
	pkg := ""
	name := strings.TrimSpace(r.FormValue("name"))
	if !ok {
		http.Error(w, "Choose where the rule applies.", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = "Lock rule"
	}
	ids, err := h.resolvePolicyTargetIDs(ctx, tt, tid, serial)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if !h.kioskPolicyInScope(w, r, ids) {
		return
	}
	var id uuid.UUID
	if s := r.FormValue("id"); s != "" {
		if id, err = uuid.Parse(s); err != nil {
			http.Error(w, "Invalid ID", http.StatusBadRequest)
			return
		}
		old, err := h.db.GetKioskPolicy(ctx, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		oldIDs, _ := h.resolvePolicyTargetIDs(ctx, old.TargetType, old.TargetID, old.TargetSerial)
		if !h.kioskPolicyInScope(w, r, oldIDs) {
			return
		}
		if err := h.db.UpdateKioskPolicy(ctx, id, name, pkg, tt, tid, serial); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	} else if id, err = h.db.CreateKioskPolicy(ctx, name, pkg, tt, tid, serial); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n := h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.save", name, fmt.Sprintf("rule=%s app=role target=%s devices_changed=%d", id, tt, n))
	h.hxRedirect(w, r, "/kiosk?flash="+url.QueryEscape(fmt.Sprintf("Saved %q.", name)))
}

// ManagePolicyDelete removes a rule; its devices fall to the next rule that covers
// them, or are released.
func (h *Handler) ManagePolicyDelete(w http.ResponseWriter, r *http.Request) {
	if !roleCanOperate(h.role(r)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetKioskPolicy(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ids, _ := h.resolvePolicyTargetIDs(ctx, p.TargetType, p.TargetID, p.TargetSerial)
	if !h.kioskPolicyInScope(w, r, ids) {
		return
	}
	if err := h.db.DeleteKioskPolicy(ctx, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n := h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.delete", p.Name, fmt.Sprintf("devices_changed=%d", n))
	h.hxRedirect(w, r, "/kiosk?flash="+url.QueryEscape(fmt.Sprintf("Deleted %q.", p.Name)))
}

// kioskPolicyInScope refuses a lock rule that reaches a device the user may not
// kiosk-lock. A rule is standing on its whole target, so it is not narrowed the way a
// one-off command is: all of it must be theirs.
func (h *Handler) kioskPolicyInScope(w http.ResponseWriter, r *http.Request, ids []uuid.UUID) bool {
	acc := h.access(r)
	if acc.unrestricted() {
		return true
	}
	if _, dropped := acc.filterDevices("kiosk", ids); dropped > 0 {
		h.denied(r, "kiosk", nil, decision{Reason: "lock rule reaches devices outside the policy"})
		http.Error(w, fmt.Sprintf("This rule reaches %d device(s) outside your access policy.", dropped), http.StatusForbidden)
		return false
	}
	return true
}

// DeviceKioskFollowRule drops a device's by-hand kiosk override so it follows its rule
// again.
func (h *Handler) DeviceKioskFollowRule(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	dev, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || dev == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.SetKioskOverride(r.Context(), dev.ID, false); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.reconcileKiosk(r.Context())
	h.audit(r, "kiosk_rule.follow", serial, "")
	h.hxRedirect(w, r, "/devices/"+url.PathEscape(serial)+"?flash="+url.QueryEscape("Following its lock rule again."))
}
