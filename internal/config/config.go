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
