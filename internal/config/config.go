package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mdm/internal/otaconfig"
)

// writeFileAtomic writes data to a temp file in the same directory and renames it
// over path (rename is atomic on the same filesystem), so a crash mid-write can't
// leave a truncated/corrupt config file — which would fail Load and brick startup.
// CheckWritable verifies the config file's directory accepts writes, by creating and
// removing a probe file next to it. Every settings change persists through
// writeFileAtomic and its error is mostly ignored by callers, so an unwritable
// directory (e.g. a data volume still owned by root after the image switched to an
// unprivileged user) would otherwise fail silently: settings apply in memory and
// vanish on restart, background jobs that persist a cursor redo the same work.
func (c *Config) CheckWritable() error {
	probe := c.path + ".probe"
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	// 0600: this file holds secrets (anthropic_api_key, alert webhook URLs), so it must
	// not be world/group-readable by other users on the host.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type ExtraColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type Config struct {
	ExtraColumns    []ExtraColumn `json:"extra_columns"`
	LegacyCheckinOn bool          `json:"legacy_checkin"`
	// Build IDs that identify legacy (WebSocket-incapable) firmware. Devices on these
	// builds only HTTP check-in, so they never hold a live WS and are shown Offline with a
	// "Legacy device" tag. Seeded with a default; admins can add/remove via Settings.
	LegacyBuildIDs     []string `json:"legacy_build_ids"`
	CheckinIntervalSec int      `json:"checkin_interval_sec"`
	// Stored as "disabled" so an existing config file (without these keys)
	// defaults to enabled.
	ShellDisabledFlag  bool `json:"shell_disabled"`
	RemoteDisabledFlag bool `json:"remote_disabled"`

	// Command governance.
	CommandExpirySecVal int      `json:"command_expiry_sec"` // 0 -> default 300
	MaxTargetsVal       int      `json:"max_targets"`        // 0 -> unlimited
	OperatorDeniedCmds  []string `json:"operator_denied_cmds"`
	RequireReasonFlag   bool     `json:"require_reason"` // require a reason for destructive commands

	// DPC agent APK for QR cold-provisioning (PROVISIONING_DEVICE_ADMIN_PACKAGE_
	// DOWNLOAD_LOCATION + SIGNATURE_CHECKSUM). Env AGENT_APK_URL / AGENT_APK_CHECKSUM
	// are the fallback when unset here, so existing deployments keep working.
	AgentAPKURLVal      string `json:"agent_apk_url"`
	AgentAPKChecksumVal string `json:"agent_apk_checksum"`
	// An agent APK uploaded through Settings and served by this server at
	// /agent/aio-mdm-dpc.apk. HostedSHA is the URL-safe base64 SHA-256 of the file
	// (PROVISIONING_DEVICE_ADMIN_PACKAGE_CHECKSUM); HostedName/Size/At describe it.
	AgentAPKHostedSHA  string    `json:"agent_apk_hosted_sha"`
	AgentAPKHostedName string    `json:"agent_apk_hosted_name"`
	AgentAPKHostedSize int64     `json:"agent_apk_hosted_size"`
	AgentAPKHostedAt   time.Time `json:"agent_apk_hosted_at"`
	// Parsed out of the hosted APK at upload: what the agent update actually is.
	// The version code is what decides whether a device is behind; the hex digest
	// is what the agent verifies the download against (the base64 one above is for
	// Android's provisioning extras).
	AgentAPKHostedPackage     string `json:"agent_apk_hosted_package"`
	AgentAPKHostedVersion     string `json:"agent_apk_hosted_version"`
	AgentAPKHostedVersionCode int64  `json:"agent_apk_hosted_version_code"`
	AgentAPKHostedSHA256Hex   string `json:"agent_apk_hosted_sha256_hex"`
	// Agent APKs beyond the DPC one, keyed by slot (see dashboard.AgentAPKSlots).
	// The firmware client is signed with the platform key of its build variant, so
	// there is one slot per variant and a device is only ever offered the build that
	// matches its own signing keys. The fields above remain the "dpc" slot, so an
	// existing config file keeps working untouched.
	AgentAPKSlotMap map[string]AgentAPKBuild `json:"agent_apk_slots,omitempty"`

	// Sessions.
	SessionTimeoutSecVal int   `json:"session_timeout_sec"` // 0 -> default 86400
	SessionEpochVal      int64 `json:"session_epoch"`       // sessions issued before this are invalid

	// Data lifecycle (0 = disabled / keep forever).
	AutoHideDaysVal         int `json:"auto_hide_days"`
	CheckinRetentionDaysVal int `json:"checkin_retention_days"`
	LogcatRetentionDaysVal  int `json:"logcat_retention_days"`
	// Minimum seconds between two stored check-in rows for one device when nothing
	// but volatile fields changed (0 -> default 30). Transitions always store a row.
	CheckinSampleSecVal int `json:"checkin_sample_sec"`
	// History older than CheckinDownsampleDaysVal is thinned to one row per device per
	// CheckinDownsampleSecVal, instead of being deleted: the shape of the week stays
	// readable years later, at a fraction of the rows. 0 days = off.
	// Pointers so an absent key (an existing config file) means "use the default"
	// while an explicit 0 means "off" — with a plain int the two are the same value.
	CheckinDownsampleDaysVal *int `json:"checkin_downsample_days,omitempty"`
	CheckinDownsampleSecVal  *int `json:"checkin_downsample_sec,omitempty"`
	// Progress cursor (YYYY-MM-DD) of the one-off legacy check-in cleanup that strips
	// bulky keys from rows written before insert-time stripping existed. "" = not
	// started, "done" = finished.
	LegacyStripCursorVal string `json:"legacy_strip_cursor"`

	// DupStripCursorVal walks the strip of keys now held in device_samples.
	DupStripCursorVal string `json:"dup_strip_cursor"`
	// First day the cleanup started from (oldest check-in at that time); fixed
	// denominator for the progress percentage shown in Settings.
	LegacyStripStartVal string `json:"legacy_strip_start"`
	// Newest-first cursor (YYYY-MM-DD) of the device_build_history backfill: the
	// next day to process. "" = not started, "done" = finished.
	BuildHistoryCursorVal string `json:"build_history_cursor"`
	// Same shape, for filling device_samples from existing check-in history so the
	// shaped tables cover the past as well as everything arriving now.
	SamplesBackfillCursorVal string `json:"samples_backfill_cursor"`
	// Dashboard shows a maintenance page to non-admin users while set.
	MaintenanceModeFlag bool `json:"maintenance_mode"`
	// Drop check-ins and telemetry from DPC agents instead of storing them. The
	// agent still gets a normal reply, so it does not retry-storm; nothing is
	// written. Devices already enrolled keep their history and simply stop
	// updating until this is turned off again.
	IgnoreDPCCheckinsFlag bool `json:"ignore_dpc_checkins"`

	// Dashboard preferences & branding.
	PageSizeVal    int    `json:"page_size"`    // 0 -> default 25
	DefaultSortVal string `json:"default_sort"` // "" -> last_seen
	DensityVal     string `json:"density"`      // "" -> comfortable
	BrandNameVal   string `json:"brand_name"`   // "" -> AIO MDM
	Use24HourFlag  bool   `json:"use_24_hour"`

	// Kiosk. Package-name patterns (glob, '*' wildcard, e.g. "com.aioapp.*") that
	// may be chosen as the locked kiosk app. Empty list = any installed app is allowed.
	KioskAllowlistVal []string `json:"kiosk_allowlist"`
	// OTAMinReleaseVal is, per product key, the oldest release whose firmware has
	// the MDM OTA agent. Devices on a build older than it get no MDM OTA.
	OTAMinReleaseVal map[string]int `json:"ota_min_release,omitempty"`
	// AppFamilyModeVal: "auto" groups package variants (x, x.internal, x.uatv2)
	// into one library family, "suggest" only proposes merges, "off" never
	// groups variants. Versions of one package always group. "" = auto.
	AppFamilyModeVal string `json:"app_family_mode,omitempty"`
	// MDMServersVal are the servers a device can be moved to from its page (the
	// dropdown behind "Move & reboot", which sets persist.sys.mdm.url). Empty means
	// the built-in defaults below.
	MDMServersVal []string `json:"mdm_servers,omitempty"`
	// LegacyOTAModeVal decides who answers the legacy otautil routes on the legacy
	// OTA port: "mdm" (this server, from its own deployments) or "passthrough"
	// (forwarded verbatim to the old ota-server container). "" = mdm.
	LegacyOTAModeVal string `json:"legacy_ota_mode,omitempty"`

	// Legacy OTA discovery config (ota_config.json on S3). The otautil app reads it
	// every poll and decides its own reboots from it; the MDM publishes the file so
	// the reboot window always sits hours ahead of the reference timezone's clock and
	// in the small hours, which keeps the reboot decision with the MDM. See
	// internal/otaconfig.
	OTAConfigManagedVal  bool   `json:"ota_config_managed,omitempty"`
	OTAConfigBaseURLVal  string `json:"ota_config_base_url,omitempty"`
	OTAConfigPollMSVal   int    `json:"ota_config_poll_ms,omitempty"`
	OTAConfigTZVal       string `json:"ota_config_tz,omitempty"`
	OTAConfigLeadHrsVal  int    `json:"ota_config_lead_hours,omitempty"`
	OTAConfigLastJSONVal string `json:"ota_config_last_json,omitempty"`
	OTAConfigLastAtVal   string `json:"ota_config_last_at,omitempty"`

	// Products whose hardware carries a wireless-charging guest pad (WLC). Pad
	// UI/telemetry surfaces render only for these products. Absent = default {"t7"}.
	WlcProductsVal []string `json:"wlc_products,omitempty"`

	// Fleet-wide device policy, merged verbatim into every device's config channel
	// (checkin response + WS config frames). Keys the agent understands today:
	// update_policy{}, location_enabled, network{ca_certs,wifi_networks,vpn},
	// app_restrictions[]. Kept as a free-form map so new agent capabilities don't
	// need a config-schema change.
	DevicePolicyVal map[string]any `json:"device_policy,omitempty"`

	// Alerting. Slack/Discord/Mattermost-compatible webhook for new alerts ("" = off).
	AlertWebhookURLVal string `json:"alert_webhook_url"`

	// AI analysis. The API key is the on/off switch ("" = disabled). Provider selects
	// the wire format: "anthropic" (Claude) or "deepseek"/"openai" (OpenAI-compatible
	// /chat/completions). BaseURL overrides the provider's default endpoint. Digest
	// posts a daily fleet summary to the alert webhook during housekeeping when on.
	AnthropicAPIKeyVal string `json:"anthropic_api_key"` // the active provider's API key
	AnthropicModelVal  string `json:"anthropic_model"`   // active model id
	AIProviderVal      string `json:"ai_provider"`       // "anthropic" | "deepseek" | "openai"
	AIBaseURLVal       string `json:"ai_base_url"`       // optional endpoint override
	AIDigestEnabledVal bool   `json:"ai_digest_enabled"`

	mu   sync.RWMutex
	path string
}

