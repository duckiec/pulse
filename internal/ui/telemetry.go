package ui

import (
	"fmt"
	"strings"
	"time"

	"pulse/internal/engine"
)

// RenderTelemetry is the dedicated timing component: a status badge with the
// protocol, size and wall time on the first row, microsecond transport phases
// on the second. It is a pure function of a snapshot and a result — no locks
// taken here (Snapshot took them), no I/O, no clock — so the G-frame gate can
// call it per frame without measuring it.
//
// A phase that never ran renders as "—", never as zero: a reused connection
// has no DNS phase, and fabricating "0µs" would claim a measurement that
// never happened (the same honesty rule the engine's own report follows).
func RenderTelemetry(snap engine.TelemetrySnapshot, res *engine.RunResult) string {
	var b strings.Builder
	if res != nil {
		label := res.StatusText
		if label == "" {
			label = fmt.Sprintf("%d", res.Status)
		}
		b.WriteString(styleStatus(res.Status).Render(" " + label + " "))
		proto := res.Proto
		if proto == "" {
			proto = "HTTP"
		}
		fmt.Fprintf(&b, "  %s", styleMuted.Render(
			fmt.Sprintf("%s  %s  %s", proto, formatBytes(res.Bytes), res.Elapsed.Round(time.Microsecond))))
		if res.Symbol != "" {
			b.WriteString("  " + styleMuted.Render(res.Symbol))
		}
		b.WriteByte('\n')
	}
	b.WriteString(phasesLine(snap))
	return b.String()
}

// phasesLine is the transport breakdown: one row of "label value" pairs. It
// is shared by the finished response and the running one, so a live stream
// shows first-byte as soon as it is known rather than only at the end.
func phasesLine(snap engine.TelemetrySnapshot) string {
	rows := []struct {
		label string
		d     time.Duration
	}{
		{"dns", snap.DNS.Duration()},
		{"tcp", snap.Connect.Duration()},
		{"tls", snap.TLS.Duration()},
		{"wrote", snap.Wrote.Duration()},
		{"first-byte", snap.FirstByte.Duration()},
		{"total", snap.Total},
	}
	parts := make([]string, 0, len(rows)+1)
	for _, r := range rows {
		parts = append(parts, r.label+" "+formatDuration(r.d))
	}
	if snap.ReusedConn {
		parts = append(parts, "reused-conn")
	}
	return strings.Join(parts, "  ")
}

// formatDuration renders a phase for a character-cell grid: microsecond
// precision while it matters, milliseconds above that, and an honest dash
// for a phase that never ran.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// formatBytes renders a body size for the status line. Binary units: the
// engine's own ceiling is expressed in KiB (32 KiB chunks, 2 MiB cap), and a
// dashboard that disagrees about what a megabyte is sends the reader to
// divide by 1.048576 mid-incident.
func formatBytes(n int64) string {
	switch {
	case n < 0:
		return "—"
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2fMiB", float64(n)/(1024*1024))
	}
}
