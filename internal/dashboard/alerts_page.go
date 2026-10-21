package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// DeviceAlertsPanel renders the device page's Alerts tab body — this device's
// crash/ANR events (with traces) plus its active alerts — on first open of the
// tab. Kept out of the page render because a crashy device carries megabytes
// of traces here that most visits never look at.
func (h *Handler) DeviceAlertsPanel(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	// The panel shows four of each and expands to the rest in place, so it only needs
	// a handful — a noisy device has hundreds of events, and fetching fifty of them
	// with their full stack traces made this fragment tens of seconds and ~90KB, which
	// reads as "the alerts never load". Everything beyond this goes to the Alerts page.
	const panelRows = 12
	crashes := toCrashCards(mustCrashes(h.db.ListDeviceCrashes(ctx, device.ID, panelRows)))
	for i := range crashes {
		crashes[i].Trace = trimTrace(crashes[i].Trace)
	}
	// keepVisibleAlerts here is about the alert TYPE, not the device (this device is
	// already known visible): it drops "Possible impersonation" for anyone but a
	// super admin, so the device page agrees with the inbox.
	raw := h.access(r).keepVisibleAlerts(mustAlerts(h.db.ListDeviceActiveAlerts(ctx, device.ID, panelRows)))
	role := h.role(r)
	canAct := roleCanOperate(role)
	var alerts []humanAlert
	for _, a := range raw {
		ha := humanizeAlert(a)
		ha.CanAct = canAct
		ha.Trace = trimTrace(ha.Trace)
		alerts = append(alerts, ha)
	}
	h.resolveAppIcons(ctx, [][]humanAlert{alerts}, crashes)
	h.render(w, r, "device-alerts-panel", map[string]any{
		"Device":        device,
		"DeviceCrashes": crashes,
		"DeviceAlerts":  alerts,
	})
}

