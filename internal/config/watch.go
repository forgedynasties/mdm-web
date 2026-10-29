package config

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"reflect"
	"time"
)

// Watch reloads the settings when the file changes under this process: with several
// server copies on one data volume (scalability plan W5), a setting saved through one
// copy reaches the others within every. Values copied out of the config once at startup
// (the check-in sample window, for one) still need a restart to follow.
func (c *Config) Watch(ctx context.Context, every time.Duration) {
	stamp := func() (time.Time, int64) {
		fi, err := os.Stat(c.path)
		if err != nil {
			return time.Time{}, 0
		}
		return fi.ModTime(), fi.Size()
	}
	lastMod, lastSize := stamp()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		mod, size := stamp()
		if mod.IsZero() || (mod.Equal(lastMod) && size == lastSize) {
			continue
		}
		lastMod, lastSize = mod, size
		if err := c.reload(); err != nil {
			log.Printf("[config] reload %s: %v (keeping the settings in memory)", c.path, err)
			continue
		}
		log.Printf("[config] reloaded %s: changed by another server copy (or by hand)", c.path)
	}
}

// reload reads the file into a fresh Config and copies every exported field across, so a
// key removed from the file (a map entry) goes too, which decoding in place would keep.
func (c *Config) reload() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	fresh := &Config{}
	if err := json.Unmarshal(data, fresh); err != nil {
		return err
	}
	if fresh.LegacyBuildIDs == nil {
		fresh.LegacyBuildIDs = defaultLegacyBuilds()
	}
	fresh.applyEnvOverrides()
	c.mu.Lock()
	defer c.mu.Unlock()
	dst, src := reflect.ValueOf(c).Elem(), reflect.ValueOf(fresh).Elem()
	for i := 0; i < dst.NumField(); i++ {
		if dst.Type().Field(i).IsExported() {
			dst.Field(i).Set(src.Field(i))
		}
	}
	return nil
}
