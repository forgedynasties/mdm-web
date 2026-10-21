package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// Overview renders the landing page: fleet pulse score, vital cards with 7-day
// sparklines, the cached AI fleet report, quick actions, and recent activity.
// The devices table itself lives on /devices.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	if roleIsCustomer(h.role(r)) {
		h.OwnerHome(w, r)
		return
	}
	// The overview is fleet-wide totals; a user who only sees part of the fleet
	// lands on their device list instead.
	if h.access(r).hidesDevices() {
		http.Redirect(w, r, "/devices", http.StatusFound)
		return
	}
	// The new Overview (device wall + map) is the default for everyone who lands
	// here; ?overview=new|classic flips it and is remembered in a cookie.
	if v := r.URL.Query().Get("overview"); v == "new" || v == "classic" {
		http.SetCookie(w, &http.Cookie{Name: overviewCookie, Value: v, Path: "/", MaxAge: 365 * 24 * 3600,
			HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	newOverview := overviewChoice(r) == "new"
	ctx := r.Context()
	activeSecs := h.cfg.CheckinInterval() * 3
	var summary db.Summary
	var err error
	if h.access(r).hidesDPC() {
		// DPC devices used to be hidden below super op; hidesDPC is false now, so this counts all.
		summary, err = h.db.GetSummaryFiltered(ctx, db.DeviceFilter{AgentKind: "firmware", Connected: h.connectedSlice(), ActiveThresholdSecs: activeSecs})
	} else {
		summary, err = h.db.GetSummary(ctx, h.connectedSlice())
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The ~8 reads below are all independent (none consumes another's result), so
	// run them concurrently instead of one round trip after another — this is the
	// page every user lands on after login. d14 covers both the 7-day sparklines/
	// activity chart AND the 14-day week-over-week trend, so it's fetched once and
	// sliced, instead of the old code fetching 7 and then 14 days separately.
	var (
		groups      []db.GroupHealth
		hot         int
		d14         []db.FleetDailyStat
		openCount   int
		crashStats  db.FleetCrashStats
		versions    []db.FleetVersion
		deployments []db.Update
		prodCounts  map[string]int
	)
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { groups, _ = h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7) })
	run(func() { hot, _ = h.db.CountHotDevices(ctx) })
	run(func() { d14, _ = h.db.GetFleetDailyStats(ctx, 14) })
	run(func() { openCount, _ = h.db.CountOpenAlerts(ctx, h.access(r).hidesIdentityAlerts()) })
	run(func() { crashStats, _ = h.db.GetFleetCrashStats(ctx, 4) })
	run(func() { versions, _ = h.db.GetFleetVersions(ctx) })
	run(func() { deployments, _ = h.db.ListDeployments(ctx) })
	run(func() { prodCounts, _ = h.db.CountDevicesByProduct(ctx) })
	wg.Wait()

	inbox, _ := h.db.ListOnboardingInbox(r.Context(), 5)
	inboxN := 0
	if fc, err := h.db.FleetCounts(r.Context(), h.access(r).hidesDPC()); err == nil {
		inboxN = fc.Inbox
	}
	data := h.overviewViewModel(r, summary, groups, hot, d14, openCount, crashStats, versions, deployments, prodCounts, activeSecs, inbox, inboxN)
	data["OverviewSwitch"] = true
	if newOverview {
		h.overviewNewData(r, data, summary, groups, inboxN, crashStats)
		h.render(w, r, "overview_new.html", data)
		return
	}

	// Power & usage widget: the same figures the restaurant page shows per site, summed
	// across every placed device (uuid.Nil = the whole deployed fleet).
	if m, err := h.db.SiteMetricsFor(ctx, uuid.Nil, 7, time.Time{}); err == nil {
		data["PowerMetrics"] = m
	}

	// Fleet map: every device with a resolved location from the geolocation pipeline.
	locs, locCount := h.deviceLocationsJSON(ctx, h.access(r).hidesDPC())
	data["DeviceLocations"] = locs
	data["DeviceMapCount"] = locCount
	data["MapsEmbedKey"] = h.mapsEmbedKey

	if roles, ok := h.overviewProducts(ctx, h.access(r).hidesDPC()); ok {
		data["Products"] = roles
	}

	// Per-user widget arrangement (hidden / order / preset).
	for k, v := range h.overviewLayoutData(r) {
		data[k] = v
	}

	h.render(w, r, "overview.html", data)
}

// overviewViewModel builds the Overview page's data map from already-fetched
// fleet data. Split out of Overview so the public /sneak-peek preview can render
// the exact same template against synthetic data (no DB reads, no real fleet).
func (h *Handler) overviewViewModel(r *http.Request, summary db.Summary, groups []db.GroupHealth, hot int, d14 []db.FleetDailyStat, openCount int, crashStats db.FleetCrashStats, versions []db.FleetVersion, deployments []db.Update, prodCounts map[string]int, activeSecs int, inbox []db.Device, inboxN int) map[string]any {
	daily := d14
	if n := len(d14); n > 7 {
		daily = d14[n-7:]
	}

	// Fleet score: device-weighted mean of the per-group health scores. Without
	// groups, fall back to an online-ratio penalty so the ring still means something.
	score := 100
	if summary.Total > 0 {
		num, den := 0, 0
		for _, g := range groups {
			num += g.Score * g.DeviceCount
			den += g.DeviceCount
		}
		if den > 0 {
			score = num / den
		} else {
			// No venue has devices yet (or no venues at all): fall back to the
			// online-ratio proxy so the ring still reflects reality.
			score -= 40 * (summary.Total - summary.RecentlyActive) / summary.Total
		}
	}
	scoreClass := "danger"
	switch {
	case score >= 80:
		scoreClass = "ok"
	case score >= 50:
		scoreClass = "warn"
	}
	// "Why this score": the venue penalties behind the ring, biggest first.
	healthReasons := explainFleetHealth(groups, crashStats.ByRestaurant)
	healthLost := 0.0
	for _, r := range healthReasons {
		healthLost += r.Points
	}

	offline := summary.Total - summary.RecentlyActive
	attention := offline + summary.LowBattery + hot
	verdict := "Fleet is healthy — all devices reporting"
	if attention == 1 {
		verdict = "Fleet is healthy — 1 device needs a look"
	} else if attention > 1 {
		verdict = fmt.Sprintf("%d devices need a look", attention)
	}

	var actS, offS, lowS, hotS []float64
	for _, ds := range daily {
		actS = append(actS, float64(ds.Active))
		offS = append(offS, math.Max(0, float64(summary.Total-ds.Active)))
		lowS = append(lowS, float64(ds.LowBattery))
		hotS = append(hotS, float64(ds.Hot))
	}

	// Activity bars for the overview chart: one bar per day, height relative to
	// the busiest day. The most recent day is flagged so the chart highlights it.
	var peak int
	for _, ds := range daily {
		if ds.Active > peak {
			peak = ds.Active
		}
	}
	activityBars := make([]map[string]any, 0, len(daily))
	for i, ds := range daily {
		pct := 4
		if peak > 0 {
			if pct = ds.Active * 100 / peak; pct < 4 {
				pct = 4
			}
		}
		activityBars = append(activityBars, map[string]any{
			"Label": ds.Day.Format("Mon")[:2],
			"Val":   ds.Active,
			"Pct":   pct,
			"Cur":   i == len(daily)-1,
		})
	}

	// Same series as a line/area chart for the overview Fleet-activity card.
	actPts := make([]map[string]any, 0, len(daily))
	for _, ds := range daily {
		actPts = append(actPts, map[string]any{"label": ds.Day.Format("Mon"), "val": ds.Active, "low": ds.LowBattery})
	}
	activityJSON := template.JS("[]")
	if b, err := json.Marshal(actPts); err == nil {
		activityJSON = template.JS(b)
	}

	hour := time.Now().Hour()
	greeting := "Good evening"
	if hour < 12 {
		greeting = "Good morning"
	} else if hour < 17 {
		greeting = "Good afternoon"
	}

	// ── Command-center additions ──
	sIf := func(n int) string {
		if n == 1 {
			return ""
		}
		return "s"
	}
	// Crash surfaces (24h rollup): signal tile + hero sentence.
	// (openCount, crashStats already fetched concurrently above.)
	var crS []float64
	for _, v := range crashStats.Daily {
		crS = append(crS, float64(v))
	}

	// Restaurant-level verdict + worst-first list for the "Where to look" bridge.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Score < groups[j].Score })
	attentionRest := 0
	worst := make([]db.GroupHealth, 0, 3)
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass != "ok" {
			attentionRest++
			if len(worst) < 3 {
				worst = append(worst, g)
			}
		}
	}
	restVerdict := "All restaurants healthy"
	switch {
	case attentionRest == 1:
		restVerdict = "1 restaurant needs a look today"
	case attentionRest > 1:
		restVerdict = fmt.Sprintf("%d restaurants need a look today", attentionRest)
	}
	statusWord := map[string]string{"ok": "Healthy", "warn": "Watch", "danger": "At risk"}[scoreClass]

	// Hero sentence: real live counts + the single worst location's reason.
	crashWord := "crashes"
	if crashStats.Total24h == 1 {
		crashWord = "crash"
	}
	heroSentence := fmt.Sprintf("%d of %d devices online · %d %s in the last 24h · %d open alert%s.",
		summary.RecentlyActive, summary.Total, crashStats.Total24h, crashWord,
		openCount, sIf(openCount))
	if len(worst) > 0 {
		w := worst[0]
		var bits []string
		if w.OfflineCount > 0 {
			bits = append(bits, fmt.Sprintf("%d offline", w.OfflineCount))
		}
		if w.TempMax != nil && *w.TempMax >= 45 {
			bits = append(bits, "a unit running hot")
		}
		if c := crashStats.ByRestaurant[w.GroupID]; c > 0 {
			bits = append(bits, fmt.Sprintf("%d crashes", c))
		}
		if len(bits) > 0 {
			heroSentence += fmt.Sprintf(" %s is the worst — %s.", w.Name, strings.Join(bits, ", "))
		}
	}

	// Week-over-week fleet-health trend: a connectivity proxy (100 − 40·offline-ratio,
	// the same fallback the ring uses) averaged this week vs the prior week.
	scoreDelta := 0
	if summary.Total > 0 {
		proxy := func(active int) float64 {
			off := float64(summary.Total - active)
			if off < 0 {
				off = 0
			}
			return 100 - 40*off/float64(summary.Total)
		}
		var recent, prior float64
		var rc, pc int
		n := len(d14)
		for i, ds := range d14 {
			if i >= n-7 {
				recent += proxy(ds.Active)
				rc++
			} else {
				prior += proxy(ds.Active)
				pc++
			}
		}
		if rc > 0 && pc > 0 {
			scoreDelta = int(math.Round(recent/float64(rc) - prior/float64(pc)))
		}
	}

	// Today-vs-yesterday deltas for the signal tiles.
	onToday, onYest, offToday, offYest, lowToday, lowYest := 0, 0, 0, 0, 0, 0
	if len(daily) >= 2 {
		a, b := daily[len(daily)-1], daily[len(daily)-2]
		onToday, onYest = a.Active, b.Active
		offToday, offYest = summary.Total-a.Active, summary.Total-b.Active
		lowToday, lowYest = a.LowBattery, b.LowBattery
	}
	crToday, crYest := 0, 0
	if n := len(crashStats.Daily); n >= 2 {
		crToday, crYest = crashStats.Daily[n-1], crashStats.Daily[n-2]
	}

	// ── Sites heatmap: every venue as a score-coloured tile, worst first ──
	type siteTile struct {
		ID         uuid.UUID
		Name       string
		Score      int
		Class      string
		Devices    int
		Offline    int
		Critical   int
		Warning    int
		Crashes    int
		Hot        bool
		BatteryAvg int
		HasBattery bool
		Why        []string // penalties behind the score, for the "?" popover
	}
	var sites []siteTile
	sitesOK, sitesWarn, sitesBad, deployedN := 0, 0, 0, 0
	for _, g := range groups {
		if g.DeviceCount == 0 {
			continue
		}
		deployedN += g.DeviceCount
		t := siteTile{
			ID: g.GroupID, Name: g.Name, Score: g.Score, Class: g.ScoreClass,
			Devices: g.DeviceCount, Offline: g.OfflineCount,
			Critical: g.OpenCritical, Warning: g.OpenWarning,
			Crashes: crashStats.ByRestaurant[g.GroupID],
			Hot:     g.TempMax != nil && *g.TempMax >= 45,
			Why:     whyScore(g),
		}
		if g.BatteryAvg != nil {
			t.BatteryAvg, t.HasBattery = int(math.Round(*g.BatteryAvg)), true
		}
		switch g.ScoreClass {
		case "ok":
			sitesOK++
		case "warn":
			sitesWarn++
		default:
			sitesBad++
		}
		sites = append(sites, t)
	}

	// ── Release adoption: top builds by device count + an "other" bucket ──
	type versionRow struct {
		Version string
		Product string
		Count   int
		Pct     int
		Status  string // published | draft | unmanaged
		Hidden  bool
	}
	versionsTotal := 0
	for _, v := range versions {
		versionsTotal += v.DeviceCount
	}
	var versionRows []versionRow
	versionOther, versionOtherN := 0, 0
	for i, v := range versions {
		if i >= 5 {
			versionOther += v.DeviceCount
			versionOtherN++
			continue
		}
		st := "unmanaged"
		if v.ReleaseID != nil {
			st = v.ReleaseStatus
			if st == "" {
				st = "draft"
			}
		}
		pct := 0
		if versionsTotal > 0 {
			pct = v.DeviceCount * 100 / versionsTotal
		}
		versionRows = append(versionRows, versionRow{
			Version: v.Version, Product: product.Label(v.Product), Count: v.DeviceCount,
			Pct: pct, Status: st, Hidden: v.ReleaseHidden,
		})
	}
	versionOtherPct := 0
	if versionsTotal > 0 {
		versionOtherPct = versionOther * 100 / versionsTotal
	}
	// Share of the fleet on a published (managed) build.
	onPublished := 0
	for _, v := range versions {
		if v.ReleaseID != nil && v.ReleaseStatus == "published" {
			onPublished += v.DeviceCount
		}
	}
	onPublishedPct := 0
	if versionsTotal > 0 {
		onPublishedPct = onPublished * 100 / versionsTotal
	}

	// ── Rollouts in flight: pending/active OTA deployments, newest first ──
	type rolloutRow struct {
		ID          int
		Version     string
		Product     string
		Status      string
		Total       int
		Installed   int
		Downloading int
		Failed      int
		PctDone     int
		PctDown     int
		PctFail     int
		Ago         string
	}
	var rollouts []rolloutRow
	rolloutsN := 0
	for _, u := range deployments {
		if u.Status != "pending" && u.Status != "active" {
			continue
		}
		rolloutsN++
		if len(rollouts) >= 4 {
			continue
		}
		rr := rolloutRow{
			ID: u.ID, Status: u.Status, Total: u.DeviceTotal, Installed: u.DeviceInstalled,
			Downloading: u.DeviceDownloading, Failed: u.DeviceFailed, Ago: agoShort(u.CreatedAt),
			Product: product.Label(u.Product),
		}
		if u.Release != nil {
			rr.Version = u.Release.Version
		}
		if rr.Total > 0 {
			rr.PctDone = rr.Installed * 100 / rr.Total
			rr.PctDown = rr.Downloading * 100 / rr.Total
			rr.PctFail = rr.Failed * 100 / rr.Total
		}
		rollouts = append(rollouts, rr)
	}

	// ── Product mix for the hero composition bar ──
	type productRow struct {
		Key   string
		Label string
		Count int
		Pct   int
		Class string // the template links a row to /devices?class= when set
	}
	var products []productRow
	var catalog []product.Product
	if len(prodCounts) > 0 { // the sneak peek passes none: its composition is by role
		catalog = h.productFilters(r.Context())
	}
	for _, p := range catalog {
		if n := prodCounts[p.Key]; n > 0 {
			pct := 0
			if summary.Total > 0 {
				pct = n * 100 / summary.Total
			}
			pr, _ := product.Resolve(p.Key)
			products = append(products, productRow{p.Key, p.Label, n, pct, pr.Class})
		}
	}
	sort.SliceStable(products, func(i, j int) bool { return products[i].Count > products[j].Count })

	// ── Fleet vitals ──
	batteryAvg, hasBatteryAvg := 0, false
	if n := len(daily); n > 0 && daily[n-1].BatteryAvg != nil {
		batteryAvg, hasBatteryAvg = int(math.Round(float64(*daily[n-1].BatteryAvg))), true
	}
	chargeNum, chargeDen := 0.0, 0
	var hottest float64
	hottestSerial := ""
	for _, g := range groups {
		if g.ChargingAvg != nil && g.DeviceCount > 0 {
			chargeNum += *g.ChargingAvg * float64(g.DeviceCount)
			chargeDen += g.DeviceCount
		}
		if g.TempMax != nil && *g.TempMax > hottest {
			hottest = *g.TempMax
			if g.TempMaxSerial != nil {
				hottestSerial = *g.TempMaxSerial
			}
		}
	}
	chargePct, hasCharge := 0, false
	if chargeDen > 0 {
		chargePct, hasCharge = int(math.Round(chargeNum/float64(chargeDen)*100)), true
	}
	pctOf := func(n int) int {
		if summary.Total == 0 {
			return 0
		}
		return n * 100 / summary.Total
	}
	data := map[string]any{
		"Title":  "Overview",
		"NowUTC": time.Now().UTC().Format(time.RFC3339),
		// New command-center surfaces.
		"Sites":           sites,
		"SitesOK":         sitesOK,
		"SitesWarn":       sitesWarn,
		"SitesBad":        sitesBad,
		"DeployedCount":   deployedN,
		"DeployedPct":     pctOf(deployedN),
		"KioskPct":        pctOf(summary.KioskCount),
		"Versions":        versionRows,
		"VersionsTotal":   versionsTotal,
		"VersionsCount":   len(versions),
		"VersionOther":    versionOther,
		"VersionOtherN":   versionOtherN,
		"VersionOtherPct": versionOtherPct,
		"OnPublishedPct":  onPublishedPct,
		"Rollouts":        rollouts,
		"RolloutsCount":   rolloutsN,
		"Inbox":           inbox,
		"InboxCount":      inboxN,
		"Products":        products,
		"BatteryAvg":      batteryAvg,
		"HasBatteryAvg":   hasBatteryAvg,
		"ChargePct":       chargePct,
		"HasCharge":       hasCharge,
		"Hottest":         int(math.Round(hottest)),
		"HottestSerial":   hottestSerial,
		// withRole overwrites this on success; the default keeps the template's
		// numeric comparison safe if the alerts count query fails.
		"AlertsOpenCount": 0,
		"Summary":         summary,
		"Offline":         offline,
		"Hot":             hot,
		"Attention":       attention,
		"Score":           score,
		"HealthReasons":   healthReasons,
		"HealthLost":      healthLost,
		"ScoreClass":      scoreClass,
		// 2π·r44 = 276.5; the ring template animates to this offset.
		"RingOffset":           fmt.Sprintf("%.1f", 276.5*float64(100-score)/100),
		"Verdict":              verdict,
		"Greeting":             greeting,
		"DateLine":             time.Now().Format("Monday, January 2"),
		"SparkActive":          sparkPoints(actS),
		"SparkOff":             sparkPoints(offS),
		"SparkLow":             sparkPoints(lowS),
		"SparkHot":             sparkPoints(hotS),
		"ActivityBars":         activityBars,
		"ActivityJSON":         activityJSON,
		"ActivityPeak":         peak,
		"ActiveThresholdLabel": fmt.Sprintf("%d min", activeSecs/60),
		// Command-center fields.
		"RestVerdict":     restVerdict,
		"StatusWord":      statusWord,
		"ScoreDelta":      scoreDelta,
		"HeroSentence":    heroSentence,
		"AttentionRest":   attentionRest,
		"Crashes24h":      crashStats.Total24h,
		"CrashDevices24h": crashStats.Devices24h,
		"CrashSpark":      sparkPoints(crS),
		"OnlineDelta":     tileDelta(onToday, onYest, false),
		"OfflineDelta":    tileDelta(offToday, offYest, true),
		"LowDelta":        tileDelta(lowToday, lowYest, true),
		"CrashDelta":      tileDelta(crToday, crYest, true),
	}
	return data
}
