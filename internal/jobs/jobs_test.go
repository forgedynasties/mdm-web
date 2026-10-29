package jobs

import (
	"context"
	"sync"
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

// fakeLeases is one lease table shared by two "copies".
type fakeLeases struct {
	mu     sync.Mutex
	holder map[string]string
	until  map[string]time.Time
}

func (f *fakeLeases) TryJobLease(_ context.Context, name, holder string, hold time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.holder[name]; ok && h != holder && time.Now().Before(f.until[name]) {
		return false, nil
	}
	f.holder[name], f.until[name] = holder, time.Now().Add(hold)
	return true, nil
}

func (f *fakeLeases) ReleaseJobLease(_ context.Context, name, holder string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder[name] == holder {
		f.until[name] = next
	}
	return nil
}

func TestEveryRunsOnceAcrossCopies(t *testing.T) {
	leases := &fakeLeases{holder: map[string]string{}, until: map[string]time.Time{}}
	a := &Registry{Lease: leases, Holder: "a"}
	b := &Registry{Lease: leases, Holder: "b"}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	runs := 0
	job := func(context.Context) { mu.Lock(); runs++; mu.Unlock(); time.Sleep(5 * time.Millisecond) }
	a.EveryNow(ctx, "j", 100*time.Millisecond, time.Second, job)
	b.EveryNow(ctx, "j", 100*time.Millisecond, time.Second, job)
	time.Sleep(450 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	// ~5 ticks in 450 ms; two copies without leases would make ~10.
	if runs < 3 || runs > 6 {
		t.Errorf("runs = %d across two copies, want one per tick (~5)", runs)
	}
	if a.Snapshot()["j"].Skipped+b.Snapshot()["j"].Skipped == 0 {
		t.Error("one copy should have skipped ticks the other held")
	}
}
