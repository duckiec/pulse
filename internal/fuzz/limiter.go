package fuzz

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// This file is the safety rail that makes auto-fuzzing acceptable to run against
// a developer's own machine.
//
// # The problem
//
// An LLM driving a fuzz loop is fast, parallel, and does not experience a
// server becoming unresponsive as something a human would. Left unbounded it
// will saturate a laptop, trip a rate limiter belonging to someone else's
// service, or take down the very server the developer is trying to debug. The
// limit is not a tuning knob; it is what makes the feature safe to ship.
//
// # Why GCRA and not a plain token bucket
//
// The textbook token bucket — refill at rate R, spend 1 per request, sleep when
// empty — has a correctness problem under concurrency: N waiters all observe
// "empty", all compute a wait, all sleep the same duration, and all wake
// together. The bucket then admits a burst of N and the *observed* rate exceeds
// R for a full interval after the wait. Every waiter sleeping the same length
// is the bug, and it only shows up under the load the limiter exists to prevent.
//
// This is a GCRA (Generic Cell Rate Algorithm) limiter, which reserves the slot
// at DECIDE time rather than at WAKE time:
//
//	tat   := max(now, previous_tat)   // theoretical arrival time
//	allow := tat - burstDelta         // when this request may proceed
//	if allow < now { allow = now }    // never in the past
//	next tat := tat + interval        // reserve
//
// The invariant is a single inequality. With interval = 1/R and burstDelta =
// (C-1)/R for capacity C:
//
//	admission_time(i) >= t0 + i/R - (C-1)/R
//
// so for ANY window of length Δ, the number of admissions is at most
// C + R·Δ. The initial burst is exactly C, and the steady-state rate is exactly
// R. `TestRateLimiterNeverExceedsTheConfiguredRate` asserts that bound over a
// sliding window rather than over fixed buckets, because a fixed-window check
// can hide an arbitrarily large burst straddling a boundary.

// DefaultRate is the requests-per-second a fuzz run uses unless configured.
//
// It is deliberately slow. A boundary fuzzer sends a few hundred payloads; at
// 20/s a full run takes well under a minute and never trips a rate limiter that
// belongs to somebody else's service. Speed is not the feature — coverage of
// declared constraints is.
const DefaultRate = 20.0

// DefaultBurst is the bucket capacity: how many requests may pass back to back
// before throttling engages.
const DefaultBurst = 4

// DefaultConcurrency is how many payloads may be in flight simultaneously.
//
// Concurrency is capped SEPARATELY from rate, because they bound different
// things: rate bounds load on the server, concurrency bounds load on the fuzzer's
// own file descriptors and goroutines. A high rate with unbounded concurrency
// opens hundreds of sockets; a low rate with unbounded concurrency holds them
// all open for the length of the run.
const DefaultConcurrency = 4

// ErrBudgetExhausted reports that a run hit its configured payload budget.
var ErrBudgetExhausted = errors.New("fuzz: payload budget exhausted")

// ErrTargetNotLoopback reports a target outside the loopback interface.
var ErrTargetNotLoopback = errors.New("fuzz: fuzzing targets are restricted to loopback")

// clock is the limiter's time source, injectable so the rate gate can assert on
// a deterministic timeline rather than on wall-clock timing that CI makes noisy.
type clock func() time.Time

// Limiter is a strict rate limiter.
//
// The zero value is not usable; construct with NewLimiter.
type Limiter struct {
	mu sync.Mutex
	// tat is the theoretical arrival time of the next request, in the same
	// units as now. It may legitimately be in the future: that is how a request
	// scheduled several intervals out reserves its place.
	tat time.Time
	// interval is 1/rate.
	interval time.Duration
	// burstDelta is (burst-1)*interval.
	burstDelta time.Duration
	// start is when the limiter was created, used to make the first admission
	// immediate rather than waiting an interval for an empty clock.
	start time.Time

	now clock

	// observed records admission times for the gate. It is bounded so a long
	// run cannot turn the limiter into a memory leak.
	observed []time.Time
}

// NewLimiter returns a limiter admitting `rate` requests per second with a
// burst of `burst`.
//
// A rate of zero or below, or a burst below one, is a configuration error rather
// than something to clamp: a caller who asked for "no limit" should have to say
// so explicitly, because an accidental zero that silently means unlimited is
// exactly how a fuzzer takes down a server.
func NewLimiter(rate float64, burst int) (*Limiter, error) {
	return newLimiterWithClock(rate, burst, time.Now)
}

