package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
	"mdm/internal/version"

	"github.com/google/uuid"
)

// Landing renders the entry page for staff: what the MDM does, guides, FAQ, the
// training videos (streamed from S3 via short-lived presigned URLs), sign in, sign up.
func (h *Handler) Landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.tmpl.ExecuteTemplate(w, "landing.html", map[string]any{
		"Brand":         h.cfg.CustomBrand(),
		"AssetVer":      h.assetVer,
		"SignupEnabled": true,
		"Training":      h.trainingVideos(r.Context()),
	})
}

// SneakPeek renders a public, no-account preview of the dashboard: the REAL
// Overview template and chrome, rendered against synthetic fleet data instead of
// the database. Reachable from the landing page's "Take a sneak peek" button.
//
// It reuses overviewViewModel — the exact code the live Overview uses — so the
// preview is the real page, not a mock. A synthetic viewer session is injected for
// the duration of this one request so the role-gated chrome renders; no cookie,
// no DB session, and none of the numbers come from real devices. Viewer (not admin)
// so the preview only offers what it can actually show — the write-action controls
// (Send action, Deploy, Actions dock) are role-locked/hidden rather than links that
// bounce a visitor to /login.
func (h *Handler) SneakPeek(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts := sneakPeekFleet()
	activeSecs := h.cfg.CheckinInterval() * 3
	data := h.overviewViewModel(r, summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts, activeSecs, nil, 0)

	// Per-user widget layout (default arrangement for the unknown preview user).
	for k, v := range h.overviewLayoutData(r) {
		data[k] = v
	}
	// Synthetic map: a handful of located devices clustered around a few venues.
	// Empty embed key makes the map widget render its preview branch (a stylised map).
	data["DeviceLocations"] = sneakPeekMapJSON()
	data["DeviceMapCount"] = 10
	data["MapsEmbedKey"] = ""

	h.sneakPeekChrome(data, "overview", openCount)
	h.sneakPeekRender(w, "overview.html", data)
}

// sneakPeekAuth injects a synthetic viewer session for the life of one preview
// request, so the role-gated chrome (dock, name, access) renders without a real
// cookie/DB session. Viewer, so write actions are hidden/role-locked.
func sneakPeekAuth(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, &db.Session{
		Username: "guest", Role: "viewer",
		ExpiresAt: time.Now().Add(time.Hour), LastSeen: time.Now(),
	}))
}

// sneakPeekChrome fills the layout chrome keys a preview page needs when rendered
// directly (not via h.render, so nothing touches the DB or writes an audit row).
func (h *Handler) sneakPeekChrome(data map[string]any, active string, openAlerts int) {
	data["Role"] = "viewer"
	data["Boosted"] = false
	data["CurrentUser"] = "Guest preview"
	if userBubbleFn != nil {
		data["CurrentBubble"] = userBubbleFn("Guest")
	}
	data["Brand"] = h.cfg.CustomBrand()
	data["Use24Hour"] = h.cfg.Use24Hour()
	data["Version"] = version.Current()
	data["AssetVer"] = h.assetVer
	data["ActivePage"] = active
	data["ShowTour"] = false
	data["AlertsOpenCount"] = openAlerts
	data["Preview"] = true
}

