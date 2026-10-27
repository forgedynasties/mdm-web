package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

// ── Compliance engine ─────────────────────────────────────────────────────────
//
// static/compliance-plan.html (15 Oct); the rules editor is static/compliance-
// baseline-demos.html, B1. One registry of checks, each a fixed question with a default
// level. The fleet baseline sets each check's level and threshold; ordered overrides
// change that for a venue, group, role, or the devices in a lab session — first override that covers the device and says something about the check
// wins, as lock rules do. A device is compliant when no required check fails. Unknown
// (the device does not report it) and n/a (it does not apply) never count either way.
//
// Evaluation is one pure function over a snapshot of the fleet, run every minute; only
// changed results are written, and every state change is kept as an event.

// compSnapshot is everything the checks read, loaded once per pass.
type compSnapshot struct {
	now     time.Time
	devices []db.Device
	scopes  map[uuid.UUID]db.DeviceScope
	lock    map[uuid.UUID]string // lock-rule status (kioskDevice.Status)
	labs    map[uuid.UUID]db.LabSession
	prodFW  map[string]string                 // model → prod firmware version, for models in prod mode
	hosted  map[string]config.AgentAPKBuild // client slot → hosted build
	ownKey  map[uuid.UUID]bool
}

// compCheck is one question in the registry.
type compCheck struct {
	ID, Group, Name, Help string
	// Problem is how a failing device reads (26 Oct 2026, user: "it should just say
	// installs from unknown sources allowed, developer options on"): the name states the
	// rule, the problem what is wrong.
	Problem string
	Device                bool   // answered by what the device reports (else by the server)
	Level                 string // default level
	Param                 int    // default threshold, 0 when the check has none
	Unit                  string // the threshold's unit ("hours", "days")
	FixedBy               string // what already acts on a failure, for the editor
	Fix                   string // what auto-fix does, "" when the check has none
	AutoFixDefault        bool   // auto-fix on before anyone sets it (decision of 15 Oct)
	eval                  func(s *compSnapshot, d *db.Device, param int) (state, detail string)
}

const (
	compGroupSetup  = "Set up as intended"
	compGroupDebug  = "Debug access closed"
	compGroupIntact = "Integrity"
)

