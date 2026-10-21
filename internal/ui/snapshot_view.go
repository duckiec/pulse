package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/engine"
)

// This file is the TUI side of `# @assert` and snapshot assertions. The model
// never reads the snapshot file itself: the composition root injects
// engine.CheckSnapshot, and the check runs in a tea.Cmd (AGENTS.md §3.2).
//
// The response pane shows the same verdict `pulse test` reports — a pass, a
// path-qualified structural diff, or the malformed-file error — so an
// interactive run and a CI run cannot disagree about what a snapshot means.

// assertOutcome is one evaluated `# @assert` expression.
type assertOutcome struct {
	Source string
	OK     bool
	Detail string
}

// snapshotState is the last snapshot verdict for the request that ran. It
// remembers HTTPFile and Name so accepting a failure targets that request even
// if the editor has since swapped the document.
type snapshotState struct {
	HTTPFile string
	Name     string
	Path     string
	Action   engine.SnapshotAction
	OK       bool
	Diff     string
	Err      error
}

// snapshotCheckedMsg carries a completed snapshot check back to the UI thread.
// It never carries a response body: the body is already in the model, and a
// message holding one would sit in the update queue past its use.
type snapshotCheckedMsg struct {
	res    engine.SnapshotResult
	err    error
	update bool
}

// SetSnapshotChecker installs the background snapshot seam. Production binds
// engine.CheckSnapshot; a nil seam fails the action closed with a toast.
func (m *Model) SetSnapshotChecker(fn func(engine.SnapshotOptions) (engine.SnapshotResult, error)) {
	m.snapCheck = fn
}

// appendBody keeps the bounded assertion copy of the response. It stops at
// engine.MaxCollectedBytes: a snapshot of a prefix is a baseline that every
// later run also truncates, so the check reports the truncation instead.
func (m *Model) appendBody(data []byte) {
	if !m.collectBody || len(data) == 0 || m.bodyTruncated {
		return
	}
	if m.bodyBuf == nil {
		m.bodyBuf = make([]byte, 0, engine.MaxCollectedBytes)
	}
	room := engine.MaxCollectedBytes - len(m.bodyBuf)
	if room <= 0 {
		m.bodyTruncated = true
		return
	}
	if len(data) > room {
		m.bodyBuf = append(m.bodyBuf, data[:room]...)
		m.bodyTruncated = true
		return
	}
	m.bodyBuf = append(m.bodyBuf, data...)
}

// captureRunContext snapshots the request identity and assertion text at
// dispatch. Completion handlers must not re-read the document: the composer
// can replace it while the stream is in flight.
func (m *Model) captureRunContext() {
	m.runPath, m.runName, m.runHasSnapshot = "", "", false
	m.runAsserts = m.runAsserts[:0]
	if m.doc == nil || m.req == nil {
		return
	}
	m.runPath = m.doc.Path
	m.runName = strings.TrimSpace(m.doc.RequestName(m.req))
	m.runHasSnapshot = m.hasSnapshotAssert()
	for _, a := range m.doc.AssertsOf(m.req) {
		m.runAsserts = append(m.runAsserts, strings.TrimSpace(m.doc.Str(a.Start, a.End)))
	}
}

// hasSnapshotAssert reports whether the selected request asks for a snapshot.
func (m *Model) hasSnapshotAssert() bool {
	if m.doc == nil || m.req == nil {
		return false
	}
	for _, a := range m.doc.AssertsOf(m.req) {
		if engine.IsSnapshotAssert(m.doc.Str(a.Start, a.End)) {
			return true
		}
	}
	return false
}

// evaluateAsserts evaluates the ordinary `# @assert` directives against the
// completed run. A malformed expression fails visibly rather than vanishing:
// the runner treats it as exit 2, and the TUI says the same thing in words.
func (m *Model) evaluateAsserts() {
	m.asserts = nil
	if m.doc == nil || m.req == nil {
		return
	}
	body := string(m.bodyBuf)
	for _, src := range m.runAsserts {
		if engine.IsSnapshotAssert(src) {
			continue // rendered from m.snap, which carries the path and diff
		}
		out := assertOutcome{Source: src, OK: true}
		assertion, err := engine.ParseAssert(src)
		switch {
		case err != nil:
			out.OK = false
			out.Detail = err.Error()
		case m.res == nil:
			out.OK = false
			out.Detail = "the request did not complete"
		default:
			if err := assertion.EvalBody(m.res, body); err != nil {
				out.OK = false
				out.Detail = err.Error()
			}
		}
		m.asserts = append(m.asserts, out)
	}
}

// snapshotCheckCmd runs one check in the background. update accepts the live
// body as the new baseline; the normal path (update=false) only writes when no
// snapshot exists yet.
func (m *Model) snapshotCheckCmd(update bool) tea.Cmd {
	if m.doc == nil || m.req == nil {
		m.toast = "no request to snapshot"
		return nil
	}
	path, name := m.runPath, m.runName
	if update && m.snap != nil {
		// `u` accepts the verdict that is on screen, not whatever the
		// editor happens to hold now.
		path, name = m.snap.HTTPFile, m.snap.Name
	}
	if name == "" {
		m.snap = &snapshotState{HTTPFile: path, Err: engine.ErrSnapshotUnnamed}
		m.toast = "snapshot needs # @name"
		return nil
	}
	if path == "" {
		m.snap = &snapshotState{
			HTTPFile: path,
			Name:     name,
			Err:      fmt.Errorf("snapshot needs a saved .http file; save with Ctrl+S first"),
		}
		m.toast = "snapshot needs a saved file"
		return nil
	}
	if m.snapCheck == nil {
		m.snap = &snapshotState{HTTPFile: path, Name: name, Path: engine.SnapshotPath(path), Err: fmt.Errorf("snapshot checker is not bound")}
		m.toast = "snapshot unbound (no checker seam)"
		return nil
	}
	// Copy the body NOW: the command runs on a later goroutine, and a new
	// send may reset bodyBuf before it runs. string() copies the bytes.
	body := string(m.bodyBuf)
	check := m.snapCheck
	opts := engine.SnapshotOptions{
		HTTPFile: path,
		Request:  name,
		Body:     body,
		Update:   update,
	}
	return func() tea.Msg {
		res, err := check(opts)
		return snapshotCheckedMsg{res: res, err: err, update: update}
	}
}

