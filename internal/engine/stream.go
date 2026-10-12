package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Streaming geometry. These constants are the entire memory argument for a
// run; changing any of them changes the ceiling (ROADMAP.md §1.2.3–1.2.4,
// MEMORY.md ADL-0016).
const (
	// chunkSize is the fixed size of every pooled response buffer. A read
	// always fills exactly one chunk, so buffer COUNT bounds bytes exactly.
	chunkSize = 32 << 10 // 32 KiB

	// chunkChannelCap is the depth of the queue feeding the consumer.
	// 64 × 32 KiB = 2 MiB resident, independent of payload size.
	chunkChannelCap = 64

	// maxInFlightChunks is the hard ceiling on simultaneously live chunks:
	// the queue, plus the one the reader is filling, plus the one the
	// dispatcher is holding. 66 × 32 KiB = 2 162 688 B = 2.0625 MiB.
	maxInFlightChunks = chunkChannelCap + 2

	// inFlightBytes is that ceiling in bytes, so the tests assert against the
	// same constant the design is reasoned about.
	inFlightBytes = maxInFlightChunks * chunkSize

	// teardownBudgetDefault bounds cancellation unwind (ROADMAP.md §1.2.6).
	teardownBudgetDefault = 50 * time.Millisecond
)

// Chunk is one fixed-size slice of the response body.
//
// Ownership is one-way: engine → Sink (via Sink.Chunk) → pool (via Recycle).
// The engine never reads a buffer after handing it over, and never reuses a
// buffer it has handed off (ROADMAP.md §1.2.3).
type Chunk struct {
	// Buf points at the pooled 32 KiB buffer. The payload is (*Buf)[:N];
	// capacity beyond N is scratch space.
	Buf *[]byte
	// N is the number of valid bytes in (*Buf)[:N].
	N int
	// Final marks the end-of-stream sentinel, whose N is 0.
	Final bool
}

// Bytes returns the valid payload. The slice aliases the pooled buffer and is
// valid only until Recycle.
func (c *Chunk) Bytes() []byte {
	if c == nil || c.Buf == nil {
		return nil
	}
	return (*c.Buf)[:c.N]
}

// Released reports whether the chunk has already been recycled.
func (c *Chunk) Released() bool { return c == nil || c.Buf == nil }

// Recycle returns the chunk's buffer and struct to the pool.
//
// It is idempotent by design: a double Recycle is counted and ignored rather
// than pushing the same buffer twice, which would hand one buffer to two
// concurrent readers — the silent data corruption ROADMAP.md §1.2.3 warns
// about. Tests assert doubleRecycles stays zero, so the guard never hides a bug
// in Pulse's own sinks.
func (c *Chunk) Recycle() {
	if c == nil {
		return
	}
	buf := c.Buf
	if buf == nil {
		doubleRecycles.Add(1)
		return
	}
	if poisonOnRecycle.Load() {
		// Test hook: scribble a sentinel so a read-after-recycle fails loudly
		// instead of silently returning plausible bytes.
		b := (*buf)[:cap(*buf)]
		for i := range b {
			b[i] = poisonByte
		}
	}
	*c = Chunk{}
	chunkPool.Put(buf)
	chunkStructPool.Put(c)
	liveChunks.Add(-1)
}

var (
	chunkPool = sync.Pool{New: func() any {
		b := make([]byte, chunkSize)
		poolMisses.Add(1)
		return &b
	}}
	chunkStructPool = sync.Pool{New: func() any { return new(Chunk) }}
)

// acquireChunk takes a buffer and a struct from their pools, so a chunk costs
// no allocation once the pools are warm.
//
// liveChunks counts buffers CHECKED OUT, not buffers ever built: the increment
// belongs here rather than in the pool's New, because a warm pool hands back an
// existing buffer without allocating and would otherwise drift the count
// negative against Recycle's decrement.
func acquireChunk() *Chunk {
	bufp := chunkStructPool.Get().(*Chunk)
	bufp.Buf = chunkPool.Get().(*[]byte)
	bufp.N, bufp.Final = 0, false
	liveChunks.Add(1)
	return bufp
}

