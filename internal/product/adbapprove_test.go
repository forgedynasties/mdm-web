package product

import (
	"encoding/json"
	"testing"
)

func TestValidateAdbApprove(t *testing.T) {
	fp := `"fingerprint":"AB:CD:EF:01:23:45:67:89:ab:cd:ef:01:23:45:67:89"`
	cases := []struct {
		name, targetType string
		targets          int
		payload          string
		ok               bool
	}{
		{"one device, fingerprint", "devices", 1, `{` + fp + `}`, true},
		{"no colons", "devices", 1, `{"fingerprint":"abcdef0123456789abcdef0123456789","wait_s":120}`, true},
		{"two devices", "devices", 2, `{` + fp + `}`, false},
		{"a group", "groups", 1, `{` + fp + `}`, false},
		{"everyone", "all", 0, `{` + fp + `}`, false},
		{"no fingerprint", "devices", 1, `{}`, false},
		{"empty payload", "devices", 1, ``, false},
		{"short fingerprint", "devices", 1, `{"fingerprint":"AB:CD"}`, false},
		{"not hex", "devices", 1, `{"fingerprint":"zzcdef0123456789abcdef0123456789"}`, false},
		{"wait too long", "devices", 1, `{` + fp + `,"wait_s":900}`, false},
	}
	for _, c := range cases {
		msg := ValidateAdbApprove(c.targetType, c.targets, json.RawMessage(c.payload))
		if (msg == "") != c.ok {
			t.Errorf("%s: got %q, want ok=%v", c.name, msg, c.ok)
		}
	}
}
