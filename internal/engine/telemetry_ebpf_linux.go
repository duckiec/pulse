//go:build linux

package engine

import (
	"os"
	"strconv"
	"strings"
)

// privilegeReason reports why raw tracing cannot run, or "" when it can.
//
// The check is deliberately plain-file based: reading /proc costs nothing,
// requires no cgo, and works in a container where a syscall-based capability
// probe would be filtered. Linux exposes both the effective capability mask and
// the kernel's unprivileged-BPF switch, and both are consulted — a binary can
// have CAP_BPF and still be refused by sysctl, and vice versa.
func privilegeReason() string {
	// 1. Kernel policy: unprivileged BPF disabled means only CAP_BPF holders
	//    may load programs at all.
	if v, err := os.ReadFile("/proc/sys/kernel/unprivileged_bpf_disabled"); err == nil {
		if strings.TrimSpace(string(v)) == "1" {
			// Policy allows only privileged loading; fall through to the
			// capability check rather than declaring BLOCKED immediately.
			if !hasCapBPF() {
				return "kernel unprivileged_bpf_disabled=1 and no CAP_BPF in CapEff"
			}
		}
	}
	// 2. Capability: CAP_BPF is capability bit 39.
	if !hasCapBPF() {
		return "no CAP_BPF (bit 39) in CapEff; raw tracing needs CAP_BPF or CAP_SYS_ADMIN"
	}
	return ""
}

// hasCapBPF reads CapEff from /proc/self/status and tests bit 39 (CAP_BPF),
// also accepting CAP_SYS_ADMIN (bit 21) as the pre-5.8 route.
func hasCapBPF() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	const (
		capSysAdmin = 21
		capBPF      = 39
	)
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		if err != nil {
			return false
		}
		const (
			maskSysAdmin = 1 << capSysAdmin
			maskBPF      = 1 << capBPF
		)
		return mask&maskBPF != 0 || mask&maskSysAdmin != 0
	}
	return false
}
