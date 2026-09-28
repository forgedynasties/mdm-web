// Package ingest is the check-in write queue: the part of a device report nothing in
// the reply depends on is handed here and written in batches, off the request.
//
// It exists for bursts, not volume. A deploy or a restaurant power cut brings the whole
// fleet back inside a few seconds, and each report used to hold a pool connection for
// every write it made while the dashboard waited for the same 60. The reply to a device
// still carries everything it needs synchronously (its id, config, commands, OTA); only
// history — samples, state transitions, crash and reboot events — waits up to a second.
//
// Deliberately in-process and bounded. A broker only earns its place with a second
// replica, and this box runs one. What that costs: a crash (not a deploy — Close drains)
// loses at most one flush interval of history, which the next report re-states anyway.
//
// Work is sharded by device so one device's writes stay in order: a state event from
// charging→idle must never land before the idle→charging that preceded it.
package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Options sizes a queue. Zero values take the defaults.
type Options struct {
	Shards     int           // workers; each owns a fixed slice of devices (default 4)
	PerShard   int           // buffered jobs per shard (default 512)
	BatchSize  int           // flush once a shard holds this many (default 200)
	FlushEvery time.Duration // and at least this often (default 1s)
}

func (o *Options) defaults() {
	if o.Shards <= 0 {
		o.Shards = 4
	}
	if o.PerShard <= 0 {
		o.PerShard = 512
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 200
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = time.Second
	}
}

// Queue batches jobs of type T. flush receives each batch in arrival order for its
// shard and owns error handling: the queue never retries, because only the writer knows
// which of its writes are safe to repeat.
type Queue[T any] struct {
	opts  Options
	shard func(T) uint32
	flush func(context.Context, []T)

	shards []chan T
	stop   chan struct{}
	wg     sync.WaitGroup

	mu     sync.RWMutex // guards closed against a send racing Close
	closed bool

	enqueued, rejected, batches, written atomic.Int64
	lastFlushUs, maxFlushUs              atomic.Int64
}

// New starts the workers. shard maps a job to a stable number (a device id's bytes);
// the same number always lands on the same worker.
func New[T any](opts Options, shard func(T) uint32, flush func(context.Context, []T)) *Queue[T] {
	opts.defaults()
	q := &Queue[T]{opts: opts, shard: shard, flush: flush, stop: make(chan struct{})}
	q.shards = make([]chan T, opts.Shards)
	for i := range q.shards {
		q.shards[i] = make(chan T, opts.PerShard)
		q.wg.Add(1)
		go q.run(q.shards[i])
	}
	return q
}

// Enqueue hands a job to its worker without blocking. False means the shard is full or
// the queue is closing, and the caller must write the job itself: history is never
// dropped for being inconvenient.
func (q *Queue[T]) Enqueue(job T) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.rejected.Add(1)
		return false
	}
	select {
	case q.shards[q.shard(job)%uint32(len(q.shards))] <- job:
		q.enqueued.Add(1)
		return true
	default:
		q.rejected.Add(1)
		return false
	}
}

// Saturated is the admission signal: past 80% of capacity a new report would mostly
// be waiting behind others, so the HTTP path tells the device to come back later rather
// than add to the pile. The WebSocket path cannot say that and writes inline instead.
func (q *Queue[T]) Saturated() bool {
	return q.Depth()*5 >= q.Capacity()*4
}

// Depth is how many jobs are waiting across all shards.
func (q *Queue[T]) Depth() int {
	n := 0
	for _, c := range q.shards {
		n += len(c)
	}
	return n
}

// Capacity is the total number of jobs the queue can hold.
func (q *Queue[T]) Capacity() int { return len(q.shards) * q.opts.PerShard }

// Close stops accepting, writes what is buffered and waits for the workers, or for ctx.
// A deploy calls it after the HTTP server has drained, so nothing is lost on a restart.
func (q *Queue[T]) Close(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.stop)
	}
	q.mu.Unlock()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue[T]) run(in chan T) {
	defer q.wg.Done()
	t := time.NewTicker(q.opts.FlushEvery)
	defer t.Stop()
	batch := make([]T, 0, q.opts.BatchSize)
	for {
		select {
		case job := <-in:
			batch = append(batch, job)
			if len(batch) >= q.opts.BatchSize {
				batch = q.write(batch)
			}
		case <-t.C:
			batch = q.write(batch)
		case <-q.stop:
			// Closed: no new sends can start (Enqueue checks closed under the lock),
			// so what is in the channel now is everything there will ever be.
			for {
				select {
				case job := <-in:
					batch = append(batch, job)
					if len(batch) >= q.opts.BatchSize {
						batch = q.write(batch)
					}
				default:
					q.write(batch)
					return
				}
			}
		}
	}
}

// write flushes one batch and returns the emptied slice for reuse. The flush gets its
// own context: the request that produced these jobs is long gone.
func (q *Queue[T]) write(batch []T) []T {
	if len(batch) == 0 {
		return batch
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	q.flush(ctx, batch)
	cancel()
	us := time.Since(start).Microseconds()
	q.lastFlushUs.Store(us)
	for {
		m := q.maxFlushUs.Load()
		if us <= m || q.maxFlushUs.CompareAndSwap(m, us) {
			break
		}
	}
	q.batches.Add(1)
	q.written.Add(int64(len(batch)))
	clear(batch) // drop references so written jobs can be collected
	return batch[:0]
}

// Stats is a point-in-time view for the Server page and /debug/vars.
type Stats struct {
	Depth       int     `json:"depth"`
	Capacity    int     `json:"capacity"`
	Enqueued    int64   `json:"enqueued"`
	Rejected    int64   `json:"rejected"` // full or closing: written inline by the caller
	Written     int64   `json:"written"`
	Batches     int64   `json:"batches"`
	LastFlushMs float64 `json:"last_flush_ms"`
	MaxFlushMs  float64 `json:"max_flush_ms"`
}

func (q *Queue[T]) Stats() Stats {
	return Stats{
		Depth:       q.Depth(),
		Capacity:    q.Capacity(),
		Enqueued:    q.enqueued.Load(),
		Rejected:    q.rejected.Load(),
		Written:     q.written.Load(),
		Batches:     q.batches.Load(),
		LastFlushMs: float64(q.lastFlushUs.Load()) / 1000,
		MaxFlushMs:  float64(q.maxFlushUs.Load()) / 1000,
	}
}
