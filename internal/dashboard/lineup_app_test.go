package dashboard

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTallyBuilds(t *testing.T) {
	base := "aio.app.nugget"
	ids := make([]uuid.UUID, 6)
	for i := range ids {
		ids[i] = uuid.New()
	}
	installed := map[uuid.UUID]map[string]string{
		ids[0]: {"aio.app.nugget.uatv2": ""},
		ids[1]: {"aio.app.nugget.uatv2": ""},
		ids[2]: {"aio.app.nugget.uatv2": "47.0.0"},
		ids[3]: {"aio.app.nugget.uatv2": "47.0.0", "aio.app.nugget": "17.0.0"},
		ids[4]: {"aio.app.nugget.qa": " "},
		ids[5]: {"aio.app.nugget": "17.0.0"}, // not in the role
	}
	inRole := map[uuid.UUID]bool{ids[0]: true, ids[1]: true, ids[2]: true, ids[3]: true, ids[4]: true}

	on, b := tallyBuilds(base, installed, inRole)
	if on != 5 {
		t.Fatalf("on = %d, want 5 (the device outside the role is not counted)", on)
	}
	if len(b) != 3 || b[0].Package != "aio.app.nugget.uatv2" || b[0].Devices != 4 {
		t.Fatalf("builds = %+v, want uatv2 (4) first", b)
	}
	// A tie between a named version and none: the named one leads, and "—" never shows.
	if b[0].Version != "47.0.0" {
		t.Errorf("uatv2 top version = %q, want 47.0.0", b[0].Version)
	}
	if !strings.Contains(b[0].Tip, "unknown version ×2") || !strings.HasPrefix(b[0].Tip, "aio.app.nugget.uatv2 · ") {
		t.Errorf("tip = %q", b[0].Tip)
	}
	for _, x := range b {
		if x.Version == "" || x.Version == "—" {
			t.Errorf("%s has no version: %+v", x.Package, x)
		}
	}
	var qa lineupBuild
	for _, x := range b {
		if x.Package == "aio.app.nugget.qa" {
			qa = x
		}
	}
	if qa.Version != unknownVersion || qa.Short != ".qa" || qa.Env != "QA" {
		t.Errorf("qa = %+v", qa)
	}
	sum := 0.0
	for _, x := range b {
		sum += x.Pct
	}
	if math.Abs(sum-100) > 0.01 {
		t.Errorf("slices add up to %.2f, want 100", sum)
	}
}
