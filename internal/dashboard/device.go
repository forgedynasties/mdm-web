package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
	"mdm/internal/shell"

	"github.com/google/uuid"
)

// DeviceSetNickname stores a friendly name for a device ("Host stand"), shown to
// restaurant owners and next to the serial on the device page.
func (h *Handler) DeviceSetNickname(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "notes", device.ID) {
		return
	}
	name := strings.TrimSpace(r.FormValue("nickname"))
	if len(name) > 40 {
		name = name[:40]
	}
	if err := h.db.SetNickname(r.Context(), device.ID, name); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.auditDev(r, "device.nickname", device.ID, device.SerialNumber, name)
	h.hxRedirect(w, r, "/devices/"+device.SerialNumber)
}

// otaStatusView maps an update_devices status (+ optional live download/install
// progress reported by the device) to a human label, a .dq-pill status class to
// reuse (already styled on the device page for the command queue), and a 0-100
// progress-bar percent. Shared by DeviceDetail's initial render and
// DeviceOtaProgress's poll fragment so they can never disagree.
func otaStatusView(status string, p *shell.OTAProgress) (label, class string, percent int) {
	// A terminal DB status is authoritative over shell.Manager's in-memory cache, which
	// has no TTL and is never cleared by an out-of-band status correction — otherwise a
	// stale live percent can keep showing next to "Installed"/"Failed" indefinitely.
	if status == "installed" || status == "failed" {
		p = nil
	}
	if p != nil {
		percent = p.Percent
		// The live phase the device just reported is finer-grained and fresher than
		// update_devices.status, which only advances at coarse checkpoints (e.g. it
		// may still read "downloading" for a beat after the device has already moved
		// into verifying/installing/finalizing) — prefer it when we have it.
		switch p.Phase {
		case "downloading":
			return "Downloading", "dl", percent
		case "verifying":
			return "Verifying", "inst", percent
		case "installing":
			return "Installing", "inst", percent
		case "finalizing":
			return "Finalizing", "inst", percent
		}
	}
	switch status {
	case "pending":
		return "Queued", "run", percent
	case "downloading":
		// No live telemetry yet (p == nil) — show the real 0%, matching the
		// deployment page's device row, which shows no percent at all in this same
		// case rather than a made-up placeholder value.
		return "Downloading", "dl", percent
	case "installing":
		if percent == 0 {
			percent = 60
		}
		return "Installing", "inst", percent
	case "awaiting_reboot":
		return "Awaiting reboot", "done", 100
	case "reboot_sent":
		return "Reboot pending", "done", 95
	case "failed":
		return "Failed", "fail", 100
	case "installed":
		return "Installed", "done", 100
	default:
		return status, "run", percent
	}
}

