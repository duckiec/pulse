package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"pulse/internal/engine"
)

// Styles are package-level lipgloss values: allocation happens once, and
// View only formats. No raw ANSI anywhere (DESIGN_SYSTEM.md §2).
var (
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C9D1D9"))
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B949E"))
	styleAccent  = lipgloss.NewStyle().Foreground(lipgloss.Color("#58A6FF"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FB950"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("#F85149"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("#D29922"))
	stylePane    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#30363D"))
	styleFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#58A6FF"))
)

// View renders the whole screen as a pure function of the model. Law: no
// I/O, no time calls, no map iteration — the same model renders the same
// bytes on every frame, which is what makes the p99 gate meaningful.
func (m *Model) View() string {
	if m.width < 80 || m.height < 24 {
		return m.viewRefusal()
	}
	var b strings.Builder
	b.WriteString(m.viewHeader())
	b.WriteByte('\n')
	if m.width >= 100 {
		b.WriteString(m.viewDualPane())
	} else {
		b.WriteString(m.viewSinglePane())
	}
	b.WriteByte('\n')
	b.WriteString(m.viewTiming())
	b.WriteByte('\n')
	b.WriteString(m.viewStatus())
	b.WriteByte('\n')
	b.WriteString(m.viewFooter())
	if m.showHelp {
		b.WriteByte('\n')
		b.WriteString(m.viewHelp())
	}
	return b.String()
}

// viewRefusal is the sub-80-column screen (DESIGN_SYSTEM.md §3.2): a refusal
// to render half-broken UI, stated in words rather than drawn in fragments.
func (m *Model) viewRefusal() string {
	return styleErr.Render(fmt.Sprintf(
		"pulse: terminal too narrow (%dx%d; need 80x24) — widen the window or pipe to `pulse test`",
		m.width, m.height))
}

// viewHeader is the fixed 1-row brand strip: collection, environment,
// connection phase.
func (m *Model) viewHeader() string {
	doc := ""
	if m.doc != nil && m.doc.Path != "" {
		doc = m.doc.Path
	}
	return styleHeader.Render("pulse") + styleMuted.Render(" ─ "+doc+" ─ "+m.phase.String())
}

// viewDualPane is the ≥100-column layout: request pane beside response pane,
// the focused one carrying the accent border. Widths split evenly; height is
// whatever the window allows minus chrome.
func (m *Model) viewDualPane() string {
	paneW := (m.width - 5) / 2
	if paneW < 20 {
		paneW = 20
	}
	left := m.renderRequestPane(paneW)
	right := m.renderResponsePane(paneW)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// viewSinglePane is the 80–99-column layout: response only, the request
// reduced to its one-line summary in the status bar.
func (m *Model) viewSinglePane() string {
	return m.renderResponsePane(m.width - 2)
}

// renderRequestPane shows the request under execution: method, target, name.
// Values come from the construction snapshot, never from the arena per frame.
func (m *Model) renderRequestPane(w int) string {
	st := stylePane
	if m.focus == 0 {
		st = styleFocused
	}
	body := fmt.Sprintf("%s %s", m.method, truncate(m.url, w-4))
	if m.name != "" {
		body += "\n" + styleMuted.Render("@"+m.name)
	}
	return st.Width(w).Render(body)
}

// renderResponsePane is the streaming viewport: the bounded tail plus a
// tail-first indicator while running.
func (m *Model) renderResponsePane(w int) string {
	st := stylePane
	if m.focus == 1 || m.width < 100 {
		st = styleFocused
	}
	title := "response"
	if m.phase == phaseRunning {
		title += " ● streaming"
	}
	content := m.vp.View()
	return st.Width(w).Render(title + "\n" + content)
}

// viewTiming is the telemetry strip: status and phases on one row. The
// snapshot is lock-guarded at the source, so reading it here races nothing
// even mid-stream.
func (m *Model) viewTiming() string {
	text := "timing —"
	if m.report != nil {
		text = RenderTelemetry(m.report.Snapshot(), m.res)
	} else if m.res != nil {
		text = RenderTelemetry(engine.TelemetrySnapshot{}, m.res)
	}
	return styleMuted.Render(text)
}

// viewStatus is the fixed 1-row run summary: byte/line counters, drops,
// frame count. Drops are shown, never hidden: a stream the UI could not keep
// up with is a fact the operator needs, not an embarrassment to trim.
func (m *Model) viewStatus() string {
	status := fmt.Sprintf("run: %s %s  %s  lines %d",
		m.method, truncate(m.url, 48), m.phase.String(), m.totalLines)
	if m.res != nil {
		status += fmt.Sprintf("  %d %s", m.res.Status, formatBytes(m.res.Bytes))
	}
	if m.droppedLines > 0 || m.droppedBytes > 0 {
		status += styleWarn.Render(fmt.Sprintf("  dropped %d lines / %s",
			m.droppedLines, formatBytes(m.droppedBytes)))
	}
	if m.err != nil {
		status += styleErr.Render("  " + firstLine(m.err.Error()))
	}
	if m.toast != "" {
		status += styleAccent.Render("  " + m.toast)
	}
	return status
}

// viewFooter renders the contextual keybinding hints from the central keymap:
// the footer cannot advertise a binding the keymap does not hold, because it
// reads the same struct the dispatcher matches.
func (m *Model) viewFooter() string {
	return styleMuted.Render("?: help   " + strings.Join(m.keys.FooterHelp(), "   "))
}

// viewHelp is the overlay: every binding, rendered from the keymap.
func (m *Model) viewHelp() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("keymap"))
	for _, binding := range m.keys.All() {
		fmt.Fprintf(&b, "\n  %-16s %s", strings.Join(binding.Keys, "/"), binding.Desc)
	}
	return stylePane.Render(b.String())
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
