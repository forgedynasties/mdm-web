# What's New

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
