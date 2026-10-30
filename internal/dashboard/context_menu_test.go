package dashboard

import (
	"encoding/json"
	"testing"
)

// The right-click menu offers what the role may do: an engineer never moves devices to a
// venue and is read-only at one; a viewer acts on nothing; Role… never lists Tableside AI.
func TestCtxCaps(t *testing.T) {
	read := func(role string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(ctxCaps(role)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	eng := read("engineer")
	if eng["venue"] != false || eng["venueReadOnly"] != true || eng["group"] != true || eng["screenshot"] != true {
		t.Errorf("engineer caps = %v", eng)
	}
	v := read("viewer")
	for _, k := range []string{"reboot", "lock", "group", "venue", "role", "remote", "screenshot"} {
		if v[k] == true {
			t.Errorf("viewer may %s", k)
		}
	}
	if a := read("admin"); a["admin"] != true || a["venue"] != true {
		t.Errorf("super admin caps = %v", a)
	}
	for _, r := range read("admin")["roles"].([]any) {
		if r.([]any)[0] == "t7" {
			t.Error("Role… offers Tableside AI")
		}
	}
}
