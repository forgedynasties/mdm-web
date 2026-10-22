package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// ReportAlertsByRestaurant renders the Daily Report's per-restaurant open-alert
// breakdown as an htmx fragment. The card re-fetches it on every mdm:alerts-update
// (fired by the global /alerts/events stream) so the breakdown tracks new, acked,
// and resolved alerts live without its own SSE connection.
func (h *Handler) ReportAlertsByRestaurant(w http.ResponseWriter, r *http.Request) {
	if h.access(r).hidesDevices() {
		http.Error(w, "This report covers the whole fleet.", http.StatusForbidden)
		return
	}
	rows, err := h.db.AlertsByRestaurant(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	total := 0
	for _, ra := range rows {
		total += ra.Total
	}
	// Cap the visible list; the header link covers the rest.
	shown := rows
	more := 0
	if len(shown) > 8 {
		more = len(shown) - 8
		shown = shown[:8]
	}
	// Alerts on devices with no venue (bench units) or on no device at all are not in
	// any row; say how many, so the rows plus this line add up to the bell.
	elsewhere := 0
	if all, err := h.db.CountOpenAlerts(r.Context(), false); err == nil && all > total {
		elsewhere = all - total
	}
	data := map[string]any{"Rows": shown, "Total": total, "More": more, "Elsewhere": elsewhere}
	// The bare fragment is what the Overview card embeds; a direct visit gets the
	// page, with the chrome and the Alerts rail around it.
	if r.Header.Get("HX-Request") != "" && r.Header.Get("HX-Boosted") == "" {
		h.tmpl.ExecuteTemplate(w, "report-alerts-by-restaurant", data)
		return
	}
	h.render(w, r, "report_alerts.html", data)
}

// ── Restaurants ─────────────────────────────────────────────────────────────────
//
// A restaurant is the venue a device physically lives in. Unlike free-form groups,
// each device belongs to at most one restaurant, and restaurants own the venue
// semantics (deployed flag, service window, health). See the design doc.

// parseFloatPtr parses an optional float form field; "" yields nil.
func parseFloatPtr(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// restaurantTimezones backs the timezone picker on the restaurant form, in the familiar
// "(GMT±HH:MM) City" style used by Google/Windows, ordered west→east. Values are IANA
// names so resolution is DST-aware (localTime); the offset shown is standard time.
type tzOption struct{ Value, Label string }

var restaurantTimezones = []tzOption{
	{"Pacific/Honolulu", "(GMT-10:00) Hawaii"},
	{"America/Anchorage", "(GMT-09:00) Alaska"},
	{"America/Los_Angeles", "(GMT-08:00) Pacific Time — Los Angeles, Vancouver"},
	{"America/Phoenix", "(GMT-07:00) Arizona — Phoenix"},
	{"America/Denver", "(GMT-07:00) Mountain Time — Denver"},
	{"America/Chicago", "(GMT-06:00) Central Time — Chicago"},
	{"America/Mexico_City", "(GMT-06:00) Mexico City"},
	{"America/New_York", "(GMT-05:00) Eastern Time — New York, Toronto"},
	{"America/Bogota", "(GMT-05:00) Bogotá, Lima"},
	{"America/Sao_Paulo", "(GMT-03:00) São Paulo"},
	{"UTC", "(GMT+00:00) UTC"},
	{"Europe/London", "(GMT+00:00) London, Dublin, Lisbon"},
	{"Africa/Lagos", "(GMT+01:00) West Africa — Lagos"},
	{"Europe/Paris", "(GMT+01:00) Paris, Madrid, Rome, Berlin"},
	{"Africa/Cairo", "(GMT+02:00) Cairo"},
	{"Africa/Johannesburg", "(GMT+02:00) Johannesburg"},
	{"Europe/Athens", "(GMT+02:00) Athens, Helsinki"},
	{"Asia/Jerusalem", "(GMT+02:00) Jerusalem"},
	{"Europe/Moscow", "(GMT+03:00) Moscow, Istanbul"},
	{"Asia/Dubai", "(GMT+04:00) Dubai, Abu Dhabi"},
	{"Asia/Karachi", "(GMT+05:00) Karachi, Tashkent"},
	{"Asia/Kolkata", "(GMT+05:30) India — Mumbai, Delhi, Kolkata"},
	{"Asia/Dhaka", "(GMT+06:00) Dhaka"},
	{"Asia/Bangkok", "(GMT+07:00) Bangkok, Jakarta, Hanoi"},
	{"Asia/Singapore", "(GMT+08:00) Singapore, Kuala Lumpur"},
	{"Asia/Hong_Kong", "(GMT+08:00) Hong Kong"},
	{"Asia/Shanghai", "(GMT+08:00) Beijing, Shanghai"},
	{"Australia/Perth", "(GMT+08:00) Perth"},
	{"Asia/Tokyo", "(GMT+09:00) Tokyo, Osaka"},
	{"Asia/Seoul", "(GMT+09:00) Seoul"},
	{"Australia/Sydney", "(GMT+10:00) Sydney, Melbourne"},
	{"Pacific/Auckland", "(GMT+12:00) Auckland"},
}

func (h *Handler) RestaurantList(w http.ResponseWriter, r *http.Request) {
	// Venues is a place of its own again. Sending it to the Fleet shell meant the
	// rail's "Venues" opened a page headed "9 of 20 devices online" with a Collections
	// rail — the nav said one thing and the page said another.
	// One row per venue (venues-demos.html, L1), built by collRows, which Fleet's
	// Restaurants, Groups and Releases views share.
	rows, sum, devices := h.collRowsDevs(r, "restaurants")
	data := map[string]any{
		"Title":   "Venues",
		"Venues":  rows,
		"Summary": sum,
	}
	// V1 (21 Oct 2026): cards and a rail; ?view=compact keeps the table.
	if r.URL.Query().Get("view") == "compact" {
		data["Compact"] = true
	} else {
		data["V1"] = buildVenuesV1(rows, h.venueExtras(r.Context(), devices), time.Now())
	}
	h.render(w, r, "restaurants.html", data)
}

// venueExtras gathers, per venue, its effective service hours, whether its staff PIN is
// still the default, its device types, and how many of its devices run hot right now.
func (h *Handler) venueExtras(ctx context.Context, devices []db.Device) map[string]vcExtra {
	out := map[string]vcExtra{}
	rests, _ := h.db.ListRestaurants(ctx)
	fleet, fleetErr := h.db.GetFleetServiceWindow(ctx)
	own, _ := h.db.ListRestaurantServiceWindows(ctx)
	pins, _ := h.db.RestaurantStaffPINs(ctx)
	for _, rest := range rests {
		e := vcExtra{TZ: rest.Timezone, Classes: map[string]int{}}
		if wnd, ok := own[rest.ID]; ok {
			e.Window = &wnd
		} else if fleetErr == nil {
			f := fleet
			e.Window = &f
		}
		if pin, ok := pins[rest.ID]; ok && pin == "" {
			e.DefaultPIN = true
		}
		out[rest.ID.String()] = e
	}
	online := h.onlineMap()
	stale := time.Duration(h.cfg.CheckinInterval()*3) * time.Second
	for i := range devices {
		d := &devices[i]
		if d.RestaurantID == nil {
			continue
		}
		e, ok := out[d.RestaurantID.String()]
		if !ok {
			continue
		}
		if c := d.Class(); c != "" {
			e.Classes[c]++
		}
		t, _, tok := deviceTempC(d.LatestExtra)
		if vcHotNow(online[d.ID] || time.Since(d.LastSeenAt) < stale, t, tok) {
			e.HotNow++
		}
		out[d.RestaurantID.String()] = e
	}
	return out
}

// venueRow is one venue on the Venues list.
type venueRow struct {
	ID, Name, Sub, Href, Text string // the collection: a venue, group or release
	H                         db.GroupHealth
	HasH                      bool
	Devices                   int
	Online, Offline, Dormant  int
	Pips                      []string // on | off | dormant, problems first, at most 20
	More                      int      // devices past the 20 squares
	Last                      time.Time
	Class                     string // ok | warn | danger | empty
	NeedsLook                 bool
}

type venueSummary struct{ Devices, Offline, Critical, NeedLook, WithOffline, Empty int }

var pipRank = map[string]int{"off": 0, "dormant": 1, "on": 2}

func (h *Handler) RestaurantNew(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "restaurant_form.html", map[string]any{
		"ScopesJSON": h.pickerScopesJSON(r),
		"Title":      "New restaurant",
		"Timezones":  restaurantTimezones,
	})
}

func (h *Handler) RestaurantCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/restaurants", http.StatusFound)
		return
	}
	contact, err := venueContactEmail(r.FormValue("contact_email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rest, err := h.db.CreateRestaurant(r.Context(), db.Restaurant{
		ContactEmail: contact,
		Name:         name,
		Address:      strings.TrimSpace(r.FormValue("address")),
		Latitude:     parseFloatPtr(r.FormValue("latitude")),
		Longitude:    parseFloatPtr(r.FormValue("longitude")),
		Timezone:     strings.TrimSpace(r.FormValue("timezone")),
		Notes:        strings.TrimSpace(r.FormValue("notes")),
	})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.create", rest.ID.String(), name)
	// Deploy any devices staged in the creation picker (moves them to this venue).
	if serials := h.keepVisibleSerials(r, parseSerialsField(r.Form["serials"])); len(serials) > 0 {
		if err := h.db.AssignDevicesToRestaurant(r.Context(), serials, rest.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
			for _, did := range ids {
				h.hub.PublishDeviceUpdate(did)
			}
			h.pushGuestToDevices(r.Context(), ids)
		}
	}
	http.Redirect(w, r, "/restaurants/"+rest.ID.String(), http.StatusFound)
}

