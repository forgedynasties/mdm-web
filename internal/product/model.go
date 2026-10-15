package product

import "strings"

// ClassForModel guesses the category of a stock (DPC-managed) device from what it
// reports about itself: the product key (Build.PRODUCT) plus the manufacturer and
// model it sends in the check-in extra.
//
// Our own hardware never needs this — its product key is in the catalog and the
// class comes from there. Stock devices report names nobody curates ("rk3399_android11",
// "SM-S146VL"), so this is a best-effort first guess that saves the admin a click:
// it is only ever used to fill an EMPTY class, and an admin's choice always wins
// (see the enrollment inbox and the per-device class override).
//
// Returns "" when nothing matches, which leaves the device in "type needed" — a
// wrong guess is worse than no guess, so the rules below stay narrow.
func ClassForModel(productKey, manufacturer, model string) string {
	hay := strings.ToLower(strings.TrimSpace(productKey + " " + manufacturer + " " + model))
	if hay = strings.TrimSpace(hay); hay == "" {
		return ""
	}
	// Order matters: the first hit wins, so the more specific families come first; a
	// kiosk that happens to be a POS terminal is still a kiosk.
	for _, r := range []struct {
		class string
		keys  []string
	}{
		// TV boxes first: their model strings carry short tokens ("d8").
		{ClassDongle, []string{"rk3528", "rbox", "hk1", "tvbox", "tv box", "dongle"}},
		{ClassKiosk, []string{"kiosk", "rk3399", "rk3288", "rk3568", "panel"}},
		{ClassPOS, []string{"pos", "t2lite", "t3", "counter", "desktop"}},
	} {
		for _, k := range r.keys {
			if strings.Contains(hay, k) {
				return r.class
			}
		}
	}
	return ""
}
