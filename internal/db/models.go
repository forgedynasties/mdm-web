package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	prod "mdm/internal/product"
)

type Device struct {
	ID             uuid.UUID       `json:"id"`
	SerialNumber   string          `json:"serial_number"`
	BuildID        string          `json:"build_id"`
	BatteryPct     int             `json:"battery_pct"`
	LastSeenAt     time.Time       `json:"last_seen_at"`
	CreatedAt      time.Time       `json:"created_at"`
	PollIntervalMs int             `json:"poll_interval_ms"`
	KioskEnabled   bool            `json:"kiosk_enabled"`
	KioskPackage   string          `json:"kiosk_package"`
	LatestExtra    json.RawMessage `json:"latest_extra,omitempty"`
	Hidden         bool            `json:"hidden"`
	// Product is the hardware category the device reports (e.g. "t7", "kiosk27"), or
	// "" if it never reported one. Use ProductLabel()/Caps() rather than reading this
	// raw so empty/unknown resolves to the default product. See internal/product.
	Product string `json:"product"`
	// DischargeTotalPct is the lifetime cumulative percent of battery capacity
	// discharged (never resets). BatteryCycles() renders it as equivalent full cycles.
	DischargeTotalPct int64 `json:"discharge_total_pct"`
	// DischargeLegacyPct is the prior range-based estimate, kept during the transition
	// to the corrected discharge accounting so the two can be compared. Zero once the
	// difference no longer matters. Rendered by BatteryCyclesLegacy().
	DischargeLegacyPct int64 `json:"discharge_legacy_pct"`
	// DischargeBackfilled is false until the one-time history seed has run for this
	// device. While false the cycle count is not yet meaningful (show "—", not 0.0).
	DischargeBackfilled bool `json:"discharge_backfilled"`
	// RestaurantID is the venue the device physically lives in (nil = lab/bench unit).
	// RestaurantName is joined for display. A device is "deployed" iff it has a restaurant.
	RestaurantID   *uuid.UUID `json:"restaurant_id,omitempty"`
	RestaurantName string     `json:"restaurant_name,omitempty"`
	// Nickname is the operator-given name for this device (device_nicknames). It is
	// only populated by the search queries, which match on it — everywhere else the
	// dashboard loads nicknames in one batch via GetNicknames.
	Nickname string `json:"nickname,omitempty"`
	// DeployedEffective is true when the device is live in a restaurant (RestaurantID set);
	// false = lab/bench unit. There is no separate deployed flag — assignment is the signal.
	DeployedEffective bool `json:"deployed_effective"`
	// AgentKind is how the device is managed: prod.KindFirmware (system-app client,
	// shared key) or prod.KindDPC (Device-Owner agent, per-device key). Persisted at
	// enrollment / check-in; use IsDPC() in templates.
	AgentKind string `json:"agent_kind"`
	// DeviceClass is the stored form factor (tablet/panel/kiosk/mpos/pos/other) or ""
	// when not set; Class() resolves "" to the product's default for firmware devices.
	DeviceClass string `json:"device_class"`
	// Capabilities is what the agent advertised on its last keyframe (DPC agents) or
	// nil for firmware clients that do not report one; CapSet()/Supports() apply the
	// product defaults in that case. CapabilitiesDegraded lists features that work
	// with limits (on-device consent, reduced scope).
	Capabilities         []string `json:"capabilities"`
	CapabilitiesDegraded []string `json:"capabilities_degraded"`
	// EnrollmentStatus: "auto" (firmware, first check-in), "enrolled" (via profile),
	// "retired", "wiped".
	EnrollmentStatus string     `json:"enrollment_status"`
	EnrolledAt       time.Time  `json:"enrolled_at"`
	KeyRotatedAt     *time.Time `json:"key_rotated_at,omitempty"`
	// EnrolledBy is the username that enrolled the device through AIO Enroll ("" = not recorded).
	EnrolledBy string `json:"enrolled_by,omitempty"`
	// OnboardedAt is nil while the device waits in the onboarding inbox (nobody has
	// confirmed its site/group/class yet).
	OnboardedAt *time.Time `json:"onboarded_at,omitempty"`
}

