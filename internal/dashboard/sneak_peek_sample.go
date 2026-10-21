package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// Sneak peek sample (S1, picked 20 Oct 2026): the preview renders today's Overview
// template (overview_new.html) from one small, made-up fleet: eight restaurants, a
// hundred-odd devices, their alerts and compliance results. Every card is computed
// from that one list with the same builders the real Overview uses (venue rows,
// health scores, attention problems, the compliance widget, the clean-up checklist),
// so the numbers add up and the preview cannot drift from the product. Nothing here
// reads the database.

// spClass is one device class in the sample, with how its devices look.
type spClass struct {
	class, prefix, product, agent, build, model, maker string
}

var spClasses = []spClass{
	{product.ClassT7, "T7", product.KeyT7, product.KindFirmware, "v2.1.034", "T7", "AIO"},
	{product.ClassKiosk, "K22", product.KeyKiosk22, product.KindFirmware, "v2.1.034", "Kiosk 22", "AIO"},
	{product.ClassDongle, "MB", "rbox_d8", product.KindDPC, "RBOX-D8.11.0.240612", "RBOX D8", "HK1"},
	{product.ClassKDS, "KDS", "d2s", product.KindDPC, "D2s_V1.4.2", "D2s", "SUNMI"},
	{product.ClassPOS, "POS", "d3pro", product.KindDPC, "D3PRO_V2.3.1", "D3 Pro", "SUNMI"},
}

// spVenue is one sample restaurant: its devices per class, in spClasses order.
type spVenue struct {
	name, addr string
	mix        [5]int
}

var spVenues = []spVenue{
	{"Bluefin Sushi Bar", "412 Market St, San Francisco", [5]int{6, 2, 2, 1, 1}},
	{"Casa Verde Taqueria", "88 Valencia St, San Francisco", [5]int{4, 1, 1, 1, 1}},
	{"Golden Wok", "1520 Broadway, Oakland", [5]int{12, 2, 3, 2, 1}},
	{"Harbor Grill", "Pier 39, San Francisco", [5]int{16, 3, 3, 2, 2}},
	{"Maple & Rye", "220 Main St, Palo Alto", [5]int{10, 2, 2, 1, 1}},
	{"Saffron Table", "77 Castro St, Mountain View", [5]int{8, 2, 2, 1, 1}},
	{"Sunrise Diner", "905 El Camino Real, San Mateo", [5]int{6, 1, 2, 1, 1}},
	{"Trattoria Lucca", "310 Columbus Ave, San Francisco", [5]int{14, 2, 3, 2, 2}},
}

// spUnplaced is the devices enrolled but not yet at a venue (the onboarding inbox).
var spUnplaced = [5]int{2, 0, 0, 1, 0}

// sneakPeekVenueID is a stable synthetic restaurant UUID for venue i. It can never be
// a real venue's, so the icon lookup always falls back to the initial tile.
func sneakPeekVenueID(i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-a000-%012d", i+1))
}

// sneakPeekDeviceID is the same for device n.
func sneakPeekDeviceID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-b000-%012d", n+1))
}

// spDevice is a sample device and what happens to it in the story.
type spDevice struct {
	db.Device
	venue  int // index into spVenues, -1 = not at a venue
	class  string
	online bool
	temp   float64
	fails  []string // required compliance checks it fails
	unk    int      // checks that could not be measured
}

// sneakPeekSample is the whole made-up fleet and everything derived from it.
type sneakPeekSample struct {
	now     time.Time
	devices []*spDevice
	alerts  []db.Alert
	crashes map[int]int // crash count per venue (24h)
}