// RestaurantNewDevices powers the deploy picker on the create form (no venue id yet):
// every assignable device, lab/unassigned shown first, deployed-elsewhere flagged.
func (h *Handler) RestaurantNewDevices(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	devices, err := h.db.ListAssignableDevices(r.Context(), uuid.Nil, q,
		r.URL.Query().Get("status"), r.URL.Query().Get("battery"), 60, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices) // only devices the user can see
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": true,
	})
}

// powerWindowChoices are the windows the venue page's Power & usage card offers. Each
// one is a whole number of days because every figure behind the card comes from
// device_daily_stats, which has one row per device per day — a 36-hour window would be
// rounded to a day anyway and only invite "which day is half missing?".
var powerWindowChoices = []int{7, 14, 30}

// powerWindowDays reads the ?days= choice, falling back to a week. Anything not offered
// is refused rather than clamped: a hand-typed 365 would scan a year of rollups per
// venue page load.
func powerWindowDays(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 7
	}
	for _, c := range powerWindowChoices {
		if c == n {
			return n
		}
	}
	return 7
}

func (h *Handler) RestaurantDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	win, hasOwn, _ := h.db.GetRestaurantServiceWindow(r.Context(), id)
	// Online the way the Venues list counts it: connected, or checked in within three
	// check-in intervals, so the two pages agree.
	online := h.onlineMap()
	onCount, stale := 0, time.Duration(h.cfg.CheckinInterval()*3)*time.Second
	for _, d := range devices {
		if online[d.ID] || time.Since(d.LastSeenAt) < stale {
			onCount++
		}
	}
	data := map[string]any{
		"Title":               rest.Name,
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              online,
		"OnlineCount":         onCount,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ServiceWindow":       windowView(id.String(), rest.Name, win, hasOwn),
	}
	// The networks the venue's devices join (venue_wifi.go).
	data["VenueWifi"] = h.venueWifiRows(r.Context(), id, devices)
	// Self-lock (S1): minutes, 0 = off.
	data["SelfLockMinutes"], _ = h.db.RestaurantSelfLockMinutes(r.Context(), id)
	data["StaffPIN"], _ = h.db.RestaurantStaffPIN(r.Context(), id)
	// The Device lock card (venue_kiosk.go). Wired into GroupDetail by mistake on 11 Oct,
	// so the venue page never showed it until 18 Oct.
	data["Kiosk"] = h.venueKiosk(r, id)
	// Profile and Location cards (E1, 19 Oct): edited in place; ?open= names the card a
	// save sent you back to.
	data["Location"] = h.venueLocationView(rest, devices)
	data["Timezones"] = restaurantTimezones
	data["Open"] = r.URL.Query().Get("open")
	// People (U3, 20 Oct): the venue's owner and staff accounts, invited from here.
	data["People"] = h.venuePeople(r.Context(), []uuid.UUID{id})
	_, _, data["IconSource"], _ = h.db.RestaurantIcon(r.Context(), id)
	// Power and usage over the chosen window: uptime, guest-pad time and what it costs
	// the tablet's own battery, and the battery levels staff plug and unplug at. The
	// same number of days feeds the tiles and the per-day strip below them, so the
	// bars are always the evidence for the figure above them.
	powerDays := powerWindowDays(r.URL.Query().Get("days"))
	data["PowerRanges"] = powerWindowChoices
	if m, err := h.db.SiteMetricsFor(r.Context(), id, powerDays, time.Time{}); err == nil {
		data["Metrics"] = m
		// The evidence behind the headline: the same window, day by day.
		if daily, err := h.db.SiteMetricsDaily(r.Context(), id, powerDays, time.Time{}); err == nil {
			data["MetricsDaily"] = daily
			max := 1.0
			for _, x := range daily {
				if x.PoweredMinutes > max {
					max = x.PoweredMinutes
				}
			}
			data["MetricsDailyMax"] = max
		}
	}
	data["ScopesJSON"] = h.pickerScopesJSON(r)
	if r.URL.Query().Get("partial") == "kpis" { // the count cards, refreshed on restaurant-updated
		_ = h.tmpl.ExecuteTemplate(w, "restaurant-kpis", h.withRole(r, data))
		return
	}
	if r.URL.Query().Get("partial") == "power" { // range buttons on the Power & usage card
		_ = h.tmpl.ExecuteTemplate(w, "restaurant-power", h.withRole(r, data))
		return
	}
	data["AutoEnroll"], _ = h.db.RestaurantAutoEnroll(r.Context(), id)
	data["Updates"] = h.venueUpdatesView(r.Context(), id, win, rest.Timezone, time.Now())
	data["P1"] = h.venueP1(r, rest, devices, online, stale, win, data["StaffPIN"], data["Updates"].(venueUpdatesView))
	h.render(w, r, "restaurant_detail.html", data)
}