// Enrollment status values (devices.enrollment_status).
const (
	EnrollAuto     = "auto"
	EnrollEnrolled = "enrolled"
	EnrollRetired  = "retired"
	EnrollWiped    = "wiped"
	// Handed back by an unenroll command: the agent dropped Device Owner and stopped
	// reporting, and the device kept its data. Out of the fleet like retired and wiped,
	// but it says which of the three happened — and only this one can be undone by
	// enrolling the device again from a factory reset.
	EnrollUnenrolled = "unenrolled"
)

// IsDPC reports whether the device runs the Device-Owner DPC agent.
func (d Device) IsDPC() bool { return d.AgentKind == prod.KindDPC }

// SerialCorrupt reports a T7 whose serial read corrupt and which identifies as
// msm-<ANDROID_ID> instead (client 1.4.7+; the raw value it read is in latest_extra as
// serial_raw). The fleet lists these so they can be found and re-flashed.
func (d Device) SerialCorrupt() bool { return strings.HasPrefix(d.SerialNumber, "msm-") }

// IsMDMLite reports whether a stock device is reported by the MDM-lite library
// embedded in an app, rather than by the DPC agent. See prod.AgentTypeMDMLite. Such a
// device never holds a live connection, so it shows Reporting / Not reporting (from
// check-in recency) where others show Online / Offline.
func (d Device) IsMDMLite() bool {
	var probe struct {
		AgentType string `json:"agent_type"`
	}
	return d.IsDPC() && json.Unmarshal(d.LatestExtra, &probe) == nil && probe.AgentType == prod.AgentTypeMDMLite
}

// Class is the device's category: the stored class when set, else the category its
// product belongs to (T7 → t7, Kiosk 18/22/27 → kiosk), else "" (a stock device
// whose type nobody has assigned and whose model told us nothing).
func (d Device) Class() string {
	if d.DeviceClass != "" {
		return d.DeviceClass
	}
	p, _ := prod.Resolve(d.Product)
	return p.Class
}

// productKeysForClass lists the product keys whose catalog category is class, so a
// firmware device that stores no class still matches its category. The legacy empty
// product is the default product, so it rides along with that product's class.
func productKeysForClass(class string) []string {
	var keys []string
	for _, p := range prod.All() {
		if p.Class == class {
			keys = append(keys, p.Key)
			if p.Key == prod.DefaultKey {
				keys = append(keys, "") // legacy pre-product rows are the default product
			}
		}
	}
	return keys
}

// derivedClassSQL is the SQL form of Device.Class(): the stored class, else the
// category of the device's product, built from the same catalog so the composition
// strip can never disagree with the class filter.
func derivedClassSQL() string {
	var b strings.Builder
	b.WriteString("COALESCE(NULLIF(device_class, ''), CASE")
	for _, c := range prod.Classes() {
		keys := productKeysForClass(c)
		if len(keys) == 0 {
			continue
		}
		quoted := make([]string, len(keys))
		for i, k := range keys {
			quoted[i] = "'" + k + "'" // catalog keys are our own constants, never user input
		}
		fmt.Fprintf(&b, " WHEN product IN (%s) THEN '%s'", strings.Join(quoted, ", "), c)
	}
	b.WriteString(" ELSE '' END)")
	return b.String()
}

// ClassLabel is the display label for Class() ("—" when unset).
func (d Device) ClassLabel() string { return prod.ClassLabel(d.Class()) }

// KindLabel is the display label for the agent kind.
func (d Device) KindLabel() string {
	switch {
	case d.IsMDMLite():
		return "MDM Lite"
	case d.IsDPC():
		return "Standard MDM"
	}
	return "Firmware MDM" // the system-app client on our own firmware
}

// Retired reports whether the device has left the fleet (retired, wiped or unenrolled).
func (d Device) Retired() bool {
	return d.EnrollmentStatus == EnrollRetired || d.EnrollmentStatus == EnrollWiped ||
		d.EnrollmentStatus == EnrollUnenrolled
}

