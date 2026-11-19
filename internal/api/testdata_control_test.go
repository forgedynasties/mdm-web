package api

import (
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

func TestQARoleAppMatch(t *testing.T) {
	for pkg, want := range map[string]bool{
		"aio.app.nugget":       true,
		"aio.app.nugget.uatv2": true,
		"aio.app.nuggetx":      false,
		"aio.app.kiosk":        false,
		"com.android.settings": false,
		"aio.app":              false,
	} {
		if got := qaRoleAppMatch("aio.app.nugget", pkg); got != want {
			t.Errorf("qaRoleAppMatch(%q) = %v, want %v", pkg, got, want)
		}
	}
}

func TestQAPackageName(t *testing.T) {
	for pkg, want := range map[string]bool{
		"aio.app.nugget":          true,
		"aio.app.nugget.uatv2":    true,
		"aio.app.nugget;reboot":   false,
		"aio.app.nugget reboot":   false,
		"aio.app.nugget$(reboot)": false,
		"nugget":                  false,
		"":                        false,
	} {
		if got := qaPackageRe.MatchString(pkg); got != want {
			t.Errorf("package %q accepted = %v, want %v", pkg, got, want)
		}
	}
}

func TestQAInScope(t *testing.T) {
	venue := uuid.New()
	// No Lab state (19 Nov 2026): the key sees every device on the server it is set on.
	for name, d := range map[string]*db.Device{
		"a firmware device":      {AgentKind: product.KindFirmware},
		"a device at a venue":    {AgentKind: product.KindFirmware, RestaurantID: &venue},
		"a Standard client unit": {AgentKind: product.KindDPC},
	} {
		if !qaInScope(d) {
			t.Errorf("%s is out of scope", name)
		}
	}
	if qaInScope(&db.Device{Hidden: true}) || qaInScope(nil) {
		t.Error("a hidden or missing device is in scope")
	}
}