// DefaultAnthropicModel is used when no model has been configured.
const DefaultAnthropicModel = "claude-opus-4-8"

// defaultLegacyBuilds is the seed list of WebSocket-incapable firmware build IDs.
func defaultLegacyBuilds() []string { return []string{"A15-v1.62-user"} }

func Load(path string) (*Config, error) {
	c := &Config{path: path}
	// Ensure the parent directory exists so setters (os.WriteFile) can persist.
	// Without this, a missing dir makes every settings write fail silently.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700) // secrets live here; keep it owner-only
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.ExtraColumns = []ExtraColumn{}
			c.LegacyBuildIDs = defaultLegacyBuilds()
			c.applyEnvOverrides()
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		// A corrupt config file must not crash-loop the whole service on boot. Preserve
		// the bad file for inspection and start from defaults + env overrides. (Atomic
		// writes make corruption unlikely; this is the last-resort safety net.)
		log.Printf("[config] %s is corrupt (%v) — backing it up to %s.corrupt and starting from defaults", path, err, path)
		_ = os.Rename(path, path+".corrupt")
		fresh := &Config{path: path, ExtraColumns: []ExtraColumn{}, LegacyBuildIDs: defaultLegacyBuilds()}
		fresh.applyEnvOverrides()
		return fresh, nil
	}
	// A config file predating the legacy_build_ids key (nil, not an explicit []) gets the
	// default seed; an admin who clears the list leaves a non-nil empty slice, respected as-is.
	if c.LegacyBuildIDs == nil {
		c.LegacyBuildIDs = defaultLegacyBuilds()
	}
	c.applyEnvOverrides()
	return c, nil
}

