package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// ── Compliance rules page (static/compliance-baseline-demos.html, B1) ─────────
//
// The fleet baseline as one list of checks, grouped, each with its level and threshold
// and how the fleet stands on it now; the overrides below it, in order, as sentences;
// and a read-only "what applies where" grid for auditing. Admins edit; everyone else
// with fleet access reads.

type compCounts struct{ Pass, Fail, Unknown, NA, Total int }

// Measurable is how many devices answered the check either way.
func (c compCounts) Measurable() int { return c.Pass + c.Fail }

type compCheckRow struct {
	compCheck
	Setting db.ComplianceSetting
	Counts  compCounts
	AutoFix bool
}

type compGroupView struct {
	Name   string
	Checks []compCheckRow
}

type compOverrideView struct {
	db.ComplianceOverride
	Pos, Devices int
	First, Last  bool
	Scope        string
	Summary      string
	Cells        map[string]string // check id → what this override sets, "" when it leaves it alone
}

func compLevelLabel(l string) string {
	switch l {
	case db.ComplianceRequired:
		return "Required"
	case db.ComplianceRecommended:
		return "Recommended"
	case db.ComplianceOff:
		return "Off"
	}
	return ""
}

// compScopeLabel names an override's scope in words.
func compScopeLabel(o db.ComplianceOverride, venues map[uuid.UUID]string, groups map[uuid.UUID]string) string {
	switch o.ScopeType {
	case "venue":
		if o.ScopeID != nil {
			if n, ok := venues[*o.ScopeID]; ok {
				return n
			}
		}
		return "A venue that no longer exists"
	case "group":
		if o.ScopeID != nil {
			if n, ok := groups[*o.ScopeID]; ok {
				return "Group " + n
			}
		}
		return "A group that no longer exists"
	case "role":
		return prod.ClassLabel(o.ScopeKey) + "s"
	case "unassigned":
		return "Devices not at a venue"
	case "lab":
		return "Devices in a lab session"
	}
	return o.ScopeType
}

// compOverrideSummary is what an override does, as a sentence.
func compOverrideSummary(o db.ComplianceOverride) (string, map[string]string) {
	cells := map[string]string{}
	var parts []string
	for _, c := range complianceChecks {
		s, ok := o.Checks[c.ID]
		if !ok {
			continue
		}
		var bits []string
		if s.Level != "" {
			bits = append(bits, strings.ToLower(compLevelLabel(s.Level)))
		}
		if s.Param > 0 && c.Unit != "" {
			bits = append(bits, fmt.Sprintf("%d %s", s.Param, c.Unit))
		}
		cells[c.ID] = strings.Join(bits, " · ")
		parts = append(parts, c.Name+": "+cells[c.ID])
	}
	switch o.Cap {
	case db.ComplianceRecommended:
		parts = append(parts, "every other required check becomes recommended")
	case db.ComplianceOff:
		parts = append(parts, "every other check is off")
	}
	if len(parts) == 0 {
		return "changes nothing yet", cells
	}
	s := strings.Join(parts, "; ")
	if len(o.Checks) == 0 && o.Cap == db.ComplianceRecommended {
		s = "every required check becomes recommended"
	} else if len(o.Checks) == 0 && o.Cap == db.ComplianceOff {
		s = "nothing is checked"
	}
	return s, cells
}

func (h *Handler) compNames(ctx context.Context) (map[uuid.UUID]string, map[uuid.UUID]string) {
	venues, groups := map[uuid.UUID]string{}, map[uuid.UUID]string{}
	if rs, err := h.db.ListRestaurants(ctx); err == nil {
		for _, r := range rs {
			venues[r.ID] = r.Name
		}
	}
	if gs, err := h.db.ListGroups(ctx); err == nil {
		for _, g := range gs {
			groups[g.ID] = g.Name
		}
	}
	return venues, groups
}

