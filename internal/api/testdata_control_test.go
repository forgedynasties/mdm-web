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
	if !qaInScope(&db.Device{AgentKind: product.KindFirmware}) {
		t.Error("a Lab firmware device is in scope")
	}
	if qaInScope(&db.Device{AgentKind: product.KindFirmware, RestaurantID: &venue}) {
		t.Error("a device at a venue is out of scope")
	}
	if qaInScope(&db.Device{AgentKind: product.KindDPC}) {
		t.Error("a Standard client device is out of scope")
	}
}
