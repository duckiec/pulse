//go:build race

package rag_test

// Under the race detector the wall clock measures instrumentation overhead,
// not product latency, so the 50k gate reports its latency leg instead of
// enforcing the 50ms production budget. Every other assertion still runs.
const underRaceDetector = true