func (h *Handler) ComplianceRulesPage(w http.ResponseWriter, r *http.Request) {
	if h.role(r) == "owner" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	results, err := h.db.ListComplianceResults(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	acc := h.access(r)
	counts := map[string]*compCounts{}
	devCompliant := map[uuid.UUID]bool{}
	for k, res := range results {
		if !acc.visible(k.DeviceID) {
			continue
		}
		c := counts[k.CheckID]
		if c == nil {
			c = &compCounts{}
			counts[k.CheckID] = c
		}
		c.Total++
		switch res.State {
		case db.CompliancePass:
			c.Pass++
		case db.ComplianceFail:
			c.Fail++
		case db.ComplianceUnknown:
			c.Unknown++
		default:
			c.NA++
		}
	}
	for id, ok := range compCompliant(results) {
		if acc.visible(id) {
			devCompliant[id] = ok
		}
	}
	compliant := 0
	for _, ok := range devCompliant {
		if ok {
			compliant++
		}
	}

	var groupsView []compGroupView
	for _, c := range complianceChecks {
		row := compCheckRow{compCheck: c, Setting: rules.baseSetting(c), AutoFix: rules.autoFix(c)}
		if n := counts[c.ID]; n != nil {
			row.Counts = *n
		}
		if len(groupsView) == 0 || groupsView[len(groupsView)-1].Name != c.Group {
			groupsView = append(groupsView, compGroupView{Name: c.Group})
		}
		g := &groupsView[len(groupsView)-1]
		g.Checks = append(g.Checks, row)
	}

	// Overrides, with how many visible devices each one covers.
	venues, groupNames := h.compNames(ctx)
	devs, _ := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "", "")
	scopes, _ := h.db.DeviceScopes(ctx)
	labs, _ := h.db.LabSessions(ctx)
	ovs := make([]compOverrideView, len(rules.overrides))
	for i, o := range rules.overrides {
		v := compOverrideView{ComplianceOverride: o, Pos: i + 1, First: i == 0, Last: i == len(rules.overrides)-1,
			Scope: compScopeLabel(o, venues, groupNames)}
		v.Summary, v.Cells = compOverrideSummary(o)
		if o.Cap != "" {
			for _, c := range complianceChecks {
				if _, named := o.Checks[c.ID]; !named {
					v.Cells[c.ID] = "≤ " + strings.ToLower(compLevelLabel(o.Cap))
				}
			}
		}
		for j := range devs {
			d := &devs[j]
			if d.Hidden || d.Retired() || !acc.visible(d.ID) {
				continue
			}
			_, inLab := labs[d.ID]
			if overrideCovers(o, d, scopes[d.ID], inLab) {
				v.Devices++
			}
		}
		ovs[i] = v
	}
	var updatedBy string
	var latest int64
	for _, b := range rules.baseline {
		if b.UpdatedAt.Unix() > latest {
			latest, updatedBy = b.UpdatedAt.Unix(), b.UpdatedBy+" · "+b.UpdatedAt.Format("2 Jan 15:04")
		}
	}
	h.render(w, r, "compliance_rules.html", map[string]any{
		"Title":     "Compliance rules",
		"Groups":    groupsView,
		"Checks":    complianceChecks,
		"Overrides": ovs,
		"Devices":   len(devCompliant),
		"Compliant": compliant,
		"UpdatedBy": updatedBy,
		"CanEdit":   h.role(r) == "admin",
		"Flash":     r.URL.Query().Get("flash"),
	})
}

// parseCompSettings reads level_<id> / param_<id> for every check. With allowEmpty a
// missing level means "leave it alone" (overrides); otherwise the check keeps def.
func parseCompSettings(r *http.Request, allowEmpty bool, def func(compCheck) db.ComplianceSetting) (map[string]db.ComplianceSetting, error) {
	out := map[string]db.ComplianceSetting{}
	for _, c := range complianceChecks {
		s := db.ComplianceSetting{}
		if !allowEmpty {
			s = def(c)
		}
		switch lv := r.FormValue("level_" + c.ID); lv {
		case db.ComplianceRequired, db.ComplianceRecommended, db.ComplianceOff:
			s.Level = lv
		case "":
		default:
			return nil, fmt.Errorf("unknown level %q", lv)
		}
		if c.Unit != "" {
			if p := strings.TrimSpace(r.FormValue("param_" + c.ID)); p != "" {
				n, err := strconv.Atoi(p)
				if err != nil || n < 1 || n > 24*365 {
					return nil, fmt.Errorf("%s: %q is not a number of %s", c.Name, p, c.Unit)
				}
				if !allowEmpty || n != def(c).Param {
					s.Param = n
				}
			}
		}
		if !allowEmpty && c.Fix != "" {
			on := r.FormValue("autofix_"+c.ID) == "1"
			s.AutoFix = &on
		}
		if allowEmpty && s.Level == "" && s.Param == 0 {
			continue
		}
		out[c.ID] = s
	}
	return out, nil
}

