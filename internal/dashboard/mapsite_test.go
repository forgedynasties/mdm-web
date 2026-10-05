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