// AlertList renders the alerts page, optionally filtered by ?status=open|acknowledged|resolved.
func (h *Handler) AlertList(w http.ResponseWriter, r *http.Request) {
	// The inbox has one segmented control: All / Needs action / Watching. Resolved
	// history is intentionally not a view here — the inbox is about what's active.
	view := r.URL.Query().Get("view")
	switch view {
	case "", "needs", "watching", "crashes":
	default:
		view = ""
	}
	summary, err := h.db.AlertSummaryCounts(r.Context(), h.access(r).hidesIdentityAlerts())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	role := h.role(r)
	canAct := roleCanOperate(role)

	// Optional per-device filter (a device's Alerts-tab "Show more" links here so the
	// full list opens scoped to that unit). Resolve the serial to an ID; an unknown
	// serial simply yields an empty, clearly-labelled list rather than the whole fleet.
	deviceSerial := strings.TrimSpace(r.URL.Query().Get("device"))
	var deviceID *uuid.UUID
	acc := h.access(r)
	if deviceSerial != "" {
		if dev, err := h.db.GetDevice(r.Context(), deviceSerial); err == nil && acc.visible(dev.ID) {
			id := dev.ID
			deviceID = &id
		} else {
			id := uuid.Nil // no such device: force an empty result set
			deviceID = &id
		}
	}

	var active []db.Alert
	if deviceID != nil {
		active, err = h.db.ListDeviceActiveAlerts(r.Context(), *deviceID, 150)
	} else {
		active, err = h.db.ListActiveAlerts(r.Context(), 150)
	}
	// Filter both branches: the device-scoped one also has to drop identity alerts
	// for a non-admin, and the device itself was already checked above.
	active = h.access(r).keepVisibleAlerts(active)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var crit, watch []humanAlert
	hs := make([]humanAlert, len(active))
	for i, a := range active {
		ha := humanizeAlert(a)
		ha.CanAct = canAct
		// Crash/ANR alerts carry the real diagnostic: attach the latest stored stack
		// trace so it renders inline on the card (same as the release problems page),
		// instead of linking out to the old logcat capture.
		if a.Type == "device_crash" && a.DeviceID != nil {
			if trace, ok, _ := h.db.LatestCrashTrace(r.Context(), *a.DeviceID); ok {
				ha.Trace = trace
			}
		}
		hs[i] = ha
		if a.Severity == "critical" {
			crit = append(crit, ha)
		} else {
			watch = append(watch, ha)
		}
	}
	// Crash events (device_events) are separate from alert rows — crash alerts
	// auto-resolve, so past crashes vanish from the active-alert list. Fold the raw
	// crash feed in as its own paginated view so "View all crashes" actually shows them.
	const crashPageSize = 25
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	// The crash feed is fleet-wide unless scoped to a device; someone who can't see the
	// whole fleet gets it only for one of their devices.
	fleetCrashes := deviceID != nil || !acc.hidesDevices()
	var crashEvents []db.CrashEvent
	var crashTotal, crashEventTotal int
	if fleetCrashes {
		crashEvents, crashTotal, _ = h.db.ListRecentCrashGroupsPage(r.Context(), deviceID, 7, crashPageSize, (page-1)*crashPageSize)
		crashEventTotal, _ = h.db.CountRecentCrashEvents(r.Context(), deviceID, 7)
	}
	// Clamp a past-the-end page back to the last real page so a stale ?page= link
	// lands on content, not an empty list.
	crashPages := (crashTotal + crashPageSize - 1) / crashPageSize
	if crashPages > 0 && page > crashPages {
		page = crashPages
		crashEvents, crashTotal, _ = h.db.ListRecentCrashGroupsPage(r.Context(), deviceID, 7, crashPageSize, (page-1)*crashPageSize)
	}
	crashes := toCrashCards(crashEvents)
	h.resolveAppIcons(r.Context(), [][]humanAlert{crit, watch, hs}, crashes)
	// Problems: the alerts open on one device worked as one, worst first.
	names := h.actorDisplayNames(r.Context())
	var needs, watching, snoozed []problemView
	isAdmin := h.role(r) == "admin"
	for _, p := range buildProblems(active, hs, names) {
		p.KeyReset = p.KeyReset && isAdmin // resetting a key is admin-only
		switch {
		case p.Snoozed:
			snoozed = append(snoozed, p)
		case p.Severity == "critical":
			needs = append(needs, p)
		default:
			watching = append(watching, p)
		}
	}
	alAnchors(needs)
	alAnchors(watching)
	alAnchors(snoozed)
	// A1 (21 Oct 2026): banner, count cards and rail. Fleet-wide counts (alerts raised,
	// crashes) only for viewers who see the whole fleet and no device filter.
	var raised *db.AlertsRaised
	var crashStats *db.FleetCrashStats
	if deviceID == nil && !acc.hidesDevices() {
		if rc, err := h.db.AlertsRaisedCounts(r.Context(), acc.hidesIdentityAlerts()); err == nil {
			raised = &rc
		}
		if cs, err := h.db.GetFleetCrashStats(r.Context(), 1); err == nil {
			crashStats = &cs
		}
	}
	now := time.Now()
	// Crash signatures on several devices are release issues, not device problems —
	// and a release concern, so the section is super-admin only.
	var issues []releaseIssueView
	if deviceID == nil && !acc.hidesDevices() && isAdmin {
		ci, _ := h.db.ListCrashIssues(r.Context(), 14, 20)
		for _, c := range ci {
			label, class := crashKindBadge(c.Kind)
			issues = append(issues, releaseIssueView{c, extractPackageName(c.Summary), label, class})
		}
	}
	var assignees []assigneeOption
	if canAct {
		if users, err := h.db.ListUsers(r.Context()); err == nil {
			for _, u := range users {
				if !roleCanSupport(u.Role) {
					continue
				}
				assignees = append(assignees, assigneeOption{u.Username, names[u.Username]})
			}
		}
	}
	// Fold each bucket by (type, site) AFTER icons are resolved, so a group's lead
	// card keeps the icon its members resolved.
	critGroups, watchGroups := groupAlerts(crit), groupAlerts(watch)
	// Preserve the device scope on the pager links.
	crashPageBase := "/alerts?view=crashes"
	if deviceSerial != "" {
		crashPageBase += "&device=" + url.QueryEscape(deviceSerial)
	}
	h.render(w, r, "alerts.html", map[string]any{
		"Title":    "Alerts",
		"Summary":  summary,
		"View":     view,
		"Critical": critGroups,
		"Watching": watchGroups,
		// Counts stay per-device: an operator wants "14 devices need attention", not
		// "1 group". Only the rendering folds.
		// Counts are problems now: "3 need action" is three faults to look at, however
		// many alerts each one raised.
		"NeedsCount":  len(needs),
		"WatchCount":  len(watching),
		"ActiveCount": len(needs) + len(watching) + len(snoozed),
		"AlertCount":  len(active),
		"Crashes":     crashes,
		// The KPI counts crashes; the list below counts distinct crashes. Both are shown
		// because "3 signatures" and "212 crashes" answer different questions.
		"CrashCount":    crashEventTotal,
		"CrashGroups":   crashTotal,
		"DeviceFilter":  deviceSerial,
		"CrashPage":     page,
		"CrashPages":    crashPages,
		"CrashPageBase": crashPageBase,
		"Needs":         needs,
		"WatchingP":     watching,
		"Snoozed":       snoozed,
		"Issues":        issues,
		"Assignees":     assignees,
		"Reasons":       problemReasons(),
		"CanAct":        canAct,
		"Me":            h.currentUsername(r),
		"AlBanner":      buildAlertBanner(needs, watching, now),
		"AlCards":       buildAlertCards(needs, watching, raised, crashStats, now),
		"AlRail":        buildAlertRail(needs, watching, crashStats, now),
		"AlVenues":      alVenues(needs, watching, snoozed),
	})
}