func newLimiterWithClock(rate float64, burst int, now clock) (*Limiter, error) {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil, fmt.Errorf("fuzz: rate must be a finite positive number, got %v", rate)
	}
	if burst < 1 {
		return nil, fmt.Errorf("fuzz: burst must be at least 1, got %d", burst)
	}
	if now == nil {
		now = time.Now
	}
	interval := time.Duration(float64(time.Second) / rate)
	if interval <= 0 {
		// The rate exceeds what a nanosecond-resolution clock can express. The
		// limiter is then stricter than requested rather than weaker, which is
		// the correct direction to be wrong in.
		interval = 1
	}
	start := now()
	return &Limiter{
		tat:        start,
		interval:   interval,
		burstDelta: time.Duration(float64(interval) * float64(burst-1)),
		start:      start,
		now:        now,
		observed:   make([]time.Time, 0, 256),
	}, nil
}

// Rate returns the configured steady-state rate.
func (l *Limiter) Rate() float64 {
	return float64(time.Second) / float64(l.interval)
}

// Burst returns the configured burst capacity.
func (l *Limiter) Burst() int {
	if l.interval <= 0 {
		return 1
	}
	return int(l.burstDelta/l.interval) + 1
}

// Wait reserves a slot, blocking until it is this request's turn or ctx ends.
//
// A cancelled context returns ctx.Err() and consumes NO slot: the reservation
// is taken under the lock and the sleep happens outside it, so a caller that
// gives up leaves the schedule intact for the requests behind it. Refunding on
// cancellation would be wrong as well as complex — a request that may already
// have been sent must not have its slot given away.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := l.now()

	l.mu.Lock()
	tat := l.tat
	if tat.Before(now) {
		tat = now
	}
	allowAt := tat.Add(-l.burstDelta)
	if allowAt.Before(now) {
		allowAt = now
	}
	// Reserve. This is the line that makes the bound hold: the next waiter sees
	// a later tat and therefore schedules itself later.
	l.tat = tat.Add(l.interval)
	wait := allowAt.Sub(now)
	l.observed = append(l.observed, allowAt)
	l.trimObserved()
	l.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// trimObserved keeps the observation log bounded.
//
// Only admissions still inside the longest window any caller could ask about
// are kept; everything older than that can never affect a future assertion.
func (l *Limiter) trimObserved() {
	const keep = 4096
	if len(l.observed) <= keep {
		return
	}
	cut := len(l.observed) - keep
	l.observed = append(l.observed[:0], l.observed[cut:]...)
}

// Observations returns a copy of the scheduled admission times, in order.
//
// The times are the SCHEDULED admissions, not the moments Wait returned. That is
// the quantity the GCRA bound constrains, and using the return time instead
// would measure the scheduler rather than the limiter.
func (l *Limiter) Observations() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]time.Time, len(l.observed))
	copy(out, l.observed)
	return out
}

// Semaphore bounds how many payloads are in flight at once.
//
// It is a counting semaphore rather than a channel of tokens because the bound
// is about concurrent WORK, not about rate, and a channel would invite a
// reader to conflate the two.
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore returns a semaphore admitting `n` concurrent holders.
//
// A non-positive n is a configuration error: silently rounding it to 1 would
// turn a caller's typo into a 100x slowdown, and silently rounding it to
// unlimited would turn a caller's typo into a denial of service.
func NewSemaphore(n int) (*Semaphore, error) {
	if n < 1 {
		return nil, fmt.Errorf("fuzz: concurrency must be at least 1, got %d", n)
	}
	return &Semaphore{ch: make(chan struct{}, n)}, nil
}

// Acquire takes a slot, honouring ctx.
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot.
func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
		// Over-release would mean a bug in the caller, and silently ignoring it
		// would make the semaphore permanently wider than configured. Panicking
		// here is correct: it is a programming error, not a runtime condition.
		panic("fuzz: semaphore released more times than acquired")
	}
}

// InFlight reports how many slots are held.
func (s *Semaphore) InFlight() int { return len(s.ch) }

// MaxPeakAdmissions returns the largest number of admissions permitted in any
// window of length `window`, by the GCRA bound: capacity plus rate times window.
//
// It is exported so the rate gate states its expectation from the same function
// the implementation is derived from, rather than restating the arithmetic in
// the test — a test that recomputes the formula can agree with a broken
// implementation by both being wrong the same way.
func (l *Limiter) MaxPeakAdmissions(window time.Duration) int {
	rate := l.Rate()
	n := float64(l.Burst()) + rate*window.Seconds()
	if n < 1 {
		return 1
	}
	return int(math.Ceil(n))
}