var complianceChecks = []compCheck{
	{ID: "firmware_approved", Group: compGroupSetup, Name: "Firmware on the approved build", Problem: "Firmware not on the approved build", Level: db.ComplianceRequired,
		Help: "The prod firmware set for its model, or newer. Devices in a lab session and stock devices are not measured.", FixedBy: "Role firmware",
		eval: evalFirmwareApproved},
	{ID: "locked", Group: compGroupSetup, Name: "Locked as its rule says", Problem: "Not locked as its rule says", Level: db.ComplianceRequired,
		Help: "The lock rule covering it has it locked. Taken out on the device, or unlocked by hand, fails.", FixedBy: "Lock rules",
		eval: evalLocked},
	{ID: "client_current", Group: compGroupSetup, Name: "Client current", Problem: "Client out of date", Level: db.ComplianceRecommended,
		Help: "On the client build hosted for its signing key. Not measured where none is hosted (v2.0 images).", FixedBy: "Client auto-update",
		eval: evalClientCurrent},
	{ID: "checked_in", Group: compGroupSetup, Name: "Checked in recently", Problem: "Not checked in recently", Level: db.ComplianceRequired, Param: 24, Unit: "hours",
		Help: "Seen by the server within the time below.",
		eval: evalCheckedIn},
	{ID: "own_key", Group: compGroupSetup, Name: "Own device key", Problem: "On the shared key, no key of its own", Level: db.ComplianceRecommended,
		Help: "Registered its own key, so the shared key no longer works for it (firmware client 1.6.2+).",
		eval: evalOwnKey},
	{ID: "adb_off", Group: compGroupDebug, Name: "USB debugging off", Problem: "USB debugging on", Device: true, Level: db.ComplianceRequired,
		eval: evalExtraFalse("adb_enabled", "USB debugging is on")},
	{ID: "wireless_adb_off", Group: compGroupDebug, Name: "Wireless adb off", Problem: "Wireless adb on", Device: true, Level: db.ComplianceRequired,
		Help: "Nothing listening for adb on the venue network.",
		Fix:  "turns wireless adb off, unless someone turned it on for a while from the adb page", AutoFixDefault: true,
		eval: evalExtraFalse("adb_tcp", "adb is listening on the network")},
	{ID: "dev_options_off", Group: compGroupDebug, Name: "Developer options off", Problem: "Developer options on", Device: true, Level: db.ComplianceRecommended,
		eval: evalExtraFalse("dev_options_enabled", "Developer options are on")},
	{ID: "release_build", Group: compGroupDebug, Name: "Release build", Problem: "Not a release build", Level: db.ComplianceRequired,
		Help: "A user build, not userdebug or eng.",
		eval: evalReleaseBuild},
	{ID: "unknown_sources_off", Group: compGroupIntact, Name: "No installs from unknown sources", Problem: "Installs from unknown sources allowed", Device: true, Level: db.ComplianceRequired,
		Help: "No app is allowed to install other apps outside the MDM.",
		eval: evalUnknownSources},
	{ID: "not_rooted", Group: compGroupIntact, Name: "Not rooted", Problem: "Rooted", Device: true, Level: db.ComplianceRequired,
		eval: evalExtraFalse("su_present", "An su binary is present")},
	{ID: "encrypted", Group: compGroupIntact, Name: "Storage encrypted", Problem: "Storage not encrypted", Device: true, Level: db.ComplianceRequired,
		eval: evalExtraTrue("storage_encrypted", "Storage is not encrypted")},
	{ID: "play_protect", Group: compGroupIntact, Name: "Play Protect on", Problem: "Play Protect off", Device: true, Level: db.ComplianceRecommended,
		Help: "Only where the image has Google Play.",
		eval: evalExtraTrue("play_protect", "Play Protect is off")},
	{ID: "security_patch_recent", Group: compGroupIntact, Name: "Security patch recent", Problem: "Security patch out of date", Device: true, Level: db.ComplianceRecommended, Param: 180, Unit: "days",
		Help: "The Android security patch level is no older than the time below.",
		eval: evalSecurityPatch},
	{ID: "verified_boot", Group: compGroupIntact, Name: "Verified boot", Problem: "Boot not verified", Device: true, Level: db.ComplianceRecommended,
		Help: "The bootloader verified the image it started (green). Yellow, orange or red means a self-signed, unlocked or failed boot.",
		eval: evalVerifiedBoot},
	{ID: "selinux_enforcing", Group: compGroupIntact, Name: "SELinux enforcing", Problem: "SELinux not enforcing", Device: true, Level: db.ComplianceRequired,
		eval: evalSELinux},
	{ID: "device_owner", Group: compGroupSetup, Name: "Standard client is Device Owner", Problem: "Standard client not Device Owner", Device: true, Level: db.ComplianceRequired,
		Help: "Without Device Owner the standard client cannot lock, install or reset. Firmware clients are system apps and are not measured.",
		eval: evalDeviceOwner},
}

func complianceCheck(id string) (compCheck, bool) {
	for _, c := range complianceChecks {
		if c.ID == id {
			return c, true
		}
	}
	return compCheck{}, false
}

// ── The checks ────────────────────────────────────────────────────────────────

func evalFirmwareApproved(s *compSnapshot, d *db.Device, _ int) (string, string) {
	if d.IsDPC() {
		return db.ComplianceNA, "Stock firmware: its maker updates it"
	}
	if _, in := s.labs[d.ID]; in {
		return db.ComplianceNA, "In a lab session"
	}
	prodV, ok := s.prodFW[modelKey(d)]
	if !ok {
		return db.ComplianceNA, "No prod firmware is set for its model"
	}
	if d.BuildID == "" {
		return db.ComplianceUnknown, "Build not reported"
	}
	if compareVersionNames(d.BuildID, prodV) >= 0 {
		return db.CompliancePass, d.BuildID
	}
	return db.ComplianceFail, d.BuildID + "; prod is " + prodV
}

