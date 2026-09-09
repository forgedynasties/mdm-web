package ota

import (
	"sync"
	"time"
)

// LegacyProgress is the live download/install state of otautil clients, keyed by
// serial. In memory only: it is rebuilt from the next byte or poll after a restart.
type LegacyProgress struct {
	mu sync.Mutex
	m  map[string]LegacyState
}

// LegacyState is one device's live state.
type LegacyState struct {
	Phase   string // downloading | installing
	Percent int
	At      time.Time
}

// Legacy is the process-wide store shared by the device listener and the page.
var Legacy = &LegacyProgress{m: map[string]LegacyState{}}

func (p *LegacyProgress) Set(serial, phase string, pct int) {
	p.mu.Lock()
	p.m[serial] = LegacyState{Phase: phase, Percent: pct, At: time.Now()}
	p.mu.Unlock()
}

func (p *LegacyProgress) Get(serial string) (LegacyState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.m[serial]
	return s, ok
}

func (p *LegacyProgress) Clear(serial string) {
	p.mu.Lock()
	delete(p.m, serial)
	p.mu.Unlock()
}
