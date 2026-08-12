package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// writeFileAtomic writes data to a temp file in the same directory and renames it
// over path (rename is atomic on the same filesystem), so a crash mid-write can't
// leave a truncated/corrupt config file — which would fail Load and brick startup.
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
	ExtraColumns       []ExtraColumn `json:"extra_columns"`
	LegacyCheckinOn    bool          `json:"legacy_checkin"`
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

	// Sessions.
	SessionTimeoutSecVal int   `json:"session_timeout_sec"` // 0 -> default 86400
	SessionEpochVal      int64 `json:"session_epoch"`       // sessions issued before this are invalid

	// Data lifecycle (0 = disabled / keep forever).
	AutoHideDaysVal         int `json:"auto_hide_days"`
	CheckinRetentionDaysVal int `json:"checkin_retention_days"`
	LogcatRetentionDaysVal  int `json:"logcat_retention_days"`

	// Dashboard preferences & branding.
	PageSizeVal    int    `json:"page_size"`    // 0 -> default 25
	DefaultSortVal string `json:"default_sort"` // "" -> last_seen
	DensityVal     string `json:"density"`      // "" -> comfortable
	BrandNameVal   string `json:"brand_name"`   // "" -> AIO MDM
	Use24HourFlag  bool   `json:"use_24_hour"`

	// Kiosk. Package-name patterns (glob, '*' wildcard, e.g. "com.aioapp.*") that
	// may be chosen as the locked kiosk app. Empty list = any installed app is allowed.
	KioskAllowlistVal []string `json:"kiosk_allowlist"`

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

// KioskAllowlist returns the configured kiosk locked-app patterns.
func (c *Config) KioskAllowlist() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.KioskAllowlistVal))
	copy(out, c.KioskAllowlistVal)
	return out
}

// SetKioskAllowlist replaces the kiosk allowlist (patterns already trimmed/deduped
// by the caller).
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

func (c *Config) SetDataLifecycle(autoHide, checkinRet, logcatRet int) error {
	c.mu.Lock()
	c.AutoHideDaysVal = autoHide
	c.CheckinRetentionDaysVal = checkinRet
	c.LogcatRetentionDaysVal = logcatRet
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return writeFileAtomic(c.path, data)
}

func (c *Config) RequireReason() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RequireReasonFlag
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
