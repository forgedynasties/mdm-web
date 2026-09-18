// Package otagate answers one question for the whole server: can this device take
// an MDM OTA, or must it update over the legacy otautil path?
//
// Firmware older than the client that could actually apply an A/B update is still
// managed by the MDM in every other way — it checks in, takes commands, reports
// telemetry — it just cannot finish an update_engine session. Pushing it one burns
// a 2 GB download and ends in a failure the operator has to clean up. The devices
// that can't are exactly the legacy OTA fleet: they still run otautil and still poll
// the legacy listener, so the answer here is a routing decision, not a dead end.
//
// The answer comes from, in order:
//
//  1. What the device says. A client that advertises capabilities speaks for
//     itself — the DPC agent always does, and the firmware client does from the
//     build that started sending them. Nothing else can override a device that
//     reports its own abilities.
//  2. An explicit row in ota_support. An admin's "yes"/"no" for a build, or proof
//     recorded when a device on that build completed an MDM OTA.
//  3. The per-product cutoff release (config ota_min_release): every release older
//     than it is out. A build with no release row at all is out too — the unknown
//     builds in a fleet are one-off images, not shipped firmware.
//
// With no cutoff configured and no rows, everything is supported: the gate is off
// until someone turns it on, so an upgrade changes nothing on its own.
package otagate

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Verdict is the answer for one device or build.
type Verdict struct {
	OK     bool
	Reason string // why not, for the UI: "" when OK
	Source string // dpc | device | manual | observed | cutoff | untracked | off
}

// SourceDPC marks a device managed by the DPC (Device-Owner) agent on stock hardware.
// OTA is for our firmware devices only: a DPC device takes neither an MDM OTA nor the
// legacy one, so it must not be routed to the legacy path like a device whose build
// merely predates MDM OTA.
const SourceDPC = "dpc"

// Legacy reports whether the legacy otautil path is this device's way to update:
// no MDM OTA, and not a DPC device (which has no OTA path at all).
func (v Verdict) Legacy() bool { return !v.OK && v.Source != SourceDPC }

// Store is the slice of the database the gate reads.
type Store interface {
	ListOTASupport(ctx context.Context) ([]db.OTASupport, error)
	ListReleases(ctx context.Context) ([]db.Release, error)
}

// Config is the slice of config the gate reads.
type Config interface {
	OTAMinRelease() map[string]int // product key -> oldest release id with working MDM OTA
}

// Gate resolves OTA support. Safe for concurrent use; the underlying tables change
// rarely, so answers are cached briefly rather than queried per device in a list.
type Gate struct {
	db  Store
	cfg Config

	mu       sync.Mutex
	cached   map[string]Verdict // "product|build" -> verdict
	cachedAt time.Time
}

const cacheTTL = 30 * time.Second

func New(store Store, cfg Config) *Gate { return &Gate{db: store, cfg: cfg} }

// Device answers for a device: what it said about itself first, then its build.
func (g *Gate) Device(ctx context.Context, d db.Device) Verdict {
	if v, ok := ForAgentKind(d.AgentKind); ok {
		return v
	}
	// A client that advertises a capability list is the authority on itself.
	if v, ok := Reported(d.Capabilities); ok {
		return v
	}
	// The firmware client says only this one thing. It is deliberately NOT part of
	// the capability list: a reported list REPLACES the product defaults for every
	// command, so one omission there would silently disable kiosk or shell across a
	// firmware rollout. A single flag can only answer the question it is asked.
	if v, ok := ReportedExtra(d.LatestExtra); ok {
		return v
	}
	return g.Build(ctx, d.BuildID, d.ProductKey())
}

// ForAgentKind answers for a DPC-managed device (agent kind, or a check-in's
// extra.agent_type, "dpc"): never, by any path. ok is false for anything else.
func ForAgentKind(kind string) (v Verdict, ok bool) {
	if kind != product.KindDPC {
		return Verdict{}, false
	}
	return Verdict{Reason: "OTA is for AIO firmware devices only — this device is managed by the DPC agent", Source: SourceDPC}, true
}

// ReportedExtra reads the firmware client's own answer out of a check-in's extra
// ("ota_supported"). ok is false when the client said nothing, which is every build
// that predates the flag — those fall through to the build rules.
func ReportedExtra(extra json.RawMessage) (v Verdict, ok bool) {
	if len(extra) == 0 {
		return Verdict{}, false
	}
	var probe struct {
		OTASupported *bool `json:"ota_supported"`
	}
	if err := json.Unmarshal(extra, &probe); err != nil || probe.OTASupported == nil {
		return Verdict{}, false
	}
	if *probe.OTASupported {
		return Verdict{OK: true, Source: "device"}, true
	}
	return Verdict{Reason: "the client reports no MDM OTA — legacy OTA only", Source: "device"}, true
}

