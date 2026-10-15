//go:build !race

package rag_test

// Native builds enforce the 50ms production budget on the latency leg.
const underRaceDetector = false
