package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"pulse/internal/engine"
)

// This file composes the workbench. The layout is a fixed spatial tree, not a
// stack of independent strings:
//
//	╭─ pulse ─────────────────────────── requests.http · RUNNING ─╮
//	│ GET  http://localhost:8080/health                   @untitled │
//	╰───────────────────────────────────────────────────────────────╯
//	╭─ [1] Requests (2) ─╮╭─ Request ────────────────────────────────╮
//	│ ▸ GET /health      ││ GET  http://localhost:8080/health        │
//	│   POST /notes      │╰──────────────────────────────────────────╯
//	│                    │╭─ Response ──────────────────────────────╮
//	│                    ││ 200 OK  HTTP/1.1  1.5ms  2.0KiB          │
//	╰────────────────────╯╰──────────────────────────────────────────╯
//	 NORMAL  ^S send  ^X cancel  tab focus  / find  ? help   lines 3
//
// Every region is drawn by framed (box.go), which returns exactly the rows
// and columns it was given. Nothing here concatenates a bordered block into
// a differently-sized one, so no terminal size can produce a clipped border
// or a pane that spills past the screen.

// responseHeaderRows is the fixed telemetry block above the response viewport:
// the status badge row, the transport-phase row, and one contextual row
// (remediation, the boot hint, or blank). Fixed rather than content-driven so
// a new response never reflows the viewport height mid-stream.
const responseHeaderRows = 3

// View renders the whole screen as a pure function of the model. Law: no I/O,
// no time calls, no map iteration — the same model renders the same bytes on
// every frame, which is what makes the p99 gate meaningful.
func (m *Model) View() string {
	if m.width < 80 || m.height < 24 {
		return m.viewRefusal()
	}
	g := computeGeometry(m.width, m.height)
	rows := make([]string, 0, m.height)
	rows = append(rows, splitRows(m.viewOmnibox(g), omniboxRows)...)
	rows = append(rows, splitRows(m.bodyView(g), g.bodyH)...)
	rows = append(rows, padTo(m.viewStatusBar(g), g.w))
	return strings.Join(rows, "\n")
}

// glyphs returns the render glyph set: Unicode default, ASCII on demand
// (DESIGN_SYSTEM.md §4.2 — never state by color alone).
func (m *Model) glyphs() (dash, dot, ell string) {
	if m.ascii {
		return "-", "*", "..."
	}
	return "—", "●", "…"
}

// viewRefusal is the sub-80x24 screen (DESIGN_SYSTEM.md §3.2): a refusal to
// render half-broken UI, stated in words rather than drawn in fragments.
func (m *Model) viewRefusal() string {
	dash, _, _ := m.glyphs()
	return styleErr.Render(fmt.Sprintf(
		"pulse: terminal too narrow (%dx%d; need 80x24) "+dash+" widen the window or pipe to `pulse test`",
		m.width, m.height))
}

// viewOmnibox is the top bar: the brand and the workspace in the border, the
// live target on the content row. It is the one strip that never hides — not
// behind a modal (the modal owns the body) and not behind the welcome state
// (there is no welcome state any more).
func (m *Model) viewOmnibox(g geometry) string {
	path := "untitled"
	if m.scratch {
		path = "scratchpad"
	} else if m.doc != nil && m.doc.Path != "" {
		path = m.doc.Path
	}
	right := path
	if m.phase != phaseIdle {
		right += m.glyph(" · ") + m.phase.String()
	}
	f := framed{w: g.w, h: omniboxRows, title: "pulse", right: right, ascii: m.ascii, titleBright: true}
	return f.render([]string{m.omniboxRow(g.w - 2)})
}