// acceptSnapshotCmd is the `u` action: accept the live body only when the
// current verdict is a failure. Accepting a passing snapshot is a no-op, so a
// stray keystroke cannot rewrite a baseline.
func (m *Model) acceptSnapshotCmd() tea.Cmd {
	switch {
	case m.snap == nil && !m.runHasSnapshot:
		m.toast = "this request has no # @assert snapshot"
		return nil
	case m.snap == nil:
		m.toast = "no snapshot verdict yet — send the request first"
		return nil
	case m.snap.Err != nil:
		// A malformed file cannot be repaired by overwriting it; the
		// operator must look at the file. Say so instead of clobbering.
		m.toast = "snapshot error cannot be accepted: " + firstLine(m.snap.Err.Error())
		return nil
	case m.snap.OK:
		m.toast = "snapshot already matches"
		return nil
	}
	return m.snapshotCheckCmd(true)
}

// applySnapshotResult records the verdict and reports it in the status toast.
func (m *Model) applySnapshotResult(msg snapshotCheckedMsg) {
	if msg.err != nil {
		m.snap = &snapshotState{
			HTTPFile: m.runPath, Name: msg.res.Name, Path: msg.res.Path, Err: msg.err,
		}
		m.toast = "snapshot failed: " + firstLine(msg.err.Error())
		return
	}
	m.snap = &snapshotState{
		HTTPFile: m.runPath,
		Name:     msg.res.Name,
		Path:     msg.res.Path,
		Action:   msg.res.Action,
		OK:       msg.res.OK,
		Diff:     msg.res.Diff,
	}
	base := filepath.Base(msg.res.Path)
	switch msg.res.Action {
	case engine.SnapshotCreated:
		m.toast = "snapshot created " + base
	case engine.SnapshotUpdated:
		m.toast = "snapshot updated " + base
	case engine.SnapshotMismatch:
		m.toast = "snapshot differs " + m.glyph("—") + " u to accept"
	default:
		m.toast = "snapshot matches " + base
	}
	// The diff arrived after runDone sized the viewport, and it is part of
	// the pane's header budget. Resize so it is not rendered over.
	m.sizeViewport()
}

// snapshotLines renders the assertion block above the response body: ordinary
// assertion verdicts, a truncation warning, and the snapshot verdict with its
// readable diff. It is pure over model state.
func (m *Model) snapshotLines(inner int) []string {
	var out []string
	for _, a := range m.asserts {
		if a.OK {
			out = append(out, styleOK.Render(cutANSI("[ok] "+a.Source, inner)))
			continue
		}
		out = append(out, styleErr.Render(cutANSI("[fail] "+a.Source+" "+m.glyph("—")+" "+a.Detail, inner)))
	}
	if m.bodyTruncated {
		out = append(out, styleWarn.Render(cutANSI(fmt.Sprintf(
			"body truncated at %d bytes %s assertions read the retained prefix", engine.MaxCollectedBytes, m.glyph("—")), inner)))
	}
	if m.snap == nil {
		return out
	}
	base := filepath.Base(m.snap.Path)
	switch {
	case m.snap.Err != nil:
		out = append(out, styleErr.Render(cutANSI("[fail] snapshot "+m.snap.Name+": "+m.snap.Err.Error(), inner)))
	case m.snap.OK:
		out = append(out, styleOK.Render(cutANSI("[ok] snapshot "+string(m.snap.Action)+" "+base, inner)))
	default:
		out = append(out, styleErr.Render(cutANSI("[fail] snapshot differs from "+base, inner)))
		out = append(out, styleMuted.Render("[u] accept the new snapshot")+"  "+styleMuted.Render("commit the .snap file with the change"))
		lines := strings.Split(m.snap.Diff, "\n")
		const maxRendered = 20
		more := 0
		if len(lines) > maxRendered {
			more = len(lines) - maxRendered
			lines = lines[:maxRendered]
		}
		for _, ln := range lines {
			style := styleMuted
			switch {
			case strings.HasPrefix(ln, "- "):
				style = styleErr
			case strings.HasPrefix(ln, "+ "):
				style = styleOK
			case strings.HasPrefix(ln, "@@"):
				style = styleAccent
			}
			out = append(out, style.Render(cutANSI(ln, inner)))
		}
		if more > 0 {
			out = append(out, styleMuted.Render(fmt.Sprintf("... %d more diff lines (see pulse test)", more)))
		}
	}
	return out
}

// cutANSI shortens s to n visible columns, passing SGR escapes through
// without counting them. A cut mid-style appends a reset so the padding that
// follows never inherits the severed style.
func cutANSI(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	esc := false
	rest := s
	for len(rest) > 0 {
		if strings.HasPrefix(rest, "\x1b[") {
			end := strings.IndexByte(rest, 'm')
			if end < 0 {
				break
			}
			b.WriteString(rest[:end+1])
			esc = true
			rest = rest[end+1:]
			continue
		}
		size := 1
		if rest[0] >= 0x80 {
			_, size = utf8.DecodeRuneInString(rest)
		}
		if w+1 > n {
			break
		}
		b.WriteString(rest[:size])
		w++
		rest = rest[size:]
	}
	if esc {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
