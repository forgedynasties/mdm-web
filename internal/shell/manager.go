// Package shell routes real-time device messages to browser connections.
//
// Device → server message types (received over the device WebSocket):
//
//	{"type":"command_output","command_id":"<uuid>","chunk":"<base64>"}
//	{"type":"command_done",  "command_id":"<uuid>","exit_code":<int>}
package shell

import (
	"strings"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// maxOutputChunks caps how many output chunks a single stream retains, so one
	// very chatty command can't grow the buffer without bound. Trimmed to this when
	// it reaches 2× (amortized O(1)).
	maxOutputChunks = 2000
	// outputRetention is how long a finished stream's buffer is kept for late
	// replay before it's freed from the map (else every command ever run leaks).
	outputRetention = 2 * time.Minute
	// replayBuffer is the per-subscriber channel size (also bounds replay).
	replayBuffer = 512
)

// outputKey identifies a command output stream for a specific (command, device) pair.
type outputKey struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
}

// outputStream holds the subscriber channels and buffered chunks for one stream.
type outputStream struct {
	chunks []string
	subs   []chan string
	closed bool
}

// OTAProgress holds the latest OTA download progress for a device.
type OTAProgress struct {
	CommandID uuid.UUID `json:"command_id"`
	Phase     string    `json:"phase"`   // downloading, verifying, installing, finalizing
	Percent   int       `json:"percent"` // 0-100
	UpdatedAt time.Time `json:"updated_at"`
	// samples are this phase's (time, percent) advances over the last otaRateWindow, for
	// Rate and ETA (27 Oct 2026, user: "add eta to ota … use download speed for
	// downloading and estimate update engine for installing").
	samples []otaSample
}

type otaSample struct {
	at  time.Time
	pct int
}

// otaRateWindow is how far back the rate looks: long enough to smooth a bursty
// download, short enough to follow a speed change.
const otaRateWindow = 10 * time.Minute

// Rate is the percent per second over the window, ok when the phase has moved at least
// 2 % over at least 30 s. A device that stops moving lowers it by itself: the window's
// oldest point stays put while now moves on.
func (p *OTAProgress) Rate(now time.Time) (float64, bool) {
	if p == nil || len(p.samples) == 0 {
		return 0, false
	}
	first := p.samples[0]
	secs := now.Sub(first.at).Seconds()
	gained := p.Percent - first.pct
	if secs < 30 || gained < 2 {
		return 0, false
	}
	return float64(gained) / secs, true
}

// ETA is the time left in this phase at the current rate.
func (p *OTAProgress) ETA(now time.Time) (time.Duration, bool) {
	r, ok := p.Rate(now)
	if !ok || r <= 0 {
		return 0, false
	}
	left := float64(100-p.Percent) / r
	return time.Duration(left * float64(time.Second)), true
}

// Manager routes messages from devices to browser connections.
type Manager struct {
	// command output streams keyed by (commandID, deviceID)
	outMu   sync.Mutex
	outputs map[outputKey]*outputStream

	// OTA progress per device
	otaMu    sync.Mutex
	otaState map[uuid.UUID]*OTAProgress

	// OnOTAProgress, if set, fires on every OTA progress report (WS ota_progress
	// frame or checkin-piggybacked, either path — both funnel through
	// updateOTAProgress). Wired in cmd/server/main.go to mark the command
	// received in the DB: this package is deliberately DB-agnostic (in-memory
	// only), so it can't do that itself, but without SOME received signal a
	// long-running download/install sits at command_status.status='delivered'
	// for its whole duration and RedriveStuckDeliveries repeatedly re-pushes it
	// as "stuck" every ~90s even though the device is actively working on it.
	OnOTAProgress func(deviceID, commandID uuid.UUID)
	// OnOTABytes carries the byte counters a client reports with its percent, so the
	// server can store a real figure instead of a number that moves once per 20MB.
	OnOTABytes func(deviceID, commandID uuid.UUID, phase string, percent int, done, total int64)

	// OnCommandDone, if set, fires when a shell command's live output stream
	// closes (the device's "command_done" frame, carrying its exit code). Shell is
	// the one command type whose completion signal is this stream closing rather
	// than a "command_ack" frame — nothing previously turned that into a DB ack, so
	// the terminal output finished (this closes it) while the command's delivery
	// status/summary pill on the page stayed at "in progress" until a reload
	// re-derived it fresh. Wired in cmd/server/main.go for the same DB-agnostic
	// reason as OnOTAProgress.
	OnCommandDone func(deviceID, commandID uuid.UUID, exitCode int)
}