// omniboxRow is the live target: method badge, interpolated URL, @name. The
// URL truncates first (it is the longest and the most reconstructable from
// the request pane), the badge never does.
func (m *Model) omniboxRow(inner int) string {
	method := m.method
	if method == "" {
		method = "???"
	}
	pill := methodPill(method)
	left := " " + pill + "  "
	avail := inner - lipgloss.Width(left) - 1
	url := m.url
	name := ""
	if m.name != "" {
		name = m.name
	}
	if name != "" {
		// Reserve the name chip first: it is short, and losing it would make
		// two requests to the same host indistinguishable in the omnibox.
		chip := lipgloss.Width(" @" + name)
		if chip+8 > avail {
			name = ""
		} else {
			avail -= chip
		}
	}
	url = truncDisplay(url, max(1, avail), ellipsis(m.ascii))
	row := left + styleHeader.Render(url)
	if name != "" {
		row += " " + styleMuted.Render("@"+name)
	}
	return row
}

// bodyView draws the body region: a modal when one is open, the two-column
// workbench otherwise. The body is always exactly g.bodyH rows, so opening a
// modal never shifts the status bar.
func (m *Model) bodyView(g geometry) string {
	var title, body string
	switch {
	case m.newfile.Active():
		title, body = "New collection", m.newfile.View(g.w-2, m.ascii)
	case m.vault.Active():
		title, body = "Seal secret", m.vault.View(g.w-2, m.ascii)
	case m.imprt.Active():
		title, body = "Import cURL", m.imprt.View(g.w-2, m.ascii)
	case m.editor.Active():
		title, body = "Compose", m.editor.View(g.w-2, m.ascii)
	case m.palette.Active():
		title, body = "Palette", m.palette.View(g.w-2, m.ascii)
	case m.showHelp:
		return framed{
			w: g.w, h: g.bodyH, title: "Keymap", right: "? closes",
			active: true, ascii: m.ascii,
		}.render(m.helpRows(g.w-2, g.bodyH-2))
	default:
		return m.workbench(g)
	}
	return framed{
		w: g.w, h: g.bodyH, title: title, right: "esc closes",
		active: true, ascii: m.ascii,
	}.render(strings.Split(body, "\n"))
}

// workbench is the two-column body: the request browser on the left, the
// request/response stack on the right.
func (m *Model) workbench(g geometry) string {
	left := framed{
		w: g.sidebarW, h: g.bodyH,
		title:  m.sidebarTitle(g.sidebarW),
		active: m.focus == 0, ascii: m.ascii,
	}.render(m.tree.Rows(g.sidebarW-2, g.bodyH-2, m.focus == 0, m.ascii))
	return joinColumns(left, m.mainColumn(g))
}

// sidebarTitle owes the inlaid hotkey only when there is room for it. A
// truncated "[1] Requests…" is worse than "Requests": the hotkey is a hint,
// the name is the information.
func (m *Model) sidebarTitle(w int) string {
	if w >= sidebarMin {
		return fmt.Sprintf("[1] Requests (%d)", m.tree.Len())
	}
	return fmt.Sprintf("Requests (%d)", m.tree.Len())
}

// mainColumn is the right-hand stack: request on top, response below, or the
// fuzz dashboard occupying the whole column when its tab is active. The split
// height comes from geometry, so it is identical on every frame of a run.
func (m *Model) mainColumn(g geometry) string {
	if m.showFuzz {
		passed, failed, skipped, _ := m.fuzz.Counts()
		right := fmt.Sprintf("pass %d · fail %d · skip %d", passed, failed, skipped)
		return framed{
			w: g.mainW, h: g.bodyH, title: "Fuzz", right: right,
			active: m.focus == 1, ascii: m.ascii,
		}.render(strings.Split(m.fuzz.View(g.mainW-2, m.ascii), "\n"))
	}
	req := framed{
		w: g.mainW, h: g.reqH, title: "Request",
		ascii: m.ascii,
	}.render(m.requestRows(g.reqH - 2))

	rows, right := m.responseRows(g)
	res := framed{
		w: g.mainW, h: g.resH, title: "Response", right: right,
		active: m.focus == 1, ascii: m.ascii,
	}.render(rows)
	return req + "\n" + res
}