func newSneakPeekSample(now time.Time) *sneakPeekSample {
	s := &sneakPeekSample{now: now, crashes: map[int]int{}}
	byKey := map[string]*spDevice{}
	seq := map[string]int{}
	add := func(venue int, c spClass, n int) {
		seq[c.prefix]++
		i := len(s.devices)
		d := &spDevice{venue: venue, class: c.class, online: true, temp: float64(31 + (i*7)%9)}
		bat := 100
		if c.class == product.ClassT7 {
			bat = 48 + (i*17)%50
		}
		extra := map[string]any{"model": c.model, "manufacturer": c.maker, "battery_temp_c": d.temp}
		if c.agent == product.KindDPC {
			extra["agent_type"] = "dpc"
		}
		if c.class == product.ClassT7 {
			extra["charging"] = i%3 != 0
		}
		raw, _ := json.Marshal(extra)
		d.Device = db.Device{
			ID: sneakPeekDeviceID(i), SerialNumber: fmt.Sprintf("%s-%04d", c.prefix, 1000+seq[c.prefix]*37%9000),
			BuildID: c.build, BatteryPct: bat, LastSeenAt: now.Add(-time.Duration(5+i%20) * time.Second),
			KioskEnabled: true, Product: c.product, AgentKind: c.agent, EnrollmentStatus: db.EnrollAuto,
			EnrolledAt: now.AddDate(0, -3, -i%40), CreatedAt: now.AddDate(0, -3, -i%40), LatestExtra: raw,
		}
		if c.agent == product.KindDPC {
			d.DeviceClass, d.EnrollmentStatus = c.class, db.EnrollEnrolled
		}
		if venue >= 0 {
			id := sneakPeekVenueID(venue)
			d.RestaurantID, d.RestaurantName, d.DeployedEffective = &id, spVenues[venue].name, true
			onb := d.EnrolledAt
			d.OnboardedAt = &onb
		} else {
			d.KioskEnabled = false
			d.EnrolledAt = now.Add(-time.Duration(3+i%5) * time.Hour)
		}
		s.devices = append(s.devices, d)
		byKey[fmt.Sprintf("%d/%s/%d", venue, c.class, n)] = d
	}
	for v, ven := range spVenues {
		for ci, c := range spClasses {
			for n := 1; n <= ven.mix[ci]; n++ {
				add(v, c, n)
			}
		}
	}
	for ci, c := range spClasses {
		for n := 1; n <= spUnplaced[ci]; n++ {
			add(-1, c, n)
		}
	}
	dev := func(venue int, class string, n int) *spDevice {
		d := byKey[fmt.Sprintf("%d/%s/%d", venue, class, n)]
		if d == nil {
			panic(fmt.Sprintf("sneak peek sample: no device %d/%s/%d", venue, class, n))
		}
		return d
	}
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	off := func(d *spDevice, mins int) {
		d.online, d.LastSeenAt = false, ago(mins)
	}
	alert := func(d *spDevice, typ, sev string, mins, occ int, detail string) {
		s.alerts = append(s.alerts, db.Alert{
			ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("sp-alert-%d", len(s.alerts)))), Type: typ,
			DeviceID: &d.ID, Serial: d.SerialNumber, RestaurantName: d.RestaurantName, Severity: sev, Status: "open",
			Summary: alertTypeLabel(typ), Detail: json.RawMessage(detail), Occurrences: occ,
			FiredAt: ago(mins), LastSeenAt: ago(mins / 3), UpdatedAt: ago(mins / 3), DeviceClass: d.class,
		})
	}
	const (
		bluefin = iota
		casa
		golden
		harbor
		maple
		saffron
		sunrise
		lucca
	)
	t7, kiosk, mb, kds, pos := product.ClassT7, product.ClassKiosk, product.ClassDongle, product.ClassKDS, product.ClassPOS

	// Bluefin: the router went down 40 minutes ago and took five devices with it; the
	// tablets spend the day off their pads; one T7 still runs a userdebug image.
	for i, d := range []*spDevice{dev(bluefin, t7, 1), dev(bluefin, t7, 2), dev(bluefin, kiosk, 1), dev(bluefin, mb, 1), dev(bluefin, mb, 2)} {
		off(d, 41+i)
		alert(d, "offline", "critical", 39+i, 1, fmt.Sprintf(`{"offline_minutes":%d}`, 41+i))
	}
	dev(bluefin, t7, 6).BuildID = "v2.1.033-userdebug"
	dev(bluefin, t7, 6).fails = append(dev(bluefin, t7, 6).fails, "firmware_approved")

	// Casa Verde: three tablets drifting offline through the day, a KDS nobody has seen
	// in weeks, the kiosk out of storage, the POS short of memory.
	off(dev(casa, t7, 1), 128)
	off(dev(casa, t7, 2), 310)
	off(dev(casa, t7, 3), 26*60)
	off(dev(casa, kds, 1), 19*24*60)
	for i, d := range []*spDevice{dev(casa, t7, 1), dev(casa, t7, 2), dev(casa, t7, 3)} {
		alert(d, "offline", "warning", []int{120, 300, 1550}[i], 1, fmt.Sprintf(`{"offline_minutes":%d}`, []int{128, 310, 1560}[i]))
	}
	dev(casa, t7, 3).fails = append(dev(casa, t7, 3).fails, "checked_in")
	dev(casa, kds, 1).fails = append(dev(casa, kds, 1).fails, "checked_in")
	alert(dev(casa, kiosk, 1), "storage_low", "critical", 95, 3, `{"storage_free_gb":0.6}`)
	alert(dev(casa, pos, 1), "memory_pressure", "warning", 72, 2, `{"ram_pct":93}`)

	// Golden Wok: a tablet by the wok station running hot, two low batteries at dinner,
	// the POS off for a few minutes, two tablets taken out of Lock.
	hot := dev(golden, t7, 4)
	hot.temp = 58
	hot.LatestExtra = json.RawMessage(`{"model":"T7","manufacturer":"AIO","battery_temp_c":58,"charging":true}`)
	alert(hot, "overheating", "critical", 18, 4, `{"temp_c":58,"limit_c":52}`)
	for i, d := range []*spDevice{dev(golden, t7, 7), dev(golden, t7, 9)} {
		d.BatteryPct = []int{14, 17}[i]
		alert(d, "battery_low", "warning", []int{33, 21}[i], 1, fmt.Sprintf(`{"battery_pct":%d}`, d.BatteryPct))
	}
	off(dev(golden, pos, 1), 26)
	alert(dev(golden, pos, 1), "offline", "warning", 24, 1, `{"offline_minutes":26}`)
	for _, d := range []*spDevice{dev(golden, t7, 2), dev(golden, t7, 11)} {
		d.KioskEnabled = false
		d.fails = append(d.fails, "locked")
	}

	// Harbor Grill: healthy, apart from a kiosk far from the access point and one taken
	// out of Lock; the menu boards can't report everything.
	alert(dev(harbor, kiosk, 2), "wifi_weak", "warning", 64, 5, `{"rssi_dbm":-79}`)
	dev(harbor, kiosk, 3).KioskEnabled = false
	dev(harbor, kiosk, 3).fails = append(dev(harbor, kiosk, 3).fails, "locked")
	for n := 1; n <= 3; n++ {
		dev(harbor, mb, n).unk = 1
	}

	// Maple & Rye: the ordering app crashing on one tablet, a dead charging pad.
	alert(dev(maple, t7, 3), "device_crash", "warning", 47, 4, `{"count":4,"kind":"crash","summary":"aio.app.nugget — IllegalStateException in OrderCart.submit"}`)
	alert(dev(maple, t7, 8), "wlc_dead", "warning", 600, 1, `{}`)
	s.crashes[maple] = 4

	// Saffron Table: five tablets still on the previous release, with the rollout under
	// way; wireless adb left on by an installer on five devices.
	for n := 1; n <= 5; n++ {
		d := dev(saffron, t7, n)
		d.BuildID = "v2.1.031"
		d.fails = append(d.fails, "firmware_approved")
	}
	for _, d := range []*spDevice{dev(saffron, kiosk, 1), dev(saffron, kiosk, 2), dev(saffron, mb, 1), dev(saffron, mb, 2), dev(saffron, pos, 1)} {
		d.fails = append(d.fails, "wireless_adb_off")
	}

	// Sunrise Diner: a tablet gone for weeks, another with a dead pad.
	off(dev(sunrise, t7, 6), 25*24*60)
	dev(sunrise, t7, 6).fails = append(dev(sunrise, t7, 6).fails, "checked_in")
	alert(dev(sunrise, t7, 2), "wlc_dead", "warning", 640, 1, `{}`)
	for n := 1; n <= 2; n++ {
		dev(sunrise, mb, n).unk = 1
	}

	// Trattoria Lucca: a crash on one tablet, USB debugging left on two, one unlocked.
	alert(dev(lucca, t7, 12), "device_crash", "warning", 140, 2, `{"count":2,"kind":"anr","summary":"aio.app.nugget — Input dispatching timed out"}`)
	s.crashes[lucca] = 2
	for _, d := range []*spDevice{dev(lucca, kds, 1), dev(lucca, kds, 2)} {
		d.fails = append(d.fails, "adb_off")
	}
	dev(lucca, t7, 5).KioskEnabled = false
	dev(lucca, t7, 5).fails = append(dev(lucca, t7, 5).fails, "locked")

	sort.SliceStable(s.alerts, func(i, j int) bool {
		if a, b := db.SeverityRank(s.alerts[i].Severity), db.SeverityRank(s.alerts[j].Severity); a != b {
			return a > b
		}
		return s.alerts[i].FiredAt.After(s.alerts[j].FiredAt)
	})
	return s
}

