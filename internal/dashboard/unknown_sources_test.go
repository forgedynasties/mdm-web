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

// Every check reads as a problem when it fails, and a detail that only repeats it is dropped.
func TestEveryCheckHasAProblem(t *testing.T) {
	for _, c := range complianceChecks {
		if c.Problem == "" || c.Problem == c.Name {
			t.Errorf("%s: problem %q", c.ID, c.Problem)
		}
	}
	if !restates("Developer options on", "Developer options are on") || !restates("Storage not encrypted", "Storage is not encrypted") {
		t.Error("restates misses a plain repeat")
	}
	if restates("Rooted", "An su binary is present") {
		t.Error("restates drops a detail that adds something")
	}
}
