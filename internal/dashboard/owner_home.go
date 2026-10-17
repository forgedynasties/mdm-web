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
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

func ownerAllowedPath(p string) bool {
	if p == "/" || p == "/profile" || p == "/logout" || p == "/maintenance" || p == "/alerts/events" {
		return true
	}
	for _, pre := range []string{"/devices/", "/icon/", "/tour/", "/overview/layout", "/theme"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// OwnerHome is the restaurant owner's whole dashboard. Which devices it shows
// comes from their access policy (allow "view" per restaurant). Everything is
// phrased for someone who runs a restaurant, not a fleet: a health score with a
// sentence, a to-do list only when something needs a hand, today's service as
// an hourly strip, station cards, and a week-over-week report card.
func (h *Handler) OwnerHome(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "owner_home.html", h.ownerHomeData(r))
}

type ownerStation struct {
	Serial, Name, Product string
	Online, HasBattery    bool
	Battery               int
	Charging              bool
	Pad                   string // "docked" | "vacant" | "faulty" | ""
	Kiosk                 bool
	LastSeen              time.Time
	Temp                  string
	Crashes               int
	State                 string        // ok | warn | bad
	Word                  string        // roster state word
	Do                    template.HTML // what to do (may contain <b>)
}

type ownerTodo struct {
	Sev, Title, Body, Serial string
	Kind, Station            string // Kind groups like problems into one "Needs you" row
}

type ownerVenue struct {
	Name            string
	Stations        []ownerStation
	Online, Offline int
}

type ownerHourCell struct {
	Label   string
	Pct     int
	Service bool
	Now     bool
}

type ownerDelta struct {
	Label, Value, Prev string
	Up, Better         bool
	Has                bool
}

// ownerNeedGroup is one "Needs you" row: like problems grouped (7 stations offline,
// 13 pads misplaced, …) instead of one row per station per problem.
type ownerNeedGroup struct {
	Sev, Kind, Title, Body string
	Stations               []ownerTodo
	More                   int // stations beyond the first few chips
}

// ownerRoster is the station list with the counts the rest of the page derives from.
type ownerRoster struct {
	venues                 []*ownerVenue
	todos                  []ownerTodo
	total, online, lowBatt int
	restaurantID           *uuid.UUID
}

// ownerHomeData is the load step of OwnerHome.
func (h *Handler) ownerHomeData(r *http.Request) map[string]any {
	ctx := r.Context()
	acc := h.access(r)
	ids := acc.visibleIDs()
	if ids == nil {
		ids = []uuid.UUID{}
	}
	var devices []db.Device
	if len(ids) > 0 {
		var err error
		devices, err = h.db.ListDevices(ctx, db.DeviceFilter{OnlyIDs: ids, Connected: h.connectedSlice()}, 0, 500, "serial", "asc")
		if err != nil {
			log.Printf("[owner] list devices: %v", err)
		}
	}
	connected := h.hub.ConnectedIDsForDisplay()
	nick, _ := h.db.GetNicknames(ctx, ids)
	incidents, incCur, incPrev, _ := h.db.IncidentCounts(ctx, ids, 7)

	ro := ownerRosterOf(devices, connected, nick, incidents)
	score, scoreClass, scoreWord := ownerHealthScore(ro, incCur)
	cells, serviceUptime, serviceLabel, loc := h.ownerServiceStrip(ctx, ids, ro)
	report := h.ownerWeekReport(ctx, ro.restaurantID, incCur, incPrev)

	hour := time.Now().In(loc).Hour()
	greeting := "Good evening"
	if hour < 12 {
		greeting = "Good morning"
	} else if hour < 17 {
		greeting = "Good afternoon"
	}
	name := h.currentUsername(r)
	if u, err := h.db.GetUserByUsername(ctx, name); err == nil && u != nil && u.FirstName != "" {
		name = u.FirstName
	}
	todos := ro.todos
	sort.SliceStable(todos, func(i, j int) bool {
		rank := map[string]int{"bad": 0, "warn": 1, "info": 2}
		return rank[todos[i].Sev] < rank[todos[j].Sev]
	})
	headline, lede := ownerHeadline(ro.venues, ro.total, hour)
	needGroups := ownerNeedGroups(todos)

	// The owner's own weekly report, inline. This card used to be a mock-up captioned
	// "Preview"; it now carries the real figures for the last finished week — the same
	// numbers the emailed report and the PDF quote, from the same query, so an owner
	// comparing the three never sees three different answers.
	reportURL, reportFrom, reportTo := "", time.Time{}, time.Time{}
	var reportMetrics *db.SiteMetrics
	if ro.restaurantID != nil {
		reportFrom, reportTo = lastFullWeek(time.Now())
		// The PDF, which the server renders on first request and caches.
		reportURL = h.ReportPDFURL(r, *ro.restaurantID, reportFrom)
		if m, err := h.db.SiteMetricsFor(ctx, *ro.restaurantID, 7, reportTo); err == nil && m.DeviceDays > 0 {
			reportMetrics = &m
		}
	}

	return map[string]any{
		"ReportURL":     reportURL,
		"ReportMetrics": reportMetrics,
		"ReportFrom":    reportFrom,
		"ReportTo":      reportTo,
		"TodoGroups":    needGroups,
		"TodoCount":     len(todos),
		"Headline":      headline,
		"Lede":          template.HTML(lede),
		"Title":         "Home",
		"Greeting":      greeting,
		"Name":          name,
		"DateLine":      time.Now().In(loc).Format("Monday 2 January"),
		"TimeLine":      strings.ToLower(time.Now().In(loc).Format("3:04 pm")),
		"Venues":        ro.venues,
		"Total":         ro.total,
		"Online":        ro.online,
		"Offline":       ro.total - ro.online,
		"LowBatt":       ro.lowBatt,
		"Score":         score,
		"ScoreClass":    scoreClass,
		"ScoreWord":     scoreWord,
		"Todos":         todos,
		"Hours":         cells,
		"ServiceUptime": serviceUptime,
		"ServiceLabel":  serviceLabel,
		"Report":        report,
		"VenueCount":    len(ro.venues),
	}
}

// ownerRosterOf builds the station cards per venue, worst first, and the to-do items
// they raise.
func ownerRosterOf(devices []db.Device, connected map[uuid.UUID]struct{}, nick map[uuid.UUID]string, incidents map[uuid.UUID]int) ownerRoster {
	byName := map[string]*ownerVenue{}
	var order []string
	var ro ownerRoster
	ro.total = len(devices)
	for _, d := range devices {
		name := d.RestaurantName
		if name == "" {
			name = "Unassigned"
		}
		if ro.restaurantID == nil && d.RestaurantID != nil {
			ro.restaurantID = d.RestaurantID
		}
		v, ok := byName[name]
		if !ok {
			v = &ownerVenue{Name: name}
			byName[name] = v
			order = append(order, name)
		}
		_, on := connected[d.ID]
		st := ownerStation{Serial: d.SerialNumber, Name: nick[d.ID], Product: product.Label(d.Product), Online: on,
			HasBattery: d.HasBattery(), Battery: d.BatteryPct, Kiosk: d.KioskEnabled, LastSeen: d.LastSeenAt, Crashes: incidents[d.ID]}
		if st.Name == "" {
			st.Name = d.SerialNumber
		}
		if len(d.LatestExtra) > 0 {
			var ex map[string]json.RawMessage
			if json.Unmarshal(d.LatestExtra, &ex) == nil {
				if v, ok := ex["charging"]; ok {
					var b bool
					if json.Unmarshal(v, &b) == nil {
						st.Charging = b
					}
				}
			}
			if w := wlcIntFromExtra(d.LatestExtra); w != nil {
				st.Pad = map[int]string{0: "vacant", 1: "docked", 2: "faulty"}[*w]
			}
			st.Temp = strings.TrimSpace(batteryTempStr(d.LatestExtra))
		}
		st.State = "ok"
		if on {
			ro.online++
			v.Online++
		} else {
			v.Offline++
			st.State = "bad"
			ro.todos = append(ro.todos, ownerTodo{Sev: "bad", Kind: "offline", Station: st.Name, Title: st.Name + " is offline", Serial: d.SerialNumber,
				Body: "Last seen " + timeSinceStr(d.LastSeenAt) + ". Check that it is powered on and connected to Wi-Fi."})
		}
		if st.HasBattery && st.Battery < 20 {
			ro.lowBatt++
			if !st.Charging {
				if st.State == "ok" {
					st.State = "warn"
				}
				ro.todos = append(ro.todos, ownerTodo{Sev: "warn", Kind: "battery", Station: st.Name, Title: st.Name + " is at " + strconv.Itoa(st.Battery) + "% and not charging", Serial: d.SerialNumber,
					Body: "Put it back on its charging dock so it lasts through service."})
			}
		}
		if st.Pad == "faulty" {
			if st.State == "ok" {
				st.State = "warn"
			}
			ro.todos = append(ro.todos, ownerTodo{Sev: "warn", Kind: "pad", Station: st.Name, Title: st.Name + "'s charging pad is misplaced", Serial: d.SerialNumber,
				Body: "Connection issue between the pad and the device. Please place it again; if it keeps happening, let the MDM team know."})
		}
		if on && !d.KioskEnabled {
			ro.todos = append(ro.todos, ownerTodo{Sev: "info", Kind: "kiosk", Station: st.Name, Title: st.Name + " is not locked to the app", Serial: d.SerialNumber,
				Body: "Guests can leave the app. Ask the MDM team to turn locked mode back on."})
		}
		if st.Crashes >= 3 {
			ro.todos = append(ro.todos, ownerTodo{Sev: "warn", Kind: "crashes", Station: st.Name, Title: st.Name + " crashed " + strconv.Itoa(st.Crashes) + " times this week", Serial: d.SerialNumber,
				Body: "The MDM team can see the details; a restart usually helps in the meantime."})
		}
		switch {
		case !on:
			st.Word, st.Do = "Offline", template.HTML("<b>Check the power cable</b> and that it is on the Wi-Fi.")
		case st.HasBattery && st.Battery < 20 && !st.Charging:
			st.Word, st.Do = "Not charging", template.HTML("<b>Put it back on the dock.</b> At "+strconv.Itoa(st.Battery)+"% it will not last the shift.")
		case st.Pad == "faulty":
			st.Word, st.Do = "Pad misplaced", template.HTML("<b>Please place it again</b> on the charging pad. If it keeps happening, tell the MDM team.")
		default:
			st.Word = "Ready"
			do := "Docked, locked, online."
			switch {
			case !st.HasBattery:
				do = "Plugged in, locked, online."
			case st.Charging && st.Battery < 100:
				do = "Charging, locked, online."
			case st.Pad == "vacant":
				do = "Off the dock but charged. Locked, online."
			}
			if !d.KioskEnabled {
				do = strings.Replace(do, "locked, ", "", 1) + " Not locked to the menu."
			}
			if st.Crashes > 0 {
				do += " Restarted " + strconv.Itoa(st.Crashes) + " time" + map[bool]string{true: "s", false: ""}[st.Crashes != 1] + " this week."
			}
			st.Do = template.HTML(do)
		}
		v.Stations = append(v.Stations, st)
	}
	rankOf := map[string]int{"bad": 0, "warn": 1, "ok": 2}
	for _, v := range byName {
		sort.SliceStable(v.Stations, func(i, j int) bool { return rankOf[v.Stations[i].State] < rankOf[v.Stations[j].State] })
	}
	for _, n := range order {
		ro.venues = append(ro.venues, byName[n])
	}
	return ro
}

// ownerHealthScore: offline share weighs most, then battery, pad and crashes.
func ownerHealthScore(ro ownerRoster, incCur int) (score int, class, word string) {
	score = 100
	if ro.total > 0 {
		score -= 60 * (ro.total - ro.online) / ro.total
		score -= 15 * ro.lowBatt / ro.total
		if incCur > 0 {
			score -= min(15, 3*incCur)
		}
		for _, t := range ro.todos {
			if t.Sev == "warn" && strings.Contains(t.Title, "pad") {
				score -= 5
				break
			}
		}
	}
	if score < 0 {
		score = 0
	}
	class, word = "ok", "Running smoothly"
	switch {
	case score < 50:
		class, word = "bad", "Needs your attention"
	case score < 80:
		class, word = "warn", "Mostly fine, a few things to fix"
	}
	return score, class, word
}

// ownerServiceStrip is today's service: hourly online counts for 24h, with the
// venue's service window shaded and uptime-during-service computed for today so far.
// It also returns the venue's time zone, which the greeting and date line use.
func (h *Handler) ownerServiceStrip(ctx context.Context, ids []uuid.UUID, ro ownerRoster) (cells []ownerHourCell, serviceUptime int, serviceLabel string, loc *time.Location) {
	hourly, _ := h.db.HourlyOnline(ctx, ids, 24)
	var win *db.ServiceWindow
	if ro.restaurantID != nil {
		if sw, ok, err := h.db.GetRestaurantServiceWindow(ctx, *ro.restaurantID); err == nil && ok {
			win = &sw
		}
	}
	loc = time.Local
	if win != nil && win.TZ != "" {
		if l, err := time.LoadLocation(win.TZ); err == nil {
			loc = l
		}
	}
	startHour := time.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	svcHours, svcOnline := 0, 0
	for i := 0; i < 24; i++ {
		t := startHour.Add(time.Duration(i) * time.Hour).In(loc)
		pct := 0
		if ro.total > 0 {
			pct = 100 * hourly[i] / ro.total
		}
		inSvc := false
		if win != nil {
			m := t.Hour()*60 + t.Minute()
			inSvc = (win.OpenMin != win.CloseMin && ((win.OpenMin < win.CloseMin && m >= win.OpenMin && m < win.CloseMin) || (win.OpenMin > win.CloseMin && (m >= win.OpenMin || m < win.CloseMin)))) ||
				(win.NightOpenMin != win.NightCloseMin && ((win.NightOpenMin < win.NightCloseMin && m >= win.NightOpenMin && m < win.NightCloseMin) || (win.NightOpenMin > win.NightCloseMin && (m >= win.NightOpenMin || m < win.NightCloseMin))))
		}
		if inSvc && i < 23 {
			svcHours++
			svcOnline += pct
		}
		cells = append(cells, ownerHourCell{Label: t.Format("3PM"), Pct: pct, Service: inSvc, Now: i == 23})
	}
	serviceUptime = -1
	if svcHours > 0 {
		serviceUptime = svcOnline / svcHours
	}
	if win != nil && win.OpenMin != win.CloseMin {
		serviceLabel = fmtMin(win.OpenMin) + " – " + fmtMin(win.CloseMin)
	}
	return cells, serviceUptime, serviceLabel, loc
}

// ownerWeekReport is the report card from daily stats: uptime and battery, this
// week vs last, then incidents.
func (h *Handler) ownerWeekReport(ctx context.Context, restaurantID *uuid.UUID, incCur, incPrev int) []ownerDelta {
	var report []ownerDelta
	if restaurantID != nil {
		if stats, err := h.db.GetRestaurantDailyStats(ctx, *restaurantID, 14); err == nil && len(stats) > 0 {
			var upCur, upPrev, batCur, batPrev float64
			var nCur, nPrev int
			cut := time.Now().UTC().AddDate(0, 0, -7)
			for _, st := range stats {
				if st.OnlineMinAvg == nil {
					continue
				}
				if st.Day.After(cut) {
					upCur += float64(*st.OnlineMinAvg) / 14.4
					if st.BatteryAvg != nil {
						batCur += float64(*st.BatteryAvg)
					}
					nCur++
				} else {
					upPrev += float64(*st.OnlineMinAvg) / 14.4
					if st.BatteryAvg != nil {
						batPrev += float64(*st.BatteryAvg)
					}
					nPrev++
				}
			}
			if nCur > 0 {
				u := upCur / float64(nCur)
				d := ownerDelta{Label: "Uptime in service", Value: fmt.Sprintf("%.0f%%", u), Has: true}
				if nPrev > 0 {
					pu := upPrev / float64(nPrev)
					d.Prev = fmt.Sprintf("%.0f%%", pu)
					d.Up, d.Better = u >= pu, u >= pu
				}
				report = append(report, d)
				b := batCur / float64(nCur)
				d2 := ownerDelta{Label: "Battery at close", Value: fmt.Sprintf("%.0f%%", b), Has: true}
				if nPrev > 0 {
					pb := batPrev / float64(nPrev)
					d2.Prev = fmt.Sprintf("%.0f%%", pb)
					d2.Up, d2.Better = b >= pb, b >= pb
				}
				report = append(report, d2)
			}
		}
	}
	return append(report, ownerDelta{Label: "Incidents", Value: strconv.Itoa(incCur), Prev: strconv.Itoa(incPrev), Up: incCur > incPrev, Better: incCur <= incPrev, Has: true})
}

// ownerHeadline: "Six of eight stations are ready for dinner." The lede is the
// exceptions in one breath, then "everything else is fine".
func ownerHeadline(venues []*ownerVenue, total, hour int) (headline, lede string) {
	ready := 0
	groups := map[string][]string{}
	for _, v := range venues {
		for _, st := range v.Stations {
			if st.Word == "Ready" {
				ready++
				continue
			}
			groups[st.Word] = append(groups[st.Word], st.Name)
		}
	}
	// One sentence per kind of problem. Up to two names are spelled out, more
	// becomes a count — twenty serials in a paragraph is noise, not a brief.
	names := func(list []string) string {
		switch len(list) {
		case 1:
			return "<b>" + list[0] + "</b>"
		case 2:
			return "<b>" + list[0] + "</b> and <b>" + list[1] + "</b>"
		}
		return "<b>" + list[0] + "</b>, <b>" + list[1] + "</b> and " + strconv.Itoa(len(list)-2) + " more"
	}
	var exceptions []string
	if l := groups["Offline"]; len(l) > 0 {
		if len(l) == 1 {
			exceptions = append(exceptions, names(l)+" is offline")
		} else {
			exceptions = append(exceptions, strconv.Itoa(len(l))+" stations are offline: "+names(l))
		}
	}
	if l := groups["Not charging"]; len(l) > 0 {
		if len(l) == 1 {
			exceptions = append(exceptions, names(l)+" is off its dock and running low")
		} else {
			exceptions = append(exceptions, strconv.Itoa(len(l))+" stations are off their docks and running low: "+names(l))
		}
	}
	if l := groups["Pad misplaced"]; len(l) > 0 {
		exceptions = append(exceptions, names(l)+map[bool]string{true: " need", false: " needs"}[len(l) > 1]+" placing again on the charging pad")
	}
	meal := "service"
	switch {
	case hour < 11:
		meal = "the day"
	case hour < 15:
		meal = "lunch"
	case hour < 22:
		meal = "dinner"
	}
	headline = "No stations are set up yet."
	if total > 0 {
		if ready == total {
			headline = "All " + numWord(total) + " station" + map[bool]string{true: "s are", false: " is"}[total != 1] + " ready for " + meal + "."
		} else if ready == 0 {
			headline = "None of your " + numWord(total) + " station" + map[bool]string{true: "s are", false: " is"}[total != 1] + " ready for " + meal + "."
		} else {
			headline = strings.Title(numWord(ready)) + " of " + numWord(total) + " stations " + map[bool]string{true: "are", false: "is"}[ready != 1] + " ready for " + meal + "."
		}
	}
	if len(exceptions) > 0 {
		lede = strings.Join(exceptions, ". ") + "."
		if ready > 0 {
			lede += " Everything else is charged and locked to the menu."
		}
	} else if total > 0 {
		lede = "Every station is charged, locked to the menu and online."
	}
	return headline, lede
}

// ownerNeedGroups folds the to-do items into one "Needs you" row per kind of
// problem; the stations list below already carries the per-station detail.
func ownerNeedGroups(todos []ownerTodo) []ownerNeedGroup {
	var needGroups []ownerNeedGroup
	idx := map[string]int{}
	order := []string{"offline", "battery", "pad", "crashes", "kiosk"}
	for _, k := range order {
		idx[k] = -1
	}
	for _, t := range todos {
		i, ok := idx[t.Kind]
		if !ok {
			continue
		}
		if i == -1 {
			needGroups = append(needGroups, ownerNeedGroup{Sev: t.Sev, Kind: t.Kind, Body: t.Body})
			i = len(needGroups) - 1
			idx[t.Kind] = i
		}
		needGroups[i].Stations = append(needGroups[i].Stations, t)
	}
	sort.SliceStable(needGroups, func(a, b int) bool {
		pos := func(k string) int {
			for i, o := range order {
				if o == k {
					return i
				}
			}
			return len(order)
		}
		return pos(needGroups[a].Kind) < pos(needGroups[b].Kind)
	})
	for i := range needGroups {
		n := len(needGroups[i].Stations)
		plural := map[bool]string{true: "s", false: ""}[n != 1]
		switch needGroups[i].Kind {
		case "offline":
			needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " offline"
			needGroups[i].Body = "Check that they are powered on and connected to Wi-Fi."
		case "battery":
			needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " low and not charging"
			needGroups[i].Body = "Put them back on their charging docks so they last through service."
		case "pad":
			needGroups[i].Title = strconv.Itoa(n) + " charging pad" + plural + " misplaced"
			needGroups[i].Body = "Place each device again on its pad. If it keeps happening, let the MDM team know."
		case "crashes":
			needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " crashing this week"
			needGroups[i].Body = "The MDM team can see the details; a restart usually helps in the meantime."
		case "kiosk":
			needGroups[i].Title = strconv.Itoa(n) + " station" + plural + " not locked to the app"
			needGroups[i].Body = "Guests can leave the app. Ask the MDM team to turn locked mode back on."
		}
		if n > 6 {
			needGroups[i].More = n - 6
			needGroups[i].Stations = needGroups[i].Stations[:6]
		}
	}
	return needGroups
}

// batteryTempStr renders the device's temperature as "39.2°C" or "": battery_temp_c,
// or cpu_temp_c on a battery-less TV box (see deviceTempC).
func batteryTempStr(raw json.RawMessage) string { return deviceTempStr(raw) }

// numWord spells small counts the way a sentence would ("six of eight").
func numWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

func fmtMin(m int) string {
	t := time.Date(2000, 1, 1, m/60, m%60, 0, 0, time.UTC)
	return strings.ToLower(t.Format("3:04pm"))
}

func timeSinceStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// deliveryNicknames maps the devices of a command's deliveries to their nicknames.
func (h *Handler) deliveryNicknames(ctx context.Context, ds []db.CommandDelivery) map[uuid.UUID]string {
	ids := make([]uuid.UUID, 0, len(ds))
	for _, d := range ds {
		ids = append(ids, d.DeviceID)
	}
	m, err := h.db.GetNicknames(ctx, ids)
	if err != nil || m == nil {
		return map[uuid.UUID]string{}
	}
	return m
}