// Devices is the sample fleet as the device list pages read it, plus the live set.
func (s *sneakPeekSample) Devices() ([]db.Device, map[uuid.UUID]bool) {
	out := make([]db.Device, 0, len(s.devices))
	online := make(map[uuid.UUID]bool, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d.Device)
		online[d.ID] = d.online
	}
	return out, online
}

// Summary is the fleet totals: devices, online, low battery, builds, locked.
func (s *sneakPeekSample) Summary() db.Summary {
	var sum db.Summary
	builds := map[string]bool{}
	for _, d := range s.devices {
		sum.Total++
		if d.online {
			sum.RecentlyActive++
		}
		if d.class == product.ClassT7 && d.BatteryPct < 20 {
			sum.LowBattery++
		}
		if d.KioskEnabled {
			sum.KioskCount++
		}
		builds[d.BuildID] = true
	}
	sum.UniqueBuilds = len(builds)
	return sum
}

// Hot is how many devices are at or above 45°C.
func (s *sneakPeekSample) Hot() int {
	n := 0
	for _, d := range s.devices {
		if d.temp >= 45 {
			n++
		}
	}
	return n
}

// Groups is each venue's health, scored by the database's own formula.
func (s *sneakPeekSample) Groups() []db.GroupHealth {
	groups := make([]db.GroupHealth, len(spVenues))
	for v, ven := range spVenues {
		groups[v] = db.GroupHealth{GroupID: sneakPeekVenueID(v), Name: ven.name, Deployed: true,
			ChargingAvg: f64Ptr(0.78 + float64(v%4)/50), BatteryDelta: f64Ptr(float64(v%3) - 1)}
	}
	groups[0].ChargingAvg = f64Ptr(0.24) // Bluefin's tablets live off their pads
	groups[1].BatteryDelta = f64Ptr(-14) // Casa Verde's batteries are not filling overnight,
	groups[1].ChargingAvg = f64Ptr(0.28) // and its tablets are rarely docked
	builds := make([]map[string]bool, len(spVenues))
	batSum, batN := make([]int, len(spVenues)), make([]int, len(spVenues))
	for _, d := range s.devices {
		if d.venue < 0 {
			continue
		}
		g := &groups[d.venue]
		g.DeviceCount++
		g.DeployedCount++
		if !d.online {
			g.OfflineCount++
			if s.now.Sub(d.LastSeenAt) > 14*24*time.Hour {
				g.DormantCount++
			}
		}
		if g.TempMax == nil || d.temp > *g.TempMax {
			g.TempMax, g.TempMaxSerial = f64Ptr(d.temp), strPtr(d.SerialNumber)
		}
		// The build spread counts the firmware line only: the stock devices each run
		// their maker's build, which is not something a venue can fix.
		if d.AgentKind == product.KindFirmware {
			if builds[d.venue] == nil {
				builds[d.venue] = map[string]bool{}
			}
			builds[d.venue][d.BuildID] = true
		}
		if d.class == product.ClassT7 {
			batSum[d.venue] += d.BatteryPct
			batN[d.venue]++
		}
	}
	for _, a := range s.alerts {
		for v := range groups {
			if groups[v].Name != a.RestaurantName {
				continue
			}
			switch a.Severity {
			case "critical":
				groups[v].OpenCritical++
			case "warning":
				groups[v].OpenWarning++
			}
		}
	}
	for v := range groups {
		groups[v].DistinctBuilds = len(builds[v])
		if batN[v] > 0 {
			groups[v].BatteryAvg = f64Ptr(math.Round(float64(batSum[v]) / float64(batN[v])))
		}
		groups[v].Rescore()
	}
	return groups
}