// venueP1 gathers what the P1 banner, cards and pill chart read (venue_p1.go).
func (h *Handler) venueP1(r *http.Request, rest *db.Restaurant, devices []db.Device, online map[uuid.UUID]bool,
	stale time.Duration, win db.ServiceWindow, pin any, upd venueUpdatesView) venueP1 {
	ctx := r.Context()
	id := rest.ID
	now := time.Now()
	in := vpInputs{ID: id, Name: rest.Name, Window: vpWindow(r.URL.Query().Get("window")), Now: now}
	in.StaffPIN, _ = pin.(string)
	in.Updates = upd
	for _, d := range devices {
		dv := vpDev{Serial: d.SerialNumber, Firmware: d.AgentKind == product.KindFirmware}
		dv.ID, dv.Class, dv.Venue = d.ID, d.Class(), &id
		dv.Online = online[d.ID] || now.Sub(d.LastSeenAt) < stale
		dv.SeenAt, dv.Battery, dv.HasBatt = d.LastSeenAt, d.BatteryPct, d.HasBattery()
		if t, _, ok := deviceTempC(d.LatestExtra); ok {
			t := t
			dv.TempC = &t
		}
		in.Devices = append(in.Devices, dv)
	}
	if win.OpenMin != win.CloseMin { // equal = never open
		in.Open = win.InService(now, rest.Timezone)
		in.OpenAt, in.CloseAt = hhmm(win.OpenMin), hhmm(win.CloseMin)
	}
	acc := h.access(r)
	if alerts, err := h.db.ListActiveAlerts(ctx, 300); err == nil {
		for _, a := range acc.keepVisibleAlerts(alerts) {
			if a.RestaurantID == id.String() {
				in.Alerts = append(in.Alerts, a)
			}
		}
	}
	if groups, err := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7); err == nil {
		for i := range groups {
			if groups[i].GroupID == id {
				in.Health = &groups[i]
			}
		}
	}
	tr, err := h.db.OverviewTrend(ctx, db.OverviewScope{Venue: &id}, ovTrendDays(in.Window))
	if err != nil {
		log.Printf("[venue] trend %s: %v", id, err)
	}
	in.Trend = tr
	return buildVenueP1(in)
}