// NeedsOnboarding is true while the device sits in the onboarding inbox.
func (d Device) NeedsOnboarding() bool { return d.OnboardedAt == nil && !d.Retired() }

// CapSet is the effective capability set: what the agent advertised, or the product
// defaults for a firmware device that never reported one. Template-friendly (a
// missing key indexes to false).
//
// For a firmware device the advertised list is MERGED onto the defaults rather than
// replacing them. Replacement is the dangerous direction: the defaults are a known-good
// baseline from the catalog, so a capability missing from a client's list — a bug, or a
// build older than the capability — would silently withdraw that command from every
// firmware device at once, and the first symptom would be a button that no longer works
// on a fleet-wide rollout. Merging means a report can only ever add.
//
// A stock device is the opposite case and keeps the replacing behaviour: it has no
// defaults to merge with (DefaultCaps returns nil for non-firmware products), so what it
// advertises is the whole truth about it. Nothing advertises capabilities there except
// the DPC agent, which lists what it can do precisely because the server cannot know.
//
// The cost of merging is that a firmware build can no longer use this to drop a
// capability it genuinely lacks. That is deliberate: which capabilities a firmware image
// has is a property of the image, so it belongs in DefaultCaps keyed by product (or the
// build line), where it is reviewable, not in per-device reporting where a single bad
// build decides it for the fleet.
func (d Device) CapSet() map[string]bool {
	if d.IsDPC() {
		if len(d.Capabilities) == 0 {
			return map[string]bool{}
		}
		return prod.CapSet(d.Capabilities)
	}
	out := prod.CapSet(prod.DefaultCaps(d.Product))
	for _, c := range d.Capabilities {
		out[c] = true
	}
	// Factory reset is the client's to offer, not the image's: 1.10.0+ says so.
	if prod.DefaultCaps(d.Product) != nil && extraTrue(d.LatestExtra, "factory_reset") {
		out[prod.CapWipe] = true
	}
	// So are lost mode and Wi-Fi config: 1.12.0+ says so the same way.
	if prod.DefaultCaps(d.Product) != nil {
		for flag, c := range prod.FirmwareCapFlags {
			if extraTrue(d.LatestExtra, flag) {
				out[c] = true
			}
		}
	}
	return out
}

// firmwareCapExtraKeys are the latest_extra keys CapSet reads, for queries that load a
// device without its whole extra.
func firmwareCapExtraKeys() []string {
	keys := []string{"factory_reset"}
	for flag := range prod.FirmwareCapFlags {
		keys = append(keys, flag)
	}
	return keys
}

// extraTrue reports whether a latest_extra key holds JSON true.
func extraTrue(extra json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(extra, &m) != nil {
		return false
	}
	return string(m[key]) == "true"
}

// Supports reports whether the device can run a command type (see prod.CommandNeeds).
// Types with no requirement are always supported. Degraded capabilities count as
// supported; check Degraded() to add a "works with limits" note.
func (d Device) Supports(cmdType string) bool {
	need := prod.CommandNeeds(cmdType)
	if need == "" {
		return true
	}
	return d.CapSet()[need] || d.Degraded(cmdType)
}

// Degraded reports whether the command works on this device only with limits (the
// agent listed its capability as degraded).
func (d Device) Degraded(cmdType string) bool {
	need := prod.CommandNeeds(cmdType)
	if need == "" {
		return false
	}
	return prod.CapSet(d.CapabilitiesDegraded)[need]
}

// ProductLabel is the human display label for the device's product ("T7", "Kiosk 27"),
// resolving empty/unknown to the default product. Safe to call from templates.
func (d Device) ProductLabel() string { return prod.Label(d.Product) }

// ProductKey is the canonical product key ("t7", "kiosk27"), default-substituted so
// legacy devices with an empty product still report as the default. Use for grouping.
func (d Device) ProductKey() string {
	p, _ := prod.Resolve(d.Product)
	return p.Key
}