// CrashStats is the last 24 hours of crashes, by venue.
func (s *sneakPeekSample) CrashStats() db.FleetCrashStats {
	cs := db.FleetCrashStats{ByRestaurant: map[uuid.UUID]int{}, WorstBuild: "v2.1.034", Daily: []int{3, 5, 2, 4, 1, 3, 6}}
	for v, n := range s.crashes {
		cs.ByRestaurant[sneakPeekVenueID(v)] = n
		cs.Total24h += n
		cs.Devices24h++
	}
	cs.WorstBuildN = cs.Total24h
	return cs
}

// Daily is 14 days of fleet stats, oldest first, ending at today's online count.
func (s *sneakPeekSample) Daily(sum db.Summary) []db.FleetDailyStat {
	dip := []int{9, 7, 8, 6, 9, 7, 6, 5, 6, 4, 5, 3, 4, 0}
	today := s.now.Truncate(24 * time.Hour)
	out := make([]db.FleetDailyStat, 0, len(dip))
	for i, d := range dip {
		active := sum.RecentlyActive - d
		out = append(out, db.FleetDailyStat{Day: today.AddDate(0, 0, i-13), Active: active,
			LowBattery: 1 + i%3, Hot: i % 2, BatteryAvg: f32Ptr(float32(76 + i%4)), Checkins: int64(active) * 1440})
	}
	return out
}