// DeviceOtaProgress renders just the OTA progress card as a standalone fragment —
// the device page self-polls this every 2s while an update is in flight (see
// "device-ota-progress" in device.html), the same pattern device-apps-list uses
// for install progress, since /events SSE doesn't carry OTA progress.
func (h *Handler) DeviceOtaProgress(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	// ResolveUpdateForDevice returns (nil, nil) — not an error — when nothing is
	// targeting this device, so both must be checked before touching u's fields.
	// The poller (see device-ota-progress) keeps running while OtaUpdate is set even
	// during "pending" — the card itself just stays hidden until the device actually
	// starts downloading, so the transition out of "pending" is still caught live.
	u, err := h.db.ResolveUpdateForDevice(r.Context(), device.ID)
	data := map[string]any{"Device": device}
	if err == nil && u != nil {
		label, class, percent := otaStatusView(u.DeviceStatus, h.shell.GetOTAProgress(device.ID))
		data["OtaUpdate"] = u
		data["OtaLabel"] = label
		data["OtaClass"] = class
		data["OtaPercent"] = percent
		ol := otaLineOf(u, label, percent)
		ol.ETA = otaETAText(h.shell.GetOTAProgress(device.ID), otaPkgSize(u), time.Now())
		data["OtaLine"] = &ol
	}
	data["Role"] = h.role(r)
	// Classic (frozen) keeps its own card: its poller hits this same endpoint.
	if h.deviceViewTemplate(r) == "device_classic.html" {
		h.tmpl.ExecuteTemplate(w, "classic-device-ota-progress", data)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-ota-progress", data)
}

// downsampleCheckins keeps at most maxPoints evenly-strided rows, preserving whatever
// order it's given (GetCheckinsForDuration/GetCheckinsBetween return newest-first).
// Mirrors the stride used by DeviceChartData for the on-demand range fetch.
// downsampleCheckins thins a series to roughly maxPoints while keeping its shape in
// TIME rather than in row count.
//
// Thinning by row index — every Nth row — is only equivalent to thinning by time if the
// device reports at a steady rate, and a misbehaving one does not. A unit with a
// flapping charger stores a row every couple of seconds while it flaps and one every
// two minutes when it settles: a 70x swing inside a single six-hour window, measured on
// AT070AABU00527. Taking every 7th row there leaves the busy stretch untouched and
// stretches the quiet stretch's real 140-second spacing into 16-minute holes, which the
// chart then draws as gaps in the data. They are not in the data; the thinning put them
// there. The same window fetched from /chart-data looked fine only because its larger
// budget happened to keep the induced gaps under the threshold.
//
// Bucketing by time cannot invent a gap. A sparse stretch has at most one point per
// bucket and so survives whole; only stretches denser than one point per bucket lose
// anything, which is exactly where losing points is safe.
func downsampleCheckins(checkins []db.Checkin, maxPoints int) []db.Checkin {
	n := len(checkins)
	if n <= maxPoints || maxPoints < 2 {
		return checkins
	}
	// The slice arrives newest-first; span it from whichever end is older.
	first, last := checkins[0].CreatedAt, checkins[n-1].CreatedAt
	span := first.Sub(last)
	if span < 0 {
		span = -span
	}
	if span <= 0 {
		// All at one instant — nothing time-based to do, fall back to row stride.
		stride := (n + maxPoints - 1) / maxPoints
		out := make([]db.Checkin, 0, maxPoints+1)
		for i := 0; i < n; i += stride {
			out = append(out, checkins[i])
		}
		return out
	}
	bucket := span / time.Duration(maxPoints)
	if bucket <= 0 {
		return checkins
	}
	out := make([]db.Checkin, 0, maxPoints+2)
	var haveBucket bool
	var curBucket int64
	for i, c := range checkins {
		b := c.CreatedAt.UnixNano() / int64(bucket)
		// Always keep the two ends, so the window's extent never shrinks.
		if i == 0 || i == n-1 || !haveBucket || b != curBucket {
			out = append(out, c)
			curBucket, haveBucket = b, true
		}
	}
	return out
}

// deviceActivityItem is one line of the device page's activity feed: when, who, and
// what they did, already phrased for reading.
type deviceActivityItem struct {
	At       time.Time
	Actor    string // display name
	ActorKey string // stable username, for the avatar/link helpers
	Did      string // "disabled wireless charging"
	Action   string // raw action, for the tag and for filtering
}

func (h *Handler) DeviceDetail(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		if h.refusedSerialPage(w, r, serial) {
			return
		}
		h.deviceNotFoundPage(w, r, serial)
		return
	}

	// The ~15 reads below are all independent of each other (each keyed only off
	// device.ID/role, none consumes another's result), so they were previously run
	// one at a time — total latency was the SUM of every query's round trip. Firing
	// them concurrently drops it to the SLOWEST single query. Only the four that were
	// already hard 500s on error (commands/apps/installedPkgs/kioskCfg) can fail this
	// group; the rest already tolerated errors silently (`_, err :=` discarded) and
	// keep doing so.
	var (
		chartCheckins []db.Checkin
		chartCharge   []chargeRun
		commands      []db.DeviceCommand
		queue         []db.DeviceCommand
		apps          []db.App
		installedPkgs []db.DevicePackage
		apkPkg        map[string]string
		kioskCfg      *db.DeviceConfig
		restaurants   []db.Restaurant
		deviceGroups  []db.Group
		addableGroups []db.Group
		release       *db.Release
		notes         string
		flapRate      int
		crashCount    int
		health        deviceHealth
		otaUpdate     *db.Update
		otaLabel      string
		otaClass      string
		otaPercent    int
		otaLn         *otaLine
		buildChanges  []db.BuildChange
		activity      []deviceActivityItem
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	role := h.role(r)
	ctx := r.Context()
	t0 := time.Now()
	// Access policy: a device the user may not view is hidden (404) when their
	// policy hides out-of-scope devices, else shown read-only; a device they may
	// view but not act on renders with the viewer's action set.
	engineerLook := false
	if acc := h.access(r); !acc.unrestricted() {
		if acc.engineerLookOnly(device.ID) {
			// An engineer at a venue's device: read-only, plus Screenshot and Collect logs.
			role, engineerLook = "viewer", true
		} else if !acc.canDevice("view", device.ID) {
			if acc.hidesDevices() {
				http.NotFound(w, r)
				return
			}
			role = "viewer"
		} else if !acc.anyDeviceAction(device.ID) {
			role = "viewer"
		}
	}
	focusParam := r.URL.Query().Get("focus")

	run(func() {
		// The chart normally loads the recent window. When the page is opened to
		// focus a past incident (e.g. a heat call-out deep-links ?focus=temp),
		// center the fetch on the day of that metric's extreme so the spike is in
		// range — but bounded to ~2 days, since these devices can check in every
		// few seconds and a multi-day pull would bloat the page. The client then
		// zooms to a 1-hour window around the peak.
		var cc []db.Checkin
		var err error
		var havePeak bool
		focus := r.URL.Query().Get("focus")
		if focus != "" {
			if stats, statErr := h.db.GetDeviceDailyStats(ctx, device.ID, 8); statErr == nil {
				var peakDay time.Time
				if peakDay, havePeak = peakDayForFocus(stats, focus); havePeak {
					from, until := peakDay.Add(-12*time.Hour), peakDay.Add(36*time.Hour)
					if shaped, ok := h.shapedCheckins(ctx, device.ID, from, until); ok {
						cc = shaped
					}
				}
			}
		}
		// History comes from the shaped tables (device_samples + device_state_events),
		// which cover every device's whole life since the checkins table was retired.
		if !havePeak {
			// Bake in only the default 6h view (matches the chart's default
			// currentDuration) instead of the full 48h range the duration buttons
			// can reach — these devices can check in every few seconds, so 48h can
			// be tens of thousands of rows, and rendering all of them into the page
			// HTML (3 template passes over the same slice, for battery/temp/ram)
			// was the single biggest driver of a slow first paint. The 12h/24h/48h
			// buttons pull their extra history from /chart-data on demand
			// (chartLoadOlder), and the client also warms that cache in the
			// background right after load — see chartWarmBackground in device.html.
			from := device.LastSeenAt.Add(-6 * time.Hour)
			if shaped, ok := h.shapedCheckins(ctx, device.ID, from, time.Now().UTC()); ok {
				cc = shaped
			}
		}
		if err != nil {
			fail(err)
			return
		}
		// Safety net in case a device's poll interval is far below normal — thin to
		// the same maxPoints the on-demand /chart-data endpoint already uses.
		if device.HasBattery() && device.HasCharging() {
			asc := make([]db.Checkin, len(cc))
			for i := range cc {
				asc[i] = cc[len(cc)-1-i]
			}
			chartCharge = chargeRuns(asc, chartGapMs(device.PollIntervalMs))
		}
		chartCheckins = downsampleCheckins(cc, 600) // the default 6h window; wider ranges come from /chart-data
	})
	run(func() {
		c, err := h.db.GetDeviceCommands(ctx, device.ID, h.cfg.CommandExpiry())
		if err != nil {
			fail(err)
			return
		}
		redactDeviceCommandURLs(role, c)
		commands = filterShellDeviceCommands(role, c)
	})
	run(func() {
		// Who did what to this device. Best-effort: losing it must not cost the page.
		entries, err := h.db.ListAuditForDevice(ctx, device.ID, 40)
		if err != nil {
			log.Printf("[device] ListAuditForDevice %s: %v", device.SerialNumber, err)
			return
		}
		items := h.deviceActivityItems(ctx, role, entries, 5) // the card is a glance; "all activity" has the rest
		mu.Lock()
		activity = items
		mu.Unlock()
	})
	run(func() {
		// Best-effort: build-change markers on the vitals chart. A failure here
		// only loses the markers, not the page.
		bc, err := h.db.GetBuildChanges(ctx, device.ID)
		if err != nil {
			log.Printf("[device] GetBuildChanges %s: %v", device.SerialNumber, err)
			return
		}
		mu.Lock()
		buildChanges = bc
		mu.Unlock()
	})
	run(func() {
		// The per-device command queue (non-terminal commands, FIFO) for the Queue tab.
		q, _ := h.db.GetDeviceQueue(ctx, device.ID)
		redactDeviceCommandURLs(role, q)
		queue = withoutOTA(filterShellDeviceCommands(role, q))
	})
	run(func() {
		a, err := h.db.ListApps(ctx)
		if err != nil {
			fail(err)
			return
		}
		apps = a
	})
	run(func() {
		p, err := h.db.GetDevicePackages(ctx, device.ID)
		if err != nil {
			fail(err)
			return
		}
		installedPkgs = p
	})
	run(func() { apkPkg, _ = h.db.GetApkPackageMap(ctx, nil) })
	run(func() {
		cfg, err := h.db.GetOrCreateDeviceConfig(ctx, device.ID)
		if err != nil {
			fail(err)
			return
		}
		kioskCfg = cfg
	})
	run(func() {
		if role == "admin" {
			restaurants, _ = h.db.ListRestaurants(ctx)
		}
	})
	run(func() {
		dg, _ := h.db.ListDeviceGroups(ctx, device.ID)
		deviceGroups = dg
		// Groups the device is NOT yet in — for the placement "+ group" picker.
		// Populated for the same roles the picker button renders for and the add
		// endpoint accepts (admin/dev/operator, i.e. canAdminOrOperator); gating this on
		// admin alone left an operator the button and popover but an empty group list,
		// so "+ group" did nothing.
		if roleCanOperate(role) {
			inGroup := make(map[uuid.UUID]bool, len(dg))
			for _, g := range dg {
				inGroup[g.ID] = true
			}
			if allGroups, err := h.db.ListGroups(ctx); err == nil {
				for _, g := range allGroups {
					if !inGroup[g.ID] {
						addableGroups = append(addableGroups, g)
					}
				}
			}
		}
	})
	run(func() {
		// Couple the device's reported build to a known release (release.version ==
		// device.build_id). nil = the device runs a build with no matching release.
		if device.BuildID != "" {
			release, _ = h.db.GetReleaseByVersion(ctx, device.BuildID, device.Product)
		}
	})
	run(func() { notes, _ = h.db.GetDeviceNotes(ctx, device.ID) })
	run(func() {
		// ResolveUpdateForDevice returns (nil, nil) — not an error — once nothing is
		// pushed/in-flight for this device (excludes status=='installed', and returns
		// no rows at all when there's no active deployment targeting it), so u must be
		// nil-checked too, not just err. That's what makes the device-page OTA card
		// disappear on its own once the update finishes.
		u, err := h.db.ResolveUpdateForDevice(ctx, device.ID)
		if err == nil && u != nil {
			otaUpdate = u
			otaLabel, otaClass, otaPercent = otaStatusView(u.DeviceStatus, h.shell.GetOTAProgress(device.ID))
			ol := otaLineOf(u, otaLabel, otaPercent)
			ol.ETA = otaETAText(h.shell.GetOTAProgress(device.ID), otaPkgSize(u), time.Now())
			otaLn = &ol
		}
	})
	run(func() {
		// Charger-fault detection: charging toggles per minute recently. A high rate
		// means the charger/dock connection is dropping in and out (faulty
		// hardware). >10/min is the flapping threshold (matches the
		// charger_flapping alert default).
		flapRate, _ = h.db.DeviceChargerFlapRate(ctx, device.ID, 5)
	})
	// The Alerts tab body (crash cards with full traces — megabytes on a crashy
	// device) is fetched on first open via DeviceAlertsPanel; the page needs only
	// the count for the tab badge.
	run(func() {
		n, err := h.db.CountDeviceCrashes(ctx, device.ID)
		if err == nil {
			mu.Lock()
			crashCount = n
			mu.Unlock()
		}
	})
	// The Health tab (T1): what needs attention, and the tab's count.
	run(func() {
		hv := h.deviceHealthView(ctx, r, device)
		mu.Lock()
		health = hv
		mu.Unlock()
	})

	wg.Wait()
	dbDur := time.Since(t0)
	if firstErr != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// In-flight installs: install_apk commands not yet completed/failed, so the
	// Applications list can show them as "installing" until the device reports them
	// (skipping any whose app the device already has present).
	pendingInstalls := pendingInstallRows(commands, apps, installedPkgs, apkPkg)

	// Count a Maps Embed load whenever this render will actually emit the map iframe
	// (key configured + resolved coordinates present). Browser embed loads are free,
	// but the count lets admins see map activity in the Google APIs usage panel.
	if h.mapsEmbedKey != "" && extraHasCoords(device.LatestExtra) {
		h.mapViews.Add(1)
	}

	// App locked-app choices: this device's app-drawer apps (launchable, when
	// the client reports it) filtered by the kiosk allowlist (empty allowlist =
	// all apps). Keeps the picker to apps a person could actually be locked into.
	kioskApps := make([]db.DevicePackage, 0, len(installedPkgs))
	for _, p := range launchableOnly(installedPkgs) {
		if h.cfg.KioskAppAllowed(p.PackageName) {
			kioskApps = append(kioskApps, p)
		}
	}

	// Multi-app kiosk: which extra packages are currently allowed, as a set so the
	// modal's checkbox list can mark them checked with a plain index lookup.
	kioskExtras := make(map[string]bool, len(kioskCfg.KioskPackages))
	for _, p := range kioskCfg.KioskPackages {
		kioskExtras[p] = true
	}

	// Server-Timing: visible in the browser's Network panel, so a slow page can be
	// attributed to queries vs. view assembly without log digging.
	w.Header().Set("Server-Timing", fmt.Sprintf("db;dur=%d, build;dur=%d", dbDur.Milliseconds(), (time.Since(t0)-dbDur).Milliseconds()))
	devFams, _, _ := h.libraryData(r.Context())
	// The lock rule covering this device, and whether it was overridden by hand.
	kioskRule := h.kioskRuleFor(r.Context(), *device)
	_, kioskOverride := h.db.KioskRuleMark(r.Context(), device.ID)
	if kioskRule == nil {
		kioskOverride = false
	}
	labSess, _ := h.db.GetLabSession(r.Context(), device.ID)
	fwModes, _ := h.db.FirmwareModes(r.Context())
	sec := h.securityFor(r.Context(), device)
	devRole := h.deviceRoleOf(r.Context(), device)
	h.render(w, r, h.deviceViewTemplate(r), map[string]any{
		"DevRole": devRole, // a model with several roles: picker + Needs a look rows
		"EngineerLook": engineerLook,
		"HandOver":     h.deviceHandOverView(r, device, kioskCfg != nil && kioskCfg.KioskEnabled),
		"Title":  device.SerialNumber,
		"DeviceSplit": h.displayPrefs(r).DeviceSplit, // the divider, per person
		"Needs":  deviceTodoOf(append(roleNeeds(devRole, device), deviceNeedsOf(sec, device, device.HasBattery() && deviceReportsBattery(device, chartCheckins), flapRate > 10)...), health),
		"Device": device,
		// Lab session (lab.go): the banner, the entry dialog and the lab view link.
		"Lab":          labSess,
		// At a venue and not in lab: the device runs its role's app, so the lab tools
		// (install, uninstall, shell) are not offered here. Their routes stay.
		"Restricted":   false, // installs are by hand again (roles no longer carry an app)
		"CanLab":       canLab(h.role(r)),
		"LabDurations": labDurations,
		"FirmwareMode": db.FirmwareModeOf(fwModes, modelKey(device)),
		"IsDPC":  device.IsDPC(),
		// The hosted agent build vs. the one this device reports, so the menu can
		// offer an update only when there is actually a newer one to install.
		"AgentUpdate": h.agentUpdateFor(r, device),
		"KeyCred":     h.keyCredentialFor(r.Context(), device),
		// The app this device is there to run, reported from outside it (host_app.go).
		"HostApp": hostAppView(device.LatestExtra),
		// Resource use (A1): the Apps tab's ranking, 24 h by default.
		"AppUsage": h.appUsageView(r.Context(), device, "24h", "", false),
		// Wi-Fi card (W1) on the Overview.
		"Wifi": h.deviceWifiView(r.Context(), device, h.canEditWifi(r, device)),
		// Charge-climb wear against the fleet and its own first weeks (battery_wear.go);
		// nil where there is nothing to read it from (no battery, the Standard client).
		"BatteryWear": h.batteryWearFor(r.Context(), device),
		// The networks the device says it has saved (client 1.12.0 / 0.3.0), names only.
		"SavedNetworks": savedNetworksView(device.LatestExtra),
		// Lost mode: the band at the top of the page and the dialog's defaults (lost_mode.go).
		"Lost": h.lostModeFor(r.Context(), device),
		// Its time out of reach (lost_reporting.go): the band's "last heard" and the Location
		// tab's timeline. nil when it was never lost and has not left lately.
		"LostReport": h.lostReportFor(r.Context(), device),
		// Lifetime battery wear by day, so hovering the graph can read out cycles as of
		// that moment — the same measure as the "Battery cycles" card, which is what
		// anyone comparing the two expects.
		"CycleSeries": dischargeSeriesJSON(h.db.DeviceDischargeSeries(r.Context(), device.ID)),
		"Custody":     deviceCustodyOrEmpty(h.db.DeviceCustody(r.Context(), device.ID)),
		// Which update path this device is on: a build whose client can't apply an
		// MDM OTA still updates, over the legacy otautil listener.
		"OTAGate": h.otaGate.Device(r.Context(), *device),
		// Servers to offer when moving a device: this one first, then any peer named
		// in MDM_PEER_URLS (comma separated) — a stage box, usually.
		"MDMServers":       h.mdmServerChoices(),
		"ThisServer":       h.thisServerURL(),
		"Caps":             device.CapSet(),
		"Classes":          product.Classes(),
		"DeviceCrashCount": crashCount,
		"Health":           health,
		"ChargerFlapRate":  flapRate,
		"ChargerFlapping":  flapRate > 10,
		"Notes":            notes,
		"Release":          release,
		"OtaUpdate":        otaUpdate,
		"OtaLabel":         otaLabel,
		"OtaClass":         otaClass,
		"OtaPercent":       otaPercent,
		"OtaLine":          otaLn,
		"Online":           h.hub.IsConnectedForDisplay(device.ID),
		"ChartCheckins":    chartCheckins,
		// Whether to render any battery UI at all. The product catalog is the first
		// word, but a device can claim a battery its hardware never reports (a kiosk on
		// a legacy or unknown product key), and then every battery surface draws an
		// empty chart and a 0%. So a claimed battery must also show up in telemetry.
		"ShowBattery":       device.HasBattery() && deviceReportsBattery(device, chartCheckins),
		// A battery product whose pack is not detected (G1, 21 Oct 2026): the graph stays,
		// with its "no pack" stretches hatched, instead of vanishing with the 0%.
		"BatteryFault":     device.HasBattery() && extractBatteryMissing(device.LatestExtra),
		"ShowBatteryChart": device.HasBattery() && (deviceReportsBattery(device, chartCheckins) || extractBatteryMissing(device.LatestExtra)),
		// No reading, no Temp tab: a device with no sensor the client can read (the Sunmi
		// D2s KDS) showed an empty temperature graph.
		"ShowTempChart":    deviceReportsTemp(device.LatestExtra),
		"ChartCharge":       chartCharge,
		"ChartFocus":        focusParam,
		"IsOwner":           roleIsCustomer(h.role(r)),
		"Nickname":          func() string { m, _ := h.db.GetNicknames(ctx, []uuid.UUID{device.ID}); return m[device.ID] }(),
		"HardwareSerial":    h.db.HardwareSerialOf(ctx, device.SerialNumber),
		"NuggetIDs":         nuggetAndroidIDs(device.LatestExtra),
		"NuggetIDsSame":     nuggetIDsAllSame(nuggetAndroidIDs(device.LatestExtra)),
		"NuggetIDsJSON":     nuggetIDsJSON(nuggetAndroidIDs(device.LatestExtra)),
		"NuggetIDByPackage": nuggetIDByPackage(device.LatestExtra),
		// The table guests are told they are at (home and lock screen on a T7).
		"BuildChanges":        buildChanges,
		"Activity":            activity,
		"Commands":            commands,
		"Queue":               queue,
		"ExtraColumns":        h.cfg.Columns(),
		"Apps":                apps,
		"Families":            devFams,
		"InstalledPackages":   launchableOnly(installedPkgs),
		"AppList":             h.deviceAppsOf(r.Context(), device, installedPkgs, hostAppView(device.LatestExtra), ""),
		"ClientPill":          h.clientPillFor(device),
		"KioskApps":           kioskApps,
		"LockApp":             lockAppFor(*device, h.db.RoleAppBase(r.Context(), device.Class()), installedPkgs, kioskCfg, h.cfg.KioskAppAllowed),
		"KioskAllowlistCSV":   strings.Join(h.cfg.KioskAllowlist(), ","),
		"KioskExtras":         kioskExtras,
		"PendingInstalls":     pendingInstalls,
		"Uninstalling":        pendingUninstallPkgs(commands),
		"InstalledSet":        pkgNameSet(installedPkgs),
		"KioskConfig":         kioskCfg,
		"KioskExit":           h.kioskExitFor(r, device),
		// Reporting but unreachable: fresh telemetry, no socket. The badge says Offline and
		// cannot say why it matters, so the banner does. See command_channel.go.
		"CommandChannel":      h.commandChannelFor(r, device),
		"LateSpans":           h.lateSpansFor(r, device),
		"KioskRule":           kioskRule,
		"KioskOverride":       kioskOverride,
		"WlcApplicable":       h.cfg.WlcApplies(device.ProductKey()),
		"PadState":            padState(device, h.cfg.WlcApplies(device.ProductKey()), kioskCfg.WlcChargingEnabled),
		"MicGain":             micGainPtr(device.LatestExtra),
		"Security":            sec,
		"BackendLine":         h.deviceBackendLine(r.Context(), device), // M2: where the AIO backend places it
		"EnrolledByName":      h.displayName(r, device.EnrolledBy),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ShellEnabled":        h.cfg.ShellEnabled(),
		"RemoteEnabled":       h.cfg.RemoteEnabled(),
		"CanRemote":           h.access(r).canDevice("remote", device.ID),
		"CanShell":            h.access(r).canDevice("shell", device.ID),
		"CanCollectLogs":      h.authorizeCommand(h.role(r), "collect_logs") == cmdAuthzOK && h.access(r).canDevice(policyActionForCommand("collect_logs"), device.ID),
		"Restaurants":         restaurants,
		"DeviceGroups":        deviceGroups,
		"AddableGroups":       addableGroups,
		"MapsEmbedKey":        h.mapsEmbedKey,
	})
}

func (h *Handler) DeviceRemote(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.RemoteEnabled() {
		http.Error(w, "Remote control is disabled by an administrator.", http.StatusForbidden)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !device.Supports("remote") || device.IsMDMLite() {
		http.Error(w, "Remote control isn't available on this device's agent ("+device.KindLabel()+").", http.StatusBadRequest)
		return
	}

	h.render(w, r, "remote.html", map[string]any{
		"Title":  "Remote — " + device.SerialNumber,
		"Serial": device.SerialNumber,
		"Online": h.hub.IsConnected(device.ID),
		"Device": device,
		// Single-use, short-lived token instead of the admin API key (which must
		// never reach the browser). The control WebSocket redeems it server-side.
		"Token": h.remote.IssueToken(device.ID, 2*time.Minute),
		"Kbd":   remoteKbdLevel(device),
		// Menu boards (TV boxes) are driven by a remote: the button column beside the
		// screen becomes a D-pad (P2, picked 22 Oct 2026; user: "for menu board, add a
		// dpad remote for navigation in remote control").
		"DPad": device.Class() == product.ClassDongle,
	})
}

// remoteKbdLevel says how much of a keyboard the remote page can give a device (R1,
// 22 Oct 2026; user: "make remote control like scrcpy, so that keyboard nav and
// keyboard keys work"):
//
//	full  firmware client 1.16.0+ (reports remote_input >= 2): keys with Shift/Ctrl/Alt,
//	      repeat, a text event for any character, scroll, clipboard both ways
//	keys  older firmware clients: plain keys only (no modifiers, a–z and 0–9 typing)
//	dpc   the Standard client: Accessibility only, so arrows, Enter, Back, Home, Recents
func remoteKbdLevel(d *db.Device) string {
	if d.AgentKind == product.KindDPC {
		return "dpc"
	}
	if extraInt64(d.LatestExtra, "remote_input") >= 2 {
		return "full"
	}
	return "keys"
}

// DeviceRemoteToken mints a fresh single-use control-socket token for a remote page
// that is already open. The page's embedded token is consumed by its first connect,
// so every reconnect (and the H.264 -> JPEG fallback) asks here for a new one. Same
// guards as the page itself.
func (h *Handler) DeviceRemoteToken(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.RemoteEnabled() {
		http.Error(w, "Remote control is disabled by an administrator.", http.StatusForbidden)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": h.remote.IssueToken(device.ID, 2*time.Minute)})
}

// DeviceShellPage renders the interactive shell console for a device. The console
// reuses the existing command-create (JSON) + command-output SSE endpoints: each
// submitted line is sent as a `shell` command and its output is streamed back.
func (h *Handler) DeviceShellPage(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	h.render(w, r, "device_shell.html", map[string]any{
		"Title":  "Shell · " + serial,
		"Device": device,
		"Online": h.hub.IsConnected(device.ID),
		"ShellSession": supportsShellSession(device),
		"ShellQueue":   h.shellQueue(r, device.ID),
	})
}

func (h *Handler) DeviceStatsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	kioskCfg, _ := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID)
	h.renderCachedHTML(w, r, "device-stats", map[string]any{
		"Device":              device,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"KioskConfig":         kioskCfg,
	})
}

// DeviceVitalsPartial renders the hero vitals chips (battery, charging, temp,
// RAM, uptime, Wi-Fi) as a standalone fragment so the device page can refresh
// them live on each "device-updated" SSE event without a full reload.
func (h *Handler) DeviceVitalsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	flapRate, _ := h.db.DeviceChargerFlapRate(r.Context(), device.ID, 5)
	tpl := "device-vitals"
	if r.URL.Query().Get("cols") == "1" {
		tpl = "device-vitals-cols" // the device pane's columns (Overview)
	}
	pad := ""
	if kc, err := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID); err == nil && kc != nil {
		pad = padState(device, h.cfg.WlcApplies(device.ProductKey()), kc.WlcChargingEnabled)
	}
	h.renderCachedHTML(w, r, tpl, map[string]any{
		"Device": device,
		// Same rule as the full page. Missing here, every live refresh fell through to the
		// "no reading yet" chip, so the battery flipped from "100% ⚡" to "—" a moment
		// after the page opened.
		"ShowBattery":         device.HasBattery() && deviceReportsBattery(device, nil),
		"Role":                h.role(r), // the mic-gain chip is admin-only
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ChargerFlapRate":     flapRate,
		"ChargerFlapping":     flapRate > 10,
		"PadState":            pad,
	})
}