// requestRows is the request pane: the flight line, then the directives that
// change what the flight does (@assert first — a failing assertion is the
// reason a run is red), then headers, then file variables, then a body
// preview. It is deliberately ordered by "what would I need to know if I only
// had four rows?".
func (m *Model) requestRows(h int) []string {
	if m.doc == nil || m.req == nil {
		return []string{styleMuted.Render("(no request selected)")}
	}
	lines := []string{m.methodPill() + " " + styleHeader.Render(m.url) +
		styleMuted.Render(" HTTP/1.1")}

	for _, a := range m.doc.AssertsOf(m.req) {
		lines = append(lines, styleOK.Render(m.glyph("✓ "))+styleMuted.Render("@assert ")+
			m.doc.Str(a.Start, a.End))
	}
	if sign := m.doc.WasmSign(m.req); sign != "" {
		lines = append(lines, styleAccent.Render(m.glyph("⚿ "))+styleMuted.Render("@wasm-sign ")+sign)
	}
	for _, hd := range m.req.Headers {
		name := m.doc.Str(hd.NameStart, hd.NameEnd)
		val := m.doc.Str(hd.ValStart, hd.ValEnd)
		lines = append(lines, styleAccent.Render(name)+styleMuted.Render(":")+" "+val)
	}
	for _, v := range m.doc.VarsOf(m.req) {
		lines = append(lines, styleMuted.Render("@")+
			styleAccent.Render(m.doc.Str(v.NameStart, v.NameEnd))+
			styleMuted.Render(" = "+m.doc.Str(v.ValStart, v.ValEnd)))
	}
	for _, v := range m.doc.Vars {
		lines = append(lines, styleMuted.Render("@")+
			styleAccent.Render(m.doc.Str(v.NameStart, v.NameEnd))+
			styleMuted.Render(" = "+m.doc.Str(v.ValStart, v.ValEnd)))
	}
	if body := strings.TrimRight(string(m.doc.BodyText(m.req.Body)), "\n"); body != "" {
		lines = append(lines, "")
		for _, l := range strings.Split(body, "\n") {
			lines = append(lines, styleMuted.Render(m.glyph("│ "))+l)
		}
	}
	return fitWithOverflow(lines, h, ellipsis(m.ascii))
}

// responseRows is the response pane: the telemetry block, then the streaming
// viewport. The viewport is tail-first — when the captured frame is taller
// than the pane (a resize without a re-flush), the newest lines survive.
func (m *Model) responseRows(g geometry) ([]string, string) {
	vh := g.resH - 2 - responseHeaderRows
	if vh < 1 {
		vh = 1
	}
	rows := m.telemetryRows(g.mainW - 2)
	vp := strings.Split(m.stream.Visible(), "\n")
	if len(vp) > vh {
		vp = vp[len(vp)-vh:]
	}
	vp = highlightBody(vp)
	rows = append(rows, fitRows(vp, vh)...)

	right := ""
	switch {
	case m.stream.Stats().Paused:
		right = "paused"
	case m.phase == phaseRunning:
		right = "live"
	}
	return rows, right
}

