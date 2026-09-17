package config

import "testing"

// TestCheckinDownsampleDefaults pins the three states the age setting has to tell
// apart. The difference matters: an existing config file has no such key, and must
// get the default rather than be read as "off"; an admin who types 0 means off and
// must not have history thinned anyway.
func TestCheckinDownsampleDefaults(t *testing.T) {
	zero, sixty, negative := 0, 90, -5

	cases := []struct {
		name string
		val  *int
		want int
	}{
		{"absent key uses the default", nil, DefaultCheckinDownsampleDays},
		{"explicit 0 means off", &zero, 0},
		{"explicit value is honoured", &sixty, 90},
		{"negative is clamped to off", &negative, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{CheckinDownsampleDaysVal: c.val}
			if got := cfg.CheckinDownsampleDays(); got != c.want {
				t.Errorf("CheckinDownsampleDays() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestCheckinDownsampleSecDefaults covers the bucket. Unlike the age, 0 is not a
// meaningful bucket, so anything non-positive falls back to the default rather than
// disabling the job — disabling is the age setting's job.
func TestCheckinDownsampleSecDefaults(t *testing.T) {
	zero, custom, negative := 0, 600, -1
	for _, c := range []struct {
		name string
		val  *int
		want int
	}{
		{"absent key uses the default", nil, DefaultCheckinDownsampleSec},
		{"zero falls back to the default", &zero, DefaultCheckinDownsampleSec},
		{"negative falls back to the default", &negative, DefaultCheckinDownsampleSec},
		{"explicit value is honoured", &custom, 600},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{CheckinDownsampleSecVal: c.val}
			if got := cfg.CheckinDownsampleSec(); got != c.want {
				t.Errorf("CheckinDownsampleSec() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestDefaultDownsampleIsFiveMinutes states the shipped default outright, so changing
// it is a deliberate edit to a test and not a quiet change to how much history the
// product throws away.
func TestDefaultDownsampleIsFiveMinutes(t *testing.T) {
	if DefaultCheckinDownsampleSec != 300 {
		t.Errorf("default bucket is %ds, expected 300 (5 minutes)", DefaultCheckinDownsampleSec)
	}
	if DefaultCheckinDownsampleDays != 60 {
		t.Errorf("default age is %dd, expected 60", DefaultCheckinDownsampleDays)
	}
}
