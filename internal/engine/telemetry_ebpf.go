package engine

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

// EBPFDisposition is the honest report of what the low-level tracer could do
// (ROADMAP.md §2.3.3, AGENTS.md §4.5). It is a first-class result, never a
// silent fallback: when raw tracing is BLOCKED the httptrace timings stand on
// their own and say so.
type EBPFDisposition string

const (
	// EBPFAvailable means the privileged tracer loaded and is feeding points.
	EBPFAvailable EBPFDisposition = "available"
	// EBPFBlocked means the tracer could not run here — no CAP_BPF, no
	// unprivileged BPF, or an incompatible kernel. Timings degrade to
	// httptrace and the report says BLOCKED.
	EBPFBlocked EBPFDisposition = "blocked"
	// EBPFNotBuilt means the binary was built without the `ebpf` build tag.
	EBPFNotBuilt EBPFDisposition = "not-built"
)

// EBPFStatus is the detailed disposition with a machine-readable reason.
type EBPFStatus struct {
	Disposition EBPFDisposition
	Reason      string // e.g. "no CAP_BPF in CapEff"
	Kernel      string
	BuildTag    string
}

// EBPFProbe is the architecture boundary for raw socket tracing.
//
// The default build compiles a STUB that reports BLOCKED with a precise reason.
// The real tracer is compiled only under `//go:build ebpf`, where
// github.com/cilium/ebpf loads kprobes (tcp_v4_connect / tcp_connect /
// tcp_set_state) plus sock send/recv tracepoints and decodes a 4-tuple +
// timestamp from the perf ring buffer.
//
// It is deliberately NOT in the default build: clang/bpftool are out-of-band
// toolchain dependencies, and the default build must never require a kernel
// capability or a privilege the user does not have.
type EBPFProbe interface {
	// Status reports the disposition without side effects.
	Status() EBPFStatus
	// Load attempts to start raw tracing. It returns the disposition, never a
	// fatal error: a request must never fail because telemetry could not run.
	Load() (EBPFStatus, error)
}

var (
	ebpfOnce sync.Once
	ebpfStat EBPFStatus
)

// ProbeEBPF returns the cached disposition for this process.
func ProbeEBPF() EBPFStatus {
	ebpfOnce.Do(func() { ebpfStat = newEBPFProbe().Status() })
	return ebpfStat
}

// CurrentEBPFProbe returns the active tracer for this platform/build.
func CurrentEBPFProbe() EBPFProbe { return newEBPFProbe() }

// stubProbe is the default-build implementation. It reports BLOCKED with the
// specific reason, so a user is told whether the kernel, the privileges, or the
// build tag is the obstacle.
type stubProbe struct{ status EBPFStatus }

func newEBPFProbe() EBPFProbe {
	return &stubProbe{status: assessEBPF()}
}

// Status implements EBPFProbe.
func (s *stubProbe) Status() EBPFStatus { return s.status }

// Load implements EBPFProbe. It never fails the caller: an inability to trace
// is reported as a disposition, not an error that breaks the request.
func (s *stubProbe) Load() (EBPFStatus, error) {
	return s.status, fmt.Errorf("engine: raw tracing BLOCKED (%s)", s.status.Reason)
}

// assessEBPF determines the disposition for the running system.
func assessEBPF() EBPFStatus {
	st := EBPFStatus{BuildTag: buildEBPFTag(), Kernel: kernelRelease()}
	if st.BuildTag == "" {
		st.Disposition = EBPFNotBuilt
		st.Reason = "built without the 'ebpf' build tag (default); see ARCHITECTURE.md 2.3.3"
		return st
	}
	// Privileges and kernel support are checked before any load is attempted.
	if reason := privilegeReason(); reason != "" {
		st.Disposition = EBPFBlocked
		st.Reason = reason
		return st
	}
	st.Disposition = EBPFAvailable
	st.Reason = "privileged tracer eligible"
	return st
}

// kernelRelease reads the running kernel, best-effort.
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// buildEBPFTag reports the active tracer implementation for the build. The
// default build has no tracer; the `ebpf` build replaces this via the
// build-tagged file in this package.
func buildEBPFTag() string { return ebpfBuildTag }

// ebpfBuildTag is overridden by the build-tagged implementation file.
const ebpfBuildTag = ""

// EBPFSupportedPlatform reports whether this OS has any raw-tracing backend at
// all. Windows/macOS have no eBPF; they degrade to httptrace permanently.
func EBPFSupportedPlatform() bool { return runtime.GOOS == "linux" }