// telemetryRows is the rich header above the body: status badge, protocol,
// latency and size on one line, transport phases on the next, then one
// contextual row. The snapshot is lock-guarded at the source, so reading it
// here races nothing even mid-stream.
func (m *Model) telemetryRows(inner int) []string {
	var snap engine.TelemetrySnapshot
	if m.report != nil {
		snap = m.report.Snapshot()
	}
	rows := make([]string, 0, responseHeaderRows)
	switch {
	case m.phase == phaseFailed:
		rows = append(rows, styleErr.Render(m.glyph("✗ "))+styleErr.Render(firstLine(errText(m.err))))
		rows = append(rows, styleMuted.Render(m.glyph(phasesLine(snap))))
	case m.res != nil:
		rows = append(rows, strings.Split(m.glyph(RenderTelemetry(snap, m.res)), "\n")...)
	case m.phase == phaseRunning:
		st := m.stream.Stats()
		rows = append(rows, styleAccent.Render(m.glyph("● "))+styleHeader.Render("streaming")+
			styleMuted.Render(fmt.Sprintf("  %s received  %d lines", formatBytes(st.TotalBytes), st.TotalLines)))
		rows = append(rows, styleMuted.Render(m.glyph(phasesLine(snap))))
	default:
		rows = append(rows, styleMuted.Render("no response yet")+
			styleMuted.Render(m.glyph("  ·  ^S sends the selected request")))
	}

	switch {
	case m.doctor != "":
		rows = append(rows, styleWarn.Render(m.doctor))
	case m.scratch && !m.touched:
		// 55 cells: the widest 80-column response pane still shows every
		// verb, and ^S send is already on a keycap in the status bar.
		rows = append(rows, styleMuted.Render(m.glyph(
			"scratchpad  ·  e edit   i curl   g starter   n new file")))
	default:
		rows = append(rows, styleBorderIdle.Render(strings.Repeat(m.glyph("—"), max(1, inner))))
	}
	for len(rows) < responseHeaderRows {
		rows = append(rows, "")
	}
	return rows[:responseHeaderRows]
}

// viewStatusBar is the unified footer: one line, the mode in a solid chip,
// then only the keycaps that apply to that mode, then the run accounting. The
// keycaps come from the central keymap (keys.go), so the footer cannot
// advertise a key the dispatcher does not honour.
func (m *Model) viewStatusBar(g geometry) string {
	mode := m.mode()
	left := m.modeChip(m.glyph(mode))
	right := m.statusRight(g.w)
	avail := g.w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	for _, tok := range m.keys.ModeKeys(mode) {
		cap := " " + tok + " "
		w := lipgloss.Width(cap)
		if w > avail {
			break
		}
		left += " " + styleKeyCap.Render(cap)
		avail -= w + 1
	}
	gap := g.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// modeChip renders the current input mode. The mode is the first thing the
// eye needs on a screen where the same letter types in one state and acts in
// another, so it is a solid fill, not coloured text.
func (m *Model) modeChip(mode string) string {
	st := styleModeChip
	if mode != "NORMAL" {
		st = styleModeActive
	}
	return st.Render(" " + mode + " ")
}

// mode reports the input mode, in the same precedence order updateKey
// dispatches in. Anything else here would make the footer lie exactly when
// the user needs it most.
func (m *Model) mode() string {
	switch {
	case m.newfile.Active():
		return "NEW FILE"
	case m.vault.Active():
		return "VAULT"
	case m.imprt.Active():
		return "IMPORT"
	case m.editor.Active():
		return "EDIT"
	case m.palette.Active():
		return "PALETTE"
	case m.search.Active():
		return "SEARCH"
	case m.showHelp:
		return "HELP"
	case m.showFuzz:
		return "FUZZ"
	default:
		return "NORMAL"
	}
}

// statusRight is the accounting half of the footer: totals, drops, pending
// backlog, search count, toast, error. Drops are shown, never hidden: a
// stream the UI could not keep up with is a fact the operator needs, not an
// embarrassment to trim.
func (m *Model) statusRight(w int) string {
	st := m.stream.Stats()
	var parts []string
	if st.TotalLines > 0 {
		parts = append(parts, fmt.Sprintf("lines %d", st.TotalLines), formatBytes(st.TotalBytes))
	}
	if m.search.Count() > 0 {
		parts = append(parts, fmt.Sprintf("/%s (%d)", m.search.Query(), m.search.Count()))
	}
	if st.DroppedLines > 0 || st.DroppedBytes > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d lines / %s", st.DroppedLines, formatBytes(st.DroppedBytes)))
	}
	if st.Pending > 0 {
		parts = append(parts, fmt.Sprintf("backlog %d", st.Pending))
	}
	out := styleMuted.Render(strings.Join(parts, "  "))
	if m.err != nil {
		out = styleErr.Render(firstLine(m.err.Error()))
		if len(parts) > 0 {
			out = styleMuted.Render(strings.Join(parts, "  ")) + "  " + out
		}
	}
	if m.toast != "" {
		if out != "" {
			out += "  "
		}
		out += styleAccent.Render(m.toast)
	}
	// Never let the accounting eat the keycaps: half the width is the most
	// this half may claim. clipANSI pads, and the padding would be counted
	// against the keycaps, so trim it back off.
	return strings.TrimRight(clipANSI(out, max(0, w/2)), " ")
}

