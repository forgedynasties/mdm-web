  ---
  High Value, Low Effort

  Shell Command Execution
  - Send arbitrary shell commands to devices, get stdout back
  - Same pattern as logcat — queue command, device runs it on checkin, POSTs
  result
  - Very powerful: check running processes, disk usage, network stats, etc.

  Device Screenshot
  - Device captures screen via screencap -p, uploads as PNG to server
  - Dashboard shows it inline
  - Bandwidth: ~100-300KB per screenshot, only on-demand

  Reboot / Factory Reset Commands
  - reboot, reboot recovery, wipe data/factory reset via shell
  - Simple one-way command, no result needed

  ---
  High Value, Medium Effort

  File Push
  - Server stores a file URL + destination path
  - Device downloads the file and places it at the given path
  - Use case: push configs, certs, wallpapers, etc.

  Property Reader
  - Device sends getprop output (or specific keys) on each checkin
  - Surface as filterable columns in the dashboard (Android version, model,
  locale, timezone)

  Geofencing / Location
  - Device sends GPS coordinates via extra fields on checkin
  - Dashboard shows device on a map (Leaflet.js, no API key needed)
  - Alert if device leaves a defined area

  Alert Rules
  - Server-side rules: "notify if battery < 10%", "notify if device not seen
  for > 2 hours"
  - Webhook or email notification
  - Stored in DB, evaluated on each checkin

  ---
  Medium Value, Medium Effort

  APK Uninstall
  - New command type: uninstall_apk with package name
  - Device runs pm uninstall <package>
  - Ack back with result

  Installed App Inventory
  - Device sends list of installed packages + versions on checkin (or
  on-demand)
  - Dashboard shows per-device app list, searchable across fleet

  Config / Policy Enforcement
  - Define key-value settings (WiFi SSID, DNS, screen timeout, etc.)
  - Device reads and applies them on checkin
  - Dashboard shows compliance status

  Bulk Actions
  - Select multiple devices in the dashboard and apply a command, reboot, etc.
  - Currently only groups or "all" are supported

  ---
  Lower Priority

  Audit Log — track who issued what command from the dashboard and when

  Multi-user Dashboard — currently single user/password; add role-based access

  Device Tags / Labels — freeform labels beyond groups (e.g. floor-3, kiosk)

  Export — CSV export of device list, checkin history, command history

  API Tokens per user — currently one shared API key for all devices