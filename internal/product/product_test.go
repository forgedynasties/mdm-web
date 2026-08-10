package product

import "testing"

func TestResolveDefaults(t *testing.T) {
	// Empty / unknown / legacy devices must resolve to the T7 default so existing
	// behaviour (wlc + charging widgets) is preserved for the pre-product fleet.
	for _, key := range []string{"", "   ", "unknown-thing"} {
		p, known := Resolve(key)
		if known {
			t.Errorf("Resolve(%q): expected known=false", key)
		}
		if p.Key != DefaultKey {
			t.Errorf("Resolve(%q): got %q, want default %q", key, p.Key, DefaultKey)
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
}

func TestLabels(t *testing.T) {
	if got := Label(KeyKiosk22); got != "Kiosk 22" {
		t.Errorf("Label(kiosk22): got %q", got)
	}
}
