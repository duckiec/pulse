package ui

import (
	"fmt"
	"strings"
	"time"

	"pulse/internal/engine"
)

// RenderTelemetry is the dedicated timing component: microsecond-precise
// transport phases beside the HTTP/gRPC status. It is a pure function of a
// snapshot and a result — no locks taken here (Snapshot took them), no I/O,
// no clock — so the G-frame gate can call it per frame without measuring it.
//
// A phase that never ran renders as "—", never as zero: a reused connection
// has no DNS phase, and fabricating "0µs" would claim a measurement that
// never happened (the same honesty rule the engine's own report follows).
func RenderTelemetry(snap engine.TelemetrySnapshot, res *engine.RunResult) string {
	var b strings.Builder
	if res != nil {
		fmt.Fprintf(&b, "%s %d", res.Proto, res.Status)
		if res.StatusText != "" {
			fmt.Fprintf(&b, " %s", res.StatusText)
		}
		fmt.Fprintf(&b, "  %s  %s", formatBytes(res.Bytes), res.Elapsed.Round(time.Microsecond))
		if res.Symbol != "" {
			fmt.Fprintf(&b, "  %s", res.Symbol)
		}
		b.WriteByte('\n')
	}
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
	for i, r := range rows {
		if i > 0 {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%s %s", r.label, formatDuration(r.d))
	}
	if snap.ReusedConn {
		b.WriteString("  reused-conn")
	}
	return b.String()
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
