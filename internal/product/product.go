// Package product is the catalog of hardware product categories the MDM manages
// and the capabilities each one has. It exists so the rest of the server can ask
// "does this device even have a wireless-charging pad?" instead of guessing from
// whether a telemetry key happened to be present.
//
// Historically the fleet was a single product (the T7 tablet, which has a battery,
// a charger, and a wireless-charging guest pad). The Kiosk 18/22/27 variants are
// wall-powered panels with none of that. Without a declared capability set the
// dashboard/alerts can't tell "device legitimately has no pad" from "pad reading
// missing", so wlc/charging widgets and alerts misfire on kiosks. This package is
// the single source of truth that fixes that.
package product

import "strings"

// Caps describes which hardware features a product category has. A false flag means
// the hardware is absent, so the corresponding telemetry/alerts/UI should be hidden
// rather than shown as "missing" or "0".
type Caps struct {
	// HasBattery is true when the device runs on an internal battery (battery %,
	// temperature, discharge-cycle accounting are meaningful).
	HasBattery bool
	// HasCharging is true when the device has a charger input (charging state,
	// charger voltage/type are meaningful).
	HasCharging bool
	// HasWLC is true when the device has a wireless-charging guest pad (wlc_status,
	// wlc_guest_frac aggregates are meaningful).
	HasWLC bool
}

// Product is one hardware category in the catalog.
type Product struct {
	Key   string // stable machine key sent by the client and stored on the device (e.g. "t7", "kiosk27")
	Label string // human label for the dashboard (e.g. "T7", "Kiosk 27")
	Caps  Caps
}

// Keys for the known products. Clients send these verbatim in the check-in payload.
const (
	KeyT7      = "t7"
	KeyKiosk18 = "kiosk18"
	KeyKiosk22 = "kiosk22"
	KeyKiosk27 = "kiosk27"
)

// DefaultKey is assumed when a device reports no product. The existing fleet is all
// T7 and predates the product field, so an empty/unknown product resolves to T7 to
// preserve today's behaviour for already-deployed devices.
const DefaultKey = KeyT7

// catalog is the declared capability set per product. Order here drives dashboard
// dropdown order (see All).
var catalog = []Product{
	{Key: KeyT7, Label: "T7", Caps: Caps{HasBattery: true, HasCharging: true, HasWLC: true}},
	{Key: KeyKiosk18, Label: "Kiosk 18", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}},
	{Key: KeyKiosk22, Label: "Kiosk 22", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}},
	{Key: KeyKiosk27, Label: "Kiosk 27", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}},
}

var byKey = func() map[string]Product {
	m := make(map[string]Product, len(catalog))
	for _, p := range catalog {
		m[p.Key] = p
	}
	return m
}()

// Normalize lower-cases and trims a client-reported product key so minor formatting
// differences ("Kiosk27", " KIOSK27 ") resolve to the same catalog entry.
func Normalize(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// Resolve returns the product for a (possibly empty or unknown) key, falling back to
// the default (T7) so callers always get a usable capability set. The second return
// is false when the key was empty/unrecognised and the default was substituted.
func Resolve(key string) (Product, bool) {
	if p, ok := byKey[Normalize(key)]; ok {
		return p, true
	}
	return byKey[DefaultKey], false
}

// CapsFor is the common shortcut: capabilities for a product key, default-substituted.
func CapsFor(key string) Caps {
	p, _ := Resolve(key)
	return p.Caps
}

// Label is the display label for a product key, default-substituted.
func Label(key string) string {
	p, _ := Resolve(key)
	return p.Label
}

// IsKnown reports whether the key names a catalog product (not the fallback).
func IsKnown(key string) bool {
	_, ok := byKey[Normalize(key)]
	return ok
}

// All returns the catalog in display order (used to build dashboard filters).
func All() []Product {
	out := make([]Product, len(catalog))
	copy(out, catalog)
	return out
}