// Rollouts is the one firmware rollout in flight: Saffron Table's stragglers.
func (s *sneakPeekSample) Rollouts() []db.Update {
	return []db.Update{{ID: 9001, Status: "active", RebootBehavior: "manual", CreatedAt: s.now.Add(-50 * time.Minute),
		Product: product.KeyT7, DeviceTotal: 5, DeviceDownloading: 2, DeviceAwaiting: 3,
		Release: &db.Release{ID: 90000, Version: "v2.1.034", Product: product.KeyT7, Status: "published"}}}
}

// ClassCounts is the fleet by device class, as the composition and the
// devices-by-type card read it.
func (s *sneakPeekSample) ClassCounts() ([]db.ClassCount, []db.ClassOnline) {
	tot, on := map[string]int{}, map[string]int{}
	for _, d := range s.devices {
		tot[d.class]++
		if d.online {
			on[d.class]++
		}
	}
	var cc []db.ClassCount
	var co []db.ClassOnline
	for _, c := range product.Classes() { // the order FleetClassOnline returns
		if tot[c] > 0 {
			cc = append(cc, db.ClassCount{Class: c, N: tot[c]})
			co = append(co, db.ClassOnline{Class: c, Total: tot[c], Online: on[c]})
		}
	}
	return cc, co
}

// Venues is the Venues widget, from the same rows the Venues page builds.
func (s *sneakPeekSample) Venues(groups []db.GroupHealth) overviewVenues {
	byID := map[uuid.UUID]db.GroupHealth{} // by ID: the view model re-sorts groups
	for _, g := range groups {
		byID[g.GroupID] = g
	}
	items := make([]collMember, len(spVenues))
	for v, ven := range spVenues {
		id := sneakPeekVenueID(v).String()
		g := byID[sneakPeekVenueID(v)]
		items[v] = collMember{ID: id, Name: ven.name, Sub: ven.addr + " · America/Los_Angeles",
			Href: "/restaurants/" + id, Text: ven.name, Health: &g}
	}
	unplaced := 0
	online := map[uuid.UUID]bool{}
	for _, d := range s.devices {
		online[d.ID] = d.online
		if d.venue < 0 {
			unplaced++
			continue
		}
		items[d.venue].Devices = append(items[d.venue].Devices, d.Device)
	}
	// A zero staleness window: the sample's online flag alone says who is online.
	rows, _ := collRowsFrom(items, online, 0)
	return venueWidget(rows, unplaced)
}

