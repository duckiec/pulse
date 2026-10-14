package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFuzzDashboardRendersEveryBridgedProbe is the headless TUI fuzz gate: N
// verdicts cross a ProgressBridge exactly the way a FuzzOptions.Progress
// callback would hand them over — Send on the producer side, PollCmd on the
// runtime side — and the dashboard must render every one: counters exact,
// probe tail non-empty, finding badges present, RPS positive.
//
// Mutation M90 disconnects the bridge (Send stops forwarding): the
// dashboard renders zero probes and every assertion below goes red.
func TestFuzzDashboardRendersEveryBridgedProbe(t *testing.T) {
	const n = 20
	bridge := &ProgressBridge{}
	base := time.Now()
	for i := 0; i < n; i++ {
		ev := ProbeEvent{
			Index: i, Total: n,
			Note:    fmt.Sprintf("boundary-%d", i),
			Payload: fmt.Sprintf("%q", fmt.Sprintf("probe-%d", i)),
			At:      base.Add(time.Duration(i) * 50 * time.Millisecond),
		}
		switch i % 4 {
		case 0, 1:
			ev.Passed = true
		case 2:
			ev.Failed = true
			ev.Status = 500
			ev.FindingID = fmt.Sprintf("finding-%04d", i)
		default:
			ev.Skipped = true
		}
		bridge.Send(ev)
	}

	fm := NewFuzzModel(80, 24)
	for i := 0; i < n; i++ {
		msg := bridge.PollCmd()()
		pm, ok := msg.(probeMsg)
		if !ok {
			t.Fatalf("poll %d: bridge empty with %d of %d probes consumed (bridge dropped %d)",
				i, i, n, bridge.Dropped())
		}
		fm.UpdateProbe(pm.ev)
	}

	passed, failed, skipped, received := fm.Counts()
	if received != n {
		t.Errorf("received = %d, want %d (probes lost between bridge and dashboard)", received, n)
	}
	if passed != 10 || failed != 5 || skipped != 5 {
		t.Errorf("counters = %d/%d/%d, want 10/5/5", passed, failed, skipped)
	}
	if fm.RPS() <= 0 {
		t.Error("RPS is not positive after 20 timestamped probes")
	}
	if got := len(fm.Findings()); got != 5 {
		t.Errorf("finding badges = %d, want 5", got)
	}
	out := fm.View(78, false)
	for _, want := range []string{"pass 10", "fail 5", "skip 5", "probe-0", "probe-19", "finding-"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard omits %q", want)
		}
	}
	if bridge.Dropped() != 0 {
		t.Errorf("bridge dropped %d events on a %d-probe campaign it should absorb", bridge.Dropped(), n)
	}
}

// TestFuzzDashboardStartsEmpty proves the zero state renders without
// counters fabricated from nothing.
func TestFuzzDashboardStartsEmpty(t *testing.T) {
	fm := NewFuzzModel(80, 24)
	if p, f, s, r := fm.Counts(); p+f+s+r != 0 {
		t.Errorf("fresh dashboard counts %d/%d/%d/%d, want zeros", p, f, s, r)
	}
	if fm.RPS() != 0 {
		t.Error("fresh dashboard reports a rate for no probes")
	}
}