// sneakPeekRender writes a preview page from the real template + synthetic data.
func (h *Handler) sneakPeekRender(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("sneak-peek render %s: %v", name, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

// SneakPeekAlerts is the preview's dummy Alerts inbox: the real alerts.html rendered
// against synthetic alerts, so the dock's Alerts item is navigable in the preview.
func (h *Handler) SneakPeekAlerts(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	summary, crit, watch, crashes := sneakPeekAlertData()
	data := map[string]any{
		"Title":         "Alerts",
		"Summary":       summary,
		"View":          "",
		"Needs":         problemsFromHumans(crit),
		"WatchingP":     problemsFromHumans(watch),
		"NeedsCount":    len(problemsFromHumans(crit)),
		"WatchCount":    len(problemsFromHumans(watch)),
		"ActiveCount":   len(crit) + len(watch),
		"Crashes":       crashes,
		"CrashCount":    len(crashes),
		"DeviceFilter":  "",
		"CrashPage":     1,
		"CrashPages":    1,
		"CrashPageBase": "/sneak-peek/alerts?view=crashes",
	}
	h.sneakPeekChrome(data, "alerts", len(crit)+len(watch))
	h.sneakPeekRender(w, "alerts.html", data)
}

// SneakPeekFleet is the preview's dummy Fleet roster: the real devices.html rendered
// against a synthetic device list, so the dock's Fleet item is navigable in the preview.
func (h *Handler) SneakPeekFleet(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	devices, online := sneakPeekDevices()
	summary := db.Summary{Total: 2622, RecentlyActive: 2611, LowBattery: 38, UniqueBuilds: 4, KioskCount: 2202}
	data := map[string]any{
		"Title": "Devices", "Devices": devices, "Online": online,
		"Nicknames": map[uuid.UUID]string{}, "Flapping": map[uuid.UUID]bool{},
		"Total": 2622, "FleetTotal": 2622, "FilterCount": 0, "InactiveCount": 0,
		"RailGroups": []any{}, "RailRestaurants": []any{}, "RailReleases": []any{}, "RailProducts": []any{},
		"Groups": []any{}, "Restaurants": []any{}, "Productions": []any{}, "Builds": []any{}, "Timezones": []any{},
		"Products": h.productFilters(r.Context()), "Classes": product.Classes(),
		"SelectedCollection": "All devices", "SelectedCount": 2622,
		"ActiveRestaurant": nil, "ActiveGroup": nil, "ActiveReleaseID": 0,
		"View": "", "ViewID": "", "ViewColl": "",
		"Page": 1, "TotalPages": 1, "Query": "", "PageSize": 60,
		"Summary": summary, "Sort": "", "SortDir": "",
		"FilterRestaurant": "", "FilterGroup": "", "FilterProduction": "", "FilterProduct": "",
		"FilterStatus": "", "FilterBuild": "", "FilterBattery": "", "FilterRAM": "", "FilterTemp": "", "FilterKiosk": "",
		"FilterCharging": "", "FilterTimezone": "", "FilterKind": "", "FilterClass": "",
		"FilterOnboarding": "", "FilterLifecycle": "", "FilterHidden": false,
		"ActiveThresholdSecs": 180, "ActiveThresholdLabel": "3 min",
		"Density": h.cfg.Density(), "MapsEmbedKey": "",
	}
	h.sneakPeekChrome(data, "devices", 11)
	h.sneakPeekRender(w, "devices.html", data)
}

// sneakPeekDevices builds ~30 synthetic devices spread across the preview's venues,
// plus the online map devices.html reads for the live status dot.
func sneakPeekDevices() ([]db.Device, map[uuid.UUID]bool) {
	type spec struct {
		rest, prod, build string
		bat, temp         int
		kiosk, online     bool
	}
	names := []string{"Harbor Grill", "Harbor Grill", "Harbor Kitchen", "Sausalito", "Sausalito",
		"Marina Point", "Marina Point", "Ferry Plaza", "Embarcadero", "North Beach",
		"Presidio", "Richmond", "SoMa", "Castro", "Dogpatch", "Glen Park",
		"Mission Rock", "Noe Valley", "Hayes Valley", "Bayview", "Potrero", "Chinatown",
		"Union Square", "Financial", "Bernal", "Cole Valley", "Nob Hill", "Russian Hill",
		"Sunset", "Harbor Café"}
	prods := []string{"kiosk22", "t7"}
	devices := make([]db.Device, 0, len(names))
	online := map[uuid.UUID]bool{}
	now := time.Now()
	for i, name := range names {
		id := uuid.New()
		prod := prods[i%len(prods)]
		build := "2026.06.01-release"
		if i%7 == 3 {
			build = "2026.05.14-release"
		}
		bat := 60 + (i*11)%40 // 60..99
		temp := 30 + (i*3)%14 // 30..43
		isOn := i%9 != 4      // most online
		if i == 0 {
			temp = 61 // the hot one
		}
		seen := now.Add(-time.Duration(2+i%9) * time.Minute)
		if !isOn {
			seen = now.Add(-time.Duration(3+i%6) * time.Hour)
		}
		hasBat := prod == "t7"
		extra := fmt.Sprintf(`{"model":"AIO-%s","manufacturer":"AIO","temp_c":%d,"charging":%t}`,
			strings.ToUpper(prod), temp, hasBat && bat < 90)
		devices = append(devices, db.Device{
			ID: id, SerialNumber: fmt.Sprintf("%s-%04d", map[bool]string{true: "TAB", false: "KSK"}[prod == "t7"], 1000+i),
			BuildID: build, BatteryPct: bat, LastSeenAt: seen, KioskEnabled: true,
			Product: prod, RestaurantName: name, DeployedEffective: true,
			AgentKind: "firmware", EnrollmentStatus: "auto", EnrolledAt: now.Add(-720 * time.Hour),
			LatestExtra: json.RawMessage(extra),
		})
		online[id] = isOn
	}
	return devices, online
}

// sneakPeekAlertData builds the synthetic alerts inbox for the preview, consistent
// with the fleet sneakPeekFleet paints (Harbor Grill hot, Sausalito offline, …).
func sneakPeekAlertData() (db.AlertSummary, []humanAlert, []humanAlert, []crashCardView) {
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }
	ha := func(icon, sev, headline, sentence, serial, rest string, mins, occ int) humanAlert {
		return humanAlert{
			ID: uuid.New(), Severity: sev, Status: "open", IconKey: icon,
			Headline: headline, Sentence: template.HTML(sentence), Serial: serial,
			Restaurant: rest, FiredAt: ago(mins), Occurrences: occ,
		}
	}
	crit := []humanAlert{
		ha("heat", "critical", "Device running hot", "Reached <b>61°C</b>, above the 60°C ceiling — throttling likely.", "KSK-1000", "Harbor Grill", 12, 4),
		ha("crash", "critical", "App crashing repeatedly", "<b>aio.app.pos</b> crashed 6 times in the last hour.", "KSK-1042", "Harbor Kitchen", 35, 6),
		ha("storage", "critical", "Storage almost full", "Only <b>2%</b> free on the data partition.", "KSK-1103", "Harbor Café", 64, 1),
	}
	watch := []humanAlert{
		ha("offline", "warning", "Device offline", "Stopped checking in <b>4h</b> ago — last seen 04:12.", "KSK-2117", "Sausalito", 240, 1),
		ha("battery", "warning", "Battery below 20%", "Discharged to <b>17%</b> and not on a charging pad.", "TAB-3088", "Marina Point", 26, 1),
		ha("charge", "warning", "Left off the charging pad", "Off power overnight; started the day at 41%.", "TAB-3091", "Ferry Plaza", 190, 1),
		ha("wifi", "warning", "Weak Wi-Fi signal", "Averaging <b>-78 dBm</b>; check the access point placement.", "KSK-2210", "Embarcadero", 88, 3),
		ha("memory", "warning", "High memory pressure", "Available memory under <b>8%</b> for 20 minutes.", "KSK-2255", "North Beach", 52, 2),
		ha("offline", "warning", "Device offline", "Stopped checking in <b>1h</b> ago.", "KSK-2260", "Presidio", 61, 1),
		ha("battery", "warning", "Battery draining fast", "Down 30% in an hour under load.", "TAB-3120", "Richmond", 44, 1),
		ha("generic", "warning", "Clock drift detected", "Device clock is <b>90s</b> behind the server.", "KSK-2301", "SoMa", 120, 1),
	}
	crash := func(kind, cls, serial, rest, pkg, summary string, mins int) crashCardView {
		return crashCardView{
			Serial: serial, Restaurant: rest, KindLabel: kind, KindClass: cls,
			BuildID: "2026.05.14-release", OccurredAt: ago(mins), Summary: summary, PackageName: pkg,
			Trace: "java.lang.RuntimeException: " + summary + "\n\tat " + pkg + ".MainActivity.onCreate(MainActivity.java:142)\n\tat android.app.Activity.performCreate(Activity.java:8000)",
		}
	}
	crashes := []crashCardView{
		crash("Crash", "crash", "KSK-1042", "Harbor Kitchen", "aio.app.pos", "aio.app.pos — NullPointerException in checkout flow", 35),
		crash("ANR", "anr", "KSK-1000", "Harbor Grill", "aio.app.kiosk", "aio.app.kiosk — Input dispatching timed out (main thread blocked)", 70),
		crash("Crash", "crash", "TAB-3088", "Marina Point", "aio.app.pay", "aio.app.pay — IllegalStateException reading card reader", 150),
	}
	summary := db.AlertSummary{Total: 361, Open: 11, Acknowledged: 2, Resolved: 348, Critical: 3, Warning: 8, Info: 0}
	return summary, crit, watch, crashes
}

// intPtr / f64Ptr / f32Ptr / strPtr are small helpers for the synthetic-data
// builder below (the db structs use pointer fields for "may be absent" metrics).
func intPtr(v int) *int         { return &v }
func f64Ptr(v float64) *float64 { return &v }
func f32Ptr(v float32) *float32 { return &v }
func strPtr(v string) *string   { return &v }

// sneakPeekVenueID is a stable synthetic restaurant UUID for venue i, so the crash
// map and the health list line up across the preview.
func sneakPeekVenueID(i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-a000-%012d", i+1))
}

// sneakPeekFleet builds the synthetic fleet the preview renders: a chain-scale fleet
// of 2500+ devices across 72 venues. Made up but internally consistent — every KPI,
// the health score, the sites heatmap and the rollouts are derived from the same
// per-venue numbers, so nothing contradicts anything else.
func sneakPeekFleet() (db.Summary, []db.GroupHealth, int, []db.FleetDailyStat, int, db.FleetCrashStats, []db.FleetVersion, []db.Update, map[string]int) {
	districts := []string{
		"Harbor", "Marina", "Ferry Plaza", "Embarcadero", "North Beach", "Mission Rock",
		"Presidio", "Sunset", "Richmond", "Castro", "Hayes Valley", "Nob Hill",
		"Russian Hill", "Potrero", "Dogpatch", "SoMa", "Union Square", "Financial",
		"Chinatown", "Glen Park", "Noe Valley", "Bernal", "Cole Valley", "Bayview",
	}
	suffixes := []string{"Grill", "Kitchen", "Café"}
	const venueCount = 72

	groups := make([]db.GroupHealth, 0, venueCount)
	total, online, openCount := 0, 0, 0
	crashBy := map[uuid.UUID]int{}
	for i := 0; i < venueCount; i++ {
		id := sneakPeekVenueID(i)
		dc := 22 + (i*13)%30 // 22..51 devices per venue
		g := db.GroupHealth{
			GroupID: id, Name: districts[i/3] + " " + suffixes[i%3],
			DeviceCount: dc, DeployedCount: dc, Deployed: true, DistinctBuilds: 1,
			BatteryAvg: f64Ptr(float64(74 + i%20)), ChargingAvg: f64Ptr(0.72 + float64(i%18)/100),
		}
		switch {
		case i < 3: // three venues at risk (worst first drives the hero + "why")
			g.ScoreClass, g.Score = "danger", []int{57, 62, 66}[i]
			g.OfflineCount, g.OpenCritical = []int{3, 2, 2}[i], 1
			openCount++
			crashBy[id] = []int{4, 2, 1}[i]
			if i < 2 { // the two hottest venues
				g.TempMax = f64Ptr([]float64{61, 51}[i])
				g.TempMaxSerial = strPtr(fmt.Sprintf("KSK-%04d", 1000+i))
			}
		case i < 11: // eight venues to watch
			g.ScoreClass, g.Score = "warn", 71+(i-3)
			g.OpenWarning = 1
			openCount++
			if i%2 == 0 {
				g.OfflineCount = 1
			}
			if i%3 == 0 {
				g.DistinctBuilds = 2
			}
		default: // healthy
			g.ScoreClass = "ok"
			if g.Score = 84 + (i*7)%15; g.Score > 99 {
				g.Score = 99
			}
		}
		total += dc
		online += dc - g.OfflineCount
		groups = append(groups, g)
	}

	hot := 6
	summary := db.Summary{
		Total: total, RecentlyActive: online, LowBattery: 38, UniqueBuilds: 4,
		KioskCount: total * 84 / 100,
	}

	// 14 days of fleet daily stats (oldest first) for the sparklines + week-over-week
	// trend. Deterministic gentle wave around the current online count.
	dip := []int{40, 18, 26, 10, 22, 34, 30, 14, 8, 20, 12, 24, 16, 6}
	batWave := []float32{77, 78, 78, 79, 78, 77, 78, 79, 80, 78, 79, 79, 78, 78}
	today := time.Now().Truncate(24 * time.Hour)
	d14 := make([]db.FleetDailyStat, 0, 14)
	for i := 0; i < 14; i++ {
		active := online - dip[i]
		d14 = append(d14, db.FleetDailyStat{
			Day: today.AddDate(0, 0, i-13), Active: active, LowBattery: 30 + i%12, Hot: 4 + i%4,
			BatteryAvg: f32Ptr(batWave[i]), Checkins: int64(active) * 288,
		})
	}

	crashStats := db.FleetCrashStats{
		Total24h: 34, Devices24h: 29, WorstBuild: "2026.05.14-release", WorstBuildN: 11,
		Daily:        []int{88, 72, 64, 79, 58, 66, 54},
		ByRestaurant: crashBy,
	}

	// Release adoption + rollouts, distributed across the fleet total.
	vLatest := total * 63 / 100
	v2 := total * 22 / 100
	v3 := total * 10 / 100
	v4 := total - vLatest - v2 - v3
	versions := []db.FleetVersion{
		{Version: "2026.06.01-release", Product: "kiosk22", DeviceCount: vLatest, ReleaseID: intPtr(90000), ReleaseStatus: "published"},
		{Version: "2026.05.14-release", Product: "kiosk22", DeviceCount: v2, ReleaseID: intPtr(89000), ReleaseStatus: "published"},
		{Version: "2026.04.02-release", Product: "t7", DeviceCount: v3, ReleaseID: intPtr(88000), ReleaseStatus: "published"},
		{Version: "2026.03.10-release", Product: "kiosk22", DeviceCount: v4},
	}

	now := time.Now()
	deployments := []db.Update{
		{ID: 9001, Status: "active", RebootBehavior: "immediate", CreatedAt: now.Add(-38 * time.Minute),
			Product: "kiosk22", DeviceTotal: total, DeviceInstalled: total * 64 / 100,
			DeviceDownloading: total * 9 / 100, DeviceFailed: total * 1 / 100,
			Release: &db.Release{ID: 90000, Version: "2026.06.01-release", Product: "kiosk22", Status: "published"}},
		{ID: 9002, Status: "active", RebootBehavior: "manual", CreatedAt: now.Add(-3 * time.Hour),
			Product: "kiosk22", DeviceTotal: 48, DeviceInstalled: 48, DeviceAwaiting: 6,
			Release: &db.Release{ID: 89000, Version: "2026.05.14-release", Product: "kiosk22", Status: "published"}},
	}

	t7 := total * 17 / 100
	prodCounts := map[string]int{"kiosk22": total - t7, "t7": t7}

	return summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts
}

// sneakPeekMapJSON is a synthetic set of located devices around a few Bay Area
// venues, in the same shape deviceLocationsJSON produces for the overview map.
func sneakPeekMapJSON() template.JS {
	pts := []deviceMapPoint{
		{Serial: "DEMO-0041", Name: "Harbor Grill", Lat: 37.8087, Lon: -122.4098, Online: true, Product: "kiosk22", Battery: 84, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Harbor Grill"},
		{Serial: "DEMO-0042", Name: "Harbor Grill", Lat: 37.8090, Lon: -122.4102, Online: true, Product: "kiosk22", Battery: 79, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Harbor Grill"},
		{Serial: "DEMO-0117", Name: "Sausalito", Lat: 37.8591, Lon: -122.4853, Online: false, Product: "kiosk22", Battery: 61, Kiosk: true, Build: "2026.05.14-release", Restaurant: "Sausalito"},
		{Serial: "DEMO-0119", Name: "Sausalito", Lat: 37.8570, Lon: -122.4840, Online: true, Product: "kiosk22", Battery: 88, Kiosk: true, Build: "2026.05.14-release", Restaurant: "Sausalito"},
		{Serial: "DEMO-0071", Name: "Marina Point", Lat: 37.8060, Lon: -122.4330, Online: true, Product: "t7", Battery: 92, Build: "2026.06.01-release", Restaurant: "Marina Point"},
		{Serial: "DEMO-0093", Name: "Ferry Plaza", Lat: 37.7955, Lon: -122.3937, Online: true, Product: "t7", Battery: 67, Build: "2026.06.01-release", Restaurant: "Ferry Plaza"},
		{Serial: "DEMO-0060", Name: "Embarcadero", Lat: 37.7929, Lon: -122.3968, Online: true, Product: "kiosk22", Battery: 90, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Embarcadero"},
		{Serial: "DEMO-0055", Name: "North Beach", Lat: 37.8003, Lon: -122.4104, Online: true, Product: "kiosk22", Battery: 85, Kiosk: true, Build: "2026.06.01-release", Restaurant: "North Beach"},
		{Serial: "DEMO-0080", Name: "Presidio", Lat: 37.7989, Lon: -122.4662, Online: true, Product: "kiosk22", Battery: 87, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Presidio"},
		{Serial: "DEMO-0102", Name: "Mission Rock", Lat: 37.7716, Lon: -122.3893, Online: true, Product: "kiosk22", Battery: 93, Kiosk: true, Build: "2026.06.01-release", Restaurant: "Mission Rock"},
	}
	b, err := json.Marshal(pts)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

// trainingChapter is one entry of training/manifest.json in the S3 bucket (written by
// tools/reel/publish-training.sh) plus presigned URLs for the page.
type trainingChapter struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Intro    string `json:"intro"`
	Steps    int    `json:"steps"`
	Duration int    `json:"duration"` // seconds
	Video    string `json:"-"`
	Poster   string `json:"-"`
}

const trainingPresignTTL = 6 * time.Hour

var (
	trainingMu     sync.Mutex
	trainingCache  []trainingChapter
	trainingCached time.Time
)

// trainingVideos returns the chapters with presigned URLs, cached for 5 minutes so the
// landing page does not hit S3 on every visit. Empty when S3 is not configured or the
// manifest is missing; the template hides the section then.
func (h *Handler) trainingVideos(ctx context.Context) []trainingChapter {
	if h.apk == nil {
		return nil
	}
	trainingMu.Lock()
	defer trainingMu.Unlock()
	if time.Since(trainingCached) < 5*time.Minute {
		return trainingCache
	}
	trainingCached = time.Now() // also caches a miss, so a missing manifest is cheap
	body, _, _, _, err := h.apk.Get(ctx, "training/manifest.json", "")
	if err != nil {
		trainingCache = nil
		return nil
	}
	defer body.Close()
	var m struct {
		Chapters []trainingChapter `json:"chapters"`
	}
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		log.Printf("[landing] training manifest: %v", err)
		trainingCache = nil
		return nil
	}
	out := m.Chapters[:0]
	for _, c := range m.Chapters {
		v, err1 := h.apk.PresignGet(ctx, "training/"+c.ID+".mp4", trainingPresignTTL)
		p, err2 := h.apk.PresignGet(ctx, "training/"+c.ID+".jpg", trainingPresignTTL)
		if err1 != nil {
			continue
		}
		c.Video = v
		if err2 == nil {
			c.Poster = p
		}
		out = append(out, c)
	}
	trainingCache = out
	return out
}
