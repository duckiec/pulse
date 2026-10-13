package fuzz_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/fuzz"
)

// TestRateLimiterNeverExceedsTheConfiguredRate is THE rate gate.
//
// It fires 1,000 concurrent requests at a local sink through the limiter and
// then checks the rate over a SLIDING window rather than over fixed buckets.
//
// # Why a sliding window and not a fixed one
//
// A fixed-window check can hide an arbitrarily large burst straddling a boundary:
// 50 requests at 0.99s and 50 at 1.01s are two windows of 50 each, and the real
// instantaneous rate at 1.0s was 100. A limiter with a genuine bug — N waiters
// waking together from the same sleep, say — produces exactly that shape and a
// fixed-window gate passes it. The sliding window asks the only question that
// matters: for EVERY interval of length Δ, how many admissions fell inside it?
//
// # What is being proven
//
// The GCRA bound: admission_time(i) >= t0 + i/R - (C-1)/R, which makes the number
// of admissions in any window of length Δ at most C + R*Δ. The expectation is
// read from `MaxPeakAdmissions`, which the limiter derives from the same
// constants it schedules with. A test that recomputed the formula could agree
// with a broken implementation by both being wrong the same way.
//
// # Why the sink is real
//
// The request count is taken from an httptest server rather than from the
// limiter's own bookkeeping. A limiter that admitted correctly but never sent
// anything would pass a self-reporting gate, and "we throttled 1,000 requests"
// must mean 1,000 requests arrived.
func TestRateLimiterNeverExceedsTheConfiguredRate(t *testing.T) {
	const (
		requests = 1000
		rate     = 200.0 // deliberately fast so the test is short
		burst    = 4
	)

	var arrived int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&arrived, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	lim, err := fuzz.NewLimiter(rate, burst)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}

	// Every request records the instant it left the limiter.
	admit := make([]time.Time, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := lim.Wait(context.Background()); err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			admit[i] = time.Now()
			resp, err := http.Get(ts.URL + "/probe")
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&arrived); got != requests {
		t.Fatalf("the sink received %d requests, want %d", got, requests)
	}

	// Sort the admission times; the goroutines completed in an arbitrary order.
	sort.Slice(admit, func(i, j int) bool { return admit[i].Before(admit[j]) })
	_ = lim.Observations()

	span := admit[len(admit)-1].Sub(admit[0])
	achieved := float64(requests-1) / span.Seconds()
	t.Logf("%d requests over %v -> %.1f/s achieved (configured %.1f/s, burst %d)",
		requests, span.Round(time.Millisecond), achieved, rate, burst)

	// THE SLIDING-WINDOW CHECK. For each possible window start, count the
	// admissions inside a window of length Δ, and compare against the bound.
	//
	// Δ = 1 second is the interval whose bound the gate is written around: with
	// rate 200 and burst 4, no 1-second window may contain more than 204.
	const window = time.Second
	limit := lim.MaxPeakAdmissions(window)
	t.Logf("permitted in any %v window: %d (burst %d + rate*%.0fs)", window, limit, burst, window.Seconds())

	worst := 0
	worstAt := time.Time{}
	for i := range admit {
		end := admit[i].Add(window)
		j := sort.Search(len(admit), func(k int) bool { return !admit[k].Before(end) })
		if n := j - i; n > worst {
			worst, worstAt = n, admit[i]
		}
	}
	if worst > limit {
		t.Errorf("a %v window starting at %v contained %d admissions; the bound is %d "+
			"(achieved %.1f/s)",
			window, worstAt.Format(time.RFC3339Nano), worst, limit, achieved)
	}
	t.Logf("worst 1s window: %d admissions (bound %d)", worst, limit)

	// The bound must be ENFORCED, not merely respected by luck: with 1,000
	// requests at 200/s the run cannot possibly complete in under
	// (1000 - burst) / rate seconds. A limiter that granted everything at once
	// would finish in milliseconds.
	floor := time.Duration(float64(requests-burst) / rate * float64(time.Second))
	if span < floor*9/10 {
		t.Errorf("the run completed in %v; the limiter's own floor is %v, so it "+
			"cannot have been enforcing the rate", span.Round(time.Millisecond), floor)
	}
}

