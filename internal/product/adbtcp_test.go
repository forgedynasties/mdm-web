package product

import (
	"encoding/json"
	"testing"
)

func TestValidateAdbTcp(t *testing.T) {
	cases := []struct {
		name       string
		targetType string
		targets    int
		payload    string
		ok         bool
	}{
		{"one device, defaults", "devices", 1, ``, true},
		{"one device, on for 2 h", "devices", 1, `{"port":5555,"hours":2}`, true},
		{"off", "devices", 1, `{"port":0}`, true},
		{"stays on", "devices", 1, `{"port":5555,"hours":0}`, true},
		{"all devices", "all", 0, ``, false},
		{"a group", "groups", 1, ``, false},
		{"two devices", "devices", 2, ``, false},
		{"privileged port", "devices", 1, `{"port":22}`, false},
		{"port too high", "devices", 1, `{"port":70000}`, false},
		{"hours too long", "devices", 1, `{"hours":1000}`, false},
		{"not an object", "devices", 1, `[1]`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := ValidateAdbTcp(c.targetType, c.targets, json.RawMessage(c.payload))
			if (msg == "") != c.ok {
				t.Fatalf("got %q, want ok=%v", msg, c.ok)
			}
		})
	}
}
