package product

import (
	"strings"
	"testing"
)

func TestResolveEmptyStaysT7(t *testing.T) {
	// Empty / whitespace-only product = legacy device that predates the product
	// field. The whole pre-product fleet is T7, so this MUST keep resolving to the
	// T7 default (label, caps, key) — release/OTA matching and the dashboard's
	// wlc/charging widgets for the legacy fleet all depend on it.
	for _, key := range []string{"", "   "} {
		p, known := Resolve(key)
		if known {
			t.Errorf("Resolve(%q): expected known=false", key)
		}
		if p.Key != DefaultKey {
			t.Errorf("Resolve(%q): got key %q, want default %q", key, p.Key, DefaultKey)
		}
		if p.Label != "T7" {
			t.Errorf("Resolve(%q): got label %q, want %q", key, p.Label, "T7")
		}
		if !p.Caps.HasWLC || !p.Caps.HasCharging || !p.Caps.HasBattery {
			t.Errorf("Resolve(%q): got caps %+v, want full T7 caps", key, p.Caps)
		}
	}
}

func TestResolveUnknownNonEmpty(t *testing.T) {
	// Unknown NON-empty products (DPC-managed Pixels, emulators, vendor tablets)
	// must NOT masquerade as T7: they resolve to their own normalized key with the
	// raw report as label and generic Android caps (battery + charger, no WLC pad).
	for _, tc := range []struct{ key, wantKey, wantLabel string }{
		{"a14xmtfn", "a14xmtfn", "a14xmtfn"},
		{"sdk_gphone64_x86_64", "sdk_gphone64_x86_64", "sdk_gphone64_x86_64"},
		{" Pixel8 ", "pixel8", "Pixel8"},
		{"unknown-thing", "unknown-thing", "unknown-thing"},
	} {
		p, known := Resolve(tc.key)
		if known {
			t.Errorf("Resolve(%q): expected known=false", tc.key)
		}
		if p.Key != tc.wantKey {
			t.Errorf("Resolve(%q): got key %q, want %q", tc.key, p.Key, tc.wantKey)
		}
		if p.Label != tc.wantLabel {
			t.Errorf("Resolve(%q): got label %q, want %q", tc.key, p.Label, tc.wantLabel)
		}
		if !p.Caps.HasBattery || !p.Caps.HasCharging || p.Caps.HasWLC {
			t.Errorf("Resolve(%q): got caps %+v, want generic Android caps (battery+charging, no WLC)", tc.key, p.Caps)
		}
	}
}

func TestResolveNormalizes(t *testing.T) {
	for _, key := range []string{"kiosk27", "KIOSK27", " Kiosk27 "} {
		p, known := Resolve(key)
		if !known || p.Key != KeyKiosk27 {
			t.Errorf("Resolve(%q): got (%q, %v), want kiosk27 known", key, p.Key, known)
		}
	}
}

func TestCaps(t *testing.T) {
	if c := CapsFor(KeyT7); !c.HasWLC || !c.HasCharging || !c.HasBattery {
		t.Errorf("T7 caps: got %+v, want all true", c)
	}
	for _, k := range []string{KeyKiosk18, KeyKiosk22, KeyKiosk27} {
		if c := CapsFor(k); c.HasWLC || c.HasCharging || c.HasBattery {
			t.Errorf("%s caps: got %+v, want all false", k, c)
		}
	}
	// Legacy/empty must keep T7 hardware assumptions.
	if c := CapsFor(""); !c.HasWLC {
		t.Errorf("empty caps: got %+v, want T7 (HasWLC true)", c)
	}
	// Unknown non-empty gets generic Android caps: no WLC pad, but battery/charging.
	if c := CapsFor("a14xmtfn"); c.HasWLC || !c.HasBattery || !c.HasCharging {
		t.Errorf("a14xmtfn caps: got %+v, want generic (no WLC, battery+charging)", c)
	}
}

