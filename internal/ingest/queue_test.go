package ingest

import (
	"context"
	"sync"
	"testing"
	"time"
)

type job struct {
	dev uint32
	seq int
}

type sink struct {
	mu      sync.Mutex
	batches [][]job
	gate    chan struct{} // when set, flush blocks until closed
}

func (s *sink) flush(_ context.Context, b []job) {
	if s.gate != nil {
		<-s.gate
	}
	cp := append([]job(nil), b...)
	s.mu.Lock()
	s.batches = append(s.batches, cp)
	s.mu.Unlock()
}

func (s *sink) all() []job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []job
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

func byDev(j job) uint32 { return j.dev }

func TestCloseDrainsEverythingInOrderPerDevice(t *testing.T) {
	s := &sink{}
	q := New(Options{Shards: 3, PerShard: 1000, BatchSize: 7, FlushEvery: time.Hour}, byDev, s.flush)
	for i := 0; i < 300; i++ {
		if !q.Enqueue(job{dev: uint32(i % 10), seq: i}) {
			t.Fatalf("enqueue %d refused", i)
		}
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.all()
	if len(got) != 300 {
		t.Fatalf("wrote %d of 300", len(got))
	}
	last := map[uint32]int{}
	for _, j := range got {
		if p, ok := last[j.dev]; ok && j.seq < p {
			t.Fatalf("device %d: seq %d written after %d", j.dev, j.seq, p)
		}
		last[j.dev] = j.seq
	}
	for _, b := range s.batches {
		if len(b) > 7 {
			t.Fatalf("batch of %d exceeds BatchSize 7", len(b))
		}
	}
}

func TestFlushesOnTimerWithoutFillingABatch(t *testing.T) {
	s := &sink{}
	q := New(Options{Shards: 1, BatchSize: 100, FlushEvery: 20 * time.Millisecond}, byDev, s.flush)
	defer q.Close(context.Background())
	q.Enqueue(job{dev: 1, seq: 1})
	deadline := time.Now().Add(2 * time.Second)
	for len(s.all()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a lone job was never flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFullShardRefusesAndSaturates(t *testing.T) {
	s := &sink{gate: make(chan struct{})}
	q := New(Options{Shards: 1, PerShard: 10, BatchSize: 1, FlushEvery: time.Hour}, byDev, s.flush)
	// The first job is taken by the worker, which then blocks in flush on the gate, so
	// the channel fills behind it.
	q.Enqueue(job{dev: 1, seq: 0})
	deadline := time.Now().Add(2 * time.Second)
	for q.Depth() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never picked up the first job")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // let it reach the gate
	for i := 1; i <= 10; i++ {
		if !q.Enqueue(job{dev: 1, seq: i}) {
			t.Fatalf("job %d refused below capacity", i)
		}
	}
	if !q.Saturated() {
		t.Fatalf("depth %d of %d not reported saturated", q.Depth(), q.Capacity())
	}
	if q.Enqueue(job{dev: 1, seq: 11}) {
		t.Fatal("a full shard accepted a job")
	}
	close(s.gate)
	if err := q.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(s.all()); n != 11 {
		t.Fatalf("wrote %d, want 11 (the refused one is the caller's)", n)
	}
	if st := q.Stats(); st.Rejected != 1 || st.Written != 11 {
		t.Fatalf("stats %+v", st)
	}
}

func TestClosedQueueRefuses(t *testing.T) {
	q := New(Options{Shards: 1}, byDev, (&sink{}).flush)
	_ = q.Close(context.Background())
	if q.Enqueue(job{dev: 1}) {
		t.Fatal("closed queue accepted a job")
	}
	_ = q.Close(context.Background()) // a second Close is harmless
}
