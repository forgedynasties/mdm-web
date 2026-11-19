package dashboard

import (
	"encoding/json"
	"testing"
)

// The right-click menu offers what the role may do: a viewer acts on nothing; Role… never lists Tableside AI.
func TestCtxCaps(t *testing.T) {
	read := func(role string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(ctxCaps(role)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	sup := read("operator")
	if sup["venue"] != true || sup["group"] != true || sup["screenshot"] != true {
		t.Errorf("support caps = %v", sup)
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
