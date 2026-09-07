package product

import "testing"

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
