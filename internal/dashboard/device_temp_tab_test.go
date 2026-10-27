package dashboard

import (
	"encoding/json"
	"testing"
)

func TestDeviceReportsTemp(t *testing.T) {
	for extra, want := range map[string]bool{
		`{"battery_temp_c":41.2}`: true,
		`{"cpu_temp_c":61}`:       true,
		`{"charging":false}`:      false, // Sunmi D2s KDS: no sensor the client can read
		``:                        false,
	} {
		if got := deviceReportsTemp(json.RawMessage(extra)); got != want {
			t.Errorf("%q: got %v, want %v", extra, got, want)
		}
	}
}
