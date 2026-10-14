package fuzz_test

// Synctest bubble for the GCRA limiter's virtual-clock exactness
// (ROADMAP P3.3.6, AGENTS.md §2.2).
//
// The limiter's contract is a TIMING property: at 10 r/s with burst 2, six
// admissions must cost exactly 400ms (the first two ride the burst, the rest
// pace at 100ms). A wall-clock test would sleep nearly half a second and
// still tolerate scheduling jitter; this bubble runs the real Wait timers
// against the synthetic clock, so the assertion is exact and the whole gate
// costs microseconds of wall time.

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"pulse/internal/fuzz"
)

func TestBubbleGCRAVirtualClockExactness(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		l, err := fuzz.NewLimiter(10, 2)
		if err != nil {
			t.Fatalf("NewLimiter: %v", err)
		}
		ctx := context.Background()
		start := time.Now()
		for i := 0; i < 6; i++ {
			if err := l.Wait(ctx); err != nil {
				t.Fatalf("Wait %d: %v", i, err)
			}
		}
		if got := time.Since(start); got != 400*time.Millisecond {
			t.Fatalf("6 admissions at 10/s burst 2 took %v virtual, want exactly 400ms", got)
		}
	})
	if wall := time.Since(wallStart); wall > 5*time.Second {
		t.Errorf("bubble consumed %v wall time: a real sleep leaked into the limiter path", wall)
	}
}
