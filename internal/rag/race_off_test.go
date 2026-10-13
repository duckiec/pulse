//go:build !race

package rag_test

// raceEnabled reports whether this binary was built with the race detector.
// See race_on_test.go for why the churn gate consults it.
const raceEnabled = false
