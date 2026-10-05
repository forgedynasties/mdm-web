package dashboard

import (
	"testing"

	"github.com/google/uuid"
)

func TestMapSiteKey(t *testing.T) {
	rid := uuid.New()
	// A restaurant wins over position: wifi fixes for one site's tablets differ by metres.
	if a, b := mapSiteKey(&rid, 36.1000, -115.1000), mapSiteKey(&rid, 36.1004, -115.1004); a != b {
		t.Errorf("same restaurant, nearby fixes: %q != %q", a, b)
	}
	other := uuid.New()
	if mapSiteKey(&rid, 36.1, -115.1) == mapSiteKey(&other, 36.1, -115.1) {
		t.Error("two restaurants at one spot must stay separate sites")
	}
	// Without a restaurant, devices within a grid cell share a site, distant ones do not.
	if mapSiteKey(nil, 36.10001, -115.10001) != mapSiteKey(nil, 36.10005, -115.10005) {
		t.Error("devices a few metres apart should share a site")
	}
	if mapSiteKey(nil, 36.1, -115.1) == mapSiteKey(nil, 36.2, -115.1) {
		t.Error("devices 11 km apart must not share a site")
	}
}

func TestDistanceMeters(t *testing.T) {
	// Los Angeles to New York is about 3,936 km.
	if d := distanceMeters(34.0522, -118.2437, 40.7128, -74.0060); d < 3_900_000 || d > 3_970_000 {
		t.Errorf("LA-NYC = %.0f m", d)
	}
	if d := distanceMeters(36.1, -115.1, 36.1, -115.1); d != 0 {
		t.Errorf("same point = %.1f m", d)
	}
	// 0.001 degrees of latitude is about 111 m.
	if d := distanceMeters(36.100, -115.1, 36.101, -115.1); d < 105 || d > 117 {
		t.Errorf("0.001 deg = %.1f m", d)
	}
}

func TestMapRegion(t *testing.T) {
	for addr, want := range map[string]string{
		"6489 Camden Ave, San Jose, CA 95120, USA":                  "United States",
		"J58C+M34, C-9, Gulberg Greens Block C Islamabad, Pakistan": "Pakistan",
		"1 Rue de Rivoli, Paris, France":                            "France",
		"6489 Camden Ave, San Jose, CA 95120":                       "",
		"No commas":                                                 "",
		"":                                                          "",
	} {
		if got := mapRegion(addr); got != want {
			t.Errorf("mapRegion(%q) = %q, want %q", addr, got, want)
		}
	}
}
