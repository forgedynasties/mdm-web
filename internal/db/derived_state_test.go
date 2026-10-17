package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// host_app_fg is the one state bit of extra.host_app: the variant in use, in front or
// not. It is what host_app_away reads, so it must follow the variant the report names.
func TestWithDerivedStateHostApp(t *testing.T) {
	cases := map[string]string{
		`{"host_app":{"package":"aio.app.nugget","variants":[{"package":"aio.app.nugget.qa","foreground":true},{"package":"aio.app.nugget","foreground":false}]}}`: "false",
		`{"host_app":{"package":"aio.app.nugget","variants":[{"package":"aio.app.nugget","foreground":true}]}}`:                                                    "true",
		`{"host_app":{"package":"aio.app.nugget"}}`: "", // MDM-lite's shape: no variants, no bit
		`{"host_app":{"package":"","variants":[]}}`: "",
		`{"charging":true}`:                         "",
	}
	for in, want := range cases {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatal(err)
		}
		withDerivedState(m)
		if got := string(m["host_app_fg"]); got != want {
			t.Errorf("%s: host_app_fg = %q, want %q", in, got, want)
		}
	}
}

// A change of the bit becomes a state event, like any report field in stateKeys.
func TestHostAppForegroundBecomesAnEvent(t *testing.T) {
	dev := uuid.New()
	on := json.RawMessage(`{"host_app":{"package":"p","variants":[{"package":"p","foreground":true}]}}`)
	off := json.RawMessage(`{"host_app":{"package":"p","variants":[{"package":"p","foreground":false}]}}`)
	b, err := shapeRows([]ShapedTelemetry{{DeviceID: dev, At: time.Now(), Prev: on, Cur: off}})
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range b.evKey {
		if k == "host_app_fg" {
			if b.evFrom[i] != "true" || b.evTo[i] != "false" || b.evSeed[i] {
				t.Errorf("event %s→%s seed=%v, want true→false", b.evFrom[i], b.evTo[i], b.evSeed[i])
			}
			return
		}
	}
	t.Errorf("no host_app_fg event among %v", b.evKey)
}
