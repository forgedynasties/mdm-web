package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
)

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// post registers a state-changing route behind the same-origin (CSRF) guard.
	// Use it for every POST so cross-site requests can't drive an authenticated
	// browser session; GET routes stay on mux.HandleFunc directly.
	post := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, h.enforceSameOrigin(handler))
	}

	h.registerAppRoutes(mux)
	mux.HandleFunc("GET /login", h.LoginPage)
	post("POST /login", h.LoginSubmit)
	post("POST /logout", h.Logout)
	mux.HandleFunc("GET /signup", h.SignupPage)
	post("POST /signup", h.SignupSubmit)
	mux.HandleFunc("GET /auth/microsoft", h.MicrosoftLoginStart)
	mux.HandleFunc("GET /auth/microsoft/callback", h.MicrosoftLoginCallback)
	mux.HandleFunc("GET /verify-email", h.VerifyEmailPage)
	post("POST /verify-email", h.VerifyEmailSubmit)
	mux.HandleFunc("GET /forgot-password", h.ForgotPasswordPage)
	post("POST /forgot-password", h.ForgotPasswordSubmit)
	mux.HandleFunc("GET /reset-password", h.ResetPasswordPage)
	post("POST /reset-password", h.ResetPasswordSubmit)

	mux.HandleFunc("GET /sneak-peek", h.SneakPeek)
	mux.HandleFunc("GET /sneak-peek/alerts", h.SneakPeekAlerts)
	mux.HandleFunc("GET /sneak-peek/fleet", h.SneakPeekFleet)

	mux.HandleFunc("GET /{$}", h.Root)
	mux.HandleFunc("GET /devices", h.requireAuth(h.DeviceList))
	mux.HandleFunc("GET /devices/select-serials", h.requireAuth(h.DeviceSelectAllSerials))
	mux.HandleFunc("GET /devices/filter-counts", h.requireAuth(h.FleetFilterCounts))
	mux.HandleFunc("GET /devices/search", h.requireAuth(h.DeviceSearch))
	mux.HandleFunc("GET /cmdk-index", h.requireAuth(h.CmdkIndex))
	mux.HandleFunc("GET /events/devices", h.requireAuth(drainable(h.FleetEvents)))
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.deviceRoute("view", h.DeviceDetail)))
	mux.HandleFunc("GET /devices/{serial}/history", h.requireAuth(h.deviceRoute("view", h.DeviceHistory)))
	mux.HandleFunc("GET /devices/{serial}/chart-data", h.requireAuth(h.deviceRoute("view", h.DeviceChartData)))
	mux.HandleFunc("GET /devices/{serial}/app-usage", h.requireAuth(h.deviceRoute("view", h.DeviceAppUsage)))
	// Wi-Fi card (W1): the card, polled while a change runs; changes are admin only.
	mux.HandleFunc("GET /devices/{serial}/wifi", h.requireAuth(h.deviceRoute("view", h.DeviceWifi)))
	post("POST /devices/{serial}/wifi", h.requireAdmin(h.deviceRoute("view", h.DeviceWifiSave)))
	mux.HandleFunc("GET /devices/{serial}/events", h.requireAuth(drainable(h.deviceRoute("view", h.DeviceEvents))))
	mux.HandleFunc("GET /devices/{serial}/ws-status", h.requireAuth(h.deviceRoute("view", h.DeviceOnlineStatus)))
	mux.HandleFunc("GET /devices/{serial}/presence-stream", h.requireAuth(drainable(h.deviceRoute("view", h.DevicePresenceStream))))
	mux.HandleFunc("GET /devices/{serial}/stats", h.requireAuth(h.deviceRoute("view", h.DeviceStatsPartial)))
	mux.HandleFunc("GET /devices/{serial}/vitals", h.requireAuth(h.deviceRoute("view", h.DeviceVitalsPartial)))
	mux.HandleFunc("GET /devices/{serial}/panel", h.requireAuth(h.deviceRoute("view", h.DeviceInspectorPanel)))
	mux.HandleFunc("GET /devices/{serial}/battery.csv", h.requireAuth(h.deviceRoute("view", h.DeviceBatteryCSV)))
	mux.HandleFunc("GET /devices/{serial}/daily-stats", h.requireAuth(h.deviceRoute("view", h.DeviceDailyStatsJSON)))
	mux.HandleFunc("GET /devices/{serial}/shell", h.requireReleaseAdmin(h.deviceRoute("shell", h.DeviceShellPage)))
	// Interactive shell sessions (shell_session.go).
	mux.HandleFunc("GET /devices/{serial}/shell/session", h.requireReleaseAdmin(drainable(h.deviceRoute("shell", h.ShellSessionStream))))
	post("POST /devices/{serial}/shell/session/{sid}/input", h.requireReleaseAdmin(h.deviceRoute("shell", h.ShellSessionInput)))
	// Releases (pipeline.go): what every role runs, and the per-model firmware.
	mux.HandleFunc("GET /lineup", h.requireAuth(h.Pipeline))
	mux.HandleFunc("GET /pipeline", h.requireAuth(h.PipelineRedirect))
	post("POST /pipeline/firmware", h.requireOTA(h.requireFleetOTA(h.PipelineFirmware)))
	post("POST /pipeline/rollout", h.requireLab(h.PipelineRollout))
	// Lab sessions (lab.go): the timed lab view and its actions.
	mux.HandleFunc("GET /devices/{serial}/lab", h.requireLab(h.deviceRoute("shell", h.LabView)))
	post("POST /devices/{serial}/lab/open", h.requireLab(h.deviceRoute("shell", h.LabOpen)))
	post("POST /devices/{serial}/lab/extend", h.requireLab(h.deviceRoute("shell", h.LabExtend)))
	post("POST /devices/{serial}/lab/end", h.requireLab(h.deviceRoute("shell", h.LabEnd)))
	post("POST /devices/{serial}/lab/install", h.requireLab(h.deviceRoute("shell", h.LabInstall)))
	mux.HandleFunc("GET /devices/{serial}/commands-status", h.requireAuth(h.deviceRoute("view", h.DeviceCommandsPartial)))
	mux.HandleFunc("GET /devices/{serial}/timeline", h.requireAuth(h.deviceRoute("view", h.DeviceTimelinePartial)))
	mux.HandleFunc("GET /devices/{serial}/compliance", h.requireAuth(h.deviceRoute("view", h.DeviceCompliancePartial)))
	mux.HandleFunc("GET /devices/{serial}/alerts-panel", h.requireAuth(h.deviceRoute("view", h.DeviceAlertsPanel)))
	mux.HandleFunc("GET /devices/{serial}/queue", h.requireAuth(h.deviceRoute("view", h.DeviceQueuePartial)))
	post("POST /devices/{serial}/queue/clear", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceQueueClear)))
	post("POST /devices/{serial}/queue/{id}/remove", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceQueueRemove)))
	post("POST /devices/{serial}/installs/cancel", h.requireOperatorOrAdmin(h.deviceRoute("queue", h.DeviceInstallCancel)))
	post("POST /devices/{serial}/commands", h.requireAuth(h.deviceRoute("view", h.DeviceCommandCreate)))
	// Wireless adb page: batch on/off and tunnels (admin only, like adb_tcp itself).
	mux.HandleFunc("GET /adb", h.requireAdmin(h.AdbPage))
	post("POST /adb/commands", h.requireAdmin(h.AdbBatch))
	// One connect flow for the page and the device pill (adb_flow.go): on + tunnel, then progress.
	post("POST /adb/connect", h.requireAdmin(h.AdbConnect))
	mux.HandleFunc("GET /adb/progress", h.requireAdmin(h.AdbProgress))
	post("POST /adb/tunnel", h.requireAdmin(h.AdbTunnelOpen))
	post("POST /adb/tunnel/{id}/end", h.requireAdmin(h.AdbTunnelEnd))
	post("POST /adb/tunnel/{id}/allow", h.requireAdmin(h.AdbTunnelAllow))
	post("POST /devices/{serial}/poll-interval", h.requireAdmin(h.deviceRoute("view", h.DeviceSetPollInterval)))
	post("POST /devices/{serial}/move-server", h.requireStrictAdmin(h.deviceRoute("shell", h.DeviceMoveServer)))
	post("POST /devices/{serial}/key-reset", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceKeyReset)))
	post("POST /devices/{serial}/notes", h.requireSupport(h.deviceRoute("notes", h.DeviceNotesUpdate)))
	post("POST /devices/{serial}/nickname", h.requireSupport(h.deviceRoute("notes", h.DeviceSetNickname)))
	post("POST /devices/{serial}/kiosk", h.requireSupport(h.deviceRoute("kiosk", h.DeviceKioskUpdate)))
	post("POST /devices/{serial}/lock", h.requireSupport(h.deviceRoute("kiosk", h.DeviceLockHost)))
	post("POST /devices/{serial}/kiosk/relock", h.requireSupport(h.deviceRoute("kiosk", h.DeviceKioskRelock)))
	post("POST /devices/{serial}/kiosk/leave-out", h.requireSupport(h.deviceRoute("kiosk", h.DeviceKioskLeaveOut)))
	post("POST /devices/{serial}/kiosk/follow-rule", h.requireSupport(h.deviceRoute("kiosk", h.DeviceKioskFollowRule)))
	post("POST /devices/{serial}/wlc", h.requireAdminOrOperator(h.deviceRoute("kiosk", h.DeviceWlcUpdate)))
	post("POST /devices/{serial}/clear-ota", h.requireAdmin(h.deviceRoute("view", h.DeviceClearOTA)))
	// Remote screen capture + input injection is highly sensitive (full control of the
	// device), so it is restricted to admins only.
	mux.HandleFunc("GET /devices/{serial}/remote", h.requireAdminOrOperator(h.deviceRoute("remote", h.DeviceRemote)))
	mux.HandleFunc("GET /devices/{serial}/remote/token", h.requireAdminOrOperator(h.deviceRoute("remote", h.DeviceRemoteToken)))
	post("POST /devices/bulk-restaurant", h.requireAdminOrOperator(h.BulkAssignRestaurant))
	post("POST /devices/bulk-nickname", h.requireAdminOrOperator(h.BulkNickname))
	post("POST /devices/bulk-retire", h.requireStrictAdmin(h.BulkRetire))
	post("POST /devices/bulk-kiosk", h.requireAdminOrOperator(h.BulkKioskUpdate))
	post("POST /devices/bulk-kiosk-apps", h.requireAdminOrOperator(h.BulkKioskApps))
	mux.HandleFunc("GET /export", h.requireAuth(h.ExportPage))
	post("POST /export/csv", h.requireAuth(h.ExportCSV))
	mux.HandleFunc("GET /export/report/inventory.csv", h.requireAuth(h.fleetWide(h.ReportInventoryCSV)))
	mux.HandleFunc("GET /export/report/activity.csv", h.requireAuth(h.ReportActivityCSV))
	// Admin-only for now (see the Overview card's same gate) — loosen to
	// requireAuth if this opens up to other roles later.
	mux.HandleFunc("GET /export/visualize", h.requireAuth(h.ExportVisualizePage))
	mux.HandleFunc("GET /devices/{serial}/packages", h.requireAuth(h.deviceRoute("view", h.DevicePackages)))
	mux.HandleFunc("GET /devices/{serial}/apps-list", h.requireAuth(h.deviceRoute("view", h.DeviceAppsList)))
	mux.HandleFunc("GET /devices/{serial}/ota-progress", h.requireAuth(h.deviceRoute("view", h.DeviceOtaProgress)))
	// App classification is a tab of the Apps hub (P1, 23 Oct 2026).
	mux.HandleFunc("GET /apps/classification", h.requireStrictAdmin(h.FleetPackages))
	mux.Handle("GET /packages", http.RedirectHandler("/apps/classification", http.StatusMovedPermanently))
	post("POST /packages/flag", h.requireStrictAdmin(h.PackageFlag))
	mux.HandleFunc("GET /devices/{serial}/logcat/live", h.requireAuth(h.deviceRoute("logcat", h.LogcatLivePage)))
	mux.HandleFunc("GET /devices/{serial}/logcat/stream", h.requireAuth(drainable(h.deviceRoute("logcat", h.LogcatStream))))

	mux.HandleFunc("GET /groups/new", h.requireAdminOrOperator(h.GroupNew))
	mux.HandleFunc("GET /groups/new/devices", h.requireAdminOrOperator(h.GroupNewDevices))
	mux.HandleFunc("GET /groups", h.requireAuth(h.GroupList))
	post("POST /groups", h.requireAdminOrOperator(h.GroupCreate))
	mux.HandleFunc("GET /groups/{id}", h.collectionRoute("group", h.requireAuth(h.GroupDetail)))
	mux.HandleFunc("GET /groups/{id}/devices-modal", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupDevicesModal)))
	mux.HandleFunc("GET /groups/{id}/device-search", h.collectionRoute("group", h.requireAuth(h.GroupDeviceSearch)))
	mux.HandleFunc("GET /groups/{id}/members", h.collectionRoute("group", h.requireAuth(h.GroupMembers)))
	mux.HandleFunc("GET /groups/{id}/daily-stats", h.collectionRoute("group", h.requireAuth(h.GroupDailyStatsJSON)))
	mux.HandleFunc("GET /fleet-health", h.requireAuth(h.FleetHealth))
	mux.HandleFunc("GET /map", h.requireAuth(h.fleetWide(h.MapPage)))
	mux.HandleFunc("GET /devices/map.json", h.requireAuth(h.DeviceMapData))
	post("POST /overview/layout", h.requireAuth(h.OverviewLayoutSave))
	post("POST /overview/layout/reset", h.requireAuth(h.OverviewLayoutReset))
	post("POST /tour/done", h.requireAuth(h.TourDone))
	post("POST /tour/reset", h.requireAuth(h.TourReset))
	mux.HandleFunc("GET /sw.js", h.ServiceWorker)
	mux.HandleFunc("GET /reports/alerts-by-restaurant", h.requireAuth(h.ReportAlertsByRestaurant))
	mux.HandleFunc("GET /alerts/newest", h.requireAuth(h.AlertNewest))
	mux.HandleFunc("GET /dock/status", h.requireAuth(h.DockStatus))
	mux.HandleFunc("GET /alerts", h.requireAuth(h.AlertList))
	mux.HandleFunc("GET /alert-config", h.requireAdmin(h.AlertConfigView))
	// Requires auth: the page exposes real device serials and restaurant/venue names,
	// so it must not be anonymous even though it's a standalone "wrapped" page.
	mux.HandleFunc("GET /wrapped", h.requireAuth(h.fleetWide(h.WrappedPage)))
	mux.HandleFunc("GET /alerts/recent", h.requireAuth(h.AlertsRecent))
	mux.HandleFunc("GET /alerts/events", h.requireAuth(drainable(h.AlertEvents)))
	post("POST /alerts/bulk", h.requireSupport(h.AlertBulk))
	post("POST /alerts/ack-all", h.requireSupport(h.AlertAckAll))
	post("POST /alerts/resolve-all", h.requireSupport(h.AlertResolveAll))
	post("POST /alerts/clear-all", h.requireAdmin(h.AlertClearAll))
	post("POST /alerts/problem", h.requireSupport(h.AlertProblemAction))
	post("POST /alerts/{id}/ack", h.requireSupport(h.AlertAck))
	post("POST /alerts/{id}/resolve", h.requireSupport(h.AlertResolve))
	post("POST /groups/{id}", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupUpdate)))
	post("POST /groups/{id}/delete", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupDelete)))
	post("POST /groups/{id}/devices", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupAddDevice)))

	// Restaurants (venue object). Static sub-paths registered before /{id}.
	mux.HandleFunc("GET /restaurants/new", h.requireAdminOrOperator(h.RestaurantNew))
	mux.HandleFunc("GET /restaurants/new/device-picker", h.requireAdminOrOperator(h.RestaurantNewDevices))
	post("POST /clients/auto-update", h.requireAdmin(h.ClientAutoUpdateSet))
	mux.HandleFunc("GET /restaurants", h.requireAuth(h.RestaurantList))
	post("POST /restaurants", h.requireAdminOrOperator(h.RestaurantCreate))
	mux.HandleFunc("GET /restaurants/{id}", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantDetail)))
	mux.HandleFunc("GET /restaurants/{id}/edit", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantEdit)))
	mux.HandleFunc("GET /restaurants/{id}/icon", h.requireAuth(h.RestaurantIconImage))
	post("POST /restaurants/{id}/kiosk", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantKioskSave)))
	mux.HandleFunc("GET /restaurants/{id}/devices-modal", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantDevicesModal)))
	mux.HandleFunc("GET /restaurants/{id}/daily-stats", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantDailyStatsJSON)))
	mux.HandleFunc("GET /restaurants/{id}/members", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantMembers)))
	mux.HandleFunc("GET /restaurants/{id}/report", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantReport)))
	// Deliberately unauthenticated: the token in the path is the credential, so a venue
	// owner can open their report from an email without a dashboard account. See the
	// note at the top of report_pdf.go.
	mux.HandleFunc("GET /reports/{token}", h.ReportPDFServe)
	mux.HandleFunc("GET /reports/{id}/{week}/{token}", h.ReportPDFToken)
	mux.HandleFunc("GET /restaurants/{id}/report.pdf", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantReportPDF)))
	// The monthly device-security report: the login-free link a report email carries
	// (answers only while customer compliance reports are on), and the venue page's copy.
	mux.HandleFunc("GET /reports/{id}/month/{month}/{token}", h.ComplianceMonthPDFToken)
	mux.HandleFunc("GET /restaurants/{id}/compliance.pdf", h.collectionRoute("restaurant", h.requireAuth(h.RestaurantCompliancePDF)))
	post("POST /restaurants/{id}/report/email", h.requireAdminOrOperator(h.fleetWide(h.RestaurantReportEmail)))
	post("POST /restaurants/{id}", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantUpdate)))
	post("POST /restaurants/{id}/profile", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantProfileSave)))
	post("POST /restaurants/{id}/icon", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantIconSave)))
	post("POST /restaurants/{id}/location", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantLocationSave)))
	mux.HandleFunc("GET /restaurants/{id}/geocode", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantGeocode)))
	post("POST /restaurants/{id}/rename", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantRename)))
	// The venue's device Wi-Fi (venue_wifi.go): admins only, it writes a credential to every device there.
	post("POST /restaurants/{id}/wifi", h.collectionRoute("restaurant", h.requireAdmin(h.RestaurantWifiSave)))
	// Self-lock (S1, lost-device reporting): devices here lock themselves when they leave.
	post("POST /restaurants/{id}/self-lock", h.collectionRoute("restaurant", h.requireAdmin(h.RestaurantSelfLock)))
	post("POST /restaurants/{id}/staff-pin", h.collectionRoute("restaurant", h.requireAdmin(h.RestaurantStaffPIN)))
	post("POST /restaurants/{id}/wifi/{nid}/remove", h.collectionRoute("restaurant", h.requireAdmin(h.RestaurantWifiRemove)))
	post("POST /restaurants/{id}/delete", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantDelete)))
	mux.HandleFunc("GET /restaurants/{id}/device-picker", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantDevicePicker)))
	post("POST /restaurants/{id}/devices", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantAssignDevices)))
	post("POST /restaurants/{id}/devices/{serial}/remove", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantRemoveDevice)))
	post("POST /restaurants/{id}/service-window", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantSetServiceWindow)))
	post("POST /restaurants/{id}/updates", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantUpdatesSave)))
	post("POST /restaurants/{id}/lost", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantLostSave)))
	post("POST /restaurants/{id}/auto-enroll", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantAutoEnrollSave)))
	post("POST /restaurants/{id}/host-app", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantHostAppSave)))
	post("POST /restaurants/{id}/backend", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantBackendSave)))
	post("POST /restaurants/{id}/backend-move", h.collectionRoute("restaurant", h.requireAdminOrOperator(h.RestaurantBackendMove)))
	post("POST /restaurants/backend-place", h.requireAdminOrOperator(h.BackendPlaceSave))
	post("POST /devices/{serial}/restaurant", h.requireAdmin(h.deviceRoute("view", h.DeviceSetRestaurant)))
	post("POST /groups/{id}/devices/remove", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupBulkRemoveDevice)))
	post("POST /groups/{id}/devices/{serial}/remove", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupRemoveDevice)))
	post("POST /groups/{id}/commands", h.collectionRoute("group", h.requireAdminOrOperator(h.GroupCommandCreate)))

	// Productions is owned by the test team: admin/dev/operator can list, view,
	// create and export (requireAdminOrOperator). Deletion stays admin/dev-only.
	// Factory runs live on Lineup › Setup (23 Oct 2026); the list page redirects there.
	mux.Handle("GET /productions", http.RedirectHandler("/lineup?view=setup#runs", http.StatusMovedPermanently))
	mux.HandleFunc("GET /productions/new", h.requireAdminOrOperator(h.ProductionNew))
	post("POST /productions", h.requireAdminOrOperator(h.ProductionCreate))
	mux.HandleFunc("GET /productions/preview-serial", h.requireAdminOrOperator(h.ProductionPreviewSerial))
	mux.HandleFunc("GET /productions/{id}", h.requireAdminOrOperator(h.fleetWide(h.ProductionDetail)))
	mux.HandleFunc("GET /productions/{id}/export.csv", h.requireAdminOrOperator(h.fleetWide(h.ProductionExportCSV)))
	post("POST /productions/{id}/delete", h.requireAdmin(h.ProductionDelete))
	post("POST /productions/{id}/model", h.requireAdminOrOperator(h.ProductionSetModel))

	// The console is gone. Every verb it owned now starts from the thing it acts on — an
	// install from the app in the library, diagnostics and the destructive pair from a
	// device selection, kiosk from the venue — and /jobs is where the work is watched.
	// The page itself redirects; POST /commands stays, because it is the one send path
	// every one of those surfaces posts to, and /commands/{id} stays for deep links.
	mux.HandleFunc("GET /commands", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs", http.StatusMovedPermanently)
	}))
	// Work in flight and what has run — the Actions page's other half, on its own route so
	// the console can be removed without taking it along.
	mux.HandleFunc("GET /jobs", h.requireAuth(h.CommandList))
	// Kiosk: the page is called Kiosk, so the URL is /kiosk. It answered to /manage for
	// historical reasons, which made the one safety-critical feature in the product the
	// one page whose address did not match its name. /manage still answers — bookmarks
	// and old links are not worth breaking — and redirects.
	// "Kiosk" is a product here — the Kiosk 18/22/27 wall panels, and device_class
	// 'kiosk' — so the feature that locks ANY device to one app (T7s and KDS included)
	// is called Lock in the UI. The config keys, the policy table and the device class
	// keep their names: they are the wire protocol to clients in the field and the
	// product's own taxonomy, neither of which is a label.
	mux.HandleFunc("GET /lock", h.requireAuth(h.Manage))
	// Compliance (static plan of 15 Oct; rules editor B1): read by anyone with fleet
	// access, changed by admins only, every change audited.
	// Battery data from firmware client 1.10.1 (fleet manager and up; checked in the handler).
	mux.HandleFunc("GET /battery", h.requireAuth(h.BatteryPage))
	mux.HandleFunc("GET /battery/export.csv", h.requireAuth(h.BatteryExport))
	mux.HandleFunc("GET /compliance", h.requireAuth(h.ComplianceIndex))
	mux.HandleFunc("GET /compliance/rules", h.requireAuth(h.ComplianceRulesPage))
	post("POST /compliance/rules", h.requireStrictAdmin(h.ComplianceBaselineSave))
	post("POST /compliance/rules/preview", h.requireStrictAdmin(h.ComplianceBaselinePreview))
	mux.HandleFunc("GET /compliance/overrides/new", h.requireStrictAdmin(h.ComplianceOverrideForm))
	mux.HandleFunc("GET /compliance/overrides/{id}/edit", h.requireStrictAdmin(h.ComplianceOverrideForm))
	post("POST /compliance/overrides", h.requireStrictAdmin(h.ComplianceOverrideSave))
	post("POST /compliance/overrides/{id}/delete", h.requireStrictAdmin(h.ComplianceOverrideDelete))
	post("POST /compliance/overrides/{id}/move", h.requireStrictAdmin(h.ComplianceOverrideMove))
	mux.HandleFunc("GET /kiosk", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/lock", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("GET /manage", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/kiosk", http.StatusMovedPermanently)
	}))
	// Enrollment is admin-only: the page, its QR codes and every profile mutation.
	mux.HandleFunc("GET /enrollment", h.requireStrictAdmin(h.EnrollmentPage))
	mux.HandleFunc("GET /enrollment/profiles/{id}/qr.png", h.requireStrictAdmin(h.EnrollmentProfileQR))
	mux.HandleFunc("GET /enrollment/android-app/qr.png", h.requireAuth(h.EnrollAndroidQR))
	post("POST /enrollment/scan", h.requireOperatorOrAdmin(h.ScoutScan))
	post("POST /enrollment/sightings/{id}/enroll", h.requireOperatorOrAdmin(h.ScoutEnroll))
	post("POST /enrollment/sightings/{id}/recheck", h.requireOperatorOrAdmin(h.ScoutRecheck))
	post("POST /enrollment/profiles", h.requireStrictAdmin(h.EnrollmentProfileCreate))
	post("POST /enrollment/profiles/{id}/revoke", h.requireStrictAdmin(h.EnrollmentProfileRevoke))
	post("POST /enrollment/profiles/{id}/activate", h.requireStrictAdmin(h.EnrollmentProfileActivate))
	post("POST /enrollment/profiles/{id}/delete", h.requireStrictAdmin(h.EnrollmentProfileDelete))
	post("POST /enrollment/profiles/{id}/update", h.requireStrictAdmin(h.EnrollmentProfileUpdate))
	post("POST /enrollment/profiles/bulk", h.requireStrictAdmin(h.EnrollmentProfilesBulk))
	post("POST /enrollment/profiles/delete-expired", h.requireStrictAdmin(h.EnrollmentProfilesDeleteExpired))
	// Device lifecycle: onboarding inbox confirmation, class override, retire/unretire.
	post("POST /devices/{serial}/onboard", h.requireOperatorOrAdmin(h.deviceRoute("notes", h.DeviceOnboard)))
	post("POST /devices/{serial}/retire", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceRetire)))
	post("POST /devices/{serial}/unretire", h.requireStrictAdmin(h.deviceRoute("view", h.DeviceUnretire)))
	mux.HandleFunc("GET /manage/policies/new", h.requireAuth(h.ManagePolicyNew))
	mux.HandleFunc("GET /kiosk/policies/new", h.requireAuth(h.ManagePolicyNew))
	mux.HandleFunc("GET /lock/policies/new", h.requireAuth(h.ManagePolicyNew))
	mux.HandleFunc("GET /manage/policies/preview", h.requireAuth(h.ManagePolicyPreview))
	mux.HandleFunc("GET /kiosk/policies/preview", h.requireAuth(h.ManagePolicyPreview))
	mux.HandleFunc("GET /lock/policies/preview", h.requireAuth(h.ManagePolicyPreview))
	mux.HandleFunc("GET /manage/policies/{id}/edit", h.requireAuth(h.ManagePolicyEditPage))
	mux.HandleFunc("GET /kiosk/policies/{id}/edit", h.requireAuth(h.ManagePolicyEditPage))
	mux.HandleFunc("GET /lock/policies/{id}/edit", h.requireAuth(h.ManagePolicyEditPage))
	post("POST /manage/policies", h.requireAuth(h.ManagePolicySave))
	post("POST /kiosk/policies", h.requireAuth(h.ManagePolicySave))
	post("POST /lock/policies", h.requireAuth(h.ManagePolicySave))
	post("POST /manage/policies/{id}/delete", h.requireAuth(h.ManagePolicyDelete))
	post("POST /kiosk/policies/{id}/delete", h.requireAuth(h.ManagePolicyDelete))
	post("POST /lock/policies/{id}/delete", h.requireAuth(h.ManagePolicyDelete))
	mux.HandleFunc("GET /commands/browse-devices", h.requireAuth(h.CommandBrowseDevices))
	mux.HandleFunc("GET /commands/resolve-serials", h.requireAuth(h.CommandResolveSerials))
	// History is a view of Jobs, so it lives under it: a child rail must only ever point
	// at pages beneath the page it belongs to, or the rail reads as a second nav.
	mux.HandleFunc("GET /jobs/history", h.requireAuth(h.CommandHistory))
	mux.HandleFunc("GET /commands/history", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.RawQuery
		if q != "" {
			q = "?" + q
		}
		http.Redirect(w, r, "/jobs/history"+q, http.StatusMovedPermanently)
	}))
	// Static route wins over /commands/{id}, so this is the global live feed the
	// history page subscribes to (not a per-command stream).
	mux.HandleFunc("GET /commands/events", h.requireAuth(drainable(h.CommandsFeedEvents)))
	post("POST /commands/clear-attention", h.requireOperatorOrAdmin(h.AttentionClear))
	mux.HandleFunc("GET /commands/impact", h.requireAuth(h.CommandImpact))
	mux.HandleFunc("GET /commands/target-packages", h.requireAuth(h.CommandTargetPackages))
	// Recipes live under /recipes (not /commands/recipes) so the {id} delete route
	// doesn't collide with /commands/{id}/resend/{serial} in the wildcard mux.
	post("POST /recipes", h.requireAuth(h.RecipeCreate))
	post("POST /recipes/{id}/delete", h.requireAdmin(h.RecipeDelete))
	// Schedules were removed on 23 Oct 2026 (user: "remove it"): nothing in the UI could
	// create a recipe to schedule, and live had none. Old links land on Jobs.
	mux.Handle("GET /schedules", http.RedirectHandler("/jobs", http.StatusMovedPermanently))
	post("POST /commands", h.requireAuth(h.CommandCreate))
	mux.HandleFunc("GET /commands/{id}", h.requireAuth(h.CommandDetail))
	mux.HandleFunc("GET /commands/{id}/screenshot/{serial}", h.requireAuth(h.CommandScreenshot))
	mux.HandleFunc("GET /commands/{id}/logs.zip", h.requireAuth(h.CommandLogsZip))
	mux.HandleFunc("GET /commands/{id}/logs/{serial}", h.requireAuth(h.CommandLogs))
	mux.HandleFunc("GET /commands/{id}/status", h.requireAuth(h.CommandStatusPartial))
	mux.HandleFunc("GET /commands/{id}/events", h.requireAuth(drainable(h.CommandEvents)))
	post("POST /commands/{id}/delete", h.requireOperatorOrAdmin(h.CommandDelete))
	post("POST /commands/{id}/resend", h.requireAuth(h.CommandResendAll))
	post("POST /commands/{id}/resend/{serial}", h.requireAuth(h.CommandResendDevice))

	// Boot logo is retired from the UI (11 Oct 2026): no nav entry, no link from
	// Settings. The route, its handlers, the update_splash command and the client
	// support all stay, so a splash can still be pushed through the API and the page
	// works for anyone who has the URL — it is simply not offered any more. Do not
	// re-add it to the directory without asking.
	mux.HandleFunc("GET /boot-logo", h.requireStrictAdmin(h.BootLogo))
	mux.HandleFunc("GET /settings", h.requireStrictAdmin(h.SettingsPage))
	mux.HandleFunc("GET /products", h.requireAuth(h.ProductsPage))
	mux.HandleFunc("GET /settings/google-usage", h.requireStrictAdmin(h.GoogleUsageJSON))
	post("POST /settings/columns/add", h.requireStrictAdmin(h.SettingsAddColumn))
	post("POST /settings/columns/{key}/remove", h.requireStrictAdmin(h.SettingsRemoveColumn))
	post("POST /settings/legacy-builds/add", h.requireStrictAdmin(h.SettingsAddLegacyBuild))
	post("POST /settings/legacy-builds/remove", h.requireStrictAdmin(h.SettingsRemoveLegacyBuild))
	post("POST /settings/legacy-checkin/toggle", h.requireStrictAdmin(h.SettingsToggleLegacyCheckin))
	post("POST /settings/command-canary/toggle", h.requireStrictAdmin(h.SettingsToggleCommandCanary))
	post("POST /settings/ignore-dpc/toggle", h.requireStrictAdmin(h.SettingsToggleIgnoreDPC))
	post("POST /settings/shell/toggle", h.requireStrictAdmin(h.SettingsToggleShell))
	post("POST /settings/remote/toggle", h.requireStrictAdmin(h.SettingsToggleRemote))
	post("POST /settings/command-expiry", h.requireStrictAdmin(h.SettingsSetCommandExpiry))
	post("POST /settings/max-targets", h.requireStrictAdmin(h.SettingsSetMaxTargets))
	mux.HandleFunc("GET /changelog", h.requireAuth(h.Changelog))
	post("POST /settings/require-reason", h.requireStrictAdmin(h.SettingsToggleRequireReason))
	post("POST /settings/compliance-reports/toggle", h.requireStrictAdmin(h.SettingsToggleComplianceReports))
	post("POST /settings/lost-mode-phone", h.requireStrictAdmin(h.SettingsSetLostModePhone))
	post("POST /settings/agent-apk", h.requireStrictAdmin(h.SettingsAgentAPK))
	post("POST /products/{key}/name", h.requireStrictAdmin(h.ProductRename))
	post("POST /products/{key}/role", h.requireStrictAdmin(h.ProductSetRole))
	// Roles & hardware (products.go): what each role runs, and each model's firmware mode.
	// Super admins only — Releases shows both read-only.
	post("POST /products/attach", h.requireStrictAdmin(h.ProductAttach))
	post("POST /products/firmware-mode", h.requireStrictAdmin(h.PipelineFirmwareMode))
	post("POST /products/role-app", h.requireStrictAdmin(h.PipelineRoleApp))
	post("POST /products/role-app-auto", h.requireStrictAdmin(h.LineupAppAuto))
	post("POST /products/role-app-retry", h.requireStrictAdmin(h.LineupAppRetry))
	post("POST /settings/agent-apk/upload", h.requireStrictAdmin(h.SettingsAgentAPKUpload))
	// Clients: what each device's MDM client is, and the builds hosted for it.
	// MDM clients and Host apps: pages of their own since 23 Oct 2026 (were Lineup tabs).
	mux.HandleFunc("GET /clients", h.requireAuth(h.clientsEntry))
	// Host apps is the Apps hub's "By product" tab (P1, 23 Oct 2026).
	mux.HandleFunc("GET /apps/products", h.requireAuth(h.Pipeline))
	mux.HandleFunc("GET /host-apps", func(w http.ResponseWriter, r *http.Request) {
		to := "/apps/products"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusMovedPermanently)
	})
	// Prod coverage (C1, 23 Oct 2026): how much of the AIO backend is in the MDM.
	mux.HandleFunc("GET /coverage", h.requireAuth(h.fleetWide(h.ProdCoveragePage)))
	// The server's own vitals. Operator-and-up: it exposes no device data, but it does
	// say how hard the box is working, which is not a customer-facing fact.
	mux.HandleFunc("GET /server", h.requireAdminOrOperator(h.ServerPage))
	mux.HandleFunc("GET /events/server", h.requireAdminOrOperator(drainable(h.ServerEvents)))
	post("POST /settings/agent-apk/remove", h.requireStrictAdmin(h.SettingsAgentAPKRemove))
	post("POST /settings/fleet-adb-key", h.requireStrictAdmin(h.SettingsFleetAdbKeyUpload))
	post("POST /settings/fleet-adb-key/remove", h.requireStrictAdmin(h.SettingsFleetAdbKeyRemove))
	// Public on purpose: a factory-reset phone downloads the agent from the QR.
	// One route per hosted agent slot (DPC, and the firmware client per tree — the
	// platform key is the tree's, so one build serves its user and userdebug devices
	// alike). Unauthenticated on purpose — see AgentAPKDownload.
	for _, name := range AgentAPKSlots {
		mux.HandleFunc("GET /agent/"+name, h.AgentAPKDownload)
	}
	// Archived builds, by digest. Authenticated, unlike the slot file above: only the
	// current build has to be reachable without a session.
	mux.HandleFunc("GET /agent/history/{slot}/{file}", h.requireAuth(h.AgentAPKHistoryDownload))
	post("POST /clients/rollback", h.requireStrictAdmin(h.AgentAPKRollback))
	post("POST /settings/maintenance", h.requireStrictAdmin(h.SettingsToggleMaintenance))
	post("POST /settings/legacy-strip-done", h.requireStrictAdmin(h.SettingsLegacyStripDone))
	mux.HandleFunc("GET /maintenance", h.MaintenancePage)
	post("POST /settings/dashboard", h.requireStrictAdmin(h.SettingsSetDashboard))
	post("POST /profile/display", h.requireAuth(h.ProfileDisplaySave))
	post("POST /settings/kiosk-allowlist", h.requireStrictAdmin(h.SettingsSetKioskAllowlist))
	post("POST /settings/ota-support", h.requireStrictAdmin(h.SettingsSetOTASupport))
	post("POST /settings/mdm-servers", h.requireStrictAdmin(h.SettingsSetMDMServers))
	post("POST /settings/ota-min-release", h.requireStrictAdmin(h.SettingsSetOTAMinRelease))
	post("POST /settings/legacy-ota", h.requireStrictAdmin(h.SettingsSetLegacyOTA))
	post("POST /settings/ota-config", h.requireStrictAdmin(h.SettingsSetOTAConfig))
	post("POST /settings/alert-webhook", h.requireStrictAdmin(h.SettingsSetAlertWebhook))
	post("POST /settings/daily-digest/toggle", h.requireStrictAdmin(h.SettingsToggleDailyDigest))
	post("POST /settings/alert-rules/{id}", h.requireStrictAdmin(h.SettingsUpdateAlertRule))
	post("POST /settings/service-window", h.requireStrictAdmin(h.SettingsSetServiceWindow))
	post("POST /settings/alert-channels", h.requireStrictAdmin(h.SettingsSaveChannel))
	post("POST /settings/alert-channels/{id}/test", h.requireStrictAdmin(h.SettingsTestChannel))
	post("POST /settings/alert-channels/{id}/delete", h.requireStrictAdmin(h.SettingsDeleteChannel))
	post("POST /settings/retention", h.requireStrictAdmin(h.SettingsSetRetention))
	post("POST /settings/session-timeout", h.requireStrictAdmin(h.SettingsSetSessionTimeout))
	post("POST /settings/logout-all", h.requireStrictAdmin(h.SettingsLogoutAll))
	post("POST /settings/checkin-interval", h.requireStrictAdmin(h.SettingsSetCheckinInterval))
	post("POST /settings/wlc-products", h.requireStrictAdmin(h.SettingsSetWlcProducts))

	// The old app repository page; the App library (/apps) is the one library since
	// 23 Oct 2026 (it also lived in Settings and here).
	mux.Handle("GET /setup", http.RedirectHandler("/apps", http.StatusMovedPermanently))
	// Managed app configurations edit the fleet device_policy, so mutations are
	// strict-admin like the other policy pages; the page itself is admin/dev.
	post("POST /setup/apps", h.requireAdmin(h.SetupCreateApp))
	post("POST /setup/apps/create", h.requireAdmin(h.SetupCreateAppJSON))
	// S3 APK uploads: presigned direct-to-S3 upload + register + device download proxy.
	mux.HandleFunc("GET /apps", h.requireAppLibrary(h.AppLibraryPage))
	// Resource use per model (R1): fleet-wide, so viewers with a device limit go to Fleet.
	mux.HandleFunc("GET /apps/usage", h.requireAuth(h.fleetWide(h.ModelUsagePage)))
	// The store's "Install on…" chooser: fleet-resolved scopes with real counts. Read-only;
	// the chosen scope posts to POST /commands like every other send.
	mux.HandleFunc("GET /apps/install-targets", h.requireAppLibrary(h.AppInstallTargets))
	// One app: every version in service and every device that has it. The page a store is
	// actually about — see app_detail.go.
	mux.HandleFunc("GET /apps/{pkg}", h.requireAppLibrary(h.AppDetailPage))
	post("POST /apps/upload-url", h.requireAppLibrary(h.AppUploadURL))
	post("POST /apps/register", h.requireAppLibrary(h.AppRegister))
	post("POST /apps/families", h.requireAppLibrary(h.AppFamilyCreate))
	post("POST /apps/families/{id}/rename", h.requireAppLibrary(h.AppFamilyRename))
	post("POST /apps/families/{id}/ungroup", h.requireAppLibrary(h.AppFamilyUngroup))
	post("POST /apps/package/assign", h.requireAppLibrary(h.AppPackageAssign))
	post("POST /settings/app-family-mode", h.requireStrictAdmin(h.SettingsSetAppFamilyMode))
	mux.HandleFunc("GET /apps/{id}/apk", h.AppAPK) // open: device installer fetches by URL
	post("POST /setup/apps/{id}/edit", h.requireAdmin(h.SetupUpdateApp))
	post("POST /setup/apps/{id}/delete", h.requireAdmin(h.SetupDeleteApp)) // removing an APK stays admin-only

	// Releases, rollouts and the summary of both are one page since 16 Oct (O2), at
	// /releases; the old list addresses land there, keeping their query.
	mux.HandleFunc("GET /releases", h.requireAdminOrOperator(h.ReleasesPage))
	mux.HandleFunc("GET /releases/settings", h.requireStrictAdmin(h.ReleasesSettingsPage))
	mux.HandleFunc("GET /releases/events", h.requireAdminOrOperator(drainable(h.ReleaseProblemEvents)))
	post("POST /releases", h.requireReleaseAdmin(h.ReleaseCreate))
	mux.HandleFunc("GET /releases/{id}", h.requireAdminOrOperator(h.ReleaseDetail))
	post("POST /releases/{id}/packages", h.requireReleaseAdmin(h.ReleaseAddPackage))
	post("POST /releases/{id}/packages/inspect", h.requireReleaseAdmin(h.PackageInspect))
	mux.HandleFunc("GET /releases/{id}/crashes", h.requireAdminOrOperator(h.fleetWide(h.ReleaseCrashes)))
	post("POST /releases/{id}/crashes/group/delete", h.requireReleaseAdmin(h.ReleaseCrashGroupDelete))
	post("POST /releases/{id}/crashes/{eid}/delete", h.requireReleaseAdmin(h.ReleaseCrashDelete))
	post("POST /releases/{id}/packages/{pid}/delete", h.requireReleaseAdmin(h.PackageDelete))
	post("POST /releases/{id}/qfil", h.requireReleaseAdmin(h.ReleaseAddQFIL))
	post("POST /releases/{id}/dev", h.requireReleaseAdmin(h.ReleaseSetDev))
	post("POST /releases/{id}/qfil/{qid}/delete", h.requireReleaseAdmin(h.ReleaseDeleteQFIL))
	post("POST /releases/track", h.requireReleaseAdmin(h.ReleaseTrack))
	post("POST /releases/order", h.requireReleaseAdmin(h.ReorderVersions))
	post("POST /releases/version/hide", h.requireReleaseAdmin(h.VersionHide))
	post("POST /releases/version/unhide", h.requireReleaseAdmin(h.VersionUnhide))
	post("POST /releases/{id}/meta", h.requireReleaseAdmin(h.ReleaseEditMeta))
	post("POST /releases/{id}/rename", h.requireReleaseAdmin(h.ReleaseRename))
	post("POST /releases/{id}/hide", h.requireReleaseAdmin(h.ReleaseSetHidden))
	post("POST /releases/{id}/delete", h.requireReleaseAdmin(h.ReleaseDelete))
	post("POST /releases/{id}/publish", h.requireReleaseAdmin(h.ReleasePublish))
	// Firmware is one job — what exists, what is going out, where it got to — and it was
	// three pages that each rendered a slice of the others: /updates drew a "Releases"
	// section, /releases drew an "Updates in progress" section. They are now one
	// destination with three tabs, so there is a single answer to "what is rolling out".
	mux.HandleFunc("GET /rollouts", h.requireAdminOrOperator(releasesRedirect))
	mux.HandleFunc("GET /updates", h.requireAdminOrOperator(releasesRedirect))
	mux.HandleFunc("GET /updates/rollouts", h.requireAdminOrOperator(releasesRedirect))
	mux.HandleFunc("GET /updates/new", h.requireOTA(h.NewUpdatePage))
	mux.HandleFunc("GET /updates/legacy", h.requireAdminOrOperator(h.fleetWide(h.LegacyOTAPage)))
	mux.HandleFunc("GET /updates/legacy/deployments/{id}", h.requireAdminOrOperator(h.fleetWide(h.LegacyDeploymentPage)))
	post("POST /updates/legacy/push", h.requireOTA(h.LegacyOTAPush))
	post("POST /updates/legacy/deployments/{id}/cancel", h.requireOTA(h.LegacyOTACancel))
	post("POST /updates/legacy/deployments/{id}/devices/{serial}/retry", h.requireOTA(h.LegacyOTARetry))
	post("POST /updates/legacy/deployments/{id}/devices/{serial}/reboot", h.requireOTA(h.LegacyOTAReboot))
	post("POST /updates", h.requireOTA(h.DeployCreate))
	post("POST /releases/{id}/deploy", h.requireOTA(h.ReleaseDeploy))
	post("POST /releases/{id}/branch", h.requireReleaseAdmin(h.ReleaseCreateBranch))
	post("POST /releases/{id}/merge", h.requireReleaseAdmin(h.ReleaseMerge))
	// Problem reports: any operator/operator can file and triage; admins can delete.
	post("POST /releases/{id}/problems", h.requireOperatorOrAdmin(h.ReleaseProblemCreate))
	post("POST /releases/{id}/problems/{pid}", h.requireOperatorOrAdmin(h.ReleaseProblemUpdate))
	post("POST /releases/{id}/problems/{pid}/delete", h.requireReleaseAdmin(h.ReleaseProblemDelete))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}", h.requireAdminOrOperator(h.DeploymentDetail))
	mux.HandleFunc("GET /releases/{id}/deployments/{did}/events", h.requireAdminOrOperator(drainable(h.DeploymentEvents)))
	post("POST /releases/{id}/deployments/{did}/settings", h.requireOTA(h.DeploymentUpdateSettings))
	post("POST /releases/{id}/deployments/{did}/cancel", h.requireOperatorOrAdmin(h.DeploymentCancel))
	post("POST /releases/{id}/deployments/{did}/add-targets", h.requireOTA(h.DeploymentAddTargets))
	post("POST /releases/{id}/deployments/{did}/reboot-all", h.requireOperatorOrAdmin(h.DeploymentRebootAll))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/reboot", h.requireOperatorOrAdmin(h.DeploymentRebootDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/retry", h.requireOTA(h.DeploymentRetryDevice))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/cancel-ota", h.requireOTA(h.DeploymentCancelDeviceOTA))
	post("POST /releases/{id}/deployments/{did}/devices/{serial}/remove", h.requireOTA(h.DeploymentRemoveDevice))
	post("POST /releases/{id}/deployments/{did}/delete", h.requireOTA(h.DeploymentDelete))

	mux.HandleFunc("GET /activity", h.requireUserManager(h.fleetWide(h.ActivityPage)))
	mux.HandleFunc("GET /users", h.requireUserManager(h.UserList))
	post("POST /users", h.requireAccountAdmin(h.UserCreate))
	post("POST /users/{id}/role", h.requireUserManager(h.UserSetRole))
	post("POST /users/{id}/name", h.requireUserManager(h.UserSetName))
	mux.HandleFunc("GET /users/{id}/avatar.png", h.requireAuth(h.UserAvatar))
	post("POST /users/{id}/avatar", h.requireAuth(h.UserSetAvatar))
	post("POST /users/{id}/avatar/delete", h.requireAuth(h.UserClearAvatar))
	post("POST /users/{id}/password", h.requireUserManager(h.UserSetPassword))
	post("POST /users/{id}/invite", h.requireUserManager(h.UserResendInvite))
	// An owner's own staff (venue_people.go): each handler checks the owner and their venues.
	post("POST /team/invite", h.requireAuth(h.TeamInvite))
	post("POST /team/{id}/invite", h.requireAuth(h.TeamResendInvite))
	post("POST /team/{id}/remove", h.requireAuth(h.TeamRemove))
	post("POST /users/{id}/delete", h.requireAccountAdmin(h.UserDelete))
	post("POST /users/{id}/verify-email", h.requireStrictAdmin(h.UserVerifyEmail))
	post("POST /users/merge", h.requireAccountAdmin(h.UserMergeActor))
	mux.HandleFunc("GET /profile", h.requireAuth(h.ProfilePage))
	mux.HandleFunc("GET /users/{id}/profile", h.requireAuth(h.UserProfilePage))
	mux.HandleFunc("GET /users/access", h.requireUserManager(h.UsersAccessPage))
	mux.HandleFunc("GET /users/access/scope-search", h.requireUserManager(h.fleetWide(h.AccessScopeSearch)))
	mux.HandleFunc("GET /users/access/who", h.requireUserManager(h.fleetWide(h.AccessWhoCanSee)))
	mux.HandleFunc("GET /users/{id}/access", h.requireUserManager(h.UserAccessPage))
	mux.HandleFunc("GET /users/{id}/manage", h.requireUserManager(h.UserAccessPage))
	mux.HandleFunc("GET /users/{id}/access/check", h.requireUserManager(h.UserAccessCheck))
	post("POST /users/{id}/access/base", h.requireUserManager(h.UserAccessSetBase))
	post("POST /users/{id}/access/grants", h.requireUserManager(h.UserAccessAddGrant))
	post("POST /users/{id}/access/grants/{gid}/edit", h.requireUserManager(h.UserAccessEditGrant))
	post("POST /users/{id}/access/grants/{gid}/delete", h.requireUserManager(h.UserAccessDeleteGrant))
	post("POST /users/{id}/access/simple", h.requireUserManager(h.UserAccessSimpleSave))
	post("POST /users/{id}/access/simple/preview", h.requireUserManager(h.UserAccessSimplePreview))
	mux.HandleFunc("GET /users/access/profiles", h.requireUserManager(h.AccessProfilesPage))
	post("POST /users/access/profiles", h.requireUserManager(h.AccessProfileSave))
	post("POST /users/access/profiles/{pid}", h.requireUserManager(h.AccessProfileSave))
	post("POST /users/access/profiles/{pid}/delete", h.requireUserManager(h.AccessProfileDelete))
	mux.HandleFunc("GET /icon/{sha}", h.IconPNG)

	// Command output SSE
	mux.HandleFunc("GET /commands/{id}/output/{serial}/stream", h.requireAuth(drainable(h.CommandOutputStream)))
}