// releaseIssueView is one crash signature on one build across several devices.
type releaseIssueView struct {
	db.CrashIssue
	Pkg       string
	KindLabel string
	KindClass string
}

// assigneeOption is one person a problem can be assigned to.
type assigneeOption struct{ Username, Name string }

// alertGroup is one rule firing across one site, folded into a single row. The
// crash feed has merged by signature for a while (ListRecentCrashGroupsPage); the
// alert list did not, so one site-wide cause — fourteen T7s on the same weak
// circuit tripping slow_charge_night — arrived as fourteen identical cards and
// buried everything else. The unique index is (type, device_id), so the dedup that
// already exists can only ever collapse repeats on ONE device.
//
// Lead is the newest member and renders the collapsed row; Members carries every
// alert so the expander can list each device with its own sentence and its own
// ack/resolve buttons (there is no bulk-resolve endpoint, and inventing one here
// would be a bigger change than the noise warrants).
type alertGroup struct {
	Lead        humanAlert
	Members     []humanAlert
	DeviceCount int
	Occurrences int    // summed across members
	Search      string // every member's searchable text, so the client filter still finds a folded device
}

// groupAlerts folds a severity bucket by (type, restaurant). Order is preserved:
// a group lands where its newest member sat, so the existing severity/fired_at
// ordering from ListActiveAlerts still drives the page. A lone alert becomes a
// one-member group and renders exactly as it did before.
//
// Restaurant is part of the key on purpose. "Fourteen devices at Flights Vegas
// charged slowly" is one fact an operator can act on; the same rule firing at four
// different sites is four separate problems and stays four rows.
func groupAlerts(alerts []humanAlert) []alertGroup {
	groups := make([]alertGroup, 0, len(alerts))
	idx := make(map[string]int, len(alerts))
	for _, a := range alerts {
		// Crash alerts carry a per-device stack trace and app icon, so folding them
		// would hide the one thing that makes them useful. The crashes view already
		// groups them by signature.
		key := a.Type + "\x00" + a.Restaurant
		if a.Type == "device_crash" || a.Type == "" {
			key = "\x00unique\x00" + a.ID.String()
		}
		if i, ok := idx[key]; ok {
			g := &groups[i]
			g.Members = append(g.Members, a)
			g.DeviceCount++
			g.Occurrences += a.Occurrences
			g.Search += " " + a.Serial
			continue
		}
		idx[key] = len(groups)
		groups = append(groups, alertGroup{
			Lead: a, Members: []humanAlert{a}, DeviceCount: 1,
			Occurrences: a.Occurrences,
			Search:      a.Headline + " " + a.Serial + " " + a.Restaurant + " " + plainSentence(a.Sentence),
		})
	}
	return groups
}

// crashCardView is one crash/ANR/tombstone event rendered as a card on the alerts
// page and the device Alerts tab: a kind badge, device, build and relative time,
// with the full trace tucked into an expander.
type crashCardView struct {
	Serial      string
	Restaurant  string
	KindLabel   string
	KindClass   string
	BuildID     string
	OccurredAt  time.Time
	Summary     string
	Trace       string
	PackageName string // app package parsed from Summary, if any
	AppIcon     string // base64 PNG from the App library, resolved below
	// Merged-signature counts: how many devices hit this crash and how many times in
	// total. Zero when the row is a single raw event.
	DeviceCount int
	EventCount  int
}

// mustCrashes drops the error from a crash-event query — a failed crash lookup on a
// device page should degrade to an empty tab, not a 500.
func mustCrashes(events []db.CrashEvent, _ error) []db.CrashEvent { return events }

