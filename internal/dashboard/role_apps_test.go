package dashboard

import (
	"testing"

	"mdm/internal/db"
)

func TestCompareVersionNames(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"4.2.1", "4.2.1", 0},
		{"4.10.0", "4.9.2", 1},
		{"4.2", "4.2.1", -1},
		{"v2.1.030", "v2.1.029", 1},
		{"4.3.0-rc2", "4.3.0", -1},
		{"4.3.0", "4.3.0-rc2", 1},
		{"4.3.1-rc1", "4.3.0", 1},
		{"1.0", "1.0.0", -1},
	} {
		if got := compareVersionNames(c.a, c.b); got != c.want {
			t.Errorf("compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPlanAppStep(t *testing.T) {
	prodBuild := &db.App{PackageName: "aio.app.nugget", VersionName: "4.2.1"}
	internal := &db.App{PackageName: "aio.app.nugget.internal", VersionName: "4.3.0"}
	base := "aio.app.nugget"

	// Nothing set: leave alone.
	if s := planAppStep(base, map[string]string{"aio.app.nugget": "1.0"}, nil); s.Install != nil || len(s.Uninstall) > 0 {
		t.Fatalf("nil target should do nothing, got %+v", s)
	}
	// Missing: install.
	if s := planAppStep(base, map[string]string{}, prodBuild); s.Install != prodBuild {
		t.Fatalf("missing app should install, got %+v", s)
	}
	// Behind: install, and do not touch other packages yet.
	s := planAppStep(base, map[string]string{"aio.app.nugget": "4.2.0", "aio.app.nugget.internal": "4.3.0"}, prodBuild)
	if s.Install != prodBuild || len(s.Uninstall) != 0 {
		t.Fatalf("behind should install first, got %+v", s)
	}
	// On target: remove the other variants, but never packages outside the family.
	s = planAppStep(base, map[string]string{"aio.app.nugget": "4.2.1", "aio.app.nugget.internal": "4.3.0", "aio.app.nuggetx": "1"}, prodBuild)
	if s.Install != nil || len(s.Uninstall) != 1 || s.Uninstall[0] != "aio.app.nugget.internal" {
		t.Fatalf("on target should remove the internal variant only, got %+v", s)
	}
	// Lab on the internal variant: install it, prod stays until it lands.
	s = planAppStep(base, map[string]string{"aio.app.nugget": "4.2.1"}, internal)
	if s.Install != internal || len(s.Uninstall) != 0 {
		t.Fatalf("lab move should install internal first, got %+v", s)
	}
	// Ahead of the channel: never downgrade in place.
	s = planAppStep(base, map[string]string{"aio.app.nugget": "4.3.0"}, prodBuild)
	if s.Install != nil || !s.Ahead {
		t.Fatalf("newer build on device must be left alone, got %+v", s)
	}
}

func TestDefaultFirmwareMode(t *testing.T) {
	for model, want := range map[string]string{"t7": db.FirmwareProd, "kiosk22": db.FirmwareLab, "kiosk27": db.FirmwareLab, "rk3568_r": db.FirmwareOff} {
		if got := db.DefaultFirmwareMode(model); got != want {
			t.Errorf("DefaultFirmwareMode(%q) = %q, want %q", model, got, want)
		}
	}
	if got := db.FirmwareModeOf(map[string]string{"kiosk22": "prod"}, "kiosk22"); got != db.FirmwareProd {
		t.Errorf("explicit mode should win, got %q", got)
	}
	if got := db.FirmwareModeOf(nil, ""); got != db.FirmwareProd {
		t.Errorf("empty product is the default product (t7), got %q", got)
	}
}

func TestFirmwareBehind(t *testing.T) {
	for _, c := range []struct {
		build, prod string
		want        bool
	}{
		{"v2.1.029", "v2.1.030", true},
		{"v2.1.030", "v2.1.030", false},
		{"v2.1.031", "v2.1.030", false},
		{"v2.0.997", "v2.1.030", false}, // other line: a migration, not this job
		{"v2.0.4l", "v2.1.030", false},
		{"v2.1.025-aio-glance-23", "v2.1.030", true},
		{"", "v2.1.030", false},
		{"A15-v1.62-user", "v2.1.030", false},
	} {
		if got := firmwareBehind(c.build, c.prod); got != c.want {
			t.Errorf("firmwareBehind(%q, %q) = %v, want %v", c.build, c.prod, got, c.want)
		}
	}
}