// Compliance is the compliance widget, from per-device results over the real check
// registry (names, levels and auto-fix defaults).
func (s *sneakPeekSample) Compliance() *compWidget {
	views := map[string]*compCheckView{}
	for _, c := range complianceChecks {
		views[c.ID] = &compCheckView{compCheck: c, Level: c.Level}
	}
	devs := make([]compDeviceView, 0, len(s.devices))
	for _, d := range s.devices {
		venue := d.RestaurantName
		v := compDeviceView{ID: d.ID, Serial: d.SerialNumber, Venue: venue, Required: true, Compliant: len(d.fails) == 0, Unknown: d.unk}
		for _, id := range d.fails {
			views[id].Failing = append(views[id].Failing, compDeviceFail{Serial: d.SerialNumber, Venue: venue, Level: db.ComplianceRequired})
		}
		devs = append(devs, v)
	}
	w := buildCompWidget(devs, views, compRules{})
	w.HasDelta, w.Delta = true, 3
	return &w
}

// Hygiene is the clean-up checklist's counts.
func (s *sneakPeekSample) Hygiene(sum db.Summary) db.FleetHygiene {
	h := db.FleetHygiene{Fleet: sum.Total, Builds: sum.UniqueBuilds, OddBuilds: []string{"v2.1.033-userdebug"}}
	seenAt := map[string]bool{}
	for _, d := range s.devices {
		age := s.now.Sub(d.LastSeenAt)
		if age > 14*24*time.Hour {
			h.Silent++
			if d.venue >= 0 {
				h.SilentPlaced++
			}
		}
		if d.venue >= 0 && age > 24*time.Hour {
			h.PlacedSilent++
			if !seenAt[d.RestaurantName] {
				seenAt[d.RestaurantName] = true
				h.PlacedSilentAt = append(h.PlacedSilentAt, d.RestaurantName)
			}
		}
	}
	for _, a := range s.alerts {
		if a.Type == "wlc_dead" {
			h.DeadPad++
		}
	}
	return h
}

// Inbox is how many devices wait for a venue.
func (s *sneakPeekSample) Inbox() int {
	n := 0
	for _, d := range s.devices {
		if d.venue < 0 {
			n++
		}
	}
	return n
}

// AlertsPage is the Alerts inbox's data: problems from the sample alerts, split the
// way the real page splits them, and the crash cards behind the crash alerts.
func (s *sneakPeekSample) AlertsPage() map[string]any {
	hs := make([]humanAlert, len(s.alerts))
	for i, a := range s.alerts {
		hs[i] = humanizeAlert(a)
	}
	var needs, watching []problemView
	sum := db.AlertSummary{Open: len(s.alerts), Resolved: 214}
	for _, a := range s.alerts {
		switch a.Severity {
		case "critical":
			sum.Critical++
		case "warning":
			sum.Warning++
		default:
			sum.Info++
		}
	}
	sum.Total = sum.Open + sum.Resolved
	for _, p := range buildProblems(s.alerts, hs, nil) {
		if p.Severity == "critical" {
			needs = append(needs, p)
		} else {
			watching = append(watching, p)
		}
	}
	var crashes []crashCardView
	for _, a := range s.alerts {
		if a.Type != "device_crash" {
			continue
		}
		var d struct{ Kind, Summary string }
		_ = json.Unmarshal(a.Detail, &d)
		kind, cls := "Crash", "crash"
		if d.Kind == "anr" {
			kind, cls = "ANR", "anr"
		}
		pkg := extractPackageName(d.Summary)
		crashes = append(crashes, crashCardView{
			Serial: a.Serial, Restaurant: a.RestaurantName, KindLabel: kind, KindClass: cls,
			BuildID: "v2.1.034", OccurredAt: a.FiredAt, Summary: d.Summary, PackageName: pkg,
			Trace: "java.lang.IllegalStateException: " + d.Summary + "\n\tat " + pkg + ".OrderCart.submit(OrderCart.kt:212)\n\tat android.os.Handler.dispatchMessage(Handler.java:106)",
		})
	}
	alAnchors(needs)
	alAnchors(watching)
	cs := s.CrashStats()
	return map[string]any{
		"Title": "Alerts", "Summary": sum, "View": "",
		"Needs": needs, "WatchingP": watching,
		"NeedsCount": len(needs), "WatchCount": len(watching), "ActiveCount": len(needs) + len(watching),
		"AlertCount": len(s.alerts),
		"Crashes":    crashes, "CrashCount": len(crashes), "CrashGroups": len(crashes),
		"DeviceFilter": "", "CrashPage": 1, "CrashPages": 1,
		"CrashPageBase": "/sneak-peek/alerts?view=crashes",
		// A1 (21 Oct 2026): the banner, cards and rail from the same sample.
		"AlBanner": buildAlertBanner(needs, watching, s.now),
		"AlCards":  buildAlertCards(needs, watching, nil, &cs, s.now),
		"AlRail":   buildAlertRail(needs, watching, &cs, s.now),
		"AlVenues": alVenues(needs, watching),
	}
}

