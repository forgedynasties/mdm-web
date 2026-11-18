package shell

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func outputFrame(cmd uuid.UUID, chunk string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "command_output", "command_id": cmd, "chunk": chunk})
	return b
}

// Output for a command that was never sent to the device is not kept.
func TestOutputRefusedForUnknownCommand(t *testing.T) {
	m := NewManager()
	m.AcceptOutput = func(_, _ uuid.UUID) bool { return false }
	dev, cmd := uuid.New(), uuid.New()
	m.HandleDeviceMessage(dev, outputFrame(cmd, "hello"))
	if got := m.CommandOutput(cmd, dev); got != "" {
		t.Fatalf("kept %q for a command the device was never sent", got)
	}
	if len(m.outputs) != 0 {
		t.Fatalf("%d streams held", len(m.outputs))
	}
}

// One stream holds at most maxOutputBytes, keeping the newest output.
func TestOutputBytesAreBounded(t *testing.T) {
	m := NewManager()
	m.AcceptOutput = func(_, _ uuid.UUID) bool { return true }
	dev, cmd := uuid.New(), uuid.New()
	chunk := strings.Repeat("a", 1<<20)
	for i := 0; i < 20; i++ {
		m.HandleDeviceMessage(dev, outputFrame(cmd, chunk))
	}
	m.HandleDeviceMessage(dev, outputFrame(cmd, "tail"))
	got := m.CommandOutput(cmd, dev)
	if len(got) > maxOutputBytes {
		t.Fatalf("stream holds %d bytes, cap is %d", len(got), maxOutputBytes)
	}
	if !strings.HasSuffix(got, "tail") {
		t.Fatal("the newest output was dropped")
	}
}
