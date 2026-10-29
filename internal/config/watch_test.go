package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchReloadsAnotherCopysSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	a, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Watch(ctx, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if err := a.SetDataLifecycle(0, 42, 0, 60, 60, 300); err != nil {
		t.Fatal(err)
	}
	// Some filesystems keep whole-second mtimes; the size changes anyway.
	for i := 0; i < 100 && b.CheckinRetentionDays() != 42; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got := b.CheckinRetentionDays(); got != 42 {
		t.Fatalf("copy B still has %d after copy A saved 42", got)
	}
	_ = os.Remove(path)
}