// Caps returns the hardware capabilities of the device's product. Gate wlc/charging
// UI and telemetry on these instead of checking whether a telemetry key is present.
func (d Device) Caps() prod.Caps {
	c := prod.CapsForDevice(d.Product, d.DeviceClass)
	// MDM-lite runs on arbitrary stock hardware whose product key says nothing, and
	// it sends battery_present only when a pack exists; without it the device is on
	// mains (TV boxes), not a phone at 0%.
	if d.IsMDMLite() && !d.batteryPresent() {
		c.HasBattery, c.HasCharging, c.HasWLC = false, false, false
	}
	return c
}

func (d Device) batteryPresent() bool {
	var probe struct {
		BatteryPresent bool `json:"battery_present"`
	}
	return json.Unmarshal(d.LatestExtra, &probe) == nil && probe.BatteryPresent
}

// HasWLC / HasCharging / HasBattery are template-friendly capability shortcuts.
func (d Device) HasWLC() bool      { return d.Caps().HasWLC }
func (d Device) HasCharging() bool { return d.Caps().HasCharging }
func (d Device) HasBattery() bool  { return d.Caps().HasBattery }

// hasBatteryD is the SQL form of HasBattery for the devices table aliased "d". Every
// battery-percentage count and filter must AND it in: battery-less devices (kiosks,
// dongles) still carry a latest_battery_pct, usually 0, and would read as "low".
var hasBatteryD = "(" + prod.BatteryPredicateSQL("d") + " AND NOT (" + mdmLiteNoBatterySQL + "))"

// mdmLiteNoBatterySQL is the SQL twin of Caps' MDM-lite rule: an MDM-lite device
// that has not reported battery_present has no battery.
const mdmLiteNoBatterySQL = "d.agent_kind = '" + prod.KindDPC + "' AND COALESCE(d.latest_extra->>'agent_type', '') = '" + prod.AgentTypeMDMLite + "'" +
	" AND COALESCE(d.latest_extra->>'battery_present', '') <> 'true'"

// BatteryCycles converts the lifetime cumulative discharge into equivalent full
// battery cycles (1 cycle = 100% of capacity discharged). 250% total => 2.5 cycles.
func (d Device) BatteryCycles() float64 {
	return float64(d.DischargeTotalPct) / 100
}

// BatteryCyclesLegacy renders the prior range-based estimate as cycles, for
// side-by-side comparison during the transition to corrected discharge accounting.
func (d Device) BatteryCyclesLegacy() float64 {
	return float64(d.DischargeLegacyPct) / 100
}

// Restaurant is a real venue a device physically lives in. It owns the venue semantics
// (service window, health, AI bucketing). A device assigned to a restaurant is "deployed";
// unassigned = lab/bench. See docs/release-versioning-and-restaurants.md.
type Restaurant struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Latitude    *float64  `json:"latitude"`
	Longitude   *float64  `json:"longitude"`
	Timezone    string    `json:"timezone"`
	Notes       string    `json:"notes"`
	CreatedAt   time.Time `json:"created_at"`
	DeviceCount int       `json:"device_count"` // populated by List/Get
	// ContactEmail is where the venue's monthly device-security report goes ('' = none).
	ContactEmail string `json:"contact_email"`
}

// DefaultKioskFeatures shows system info (battery/wifi) but blocks home, recents,
// notifications, global actions and keyguard — matching Android LOCK_TASK_FEATURE_SYSTEM_INFO.
const DefaultKioskFeatures = 1