// reportBar is one day in the weekly report's powered-hours strip, already reduced
// to what the template draws: a label, the per-device hours, and a bar height.
type reportBar struct {
	Label string
	Hours float64
	Pct   int
}

// RestaurantReport renders a venue's weekly report: the same figures the venue page
// shows in its Power & usage card, plus a row per device, on a standalone printable
// page. Everything comes from device_daily_stats rollups, never raw check-ins.
// lastFullWeek returns the Monday and Sunday of the most recently COMPLETED week.
//
// The report used a rolling seven days ending now, which means a report opened on
// Wednesday covers half of this week and half of last, and the same link shows
// different numbers depending on when it is clicked. A weekly report should describe a
// week: one that has finished and will not change again.
//
// The Sunday is strictly before today, so on a Sunday the week that ends tonight is not
// yet claimed as complete.
func lastFullWeek(now time.Time) (from, to time.Time) {
	today := now.UTC().Truncate(24 * time.Hour)
	back := int(today.Weekday()) // Sunday = 0
	if back == 0 {
		back = 7
	}
	to = today.AddDate(0, 0, -back)
	return to.AddDate(0, 0, -6), to
}

// reportRange is a window as a reader says it: "Mon 21 Sep – Sun 27 Sep 2026", or
// "Mon 28 Sep – today" for the week still in progress.
func reportRange(w reportWindow) string {
	if w.Current {
		return w.From.Format("Mon 2 Jan") + " – today"
	}
	return w.From.Format("Mon 2 Jan") + " – " + w.To.Format("Mon 2 Jan 2006")
}