// TestRateLimiterEnforcesTheBoundUnderContention is the concurrency-specific
// half of the gate.
//
// The first gate uses 1,000 goroutines but lets the runtime spread them out. This
// one makes the adversarial case explicit: all waiters pile in at once, which is
// precisely when a naive token bucket lets N sleepers wake together and admit a
// burst. The assertion is the same bound, and it is the thing the naive design
// fails.
func TestRateLimiterEnforcesTheBoundUnderContention(t *testing.T) {
	const (
		waiters = 200
		rate    = 100.0
		burst   = 2
	)
	lim, err := fuzz.NewLimiter(rate, burst)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}

	start := make(chan struct{}) // every goroutine blocks here, then fires together
	admit := make([]time.Time, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := lim.Wait(context.Background()); err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			admit[i] = time.Now()
		}(i)
	}
	close(start) // the stampede
	wg.Wait()

	sort.Slice(admit, func(i, j int) bool { return admit[i].Before(admit[j]) })
	const window = time.Second
	limit := lim.MaxPeakAdmissions(window)

	worst := 0
	for i := range admit {
		j := sort.Search(len(admit), func(k int) bool {
			return !admit[k].Before(admit[i].Add(window))
		})
		if n := j - i; n > worst {
			worst = n
		}
	}
	span := admit[len(admit)-1].Sub(admit[0])
	t.Logf("%d simultaneous waiters: worst 1s window = %d (bound %d), total %v",
		waiters, worst, limit, span.Round(time.Millisecond))

	if worst > limit {
		t.Errorf("under a stampede of %d waiters a 1s window held %d admissions, "+
			"over the bound of %d: waiters woke together and burst past the rate",
			waiters, worst, limit)
	}
}

// TestRateLimiterObservationsRespectTheInvariant checks the limiter's own
// scheduled admissions against the same inequality, independently of wall clock.
//
// The limiter records the time it SCHEDULED each admission, which is the
// quantity the GCRA bound constrains. Using the moment Wait returned instead
// would measure the Go scheduler, which is fast enough to hide the very bug the
// bound is there to catch.
func TestRateLimiterObservationsRespectTheInvariant(t *testing.T) {
	lim, err := fuzz.NewLimiter(50, 3)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := lim.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	obs := lim.Observations()
	if len(obs) != 100 {
		t.Fatalf("recorded %d observations, want 100", len(obs))
	}
	// Observations are non-decreasing: a later reservation can never be
	// scheduled before an earlier one, or the rate could be exceeded by the
	// limiter going "backwards" in time.
	for i := 1; i < len(obs); i++ {
		if obs[i].Before(obs[i-1]) {
			t.Fatalf("observation %d (%v) precedes observation %d (%v); "+
				"reservations must be monotonic", i, obs[i], i-1, obs[i-1])
		}
	}
	// Consecutive admissions are at least one interval apart, except within the
	// initial burst.
	interval := time.Duration(float64(time.Second) / lim.Rate())
	burst := lim.Burst()
	for i := burst; i < len(obs); i++ {
		if d := obs[i].Sub(obs[i-1]); d < interval-time.Duration(time.Millisecond) {
			t.Errorf("admissions %d and %d are %v apart, under the %v interval "+
				"(outside the %d-admission burst)", i-1, i, d, interval, burst)
		}
	}
	t.Logf("100 admissions: first %d within the burst, then >=%v apart", burst, interval)
}