type DeviceConfig struct {
	DeviceID      uuid.UUID `json:"device_id"`
	KioskEnabled  bool      `json:"kiosk_enabled"`
	KioskPackage  string    `json:"kiosk_package"`
	KioskFeatures int       `json:"kiosk_features"`
	// Kiosk mode: "app" (single locked app — the classic pair above), "multi"
	// (locked app plus KioskPackages also allowed in lock-task), or "browser"
	// (locked browser showing KioskURL, navigation limited to KioskURLAllow prefixes).
	KioskMode     string   `json:"kiosk_mode"`
	KioskPackages []string `json:"kiosk_packages"`
	KioskURL      string   `json:"kiosk_url"`
	KioskURLAllow []string `json:"kiosk_url_allow"`
	// WlcChargingEnabled controls wireless-charging on the pad (client writes the
	// customer_gpio line). Default true.
	WlcChargingEnabled bool `json:"wlc_charging_enabled"`
	// ScreenBrightness is the level the client sets (1-255), nil when the dashboard does
	// not manage it; ScreenBrightnessLocked holds it there on the device.
	ScreenBrightness       *int `json:"screen_brightness"`
	ScreenBrightnessLocked bool `json:"screen_brightness_locked"`
	// Who set this device's own level and when (empty / nil on the default).
	ScreenBrightnessSetBy string     `json:"-"`
	ScreenBrightnessSetAt *time.Time `json:"-"`
	// DeviceClass is the device's derived class, read with the row (not stored here): it
	// decides whether the brightness default applies.
	DeviceClass string `json:"-"`
	// Offline kiosk-exit (the on-device exit PIN). The per-device rotating code (TOTP seed)
	// was removed on 17 Oct; the offline_exit_seed column is left in place, unused.
	OfflineExitEnabled bool   `json:"offline_exit_enabled"`
	OfflineExitRelock  string `json:"offline_exit_relock"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Summary struct {
	Total          int
	RecentlyActive int // checked in within last 3 minutes
	LowBattery     int // battery < 20%
	UniqueBuilds   int
	KioskCount     int // devices currently in kiosk mode
}

type Checkin struct {
	ID         uuid.UUID       `json:"id"`
	DeviceID   uuid.UUID       `json:"device_id"`
	BatteryPct int             `json:"battery_pct"`
	BuildID    string          `json:"build_id"`
	Extra      json.RawMessage `json:"extra"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Group struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	DeviceCount int       `json:"device_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type OTAPackage struct {
	ID              int       `json:"id"`
	ReleaseID       int       `json:"release_id"`
	Type            string    `json:"type"` // "full" or "incremental"
	TargetBuildID   string    `json:"target_build_id"`
	SourceBuildID   string    `json:"source_build_id"` // incremental only
	ReleaseDate     time.Time `json:"release_date"`
	UpdateURL       string    `json:"update_url"`
	Changelog       string    `json:"changelog"`
	Status          string    `json:"status"` // "active" or "yanked"
	Wipe            bool      `json:"wipe"`   // installing it factory-resets the device
	CreatedAt       time.Time `json:"created_at"`
	DeploymentCount int       `json:"deployment_count,omitempty"` // populated by ListOTAPackages
	// PayloadSize is the package's size in bytes where known — filled when the package
	// is made, or learned later from the host (see LearnPackageSize). It is what turns a
	// device's percent into a figure a person can read.
	PayloadSize int64 `json:"payload_size,omitempty"`
}

// QFILPackage is a Qualcomm Flash Image Loader bundle attached to a release — the
// artifact the test team uses to flash a device from scratch over USB. It carries
// only a URL to the externally hosted bundle plus a label/notes; it is never
// deployed OTA, so it has no build/deployment plumbing of its own.
type QFILPackage struct {
	ID        int       `json:"id"`
	ReleaseID int       `json:"release_id"`
	Label     string    `json:"label"`
	URL       string    `json:"url"`
	Notes     string    `json:"notes"`
	AddedBy   string    `json:"added_by"`
	Status    string    `json:"status"` // "active" or "removed"
	CreatedAt time.Time `json:"created_at"`
}

// Release groups the packages for one target build version under a single
// lifecycle. Version equals the target build id.
type Release struct {
	ID                  int        `json:"id"`
	Version             string     `json:"version"`
	Product             string     `json:"product"` // hardware product this release targets (t7/kiosk18/22/27)
	Name                string     `json:"name"`
	Changelog           string     `json:"changelog"`
	Status              string     `json:"status"`          // "draft" | "published"
	Hidden              bool       `json:"hidden"`          // hidden from the main releases list (irrelevant)
	CreatedAt           time.Time  `json:"created_at"`
	PublishedAt         *time.Time `json:"published_at"`
	ParentReleaseID     *int       `json:"parent_release_id"`       // set for branch builds — the release this forked from
	IsBranch            bool       `json:"is_branch"`               // off-mainline temporary test build
	IsDev               bool       `json:"is_dev"`                  // work in progress: only devs/admins see it
	// Which artifacts a release can actually deliver, populated by ListDeployableReleases.
	// A full image reaches any device (and is the only thing the legacy path can use);
	// an incremental only reaches devices on its exact source build.
	HasFull      bool `json:"has_full,omitempty"`
	Incrementals int  `json:"incrementals,omitempty"`
	PackageCount        int        `json:"package_count,omitempty"` // populated by ListReleases
	DeployCount         int        `json:"deploy_count,omitempty"`  // populated by ListReleases
	MergedFromReleaseID *int       `json:"merged_from_release_id"`  // mainline node: the branch it absorbed on merge
	MergedIntoReleaseID *int       `json:"merged_into_release_id"`  // branch: the mainline release it merged into (terminal)
	MergedAt            *time.Time `json:"merged_at"`
	MergedBy            string     `json:"merged_by"`
}

// FleetVersion is one release version actually reported by devices in the field, with the
// devices on it and a link to the managed release (if any). Powers release tracking.
type FleetVersion struct {
	Version       string   `json:"version"`
	DeviceCount   int      `json:"device_count"`
	Serials       []string `json:"serials"`
	ReleaseID     *int     `json:"release_id"`     // non-nil when this version is a managed release
	ReleaseStatus string   `json:"release_status"` // "" when unmanaged
	ReleaseHidden bool     `json:"release_hidden"`
	Product       string   `json:"product"` // product of the devices reporting this build (empty->t7)
}

type Update struct {
	ID              int            `json:"id"`
	OtaPackageID    int            `json:"ota_package_id"`
	ReleaseID       int            `json:"release_id"`
	RebootBehavior  string         `json:"reboot_behavior"` // "immediate", "scheduled", "manual"
	ScheduledTime   *time.Time     `json:"scheduled_time"`
	Status          string         `json:"status"` // "pending", "active", "complete"
	CreatedAt       time.Time      `json:"created_at"`
	CreatedBy       string         `json:"created_by"` // username that pushed it ("" for legacy rows)
	OtaPackage      *OTAPackage    `json:"ota_package,omitempty"`
	Release         *Release       `json:"release,omitempty"`
	Targets         []UpdateTarget `json:"targets,omitempty"`
	DeviceTotal       int          `json:"device_total,omitempty"`       // populated by ListDeploymentsByPackage
	DeviceInstalled   int          `json:"device_installed,omitempty"`   // populated by ListDeploymentsByPackage
	DeviceDownloading int          `json:"device_downloading,omitempty"` // populated by ListDeployments
	DeviceInstalling  int          `json:"device_installing,omitempty"`  // installing/verifying/finalizing
	DeviceAwaiting    int          `json:"device_awaiting,omitempty"`    // applied, waiting on its reboot
	DeviceFailed      int          `json:"device_failed,omitempty"`      // populated by ListDeployments
	DeviceNotBack     int          `json:"device_not_back,omitempty"`    // of DeviceAwaiting: rebooted 15+ min ago, not back (ListDeployments)
	Product           string       `json:"product,omitempty"`            // release product (populated by ListDeployments)
	ReleaseIsDev      bool         `json:"release_is_dev,omitempty"`     // release is dev-only (populated by ListDeployments)
	DeviceStatus      string       `json:"device_status,omitempty"`      // populated by ResolveUpdateForDevice
	DeviceRebootSentAt *time.Time  `json:"-"`                             // populated by ResolveUpdateForDevice
}

type UpdateTarget struct {
	UpdateID     int       `json:"update_id"`
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"` // joined from devices
	BuildID      string    `json:"build_id"`      // current device build
	Status       string    `json:"status"`        // "pending", "downloading", "installing", "installed"
	ErrorCode    string    `json:"error_code"`    // device-reported code when status == "failed"
	UpdatedAt    time.Time `json:"updated_at"`    // when this row last changed state

	StartedAt    *time.Time `json:"started_at"`     // when the device began downloading (nil if not yet)
	CompletedAt  *time.Time `json:"completed_at"`   // when the device reported installed (nil if not yet)
	RebootSentAt *time.Time `json:"reboot_sent_at"` // when the applying reboot was pushed to the device (nil if not yet)
	LastSeenAt   time.Time  `json:"last_seen_at"`   // the device's own last check-in, joined from devices

	VenueID       *uuid.UUID `json:"venue_id,omitempty"` // the device's restaurant, joined
	Venue         string     `json:"venue,omitempty"`
	SourceBuildID string     `json:"source_build_id,omitempty"` // the build it was on when offered
	ForceFull     bool       `json:"force_full,omitempty"`      // pinned to the full image
}

