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
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C9D1D9"))
	// Muted gray is #6E7681 rather than the spec's first-draft #8B949E: the
	// two are near-twins in truecolor, but under the 16-color profile the
	// draft quantizes to bright-blue (94) — the accent's own code — while
	// this one lands on cyan (36), distinct from every other token. The
	// degradation ladder (DESIGN_SYSTEM.md §4.2) is decided at 16 colors,
	// not at truecolor; the color-token gate pins all six text codes.
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6E7681"))
	styleAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("#58A6FF"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FB950"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F85149"))
	// Warning amber is #E3B341 rather than #D29922 for the same ladder
	// reason: the draft quantizes to bright-red (91) — the error code —
	// while this one lands on yellow (33). Same family, honest at 16.
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("#E3B341"))
	stylePane    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#30363D"))
	styleFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#58A6FF"))
)

// glyphs returns the render glyph set: Unicode default, ASCII on demand
// (DESIGN_SYSTEM.md §4.2 — never state by color alone, and the monochrome
// profile preserves every distinction because the words stay put).
func (m *Model) glyphs() (dash, dot, ell string) {
	if m.ascii {
		return "-", "*", "..."
	}
	return "─", "●", "…"
}

// asciiBorder is the degradation-ladder border: lipgloss's NormalBorder is
// still box-drawing (square corners, not ASCII), so the genuinely ASCII
// frame needs its own preset. The border is a glyph like any other, and the
// ascii gate asserts zero non-ASCII bytes in the whole frame.
var asciiBorder = lipgloss.Border{
	Top: "-", Bottom: "-", Left: "|", Right: "|",
	TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
	MiddleLeft: "+", MiddleRight: "+", Middle: "+",
	MiddleTop: "+", MiddleBottom: "+",
}

// paneStyle selects the pane border: focused panes carry the accent edge,
// and ASCII mode swaps the rounded border for the ASCII preset.
func paneStyle(focused, ascii bool) lipgloss.Style {
	st := stylePane
	if focused {
		st = styleFocused
	}
	if ascii {
		st = st.Border(asciiBorder)
	}
	return st
}

// trunc shortens to n columns with the active glyph set's ellipsis.
func (m *Model) trunc(s string, n int) string {
	_, _, ell := m.glyphs()
	return truncFor(s, n, ell)
}

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
	switch {
	case m.editor.Active():
		b.WriteString(m.editor.View(m.width-2, m.ascii))
	case m.palette.Active():
		b.WriteString(m.palette.View(m.width-2, m.ascii))
	case m.width >= 100:
		b.WriteString(m.viewDualPane())
	default:
		b.WriteString(m.viewSinglePane())
	}
	if m.search.Active() {
		b.WriteByte('\n')
		b.WriteString(styleAccent.Render("/" + m.search.Query()))
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
	dash, _, _ := m.glyphs()
	return styleErr.Render(fmt.Sprintf(
		"pulse: terminal too narrow (%dx%d; need 80x24) "+dash+" widen the window or pipe to `pulse test`",
		m.width, m.height))
}

// viewHeader is the fixed 1-row brand strip: collection, environment,
// connection phase.
func (m *Model) viewHeader() string {
	doc := ""
	if m.doc != nil && m.doc.Path != "" {
		doc = m.doc.Path
	}
	dash, _, _ := m.glyphs()
	return styleHeader.Render("pulse") + styleMuted.Render(" "+dash+" "+doc+" "+dash+" "+m.phase.String())
}

// rightPane renders the response stream or the fuzz dashboard, whichever
// tab is active.
func (m *Model) rightPane(w int) string {
	if m.showFuzz {
		return m.fuzz.View(w, m.ascii)
	}
	return m.renderResponsePane(w)
}

// viewDualPane is the ≥100-column layout: tree pane beside the right pane,
// the focused one carrying the accent border. Widths split evenly; height is
// whatever the window allows minus chrome.
func (m *Model) viewDualPane() string {
	paneW := (m.width - 5) / 2
	if paneW < 20 {
		paneW = 20
	}
	left := m.tree.View(m.focus == 0, paneW, m.ascii)
	right := m.rightPane(paneW)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// viewSinglePane is the 80–99-column layout: right pane only, the tree
// reduced to the selected row in the status bar.
func (m *Model) viewSinglePane() string {
	return m.rightPane(m.width - 2)
}

// renderResponsePane is the streaming viewport: the bounded tail plus a
// tail-first indicator while running.
func (m *Model) renderResponsePane(w int) string {
	st := paneStyle(m.focus == 1 || m.width < 100, m.ascii)
	_, dot, _ := m.glyphs()
	title := "response"
	if m.phase == phaseRunning {
		title += " " + dot + " streaming"
	}
	if m.stream.Stats().Paused {
		title += " (paused)"
	}
	content := m.stream.Visible()
	return st.Width(w).Render(title + "\n" + content)
}

// viewTiming is the telemetry strip: status and phases on one row. The
// snapshot is lock-guarded at the source, so reading it here races nothing
// even mid-stream.
func (m *Model) viewTiming() string {
	dash, _, _ := m.glyphs()
	text := "timing " + dash
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
	st := m.stream.Stats()
	status := fmt.Sprintf("run: %s %s  %s  lines %d",
		m.method, m.trunc(m.url, 48), m.phase.String(), st.TotalLines)
	if m.showFuzz {
		status += styleAccent.Render("  [fuzz]")
	}
	if m.search.Count() > 0 {
		status += fmt.Sprintf("  /%s (%d)", m.search.Query(), m.search.Count())
	}
	if m.res != nil {
		status += fmt.Sprintf("  %d %s", m.res.Status, formatBytes(m.res.Bytes))
	}
	if st.DroppedLines > 0 || st.DroppedBytes > 0 {
		status += styleWarn.Render(fmt.Sprintf("  dropped %d lines / %s",
			st.DroppedLines, formatBytes(st.DroppedBytes)))
	}
	if st.Paused {
		status += styleAccent.Render("  paused")
	}
	if st.Pending > 0 {
		status += styleMuted.Render(fmt.Sprintf("  backlog %d", st.Pending))
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

// truncFor shortens to n columns with the given ellipsis. Width is counted
// in bytes: truncation points sit inside ASCII spans in practice (URLs,
// labels), and a byte-exact cut never grows the layout it feeds.
func truncFor(s string, n int, ell string) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= len(ell) {
		return ell[:n]
	}
	return s[:n-len(ell)] + ell
}

func truncate(s string, n int) string { return truncFor(s, n, "…") }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
