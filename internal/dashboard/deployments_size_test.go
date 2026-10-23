package dashboard

import (
	"testing"

	"mdm/internal/db"
)

// An incremental install read "of 1.8 GB" because the page took the release's largest
// package; it must take the one the device is offered.
func TestOfferedPackageSize(t *testing.T) {
	inc := map[string]int64{"v2.0.96l": 18 << 20, "v2.0.995": 4530 << 10}
	full := int64(1806 << 20)
	cases := []struct {
		t    db.UpdateTarget
		want int64
	}{
		{db.UpdateTarget{SourceBuildID: "v2.0.96l", BuildID: "v2.0.96l"}, 18 << 20},
		{db.UpdateTarget{BuildID: "v2.0.995"}, 4530 << 10},
		{db.UpdateTarget{SourceBuildID: "v2.0.96l", ForceFull: true}, full},
		{db.UpdateTarget{BuildID: "v2.0.900"}, full},
	}
	for _, c := range cases {
		if got := offeredPackageSize(c.t, inc, full); got != c.want {
			t.Errorf("%+v: got %d, want %d", c.t, got, c.want)
		}
	}
}