// mustAlerts is mustCrashes for alert rows: the panels that take these treat a read
// error as "nothing to show".
func mustAlerts(alerts []db.Alert, _ error) []db.Alert { return alerts }

// toCrashCards maps raw crash events to their view rows, deriving the kind badge.
func toCrashCards(events []db.CrashEvent) []crashCardView {
	cards := make([]crashCardView, 0, len(events))
	for _, e := range events {
		label, class := crashKindBadge(e.Kind)
		cards = append(cards, crashCardView{
			Serial:      e.Serial,
			Restaurant:  e.Restaurant,
			KindLabel:   label,
			KindClass:   class,
			BuildID:     e.BuildID,
			OccurredAt:  e.OccurredAt,
			Summary:     e.Summary,
			Trace:       e.Detail,
			PackageName: extractPackageName(e.Summary),
			DeviceCount: e.DeviceCount,
			EventCount:  e.EventCount,
		})
	}
	return cards
}

// resolveAppIcons resolves each device_crash alert's / crash card's app icon from the
// App library's shared app_icons index in one batch query, so crash/ANR cards show the
// real app icon instead of the generic crash glyph wherever a package name was parsed
// out of the summary. Mutates alertGroups and crashes in place.
func (h *Handler) resolveAppIcons(ctx context.Context, alertGroups [][]humanAlert, crashes []crashCardView) {
	pkgSet := map[string]struct{}{}
	for _, group := range alertGroups {
		for _, ha := range group {
			if ha.PackageName != "" {
				pkgSet[ha.PackageName] = struct{}{}
			}
		}
	}
	for _, c := range crashes {
		if c.PackageName != "" {
			pkgSet[c.PackageName] = struct{}{}
		}
	}
	if len(pkgSet) == 0 {
		return
	}
	pkgs := make([]string, 0, len(pkgSet))
	for pkg := range pkgSet {
		pkgs = append(pkgs, pkg)
	}
	icons, err := h.db.GetAppIconsByPackages(ctx, pkgs)
	if err != nil {
		return
	}
	for _, group := range alertGroups {
		for i := range group {
			group[i].AppIcon = icons[group[i].PackageName]
		}
	}
	for i := range crashes {
		crashes[i].AppIcon = icons[crashes[i].PackageName]
	}
}

// AlertNewest returns the single most urgent active alert in friendly form as JSON,
// for the toast that pops when a new critical alert arrives.
func (h *Handler) AlertNewest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	alerts, _ := h.db.ListActiveAlerts(r.Context(), 20)
	alerts = h.access(r).keepVisibleAlerts(alerts)
	if len(alerts) == 0 {
		w.Write([]byte("null"))
		return
	}
	ha := humanizeAlert(alerts[0])
	href := ""
	if ha.Primary != nil {
		href = ha.Primary.Href
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       ha.ID.String(),
		"severity": ha.Severity,
		"headline": ha.Headline,
		"sentence": plainSentence(ha.Sentence),
		"href":     href,
	})
}

// AlertsRecent renders a compact list of the latest open alerts for the top-bar
// bell dropdown (lazy-loaded via htmx when the dropdown opens).
func (h *Handler) AlertsRecent(w http.ResponseWriter, r *http.Request) {
	alerts, err := h.db.ListActiveAlerts(r.Context(), 30)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Snoozed alerts (by hand, or auto-snoozed while flapping) are not "needs attention":
	// the Alerts page files them under Snoozed and the dock leaves them out of its count,
	// so the bell does too (20 Oct: a flap-snoozed "Left the venue" sat in the bell only).
	now := time.Now()
	shown := alerts[:0:0]
	for _, a := range h.access(r).keepVisibleAlerts(alerts) {
		if a.MutedUntil != nil && a.MutedUntil.After(now) {
			continue
		}
		shown = append(shown, a)
	}
	if alerts = shown; len(alerts) > 6 {
		alerts = alerts[:6]
	}
	h.tmpl.ExecuteTemplate(w, "alerts-recent", map[string]any{"Alerts": humanizeAll(alerts)})
}

