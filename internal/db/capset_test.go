package db

import (
	"testing"

	"mdm/internal/product"
)

// A firmware device's advertised capabilities must never be able to *subtract* from what
// the product implies. The failure this guards against is not subtle: one capability
// missing from a client's list would withdraw that command from every firmware device at
// once, and the first symptom would be a button that stopped working on a rollout.
func TestFirmwareCapabilitiesMergeWithDefaults(t *testing.T) {
	// A T7 implies the firmware base set plus WLC and mic gain.
	fw := Device{Product: "t7", AgentKind: product.KindFirmware}

	// What a client reports is a strict subset — as a buggy or older client's list would be.
	fw.Capabilities = []string{product.CapTelemetry}
	got := fw.CapSet()
	for _, want := range []string{
		product.CapTelemetry, product.CapInstallAPK, product.CapReboot,
		product.CapShell, product.CapKiosk, product.CapOTA, product.CapWLC, product.CapMicGain,
	} {
		if !got[want] {
			t.Errorf("firmware reported a subset and lost %q — the merge is not holding", want)
		}
	}

	// An advertised capability the defaults do not carry is still added.
	fw.Capabilities = []string{product.CapAppControl}
	if !fw.CapSet()[product.CapAppControl] {
		t.Error("an advertised capability was not merged in")
	}

	// No list at all is the normal case today: the defaults, unchanged.
	fw.Capabilities = nil
	if !fw.CapSet()[product.CapShell] {
		t.Error("a firmware device with no reported list lost its defaults")
	}
}

// A stock device is the opposite case and must keep the replacing behaviour: it has no
// defaults, and what it advertises is the whole truth about it.
func TestStockDeviceCapabilitiesReplace(t *testing.T) {
	dpc := Device{Product: "a14xmtfn", AgentKind: product.KindDPC}

	// Nothing advertised: a stock device can run nothing that is gated.
	if len(dpc.CapSet()) != 0 {
		t.Errorf("a DPC device with no reported list should have no capabilities, got %v", dpc.CapSet())
	}

	// What it advertises is authoritative — including the absence of everything else.
	dpc.Capabilities = []string{product.CapReboot}
	got := dpc.CapSet()
	if !got[product.CapReboot] {
		t.Error("DPC lost the capability it advertised")
	}
	if got[product.CapShell] || got[product.CapInstallAPK] {
		t.Error("DPC gained a capability it did not advertise — stock devices must not inherit firmware defaults")
	}
}
