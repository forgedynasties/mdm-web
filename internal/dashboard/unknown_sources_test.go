package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"mdm/internal/db"
)

func TestUnknownSourcesIgnoresTheMDM(t *testing.T) {
	cases := []struct {
		extra, want, detail string
	}{
		// 031 on 26 Oct: the MDM client and Nugget listed, the old global setting on.
		{`{"unknown_sources":true,"unknown_source_apps":["aio.app.nugget","com.aioapp.mdm"]}`, db.ComplianceFail, "Allowed for aio.app.nugget"},
		// Only the MDM (and the ignored global setting): passes.
		{`{"unknown_sources":true,"unknown_source_apps":["com.aioapp.mdm"]}`, db.CompliancePass, "None allowed"},
		{`{"unknown_sources":true,"unknown_source_apps":["aio.app.mdmclient.dpc"]}`, db.CompliancePass, "None allowed"},
		// An older client with no list: its flag decides.
		{`{"unknown_sources":true}`, db.ComplianceFail, "An app may install other apps"},
		{`{}`, db.ComplianceUnknown, "Not reported"},
	}
	for _, c := range cases {
		got, detail := evalUnknownSources(nil, &db.Device{LatestExtra: json.RawMessage(c.extra)}, 0)
		if got != c.want || !strings.HasPrefix(detail, c.detail) {
			t.Errorf("%s: %s %q, want %s %q", c.extra, got, detail, c.want, c.detail)
		}
	}
}
