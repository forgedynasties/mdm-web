package shell

import (
	"testing"
	"time"
)

// ETA from the rate inside one phase (27 Oct 2026).
func TestOTAETA(t *testing.T) {
	t0 := time.Now().Add(-5 * time.Minute)
	p := &OTAProgress{Phase: "downloading", Percent: 50, samples: []otaSample{{t0, 0}, {t0.Add(2 * time.Minute), 20}, {time.Now(), 50}}}
	eta, ok := p.ETA(time.Now())
	if !ok || eta < 4*time.Minute || eta > 6*time.Minute { // 50 % in 5 min → 5 min left
		t.Fatalf("eta = %v %v", eta, ok)
	}
	if _, ok := (&OTAProgress{Percent: 1, samples: []otaSample{{time.Now().Add(-10 * time.Second), 0}}}).ETA(time.Now()); ok {
		t.Error("an estimate from 10 s and 1 %")
	}
}