// applyEnvOverrides lets environment variables override the persisted AI settings
// (convenient for .env-based deployment). A set env var wins over the stored value
// and over the Settings UI on every restart; unset vars leave the file value intact.
//
//	AI_PROVIDER  anthropic | deepseek | openai
//	AI_API_KEY   the provider's API key ("" = AI disabled)
//	AI_MODEL     model id (e.g. deepseek-chat, claude-opus-4-8)
//	AI_BASE_URL  endpoint override (e.g. https://api.deepseek.com/anthropic)
//	AI_DIGEST    1/true/on to enable the daily fleet digest
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("AI_PROVIDER"); v != "" {
		c.AIProviderVal = v
	}
	if v := os.Getenv("AI_API_KEY"); v != "" {
		c.AnthropicAPIKeyVal = v
	}
	if v := os.Getenv("AI_MODEL"); v != "" {
		c.AnthropicModelVal = v
	}
	if v := os.Getenv("AI_BASE_URL"); v != "" {
		c.AIBaseURLVal = v
	}
	if v := os.Getenv("AI_DIGEST"); v != "" {
		c.AIDigestEnabledVal = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	}
}

func (c *Config) Columns() []ExtraColumn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ExtraColumn, len(c.ExtraColumns))
	copy(out, c.ExtraColumns)
	return out
}

func (c *Config) Add(col ExtraColumn) error {
	c.mu.Lock()
	c.ExtraColumns = append(c.ExtraColumns, col)
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) LegacyCheckin() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.LegacyCheckinOn
}

func (c *Config) SetLegacyCheckin(v bool) error {
	c.mu.Lock()
	c.LegacyCheckinOn = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// LegacyBuilds returns the configured legacy (WS-incapable) build IDs.
func (c *Config) LegacyBuilds() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.LegacyBuildIDs))
	copy(out, c.LegacyBuildIDs)
	return out
}

