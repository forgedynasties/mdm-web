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
		Version: "1.64.43",
		Date:    "2026-11-20",
		Changes: []string{
			"Installing an app on a device is called Install app everywhere (button, dialog, menus, group and venue menus), not Add app; the library's Add apps stays.",
			"The newest build now means the highest version, not the latest upload, so re-uploading an old build no longer becomes the one that installs.",
			"Device page Apps: the 24h/7d/30d pills are no longer cut off at the edge of the narrow left column.",
		},
	},
	{
		Version: "1.64.42",
		Date:    "2026-11-20",
		Changes: []string{
			"Install app on a device: picking an app that is already on the device no longer promises an install that never happens; the button is off and the footer says it updates itself, and a mixed pick counts only what is missing.",
			"The install form's clean-up after a failed request and the Lineup rename's reload ran through htmx's hx-on, which our Content-Security-Policy blocks (an EvalError on every Add app); they are plain listeners now.",
		},
	},
	{
		Version: "1.64.41",
		Date:    "2026-11-20",
		Changes: []string{
			"Menus and popups that ran past the bottom of the window can be reached again: the device page's Move to venue / Add to group popovers open upward when there is more room above and scroll inside when the list is long, and the Fleet map's device popup is panned fully into the map with its height capped, so Open device and Close are never cut off.",
		},
	},
	{
		Version: "1.64.40",
		Date:    "2026-11-20",
		Changes: []string{
			"Device page, Apps: the yellow \"On its way\" state pill (for example \"Waiting for the device · offline 2 d\") no longer squeezes the app's name to nothing and overlaps it in a narrow sheet; it sits under the name, the actions stay on the name's row.",
		},
	},
	{
		Version: "1.64.39",
		Date:    "2026-11-20",
		Changes: []string{
			"Moves to prod: a device whose last check-in is over 7.5 KB now waits instead of moving (prod's load balancer firewall refuses request bodies over 8 KB, so such a device was refused on prod for ten minutes and fell back to mdm.dev; a POS with a 53 KB crash report did exactly that). The move notes say why. MOVE_MAX_CHECKIN_BYTES overrides the limit (0 turns it off once the firewall rule is raised).",
		},
	},
	{
		Version: "1.64.38",
		Date:    "2026-11-19",
		Changes: []string{
			"Moves page (mdm.dev, super admin): /moves shows the progress of moving devices to prod, with a rail of move states and where each device is (To place or a venue, hidden venues included), a progress ring, what needs attention, a table with the client version against the hosted build, last seen and the watcher's note, and a timeline for the picked device. Read-only; refreshes every 30 seconds.",
		},
	},
	{
		Version: "1.64.37",
		Date:    "2026-11-19",
		Changes: []string{
			"Moving devices to prod: venues can be hidden (a hidden venue and its devices vanish from every list on mdm.dev). `POST /api/v1/admin/move/arm` (a dry run unless confirm=arm) files the missing moves, hides the venues and devices that are waiting, and skips AIO Lab; `unarm` undoes it. The move job now makes sure a device's MDM client is up to date before it moves (one app_update, then the move), and `move/status` shows each move's note, client version, last seen and what needs attention.",
		},
	},
	{
		Version: "1.64.36",
		Date:    "2026-11-19",
		Changes: []string{
			"Fleet: the browser's Back and Forward buttons now restore the whole page state (the rail highlight, the list and counts, the heading, the search box, the filter chips, sort, the product tab and the page), instead of changing only the address.",
			"Venue setting \"Alerts\": pause a venue's alerts for 24 h, 3 days, 7 days or until resumed (like the Lost devices pause). While paused it raises no new alerts, sends nothing to the alert channels, and its open alerts are held back from the Alerts page, the bell, the Overview and the counts; they return when the pause ends or is resumed. The venue page says how many are held back.",
		},
	},
	{
		Version: "1.64.35",
		Date:    "2026-11-19",
		Changes: []string{
			"Add app dialog on the device page: it no longer opens blank and clipped. A page-edge rounding rule (clip-path) was cutting the dialog to the card it sat in; the dialog now opens from the page itself, and a card holding an open dialog is never clipped. Same fix for the Follow dialog, the app-info popup and the Nugget-ID list, and for the bento view's Apps sheet.",
			"Fleet: picking a venue could show another venue's list with the rail on the new pick (a queued request was dropped when another request ran, and a late refresh could paint the old scope). List requests now have their own channel and a stale answer for another scope is dropped.",
			"Failed installs: \"Why did it fail?\" on a failed install (command page, the device's On its way box) asks the device for its install log (one fixed read-only command, audited) and shows the reason in plain words with the log lines under Details; a result that already carries an INSTALL_FAILED code shows its plain sentence at once.",
			"Setup (Ops lead): the rail no longer lists every venue (an All venues link instead), so it does not scroll; the queue can be grouped by who enrolled the devices or by model, with Deploy all N here for a group.",
		},
	},
	{
		Version: "1.64.34",
		Date:    "2026-11-19",
		Changes: []string{
			"Move API: `?import_only=1` on a device puts it on prod in its venue, marked as still reporting to mdm.dev, without filing a move. For clients too old to move themselves and boxes that have been dark for weeks, so prod's venues list all their devices.",
		},
	},
	{
		Version: "1.64.33",
		Date:    "2026-11-19",
		Changes: []string{
			"Fleet: picking a venue (or group) in the left rail now writes it to the address before the list is fetched, so anything that re-asks for the list while that request is in flight (a resize, the rows settling, a refresh) asks for the same scope instead of the old one.",
		},
	},
	{
		Version: "1.64.32",
		Date:    "2026-11-19",
		Changes: []string{
			"No Lab state: a device is To place (waiting in the queue) or In service (at a venue). The Lab filter, chips, counts, access rules and \"Send to Lab\" are gone; lab devices on the dev server now live in an AIO Lab venue like any other venue. The per-device \"Open in lab\" session stays. On first start, devices at no venue and not queued (about 4 on prod) are retired (reversible from their page), enrolment profiles that placed into Lab now queue, and an old AIO Enroll asking for Lab is treated as the queue.",
			"Engineer role retired: the 10 engineers become Support on first start (the role key stays valid but cannot be assigned). The QA API key now sees every device on the server it is configured on, and is not configured on prod.",
			"The Standard client's group picker works for a device that is in the queue or has no group yet. Online/offline history: a second server copy writing the same change is no longer an error (it filled server2's log every minute).",
		},
	},
	{
		Version: "1.64.31",
		Date:    "2026-11-19",
		Changes: []string{
			"Venue setting \"Apps\": install the role app automatically (yes by default). A venue set to no keeps whatever its devices run: the role-app install skips it and the Lineup Apps tab shows its units as \"auto-install off for this venue\". Part of moving lab devices into an AIO Lab venue.",
		},
	},
	{
		Version: "1.64.30",
		Date:    "2026-11-19",
		Changes: []string{
			"Deployment page: no made-up time left. The 14-minute default was shown as fact on a rollout where nothing had finished; the time left now comes only from installs that really finished here, and reads \"estimating…\" until one has.",
			"New venue page: the page no longer scrolls on a desktop window. The lists under the search (waiting devices, added lately), the search results and the picked place's card scroll inside their own boxes, and Add manually fits the window.",
		},
	},
	{
		Version: "1.64.29",
		Date:    "2026-11-19",
		Changes: []string{
			"Deployment page: the How it goes out tile shows its full text (no cut-off), Change is a proper button, and the tile rows are re-balanced so nothing is clipped or scrolls at any window size from 1280x700 up.",
		},
	},
	{
		Version: "1.64.28",
		Date:    "2026-11-19",
		Changes: []string{
			"Google Geocoding bill: reverse-geocoded addresses are now kept in the database (shared by both server copies, a month), in ~110 m cells instead of ~11 m, so restarts and Wi-Fi position jitter no longer buy the same address again; at most 300 paid Geocoding requests a day (GEOCODE_DAILY_CAP, 0 = no cap), after which the address is left off until the next day. November's bill was the Geocoding API alone ($21 by the 18th).",
		},
	},
	{
		Version: "1.64.27",
		Date:    "2026-11-19",
		Changes: []string{
			"Deployment page: the left column is a fixed grid of tiles that fits without scrolling (progress ring and state bar, time left, to do, installs over time, venues, how it goes out); each device row shows a progress bar with its percent, and the picked device shows a large readout with the amount transferred, speed and time left when known.",
		},
	},
	{
		Version: "1.64.26",
		Date:    "2026-11-19",
		Changes: []string{
			"Crash feed: Google Play services' location crash on the HK1 boxes (a NullPointerException in LocationProviderManager.getProperties on the Android 13 image) is no longer stored, and the rows already stored are removed once. It only restarts Google's own process; the fix is a newer Play services in the vendor image.",
		},
	},
	{
		Version: "1.64.25",
		Date:    "2026-11-19",
		Changes: []string{
			"Setup is simpler: no Setting up state, no Hand over, no Set up again. A device is Lab, To place or In service, and placing it at a venue puts it in service at once. The Ops lead Setup page is /setup (a queue table with an assign bar) and Field's is /setup/mine (their own devices to place) with Enroll at /setup/mine/enroll; the old /visits and /my-visits addresses redirect. Devices already at a venue are put in service on upgrade and the old Setting up grants are removed.",
			"Apps library in the Overview's language: a rail by product, one table, and a preview pane for the picked row with its channels, versions, Add version and the family tools.",
			"App page: a strip of channel cards with coverage rings; click a version to see only the devices running it.",
		},
	},
	{
		Version: "1.64.24",
		Date:    "2026-11-19",
		Changes: []string{
			"New landing page (the door): the shield, the name, one line, Sign in, Microsoft sign-in when configured and Create account, with the six device types as a quiet row. The sneak-peek preview (/sneak-peek and its sample fleet) is gone.",
		},
	},
	{
		Version: "1.64.23",
		Date:    "2026-11-18",
		Changes: []string{
			"People and account pages in the Overview's language, fitted to the window: Users (tiles, roles, table), the person page (profile, what they can do, can they reach a device), Access (who can do what matrix, grants, risks), People activity (actor first), My profile (display preferences with a live preview) and the owner's Your team card with an invite dialog; sign-in, sign-up, forgot and reset password redesigned, and the sign-in page now says when an email was verified or a password saved.",
			"Device page: the Apps list and the Health sheet no longer fold anything, the Health status line wraps, and the Network sheet shows the connection with 24 h of Wi-Fi signal on the left and the saved networks on the right.",
		},
	},
	{
		Version: "1.64.22",
		Date:    "2026-11-18",
		Changes: []string{
			"Releases and updates in the Overview's language, fitted to the window with no page scrolling: the Releases page is a table of releases with a preview (add a package, inspect, attach a flashing bundle, create a release with its first package); New update is a three-step stepper with a live \"will update N devices\" summary; the deployment page has the progress and to-do on the left and the devices with a detail and actions on the right; Update settings is a list with an explanation pane and tonight's counts; Release crashes lists the crash groups with their trace, trend and devices.",
		},
	},
	{
		Version: "1.64.21",
		Date:    "2026-11-18",
		Changes: []string{
			"Bento view sheets: Notes is a log (each entry has a time, author and kind, with what the MDM did to the device mixed in; the old single note becomes its first entry), Activity is a list with the picked event explained beside it, Health puts what needs a look first with passing checks as chips, About gets its admin actions, and Network and About are regrouped.",
			"Fleet: the device table no longer scrolls, only pages (the pager, the numbers and the arrow keys flip them).",
		},
	},
	{
		Version: "1.64.20",
		Date:    "2026-11-18",
		Changes: []string{
			"Sign-in: a few wrong passwords from one office no longer lock everyone on that network out. One account from one address is still stopped after 8 failures; one address trying many accounts after 60.",
		},
	},
	{
		Version: "1.64.19",
		Date:    "2026-11-18",
		Changes: []string{
			"Bento view: the Graph pane's metric list is a thin rail (dot, number, label), so the graph is wider.",
		},
	},
	{
		Version: "1.64.18",
		Date:    "2026-11-18",
		Changes: []string{
			"Bento view sheets: About fits without scrolling, the Location map fills its sheet, Activity, Health and Apps load in the background so they open filled, and Apps is a list with the picked app explained beside the host app card (Details… opens the existing info popup).",
		},
	},
	{
		Version: "1.64.17",
		Date:    "2026-11-18",
		Changes: []string{
			"Bento view: the facts strip and the Notes / Pad row are gone. The client, venue and groups are in the Details tile; a T7 gets a Charging pad tile with the on/off switch (other devices keep the Vitals rings); Notes is a dock entry. The graph card's enlarge button opens a Graphs sheet instead of full screen, and the sheets now come in with the page stepping back behind them.",
		},
	},
	{
		Version: "1.64.16",
		Date:    "2026-11-18",
		Changes: []string{
			"Bento view: the panel sheet opens in the centre, a second click on the dock button closes it, its header animates in with it, and the tab bar on the sheet is gone (the dock already switches panels).",
		},
	},
	{
		Version: "1.64.15",
		Date:    "2026-11-18",
		Changes: []string{
			"Device page, Bento view: no tab row. A labelled dock on the right edge opens Apps, Activity, Health, Network, Location and About as a sheet over the bento, with a switcher, Esc to close, and ?tab= links still working. Phones get the dock as a row.",
		},
	},
	{
		Version: "1.64.14",
		Date:    "2026-11-18",
		Changes: []string{
			"Device page, Bento view: now the Overview's own panes and fits the window with no scrolling. A radar of the device's health, a Today card (Graph, Needs, Activity, Apps; each a list with the pick explained), a coral action card, vitals rings and details. Phones stack. Still opt-in from the page's ⋯ menu.",
		},
	},
	{
		Version: "1.64.13",
		Date:    "2026-11-18",
		Changes: []string{
			"Device page: an optional Bento view, the Overview as tiles. Switch from the page's ⋯ menu (Bento view / Standard view); it is remembered per person and never the default.",
			"Menus close when you choose an item. Enrolment checks the size of the token and product, a scheduled recipe is sent at most once, and a few database write failures are now logged.",
		},
	},
	{
		Version: "1.64.12",
		Date:    "2026-11-18",
		Changes: []string{
			"QA audit fixes: a device socket can only report its own serial; the shared key is refused (not accepted) while the key lookup is down; a repeated or late command reply changes nothing; a command is handed to one poll at a time.",
			"Venues and groups: a taken group name says so (409), service hours and timezones are checked, names and notes have size limits, and removing or handing over devices checks the venue and each device's access.",
			"Hardening: QA history and battery log limits, migrations and partitions take a lock, job leases use database time, resent and scheduled shell commands obey the shell switch, and the database tests now run in CI.",
		},
	},
	{
		Version: "1.64.11",
		Date:    "2026-11-18",
		Changes: []string{
			"A computer's adb key can be approved on a Standard-client device from the admin API (command adb_approve): the device presses Allow on Android's own dialog, only when it shows the key named in the command. For two-screen POS units, where that dialog opens out of the remote screen's reach. Needs Standard client 0.16.1.",
		},
	},
	{
		Version: "1.64.10",
		Date:    "2026-11-18",
		Changes: []string{
			"Security: a remote-control connection must come from the session that opened it; reset, invite and verify links are stored hashed (links already mailed keep working).",
			"Security: failed sign-ins lock an account for the address they came from, not for everyone; an account is locked for all only after 48 failures in 15 minutes.",
			"Security: cancelling a firmware update is for an admin; resending a command needs you to see it and goes only to a device it was sent to; clearing Needs attention clears your own; the Groups list shows the groups holding a device you see.",
			"Security: an APK's icon is size-checked before it is decoded; a device's Save to library must answer within two hours with the version asked for; AIO Enroll no longer says when a device you can't see was last heard.",
		},
	},
	{
		Version: "1.64.9",
		Date:    "2026-11-18",
		Changes: []string{
			"Security: sign-in and Forgot password take the same time whether or not the account exists.",
			"Security: an app icon a device reports is kept only when it is a PNG; opening remote control is in the activity log.",
			"Security: owners can't add an AIO team address as staff; the inventory export can't be read as formulas by a spreadsheet; the release package check shows what it read as text.",
		},
	},
	{
		Version: "1.64.8",
		Date:    "2026-11-18",
		Changes: []string{
			"Security: installs take a build from the app library or one this server hosts; only a super admin can install from another address.",
			"Security: engineers can't place a device at a venue; deleting a venue is for an Ops lead or an admin; a device at a venue has its role changed in the MDM, not from AIO Enroll, unless you can operate it.",
			"Security: a caller with the right key is never refused because of someone else's wrong keys from the same network; one address can add at most 300 new devices an hour with the shared key.",
			"Security: the app library accepts only its own uploads and refuses APKs whose manifest or resources would unpack to an unreasonable size; log bundles leave out properties named key, token, secret or password.",
			"Security: live logcat tags and buffers can't be read as options; the public restaurant search has a daily ceiling; the splash folder no longer lists its files.",
		},
	},
	{
		Version: "1.64.7",
		Date:    "2026-11-18",
		Changes: []string{
			"Security: only the enrollment key lets a device with a lost key back in by itself. A device that has only the public key needs Reset device key.",
			"Security: an enrollment token no longer gives a new key to a Standard device that is online with its own. Viewers enrol into the To place queue, not straight into a venue.",
			"Security: signing in with Microsoft to an unverified sign-up replaces that account's password and signs it out everywhere.",
			"Security: a device's command output is kept only for commands sent to it, up to 8 MB each; oversized images (over 16 megapixels) are refused before decoding.",
			"Security: a check-in can't change a device from Firmware to Standard; one caller can't use up another device's request budget; a stored session value can't be used to sign in; nicknames are escaped on the owner home.",
		},
	},
	{
		Version: "1.64.6",
		Date:    "2026-11-17",
		Changes: []string{
			"Side rails are 1.3× wider (302 px) on every list page; Releases' list matches them.",
		},
	},
	{
		Version: "1.64.5",
		Date:    "2026-11-17",
		Changes: []string{
			"Updates: a device's \"last moved\" time is kept in the database, so the update page no longer flips between Stuck and Installing depending on which server copy answers.",
			"Every list page (Venues, Groups, Alerts, Apps, Models, Setup, MDM clients, Users, Access, Activity, Compliance, Releases, Lineup) has Fleet's width and placement, its pills at the top of the pane, and never scrolls as a whole.",
			"Pills and rail links switch the view in place: the page stays, only the pane dims, no skeleton or slide. Apps › Other apps lists the apps' makers in the rail.",
			"Loading L4: rail rows, pills and releases load on hover and views opened in the last minute come back instantly (refreshed after a change or reload); the loading bar shows only after 120 ms.",
		},
	},
	{
		Version: "1.64.4",
		Date:    "2026-11-17",
		Changes: []string{
			"UI review, first round: the Overview's By type and Right now cards have the open arrow; gradient buttons leave no coloured line on hover; Fleet fits its rows to the window again (no more 1–25 with a scrolling table).",
			"A rail that loads its pane (Setup, Models…) marks the pick at once and dims the pane with a moving bar while it loads; pages with a rail open with a two-column skeleton.",
			"List and stat cards no longer lift on hover; Setup's Enrolled and Profiles scroll after Show more, with a gap under their chips; Setup's rail drops Venues.",
			"Apps: AIO apps | Other apps as the top pills and only the products in the rail. Releases: the list and the detail run to the bottom, level at the top. The dock keeps Users on Access and Activity.",
		},
	},
	{
		Version: "1.64.3",
		Date:    "2026-11-17",
		Changes: []string{
			"Admin: the AIO backend placements (Enrollment › From the AIO backend: create or link a venue, move a venue's devices there) work over the admin API too.",
		},
	},
	{
		Version: "1.64.2",
		Date:    "2026-11-17",
		Changes: []string{
			"Models: the rail is a search and the list, no filters; picking a model changes only its pane, so the rail keeps its place.",
		},
	},
	{
		Version: "1.64.1",
		Date:    "2026-11-17",
		Changes: []string{
			"Models: each model's second line starts with Firmware or Standard (nothing is cut off on long names), and the rail's chips became one All · Firmware · Standard switch plus Needs a role and Several roles as rows.",
		},
	},
	{
		Version: "1.64.0",
		Date:    "2026-11-17",
		Changes: []string{
			"Models replaces Lineup's product list: every model in one list, biggest first, each tagged Firmware or Standard with its roles; chips show Firmware, Standard, Needs a role or Several roles. A model's page has its role setting, its devices by role, its MDM client, its roles' apps and, for our own boards, firmware with Promote.",
			"A Standard model has one role (every device of it is that role; AIO Enroll doesn't ask) or several (AIO Enroll asks which). A super admin or an Ops lead changes it; switching to one role says how many devices change first.",
			"A device picks its role only among its model's roles, on its page; a model with one role wins over a role set on a single device before. Role… left the right-click menu.",
		},
	},
	{
		Version: "1.63.1",
		Date:    "2026-11-17",
		Changes: []string{
			"Admin: a device's move to mdm.aioapp.com can be sent now (?now=1), so a firmware device at an open venue moves without waiting for closing time; it reboots into the move.",
		},
	},
	{
		Version: "1.63.0",
		Date:    "2026-11-17",
		Changes: []string{
			"Apps: Install app is now Add app. The app is added to the device and installs its newest build whenever the device is online, so an offline device gets it when it's back instead of losing it after an hour. If it disappears later, it's put back. Versions aren't tracked after that: AIO apps update themselves.",
			"Apps: the Apps tab has an \"On its way\" box for apps still being added or removed: waiting for the device, downloading, couldn't install (with Try again), removing. Cancel takes one back.",
			"Apps: Remove keeps an app off the device. The role app can't be removed. A successful uninstall takes the app off the device's list at once.",
			"Apps: Add app works on many devices at once: from ticked Fleet rows, the right-click menu, and a group's or a venue's ⋯ menu. Offline devices get it when they're back.",
			"Apps: follows are now added apps, and Lineup's auto-upgrade switches are gone; the role app is still installed where it's missing.",
			"Reboot needs the device online. Pressed on an offline device, a prompt says so and nothing is sent; on several devices, the offline ones are skipped and named. A reboot that never restarted the device reads \"sent · didn't restart\" after 15 minutes.",
			"A Wi-Fi change made on one device's page, and lost mode, wait a week for an offline device instead of expiring after an hour.",
		},
	},
	{
		Version: "1.62.9",
		Date:    "2026-11-17",
		Changes: []string{
			"Admin: a device that was offline when its move to mdm.aioapp.com went out, and collected it later, is no longer listed as having come back; a move listed as come back turns to arrived once mdm.aioapp.com hears from the device more recently than mdm.dev does.",
		},
	},
	{
		Version: "1.62.8",
		Date:    "2026-11-16",
		Changes: []string{
			"Header: mdm.dev.aioapp.com says DEV after the AIO MDM wordmark (and stage says STAGE), and its browser tabs start with \"DEV ·\", so it can't be mistaken for mdm.aioapp.com. Prod is unchanged.",
		},
	},
	{
		Version: "1.62.7",
		Date:    "2026-11-16",
		Changes: []string{
			"Alerts: \"Key reset automatically\" is raised only for devices at a venue. Lab devices are reflashed all the time and still get back in by themselves, without an alert; ones left open on Lab devices are resolved.",
		},
	},
	{
		Version: "1.62.6",
		Date:    "2026-11-16",
		Changes: []string{
			"Admin: a device's crash reports stay behind when it moves to mdm.aioapp.com (their stack traces were refused by the firewall in front of it); it sends its own there. The move list without a state filter lists every move.",
		},
	},
	{
		Version: "1.62.5",
		Date:    "2026-11-16",
		Changes: []string{
			"Admin: devices move from mdm.dev.aioapp.com to mdm.aioapp.com one by one or a venue at a time, through the admin API. Each device's record and settings are copied to mdm.aioapp.com first, where it waits without offline alerts; the move then goes out when the device is online (a firmware device at a venue reboots into it, so only while the venue is closed), and mdm.dev follows it until it checks in there. History stays on mdm.dev.",
			"Admin: /debug/vars shows a fingerprint of each shared device key, so two servers can be checked for the same keys without showing them.",
		},
	},
	{
		Version: "1.62.4",
		Date:    "2026-11-16",
		Changes: []string{
			"Devices: the admin API's device key reset works on Standard devices too. The server refuses the device from then on, and it enrolls again by itself about 10 minutes later with the token it came with (a device just moved here goes back to the server it came from instead).",
		},
	},
	{
		Version: "1.62.3",
		Date:    "2026-11-16",
		Changes: []string{
			"Admin: people (with their roles and access), Lineup and venues (with their settings and hours, not their devices or groups) can be copied from mdm.dev.aioapp.com to mdm.aioapp.com through the admin API, with a dry run first.",
		},
	},
	{
		Version: "1.62.2",
		Date:    "2026-11-16",
		Changes: []string{
			"Admin: mdm.aioapp.com can be emptied of its old fleet copy (devices, venues, groups and their history; people, Lineup, the app library and releases stay) before devices move to it one by one. Only there, only through the admin API, after a dry run, and never while a device is talking to it.",
		},
	},
	{
		Version: "1.62.1",
		Date:    "2026-11-16",
		Changes: []string{
			"Install app: AIO Launcher shows all its channels (Prod, Internal, Main (QA), Stage, UAT) with their builds, and a device can follow one, like the other AIO apps.",
			"A command sent while a device was offline, and not delivered within its hour, now shows as expired instead of waiting for good. Apps no longer spin \"Uninstalling…\" for an uninstall that will never run.",
			"Updates of the MDM restart its two server copies one at a time on every server, so the dashboard stays up and devices barely notice.",
		},
	},
	{
		Version: "1.62.0",
		Date:    "2026-11-16",
		Changes: []string{
			"Roles that match the work: Super admin, Engineer, Ops lead, Support, Field, Viewer, Owner and Staff, each with its own home and a dock of only its pages. A device is in the Lab, waiting to be placed, being set up or in service, and that decides who sees it. Access is given in three levels (View, Operate, Manage) on groups, venues and device states, and a page you can't open says why and who can give you access.",
			"Setup: enrolled devices wait in a queue until the Ops lead gives them a role and a venue (or creates the venue on the spot); placing a device puts it in service. Field people see every device being set up, and the devices they enrolled themselves. Enrollment lives inside Setup, and AIO Enroll for Android asks where a new device is going.",
			"A new look: one grey canvas with white cards, header, island and dock; coral for what is selected. The Overview is one screen: a fleet health score and what makes it up, Today with one item explained at a time, and the biggest device types as dials. The island shows where you are, your messages, new critical alerts and the progress of jobs you started.",
			"Every list works like Fleet: Venues, Groups, Apps, Alerts, Lineup, Compliance, MDM clients, Wireless adb and Setup have a side list, search, sort, quick filters and one table. Right-click a device, venue, group, app or person for its actions. Long side lists have a search of their own and run the full height of the page.",
			"Fleet: on a computer a page holds exactly the rows that fit and one scroll turns one page, with a bar that shows where you are in the list; a reload keeps your page. Our own map (light and dark) shows venues and devices, on Fleet and the device page. A new filter finds devices by MDM client version.",
			"Device page: a cleaner layout with a Location tab again. Install app picks the app, then its channel (prod, UAT, stage…) and build, and a device can follow a channel so it always gets that channel's newest build. Standard-client devices get the host app card too. One model can serve several roles, and a Standard device's role can be set one by one. Handheld is now called MPOS.",
			"Venues: a new venue starts with one search of Google Maps and the AIO backend, and fills in its address, hours and icon. A venue's page lists its settings down the side, and shows its devices by type with a button that opens them in Fleet.",
			"Apps: the library takes split APK bundles (.xapk) and installs them on clients that can. Builds come in by upload only; the library no longer pulls apps off devices. An install that fails three times in a day stops being retried, and every library build has a download button.",
			"Phones: every page works on a phone. Wide tables turn into cards and side lists fold into one line.",
			"More devices, no downtime: the server runs as two copies that share the fleet, so an update of the MDM restarts one at a time and devices barely notice. Device readings keep full detail for 14 days and a thinned history for good. A device that loses its key gets back in by itself, and Standard-client devices can be moved to another MDM server.",
			"Retired: the legacy OTA updater, MDM Lite, the link between MDM servers, setup visits, View as, and the \"Possible impersonation\" and \"Host app not on screen\" alerts.",
		},
	},
	{
		Version: "1.61.0",
		Date:    "2026-10-27",
		Changes: []string{
			"Sign in with Microsoft: aioapp.com accounts sign in with their work account. A first sign-in creates a Viewer account, and an admin gives it a role.",
			"The device page is tidier. Every tab is one card where one will do: Apps is one list ranked by what each app uses, with the host app on a line above it and system apps folded; Activity shows what is queued and running at the top of the same timeline; Health is one card with the same to-do list as the Overview; Network and About lost their extra cards. Needs a look says what is wrong and offers the fix. Drag the divider between the graph and the tabs and it stays where you left it; the graph never grows taller than half its width, and Recent on this device fills the space under it. The New view is now everyone's default.",
			"Updates tell you more. The device's update line shows its steps in order with the download speed and the time left, and a device can be lined up to finish one update before the next one starts (the update page says which it completes first).",
			"Host apps: each venue can pick a channel per product (prod, UAT, stage…), and a product can keep its devices on the library's newest build by itself, inside the update window. A venue that switches channel gets the new build straight away.",
			"Save to library: an app installed on a device can be copied into the app library, and the library keeps itself up to date from the devices, for every channel. A build signed with another key is refused and skipped.",
			"Compliance names the problem (\"Developer options on\", \"Installs from unknown sources allowed\"), and the MDM's own clients no longer count as an unknown source.",
			"Settings is down to Sign-in, Devices, Data and Display; every other setting lives on the page it is about. The menu has six sections, display preferences are set per person in My profile, and pages use the width of big screens up to 1760 px.",
			"Enrollment opens on How to enroll, with the onboarding video and its steps. Wi-Fi finds and the AIO backend view are for super admins.",
			"TV boxes, kiosks, kitchen displays and POS terminals on the Standard client show their real CPU temperature instead of a stuck battery reading.",
			"A new shield icon for the website, the dashboard app and the device clients. Security hardening across sign-in, access rules and enrollment.",
		},
	},
	{
		Version: "1.60.0",
		Date:    "2026-10-23",
		Changes: []string{
			"The dashboard speaks one language. The Overview, Venues, each venue's page, Apps and each app's page, Alerts, Compliance, Releases, rollouts, Jobs, Lineup and the people pages now share one layout: filter pills at the top, a short to-do list of what needs a look with a button on each line, cards that compare with the period before, and a side column with the headline number. Menus everywhere close when you click outside them or press Esc.",
			"Rollouts are calmer and quicker. Devices that are offline are never failed: they get the update when they come back. The limit of a few downloads per venue at a time is gone, so every device that checks in starts straight away. A rollout's page shows each device's download size from the package it was actually sent, and older devices updating the legacy way show up on the Releases page as updating.",
			"Resource use: every app on a device, and on every device of a model, ranked by memory, CPU, screen time, network and battery. The device page's Apps tab opens on the top five apps for memory and CPU side by side, and a Resource use page compares whole models and venues, with what to look at first (an app stopping, an app over its limit, a device far above the rest).",
			"Network: the device graph has a Network tab with Wi-Fi signal, when the device was online and offline, and when it changed network. A Wi-Fi card lists the device's saved networks with their signal, and can add, change the password of, forget and switch networks. A switch is safe: the device goes back to its old network by itself if it can't reach the MDM from the new one within 90 seconds, and says why. Add network offers the networks saved for the device's venue, nearest first, with their passwords filled in.",
			"The device page is tidier: the Overview tab shows only what needs a look and the device card (now with its build), and Network and Location are tabs of their own.",
			"Remote control works like a real keyboard: click the screen and every key goes to the device, Tab and Esc included, with shortcuts for Home, Back and Recents, typing, scrolling and pasting from your clipboard. Menu boards get a TV remote with a D-pad beside the screen.",
			"Venues: pause updates for a venue or keep them to when it is closed, turn auto-enrol on per venue, pause the lost-device watch, and see the venue's devices by type in one card.",
			"Notifications on the devices are in plain words with the AIO icon: \"Connected to AIO\" or \"Offline · trying again\", which app is being installed and how far along, and \"AIO MDM updated\" from one version to the next. Android's own \"Updated by your admin\" with its robot picture is gone from tablets (Firmware MDM 1.19.0).",
			"The Standard client (0.5.0 to 0.9.1) keeps its readings while offline and sends them later, reports app usage, switches Wi-Fi safely, shows the same notifications and AIO icon, and now reports where it is from the Wi-Fi networks around it, so its devices appear on the map like the tablets do. Its versions now come with a changelog.",
			"Tableside AI tablets hold their screen brightness at a fixed level, and staff can unlock a locked tablet with the venue's staff PIN for staff mode. A tablet with its battery unplugged shows a battery fault instead of reading 0%.",
			"People: the team and customers each have their own roles (Viewer, Support, Operator, Fleet manager, Admin, Super admin; Owner and Staff for customers), invitations go by email, and venue owners manage their own staff.",
			"Retired: QA checklists and sign-off on releases, the Mega kiosk device class, and the screen brightness control on the device page (brightness is still managed from the server).",
		},
	},
	{
		Version: "1.56.0",
		Date:    "2026-10-20",
		Changes: []string{
			"Kiosk is the product, Lock is the feature. Locking a device to one app is now called Lock everywhere in the dashboard, with a Device lock page of venues and an on/off switch each. Every role names its app on Lineup, and Lock uses it: the Lock button on a device locks to its role's app by itself.",
			"Venues get icons, found from the venue's website, and one Venue settings card where Profile, Location, Service hours, Device lock and Device Wi-Fi each open in place. Location searches addresses and can use where the venue's devices are. A venue's Wi-Fi networks are pushed to every device there, new ones included.",
			"Lost mode: mark a tablet as lost and it shows a lost screen with the venue and a support number; nearby tablets listen for it, and a venue can have tablets lock themselves when they leave its Wi-Fi.",
			"Compliance: one page with the share of devices passing every required check, per check, per venue and per device, the rules behind it, and a compliance section in the venue reports.",
			"Clients keep themselves up to date. Each client has a standing instruction to keep devices on the hosted build, inside each venue's own quiet hours.",
			"A new device page view with Overview, Apps, Activity, Health, Location and About tabs, a week-long graph finder, a Host app card that shows the app a tablet is there to run, and a Battery wear card. The classic view stays one click away.",
			"A new Overview: venues with their health, compliance, devices by type, what needs attention and a clean-up list. Wireless adb connects from the device page with live progress, ending in the command to copy.",
			"Retired: the rotating unlock code (the exit PIN stays), guest Wi-Fi, the boot logo page, and the Overview map card.",
			"Polish: Forgot password and Reset password match the sign-in page and follow dark mode, the sign-up form fits the screen, keyboard focus shows on every header button, and empty pages say what to do next.",
		},
	},
	{
		Version: "1.55.0",
		Date:    "2026-10-07",
		Changes: []string{
			"Wireless adb from anywhere. A new Wireless adb page lists every device adb was ever switched on for, switches it on or off in batches, and opens an adb tunnel through the MDM server, so `adb connect` works from any network without being on the device's Wi-Fi. A tunnel accepts only the addresses you allow, shows a refused address with an Allow-it button, and closes itself after an hour idle. Firmware client 1.8.9 and standard client 0.2.9 carry the device side.",
			"AIO Enroll, a desktop app for Mac, Windows and Linux, enrolls devices over a USB cable with a dashboard sign-in. The dashboard records who enrolled each standard-client device: on the device page, as a Fleet column and filter, in a Recent enrollments log on the Enrollment page, and on the person's profile.",
			"A new Overview for everyone: every device is a square grouped by restaurant and type, worst first, next to a map of your restaurants, a needs-attention list, service uptime and a clean-up checklist. Devices away from their venue show a dashed line on the map.",
			"The dock: names under every icon, live figures (online ring, alert count, running actions), your three most-visited pages, and the search field. Admin pages live in the ⌘K palette's pages mode; Esc closes it from anywhere and Shift-Enter opens a result in a new tab.",
			"The top bar pulls in to a floating pill once you scroll, over a warm paper background. Long lists on Clients, Enrollment and Wireless adb get a search box, filter chips and a Show-all cap.",
			"Alerts are problems now: one card per device fault with assign, acknowledge, snooze and resolve-with-a-reason; crash signatures across several devices are release issues; after-hours alerts hold until morning with a digest. Kiosk exits on site stay out until someone locks the device again, and say so.",
			"Access control rewritten around two questions, which devices and what they can do, with reusable access profiles. The dev role is retired: those accounts are super ops, and releases, shell and release issues belong to the super admin.",
			"Device keys: firmware devices register their own key and the shared one is refused for them afterwards; a device that lost its key is let back in from its usual address once a day with a warning, or reset from its Settings tab. Tablets reporting a corrupted serial are refused and shown at the top of the Fleet.",
			"Kiosk policies are rules only: ordered, enforced every minute, with a per-device status. Collect logs from one device or many. Guest info (venue Wi-Fi, table label) is sent to tablets. Fast temperature readings per device, kept 24 hours.",
			"Offline check-ins: a device that was out of reach sends its readings later, drawn as shaded spans on its charts. The device graph reads battery cycles from lifetime wear, hovers with a readout beside the plot, and shows a flapping charger as a hatched run.",
			"Clients page: the firmware client and standard client, their devices grouped by what can be acted on, releases with download, checksum and rollback. Every venue can get a weekly report in six plain-language layouts, as a page, a PDF or an email.",
			"Under the hood: check-in history on a bounded queue, telemetry stored by shape instead of one row per report, faster deploys that confirm what is running, a deploying page instead of a gateway error, and a Server page with the MDM's own load and slowest routes. Both environments now share the development database cluster.",
		},
	},
	{
		Version: "1.54.0",
		Date:    "2026-09-20",
		Changes: []string{
			"The DPC agent updates itself. When a newer agent build is hosted, a device running an older one shows “Update agent” on its device page, and the whole DPC fleet can be updated from Actions. The agent checks the download is genuinely a newer build of itself, signed with the same key, before installing — silently, since it is Device Owner — and reports the result once it restarts.",
			"A new agent build can be published straight from a build machine over the admin API, instead of being uploaded by hand.",
			"Enrollment QR codes now carry the agent's signing-certificate checksum alongside the file checksum, which is what current Android versions prefer when downloading the agent during setup.",
			"Screenshots work on DPC devices.",
			"Devices enrolled with an agent from before this release keep working, but cannot be updated to the current one — they need to be re-enrolled after a factory reset.",
		},
	},
	{
		Version: "1.53.0",
		Date:    "2026-09-19",
		Changes: []string{
			"Menu boards have their own device class, so a TV stick is no longer filed as a tablet.",
			"The Fleet page has been rebuilt: products are tabs above the list, devices are two-line cards with type and power icons and a status dot, ranges can be selected with shift-click, and the filters panel fits properly on smaller screens. A switch lets you go back to the classic layout.",
			"Every venue can get a weekly report — printable, downloadable as a real PDF, and emailed in the same shape as the page.",
			"Charts and exports are computed from purpose-built tables, so longer windows draw quickly; charge sessions are counted as sessions rather than flag changes, and site metrics no longer count a flapping device many times over.",
			"Deploys are faster and confirm what ended up running.",
			"Fixes: an operator with a base-deny policy saw an empty device list; a confirmed enrollment was reported as failed; latitude and longitude were rounded to about 11 km in the CSV; clicking a date closed the range picker.",
		},
	},
	{
		Version: "1.52.0",
		Date:    "2026-09-15",
		Changes: []string{
			"Older devices join the same rollout as everyone else. One push now drives both transports, so a release goes out to legacy and current devices together, with a rollout page of its own, an explicit “awaiting reboot” state, and live install progress even for devices with no live connection.",
			"Releases are easier to steer: new releases start visible to devs and admins only, the release picker is a type-ahead, each release says what it can actually deliver, and re-publishing a build can no longer quietly rewrite the release it belongs to. Releases and OTA packages can also be published over the admin API, so a build machine can do it without a browser.",
			"A device can be moved to another MDM server from its own page, choosing from the servers configured in Settings.",
			"Crashes that repeat are folded into a single row with a device count, and there's an Acknowledge all button.",
			"Battery reporting is honest about the hardware: a charging strip under the battery chart, and no battery UI at all for devices that don't have one.",
			"Fixes: a restaurant scope pulled in devices it shouldn't have; the headline count went stale mid-rollout; fresh installs failed to migrate on an empty database; the move-server control spilled out of its card; a blank screen when the browser couldn't decode H.264.",
		},
	},
	{
		Version: "1.51.0",
		Date:    "2026-09-10",
		Changes: []string{
			"Who can do what is now set in one place. A new Access control page shows every policy at a glance, with an editor for the rules, a panel that answers “who can reach this device?”, and rule counts so a policy that does nothing is obvious. Access is enforced everywhere it should be — device pages, group sends, command resends, remote control, shell and OTA all follow the same rules.",
			"The Actions page has been rebuilt around the target: choose who first, then what. Restaurants, groups and individual devices can be combined in one send and are de-duplicated automatically, devices already covered by a chosen scope drop out of the list, and serials can be pasted in bulk and are validated as they land. Operators get presets for the actions they run most.",
			"Apps live in a library. The App library page collects every APK, groups variants and versions of the same app under one tile, and accepts new ones by dropping them anywhere on the page. The same picker is used everywhere apps are chosen.",
			"Actions history is easier to read: multi-app sends appear as one batch, rows carry the app's icon and name, and “by” is its own aligned column. Admin-only actions are hidden behind a toggle that is off by default.",
			"Deployments record who pushed them, explain update_engine failures in words, and offer Retry on a stalled row or Cancel on a stuck download. The deployment page leads with the release, shows full versus incremental per device, and gathers the reboot settings into one pane.",
			"People have faces: profile pictures appear in the activity log, actions history and the command palette, and admins can set or remove any user's picture.",
			"The dashboard now shares the landing page's design language throughout.",
			"Fixes: an id clash broke every confirmation dialog; Send was refused for admins and devs; a failed or cancelled deployment row blocked the next push; CSV exports and device graphs disagreed about timestamps and timezones.",
		},
	},
	{
		Version: "1.50.0",
		Date:    "2026-09-04",
		Changes: []string{
			"A new look for the whole dashboard. Every page — Home, Fleet, Actions, Alerts, Releases, Updates, Settings and the device pages — now shares one “command center” design with consistent cards, tables, buttons and a retuned dark theme with better contrast. Dropdowns, toasts and the bottom dock use a frosted-glass style.",
			"A Daily Report on the Home and Fleet health pages summarises what needs attention today, with the charts behind each finding one click away.",
			"Install the dashboard as an app on your phone, tablet or desktop, and take a short, skippable guided walkthrough the first time you sign in.",
			"Sign in with Microsoft, or create your own account and reset a forgotten password by email. Accounts are now keyed by email and carry a first and last name, so every action, command and change shows who did it — including a new “By” column in Actions history and a paginated Activity page with filters. The Tester role is now called Operator.",
			"The Actions page has been rebuilt: pick apps, devices, shell commands and queries in focused dialogs; the target starts empty so nothing goes fleet-wide by accident, and sending to the whole fleet asks first. History updates live, is grouped by day, and rows can be dismissed or deleted without a reload.",
			"Software updates are faster and clearer. Updates reach connected devices instantly, and the device page shows a live download/install bar that survives a server restart, with a cancel button. The redesigned deployment page shows each device's progress, a plain Retry next to Retry with full image, exactly when the applying reboot was pushed, and which build the device will boot into.",
			"The device page graph shows when a device switched builds — a marker with the new build name at every change, with a toggle to show or hide them — and a second toggle to overlay when commands were sent. Hover a marker to see the details.",
			"On the device page: tap the Wi-Fi pill to see nearby networks, see APK versions everywhere apps are listed, and a wireless pad that keeps flapping is called out as “Faulty” with an explanation. A device gets a short grace period before it's shown offline, and the location map matches the dashboard theme.",
			"Import & Visualize: open a CSV from Export and turn it into a sortable table and charts, entirely in your browser — the file never leaves your computer. Now available to every signed-in user from the Home page. Export also gains a Charging column.",
			"Fleet page improvements: a Map view toggle, “Select all matching” beyond the current page, inline rename of groups, venues and releases from a small menu, and adding or removing group members in place without reloading.",
			"Faster everywhere: pages that ran a dozen queries in sequence now run them in parallel, static files are compressed, the main dock pages are prefetched so switching between them feels instant, and the device page loads its default graph window first and fetches the rest in the background.",
			"Crash and ANR cards show the app's real icon, the Crashes feed no longer lists kiosk-exit events, the remote-control screen has a redesigned cockpit, notifications have a new toast design, and a branded page greets a wrong URL.",
			"Fixes: a deployment could sit at “pending” forever after a recent failed update; an in-progress OTA was re-pushed every minute; commands could show “in progress” after finishing; a cancelled command on an offline device could still run; the Overview's Customize menu could not be opened; several dark-mode contrast and clipping issues across the app.",
		},
	},
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
			"Alerts now has its own spot in the main navigation. Fleet Health was folded away — its score, verdict, and crash signals already live on the Overview page, so it's no longer a separate page.",
			"Every action button (Screenshot, Reboot, install, and more) now shows a clear loading state the moment you click it, so a fast action doesn't feel like nothing happened. Clicking Screenshot again while one is still pending now takes you straight to that in-progress command instead of just flashing a message.",
			"Fixed a bug where cancelling a queued command on an offline device didn't actually stop it from running once the device reconnected.",
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
