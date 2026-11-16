package dashboard

import "testing"

func TestEnvLabelFor(t *testing.T) {
	for in, want := range map[string]string{
		"https://mdm.dev.aioapp.com":                       "DEV",
		"https://mdm.dev.aioapp.com,https://other.example": "DEV",
		"https://mdm-stage.dev.aioapp.com":                 "STAGE",
		"https://mdm.aioapp.com":                           "",
		"https://MDM.DEV.AIOAPP.COM/":                      "DEV",
		"":                                                 "",
		"https://mdm.aioapp.com,https://mdm.dev.aioapp.com": "",
	} {
		if got := envLabelFor(in); got != want {
			t.Errorf("envLabelFor(%q) = %q, want %q", in, got, want)
		}
	}
}