// IsLegacyBuild reports whether a build ID is on the legacy list.
func (c *Config) IsLegacyBuild(buildID string) bool {
	if buildID == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, b := range c.LegacyBuildIDs {
		if b == buildID {
			return true
		}
	}
	return false
}

func (c *Config) AddLegacyBuild(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	c.mu.Lock()
	for _, b := range c.LegacyBuildIDs {
		if b == id {
			c.mu.Unlock()
			return nil // already present
		}
	}
	c.LegacyBuildIDs = append(c.LegacyBuildIDs, id)
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) RemoveLegacyBuild(id string) error {
	c.mu.Lock()
	filtered := c.LegacyBuildIDs[:0]
	for _, b := range c.LegacyBuildIDs {
		if b != id {
			filtered = append(filtered, b)
		}
	}
	c.LegacyBuildIDs = filtered
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) CheckinInterval() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CheckinIntervalSec <= 0 {
		return 60
	}
	return c.CheckinIntervalSec
}

func (c *Config) SetCheckinInterval(sec int) error {
	c.mu.Lock()
	c.CheckinIntervalSec = sec
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) ShellEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.ShellDisabledFlag
}

func (c *Config) SetShellEnabled(v bool) error {
	c.mu.Lock()
	c.ShellDisabledFlag = !v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) RemoteEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.RemoteDisabledFlag
}

func (c *Config) SetRemoteEnabled(v bool) error {
	c.mu.Lock()
	c.RemoteDisabledFlag = !v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) CommandExpiry() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CommandExpirySecVal <= 0 {
		return 300
	}
	return c.CommandExpirySecVal
}

func (c *Config) SetCommandExpiry(sec int) error {
	c.mu.Lock()
	c.CommandExpirySecVal = sec
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) MaxTargets() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxTargetsVal < 0 {
		return 0
	}
	return c.MaxTargetsVal
}

func (c *Config) SetMaxTargets(n int) error {
	c.mu.Lock()
	c.MaxTargetsVal = n
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// OperatorDenied returns the command types operators are barred from sending.
func (c *Config) OperatorDenied() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.OperatorDeniedCmds))
	copy(out, c.OperatorDeniedCmds)
	return out
}

// OperatorAllows reports whether operators may send the given command type.
func (c *Config) OperatorAllows(cmdType string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, t := range c.OperatorDeniedCmds {
		if t == cmdType {
			return false
		}
	}
	return true
}

func (c *Config) SetOperatorDenied(denied []string) error {
	c.mu.Lock()
	c.OperatorDeniedCmds = denied
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// DevicePolicy returns a deep copy of the fleet-wide device-policy map (nil-safe).
func (c *Config) DevicePolicy() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.DevicePolicyVal) == 0 {
		return nil
	}
	// Deep-copy through JSON so callers can't mutate shared nested maps.
	raw, err := json.Marshal(c.DevicePolicyVal)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// DevicePolicyKey returns one policy entry (nil if unset).
func (c *Config) DevicePolicyKey(key string) any {
	return c.DevicePolicy()[key]
}

// SetDevicePolicyKey sets (or, with a nil value, removes) one fleet policy entry.
func (c *Config) SetDevicePolicyKey(key string, v any) error {
	c.mu.Lock()
	if c.DevicePolicyVal == nil {
		c.DevicePolicyVal = map[string]any{}
	}
	if v == nil {
		delete(c.DevicePolicyVal, key)
	} else {
		c.DevicePolicyVal[key] = v
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// KioskAllowlist returns the configured kiosk locked-app patterns.
// WlcProducts returns the product keys that have a wireless-charging pad.
// Nil/empty config falls back to the historical default: the T7.
func (c *Config) WlcProducts() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.WlcProductsVal) == 0 {
		return []string{"t7"}
	}
	out := make([]string, len(c.WlcProductsVal))
	copy(out, c.WlcProductsVal)
	return out
}

func (c *Config) SetWlcProducts(keys []string) error {
	c.mu.Lock()
	c.WlcProductsVal = keys
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// WlcApplies reports whether a device's product has a charging pad. An empty
// product key counts as "t7" (legacy devices predate the product field).
func (c *Config) WlcApplies(productKey string) bool {
	key := strings.ToLower(strings.TrimSpace(productKey))
	if key == "" {
		key = "t7"
	}
	for _, p := range c.WlcProducts() {
		if strings.EqualFold(strings.TrimSpace(p), key) {
			return true
		}
	}
	return false
}

func (c *Config) KioskAllowlist() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.KioskAllowlistVal))
	copy(out, c.KioskAllowlistVal)
	return out
}

// defaultMDMServers is where a device can be pointed when nothing is configured:
// the live server and the stage one, which is the move people actually make.
var defaultMDMServers = []string{"https://mdm.dev.aioapp.com", "https://mdm-stage.dev.aioapp.com"}