// helpRows renders every binding as an adaptive grid: one column when the
// terminal is tall enough, more when it is not. The overlay replaces the body
// rather than appending below it, which is what keeps the frame exact.
func (m *Model) helpRows(w, h int) []string {
	all := m.keys.All()
	if h < 1 {
		h = 1
	}
	cols := (len(all) + h - 1) / h
	if cols < 1 {
		cols = 1
	}
	if maxCols := w / 28; cols > maxCols && maxCols > 0 {
		cols = maxCols
	}
	colW := w / cols
	rows := make([]string, 0, h)
	for r := 0; r < h; r++ {
		var line strings.Builder
		for c := 0; c < cols; c++ {
			idx := c*h + r
			if idx >= len(all) {
				break
			}
			b := all[idx]
			line.WriteString(padTo(styleAccent.Render(strings.Join(b.Keys, "/"))+
				" "+styleMuted.Render(b.Desc), colW-1) + " ")
		}
		rows = append(rows, strings.TrimRight(line.String(), " "))
	}
	return rows
}

// joinColumns zips two already-exact blocks side by side.
func joinColumns(left, right string) string {
	l, r := strings.Split(left, "\n"), strings.Split(right, "\n")
	var b strings.Builder
	for i := 0; i < max(len(l), len(r)); i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i < len(l) {
			b.WriteString(l[i])
		}
		if i < len(r) {
			b.WriteString(r[i])
		}
	}
	return b.String()
}

// fitWithOverflow caps content at h rows, spending the last row on how much
// was withheld. Silent truncation in a request pane is how a user ships a
// request with a header they never saw.
func fitWithOverflow(lines []string, h int, ell string) []string {
	if h <= 0 {
		return nil
	}
	if len(lines) <= h {
		return lines
	}
	hidden := len(lines) - (h - 1)
	out := append([]string(nil), lines[:h-1]...)
	return append(out, styleMuted.Render(fmt.Sprintf("%s +%d more", ell, hidden)))
}

// glyph folds a chrome glyph to its ASCII equivalent when the model is in
// ASCII mode. Stream *data* is never folded — a body has to reach the screen
// byte-identical — but every decoration the workbench draws itself is.
func (m *Model) glyph(s string) string {
	if !m.ascii {
		return s
	}
	return asciiGlyphs.Replace(s)
}

var asciiGlyphs = strings.NewReplacer(
	"—", "-", "…", "...", "●", "*", "✓", "+", "✗", "x",
	"⚿", "@", "▸", ">", "▾", "-", "▏", "|", "·", "-",
)

// methodPill is the request's method as a solid badge.
func (m *Model) methodPill() string {
	method := m.method
	if method == "" {
		method = "???"
	}
	return methodPill(method)
}

// methodPill renders a method badge: a solid fill with contrasting text. The
// word is always inside the fill, so a monochrome terminal loses the colour
// and keeps the meaning.
func methodPill(method string) string { return styleMethod(method).Render(" " + method + " ") }

// errText keeps errText nil-safe for the failure row.
func errText(err error) string {
	if err == nil {
		return "run failed"
	}
	return err.Error()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