// ComplianceBaselineSave writes the fleet baseline.
func (h *Handler) ComplianceBaselineSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	set, err := parseCompSettings(r, false, rules.baseSetting)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var changes []string
	for _, c := range complianceChecks {
		was, now := rules.baseSetting(c), set[c.ID]
		if was.Level != now.Level {
			changes = append(changes, fmt.Sprintf("%s: %s → %s", c.ID, was.Level, now.Level))
		}
		if c.Unit != "" && was.Param != now.Param {
			changes = append(changes, fmt.Sprintf("%s: %d → %d %s", c.ID, was.Param, now.Param, c.Unit))
		}
		if c.Fix != "" && now.AutoFix != nil && *now.AutoFix != rules.autoFix(c) {
			changes = append(changes, fmt.Sprintf("%s: auto-fix %s", c.ID, map[bool]string{true: "on", false: "off"}[*now.AutoFix]))
		}
	}
	if len(changes) == 0 {
		http.Redirect(w, r, "/compliance/rules", http.StatusFound)
		return
	}
	if err := h.db.SetComplianceBaseline(ctx, set, h.currentUsername(r)); err != nil {
		log.Printf("[compliance] save baseline: %v", err)
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.audit(r, "compliance.baseline", "", strings.Join(changes, "; "))
	go h.RunCompliance(context.Background())
	http.Redirect(w, r, "/compliance/rules?flash=saved", http.StatusFound)
}

