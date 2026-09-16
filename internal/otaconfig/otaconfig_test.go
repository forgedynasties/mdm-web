package otaconfig

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func la(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata for America/Los_Angeles: %v", err)
	}
	return loc
}

// The property the whole scheme rests on: whatever the moment, the published window is in
// the small hours AND far enough ahead that a device finishing an install right now defers
// instead of rebooting, leaving the MDM time to reboot it itself.
func TestWindowIsAlwaysNightAndAhead(t *testing.T) {
	loc := la(t)
	o := Defaults()
	for hour := 0; hour < 24; hour++ {
		for _, min := range []int{0, 30, 59} {
			now := time.Date(2026, 9, 16, hour, min, 0, 0, loc)
			start, end := Window(o, now)
			if start < 2 || start > 4 {
				t.Errorf("%02d:%02d LA: start %d, want a night hour (2-4)", hour, min, start)
			}
			if end != start+1 {
				t.Errorf("%02d:%02d LA: end %d, want start+1", hour, min, end)
			}
			if start == now.Hour() {
				t.Errorf("%02d:%02d LA: window starts in the current hour — a staged device would reboot now", hour, min)
			}
			if got := hoursUntil(now, start); got < o.LeadHours && (hour < 2 || hour > 4) {
				t.Errorf("%02d:%02d LA: start %d is only %dh away, want >= %dh", hour, min, start, got, o.LeadHours)
			}
		}
	}
}

func TestWindowRollsForward(t *testing.T) {
	loc := la(t)
	o := Defaults()
	for _, tc := range []struct {
		hour, want int
		why        string
	}{
		{9, 2, "mid-morning: 02:00 is 17h away"},
		{14, 2, "afternoon: still 02:00"},
		{22, 2, "22:00: 02:00 is 4h away, still >= lead"},
		{23, 2, "23:00: 02:00 is exactly the 3h lead away, which qualifies"},
		{0, 3, "midnight: 02:00 is 2h away, step to 03:00"},
		{1, 4, "01:00: 02:00 and 03:00 too close, step to 04:00"},
	} {
		now := time.Date(2026, 9, 16, tc.hour, 0, 0, 0, loc)
		start, _ := Window(o, now)
		if start != tc.want {
			t.Errorf("%02d:00 LA: start %d, want %d (%s)", tc.hour, start, tc.want, tc.why)
		}
	}
}

func TestFixedHourOverride(t *testing.T) {
	o := Defaults()
	o.UseFixedHour, o.FixedStart, o.FixedEnd = true, 2, 3
	if start, end := Window(o, time.Now()); start != 2 || end != 3 {
		t.Errorf("fixed window: got %d-%d, want 2-3", start, end)
	}
}

// The app parses with getJSONObject, so both objects must exist and the policy must carry
// all three keys; a missing one silently reverts the device to its built-in defaults.
func TestRenderShapeMatchesTheApp(t *testing.T) {
	b, err := Render(Defaults(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ServerConfig struct {
			APIBaseURL string `json:"api_base_url"`
		} `json:"server_config"`
		AppBehavior struct {
			PollIntervalMS int `json:"poll_interval_ms"`
			RebootPolicy   *struct {
				ForceReboot     bool `json:"force_reboot"`
				WindowStartHour int  `json:"window_start_hour"`
				WindowEndHour   int  `json:"window_end_hour"`
			} `json:"reboot_policy"`
		} `json:"app_behavior"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	if doc.ServerConfig.APIBaseURL == "" {
		t.Error("api_base_url missing — devices would lose their server")
	}
	if doc.AppBehavior.PollIntervalMS <= 0 {
		t.Error("poll_interval_ms missing")
	}
	if doc.AppBehavior.RebootPolicy == nil {
		t.Fatal("reboot_policy missing")
	}
	if doc.AppBehavior.RebootPolicy.ForceReboot {
		t.Error("force_reboot must stay false: true hands the reboot back to the device")
	}
}

type fakePut struct {
	calls int
	err   error
}

func (f *fakePut) Put(ctx context.Context, key, contentType string, body []byte) error {
	f.calls++
	return f.err
}

func TestPublishSkipsUnchanged(t *testing.T) {
	p := &fakePut{}
	o := Defaults()
	now := time.Now()
	body, wrote, err := Publish(context.Background(), p, o, now, nil)
	if err != nil || !wrote || p.calls != 1 {
		t.Fatalf("first publish: wrote=%v calls=%d err=%v", wrote, p.calls, err)
	}
	if _, wrote, err = Publish(context.Background(), p, o, now, body); err != nil || wrote || p.calls != 1 {
		t.Fatalf("unchanged publish should not PUT: wrote=%v calls=%d err=%v", wrote, p.calls, err)
	}
}

func TestPublishReportsPutFailure(t *testing.T) {
	p := &fakePut{err: errors.New("denied")}
	if _, wrote, err := Publish(context.Background(), p, Defaults(), time.Now(), nil); err == nil || wrote {
		t.Fatalf("expected the put error to surface, got wrote=%v err=%v", wrote, err)
	}
}
