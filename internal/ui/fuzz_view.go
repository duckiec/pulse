package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ProbeEvent is one fuzz probe verdict as the dashboard sees it. The fields
// are primitives on purpose: this package never imports internal/fuzz or
// internal/mcp (ui holds no transport and no store), so the producer side
// converts once at the boundary and the dashboard programs against this.
//
// A test producer, a tea.Cmd poller, and the future production wiring all
// construct these the same way — the dashboard cannot tell them apart, which
// is what makes the headless gate a proof about the production path.
type ProbeEvent struct {
	Index     int
	Total     int
	Passed    bool
	Failed    bool
	Skipped   bool
	Status    int
	Payload   string
	Note      string
	FindingID string
	At        time.Time
}

// probeMsg carries one bridged probe verdict to the UI thread.
type probeMsg struct {
	ev ProbeEvent
}

// ProgressBridge is the FuzzOptions.Progress-side channel into the UI. The
// producer (a Progress callback, a test, or any goroutine) calls Send; the
// program polls with PollCmd. Both directions are non-blocking: Send drops
// with a count when the ring is full rather than back-pressuring the fuzzer,
// and PollCmd returns nil when the ring is empty rather than parking the
// runtime. Neither direction touches stdout — the JSON-RPC wire is never in
// this call graph (AGENTS.md §3.3).
type ProgressBridge struct {
	mu      sync.Mutex
	ring    []ProbeEvent
	dropped int
}

// bridgeRingCap bounds the bridge. A campaign can hold thousands of probes;
// the dashboard consumes per frame, so the ring only needs to cover the
// burst between two polls.
const bridgeRingCap = 256

// Send enqueues one event, stamping its arrival. It never blocks: a full
// ring drops the oldest event with a count the dashboard shows.
func (b *ProgressBridge) Send(ev ProbeEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.ring) >= bridgeRingCap {
		b.ring = append(b.ring[:0], b.ring[1:]...)
		b.dropped++
	}
	b.ring = append(b.ring, ev)
}

// PollCmd returns a tea.Cmd delivering at most one queued event. Nil means
// the ring is empty — the runtime idles rather than spins.
func (b *ProgressBridge) PollCmd() tea.Cmd {
	return func() tea.Msg {
		b.mu.Lock()
		defer b.mu.Unlock()
		if len(b.ring) == 0 {
			return nil
		}
		ev := b.ring[0]
		b.ring = append(b.ring[:0], b.ring[1:]...)
		return probeMsg{ev: ev}
	}
}

// Dropped reports bridge-level drops.
func (b *ProgressBridge) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// FuzzModel is the live fuzz dashboard: a second StreamView instance for the
// probe tail plus Pass/Fail/Skip counters, achieved RPS, and persisted
// finding badges. It is a sub-model: the owning Model routes probeMsg to
// UpdateProbe and renders View when the fuzz tab is active.
type FuzzModel struct {
	stream   *StreamView
	passed   int
	failed   int
	skipped  int
	total    int
	received int
	started  time.Time
	findings []string
	rps      float64
}

// maxFindingBadges bounds the badge list. Findings are persisted to the
// store by the runner; the dashboard shows the trailing window, not the
// archive.
const maxFindingBadges = 32

// NewFuzzModel returns an empty dashboard sized to w×h.
func NewFuzzModel(w, h int) *FuzzModel {
	return &FuzzModel{stream: NewStreamView(w, h)}
}

// Resize fits the owned probe tail.
func (f *FuzzModel) Resize(w, h int) { f.stream.Resize(w, h) }

// SetASCII degrades the owned tail's chrome glyphs.
func (f *FuzzModel) SetASCII(ascii bool) { f.stream.SetASCII(ascii) }

// UpdateProbe consumes one verdict: counters advance, the probe line enters
// the owned StreamView, and a finding id becomes a badge. It is pure
// model-side work — no I/O, no blocking — so it is safe inside Update.
func (f *FuzzModel) UpdateProbe(ev ProbeEvent) {
	if f.started.IsZero() {
		f.started = ev.At
		if f.started.IsZero() {
			f.started = time.Now()
		}
	}
	f.received++
	f.total = ev.Total
	switch {
	case ev.Skipped:
		f.skipped++
	case ev.Passed:
		f.passed++
	case ev.Failed:
		f.failed++
	default:
		f.skipped++
	}
	verdict := "ok"
	if ev.Skipped {
		verdict = "skip"
	} else if ev.Failed {
		verdict = "FAIL"
	}
	line := fmt.Sprintf("%d/%d %s [%s] %s", ev.Index+1, ev.Total, verdict, ev.Note, ev.Payload)
	if ev.FindingID != "" {
		line += " finding:" + ev.FindingID
		f.findings = append(f.findings, ev.FindingID)
		if len(f.findings) > maxFindingBadges {
			f.findings = f.findings[len(f.findings)-maxFindingBadges:]
		}
	}
	f.stream.Append([]byte(line + "\n"))
	f.stream.Flush()
	if !ev.At.IsZero() && ev.At.After(f.started) {
		if dt := ev.At.Sub(f.started).Seconds(); dt > 0 {
			f.rps = float64(f.received) / dt
		}
	}
}

// Counts exposes the dashboard counters for gates.
func (f *FuzzModel) Counts() (passed, failed, skipped, received int) {
	return f.passed, f.failed, f.skipped, f.received
}

// RPS reports the achieved probe rate.
func (f *FuzzModel) RPS() float64 { return f.rps }

// Findings returns the badge window.
func (f *FuzzModel) Findings() []string { return append([]string(nil), f.findings...) }

// View renders the dashboard: counters and RPS on top, finding badges, then
// the streaming probe tail.
func (f *FuzzModel) View(w int, ascii bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fuzz  pass %d  fail %d  skip %d  received %d/%d  %.1f rps",
		f.passed, f.failed, f.skipped, f.received, f.total, f.rps)
	if len(f.findings) > 0 {
		b.WriteString("\nfindings:")
		for _, id := range f.findings {
			b.WriteString(" [" + shortenID(id) + "]")
		}
	}
	b.WriteString("\n" + f.stream.Visible())
	return paneStyle(true, ascii).Width(w).Render(b.String())
}

func shortenID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