// GetLastSeenAt lets the dashboard ask a row whether its device is still there.
func (t UpdateTarget) GetLastSeenAt() time.Time { return t.LastSeenAt }

// DurationSeconds returns how long this device took to update (download → installed),
// or -1 if it hasn't both started and completed.
func (t UpdateTarget) DurationSeconds() int {
	if t.StartedAt == nil || t.CompletedAt == nil {
		return -1
	}
	return int(t.CompletedAt.Sub(*t.StartedAt).Seconds())
}

type Command struct {
	ID          uuid.UUID       `json:"id"`
	Type        string          `json:"type"`
	ApkURL      string          `json:"apk_url"`
	Payload     json.RawMessage `json:"payload"`
	TargetType  string          `json:"target_type"`
	CreatedBy   string          `json:"created_by"` // display name/email snapshotted at creation; "" for system-initiated
	CreatedAt   time.Time       `json:"created_at"`
}

type ExportRow struct {
	SerialNumber string          `json:"serial_number"`
	BatteryPct   int             `json:"battery_pct"`
	BuildID      string          `json:"build_id"`
	Extra        json.RawMessage `json:"extra"`
	Timestamp    time.Time       `json:"timestamp"`
	// SampleAt is when the check-in that supplied this row's values was actually
	// recorded. Equal to Timestamp for raw rows; for interval rows it is the
	// check-in chosen for the bucket; for cycles rows it is the carried-forward
	// check-in (zero when the row is Empty). It is what a chart point shows.
	SampleAt     time.Time       `json:"sample_at"`
	LastSeenAt   time.Time       `json:"last_seen_at"`
	// Empty marks a cycles-mode grid row with no check-in within one interval
	// of the mark — data fields are zero values and should export as blanks.
	Empty bool `json:"empty,omitempty"`
}

