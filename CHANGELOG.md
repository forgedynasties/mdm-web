# What's New

## v1.50

### One design for the whole dashboard
- Every page now shares a single "command center" look: consistent cards, tables and
  buttons, a retuned dark theme with better contrast, and frosted-glass dropdowns,
  toasts and dock.
- Install the dashboard as an app on your phone, tablet or desktop.
- A short, skippable guided walkthrough greets first-time users.

### Home page is a command center
- Sites shown as health-scored heat-map tiles, plus active rollouts, release adoption,
  fleet vitals and a live fleet map (with its own full-page Map view).
- Hide widgets, drag them between columns, or pick a preset view — saved to your
  account. Switching views updates in place with a progress bar, no full reload.
- A Daily Report (also on Fleet health) summarises what needs attention today, with
  the charts behind each finding one click away.

### Accounts and who-did-what
- Sign in with Microsoft.
- Create your own account and reset a forgotten password by email.
- Accounts are keyed by email and carry first/last names; every action shows who did
  it — a new "By" column in Actions history and a paginated Activity page with filters.
- The Tester role is now called Operator.

### Kiosk policies
- Kiosk mode is now a policy: define named policies (locked app, allowed apps) on the
  new Policies page and apply them to groups or venues from a proper device picker.
- Enabling kiosk wakes the screen; the kiosk app picker is a compact list.

### Actions page rebuilt
- Focused dialogs to pick apps, devices, shell commands and queries.
- Target starts empty, and sending to the whole fleet asks first.
- History updates live, is grouped by day, and rows can be dismissed or deleted
  without reload.

### Software updates
- Updates reach connected devices instantly.
- Device page shows a live download/install bar (survives a server restart) with a
  cancel button and a "will boot into X" indicator.
- Redesigned deployment page: per-device progress, plain Retry alongside Retry with
  full image, and exactly when the applying reboot was pushed.

### Device page
- Graph markers show every build switch, labelled with the new build; toggle them on
  or off. A second toggle overlays when commands were sent. Hover for details.
- Tap the Wi-Fi pill to see nearby networks.
- APK versions shown wherever apps are listed.
- A flapping wireless pad is called out as "Faulty" with an explanation.
- Short grace period before a device is shown offline.
- Location map matches the dashboard theme.

### Export
- Import & Visualize: open a CSV from Export as a table and charts, entirely in your
  browser — the file never leaves your computer. Available to every signed-in user.
- New Charging column in exports.

### Fleet page
- Map view toggle.
- "Select all matching" beyond the current page.
- Inline rename of groups, venues and releases; add or remove members in place.

### Faster
- Parallel queries on the heaviest pages, compressed static files, prefetched dock
  pages, and the device page loads its default graph window first.

### Also
- Crash/ANR cards show the app's real icon; kiosk-exit events no longer appear in the
  Crashes feed.
- Redesigned remote-control cockpit and notification toasts.
- Branded 404 page.

### Fixes
- Deployment stuck at "pending" after a recent failed update.
- In-progress OTA re-pushed every minute.
- Commands showing "in progress" after finishing.
- Cancelled command on an offline device could still run.
- Overview's Customize menu could not be opened.
- Various dark-mode contrast and clipping issues.

## v1.41

### Apps are now a library, not URLs
- Upload your APKs once and install them from a searchable library with icons — no
  more pasting download links.
- Upload several APKs at once, cancel one mid-upload, and review the details before
  anything is added.
- Tap an app to see its details.

### Installing apps is instant
- Pick one app or many, hit Install once, and watch them all on a single progress
  page.
- The picker closes right away and the apps show up as "Installing…" in the device's
  app drawer — no waiting on a spinning overlay.
- See live install progress, and cancel an install that's still running.
- Apps already on the device are hidden from the picker so you only see what you can
  actually add.

### Uninstalling is instant too
- The app shows an "Uninstalling…" spinner the moment you tap it and stays put until
  it's really gone — then updates once.
- An app you just removed comes right back in the Install list, ready to reinstall.

### Wireless charging
- Turn the charging pad on or off straight from the device page — the toggle flips
  in place, no reload.
- When charging is off, the status reads "Not available" instead of a misleading
  reading, and the graph legend explains each state.

### Deployments
- OTA updates now go out instantly, with an "apply now" reboot button for manual
  deploys.
- New "Reboot all installed" action to reboot a whole group at once.

### Device page polish
- Redesigned device page.
- Switch graphs and timestamps between your time and the device's time.
- Open any graph full screen.
- Kiosk lock only offers apps that are actually installed on the device.
- Command history shows the app's name instead of a long download URL.
- Fixed the Actions menu getting cut off, and cleaned up confusing "on next
  check-in" wording throughout.

### Under the hood
- More reliable reboots and app installs: fixed cases where a reboot looked "done"
  before it happened and where an install could stall or get re-sent to an offline
  device.
