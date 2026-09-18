package otagate

import (
	"context"
	"testing"

	"mdm/internal/db"
	"mdm/internal/product"
)

// A DPC device takes no OTA by any path. It must not read as "legacy only", which
// would route it to the otautil half of a rollout it can never poll.
func TestDPCHasNoOTAPath(t *testing.T) {
	g := New(nil, nil)
	v := g.Device(context.Background(), db.Device{AgentKind: product.KindDPC, Capabilities: []string{"kiosk", "reboot"}})
	if v.OK || v.Source != SourceDPC || v.Legacy() {
		t.Errorf("DPC verdict = %+v (legacy %v), want not OK, source dpc, not legacy", v, v.Legacy())
	}
	if v, ok := ForAgentKind(product.AgentTypeApp); !ok || v.Legacy() {
		t.Errorf("app agent: got %+v ok=%v, want the no-OTA verdict", v, ok)
	}
	if _, ok := ForAgentKind(product.KindFirmware); ok {
		t.Error("ForAgentKind answered for a firmware device")
	}
	// A firmware client without MDM OTA still goes legacy.
	fw := g.Device(context.Background(), db.Device{AgentKind: product.KindFirmware, Capabilities: []string{"kiosk"}})
	if fw.OK || !fw.Legacy() {
		t.Errorf("firmware without ota = %+v, want legacy", fw)
	}
}