type CommandDelivery struct {
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"`
	Status       string    `json:"status"`
	Progress     *int      `json:"progress,omitempty"` // 0-100 while an install is downloading; nil otherwise
	UpdatedAt    time.Time `json:"updated_at"`
	Output       string    `json:"output"`
	OutputBytes  int       `json:"output_bytes"` // size of the stored output (a log bundle's is not loaded)
	LastSeenAt   time.Time `json:"last_seen_at"`
	Online       bool      `json:"online"` // set by the handler from the ws.Hub, not the DB
}

// DeviceFilter holds optional filter parameters for device listing.
type DeviceFilter struct {
	Search              string    // search by serial substring
	GroupID             uuid.UUID // filter by group membership (uuid.Nil = no filter)
	ExcludeGroupID      uuid.UUID // exclude devices already in this group (uuid.Nil = no filter)
	ExcludeRestaurantID uuid.UUID // exclude devices already at this venue (uuid.Nil = no filter)
	RestaurantID        uuid.UUID // filter by restaurant/venue (uuid.Nil = no filter)
	ProductionID        uuid.UUID // filter by production (uuid.Nil = no filter)
	Product             string    // filter by hardware product key ("t7", "kiosk27", ...), or "" (no filter)
	Online              string    // "online", "offline", or "" (no filter)
	BuildID             string    // exact build_id match, or "" (no filter)
	Battery             string    // "low" (<20%), "mid" (20-49%), "ok" (>=50%), or "" (no filter)
	Kiosk               string    // "enabled" (kiosk on), "disabled" (kiosk off), "lost" (in lost mode), or "" (no filter)
	Charging            string    // "yes" (charging), "no" (not charging), or "" (no filter)
	RAM                 string    // "high" (>=80% used), "warn" (>=60%), or "" (no filter)
	Temp                string    // "hot" (danger level), "warm" (warn level or above), or "" (no filter)
	Timezone            string    // exact timezone match (latest_extra->>'timezone'), or "" (no filter)
	Hidden              string    // "include" (show all), "only" (hidden only), or "" (active only)
	AgentKind           string    // "firmware" | "dpc" (DPC agent) | "mdm-lite" | "" (no filter)
	Class               string    // device class; firmware devices match on their product default. "" = no filter
	Onboarding          string    // "pending" (in the inbox), "done", or "" (no filter)
	EnrolledBy          string    // username that enrolled the device via AIO Enroll, or "" (no filter)
	Lifecycle           string    // "retired" (retired/wiped only), "all", or "" (active only)
	Hygiene             string    // a clean-up job from the Overview (see hygieneWhere), or ""
	ActiveThresholdSecs int       // legacy: seconds before a device is considered offline (unused for online/offline now)
	// Connected is the set of device IDs with a live WebSocket, used to compute
	// online/offline from real presence rather than check-in recency. Supplied by the
	// handler from ws.Hub; nil means "nobody connected" (all offline).
	Connected []uuid.UUID
	// OnlyIDs, when non-nil, restricts results to these device ids (an empty
	// non-nil slice matches nothing). Set from the user's access policy when they
	// may only see part of the fleet.
	OnlyIDs []uuid.UUID
}

