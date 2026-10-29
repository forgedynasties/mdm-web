// Package jobs runs the server's periodic work and keeps a record of it (scalability
// plan W1/W2a, 29 Oct 2026).
//
// Until then ~25 jobs ran one after another in a single minute ticker: one slow job
// delayed every job behind it, and once the loop took longer than a minute Go's ticker
// silently dropped ticks, so everything ran less often as the fleet grew. Every gives
// each job (or each group of jobs that must stay in order) its own ticker, and Run
// records what every run cost, for /debug/vars ("jobs") and for the overrun log line.
package jobs

import (
	"context"
	"hash/fnv"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Stat is one job's record. Durations are milliseconds.
type Stat struct {
	Interval  string    `json:"interval,omitempty"`
	Runs      int64     `json:"runs"`
	Panics    int64     `json:"panics"`
	TimedOut  int64     `json:"timed_out"`
	Overruns  int64     `json:"overruns"` // runs that took longer than the interval
	LastStart time.Time `json:"last_start"`
	LastMs    int64     `json:"last_ms"`
	MaxMs     int64     `json:"max_ms"`
	TotalMs   int64     `json:"total_ms"`
	Running   bool      `json:"running"`
	Skipped   int64     `json:"skipped"` // ticks another server copy held the lease for
}

// Leaser claims a job's tick across server copies (scale plan W2b): a copy runs the tick
// only if it gets the lease, so with two copies up each job still runs once. db.DB is one.
type Leaser interface {
	TryJobLease(ctx context.Context, name, holder string, hold time.Duration) (bool, error)
	ReleaseJobLease(ctx context.Context, name, holder string, next time.Time) error
}

// Registry holds the stats. Default is the process-wide one.
type Registry struct {
	mu    sync.Mutex
	stats map[string]*Stat
	// Lease, when set, is asked before every Every tick; Holder names this copy. Unset
	// (tests, a single copy with no database), every tick runs.
	Lease  Leaser
	Holder string
}

var Default = &Registry{}

func (r *Registry) stat(name string) *Stat {
	if r.stats == nil {
		r.stats = map[string]*Stat{}
	}
	s := r.stats[name]
	if s == nil {
		s = &Stat{}
		r.stats[name] = s
	}
	return s
}

// Run runs fn once with a timeout and panic recovery and records it under name. A
// panic is logged and counted, never re-raised: one bad tick must not take the
// process (or the job's ticker) down.
func (r *Registry) Run(ctx context.Context, name string, timeout time.Duration, fn func(context.Context)) {
	start := time.Now()
	r.mu.Lock()
	s := r.stat(name)
	s.LastStart, s.Running = start, true
	r.mu.Unlock()

	jctx, cancel := context.WithTimeout(ctx, timeout)
	panicked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = true
				log.Printf("[panic] job %s: %v\n%s", name, p, debug.Stack())
			}
		}()
		fn(jctx)
	}()
	timedOut := jctx.Err() == context.DeadlineExceeded
	cancel()

	ms := time.Since(start).Milliseconds()
	r.mu.Lock()
	s.Running = false
	s.Runs++
	s.LastMs = ms
	s.TotalMs += ms
	if ms > s.MaxMs {
		s.MaxMs = ms
	}
	if panicked {
		s.Panics++
	}
	if timedOut {
		s.TimedOut++
	}
	r.mu.Unlock()
}

// Every runs fn every interval in its own goroutine until ctx ends, each run through Run
// with the given timeout. The first run waits a fixed, name-derived part of the interval,
// so jobs started together don't all fire in the same second. A run longer than the
// interval is logged and counted as an overrun (the ticker then skips what it missed).
func (r *Registry) Every(ctx context.Context, name string, interval, timeout time.Duration, fn func(context.Context)) {
	r.every(ctx, name, interval, timeout, Offset(name, interval), fn)
}

// EveryNow is Every with the first run straight away (housekeeping's rollups, the OTA
// config: things a fresh start should not wait half an hour for).
func (r *Registry) EveryNow(ctx context.Context, name string, interval, timeout time.Duration, fn func(context.Context)) {
	r.every(ctx, name, interval, timeout, 0, fn)
}

func (r *Registry) every(ctx context.Context, name string, interval, timeout, first time.Duration, fn func(context.Context)) {
	r.mu.Lock()
	r.stat(name).Interval = interval.String()
	r.mu.Unlock()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("[panic] job loop %s: %v\n%s", name, p, debug.Stack())
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(first):
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			start := time.Now()
			if r.claim(ctx, name, timeout) {
				r.Run(ctx, name, timeout, fn)
				r.release(name, start.Add(interval-interval/20))
			}
			if took := time.Since(start); took > interval {
				r.mu.Lock()
				r.stat(name).Overruns++
				r.mu.Unlock()
				log.Printf("[jobs] %s took %s, longer than its %s interval: later runs are skipped until it catches up", name, took.Round(time.Second), interval)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Offset is name's start delay within interval: stable per name, spread across the
// first half of the interval.
func Offset(name string, interval time.Duration) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(name))
	half := interval / 2
	if half <= 0 {
		return 0
	}
	return time.Duration(uint64(h.Sum32()) % uint64(half))
}

// Snapshot copies every job's record, for /debug/vars.
func (r *Registry) Snapshot() map[string]Stat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Stat, len(r.stats))
	for k, v := range r.stats {
		out[k] = *v
	}
	return out
}

// claim takes the job's lease for this tick (true with no Leaser). A lease error skips the
// tick rather than risk running it twice; the next tick asks again.
func (r *Registry) claim(ctx context.Context, name string, hold time.Duration) bool {
	if r.Lease == nil {
		return true
	}
	ok, err := r.Lease.TryJobLease(ctx, name, r.Holder, hold)
	if err != nil {
		log.Printf("[jobs] lease %s: %v (skipping this tick)", name, err)
	}
	if !ok {
		r.mu.Lock()
		r.stat(name).Skipped++
		r.mu.Unlock()
	}
	return ok
}

// release shortens this copy's lease to just before the next tick, so any copy may take
// that one.
func (r *Registry) release(name string, next time.Time) {
	if r.Lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Lease.ReleaseJobLease(ctx, name, r.Holder, next); err != nil {
		log.Printf("[jobs] release %s: %v", name, err)
	}
}