func evalLocked(s *compSnapshot, d *db.Device, _ int) (string, string) {
	switch s.lock[d.ID] {
	case "":
		return db.ComplianceNA, "No lock rule covers it"
	case "locked":
		return db.CompliancePass, "Locked"
	case "exited":
		return db.ComplianceFail, "Taken out of lock on the device"
	case "override":
		return db.ComplianceFail, "Unlocked by hand on its page"
	case "missing":
		return db.ComplianceFail, "The app it locks to is not installed"
	case "cant":
		return db.ComplianceFail, "Its client cannot lock the screen"
	}
	return db.ComplianceFail, "Not locked yet: the device has not picked up its rule"
}

func evalClientCurrent(s *compSnapshot, d *db.Device, _ int) (string, string) {
	b, ok := s.hosted[agentSlotFor(d)]
	if !ok {
		return db.ComplianceNA, "No client build is hosted for its signing key"
	}
	name, code := clientVersionOf(d)
	if code == 0 || !versionComparable(d, b.Package) {
		return db.ComplianceUnknown, "Client version not reported"
	}
	if code >= b.VersionCode {
		return db.CompliancePass, name
	}
	return db.ComplianceFail, name + "; " + b.Version + " is hosted"
}

func evalCheckedIn(s *compSnapshot, d *db.Device, hours int) (string, string) {
	if d.LastSeenAt.IsZero() {
		return db.ComplianceFail, "Never checked in"
	}
	ago := s.now.Sub(d.LastSeenAt)
	if ago <= time.Duration(hours)*time.Hour {
		return db.CompliancePass, "Seen " + compAgo(ago) + " ago"
	}
	return db.ComplianceFail, "Last seen " + compAgo(ago) + " ago"
}

func evalOwnKey(s *compSnapshot, d *db.Device, _ int) (string, string) {
	if s.ownKey[d.ID] {
		return db.CompliancePass, "Registered its own key"
	}
	return db.ComplianceFail, "Still uses the shared key"
}

func evalReleaseBuild(_ *compSnapshot, d *db.Device, _ int) (string, string) {
	bt := strings.ToLower(extraString(d.LatestExtra, "build_type"))
	if bt == "" {
		id := strings.ToLower(d.BuildID)
		switch {
		case strings.Contains(id, "userdebug"):
			bt = "userdebug"
		case strings.Contains(id, "-eng"):
			bt = "eng"
		case strings.Contains(id, "-user"):
			bt = "user"
		}
	}
	switch bt {
	case "":
		return db.ComplianceUnknown, "Build type not reported"
	case "user":
		return db.CompliancePass, "user build"
	}
	return db.ComplianceFail, bt + " build"
}

func evalUnknownSources(_ *compSnapshot, d *db.Device, _ int) (string, string) {
	apps, on, ok := unknownSourcesOf(d.LatestExtra)
	if !ok {
		return db.ComplianceUnknown, "Not reported by its client"
	}
	if !on {
		return db.CompliancePass, "None allowed"
	}
	if len(apps) == 0 {
		return db.ComplianceFail, "An app may install other apps"
	}
	return db.ComplianceFail, "Allowed for " + strings.Join(apps, ", ")
}

// mdmInstallers are our own clients. Installing apps is their job, so their holding
// "install unknown apps" is not a finding (26 Oct 2026: the firmware client listed itself,
// so every firmware device failed this check).
var mdmInstallers = map[string]bool{"com.aioapp.mdm": true, "aio.app.mdmclient.dpc": true}

// unknownSourcesOf reads whether any app other than the MDM may install apps, and which.
// When the client sends its list, the list alone decides: the firmware client also counts
// the old global install_non_market_apps setting, which Android ignores since 8.0. ok is
// false when the client reports nothing.
func unknownSourcesOf(extra json.RawMessage) (apps []string, on, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(extra, &m) != nil {
		return nil, false, false
	}
	var listed []string
	if raw, has := m["unknown_source_apps"]; has && json.Unmarshal(raw, &listed) == nil {
		for _, a := range listed {
			if !mdmInstallers[a] {
				apps = append(apps, a)
			}
		}
		return apps, len(apps) > 0, true
	}
	on, ok = extraTri(extra, "unknown_sources")
	return nil, on, ok
}