// MDMServers returns the servers offered when moving a device, a copy.
func (c *Config) MDMServers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.MDMServersVal) == 0 {
		out := make([]string, len(defaultMDMServers))
		copy(out, defaultMDMServers)
		return out
	}
	out := make([]string, len(c.MDMServersVal))
	copy(out, c.MDMServersVal)
	return out
}

// SetMDMServers replaces that list; an empty list restores the defaults.
func (c *Config) SetMDMServers(urls []string) error {
	c.mu.Lock()
	c.MDMServersVal = urls
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// SetKioskAllowlist replaces the kiosk allowlist (patterns already trimmed/deduped
// by the caller).
// OTAMinRelease returns the per-product OTA support cutoff (release id), a copy.
// AppFamilyMode is "auto", "suggest" or "off"; see AppFamilyModeVal.
func (c *Config) AppFamilyMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch c.AppFamilyModeVal {
	case "suggest", "off":
		return c.AppFamilyModeVal
	}
	return "auto"
}

func (c *Config) SetAppFamilyMode(mode string) error {
	if mode != "suggest" && mode != "off" {
		mode = "auto"
	}
	c.mu.Lock()
	c.AppFamilyModeVal = mode
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// LegacyOTAMode is "mdm" or "passthrough"; see LegacyOTAModeVal.
func (c *Config) LegacyOTAMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.LegacyOTAModeVal == "passthrough" {
		return "passthrough"
	}
	return "mdm"
}

// SetLegacyOTAMode switches the legacy OTA routes between the MDM and the old server.
func (c *Config) SetLegacyOTAMode(mode string) error {
	if mode != "passthrough" {
		mode = "mdm"
	}
	c.mu.Lock()
	c.LegacyOTAModeVal = mode
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) OTAMinRelease() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int, len(c.OTAMinReleaseVal))
	for k, v := range c.OTAMinReleaseVal {
		out[k] = v
	}
	return out
}

// SetOTAMinRelease replaces the per-product OTA support cutoff (0 = none).
func (c *Config) SetOTAMinRelease(m map[string]int) error {
	c.mu.Lock()
	c.OTAMinReleaseVal = map[string]int{}
	for k, v := range m {
		if v > 0 {
			c.OTAMinReleaseVal[k] = v
		}
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetKioskAllowlist(patterns []string) error {
	c.mu.Lock()
	c.KioskAllowlistVal = patterns
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// KioskAppAllowed reports whether a package may be chosen as the kiosk app. An
// empty allowlist allows everything; otherwise the package must match at least one
// glob pattern ('*' matches any run of characters).
func (c *Config) KioskAppAllowed(pkg string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.KioskAllowlistVal) == 0 {
		return true
	}
	for _, pat := range c.KioskAllowlistVal {
		pat = strings.TrimSpace(pat)
		if pat != "" && globMatch(pat, pkg) {
			return true
		}
	}
	return false
}

// globMatch reports whether s matches a glob pattern whose only wildcard is '*'
// (matching any run of characters, including dots). Everything else is literal.
func globMatch(pattern, s string) bool {
	p, si := 0, 0
	star, ss := -1, 0
	for si < len(s) {
		if p < len(pattern) && pattern[p] == s[si] {
			p++
			si++
		} else if p < len(pattern) && pattern[p] == '*' {
			star = p
			ss = si
			p++
		} else if star != -1 {
			p = star + 1
			ss++
			si = ss
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func (c *Config) SessionTimeout() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.SessionTimeoutSecVal <= 0 {
		return 86400
	}
	return c.SessionTimeoutSecVal
}

func (c *Config) SetSessionTimeout(sec int) error {
	c.mu.Lock()
	c.SessionTimeoutSecVal = sec
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SessionEpoch() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionEpochVal
}

func (c *Config) SetSessionEpoch(ts int64) error {
	c.mu.Lock()
	c.SessionEpochVal = ts
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) AutoHideDays() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AutoHideDaysVal < 0 {
		return 0
	}
	return c.AutoHideDaysVal
}

func (c *Config) CheckinRetentionDays() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CheckinRetentionDaysVal < 0 {
		return 0
	}
	return c.CheckinRetentionDaysVal
}

func (c *Config) LogcatRetentionDays() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.LogcatRetentionDaysVal < 0 {
		return 0
	}
	return c.LogcatRetentionDaysVal
}

// DefaultCheckinSampleSec is the coalescing window used when none is configured.
//
// 60 rather than 30: a device that changes nothing now stores half as many history
// rows, and nothing is lost by it — a transition (battery %, build, charging, pad,
// kiosk, boot) still writes its row the instant it happens, so charts keep every real
// event and only the idle heartbeat thins out. 30 was chosen when the window rarely
// applied anyway, because sensor jitter was counting as a state change; with that
// fixed the window is what actually governs the row rate.
const DefaultCheckinSampleSec = 60

func (c *Config) CheckinSampleSec() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CheckinSampleSecVal <= 0 {
		return DefaultCheckinSampleSec
	}
	return c.CheckinSampleSecVal
}