// ComplianceBaselinePreview answers "what would this change": the devices whose verdict
// (compliant or not) would flip if the posted baseline were saved.
func (h *Handler) ComplianceBaselinePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	set, err := parseCompSettings(r, false, rules.baseSetting)
	if err != nil {
		fmt.Fprintf(w, `<span class="cr-prev bad">%s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}
	s, err := h.loadCompSnapshot(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	proposed := compRules{baseline: map[string]db.ComplianceBaselineRow{}, overrides: rules.overrides}
	for id, v := range set {
		proposed.baseline[id] = db.ComplianceBaselineRow{CheckID: id, Setting: v}
	}
	before, after := compCompliant(compEvaluate(s, rules)), compCompliant(compEvaluate(s, proposed))
	acc := h.access(r)
	worse, better := 0, 0
	for id, now := range after {
		if !acc.visible(id) || before[id] == now {
			continue
		}
		if now {
			better++
		} else {
			worse++
		}
	}
	switch {
	case worse == 0 && better == 0:
		fmt.Fprint(w, `<span class="cr-prev">No device would change</span>`)
	default:
		fmt.Fprintf(w, `<span class="cr-prev">%d device%s would stop being compliant · %d would become compliant</span>`, worse, plural(worse), better)
	}
}


// ── Overrides ─────────────────────────────────────────────────────────────────

func (h *Handler) ComplianceOverrideForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o := db.ComplianceOverride{ScopeType: "venue", Cap: ""}
	if id := r.PathValue("id"); id != "" {
		uid, err := uuid.Parse(id)
		if err != nil {
			http.Error(w, "Invalid ID", http.StatusBadRequest)
			return
		}
		if o, err = h.db.GetComplianceOverride(ctx, uid); err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
	}
	rules, _ := h.loadCompRules(ctx)
	type row struct {
		compCheck
		Base db.ComplianceSetting
		Set  db.ComplianceSetting
	}
	var rows []row
	for _, c := range complianceChecks {
		rows = append(rows, row{compCheck: c, Base: rules.baseSetting(c), Set: o.Checks[c.ID]})
	}
	restaurants, _ := h.db.ListRestaurants(ctx)
	groups, _ := h.db.ListGroups(ctx)
	sort.Slice(restaurants, func(i, j int) bool { return strings.ToLower(restaurants[i].Name) < strings.ToLower(restaurants[j].Name) })
	type roleOpt struct{ Key, Label string }
	var roles []roleOpt
	for _, c := range prod.Classes() {
		roles = append(roles, roleOpt{c, prod.ClassLabel(c)})
	}
	h.render(w, r, "compliance_override.html", map[string]any{
		"Title":       "Compliance override",
		"O":           o,
		"New":         o.ID == uuid.Nil,
		"Rows":        rows,
		"Restaurants": restaurants,
		"Groups":      groups,
		"Roles":       roles,
		"CanEdit":     h.role(r) == "admin",
	})
}

func (h *Handler) ComplianceOverrideSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o := db.ComplianceOverride{Name: strings.TrimSpace(r.FormValue("name")), ScopeType: r.FormValue("scope_type"),
		Cap: r.FormValue("cap"), CreatedBy: h.currentUsername(r)}
	if id := r.FormValue("id"); id != "" {
		uid, err := uuid.Parse(id)
		if err != nil {
			http.Error(w, "Invalid ID", http.StatusBadRequest)
			return
		}
		o.ID = uid
	}
	switch o.ScopeType {
	case "venue", "group":
		raw := r.FormValue("scope_" + o.ScopeType)
		uid, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "Choose the "+o.ScopeType+" this applies to", http.StatusBadRequest)
			return
		}
		o.ScopeID = &uid
	case "role":
		o.ScopeKey = r.FormValue("scope_role")
		if !prod.IsClass(o.ScopeKey) {
			http.Error(w, "Choose a role", http.StatusBadRequest)
			return
		}
	case "unassigned", "lab":
	default:
		http.Error(w, "Choose who this applies to", http.StatusBadRequest)
		return
	}
	switch o.Cap {
	case "", db.ComplianceRecommended, db.ComplianceOff:
	default:
		http.Error(w, "Unknown cap", http.StatusBadRequest)
		return
	}
	rules, _ := h.loadCompRules(ctx)
	checks, err := parseCompSettings(r, true, rules.baseSetting)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	o.Checks = checks
	if o.Name == "" {
		venues, groups := h.compNames(ctx)
		o.Name = compScopeLabel(o, venues, groups)
	}
	id, err := h.db.SaveComplianceOverride(ctx, o)
	if err != nil {
		log.Printf("[compliance] save override: %v", err)
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	summary, _ := compOverrideSummary(o)
	h.audit(r, "compliance.override", id.String(), o.Name+": "+summary)
	go h.RunCompliance(context.Background())
	http.Redirect(w, r, "/compliance/rules?flash=override", http.StatusFound)
}

func (h *Handler) ComplianceOverrideDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	o, err := h.db.GetComplianceOverride(r.Context(), id)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if err := h.db.DeleteComplianceOverride(r.Context(), id); err != nil {
		http.Error(w, "Could not delete", http.StatusInternalServerError)
		return
	}
	h.audit(r, "compliance.override_delete", id.String(), o.Name)
	go h.RunCompliance(context.Background())
	http.Redirect(w, r, "/compliance/rules", http.StatusFound)
}

func (h *Handler) ComplianceOverrideMove(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	dir := 1
	if r.FormValue("dir") == "up" {
		dir = -1
	}
	if err := h.db.MoveComplianceOverride(r.Context(), id, dir); err != nil {
		http.Error(w, "Could not move", http.StatusInternalServerError)
		return
	}
	h.audit(r, "compliance.override_move", id.String(), r.FormValue("dir"))
	go h.RunCompliance(context.Background())
	http.Redirect(w, r, "/compliance/rules", http.StatusFound)
}