// Pool instrumentation. Counters rather than engine state: they exist so the
// memory gate can assert a hard bound instead of inferring one from a noisy
// heap sample.
var (
	poolMisses  atomic.Int64 // buffers ever allocated (0 once warm)
	liveChunks  atomic.Int64 // chunks currently checked out of the pool
	peakLive    atomic.Int64 // high-water mark of liveChunks
	drainChunks atomic.Int64 // chunks discarded by a cancelled teardown

	// doubleRecycles counts Recycle calls on an already-released chunk.
	doubleRecycles atomic.Int64

	// poisonOnRecycle is set only by white-box tests, to detect
	// use-after-recycle.
	poisonOnRecycle atomic.Bool
)

// poisonByte is the sentinel written into recycled buffers by the test hook.
const poisonByte = 0xDE

// noteLive updates the in-flight high-water mark with a CAS loop: a read must
// never report a peak lower than the true one.
func noteLive(n int64) {
	for {
		cur := peakLive.Load()
		if n <= cur || peakLive.CompareAndSwap(cur, n) {
			return
		}
	}
}

// resetPeakStats re-bases the high-water mark on the current in-flight count so
// one test cannot poison the next.
func resetPeakStats() { peakLive.Store(liveChunks.Load()) }

// digestOf extracts the fixed 32-byte digest from an accumulating hash.
func digestOf(h hash.Hash) [sha256.Size]byte {
	var d [sha256.Size]byte
	copy(d[:], h.Sum(d[:0]))
	return d
}

// stream reads r into pooled chunks and queues them for the consumer, returning
// the number of payload bytes streamed. It is the hot path: once the pools are
// warm it performs zero heap allocations (ROADMAP.md §1.2.2, G-alloc).
//
// r is normally an io.TeeReader over the response body; the tee mirror is built
// by the caller so this function stays independently benchmarkable.
func (e *Engine) stream(ctx context.Context, r io.Reader, chunks chan *Chunk) (int64, error) {
	var total int64
	for {
		c := acquireChunk()
		n, err := io.ReadFull(r, (*c.Buf)[:chunkSize])
		c.N = n
		if n > 0 {
			total += int64(n)
			noteLive(liveChunks.Load())
			// Backpressure: a full queue blocks HERE, in the reader goroutine,
			// which propagates to the socket — never to the consumer's caller.
			select {
			case chunks <- c:
			case <-ctx.Done():
				c.Recycle()
				return total, ctx.Err()
			}
		} else {
			c.Recycle()
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return total, e.sendFinal(ctx, chunks)
			}
			return total, err
		}
	}
}

// sendFinal emits the zero-length end-of-stream sentinel, so a consumer can
// distinguish "no data yet" from "not finished".
func (e *Engine) sendFinal(ctx context.Context, chunks chan *Chunk) error {
	final := acquireChunk()
	final.Final = true
	select {
	case chunks <- final:
		return nil
	case <-ctx.Done():
		final.Recycle()
		return ctx.Err()
	}
}

// dispatch delivers queued chunks to the sink, serially, until the queue is
// closed. It always closes dispatched exactly once.
func (e *Engine) dispatch(ctx context.Context, chunks chan *Chunk, sink Sink, dispatched chan<- struct{}) {
	defer close(dispatched)
	for c := range chunks {
		if ctx.Err() != nil {
			// A cancelled run must not hand more work to the consumer; recycle
			// instead so no buffer is orphaned.
			drainChunks.Add(1)
			c.Recycle()
			continue
		}
		sink.Chunk(c)
	}
}

// drain discards and recycles every buffered chunk, unblocking a full queue
// after cancellation.
func drain(chunks chan *Chunk) {
	for {
		select {
		case c := <-chunks:
			drainChunks.Add(1)
			c.Recycle()
		default:
			return
		}
	}
}