// DeviceInspectorPanel renders the compact "inspector" card for a single
// device, loaded into the split-pane drawer on the devices list so an operator
// can glance at a unit without leaving the 1,000-row roster. It deliberately
// mirrors the at-a-glance vitals only; deep work still happens on the full
// /devices/{serial} page, linked from the panel.
func (h *Handler) DeviceInspectorPanel(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	var release *db.Release
	if device.BuildID != "" {
		release, _ = h.db.GetReleaseByVersion(r.Context(), device.BuildID, device.Product)
	}
	h.renderCachedHTML(w, r, "device-panel", map[string]any{
		"Device":              device,
		"Online":              h.hub.IsConnectedForDisplay(device.ID),
		"Release":             release,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"ShellEnabled":        h.cfg.ShellEnabled(),
		"Role":                h.role(r),
	})
}

func (h *Handler) DeviceCommandsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), commands)
	commands = filterShellDeviceCommands(h.role(r), commands)
	h.renderCachedHTML(w, r, "device-commands", map[string]any{
		"Device":   device,
		"Commands": commands,
		"Online":   h.hub.IsConnectedForDisplay(device.ID),
	})
}

// withoutOTA drops OTA updates from the per-device command queue: an OTA runs
// alongside commands (its own progress card on the device page), it is not a
// queued command and never blocks one.
func withoutOTA(in []db.DeviceCommand) []db.DeviceCommand {
	out := in[:0:0]
	for _, c := range in {
		if c.Type != "ota" {
			out = append(out, c)
		}
	}
	return out
}