// evalSecurityPatch passes when extra.security_patch ("2026-08-05") is no more than
// days old. A patch level is a date, so it is measured to the day.
func evalSecurityPatch(s *compSnapshot, d *db.Device, days int) (string, string) {
	raw := strings.TrimSpace(extraString(d.LatestExtra, "security_patch"))
	if raw == "" || raw == "—" || raw == "null" {
		return db.ComplianceUnknown, "Not reported by its client"
	}
	at, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return db.ComplianceUnknown, "Patch level " + raw + " is not a date"
	}
	age := int(s.now.Sub(at).Hours() / 24)
	if age <= days {
		return db.CompliancePass, raw
	}
	return db.ComplianceFail, fmt.Sprintf("%s, %d days old", raw, age)
}

// evalVerifiedBoot reads extra.verified_boot (ro.boot.verifiedbootstate).
func evalVerifiedBoot(_ *compSnapshot, d *db.Device, _ int) (string, string) {
	state := strings.ToLower(strings.TrimSpace(extraString(d.LatestExtra, "verified_boot")))
	switch state {
	case "", "—", "null":
		return db.ComplianceUnknown, "Not reported by its client"
	case "green":
		return db.CompliancePass, "green"
	case "yellow":
		return db.ComplianceFail, "yellow: booted an image signed with its own key"
	case "orange":
		return db.ComplianceFail, "orange: the bootloader is unlocked"
	case "red":
		return db.ComplianceFail, "red: verification failed"
	}
	return db.ComplianceUnknown, "Unrecognised state " + state
}

// evalDeviceOwner applies to the standard (DPC) client only: the firmware client is a
// system app and MDM-lite is a host-app library, neither of which holds Device Owner.
func evalDeviceOwner(_ *compSnapshot, d *db.Device, _ int) (string, string) {
	if !d.IsDPC() || d.IsMDMLite() {
		return db.ComplianceNA, "Not a standard client"
	}
	on, ok := extraTri(d.LatestExtra, "device_owner")
	switch {
	case !ok:
		return db.ComplianceUnknown, "Not reported by its client"
	case !on:
		return db.ComplianceFail, "The client is not Device Owner"
	}
	return db.CompliancePass, "Device Owner"
}

// fwSELinuxMisread is the firmware client that reported selinux_enforcing false whenever it
// could not read the mode: SELinux.isSELinuxEnforced() answers false when
// /sys/fs/selinux/enforce is refused to system_app — and that refusal itself only happens
// when SELinux is enforcing. Found on AT070AA2600024 (17 Oct); 1.12.1 reads it properly.
const fwSELinuxMisread = 87

func evalSELinux(s *compSnapshot, d *db.Device, p int) (string, string) {
	if !d.IsDPC() {
		if _, code := clientVersionOf(d); code == fwSELinuxMisread {
			if on, ok := extraTri(d.LatestExtra, "selinux_enforcing"); ok && !on {
				return db.ComplianceUnknown, "Not read reliably by firmware client 1.12.0"
			}
		}
	}
	return evalExtraTrue("selinux_enforcing", "SELinux is permissive")(s, d, p)
}

// evalExtraFalse passes when the reported flag is false (adb_enabled, su_present…).
func evalExtraFalse(key, failText string) func(*compSnapshot, *db.Device, int) (string, string) {
	return func(_ *compSnapshot, d *db.Device, _ int) (string, string) {
		on, ok := extraTri(d.LatestExtra, key)
		switch {
		case !ok:
			return db.ComplianceUnknown, "Not reported by its client"
		case on:
			return db.ComplianceFail, failText
		}
		return db.CompliancePass, "Off"
	}
}

