package dashboard

import (
	"strings"
	"testing"

	"mdm/internal/config"
)

// 0.15.3 was built with code 59 after 0.16.1 shipped as code 58: the table orders by
// version name, and says why the 0.15.3 devices can't take the hosted 0.16.1.
func TestClientVersionRowsOrderByName(t *testing.T) {
	sel := &ClientSlotView{
		Hosted: true,
		Build:  config.AgentAPKBuild{Version: "0.16.1", VersionCode: 58},
		Rows: []ClientDeviceRow{
			{Serial: "A", Version: "0.15.3", Code: 59, State: "current"},
			{Serial: "B", Version: "0.16.1", Code: 58, State: "current"},
			{Serial: "C", Version: "0.15.2", Code: 56, State: "behind"},
		},
		History: []ClientHistoryRow{{Build: config.AgentAPKBuild{Version: "0.16.1", VersionCode: 58}, Hosted: true}},
	}
	rows := clientVersionRows(sel, "0.16.1")
	var got []string
	for _, r := range rows {
		got = append(got, r.Version)
	}
	if strings.Join(got, ",") != "0.16.1,0.15.3,0.15.2" {
		t.Fatalf("order = %v", got)
	}
	if rows[1].Kind != "behind" || !strings.Contains(rows[1].Detail, "can't update") {
		t.Fatalf("0.15.3 row = %q %q", rows[1].Kind, rows[1].Detail)
	}
	if rows[2].NSend != 1 {
		t.Fatalf("0.15.2 row sends %d", rows[2].NSend)
	}
}