// Defaults for thinning old history. 60 days of full-resolution check-ins covers every
// chart and export the dashboard offers; past that a point every 5 minutes still shows
// when a device was on, charging or on a pad, which is all anyone reads that far back.
const (
	DefaultCheckinDownsampleDays = 60
	DefaultCheckinDownsampleSec  = 300
)

// CheckinDownsampleDays is the age past which history is thinned (0 = keep every row
// forever, whatever its age).
func (c *Config) CheckinDownsampleDays() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CheckinDownsampleDaysVal == nil {
		return DefaultCheckinDownsampleDays
	}
	if *c.CheckinDownsampleDaysVal < 0 {
		return 0
	}
	return *c.CheckinDownsampleDaysVal
}

// CheckinDownsampleSec is the bucket kept in thinned history: one row per device per
// this many seconds.
func (c *Config) CheckinDownsampleSec() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CheckinDownsampleSecVal == nil || *c.CheckinDownsampleSecVal <= 0 {
		return DefaultCheckinDownsampleSec
	}
	return *c.CheckinDownsampleSecVal
}

func (c *Config) DupStripCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DupStripCursorVal
}

func (c *Config) SetDupStripCursor(cur string) error {
	c.mu.Lock()
	c.DupStripCursorVal = cur
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) LegacyStripCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.LegacyStripCursorVal
}

func (c *Config) LegacyStripStart() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.LegacyStripStartVal
}

func (c *Config) SetLegacyStripStart(day string) error {
	c.mu.Lock()
	c.LegacyStripStartVal = day
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) BuildHistoryCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BuildHistoryCursorVal
}

func (c *Config) SamplesBackfillCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SamplesBackfillCursorVal
}

func (c *Config) SetSamplesBackfillCursor(cur string) error {
	c.mu.Lock()
	c.SamplesBackfillCursorVal = cur
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetBuildHistoryCursor(cur string) error {
	c.mu.Lock()
	c.BuildHistoryCursorVal = cur
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetLegacyStripCursor(cur string) error {
	c.mu.Lock()
	c.LegacyStripCursorVal = cur
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetDataLifecycle(autoHide, checkinRet, logcatRet, sampleSec, downsampleDays, downsampleSec int) error {
	c.mu.Lock()
	c.AutoHideDaysVal = autoHide
	c.CheckinRetentionDaysVal = checkinRet
	c.LogcatRetentionDaysVal = logcatRet
	c.CheckinSampleSecVal = sampleSec
	// Written explicitly from here on, so a later default change never silently
	// re-thins history an admin chose to keep.
	c.CheckinDownsampleDaysVal = &downsampleDays
	c.CheckinDownsampleSecVal = &downsampleSec
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// MaintenanceMode, when on, turns every dashboard page into a maintenance notice for
// non-admin users. Admins keep full access; the device API and WebSocket are never
// affected. Used while running heavy one-off database work.
func (c *Config) MaintenanceMode() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MaintenanceModeFlag
}

func (c *Config) SetMaintenanceMode(v bool) error {
	c.mu.Lock()
	c.MaintenanceModeFlag = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// IgnoreDPCCheckins, when on, makes the device API accept check-ins and WS
// telemetry from DPC agents and store nothing: no check-in row, no device
// update, no events, packages or OTA work. The agent gets an ordinary reply so
// it keeps its normal interval instead of retrying harder. Firmware clients are
// never affected.
func (c *Config) IgnoreDPCCheckins() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IgnoreDPCCheckinsFlag
}

func (c *Config) SetIgnoreDPCCheckins(v bool) error {
	c.mu.Lock()
	c.IgnoreDPCCheckinsFlag = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) RequireReason() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RequireReasonFlag
}

// AgentAPKURL is where a factory-reset device downloads the DPC agent from during
// QR provisioning ("" = QR cold-provisioning is off). Settings win over env.
func (c *Config) AgentAPKURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AgentAPKURLVal != "" {
		return c.AgentAPKURLVal
	}
	return os.Getenv("AGENT_APK_URL")
}

// AgentAPKChecksum is the URL-safe base64 SHA-256 of the agent APK's signing
// certificate, as Android's provisioning expects.
func (c *Config) AgentAPKChecksum() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AgentAPKChecksumVal != "" {
		return c.AgentAPKChecksumVal
	}
	return os.Getenv("AGENT_APK_CHECKSUM")
}

// AgentAPKHosted reports whether an uploaded agent APK is being served by this server.
func (c *Config) AgentAPKHosted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentAPKHostedSHA != ""
}

// AgentAPKHostedInfo returns the hosted APK's checksum, original name, size and time.
func (c *Config) AgentAPKHostedInfo() (sha, name string, size int64, at time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentAPKHostedSHA, c.AgentAPKHostedName, c.AgentAPKHostedSize, c.AgentAPKHostedAt
}