// evalExtraTrue passes when the reported flag is true (storage_encrypted, play_protect).
func evalExtraTrue(key, failText string) func(*compSnapshot, *db.Device, int) (string, string) {
	return func(_ *compSnapshot, d *db.Device, _ int) (string, string) {
		on, ok := extraTri(d.LatestExtra, key)
		switch {
		case !ok:
			return db.ComplianceUnknown, "Not reported by its client"
		case !on:
			return db.ComplianceFail, failText
		}
		return db.CompliancePass, "On"
	}
}

// extraTri reads a latest_extra flag that clients send as a bool, a number (a port,
// 0 for none) or a string. ok is false when the key is absent or unreadable.
func extraTri(raw json.RawMessage, key string) (on, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false, false
	}
	v, present := m[key]
	if !present || string(v) == "null" {
		return false, false
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return b, true
	}
	var n float64
	if json.Unmarshal(v, &n) == nil {
		return n != 0, true
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "on", "yes":
			return true, true
		case "false", "0", "off", "no", "":
			return false, true
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n != 0, true
		}
	}
	return false, false
}

func compAgo(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// ── Levels: baseline, then overrides ──────────────────────────────────────────

// compRules is the baseline and overrides, as stored.
type compRules struct {
	baseline  map[string]db.ComplianceBaselineRow
	overrides []db.ComplianceOverride
}

// baseSetting is the fleet's setting for a check: its row, else the registry default.
func (r compRules) baseSetting(c compCheck) db.ComplianceSetting {
	set := db.ComplianceSetting{Level: c.Level, Param: c.Param}
	if b, ok := r.baseline[c.ID]; ok {
		if b.Setting.Level != "" {
			set.Level = b.Setting.Level
		}
		if b.Setting.Param > 0 {
			set.Param = b.Setting.Param
		}
	}
	return set
}

// autoFix reports whether the baseline has auto-fix on for a check that has a fix.
func (r compRules) autoFix(c compCheck) bool {
	if c.Fix == "" {
		return false
	}
	if b, ok := r.baseline[c.ID]; ok && b.Setting.AutoFix != nil {
		return *b.Setting.AutoFix
	}
	return c.AutoFixDefault
}

// overrideCovers reports whether an override's scope includes the device.
func overrideCovers(o db.ComplianceOverride, d *db.Device, sc db.DeviceScope, inLab bool) bool {
	switch o.ScopeType {
	case "venue":
		return o.ScopeID != nil && d.RestaurantID != nil && *d.RestaurantID == *o.ScopeID
	case "group":
		if o.ScopeID == nil {
			return false
		}
		for _, g := range sc.Groups {
			if g == *o.ScopeID {
				return true
			}
		}
	case "role":
		return o.ScopeKey != "" && d.Class() == o.ScopeKey
	case "lab":
		return inLab
	}
	return false
}

// settingFor resolves one check for one device: the first covering override that names
// the check or carries a cap decides; otherwise the baseline. by is the override, nil for
// the baseline.
func (r compRules) settingFor(c compCheck, d *db.Device, sc db.DeviceScope, inLab bool) (db.ComplianceSetting, *db.ComplianceOverride) {
	set := r.baseSetting(c)
	for i := range r.overrides {
		o := &r.overrides[i]
		if !overrideCovers(*o, d, sc, inLab) {
			continue
		}
		if s, ok := o.Checks[c.ID]; ok {
			if s.Level != "" {
				set.Level = s.Level
			}
			if s.Param > 0 {
				set.Param = s.Param
			}
			return set, o
		}
		switch o.Cap {
		case db.ComplianceRecommended:
			if set.Level == db.ComplianceRequired {
				set.Level = db.ComplianceRecommended
			}
			return set, o
		case db.ComplianceOff:
			set.Level = db.ComplianceOff
			return set, o
		}
	}
	return set, nil
}

// ── Evaluation ────────────────────────────────────────────────────────────────

// compEvaluate scores every device in the snapshot against the rules. Checks that are
// off for a device have no result.
func compEvaluate(s *compSnapshot, r compRules) map[db.ComplianceResultKey]db.ComplianceResult {
	out := map[db.ComplianceResultKey]db.ComplianceResult{}
	for i := range s.devices {
		d := &s.devices[i]
		// Only devices at a venue are held to the rules (15 Oct): a unit in the office or
		// on a bench is not something a restaurant relies on, so it is not scored at all.
		if d.Hidden || d.Retired() || d.RestaurantID == nil {
			continue
		}
		_, inLab := s.labs[d.ID]
		sc := s.scopes[d.ID]
		for _, c := range complianceChecks {
			set, _ := r.settingFor(c, d, sc, inLab)
			if set.Level == db.ComplianceOff || set.Level == "" {
				continue
			}
			state, detail := c.eval(s, d, set.Param)
			out[db.ComplianceResultKey{DeviceID: d.ID, CheckID: c.ID}] = db.ComplianceResult{
				DeviceID: d.ID, CheckID: c.ID, State: state, Level: set.Level, Detail: detail}
		}
	}
	return out
}

// compCompliant is the per-device verdict: no required check fails.
func compCompliant(results map[db.ComplianceResultKey]db.ComplianceResult) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for k, r := range results {
		if _, seen := out[k.DeviceID]; !seen {
			out[k.DeviceID] = true
		}
		if r.Level == db.ComplianceRequired && r.State == db.ComplianceFail {
			out[k.DeviceID] = false
		}
	}
	return out
}