// Reported answers from a capability list a device just sent, when it sent one.
// ok is false when the device advertised nothing and the build rules should decide.
func Reported(caps []string) (v Verdict, ok bool) {
	if len(caps) == 0 {
		return Verdict{}, false
	}
	for _, c := range caps {
		if c == product.CapOTA {
			return Verdict{OK: true, Source: "device"}, true
		}
	}
	return Verdict{Reason: "this build's client has no MDM OTA — legacy OTA only", Source: "device"}, true
}

// Build answers for a (build, product) pair, for callers that only have the strings.
func (g *Gate) Build(ctx context.Context, buildID, productKey string) Verdict {
	pk := product.Normalize(productKey)
	m := g.load(ctx)
	if v, ok := m[key(pk, buildID)]; ok {
		return v
	}
	return g.UnknownBuildVerdict(pk)
}

// Unsupported returns the builds of one product that cannot take an MDM OTA, for
// the pickers, which mark whole rows at once.
func (g *Gate) Unsupported(ctx context.Context, productKey string) map[string]string {
	pk := product.Normalize(productKey)
	out := map[string]string{}
	for k, v := range g.load(ctx) {
		p, b := split(k)
		if p == pk && !v.OK {
			out[b] = v.Reason
		}
	}
	return out
}

// Invalidate drops the cache — call after an admin changes the cutoff or a row.
func (g *Gate) Invalidate() {
	g.mu.Lock()
	g.cached, g.cachedAt = nil, time.Time{}
	g.mu.Unlock()
}

// load builds the whole verdict map: one pass over the releases and the explicit
// rows, which is cheaper than asking per build and keeps the rules in one place.
func (g *Gate) load(ctx context.Context) map[string]Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached != nil && time.Since(g.cachedAt) < cacheTTL {
		return g.cached
	}
	out := map[string]Verdict{}
	releases, _ := g.db.ListReleases(ctx)

	// The cutoff: every release of that product older than the named one is out, and
	// so is any build with no release row.
	min := g.cfg.OTAMinRelease()
	cutoffAt := map[string]time.Time{}
	for pk, id := range min {
		if id == 0 {
			continue
		}
		for _, rel := range releases {
			if rel.ID == id {
				cutoffAt[product.Normalize(pk)] = rel.CreatedAt
			}
		}
	}
	for _, rel := range releases {
		pk := product.Normalize(rel.Product)
		at, on := cutoffAt[pk]
		if !on {
			out[key(pk, rel.Version)] = Verdict{OK: true, Source: "off"}
			continue
		}
		if rel.CreatedAt.Before(at) {
			out[key(pk, rel.Version)] = Verdict{Reason: "no MDM OTA on this build — legacy OTA only", Source: "cutoff"}
		} else {
			out[key(pk, rel.Version)] = Verdict{OK: true, Source: "cutoff"}
		}
	}

	// Explicit rows win over the cutoff.
	rows, _ := g.db.ListOTASupport(ctx)
	for _, r := range rows {
		v := Verdict{OK: r.Supported, Source: r.Source}
		if !r.Supported {
			v.Reason = "no MDM OTA on this build — legacy OTA only"
			if r.Note != "" {
				v.Reason = r.Note
			}
		}
		out[key(product.Normalize(r.Product), r.BuildID)] = v
	}
	g.cached, g.cachedAt = out, time.Now()
	return out
}

// UnknownBuildVerdict is the answer for a build with no release row and no explicit
// row, once a cutoff is configured for its product: a one-off image nobody tracked.
func (g *Gate) UnknownBuildVerdict(productKey string) Verdict {
	if g.cfg.OTAMinRelease()[product.Normalize(productKey)] == 0 {
		return Verdict{OK: true, Source: "off"}
	}
	return Verdict{Reason: "build isn't a tracked release — MDM OTA support unknown", Source: "untracked"}
}

func key(productKey, build string) string { return productKey + "|" + build }

func split(k string) (productKey, build string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// Describe renders a verdict for a log line.
func (v Verdict) Describe() string {
	if v.OK {
		return fmt.Sprintf("ota ok (%s)", v.Source)
	}
	return fmt.Sprintf("no ota: %s (%s)", v.Reason, v.Source)
}
