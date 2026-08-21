// Package version is the single source of truth for the MDM server version and its
// user-facing changelog. To cut a release, add a new Entry at the TOP of Changelog:
// the first entry's Version is the live server version shown in the dashboard.
//
// Versioning is SemVer-lite: MAJOR.MINOR.PATCH — bump MINOR for new features, PATCH
// for fixes, MAJOR only for big/breaking changes. Keep Changes brief and in plain
// language a non-technical user can follow.
package version

// Entry is one released version and its plain-language change list.
type Entry struct {
	Version string
	Date    string // YYYY-MM-DD
	Changes []string
	// Media is an optional gallery of screenshots/gifs illustrating the release,
	// shown above the change list on the changelog page and in the "What's new"
	// popup. Curated to the headline features — not one per change.
	Media []Media
}

// Media is one screenshot or gif in a release's gallery. Src is a path under
// /static (e.g. "/static/changelog/1.40/fleet-map.png"); a ".gif" Src animates
// on its own. Caption names the feature it shows. Wide makes it span the full
// gallery row (good for a hero shot or a wide map).
type Media struct {
	Src     string
	Alt     string
	Caption string
	Wide    bool
}

// Changelog is newest-first. The top entry is the current server version.
var Changelog = []Entry{
	{
		Version: "1.41.0",
		Date:    "2026-08-21",
		Changes: []string{
			"Apps install from a library, not links. Pick apps from a searchable library with icons and details instead of pasting download URLs.",
			"The app drawer on the device page has been redesigned. Install one or several apps at once and remove them right from the drawer, each with a live progress state and the option to cancel an install that's still running; apps already on the device are hidden from the install picker so you only see what you can add.",
			"Control wireless charging from the device page. A new toggle turns the charging pad on or off in place, with no reload. When charging is off the pad status reads “Not available” instead of a misleading reading, and the battery graph's legend now spells out each state — device placed, pad vacant, and charging disabled.",
			"Software updates go out instantly, with an “apply now” reboot button for manual deploys and a new “Reboot all installed” action to restart a whole group at once.",
			"A redesigned device page: switch graphs and timestamps between your time and the device's own time, open any graph full screen, and see command history by app name instead of a long download link. Kiosk lock now only offers apps actually installed on the device.",
			"Reliability fixes under the hood: reboots no longer look “done” before they've happened, app installs no longer stall or get re-sent to a device that's gone offline, and a transient hiccup while an app is uninstalling can no longer leave an error in the app drawer.",
			"Every device now has a Queue tab. Commands you send line up there and run one at a time, in order — a device works through its queue the moment it's online, and you can remove anything still waiting. A command sent to an offline device shows as “queued” (not “delivered”) until the device is back.",
			"Command delivery is now reliable end to end. A command that didn't actually reach the device is re-sent automatically, runs exactly once (no duplicate installs), and can no longer sit stuck showing “delivered” while nothing happens — so what the dashboard shows matches what the device did. On reconnect a device runs its queued commands right away instead of after a delay.",
			"App installs run one at a time per device instead of all at once, so a batch of installs applies cleanly and in order.",
		},
	},
	{
		Version: "1.40.0",
		Date:    "2026-08-13",
		Changes: []string{
			"The dashboard now supports more than one device type: the T7 tablet and the Kiosk 18/22/27 wall panels. Battery, charging, and wireless-pad widgets are hidden on the mains-powered kiosks, and a new Products section in the Fleet sidebar lets you filter to one hardware type.",
			"Devices now show their location. The device page shows a street address and a map, worked out from nearby Wi-Fi. Repeat lookups are cached to cut down on Google API calls, and admins get a Google API usage and cost panel in Settings.",
			"Kiosk mode is easier to manage: a redesigned pop-up with a visual app picker, an optional allowlist of approved apps, and clear Enable and Change-app buttons. A device's kiosk on/off state now updates live on its page.",
			"You leave kiosk mode with a gesture — long-press Back and Power together. A device taken out of kiosk on-site shows that on its page right away.",
			"Crashes are now easy to find. Each device page has an Alerts tab listing its crashes and alerts, and the Alerts page has a Crashes view you can filter by device and page through. The top-crashing-device links on Home and Fleet Health go straight there.",
			"Software updates have a dedicated push page with product-aware rollouts, delivered as a full image or a smaller incremental. App installs now ride out a network drop: the download keeps retrying and resumes where it left off when the connection returns, and if a device never recovers the install is failed cleanly instead of staying stuck on “installing”.",
			"The Releases list is ordered by release date and titled by build name, with reported builds behind an admin toggle and the release date editable in place.",
			"On the Fleet page, clicking a serial opens the full device page, and “Last seen” reflects a device's live connection instead of its last check-in.",
			"Manage a group, venue, or release straight from the Fleet page. Pick one in the left sidebar and a small toolbar appears above the roster: rename it in place, delete it, or open a venue's full details (address, map coordinates, timezone) in a slide-in panel — all without leaving the device list. The sidebar section headings (Products, Restaurants, Groups, Releases) are now clearly tappable pills.",
			"Remove several devices from a group at once, either from the group's own page or right on the Fleet page when a group is selected — tick the devices and choose “Remove from group.”",
			"The Home page now has a live map of every device that reports a location, coloured by whether it's online, and Fleet activity is drawn as a graph instead of bars. The maps follow your light/dark theme. The old Daily Report and Quick Actions blocks were removed to keep Home focused.",
			"Remote control is smoother and closer to real-time: the video now uses efficient H.264 streaming where the device supports it (falling back automatically where it doesn't), always shows the freshest frame instead of replaying a backlog, and a Smooth toggle lets you trade a touch of latency for extra fluidity. Closing the session returns you to the device page, and testers can now use remote control too.",
			"Device graphs are far easier to explore: drag across the plot to zoom to a span, Ctrl+scroll to zoom in and out, Ctrl+drag to pan, and double-click (or the Reset button) to return to the default view. The custom-range picker was redesigned with one-tap presets and a cleaner From/To, the time axis shows the date on multi-day views, the temperature and battery danger zones are shaded so a bad reading stands out, and a button saves the current chart as an image.",
			"New Queries: an admin defines a set of read-only property checks in Settings (for example Build ID, Model, Android version, Battery, Storage), and testers, devs and admins can run them from the Actions page like any other action — no shell access required.",
			"Access changes: the raw Shell console is now limited to admins and devs. The Operator role has been retired — existing operators become Viewers, and the everyday actions operators used to have (install, uninstall, reboot, screenshots) now belong to the Tester role.",
			"Smaller changes: rename a release from its Manage page; remove several devices from a group at once; the live logs console is now “Realtime logs”; the Actions shell follows your light/dark theme; screenshots open from a proper link; and a device coming back online now clears its “Last seen” and refreshes its build number live, without a manual refresh.",
		},
		// Media (screenshots/gifs) for this release are captured separately and dropped
		// into static/changelog/1.40/. Re-enable by populating this slice once the files
		// exist — the gallery renders on the changelog page and in the What's new popup:
		//	Media: []Media{
		//		{Src: "/static/changelog/1.40/fleet-map.png", Alt: "Home page live map of devices", Caption: "Live fleet map on the Home page", Wide: true},
		//		{Src: "/static/changelog/1.40/kiosk-picker.gif", Alt: "Kiosk mode visual app picker", Caption: "Kiosk mode: visual app picker"},
		//		{Src: "/static/changelog/1.40/device-location.png", Alt: "Device page street address and map", Caption: "Device location from nearby Wi-Fi"},
		//		{Src: "/static/changelog/1.40/graph-zoom.gif", Alt: "Drag across a device graph to zoom", Caption: "Drag-to-zoom device graphs"},
		//		{Src: "/static/changelog/1.40/crashes.png", Alt: "Alerts page Crashes view", Caption: "Crashes view on the Alerts page"},
		//	},
	},
	{
		Version: "1.31.0",
		Date:    "2026-07-27",
		Changes: []string{
			"See wireless charging live. A device on a Qi pad now shows its charging status the moment it changes — a status chip on the device's vitals, and the battery graph shades exactly when the pad was powering it. The graph also tells apart a pad that's switched off, a pad the device was lifted off of, and a pad it simply couldn't read — instead of lumping them all together as “on.” The same detail carries through to the battery CSV export (an unreadable pad now exports as “pad_unreadable”).",
			"Offline gaps in the graph now draw correctly. When a device goes quiet, the battery line breaks with a visible empty stretch — and it now appears right away while the device is still dark, not only after it comes back.",
			"Battery graphs now load reliably. A graph that could previously come up blank or stuck now draws every time you open a device, showing a loading spinner while it fills in.",
		},
	},
	{
		Version: "1.30.0",
		Date:    "2026-07-09",
		Changes: []string{
			"Spot a failing charger at a glance. When a device's charging keeps flicking on and off — more than ten times a minute, a classic sign of a faulty charger, dock, or cable — a broken-plug symbol now appears on its battery on the Fleet cards and its device page, and a new “Faulty charger” alert flags it. A chatty faulty unit no longer makes the page churn, either: its rapid check-ins are smoothed out so the dashboard stays calm.",
			"Releases now work like a git history: one main line with branches hanging off it, drawn as a commit-style timeline, newest-first. Merge a tested branch back into the main line — carrying its build, QA results, and any open problems — delete releases that never reached a device, and edit each release's download link right in the list.",
			"The Alerts page is now a plain-language inbox — every alert reads as a friendly headline and sentence with the key numbers, mirrored in the top-bar bell and in pop-up toasts when something new fires. It's quieter too: one offline alert per device (no runaway repeats) and resolved alerts are tidied away automatically.",
			"A crash or freeze alert now shows the actual crash trace right on the alert, so you can see what happened without pulling logs. And the three storage alerts have each been simplified to a single setting.",
			"Redesigned home Overview as a live command center, and Fleet Health is now crash-aware — surfacing which builds and units are crashing alongside battery and heat.",
			"Devices that go quiet for a while are now set aside as “inactive” automatically, and come back the moment they check in again — so long-dead units don't clutter your lists or skew the fleet stats.",
			"Smaller touches: a streamlined Shell action, consoles that follow your light/dark theme, admins can reset any user's password, and you can paste a whole list of serials straight into a command's targets.",
		},
	},
	{
		Version: "1.21.0",
		Date:    "2026-07-06",
		Changes: []string{
			"Fleet Wrapped — a playful, full-screen year-in-review of the whole fleet. Open it from your account menu (or share the public /wrapped link with anyone): scroll through the fleet's total check-ins and uptime, the battery cycles it's clocked, its hardest-working unit, busiest restaurant, hottest moment, and a summary card at the end.",
			"Testers and developers now have an Alert config page in the account menu: a read-only view of every alert rule, its thresholds, and the fleet's peak hours — so they can see exactly what's being watched without needing admin access to Settings.",
		},
	},
	{
		Version: "1.20.0",
		Date:    "2026-07-05",
		Changes: []string{
			"AI-assisted log capture (beta): on the Actions page, just describe the problem in plain words and the assistant picks the log level, how many lines to pull, and the tag to focus on — you can tweak its suggestion before sending. Shows up when an AI key is configured.",
			"Reworked how you choose which devices an action hits: start from everything, a restaurant, a group, or a release, then refine down to a hand-picked set — with a search so you can find and add specific devices before you send.",
			"Schedule a saved recipe to run on its own: pick a recipe and a cron schedule (one-off or repeating), and its target is re-resolved each time it fires. Manage everything from the new Scheduled recipes page.",
			"App classification: admins can now force an app to be treated as a system app, overriding what devices report — handy for hiding an app from the Uninstall and Kiosk pickers.",
			"When several devices are selected on the Fleet page, the toolbar now offers a single Action button that takes them straight to the composer; Kiosk mode is also available directly from the Actions page.",
			"Fixes: an offline alert no longer re-fires while a device is still down — once you've resolved it, it stays quiet until the device comes back online and drops again; the device graphs now show a loading spinner while they draw.",
		},
	},
	{
		Version: "1.19.0",
		Date:    "2026-07-05",
		Changes: []string{
			"Battery wear at a glance: every device now tracks its lifetime battery cycles. You'll see it on the device page and on the fleet cards, and you can sort the fleet by it to find the most-worn units.",
			"A big batch of new health alerts. Overheating now knows when a device is sitting on its wireless charger and allows a higher temperature before flagging it. New alerts cover: low battery during peak hours, storage running low, being stuck on the wireless charger for over an hour, staying offline for more than an hour, sitting fully charged overnight, a charging pad that hasn't worked all day, repeated Wi-Fi drop-outs, app crashes, and slow overnight charging on a weak charger.",
			"Peak hours are now set per restaurant — add each venue's real busy periods (for example lunch and dinner) so the 'during peak' alerts fire when it actually matters for that location.",
			"Retire devices you've taken out of service. A retired device disappears from every list and search and won't come back on its next check-in, but all its data is kept. Retiring is admin-only, with a dedicated Retired view to reactivate one later.",
			"Fixes: the ← Fleet back arrow no longer bounces back and forth between two device pages; the CSV download button no longer gets stuck showing “Preparing CSV…”; and resetting a custom time range on the device graphs now clears it properly.",
		},
	},
	{
		Version: "1.18.0",
		Date:    "2026-07-04",
		Changes: []string{
			"You now stay signed in for a full day — sessions no longer time out after 30 minutes of sitting idle.",
			"Reworked how you build a command on the Actions page: every action (Install, Uninstall, Screenshot, Log capture, Shell, Reboot, Boot logo) is now a button you can see at a glance, and picking one opens a panel built for that job — choose an app from a visual library to install, browse only the apps you can actually remove to uninstall, set a log level and how many lines to capture, type straight into a shell, or drop in a boot-logo image and preview it before you send.",
			"Refined the Fleet Health page: a clear overall health score, at-a-glance vital tiles (online, offline, low battery, running hot, open alerts), your restaurants ranked worst-first, and a “what's dragging the score” breakdown.",
			"App download links are now visible only to admins.",
			"Fixed a couple of visual glitches where the top of a recipe button or a health row could look cut off when hovered or selected.",
		},
	},
	{
		Version: "1.17.0",
		Date:    "2026-07-03",
		Changes: []string{
			"Every page now shows an instant preview the moment you click — a shimmering outline appears right away and fills in with real data a beat later, so the dashboard feels immediate. The Fleet page previews its sidebar and device list too.",
			"The bottom navigation highlights the page you're heading to instantly, instead of only after it finishes loading.",
			"Fresh look for the Daily Report: it now reads like a short briefing from your fleet assistant — a plain-language headline, the key numbers as pills, and each thing worth a look paired with what to do about it. Device and restaurant names in the report are clickable, and you can watch it think and write itself when you press Refresh.",
			"New Health button in the bottom bar, opening a redesigned Fleet Health page: an overall health score, your restaurants ranked worst-first, and the open alerts behind those scores right alongside.",
			"Fleet page: picking a restaurant or group now refreshes just the device list (with a brief skeleton) instead of reloading the whole page — and choosing a filter or changing pages no longer scrambles the layout.",
			"Search improvements: the device search on the Actions page now filters the same list you're looking at, and hidden devices no longer appear in any search (including the top-right search box).",
			"Tidied the home page by removing the quick-action tiles.",
		},
	},
	{
		Version: "1.16.0",
		Date:    "2026-07-02",
		Changes: []string{
			"The dashboard is much faster and no longer reloads the whole page as you click around — pages swap in place, so things like software-update progress update smoothly without a full reload.",
			"Major speed-up for the pages that used to feel slow (Fleet, Actions, and Releases), so they stay quick even with thousands of devices.",
			"Changes you make now appear automatically in your other open tabs — no refresh needed.",
			"Redesigned Actions page: preview exactly which devices a command will hit before you send it, watch what's in progress at a glance, save and reuse common commands as “recipes”, and browse a clearer history split into In progress, Needs attention, and Completed — with its own dedicated history page.",
			"Viewers can now open the Actions page in read-only mode — they can see what's possible and view saved recipes, with the controls clearly disabled.",
			"The uninstall picker now lists only your installed apps, hiding the built-in system packages you can't remove.",
			"Slower sections of a page now show a loading placeholder instead of a blank gap while they load.",
			"Quick actions on the home page are now shown only to operators and above, and the Releases menu is limited to admin, dev, and tester accounts.",
		},
	},
	{
		Version: "1.15.1",
		Date:    "2026-07-01",
		Changes: []string{
			"Device charts read like they used to: the battery line goes dashed while a device is draining, and offline periods show as gaps in the line instead of being smoothed over.",
		},
	},
	{
		Version: "1.15.0",
		Date:    "2026-06-29",
		Changes: []string{
			"Installing the same app twice no longer stacks duplicate “installing” rows — repeats now collapse into a single entry that shows how many times it was sent.",
			"Pressing Install again while that app is already installing no longer queues a duplicate copy on the device.",
		},
	},
	{
		Version: "1.14.0",
		Date:    "2026-06-26",
		Changes: []string{
			"Fresh new look across the dashboard, built around the AIO MDM brand — softer cards, clearer colours, and a search box in the top-right of every page.",
			"Redesigned home page: see your fleet at a glance with device counts, an overall health score, a 7-day activity chart, open alerts, and one-click quick actions.",
			"Redesigned Fleet page: a collections sidebar lists All devices plus every restaurant and group — click one to filter the list to it, or open it to see its devices and health. Restaurants and groups are now managed right inside Fleet.",
			"The device list is now easy-to-scan cards instead of a wide table, so it reads clearly and no longer gets cut off on narrower screens.",
			"New “Sort” control for the device list — order by last seen, battery, temperature, memory, serial, or onboarding date.",
			"You can now bring hidden devices back: hidden devices show an “unhide” button, and selecting one or more hidden devices reveals “Unhide Selected”.",
			"Viewers no longer see the Commands menu.",
		},
	},
	{
		Version: "1.13.0",
		Date:    "2026-06-23",
		Changes: []string{
			"The dashboard now updates live when devices are hidden, assigned to a restaurant or group, or sent a command — no need to refresh to see the change.",
			"Adding a single device to a group from the search box now works; previously the device you picked was silently dropped.",
			"The Alerts page refreshes on its own when anyone acknowledges or resolves an alert.",
			"Software updates: remove a device that's still “pending” from a rollout so it won't receive that update.",
			"Remote control is more secure — the access key is no longer put in the page address.",
			"New production runs now check the product code, so you can't create a run that silently matches no devices.",
			"Assigning several devices to a restaurant is now all-or-nothing, and rebooting a device respects the “reason required” setting.",
			"Devices last longer on battery: the on-device agent scans Wi-Fi far less and wakes up less often. (Device Wi-Fi location has been removed.)",
		},
	},
	{
		Version: "1.12.0",
		Date:    "2026-06-18",
		Changes: []string{
			"Send a command by filter: the command builder's device picker now has the same filters as the Devices list (status, group, production, release, battery) plus “Select all matching”, so you can target every device that matches.",
			"The device pickers for OTA deploy / add-targets and restaurant assignment gained matching status/battery filters too, so device selection works the same everywhere.",
			"New Logs page: recent logcat captures across the whole fleet, plus your most-used capture presets.",
			"Capture logs from the New Command page — pull logcat from all devices, a group, or specific serials at once, with quick chips for recent and frequent presets.",
			"Retry any past log capture in one click from a device's Logcat page.",
			"Every device serial across the dashboard is now clickable and jumps straight to that device.",
			"Device search boxes now look and behave the same everywhere (and the restaurant device-assignment search filters as you type again).",
			"New Dev account type: full access to releases, OTA, devices and commands, but not settings or user management.",
			"Releases now carry a developer sign-off — a dev marks a build “smoke-tested, OK for QA”, shown as a badge on the Releases list.",
			"Fixed OTA updates that could stay stuck on “pending” after a retry, and a deploy hiccup that could briefly show a 502.",
		},
	},
	{
		Version: "1.11.0",
		Date:    "2026-06-16",
		Changes: []string{
			"Releases page redesigned for a cleaner, more professional look.",
			"Standard test cases are now managed right on the Releases page — the separate Testing and Test cases tabs have been removed.",
			"Only the Tester account can now mark QA results; everyone else sees them read-only.",
			"A release can hold only one full image — the add option is disabled once one exists, so incrementals are added instead.",
			"Removed the per-release adoption chart; the list of devices on each version stays.",
			"Settings → Alerts: alert rules are now split into tabs by category for easier browsing.",
			"A release can now skip the standard test cases, so QA only checks that release's own cases.",
		},
	},
	{
		Version: "1.10.0",
		Date:    "2026-06-16",
		Changes: []string{
			"Test team: a new “Tester” account type and a per-release QA checklist.",
			"Admins keep a library of standard test cases (checked every release) and can add release-specific ones.",
			"Testers mark each case pass/fail/blocked/skip with notes; the release shows the QA summary and warns before publishing if not everything passed.",
			"Uninstall an app from a device directly from its installed-packages list.",
		},
	},
	{
		Version: "1.9.0",
		Date:    "2026-06-16",
		Changes: []string{
			"New device-health alerts: battery wear, app crashes and freezes, weak Wi-Fi, and someone leaving kiosk mode.",
			"Device-offline alerts are now on by default.",
			"Software-update screen: no more flickering, downloads in progress are no longer wrongly marked “stalled”, and it now shows how long each device took plus the deployment average.",
			"Logcat: quick chips to re-run your recent and most-used log requests.",
			"Added this “What's new” page.",
		},
	},
	{
		Version: "1.8.0",
		Date:    "2026-06-15",
		Changes: []string{
			"Restaurants: organise devices by venue, with a searchable picker and bulk assign.",
			"Per-venue service hours, so alerts only fire during opening times.",
			"Rebuilt the Send Command screen — searchable, with duplicate-and-edit and recent/frequent shortcuts.",
		},
	},
	{
		Version: "1.7.0",
		Date:    "2026-06-13",
		Changes: []string{
			"Software updates (OTA): roll out a new build to devices with live progress, retry, and cancel.",
			"Releases: manage build versions and see how many devices are on each.",
			"Renamed the product to AIO MDM.",
		},
	},
	{
		Version: "1.6.0",
		Date:    "2026-06-12",
		Changes: []string{
			"New home overview: fleet status at a glance with key vitals and quick actions.",
			"Refreshed look and feel across the dashboard, with light and dark themes.",
		},
	},
	{
		Version: "1.5.0",
		Date:    "2026-06-11",
		Changes: []string{
			"Security hardening: safer sign-in sessions, login rate-limiting, and stricter request checks.",
			"All assets are now served by the MDM itself (no third-party CDNs).",
		},
	},
	{
		Version: "1.4.0",
		Date:    "2026-06-10",
		Changes: []string{
			"AI fleet report: a plain-language hourly summary of how the fleet is doing.",
			"On-demand AI analysis for a single device, the whole fleet, or settings.",
		},
	},
	{
		Version: "1.3.0",
		Date:    "2026-06-09",
		Changes: []string{
			"Alerts: rules for overheating, charging problems and more, with a dedicated Alerts page.",
			"Get notified in Slack, Discord, or Microsoft Teams when something needs attention.",
			"Fleet Health page scoring each group worst-first.",
		},
	},
	{
		Version: "1.2.0",
		Date:    "2026-06-09",
		Changes: []string{
			"Trends: 30-day battery and charging charts for each device and group.",
		},
	},
	{
		Version: "1.1.0",
		Date:    "2026-06-08",
		Changes: []string{
			"Settings & controls: kill-switches for shell and remote control, command approval rules, and an audit log.",
			"Automatic clean-up of old data on a schedule.",
		},
	},
	{
		Version: "1.0.0",
		Date:    "2026-03-06",
		Changes: []string{
			"First release: monitor devices, view battery and status, and organise them into groups.",
			"Remote control basics — shell, screenshots, reboot, kiosk mode, and app/package inventory.",
			"Logcat and guest charging-pad status.",
		},
	},
}

// Current returns the live server version (the newest changelog entry).
func Current() string {
	if len(Changelog) == 0 {
		return "0.0.0"
	}
	return Changelog[0].Version
}