// loadCompSnapshot reads the fleet once for a pass.
func (h *Handler) loadCompSnapshot(ctx context.Context) (*compSnapshot, error) {
	s := &compSnapshot{now: time.Now(), prodFW: map[string]string{}, hosted: map[string]config.AgentAPKBuild{}, lock: map[uuid.UUID]string{}}
	_, kds, err := h.kioskSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range kds {
		s.devices = append(s.devices, k.Dev)
		s.lock[k.Dev.ID] = k.Status()
	}
	if s.scopes, err = h.db.DeviceScopes(ctx); err != nil {
		return nil, err
	}
	if s.labs, err = h.db.LabSessions(ctx); err != nil {
		return nil, err
	}
	if s.ownKey, err = h.db.OwnKeyDevices(ctx); err != nil {
		return nil, err
	}
	chans, err := h.db.ListFirmwareChannels(ctx)
	if err != nil {
		return nil, err
	}
	modes, err := h.db.FirmwareModes(ctx)
	if err != nil {
		return nil, err
	}
	for _, ch := range chans {
		if ch.ReleaseID != nil && ch.Version != "" && db.FirmwareModeOf(modes, ch.Model) == db.FirmwareProd {
			s.prodFW[ch.Model] = ch.Version
		}
	}
	for slot := range AgentAPKSlots {
		if b, ok := h.cfg.AgentAPKSlot(slot); ok && b.VersionCode > 0 {
			s.hosted[slot] = b
		}
	}
	return s, nil
}

func (h *Handler) loadCompRules(ctx context.Context) (compRules, error) {
	var r compRules
	var err error
	if r.baseline, err = h.db.ComplianceBaseline(ctx); err != nil {
		return r, err
	}
	r.overrides, err = h.db.ListComplianceOverrides(ctx)
	return r, err
}

// RunCompliance scores the fleet and writes what changed. Safe to call every minute.
func (h *Handler) RunCompliance(ctx context.Context) {
	if err := h.db.SeedComplianceDefaults(ctx); err != nil {
		log.Printf("[compliance] seed: %v", err)
	}
	s, err := h.loadCompSnapshot(ctx)
	if err != nil {
		log.Printf("[compliance] snapshot: %v", err)
		return
	}
	rules, err := h.loadCompRules(ctx)
	if err != nil {
		log.Printf("[compliance] rules: %v", err)
		return
	}
	stored, err := h.db.ListComplianceResults(ctx)
	if err != nil {
		log.Printf("[compliance] results: %v", err)
		return
	}
	fresh := compEvaluate(s, rules)
	var w db.ComplianceWrite
	moved := 0
	for k, r := range fresh {
		old, had := stored[k]
		if had && old.State == r.State && old.Level == r.Level && old.Detail == r.Detail {
			continue
		}
		u := db.ComplianceUpsert{ComplianceResult: r}
		if had {
			u.PrevState = old.State
		}
		if u.PrevState != r.State {
			moved++
		}
		w.Upsert = append(w.Upsert, u)
	}
	for k := range stored {
		if _, ok := fresh[k]; !ok {
			w.Remove = append(w.Remove, k)
		}
	}
	if err := h.db.ApplyCompliance(ctx, w); err != nil {
		log.Printf("[compliance] write: %v", err)
		return
	}
	if moved > 0 || len(w.Remove) > 0 {
		log.Printf("[compliance] %d result(s) changed state, %d removed", moved, len(w.Remove))
	}
	h.compAutoFix(ctx, s, rules, fresh)
	if s.now.Minute() == 0 {
		_ = h.db.PruneComplianceEvents(ctx, 400*24*time.Hour)
	}
}

