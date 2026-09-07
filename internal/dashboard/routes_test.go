package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDeviceRoutesAreGuarded fails the build when a /devices/{serial}/... route
// is registered without the deviceRoute guard, so a new endpoint cannot leak a
// hidden device by URL. It reads RegisterRoutes from source because ServeMux
// patterns cannot be enumerated at runtime.
func TestDeviceRoutesAreGuarded(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"(GET|POST) /devices/\{serial\}[^"]*", h\.[A-Za-z]+\(([^\n]*)\)`)
	var bad []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if !strings.Contains(m[2], "h.deviceRoute(") {
			bad = append(bad, m[0])
		}
	}
	if len(bad) > 0 {
		t.Fatalf("device routes without deviceRoute guard:\n%s", strings.Join(bad, "\n"))
	}
}

// TestListRoutesScoped checks the list-style handlers that must narrow their
// results to the caller's visible devices keep doing so.
func TestListRoutesScoped(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	handlers := []string{"DeviceList", "DeviceMapData", "DeviceSearch", "DeviceSelectAllSerials", "TargetCountJSON", "AlertList", "AlertNewest", "AlertsRecent", "GroupDetail", "GroupMembers", "RestaurantDetail", "RestaurantMembers", "CommandBrowseDevices", "CommandList", "CommandHistory", "CommandDetail", "FleetEvents", "Overview", "FleetHealth", "OwnerHome"}
	for _, name := range handlers {
		i := strings.Index(s, "func (h *Handler) "+name+"(")
		if i < 0 {
			t.Errorf("%s: handler not found", name)
			continue
		}
		end := strings.Index(s[i:], "\n}\n")
		body := s[i : i+end]
		if !strings.Contains(body, "visibleIDs()") && !strings.Contains(body, "applyFilter(") && !strings.Contains(body, "keepVisible") && !strings.Contains(body, "filterHiddenCommands(") && !strings.Contains(body, "hidesDevices()") && !strings.Contains(body, "acc.visible(") && !strings.Contains(body, "deviceFilterFromRequest(") {
			t.Errorf("%s: no visibility scoping call in handler body", name)
		}
	}
}