// AlertBulk applies an action (acknowledge|resolve) to the alert IDs selected via
// checkboxes on the alerts page.
func (h *Handler) AlertBulk(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	r.ParseForm()
	var status string
	switch r.FormValue("action") {
	case "acknowledge":
		status = "acknowledged"
	case "resolve":
		status = "resolved"
	default:
		h.hxDone(w, r, "/alerts")
		return
	}
	visible, all := h.visibleAlertIDs(r)
	var ids []uuid.UUID
	for _, s := range r.Form["ids"] {
		if id, err := uuid.Parse(s); err == nil && (all || visible[id]) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		if _, err := h.db.BulkSetAlertStatusByIDs(r.Context(), ids, status); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "alert."+status+"_selected", "", strconv.Itoa(len(ids)))
		h.hub.PublishAlertUpdate()
	}
	h.hxDone(w, r, "/alerts")
}

// visibleAlertIDs is the set of active alerts the user may see and act on; all is
// true when nothing is hidden from them (the set is then not computed).
func (h *Handler) visibleAlertIDs(r *http.Request) (map[uuid.UUID]bool, bool) {
	acc := h.access(r)
	if !acc.hidesDevices() {
		return nil, true
	}
	active, err := h.db.ListActiveAlerts(r.Context(), 5000)
	out := map[uuid.UUID]bool{}
	if err != nil {
		return out, false
	}
	for _, a := range acc.keepVisibleAlerts(active) {
		out[a.ID] = true
	}
	return out, false
}

// AlertAck marks an alert acknowledged. AlertResolve resolves it.
func (h *Handler) AlertAck(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.setAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolve(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.setAlertStatus(w, r, "resolved")
}

// AlertProblemAction works one problem: every open alert in it is assigned,
// acknowledged (with an optional note), snoozed or resolved with a reason at once.
// The ids come from the page; only alerts this user can see are touched.
func (h *Handler) AlertProblemAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	op, value := r.FormValue("op"), strings.TrimSpace(r.FormValue("value"))
	if op == "ack" {
		value = strings.TrimSpace(r.FormValue("note"))
		if len(value) > 500 {
			value = value[:500]
		}
	}
	active, err := h.db.ListActiveAlerts(r.Context(), 1000)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	visible := map[uuid.UUID]bool{}
	for _, a := range h.access(r).keepVisibleAlerts(active) {
		visible[a.ID] = true
	}
	var ids []uuid.UUID
	for _, part := range strings.Split(r.FormValue("ids"), ",") {
		if id, err := uuid.Parse(strings.TrimSpace(part)); err == nil && visible[id] {
			ids = append(ids, id)
		}
	}
	if op == "assign" && value == "me" {
		value = h.currentUsername(r)
	}
	n, err := h.db.ProblemAction(r.Context(), ids, op, value)
	if err != nil {
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	h.audit(r, "alert.problem_"+op, r.FormValue("ids"), fmt.Sprintf("%s (%d alerts)", value, n))
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

func (h *Handler) setAlertStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}
	if visible, all := h.visibleAlertIDs(r); !all && !visible[id] {
		http.Error(w, "Alert not found", http.StatusNotFound)
		return
	}
	if err := h.db.SetAlertStatus(r.Context(), id, status); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert."+status, id.String(), "")
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

// AlertAckAll acknowledges every open alert; AlertResolveAll resolves every
// non-resolved alert.
func (h *Handler) AlertAckAll(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.bulkAlertStatus(w, r, "acknowledged")
}
func (h *Handler) AlertResolveAll(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "alerts") {
		return
	}
	h.bulkAlertStatus(w, r, "resolved")
}

// AlertClearAll empties the list: every open alert is resolved and muted for 24 hours.
// It is no longer a delete — see db.DeleteAllAlerts for why deleting achieved nothing.
func (h *Handler) AlertClearAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.db.DeleteAllAlerts(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert.clear_all", "", strconv.FormatInt(n, 10))
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}

func (h *Handler) bulkAlertStatus(w http.ResponseWriter, r *http.Request, status string) {
	// "All" is all the user can see: a restricted user's click must not settle alerts
	// about devices hidden from them.
	if visible, all := h.visibleAlertIDs(r); !all {
		ids := make([]uuid.UUID, 0, len(visible))
		for id := range visible {
			ids = append(ids, id)
		}
		n, err := h.db.BulkSetAlertStatusByIDs(r.Context(), ids, status)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "alert."+status+"_all", "", strconv.FormatInt(n, 10))
		h.hub.PublishAlertUpdate()
		h.hxDone(w, r, "/alerts")
		return
	}
	n, err := h.db.BulkSetAlertStatus(r.Context(), status)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alert."+status+"_all", "", strconv.FormatInt(n, 10))
	h.hub.PublishAlertUpdate()
	h.hxDone(w, r, "/alerts")
}
