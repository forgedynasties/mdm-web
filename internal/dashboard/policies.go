package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The Policies page is kiosk rules and nothing else (30 Sep): an ordered list where
// each rule shows what actually happened on its devices, and a three-question editor
// (where, which app, how staff get out) with a live preview. Enforcement lives in
// kiosk_rules.go.

// kioskRuleView is one row of the rules list.
type kioskRuleView struct {
	db.KioskPolicy
	Pos         int
	First, Last bool
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

// Manage renders the kiosk rules list.
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

	views := make([]kioskRuleView, len(rules))
	idx := map[uuid.UUID]int{}
	for i, p := range rules {
		v := kioskRuleView{KioskPolicy: p, Pos: i + 1, First: i == 0, Last: i == len(rules)-1,
			AppName: p.KioskPackage, TargetLabel: manageTargetLabel(p, restaurants, groups), TargetHref: kioskTargetHref(p)}
		if a, ok := apps[p.KioskPackage]; ok {
			if a.AppName != "" {
				v.AppName = a.AppName
			}
			v.AppIcon = a.Icon
		}
		views[i] = v
		idx[p.ID] = i
	}
	scopes, _ := h.db.DeviceScopes(ctx)
	locked, notCovered, byHand := 0, 0, 0
	seenRule := map[uuid.UUID]bool{}
	for _, k := range devs {
		if !acc.visible(k.Dev.ID) {
			continue
		}
		if k.Rule == nil {
			notCovered++
			if k.State.Enabled {
				byHand++
			}
			continue
		}
		v := &views[idx[k.Rule.ID]]
		seenRule[k.Rule.ID] = true
		v.Devices++
		switch k.Status() {
		case "locked":
			v.Locked++
			locked++
		case "waiting":
			v.Waiting++
		case "exited":
			v.Exited++
		case "missing":
			v.Missing++
		case "cant":
			v.Cant++
		case "override":
			v.Override++
		}
		// Count it against every lower rule that also covers it.
		if k.Covering > 1 {
			for j := idx[k.Rule.ID] + 1; j < len(rules); j++ {
				if ruleCovers(rules[j], k.Dev, scopes[k.Dev.ID]) {
					views[j].Higher++
					seenRule[rules[j].ID] = true
				}
			}
		}
	}
	// Someone who sees part of the fleet sees the rules that touch their devices.
	if acc.hidesDevices() {
		kept := views[:0:0]
		for _, v := range views {
			if seenRule[v.ID] {
				kept = append(kept, v)
			}
		}
		views = kept
	}
	var exits []kioskExitView
	if all, err := h.db.ListKioskExits(ctx); err == nil {
		for _, e := range all {
			if acc.visible(e.DeviceID) {
				exits = append(exits, kioskExitView{KioskExit: e, AppName: appNameOf(apps, e.Package)})
			}
		}
	}
	h.render(w, r, "manage.html", map[string]any{
		"Title":      "Kiosk",
		"Exits":      exits,
		"Rules":      views,
		"Locked":     locked,
		"NotCovered": notCovered,
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
	data["Title"] = "New kiosk rule"
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
	data["Title"] = "Edit kiosk rule"
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
	}
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
	pkg := r.FormValue("kiosk_package")
	installed, _ := h.db.InstalledPackages(ctx, []string{pkg})
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
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	name := strings.TrimSpace(r.FormValue("name"))
	offline := r.FormValue("offline_exit") == "1"
	if !ok || pkg == "" {
		http.Error(w, "Choose where the rule applies and which app to lock to.", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = pkg
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
		if err := h.db.UpdateKioskPolicy(ctx, id, name, pkg, tt, tid, serial, offline); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	} else if id, err = h.db.CreateKioskPolicy(ctx, name, pkg, tt, tid, serial, offline); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n := h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.save", name, fmt.Sprintf("rule=%s app=%s target=%s devices_changed=%d", id, pkg, tt, n))
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("Saved %q.", name)))
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
	h.hxRedirect(w, r, "/manage?flash="+url.QueryEscape(fmt.Sprintf("Deleted %q.", p.Name)))
}

// ManagePolicyMove moves a rule up or down the order. Order decides which rule a
// device covered by two follows, so both rules' devices must be the user's.
func (h *Handler) ManagePolicyMove(w http.ResponseWriter, r *http.Request) {
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
	up := r.FormValue("dir") == "up"
	rules, err := h.db.ListKioskPolicies(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	for i, p := range rules {
		if p.ID != id {
			continue
		}
		j := i + 1
		if up {
			j = i - 1
		}
		if j >= 0 && j < len(rules) {
			var ids []uuid.UUID
			for _, q := range []db.KioskPolicy{p, rules[j]} {
				x, _ := h.resolvePolicyTargetIDs(ctx, q.TargetType, q.TargetID, q.TargetSerial)
				ids = append(ids, x...)
			}
			if !h.kioskPolicyInScope(w, r, ids) {
				return
			}
		}
	}
	if err := h.db.MoveKioskPolicy(ctx, id, up); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.reconcileKiosk(ctx)
	h.audit(r, "kiosk_rule.move", id.String(), map[bool]string{true: "up", false: "down"}[up])
	h.hxRedirect(w, r, "/manage")
}

// kioskPolicyInScope refuses a kiosk rule that reaches a device the user may not
// kiosk-lock. A rule is standing on its whole target, so it is not narrowed the way a
// one-off command is: all of it must be theirs.
func (h *Handler) kioskPolicyInScope(w http.ResponseWriter, r *http.Request, ids []uuid.UUID) bool {
	acc := h.access(r)
	if acc.unrestricted() {
		return true
	}
	if _, dropped := acc.filterDevices("kiosk", ids); dropped > 0 {
		h.denied(r, "kiosk", nil, decision{Reason: "kiosk rule reaches devices outside the policy"})
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
	h.hxRedirect(w, r, "/devices/"+url.PathEscape(serial)+"?flash="+url.QueryEscape("Following its kiosk rule again."))
}
