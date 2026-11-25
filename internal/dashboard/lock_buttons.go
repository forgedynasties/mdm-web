package dashboard

// lockButtonsShown gates the device page's Lock controls (the island Lock button, the
// Lock pill, the ⋯ menu's Lock item, and "Lock again" on the unlocked notice). Off since
// 25 Nov 2026 (user: "i want no one to be able to lock the devices in kiosk mode", "keep a
// bool instead to hide the lock button"). Only the buttons are hidden: the routes, the
// Lock dialog and lock-host.js stay, so setting this to true brings them back as they were.
const lockButtonsShown = false

// lockButtons is the template func for lockButtonsShown.
func lockButtons() bool { return lockButtonsShown }