// ── Productions ───────────────────────────────────────────────────────────────

const base36Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// EncodeBatch encodes a month (1-12) and 2-digit year into a 2-char base36 batch code.
// Formula: month*100 + year → base36(2 chars).
// e.g. April 2026 → 4*100+26 = 426 → "BU"
func EncodeBatch(month, year int) string {
	val := month*100 + (year % 100)
	return string([]byte{base36Chars[val/36], base36Chars[val%36]})
}

// DecodeBatch decodes a 2-char base36 batch code into month and 2-digit year.
func DecodeBatch(batch string) (month, year int) {
	hi := strings.IndexByte(base36Chars, batch[0])
	lo := strings.IndexByte(base36Chars, batch[1])
	val := hi*36 + lo
	return val / 100, val % 100
}

type Production struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	ProductCode   string    `json:"product_code"`
	ModelCode     string    `json:"model_code"`
	Variant       string    `json:"variant"`
	SKU           string    `json:"sku"`
	Batch         string    `json:"batch"`
	BatchMonth    int       `json:"batch_month"`
	BatchYear     int       `json:"batch_year"`
	StartSequence int       `json:"start_sequence"`
	EndSequence   int       `json:"end_sequence"`
	Notes         string    `json:"notes"`
	Model         string    `json:"model"` // hardware model (product key) the run built; "" = not linked
	CreatedAt     time.Time `json:"created_at"`
	// Computed stats
	Total         int `json:"total"`
	EverConnected int `json:"ever_connected"`
	Online        int `json:"online"`
}

func (p *Production) SerialPrefix() string {
	return p.ProductCode + p.ModelCode + p.Variant + p.SKU + p.Batch
}

func (p *Production) FirstSerial() string {
	return fmt.Sprintf("%s%05d", p.SerialPrefix(), p.StartSequence)
}

func (p *Production) LastSerial() string {
	return fmt.Sprintf("%s%05d", p.SerialPrefix(), p.EndSequence)
}