func (c *Config) SetAgentAPKHosted(sha, name string, size int64) error {
	c.mu.Lock()
	c.AgentAPKHostedSHA, c.AgentAPKHostedName, c.AgentAPKHostedSize = sha, name, size
	if sha != "" {
		c.AgentAPKHostedAt = time.Now()
	} else {
		c.AgentAPKHostedAt = time.Time{}
		c.AgentAPKHostedPackage, c.AgentAPKHostedVersion = "", ""
		c.AgentAPKHostedVersionCode, c.AgentAPKHostedSHA256Hex = 0, ""
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// SetAgentAPKHostedBuild records what the hosted APK actually is, read from the
// file itself rather than from whoever uploaded it.
func (c *Config) SetAgentAPKHostedBuild(pkg, version string, versionCode int64, sha256Hex string) error {
	c.mu.Lock()
	c.AgentAPKHostedPackage, c.AgentAPKHostedVersion = pkg, version
	c.AgentAPKHostedVersionCode, c.AgentAPKHostedSHA256Hex = versionCode, sha256Hex
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AgentAPKBuild is one hosted agent APK: what it is and what verifies it.
type AgentAPKBuild struct {
	SHA         string    `json:"sha"`    // base64url SHA-256 of the file (QR extras)
	SHA256Hex   string    `json:"sha256"` // hex SHA-256 (what the agent checks)
	Name        string    `json:"name"`   // original filename, for the operator
	Size        int64     `json:"size"`
	At          time.Time `json:"at"`
	Package     string    `json:"package"`
	Version     string    `json:"version"`
	VersionCode int64     `json:"version_code"`
}

// AgentAPKSlot returns the build hosted in a slot. The "dpc" slot reads the original
// single-APK fields, so nothing had to be migrated when slots were introduced.
func (c *Config) AgentAPKSlot(slot string) (AgentAPKBuild, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if slot == "" || slot == "dpc" {
		if c.AgentAPKHostedSHA == "" {
			return AgentAPKBuild{}, false
		}
		return AgentAPKBuild{
			SHA: c.AgentAPKHostedSHA, SHA256Hex: c.AgentAPKHostedSHA256Hex,
			Name: c.AgentAPKHostedName, Size: c.AgentAPKHostedSize, At: c.AgentAPKHostedAt,
			Package: c.AgentAPKHostedPackage, Version: c.AgentAPKHostedVersion,
			VersionCode: c.AgentAPKHostedVersionCode,
		}, true
	}
	b, ok := c.AgentAPKSlotMap[slot]
	return b, ok && b.SHA != ""
}

// SetAgentAPKSlot records (or, with an empty build, clears) a slot.
func (c *Config) SetAgentAPKSlot(slot string, b AgentAPKBuild) error {
	c.mu.Lock()
	if slot == "" || slot == "dpc" {
		c.AgentAPKHostedSHA, c.AgentAPKHostedSHA256Hex = b.SHA, b.SHA256Hex
		c.AgentAPKHostedName, c.AgentAPKHostedSize = b.Name, b.Size
		c.AgentAPKHostedPackage, c.AgentAPKHostedVersion = b.Package, b.Version
		c.AgentAPKHostedVersionCode = b.VersionCode
		c.AgentAPKHostedAt = b.At
	} else {
		if c.AgentAPKSlotMap == nil {
			c.AgentAPKSlotMap = map[string]AgentAPKBuild{}
		}
		if b.SHA == "" {
			delete(c.AgentAPKSlotMap, slot)
		} else {
			c.AgentAPKSlotMap[slot] = b
		}
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AgentAPKHostedBuild is the hosted agent's package, version name, version code and
// hex SHA-256. Zero values mean nothing is hosted, or it predates version parsing.
func (c *Config) AgentAPKHostedBuild() (pkg, version string, versionCode int64, sha256Hex string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentAPKHostedPackage, c.AgentAPKHostedVersion, c.AgentAPKHostedVersionCode, c.AgentAPKHostedSHA256Hex
}

func (c *Config) SetAgentAPK(url, checksum string) error {
	c.mu.Lock()
	c.AgentAPKURLVal = url
	c.AgentAPKChecksumVal = checksum
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetRequireReason(v bool) error {
	c.mu.Lock()
	c.RequireReasonFlag = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) PageSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.PageSizeVal <= 0 {
		return 25
	}
	// Defensive ceiling matching the setters' clamp (handlers.go DeviceList /
	// SettingsSetDashboard): a config persisted before that clamp existed could
	// still hold up to 500, which renders thousands of DOM nodes client-side.
	if c.PageSizeVal > 200 {
		return 200
	}
	return c.PageSizeVal
}

func (c *Config) SetPageSize(n int) error {
	c.mu.Lock()
	c.PageSizeVal = n
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) DefaultSort() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.DefaultSortVal == "" {
		return "last_seen"
	}
	return c.DefaultSortVal
}

func (c *Config) SetDefaultSort(s string) error {
	c.mu.Lock()
	c.DefaultSortVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) Density() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.DensityVal == "" {
		return "comfortable"
	}
	return c.DensityVal
}

func (c *Config) SetDensity(s string) error {
	c.mu.Lock()
	c.DensityVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) BrandName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.BrandNameVal == "" {
		return "AIO MDM"
	}
	return c.BrandNameVal
}

// CustomBrand returns the operator-configured brand override, or "" when unset.
// Templates treat the empty case as "render the default styled AIO MDM
// wordmark" — so feed this (not BrandName) into the dashboard's .Brand var,
// otherwise a non-empty default would replace the styled wordmark with plain text.
func (c *Config) CustomBrand() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BrandNameVal
}

func (c *Config) AlertWebhookURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AlertWebhookURLVal
}

