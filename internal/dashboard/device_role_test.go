package dashboard

import (
	"slices"
	"testing"

	"mdm/internal/product"
)

// A Standard-client device can be set to any class by serial, except Tableside AI.
func TestPerSerialChoicesLeaveOutTSAI(t *testing.T) {
	got := deviceRole{PerSerial: true}.Choices()
	if slices.Contains(got, product.ClassT7) {
		t.Errorf("TSAI offered: %v", got)
	}
	if !slices.Contains(got, product.ClassKDS) || !slices.Contains(got, product.ClassPOS) {
		t.Errorf("choices = %v", got)
	}
	if perSerialClass(product.ClassT7) || perSerialClass("nope") {
		t.Error("perSerialClass let t7 or an unknown class through")
	}
}
