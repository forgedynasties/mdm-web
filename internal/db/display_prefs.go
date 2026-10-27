package db

import (
	"context"
	"encoding/json"
)

// DisplayPrefs is one person's display preferences (24 Oct 2026; user: "display defaults
// each person"). A nil field follows the fleet default in Settings › Display.
type DisplayPrefs struct {
	PageSize *int    `json:"page_size,omitempty"`
	Sort     *string `json:"sort,omitempty"`
	Density  *string `json:"density,omitempty"`
	Use24    *bool   `json:"use24,omitempty"`
	// DeviceSplit is the device page's divider: the graph pane's share of the width,
	// 30-75 % (27 Oct 2026, user: "can it remember the divider per user?").
	DeviceSplit *float64 `json:"device_split,omitempty"`
}

// UserDisplayPrefs reads a user's preferences (empty when unset or unknown).
func (d *DB) UserDisplayPrefs(ctx context.Context, username string) (DisplayPrefs, error) {
	var p DisplayPrefs
	var b []byte
	if err := d.pool.QueryRow(ctx, `SELECT display_prefs FROM users WHERE username = $1`, username).Scan(&b); err != nil {
		return p, err
	}
	_ = json.Unmarshal(b, &p)
	return p, nil
}

// SetUserDisplayPrefs stores a user's preferences.
func (d *DB) SetUserDisplayPrefs(ctx context.Context, username string, p DisplayPrefs) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = d.pool.Exec(ctx, `UPDATE users SET display_prefs = $2::jsonb WHERE username = $1`, username, string(b))
	return err
}
