//go:build race

package rag_test

// raceEnabled reports whether this binary was built with the race detector.
//
// It exists because the race detector instruments every memory access with
// shadow state, which inflates TotalAlloc by 8-10% for allocation-heavy code.
// A churn gate whose threshold sits within that margin passes under a normal
// build and fails under `-race` on identical source — a gate measuring the
// instrumentation, not the code. Callers use it to skip the churn assertion
// rather than to loosen the bound (see TestMemoryCeilingRSSTotalAlloc).
const raceEnabled = true
