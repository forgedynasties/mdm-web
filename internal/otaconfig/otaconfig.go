// Package otaconfig renders and publishes ota_config.json — the discovery file the
// legacy otautil app reads from S3 on every poll.
//
// Why the MDM owns this file: otautil decides its own reboots. When an update finishes
// installing it reboots immediately if force_reboot is set or the device's LOCAL hour is
// inside [window_start_hour, window_end_hour); otherwise it arms an AllowWhileIdle alarm
// for window_start_hour. There is no "never reboot" setting — an empty window still arms
// the alarm — so the MDM cannot stop that reboot, only place it.
//
// So we place it: the published window is always a few hours ahead of the reference
// timezone's clock and always inside the small hours. A device that finishes an install
// is therefore never inside the window, defers, and the MDM reboots it first (an Android
// alarm does not survive that reboot, and otautil's boot receiver does not re-arm it). If
// nobody reboots it, the fallback lands at 02:00–05:00 local, never mid-service.
package otaconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Key is the object the app fetches. The URL is hardcoded in the app
// (UpdateService.DISCOVERY_URL), so this name cannot change.
const Key = "ota_config.json"

// nightHours are the candidate reboot hours, in the reference timezone. Any auto-reboot
// the MDM does not pre-empt lands on one of these.
var nightHours = []int{2, 3, 4}

// Options is everything the published file carries.
type Options struct {
	BaseURL      string // server_config.api_base_url — what devices poll for updates
	PollMS       int    // app_behavior.poll_interval_ms
	Timezone     string // reference zone for the window, e.g. America/Los_Angeles
	LeadHours    int    // how far ahead of "now" the window must sit
	ForceReboot  bool   // true = reboot the moment an install finishes (MDM no longer owns it)
	FixedStart   int    // when > 0, publish this hour instead of the rolling one
	FixedEnd     int
	UseFixedHour bool
}

// Defaults mirror what the fleet runs today, with the reboot window moved under MDM control.
func Defaults() Options {
	return Options{
		BaseURL:   "http://18.237.229.116:8000",
		PollMS:    30000,
		Timezone:  "America/Los_Angeles",
		LeadHours: 3,
	}
}

// FallbackZone is what we publish for when the devices under update do not agree on a
// timezone, or when nothing is updating: the fleet's centre of gravity is Los Angeles, and
// a window computed for LA night is at worst inconvenient elsewhere, never mid-service
// here.
const FallbackZone = "America/Los_Angeles"

// ZoneFor picks the timezone the published window should be computed for, given the zones
// of the devices currently taking an update. One shared zone wins; a mix falls back to LA,
// because a window can only be right for one zone at a time and this is the one that
// matters most.
func ZoneFor(deviceZones []string) string {
	seen := ""
	for _, z := range deviceZones {
		z = strings.TrimSpace(z)
		if z == "" {
			continue
		}
		if seen == "" {
			seen = z
			continue
		}
		if seen != z {
			return FallbackZone // mixed
		}
	}
	if seen == "" {
		return FallbackZone // nothing updating
	}
	return seen
}

// Window returns the reboot window to publish for a given moment.
//
// The rolling rule: the first night hour that is at least LeadHours ahead of now in the
// reference zone. During the working day that is 02:00 (many hours away); late in the
// evening 02:00 is too close, so it steps to 03:00 or 04:00. The current hour is never
// inside the returned window, which is the property the whole scheme rests on.
func Window(o Options, now time.Time) (start, end int) {
	if o.UseFixedHour {
		return o.FixedStart, o.FixedEnd
	}
	lead := o.LeadHours
	if lead < 1 {
		lead = 1
	}
	loc, err := time.LoadLocation(o.Timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	for _, h := range nightHours {
		// Never publish the hour we are currently in: the device would read itself as
		// INSIDE the window and reboot on the spot, which is the one outcome this whole
		// scheme exists to prevent. (hoursUntil reports 24 for the current hour.)
		if h == local.Hour() {
			continue
		}
		if hoursUntil(local, h) >= lead {
			return h, h + 1
		}
	}
	// Only reachable inside the night band itself, where every candidate is within the
	// lead time. Take the furthest one away — we are already in the safe hours, so the
	// worst case is a reboot an hour or two from now, at 3 or 4 in the morning.
	best := -1
	for _, h := range nightHours {
		if h == local.Hour() {
			continue // hoursUntil says 24 for the current hour, which would always "win"
		}
		if best < 0 || hoursUntil(local, h) > hoursUntil(local, best) {
			best = h
		}
	}
	if best < 0 {
		best = nightHours[0]
	}
	return best, best + 1
}

// hoursUntil is whole hours from local until the next occurrence of hour h, 1..24.
func hoursUntil(local time.Time, h int) int {
	d := h - local.Hour()
	if d <= 0 {
		d += 24
	}
	return d
}

// Render produces the file exactly as otautil parses it. The app reads server_config and
// app_behavior with getJSONObject (not opt), so both objects must always be present.
func Render(o Options, now time.Time) ([]byte, error) {
	start, end := Window(o, now)
	doc := map[string]any{
		"server_config": map[string]any{
			"api_base_url": o.BaseURL,
		},
		"app_behavior": map[string]any{
			"poll_interval_ms": o.PollMS,
			"reboot_policy": map[string]any{
				"force_reboot":      o.ForceReboot,
				"window_start_hour": start,
				"window_end_hour":   end,
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Putter is the S3 write the publisher needs (*apkstore.Store satisfies it).
type Putter interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
}

// Publish writes the rendered config when it differs from prev. It returns the bytes it
// rendered and whether it actually wrote, so an unchanged hour costs no PUT — the window
// only moves a few times a day.
func Publish(ctx context.Context, p Putter, o Options, now time.Time, prev []byte) (body []byte, wrote bool, err error) {
	body, err = Render(o, now)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(bytes.TrimSpace(prev), bytes.TrimSpace(body)) {
		return body, false, nil
	}
	if err := p.Put(ctx, Key, "application/json", body); err != nil {
		return body, false, fmt.Errorf("put %s: %w", Key, err)
	}
	return body, true, nil
}