func TestCatalogClasses(t *testing.T) {
	// Our own hardware is categorised by model: the T7 is its own category (not a
	// generic tablet) and every wall kiosk is a Kiosk.
	if p, _ := Resolve(KeyT7); p.Class != ClassT7 {
		t.Errorf("t7 class: got %q, want %q", p.Class, ClassT7)
	}
	if p, _ := Resolve(""); p.Class != ClassT7 {
		t.Errorf("legacy empty class: got %q, want %q", p.Class, ClassT7)
	}
	for _, k := range []string{KeyKiosk18, KeyKiosk22, KeyKiosk27} {
		if p, _ := Resolve(k); p.Class != ClassKiosk {
			t.Errorf("%s class: got %q, want %q", k, p.Class, ClassKiosk)
		}
	}
	// A non-catalog product has no implied class — it is guessed/assigned per device.
	if p, _ := Resolve("a14xmtfn"); p.Class != "" {
		t.Errorf("unknown product class: got %q, want empty", p.Class)
	}
	// The picker no longer offers the retired panel class, but a row still holding it
	// must render.
	for _, c := range Classes() {
		if c == ClassPanel {
			t.Error("Classes() still offers the retired panel class")
		}
	}
	if got := ClassLabel(ClassPanel); got != "Panel" {
		t.Errorf("ClassLabel(panel): got %q, want Panel", got)
	}
	if got := ClassLabel(ClassT7); got != "T7" {
		t.Errorf("ClassLabel(t7): got %q, want T7", got)
	}
}

func TestClassForModel(t *testing.T) {
	// The three stock devices in the fleet today, with the class an admin already
	// chose for each by hand — the guess must reproduce those.
	for _, tc := range []struct {
		productKey, mfr, model, want string
	}{
		{"d3_pro", "SUNMI", "D3 PRO", ClassMPOS},
		{"rk3399_android11", "telpo", "K20", ClassKiosk},
		{"a14xmtfn", "samsung", "SM-S146VL", ClassMPOS},
		{"kiosk_x", "vendor", "Self Service 21", ClassKiosk},
		{"sm-x200", "samsung", "Galaxy Tab A8", ClassTablet},
		{"rockchip029", "Google", "HK1 RBOX D8", ClassDongle},
		{"sdk_gphone64_x86_64", "Google", "sdk_gphone64_x86_64", ""},
		{"", "", "", ""},
	} {
		if got := ClassForModel(tc.productKey, tc.mfr, tc.model); got != tc.want {
			t.Errorf("ClassForModel(%q,%q,%q): got %q, want %q", tc.productKey, tc.mfr, tc.model, got, tc.want)
		}
	}
}

func TestLabels(t *testing.T) {
	if got := Label(KeyKiosk22); got != "Kiosk 22" {
		t.Errorf("Label(kiosk22): got %q", got)
	}
	if got := Label(""); got != "T7" {
		t.Errorf("Label(\"\"): got %q, want T7", got)
	}
	if got := Label("a14xmtfn"); got != "a14xmtfn" {
		t.Errorf("Label(a14xmtfn): got %q, want a14xmtfn", got)
	}
}

func TestCapsForDevice(t *testing.T) {
	// An unknown product gets generic (battery) caps; the dongle class overrides them.
	if !CapsForDevice("rockchip029", "").HasBattery {
		t.Error("unclassified stock device: want generic caps with a battery")
	}
	if c := CapsForDevice("rockchip029", ClassDongle); c.HasBattery || c.HasCharging || c.HasWLC {
		t.Errorf("dongle caps: got %+v, want none", c)
	}
	if !CapsForDevice(KeyT7, ClassT7).HasBattery {
		t.Error("T7 lost its battery")
	}
}

func TestBatteryPredicateSQL(t *testing.T) {
	got := BatteryPredicateSQL("d")
	for _, want := range []string{"d.device_class <> 'dongle'", "'kiosk18'", "'kiosk22'", "'kiosk27'", "'" + DefaultKey + "')"} {
		if !strings.Contains(got, want) {
			t.Errorf("BatteryPredicateSQL: %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "'"+KeyT7+"',") || strings.Contains(got, "IN ('"+KeyT7) {
		t.Errorf("BatteryPredicateSQL excludes the T7: %q", got)
	}
}