// OpenAlerts is the bell's count.
func (s *sneakPeekSample) OpenAlerts() int { return len(s.alerts) }

// f64Ptr / f32Ptr / strPtr build the pointer fields the db structs use for metrics that
// may be absent.
func f64Ptr(v float64) *float64 { return &v }
func f32Ptr(v float32) *float32 { return &v }
func strPtr(v string) *string   { return &v }

// OvDevs is the sample fleet as the Overview's C builders read it.
func (s *sneakPeekSample) OvDevs() []ovDev {
	out := make([]ovDev, 0, len(s.devices))
	for _, d := range s.devices {
		t := d.temp
		od := ovDev{ID: d.ID, Class: d.class, Online: d.online, SeenAt: d.LastSeenAt, Battery: d.BatteryPct,
			HasBatt: product.ClassIsBatteryPowered(d.class), TempC: &t, Unplaced: d.venue < 0}
		if d.venue >= 0 {
			id := sneakPeekVenueID(d.venue)
			od.Venue = &id
		}
		out = append(out, od)
	}
	return out
}

// Trend is the delta cards' history: the 14 days of Daily, with a few crashes a day
// ending at today's 24 h count.
func (s *sneakPeekSample) Trend(sum db.Summary) db.ScopedTrend {
	var tr db.ScopedTrend
	cs := s.CrashStats()
	for i, d := range s.Daily(sum) {
		tr.Days = append(tr.Days, db.ScopedDay{Day: d.Day, Active: d.Active, LowBattery: d.LowBattery, Hot: d.Hot,
			BatteryAvg: d.BatteryAvg, Crashes: 2 + (i*5)%7})
	}
	if n := len(tr.Days); n > 0 {
		tr.Days[n-1].Crashes = cs.Total24h
	}
	tr.Crashes24h, tr.CrashesPrev = cs.Total24h, cs.Total24h+3
	return tr
}

// RolloutBars is the last week's rollouts: the firmware stragglers still going, and a
// finished client auto-update and app install before them.
func (s *sneakPeekSample) RolloutBars() []db.Rollout {
	return []db.Rollout{
		{Kind: "app_update", Package: "com.aioapp.mdm", Name: "1.13.10", Who: "auto-update",
			Start: s.now.Add(-122 * time.Hour), End: s.now.Add(-70 * time.Hour), Total: 76, Done: 76},
		{Kind: "install_apk", Name: "Menu Boards 3.4.1", Who: "maria.lopez@aioapp.com",
			Start: s.now.Add(-50 * time.Hour), End: s.now.Add(-46 * time.Hour), Total: 18, Done: 17, Failed: 1},
		{Kind: "ota", Name: "v2.1.034", Product: product.KeyT7, Who: "ops", Status: "active",
			Start: s.now.Add(-50 * time.Minute), End: s.now, Total: 5, InFlight: 5, ReleaseID: 90000, DeploymentID: 9001},
	}
}
