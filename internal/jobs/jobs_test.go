package jobs

import (
	"context"
	"testing"
	"time"
)

func TestRunRecordsPanicsAndTimeouts(t *testing.T) {
	r := &Registry{}
	ctx := context.Background()
	r.Run(ctx, "ok", time.Second, func(context.Context) {})
	r.Run(ctx, "boom", time.Second, func(context.Context) { panic("x") })
	r.Run(ctx, "slow", 10*time.Millisecond, func(c context.Context) { <-c.Done() })
	s := r.Snapshot()
	if s["ok"].Runs != 1 || s["ok"].Panics != 0 {
		t.Errorf("ok = %+v", s["ok"])
	}
	if s["boom"].Panics != 1 || s["boom"].Runs != 1 {
		t.Errorf("a panic must be counted, not re-raised: %+v", s["boom"])
	}
	if s["slow"].TimedOut != 1 || s["slow"].Running {
		t.Errorf("slow = %+v", s["slow"])
	}
}

func TestEveryRunsAndCountsOverruns(t *testing.T) {
	r := &Registry{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan struct{}, 10)
	// Offset is under half the interval, so with 20 ms the first run is quick.
	r.Every(ctx, "tick", 20*time.Millisecond, time.Second, func(context.Context) {
		time.Sleep(30 * time.Millisecond) // longer than the interval: an overrun
		runs <- struct{}{}
	})
	for i := 0; i < 2; i++ {
		select {
		case <-runs:
		case <-time.After(2 * time.Second):
			t.Fatal("Every did not run")
		}
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	s := r.Snapshot()["tick"]
	if s.Runs < 2 || s.Overruns < 1 || s.Interval != "20ms" {
		t.Errorf("tick = %+v", s)
	}
}

func TestOffsetStaysInFirstHalf(t *testing.T) {
	for _, n := range []string{"commands", "rollouts", "presence", "compliance"} {
		if o := Offset(n, time.Minute); o < 0 || o >= 30*time.Second {
			t.Errorf("Offset(%s) = %s", n, o)
		}
	}
	if Offset("a", time.Minute) != Offset("a", time.Minute) {
		t.Error("Offset must be stable per name")
	}
}