// DeviceQueuePartial renders the per-device command queue (non-terminal commands, FIFO)
// for the Queue tab — refetched live on device-updated so it drains in place as the device
// works through it.
func (h *Handler) DeviceQueuePartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	queue, err := h.db.GetDeviceQueue(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(h.role(r), queue)
	queue = withoutOTA(filterShellDeviceCommands(h.role(r), queue))
	h.renderCachedHTML(w, r, "device-queue", map[string]any{
		"Device": device,
		"Queue":  queue,
		"Online": h.hub.IsConnectedForDisplay(device.ID),
		"Role":   h.role(r),
	})
}

func (h *Handler) DeviceClearOTA(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.db.ClearPendingOTACommands(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxDone(w, r, "/devices/"+serial, "device-updated")
}

// deviceReportsBattery reports whether the hardware actually sends battery readings.
// True when there is no evidence either way (a device that has only just enrolled), so
// a new T7 keeps its battery UI until its checkins say otherwise.
// deviceReportsTemp: the device's last report carries a temperature, battery or CPU.
// A keyframe replaces the whole snapshot, so a device with a sensor always has one.
func deviceReportsTemp(extra json.RawMessage) bool {
	_, _, ok := deviceTempC(extra)
	return ok
}

func deviceReportsBattery(d *db.Device, recent []db.Checkin) bool {
	if d.BatteryPct > 0 {
		return true
	}
	if len(recent) == 0 {
		return true // nothing to judge by yet: trust the product catalog
	}
	for _, c := range recent {
		if c.BatteryPct > 0 {
			return true
		}
	}
	return false
}
