package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fleet server must never offer the reset: a request that names prod in its Host
// header changes nothing, only the server's own PUBLIC_ORIGIN or ALLOW_FLEET_RESET do.
func TestFleetResetHiddenOffProd(t *testing.T) {
	t.Setenv("ALLOW_FLEET_RESET", "")
	for _, origin := range []string{"https://mdm.dev.aioapp.com", "", "https://mdm-stage.dev.aioapp.com,https://mdm.aioapp.com"} {
		t.Setenv("PUBLIC_ORIGIN", origin)
		h := &Handler{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/fleet-reset", strings.NewReader(`{"confirm":"mdm.aioapp.com"}`))
		req.Host = "mdm.aioapp.com"
		rec := httptest.NewRecorder()
		h.FleetReset(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("PUBLIC_ORIGIN %q: got %d, want 404", origin, rec.Code)
		}
	}
}

func TestOwnHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://mdm.aioapp.com":                             "mdm.aioapp.com",
		"https://MDM.aioapp.com/,https://mdm.dev.aioapp.com": "mdm.aioapp.com",
		"https://mdm.aioapp.com:443":                         "mdm.aioapp.com",
		"":                                                   "",
	} {
		t.Setenv("PUBLIC_ORIGIN", in)
		if got := ownHost(); got != want {
			t.Errorf("ownHost(%q) = %q, want %q", in, got, want)
		}
	}
}