// ── Auto-fix ──────────────────────────────────────────────────────────────────

const (
	compFixActor = "compliance" // db.SystemActorLabel: "Compliance auto-fix"
	compFixBatch = 20           // per tick
	compFixAgain = 6 * time.Hour
)

// compAutoFix acts on failures it is allowed to fix. Today that is wireless adb: a device
// that must have it off (required, so never a lab-session device, which the default
// override holds to recommendations; devices with no venue are not scored at all) and has it on is sent "adb off" — unless the
// last adb command was someone turning it on and that window has not run out, or "stays
// on". At most once per device every few hours, and logged as the Compliance auto-fix.
func (h *Handler) compAutoFix(ctx context.Context, s *compSnapshot, rules compRules, fresh map[db.ComplianceResultKey]db.ComplianceResult) {
	c, ok := complianceCheck("wireless_adb_off")
	if !ok || !rules.autoFix(c) {
		return
	}
	devs := map[uuid.UUID]*db.Device{}
	for i := range s.devices {
		devs[s.devices[i].ID] = &s.devices[i]
	}
	var cand []uuid.UUID
	for k, r := range fresh {
		if k.CheckID != c.ID || r.State != db.ComplianceFail || r.Level != db.ComplianceRequired {
			continue
		}
		d := devs[k.DeviceID]
		if d == nil {
			continue
		}
		if _, inLab := s.labs[d.ID]; inLab {
			continue
		}
		if _, code := clientVersionOf(d); !d.Supports("adb_tcp") || code < adbTCPMinClientCode {
			continue
		}
		cand = append(cand, d.ID)
	}
	if len(cand) == 0 {
		return
	}
	last, err := h.db.LastAdbTcpCommands(ctx, cand)
	if err != nil {
		log.Printf("[compliance] auto-fix adb: %v", err)
		return
	}
	payload, _ := json.Marshal(map[string]int{"port": 0, "hours": 0})
	sent := 0
	for _, id := range cand {
		if sent >= compFixBatch {
			break
		}
		if l, ok := last[id]; ok {
			if l.By == compFixActor && s.now.Sub(l.At) < compFixAgain {
				continue // asked recently; give the device time to answer
			}
			if l.Port > 0 && (l.Hours == 0 || s.now.Before(l.At.Add(time.Duration(l.Hours)*time.Hour))) {
				continue // someone turned it on on purpose and the window is still open
			}
		}
		cmd, err := h.db.CreateCommandBy(ctx, "adb_tcp", "", payload, "devices", []uuid.UUID{id}, compFixActor)
		if err != nil {
			log.Printf("[compliance] auto-fix adb %s: %v", devs[id].SerialNumber, err)
			continue
		}
		h.pushCommand(ctx, cmd, "devices", []uuid.UUID{id})
		log.Printf("[compliance] auto-fix: wireless adb off for %s", devs[id].SerialNumber)
		sent++
	}
}

// restates reports whether a check's detail only repeats its problem ("Developer options
// are on" under "Developer options on"), so a list can leave it out.
func restates(problem, detail string) bool {
	norm := func(x string) string {
		var out []string
		for _, w := range strings.Fields(strings.ToLower(x)) {
			if w != "is" && w != "are" && w != "the" {
				out = append(out, w)
			}
		}
		return strings.Join(out, " ")
	}
	return norm(problem) == norm(detail)
}
