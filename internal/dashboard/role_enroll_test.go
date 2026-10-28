package dashboard

import "testing"

// AIO Enroll is for every AIO team role, Viewer included; customers stay out, and the
// fleet adb key stays with Operator and up.
func TestRoleCanEnroll(t *testing.T) {
	for role, want := range map[string]bool{
		"viewer": true, "support": true, "operator": true, "fleet_manager": true, "fleet_admin": true, "admin": true,
		"owner": false, "staff": false, "": false, "nobody": false,
	} {
		if got := roleCanEnroll(role); got != want {
			t.Errorf("roleCanEnroll(%q) = %v, want %v", role, got, want)
		}
	}
	for role, want := range map[string]bool{"viewer": false, "support": false, "operator": true, "admin": true} {
		if got := roleCanFetchFleetKey(role); got != want {
			t.Errorf("roleCanFetchFleetKey(%q) = %v, want %v", role, got, want)
		}
	}
}