func (h *Handler) RestaurantReport(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	win, weeks := pickReportWeek(r.URL.Query().Get("week"), time.Now())
	v, err := h.buildVenueReport(r.Context(), id, win, h.access(r).visible)
	if err != nil {
		log.Printf("[report] device weeks %s: %v", id, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title":             rest.Name + " — Weekly report",
		"Restaurant":        rest,
		"Days":              win.Days,
		"WindowFrom":        win.From,
		"WindowTo":          win.To,
		"Week":              win,
		"Weeks":             weeks,
		"Bars":              v.Bars,
		"DeviceWeeks":       v.DeviceWeeks,
		"WindowHours":       v.WindowHours(),
		"PDFURL":            fmt.Sprintf("/restaurants/%s/report.pdf?week=%s", id, win.Value()),
		"AvgPoweredMinutes": v.AvgPoweredMinutes,
		"AvgPluggedMinutes": v.AvgPluggedMinutes,
		"AvgPluggedPct":     v.AvgPluggedPct,
		"AvgPadMinutes":     v.AvgPadMinutes,
		"AvgStandbyMinutes": v.AvgStandbyMinutes,
	}
	if v.Metrics != nil {
		data["Metrics"] = *v.Metrics
	}
	// The plain-language views (report_views.go). The standard report stays the default,
	// and is the only thing built when no view is asked for, so it costs nothing extra.
	view, views := pickReportView(r.URL.Query().Get("view"))
	data["View"], data["Views"] = view, views
	if view != "" {
		st, err := h.venueStoryFor(r.Context(), rest, v)
		if err != nil {
			log.Printf("[report] story %s: %v", id, err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		data["Story"] = st
	}
	h.render(w, r, "restaurant_report.html", data)
}

// pctCapped is a share of a whole as a whole number, never past 100 — the report's
// meters are bars, and a bar past its track reads as a bug rather than as good news.
func pctCapped(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	p := int(part * 100 / whole)
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}

// reportRecipientDomain is the only domain a venue report may be sent to. A report
// names every device at a site and how it behaved all week, so it goes to colleagues
// and nowhere else — an open recipient box on an internal dashboard is a data-exfil
// path that looks like a feature.
const reportRecipientDomain = "@aioapp.com"

// RestaurantReportEmail mails a venue's weekly report. The email carries the figures
// themselves rather than a bare link, so it is readable without a login, and a link
// back for the full per-device table.
func (h *Handler) RestaurantReportEmail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	to := strings.TrimSpace(r.FormValue("to"))
	if to == "" || !strings.Contains(to, "@") {
		h.reportMailResult(w, r, false, "Enter an email address")
		return
	}
	if !strings.HasSuffix(strings.ToLower(to), reportRecipientDomain) {
		h.reportMailResult(w, r, false, "Reports can only be sent to "+reportRecipientDomain+" addresses")
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}

	// Same window as the page it was sent from, or the mail and the report it points
	// at would quote different numbers for the same venue.
	win, _ := pickReportWeek(r.FormValue("week"), time.Now())
	v, err := h.buildVenueReport(r.Context(), id, win, h.access(r).visible)
	if err != nil {
		log.Printf("[report] email device weeks %s: %v", id, err)
		h.reportMailResult(w, r, false, "Could not build the report")
		return
	}
	var m db.SiteMetrics
	if v.Metrics != nil {
		m = *v.Metrics
	}
	visible := v.DeviceWeeks

	subject := fmt.Sprintf("%s — weekly device report", rest.Name)
	// The link is the PDF: it opens without a dashboard login, and the server renders
	// it on first request.
	reportURL := h.ReportPDFURL(r, id, win.From)
	body := reportEmailHTML(rest.Name, win, m, visible, v.AvgPadMinutes, reportURL)
	if err := h.mail.Send(r.Context(), to, subject, body); err != nil {
		log.Printf("[report] send to %s: %v", to, err)
		h.reportMailResult(w, r, false, "Sending failed — check the server log")
		return
	}
	h.audit(r, "restaurant.report.email", rest.Name, to)
	h.reportMailResult(w, r, true, "Sent to "+to)
}

// reportMailResult answers the htmx form with a one-line status, or redirects back to
// the report when the browser posted the form without JS.
func (h *Handler) reportMailResult(w http.ResponseWriter, r *http.Request, ok bool, msg string) {
	if !hxReq(r) {
		h.hxRedirect(w, r, r.Header.Get("Referer"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	class := "bad"
	if ok {
		class = "ok"
	}
	fmt.Fprintf(w, `<span class="%s">%s</span>`, class, template.HTMLEscapeString(msg))
}

// reportEmailHTML renders the report for an email client. Tables and inline styles
// throughout, no stylesheet, no flex or grid and no CSS variables: Outlook renders
// with Word's engine, which supports none of them. The palette is the dashboard's so
// the mail reads as the same product as the page it came from.
func reportEmailHTML(venue string, win reportWindow, m db.SiteMetrics, weeks []db.DeviceWeek, avgPadMinutes float64, reportURL string) string {
	const (
		ink    = "#2c2c2b"
		muted  = "#77736f"
		faint  = "#8a8a92"
		canvas = "#f7f7f6"
		line   = "#e6e5e3"
		coral  = "#ff654f"
	)
	hrs := func(min float64) string { return fmt.Sprintf("%.1f", min/60) }
	esc := template.HTMLEscapeString

	// tile is one headline figure: a big number over a small caption.
	tile := func(label, value, unit, note string) string {
		return fmt.Sprintf(`<td width="25%%" valign="top" height="104" style="height:104px;padding:14px 16px;border-left:1px solid %s;">`+
			`<div style="font:600 11px -apple-system,Segoe UI,Roboto,sans-serif;letter-spacing:.06em;text-transform:uppercase;color:%s;">%s</div>`+
			`<div style="font:300 30px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:8px 0 0;">%s<span style="font-size:14px;color:%s;"> %s</span></div>`+
			`<div style="font:400 11.5px -apple-system,Segoe UI,Roboto,sans-serif;line-height:1.35;color:%s;padding:6px 0 0;">%s</div></td>`,
			line, muted, label, ink, value, muted, unit, muted, note)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><body style="margin:0;padding:0;background:%s;">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%" style="background:%s;padding:24px 12px;"><tr><td align="center">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="680" style="width:680px;max-width:100%%;background:#ffffff;border:1px solid %s;border-radius:14px;overflow:hidden;">`, line)

	// Header band.
	fmt.Fprintf(&b, `<tr><td style="padding:22px 24px 18px;border-bottom:1px solid %s;">`+
		`<div style="font:600 11px -apple-system,Segoe UI,Roboto,sans-serif;letter-spacing:.11em;text-transform:uppercase;color:%s;">Weekly device report</div>`+
		`<div style="font:600 26px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:6px 0 0;">%s</div>`+
		`<div style="font:400 13px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;padding:6px 0 0;">%s · %d device%s reporting</div>`+
		`</td></tr>`,
		line, coral, ink, esc(venue), muted, esc(reportRange(win)), len(weeks), plural(len(weeks)))

	// Headline figures.
	uptimeNote := "of the measured window"
	padNote := "per device, a customer's phone on the T7 charging pad"
	costValue, costUnit, costNote := "—", "", "under 10 min of wireless charging off the charger"
	if m.HasPadDrain() {
		costValue = fmt.Sprintf("%.2f", m.PadDrainPctPerMin())
		costUnit = "%/min"
		costNote = fmt.Sprintf("measured over %s hrs off the charger, all devices", hrs(m.PadDrainMinutes))
	}
	standbyValue, standbyUnit, standbyNote := "", "", ""
	if m.HasStandby() {
		standbyValue = fmt.Sprintf("%d", m.StandbyPct())
		standbyUnit = "%"
		standbyNote = "of powered time, screen off"
	}
	fmt.Fprintf(&b, `<tr><td style="padding:0;"><table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%"><tr>`)
	b.WriteString(strings.Replace(tile("Average uptime", fmt.Sprintf("%d", m.UptimeFullPct()), "%", uptimeNote), "border-left:1px solid "+line+";", "", 1))
	b.WriteString(tile("Wireless charging", hrs(avgPadMinutes), "hrs", padNote))
	b.WriteString(tile("Battery drain due to wireless charging", costValue, costUnit, costNote))
	// Omitted entirely where the firmware does not report screen state, matching the
	// web report: a tile that only explains its own emptiness is worse than one fewer.
	if m.HasStandby() {
		b.WriteString(tile("Standby", standbyValue, standbyUnit, standbyNote))
	}
	fmt.Fprintf(&b, `</tr></table></td></tr>`)

	// No per-device table here. An email is a summary — the headline figures are what
	// someone reads on a phone — and a table of every device made it long, made it wrap
	// badly in narrow reading panes, and duplicated a page that renders it properly.
	// The full breakdown is the PDF, one click away.
	fmt.Fprintf(&b, `<tr><td align="center" style="padding:22px 16px 4px;">`+
		`<a href="%s" style="display:inline-block;background:%s;color:#ffffff;text-decoration:none;`+
		`font:600 14px -apple-system,Segoe UI,Roboto,sans-serif;padding:12px 22px;border-radius:8px;">`+
		`Open the full report (PDF)</a></td></tr>`, esc(reportURL), coral)
	fmt.Fprintf(&b, `<tr><td align="center" style="padding:8px 16px 18px;`+
		`font:400 12px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;">`+
		`Every device, with its uptime, wireless charging and time on the charger.`+
		`</td></tr>`, muted)

	fmt.Fprintf(&b, `<tr><td style="padding:14px 16px 18px;font:400 11.5px -apple-system,Segoe UI,Roboto,sans-serif;color:%s;">`+
		`Every figure is measured over the device-days that actually reported, so a device deployed midweek shortens its own window instead of dragging the venue down. A dash means the reading was never measured, not that it was zero.`+
		`</td></tr>`, muted)

	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

// RestaurantDevicesModal serves just the "deploy devices" card + members list —
// the fleet-page kebab menu's "Add/remove devices" popup loads this via htmx
// instead of navigating to the full venue page.
func (h *Handler) RestaurantDevicesModal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	h.render(w, r, "restaurant-devices-modal", map[string]any{
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"Lean":                true,
	})
}

// peakRangeView / peakWindowView present a scope's peak-hour ranges for editing. When
// the restaurant has no rows of its own the fleet-default set is shown with Inherited=true.
type peakRangeView struct{ Start, End string }
type peakWindowView struct {
	Ranges    []peakRangeView
	Inherited bool // showing the fleet default (restaurant has no own rows)
}

// peakView resolves the peak ranges to display for a restaurant: its own rows if any,
// otherwise the fleet default (marked Inherited). Pass nil for the fleet default itself.
func (h *Handler) peakView(ctx context.Context, restaurantID *uuid.UUID) peakWindowView {
	rows, _ := h.db.GetPeakWindows(ctx, restaurantID)
	inherited := false
	if len(rows) == 0 && restaurantID != nil {
		rows, _ = h.db.GetPeakWindows(ctx, nil)
		inherited = true
	}
	v := peakWindowView{Inherited: inherited}
	for _, r := range rows {
		v.Ranges = append(v.Ranges, peakRangeView{Start: hhmm(r.StartMin), End: hhmm(r.EndMin)})
	}
	return v
}

// RestaurantEdit was the separate Edit page; a venue is edited on its own page now
// (Profile and Location cards, E1 picked 19 Oct 2026).
func (h *Handler) RestaurantEdit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/restaurants/"+id.String()+"?open=profile", http.StatusSeeOther)
}

func (h *Handler) RestaurantUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	// Only a form that carries the field changes it: an older form, or one posted
	// without it, leaves the contact email alone instead of blanking it.
	_, hasContact := r.Form["contact_email"]
	contact, err := venueContactEmail(r.FormValue("contact_email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateRestaurant(r.Context(), db.Restaurant{
		ID:        id,
		Name:      name,
		Address:   strings.TrimSpace(r.FormValue("address")),
		Latitude:  parseFloatPtr(r.FormValue("latitude")),
		Longitude: parseFloatPtr(r.FormValue("longitude")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
		Notes:     strings.TrimSpace(r.FormValue("notes")),
	}); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if hasContact {
		if err := h.db.SetRestaurantContactEmail(r.Context(), id, contact); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}
	h.audit(r, "restaurant.update", id.String(), name)
	h.pushGuestToRestaurant(r.Context(), id) // the name is on every table's welcome
	localRedirect(w, r, "/restaurants/"+id.String())
}

// RestaurantRename changes only a restaurant's display name — the fleet toolbar's
// inline rename posts here instead of RestaurantUpdate, which overwrites every
// field and would blank address/timezone/notes if only "name" were submitted.
func (h *Handler) RestaurantRename(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.RenameRestaurant(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.rename", id.String(), name)
	h.pushGuestToRestaurant(r.Context(), id)
	localRedirect(w, r, "/devices?restaurant="+id.String())
}

func (h *Handler) RestaurantDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	// Its tablets go back to the lab, so they stop showing its name and Wi-Fi.
	members, _ := h.db.GetDeviceIDsByRestaurantIDs(r.Context(), []uuid.UUID{id})
	if err := h.db.DeleteRestaurant(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.delete", id.String(), "")
	h.pushGuestToDevices(r.Context(), members)
	localRedirect(w, r, "/restaurants")
}

// RestaurantAssignDevices assigns one or more devices (by serial) to this restaurant.
func (h *Handler) RestaurantAssignDevices(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	serials := h.keepVisibleSerials(r, parseSerialsField(r.Form["serials"]))
	if len(serials) == 0 {
		http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
		return
	}
	// Atomic bulk assign: avoids leaving a partial set assigned if one row errors.
	if err := h.db.AssignDevicesToRestaurant(r.Context(), serials, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.assign", id.String(), fmt.Sprintf("%d devices", len(serials)))
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
		h.pushGuestToDevices(r.Context(), ids)
	}
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
}

// RestaurantDevicePicker renders the searchable candidate-device list for the assign
// picker (devices not already in this restaurant; unassigned first).
func (h *Handler) RestaurantDevicePicker(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	battery := r.URL.Query().Get("battery")
	devices, err := h.db.ListAssignableDevices(r.Context(), id, q, status, battery, 25, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices) // only devices the user can see
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": true,
	})
}

// RestaurantRemoveDevice unassigns a device from this restaurant (back to lab).
func (h *Handler) RestaurantRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if len(h.keepVisibleSerials(r, []string{serial})) == 0 {
		http.NotFound(w, r)
		return
	}
	if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, nil); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.unassign", id.String(), serial)
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
		h.pushGuestToDevices(r.Context(), []uuid.UUID{device.ID})
	}
	h.hxDone(w, r, "/restaurants/"+id.String(), "restaurant-updated")
}

// RestaurantMembers renders just the devices card of a restaurant for an in-place
// htmx refresh (fired by "restaurant-updated" after a device is removed).
func (h *Handler) RestaurantMembers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.Error(w, "Restaurant not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "restaurant-members", h.withRole(r, map[string]any{
		"Restaurant":          rest,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		// Set by the fleet-page devices popup's own hx-get URL (?lean=1) — see the
		// matching comment in GroupMembers.
		"Lean": r.URL.Query().Get("lean") == "1",
		// The venue page's card has its own header and type tabs (D1).
		"NoHead": r.URL.Query().Get("head") == "0",
	}))
}

