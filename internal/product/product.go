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
//
// Devices OUTSIDE the catalog split two ways:
//   - An EMPTY product is a legacy device that predates the product field — the
//     whole pre-product fleet is T7, so empty still resolves to T7 exactly as it
//     always has (labels, caps, release/OTA matching all depend on this).
//   - An unknown NON-EMPTY key (a stock Pixel running the DPC agent, an emulator
//     like "sdk_gphone64_x86_64") is NOT a T7 and must stop masquerading as one:
//     it resolves to its own normalized key with generic Android caps (battery +
//     charger, no bespoke wireless-charging guest pad).
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
	// Kind is how the product is managed: KindFirmware for our own hardware (system-app
	// client, shared key, auto-enrol on first check-in) or KindAndroid for stock devices
	// managed by the Device-Owner DPC agent (explicit enrollment, per-device key).
	Kind string
	// Class is the category devices of this product belong to. Our own hardware is
	// categorised by model (ClassT7 for the T7, ClassKiosk for the wall kiosks) —
	// the model IS the category. Empty for non-catalog products: their class is
	// guessed from the reported model and set per device, not implied by hardware.
	Class string
	// DisplayName is what a device of this product is called on its fleet row and
	// device page (the T7 is sold as "TSAI" — Tableside AI); "" = use Label. Label
	// stays the product name everywhere products are listed or filtered.
	DisplayName string
}

// Keys for the known products. Clients send these verbatim in the check-in payload.
const (
	KeyT7      = "t7"
	KeyKiosk18 = "kiosk18"
	KeyKiosk22 = "kiosk22"
	KeyKiosk27 = "kiosk27"
)

// DefaultKey is assumed when a device reports no product. The existing fleet is all
// T7 and predates the product field, so an EMPTY product resolves to T7 to preserve
// today's behaviour for already-deployed devices. (Unknown non-empty products do NOT
// fall back to the default — see Resolve.)
const DefaultKey = KeyT7

// genericCaps is the capability set assumed for any non-empty product outside the
// catalog: standard Android hardware (battery + charger), no bespoke
// wireless-charging guest pad (that pad is T7-only hardware).
var genericCaps = Caps{HasBattery: true, HasCharging: true, HasWLC: false}

// catalog is the declared capability set per product. Order here drives dashboard
// dropdown order (see All).
var catalog = []Product{
	{Key: KeyT7, Label: "T7", Caps: Caps{HasBattery: true, HasCharging: true, HasWLC: true}, Kind: KindFirmware, Class: ClassT7, DisplayName: "TSAI"},
	{Key: KeyKiosk18, Label: "Kiosk 18", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}, Kind: KindFirmware, Class: ClassKiosk},
	{Key: KeyKiosk22, Label: "Kiosk 22", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}, Kind: KindFirmware, Class: ClassKiosk},
	{Key: KeyKiosk27, Label: "Kiosk 27", Caps: Caps{HasBattery: false, HasCharging: false, HasWLC: false}, Kind: KindFirmware, Class: ClassKiosk},
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

// Resolve returns the product for a (possibly empty or unknown) key. Known keys get
// their catalog entry (ok=true). An EMPTY key is a legacy pre-product device and
// resolves to the default (T7) — unchanged behaviour the existing fleet depends on.
// An unknown NON-EMPTY key synthesizes a product from the key itself (the reported
// key doubles as the label) with generic Android caps, so e.g. a DPC-managed Pixel
// stops masquerading as a T7. ok=false in both non-catalog cases.
func Resolve(key string) (Product, bool) {
	norm := Normalize(key)
	if p, ok := byKey[norm]; ok {
		return p, true
	}
	if norm == "" {
		return byKey[DefaultKey], false
	}
	return Product{Key: norm, Label: strings.TrimSpace(key), Caps: genericCaps, Kind: KindAndroid}, false
}

// CapsFor is the common shortcut: capabilities for a product key (T7 caps when the
// key is empty, generic Android caps when it is unknown).
func CapsFor(key string) Caps {
	p, _ := Resolve(key)
	return p.Caps
}

// CapsForDevice is CapsFor with the device's class applied. A stock device's product
// key rarely says what the hardware is ("rockchip029"), so the generic caps assume a
// phone-like battery; the class, once known, overrides that. A dongle (TV box on an
// HDMI screen) is always mains-powered: no battery, no charger, no pad.
func CapsForDevice(key, class string) Caps {
	if class == ClassDongle {
		return Caps{}
	}
	return CapsFor(key)
}

// BatteryPredicateSQL is a WHERE fragment, for the devices table aliased as `alias`,
// true only for devices that have a battery — the SQL twin of CapsForDevice().HasBattery.
// Battery-less devices still store a latest_battery_pct (often 0), and without this
// every low-battery count and filter would include them.
func BatteryPredicateSQL(alias string) string {
	var none []string
	for _, p := range catalog {
		if !p.Caps.HasBattery {
			none = append(none, "'"+p.Key+"'")
		}
	}
	pred := alias + ".device_class <> '" + ClassDongle + "'"
	if len(none) > 0 {
		// An empty product resolves to the default (T7), which has a battery.
		pred += " AND COALESCE(NULLIF(" + alias + ".product, ''), '" + DefaultKey + "') NOT IN (" + strings.Join(none, ", ") + ")"
	}
	return "(" + pred + ")"
}

// Label is the display label for a product key: the catalog label for known keys,
// the T7 default for empty (legacy) ones, the reported key itself for unknown ones.
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