func NewManager() *Manager {
	return &Manager{
		outputs:  make(map[outputKey]*outputStream),
		otaState: make(map[uuid.UUID]*OTAProgress),
	}
}

// HandleDeviceMessage is called by the hub for every message a device sends.
func (m *Manager) HandleDeviceMessage(deviceID uuid.UUID, raw []byte) {
	var frame struct {
		Type      string    `json:"type"`
		CommandID uuid.UUID `json:"command_id"`
		Chunk     string    `json:"chunk"`
		ExitCode  int       `json:"exit_code"`
		Phase      string   `json:"phase"`
		Percent    int      `json:"percent"`
		Bytes      int64    `json:"bytes"`
		BytesTotal int64    `json:"bytes_total"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "command_output":
		m.appendCommandOutput(outputKey{frame.CommandID, deviceID}, frame.Chunk)
	case "command_done":
		log.Printf("[shell] done device=%s command=%s exit=%d", deviceID, frame.CommandID, frame.ExitCode)
		m.closeCommandOutput(outputKey{frame.CommandID, deviceID})
		if m.OnCommandDone != nil {
			m.OnCommandDone(deviceID, frame.CommandID, frame.ExitCode)
		}
	case "ota_progress":
		m.updateOTAProgress(deviceID, frame.CommandID, frame.Phase, frame.Percent)
		if m.OnOTABytes != nil && (frame.Bytes > 0 || frame.BytesTotal > 0) {
			m.OnOTABytes(deviceID, frame.CommandID, frame.Phase, frame.Percent, frame.Bytes, frame.BytesTotal)
		}
	}
}

// ── Command output ────────────────────────────────────────────────────────────

func (m *Manager) appendCommandOutput(key outputKey, chunk string) {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	s := m.ensureStream(key)
	if s.closed {
		return
	}
	s.chunks = append(s.chunks, chunk)
	// Bound retained output: keep the most recent maxOutputChunks, trimming only
	// when we hit 2× so this is amortized O(1) rather than a copy per chunk.
	if len(s.chunks) > 2*maxOutputChunks {
		s.chunks = append([]string(nil), s.chunks[len(s.chunks)-maxOutputChunks:]...)
	}
	for _, ch := range s.subs {
		select {
		case ch <- chunk:
		default:
			log.Printf("[shell] command output subscriber buffer full, dropping chunk")
		}
	}
}

func (m *Manager) closeCommandOutput(key outputKey) {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	s := m.ensureStream(key)
	if s.closed {
		return
	}
	s.closed = true
	for _, ch := range s.subs {
		close(ch)
	}
	s.subs = nil
	// Free the buffered output after a grace period (lets a late viewer still
	// replay it), so finished commands don't accumulate in the map forever.
	time.AfterFunc(outputRetention, func() {
		m.outMu.Lock()
		delete(m.outputs, key)
		m.outMu.Unlock()
	})
}

// CommandOutput returns everything a command has printed so far, joined. Used by
// callers that want the whole answer once rather than a live stream — a device's
// shell output arrives as stream frames and only sometimes rides along on the ack,
// so this is the reliable place to read it from.
func (m *Manager) CommandOutput(commandID, deviceID uuid.UUID) string {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	s := m.outputs[outputKey{commandID, deviceID}]
	if s == nil {
		return ""
	}
	return strings.Join(s.chunks, "")
}

// SubscribeCommandOutput returns a channel that receives output chunks for the
// given (command, device) pair. The caller must consume the channel until it is
// closed. Call the returned unsubscribe func to clean up if the browser
// disconnects before the stream ends.
func (m *Manager) SubscribeCommandOutput(commandID, deviceID uuid.UUID) (<-chan string, func()) {
	key := outputKey{commandID, deviceID}
	ch := make(chan string, replayBuffer)

	m.outMu.Lock()
	s := m.ensureStream(key)
	// Replay the most recent chunks that fit the buffer. Replaying an unbounded
	// backlog into a fixed channel while holding outMu previously blocked on the
	// (buffer+1)th send with the lock held — deadlocking the whole shell subsystem.
	// Bounding to cap(ch) guarantees the replay never blocks.
	start := 0
	if n := len(s.chunks); n > cap(ch) {
		start = n - cap(ch)
	}
	for _, c := range s.chunks[start:] {
		ch <- c
	}
	if s.closed {
		close(ch)
		m.outMu.Unlock()
		return ch, func() {}
	}
	s.subs = append(s.subs, ch)
	m.outMu.Unlock()

	unsub := func() {
		m.outMu.Lock()
		if st, ok := m.outputs[key]; ok {
			for i, sub := range st.subs {
				if sub == ch {
					st.subs = append(st.subs[:i], st.subs[i+1:]...)
					break
				}
			}
		}
		m.outMu.Unlock()
	}
	return ch, unsub
}

func (m *Manager) ensureStream(key outputKey) *outputStream {
	s, ok := m.outputs[key]
	if !ok {
		s = &outputStream{}
		m.outputs[key] = s
	}
	return s
}

// ── OTA progress ─────────────────────────────────────────────────────────────

func (m *Manager) updateOTAProgress(deviceID, commandID uuid.UUID, phase string, percent int) {
	// Clamp device-reported percent to 0–100; it's rendered into a CSS width and
	// drives "stalled/complete" heuristics, so an out-of-range value distorts both.
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	m.otaMu.Lock()
	// UpdatedAt tracks the last time progress actually advanced (phase or percent
	// changed), not merely the last report — so a device that keeps checking in but is
	// wedged at the same percent goes stale and gets flagged "stalled".
	now := time.Now()
	updatedAt := now
	prev := m.otaState[deviceID]
	if prev != nil && prev.Phase == phase && prev.Percent == percent {
		updatedAt = prev.UpdatedAt
	}
	// The rate samples carry on within one phase of one command; a new phase or attempt
	// starts them again. Points older than the window drop off, keeping at least one.
	var samples []otaSample
	if prev != nil && prev.Phase == phase && prev.CommandID == commandID && percent >= prev.Percent {
		samples = prev.samples
	}
	if len(samples) == 0 || samples[len(samples)-1].pct != percent {
		samples = append(samples, otaSample{at: now, pct: percent})
	}
	for len(samples) > 1 && now.Sub(samples[1].at) > otaRateWindow {
		samples = samples[1:]
	}
	m.otaState[deviceID] = &OTAProgress{
		CommandID: commandID,
		Phase:     phase,
		Percent:   percent,
		UpdatedAt: updatedAt,
		samples:   samples,
	}
	m.otaMu.Unlock()
	if m.OnOTAProgress != nil {
		m.OnOTAProgress(deviceID, commandID)
	}
}

// SetOTAProgress records OTA progress reported outside the WebSocket path —
// devices piggyback their current phase/percent on HTTP checkins so progress
// stays visible even when the WS connection is down.
func (m *Manager) SetOTAProgress(deviceID, commandID uuid.UUID, phase string, percent int) {
	m.updateOTAProgress(deviceID, commandID, phase, percent)
}

// GetOTAProgress returns the latest OTA progress for a device, or nil if none.
func (m *Manager) GetOTAProgress(deviceID uuid.UUID) *OTAProgress {
	m.otaMu.Lock()
	defer m.otaMu.Unlock()
	return m.otaState[deviceID]
}

// ClearOTAProgress removes stored OTA progress for a device (call on completion/error).
func (m *Manager) ClearOTAProgress(deviceID uuid.UUID) {
	m.otaMu.Lock()
	delete(m.otaState, deviceID)
	m.otaMu.Unlock()
}