// RestaurantSetServiceWindow upserts (or resets) this restaurant's service window.
func (h *Handler) RestaurantSetServiceWindow(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") { // "Manage groups & venues", like its siblings
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	if r.FormValue("action") == "reset" {
		_ = h.db.DeleteServiceWindow(r.Context(), id)
		h.audit(r, "restaurant.service_window", id.String(), "reset")
		h.pushGuestToRestaurant(r.Context(), id) // the welcome screens' closed-hours screen
		http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
		return
	}
	sw := db.ServiceWindow{
		RestaurantID:  &id,
		OpenMin:       parseHHMM(r.FormValue("open"), 420),
		CloseMin:      parseHHMM(r.FormValue("close"), 1380),
		NightOpenMin:  parseHHMM(r.FormValue("night_open"), 1410),
		NightCloseMin: parseHHMM(r.FormValue("night_close"), 360),
		TZ:            strings.TrimSpace(r.FormValue("timezone")),
	}
	if err := h.db.SetServiceWindow(r.Context(), sw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.service_window", id.String(), "")
	h.pushGuestToRestaurant(r.Context(), id) // the welcome screens' closed-hours screen
	http.Redirect(w, r, "/restaurants/"+id.String(), http.StatusFound)
}

// RestaurantDailyStatsJSON powers the restaurant Trends chart.
func (h *Handler) RestaurantDailyStatsJSON(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			days = n
		}
	}
	stats, err := h.db.GetRestaurantDailyStats(r.Context(), id, days)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []db.GroupDailyStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// DeviceSetRestaurant assigns/clears a single device's restaurant from the device page.
func (h *Handler) DeviceSetRestaurant(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	ridStr := strings.TrimSpace(r.FormValue("restaurant_id"))
	var rid *uuid.UUID
	if ridStr != "" {
		id, err := uuid.Parse(ridStr)
		if err != nil {
			http.Error(w, "Invalid restaurant", http.StatusBadRequest)
			return
		}
		rid = &id
	}
	if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, rid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.auditDev(r, "device.restaurant", device.ID, serial, ridStr)
		h.hub.PublishDeviceUpdate(device.ID)
		h.pushGuestToDevices(r.Context(), []uuid.UUID{device.ID})
	} else {
		h.audit(r, "device.restaurant", serial, ridStr)
	}
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// venueContactEmail checks the venue form's contact email: empty (no contact), or one
// plain address — never a display name or a list, since it becomes a To: header.
func venueContactEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Name != "" || a.Address != raw || len(raw) > 254 || strings.ContainsAny(raw, ",; ") {
		return "", fmt.Errorf("Contact email must be one email address")
	}
	return raw, nil
}
