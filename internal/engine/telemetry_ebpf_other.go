//go:build !linux

package engine

// On non-Linux there is no eBPF at all: the capability probe has no meaning,
// so raw tracing is permanently BLOCKED and the httptrace timings are the only
// source. Reporting that plainly is the honest disposition (AGENTS.md §4.5) —
// pretending a syscall-level tracer exists here would be worse than useless.
func privilegeReason() string {
	return "eBPF is a Linux facility; this platform has no raw-tracing backend"
}
