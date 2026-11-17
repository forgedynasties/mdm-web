package dashboard

import (
	"slices"
	"testing"

	"mdm/internal/product"
)

// A device picks only among its model's roles (Models, 17 Nov 2026): a picker only for a
// model with several, offering exactly those.
func TestDeviceRoleChoicesAreTheModels(t *testing.T) {
	several := deviceRole{Roles: []string{product.ClassPOS, product.ClassKDS}}
	if !several.Picker() || !slices.Equal(several.Choices(), []string{product.ClassPOS, product.ClassKDS}) {
		t.Errorf("several roles: picker %v, choices %v", several.Picker(), several.Choices())
	}
	for _, r := range []deviceRole{{}, {Roles: []string{product.ClassPOS}}, {PerSerial: true}} {
		if r.Picker() {
			t.Errorf("%+v offers a picker", r)
		}
	}
	if perSerialClass(product.ClassT7) || perSerialClass("nope") {
		t.Error("perSerialClass let t7 or an unknown class through")
	}
}
