package config

import (
	"encoding/json"
	"os"
	"sync"
)

type ExtraColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type Config struct {
	ExtraColumns       []ExtraColumn `json:"extra_columns"`
	LegacyCheckinOn    bool          `json:"legacy_checkin"`
	CheckinIntervalSec int           `json:"checkin_interval_sec"`
	// Stored as "disabled" so an existing config file (without these keys)
	// defaults to enabled.
	ShellDisabledFlag  bool `json:"shell_disabled"`
	RemoteDisabledFlag bool `json:"remote_disabled"`

	// Command governance.
	CommandExpirySecVal int      `json:"command_expiry_sec"` // 0 -> default 300
	MaxTargetsVal       int      `json:"max_targets"`        // 0 -> unlimited
	OperatorDeniedCmds  []string `json:"operator_denied_cmds"`

	// Sessions.
	SessionTimeoutSecVal int   `json:"session_timeout_sec"` // 0 -> default 86400
	SessionEpochVal      int64 `json:"session_epoch"`       // sessions issued before this are invalid

	// Dashboard preferences & branding.
	PageSizeVal    int    `json:"page_size"`    // 0 -> default 25
	DefaultSortVal string `json:"default_sort"` // "" -> last_seen
	DensityVal     string `json:"density"`      // "" -> comfortable
	BrandNameVal   string `json:"brand_name"`   // "" -> MDM
	Use24HourFlag  bool   `json:"use_24_hour"`

	mu   sync.RWMutex
	path string
}

func Load(path string) (*Config, error) {
	c := &Config{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.ExtraColumns = []ExtraColumn{}
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	return c, nil
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
}

func (c *Config) BrandName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.BrandNameVal == "" {
		return "MDM"
	}
	return c.BrandNameVal
}

func (c *Config) SetBrandName(s string) error {
	c.mu.Lock()
	c.BrandNameVal = s
	data, _ := json.MarshalIndent(c, "", "  ")
	c.mu.Unlock()
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
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
	return os.WriteFile(c.path, data, 0644)
}