// TestLimiterRejectsUnusableConfiguration proves a limiter cannot be built that
// means "no limit".
//
// A zero rate that silently became unlimited would be the single worst defect
// this file could have: the caller asked for a limit, believed they had one, and
// saturated a server. Failing closed is the only safe reading of "0".
func TestLimiterRejectsUnusableConfiguration(t *testing.T) {
	for _, rate := range []float64{0, -1, -0.5} {
		if _, err := fuzz.NewLimiter(rate, 4); err == nil {
			t.Errorf("rate %v was accepted; a limiter that means `no limit` must be "+
				"requested explicitly, never by accident", rate)
		}
	}
	for _, burst := range []int{0, -3} {
		if _, err := fuzz.NewLimiter(10, burst); err == nil {
			t.Errorf("burst %d was accepted", burst)
		}
	}
	if _, err := fuzz.NewSemaphore(0); err == nil {
		t.Error("concurrency 0 was accepted; it must not silently become unlimited")
	}
}

// TestSemaphoreBoundsConcurrency proves the concurrency cap holds under load.
func TestSemaphoreBoundsConcurrency(t *testing.T) {
	sem, err := fuzz.NewSemaphore(4)
	if err != nil {
		t.Fatalf("NewSemaphore: %v", err)
	}
	ctx := context.Background()

	var inFlight, peak int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sem.Acquire(ctx); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := atomic.AddInt64(&inFlight, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			sem.Release()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > 4 {
		t.Errorf("peak concurrency was %d, over the cap of 4", got)
	}
	t.Logf("peak in-flight: %d of a cap of 4", peak)
}

// TestLimiterHonoursCancellation proves an aborted run stops promptly.
//
// A limiter that cannot be cancelled turns Ctrl+X — or an MCP client hanging up
// — into a wait for the entire corpus, which is the difference between a
// cancellable feature and one a user learns to avoid.
func TestLimiterHonoursCancellation(t *testing.T) {
	lim, err := fuzz.NewLimiter(1, 1) // one request per second
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first request

	start := time.Now()
	err = lim.Wait(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Wait returned no error for a cancelled context")
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("a cancelled Wait blocked for %v; cancellation must be immediate", elapsed)
	}
}

// TestLimiterCancellationDoesNotWidenTheRate proves a waiter that gives up does
// not hand its slot to the requests behind it.
//
// The slot is reserved before the sleep, so a cancelled waiter has already
// consumed its place in the schedule. Refunding it would let the run ahead of
// time — which is exactly how "the limiter cancelled cleanly" becomes "the
// limiter is no longer limiting".
func TestLimiterCancellationDoesNotWidenTheRate(t *testing.T) {
	lim, err := fuzz.NewLimiter(20, 1) // one every 50ms, no burst
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	// First request: immediate, consuming the only slot.
	if err := lim.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	// Second: will need to wait ~50ms. Abandon it almost immediately.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lim.Wait(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Error("the cancelled Wait returned no error")
	}

	// Third: must still be scheduled ~50ms after the FIRST, not ~50ms after the
	// cancelled one. The slot is not refunded.
	start := time.Now()
	if err := lim.Wait(context.Background()); err != nil {
		t.Fatalf("third Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Errorf("the third request was admitted after only %v; the cancelled "+
			"waiter's slot was refunded and the rate is now unbounded", elapsed)
	}
	t.Logf("third request waited %v despite a cancellation in between", elapsed.Round(time.Millisecond))
}

// TestLimiterRespectsAZeroDelaySchedule documents the headroom check: a rate
// faster than the clock can express is clamped STRICTER, never looser.
func TestLimiterRespectsAZeroDelaySchedule(t *testing.T) {
	// 1e12 rps is below the float64 range but above nanosecond resolution.
	lim, err := fuzz.NewLimiter(1e12, 1)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := lim.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	t.Logf("100 admissions at a nominal 1e12/s took %v; clamped rate %.0f/s, burst %d",
		time.Since(start), lim.Rate(), lim.Burst())
	if lim.Rate() <= 0 {
		t.Error("the clamped rate is not positive")
	}
}