func (c *Config) SetAlertWebhookURL(s string) error {
	c.mu.Lock()
	c.AlertWebhookURLVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AnthropicAPIKey returns the configured key ("" = AI analysis disabled).
func (c *Config) AnthropicAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AnthropicAPIKeyVal
}

func (c *Config) SetAnthropicAPIKey(s string) error {
	c.mu.Lock()
	c.AnthropicAPIKeyVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AIEnabled reports whether AI analysis can run (a key is set).
func (c *Config) AIEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AnthropicAPIKeyVal != ""
}

// AnthropicModel returns the configured model id verbatim ("" if unset — the ai
// package fills in a provider-appropriate default).
func (c *Config) AnthropicModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AnthropicModelVal
}

func (c *Config) SetAnthropicModel(s string) error {
	c.mu.Lock()
	c.AnthropicModelVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AIProvider returns the configured provider, defaulting to "anthropic".
func (c *Config) AIProvider() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AIProviderVal == "" {
		return "anthropic"
	}
	return c.AIProviderVal
}

func (c *Config) SetAIProvider(s string) error {
	c.mu.Lock()
	c.AIProviderVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// AIBaseURL returns the optional endpoint override ("" = use provider default).
func (c *Config) AIBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AIBaseURLVal
}

func (c *Config) SetAIBaseURL(s string) error {
	c.mu.Lock()
	c.AIBaseURLVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) AIDigestEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AIDigestEnabledVal
}

func (c *Config) SetAIDigestEnabled(v bool) error {
	c.mu.Lock()
	c.AIDigestEnabledVal = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) SetBrandName(s string) error {
	c.mu.Lock()
	c.BrandNameVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) Use24Hour() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Use24HourFlag
}

func (c *Config) SetUse24Hour(v bool) error {
	c.mu.Lock()
	c.Use24HourFlag = v
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) Remove(key string) error {
	c.mu.Lock()
	filtered := c.ExtraColumns[:0]
	for _, col := range c.ExtraColumns {
		if col.Key != key {
			filtered = append(filtered, col)
		}
	}
	c.ExtraColumns = filtered
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// ── Legacy OTA discovery config (ota_config.json) ──────────────────────────────

// OTAConfigManaged reports whether this server publishes ota_config.json to S3. Off by
// default: only one MDM may own the file, and it is read by every legacy device.
func (c *Config) OTAConfigManaged() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.OTAConfigManagedVal
}

// OTAConfigOptions returns the publish settings, filling anything unset from the
// package defaults so a half-configured file still renders something the app accepts.
func (c *Config) OTAConfigOptions() otaconfig.Options {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o := otaconfig.Defaults()
	if c.OTAConfigBaseURLVal != "" {
		o.BaseURL = c.OTAConfigBaseURLVal
	}
	if c.OTAConfigPollMSVal > 0 {
		o.PollMS = c.OTAConfigPollMSVal
	}
	if c.OTAConfigTZVal != "" {
		o.Timezone = c.OTAConfigTZVal
	}
	if c.OTAConfigLeadHrsVal > 0 {
		o.LeadHours = c.OTAConfigLeadHrsVal
	}
	return o
}

// OTAConfigLast returns the last published document and when it went out, for the
// settings page and to skip a no-op PUT.
func (c *Config) OTAConfigLast() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.OTAConfigLastJSONVal, c.OTAConfigLastAtVal
}

// SetOTAConfig saves the publish settings. Empty/zero values fall back to defaults.
func (c *Config) SetOTAConfig(managed bool, baseURL string, pollMS int, tz string, leadHours int) error {
	c.mu.Lock()
	c.OTAConfigManagedVal = managed
	c.OTAConfigBaseURLVal = strings.TrimSpace(baseURL)
	c.OTAConfigPollMSVal = pollMS
	c.OTAConfigTZVal = strings.TrimSpace(tz)
	c.OTAConfigLeadHrsVal = leadHours
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

// SetOTAConfigPublished records what was last written to S3.
func (c *Config) SetOTAConfigPublished(body string, at time.Time) error {
	c.mu.Lock()
	c.OTAConfigLastJSONVal = body
	c.OTAConfigLastAtVal = at.UTC().Format(time.RFC3339)
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}
