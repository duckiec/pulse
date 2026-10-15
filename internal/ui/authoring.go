package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
)

// This file is Phase 2.2's authoring layer: the composer dispatch, the
// workspace saver seams, the import/vault/new-file flows, the starter
// generator, the error doctor's wiring, and the yank register.
//
// Seams, not imports: the model never touches disk, the keychain, or the
// network. Persistence and sealing arrive as injected functions that the
// composition root (cmd/pulse over internal/app) provides; each runs
// inside a tea.Cmd — a runtime-owned background goroutine — and reports
// back as a message (AGENTS.md §3.2). A nil seam fails closed with a
// toast, never with a panic or a silent drop.

// Messages for the background round-trips.

// saveDoneMsg reports a composer save (UpsertRequestInFile).
type saveDoneMsg struct {
	rel string
	err error
}

// fileLoadedMsg carries freshly read file bytes (create-then-read for new
// files and the starter, plain read for switching): the UI thread parses
// and swaps, which is CPU over a small buffer, never I/O.
type fileLoadedMsg struct {
	rel     string
	content []byte
	err     error
}

// sealedMsg reports a vault seal. It carries the name only — a message
// holding a secret value would sit in the update queue past its use.
type sealedMsg struct {
	name string
	err  error
}

// SetWorkspace binds the workspace root and the default save target
// (requests.http for the scratchpad, the opened file otherwise).
func (m *Model) SetWorkspace(root, saveTarget string) {
	m.wsRoot = root
	m.saveTarget = saveTarget
}

// SetSavers installs the background persistence and sealing seams. Any
// nil seam leaves its action bound but failing closed with a toast, so a
// test can bind exactly the seam under test.
func (m *Model) SetSavers(
	saveReq func(root, rel, name string, block []byte) tea.Msg,
	saveFile func(root, rel string, content []byte) tea.Msg,
	loadFile func(root, rel string) tea.Msg,
	seal func(name, value string) tea.Msg,
) {
	m.saveReqFn = saveReq
	m.saveFileFn = saveFile
	m.loadFileFn = loadFile
	m.sealFn = seal
}

// The Adapt constructors lift plain error-returning composition-root
// functions into the tea.Msg seams SetSavers takes. They exist so cmd
// wiring passes app methods directly (application.UpsertUIRequest) while
// the model only ever sees messages — and so gates bind fakes without
// importing internal/app.

// RequestSaver adapts a request-block upsert into a save-done message.
func RequestSaver(fn func(root, rel, name string, block []byte) error) func(root, rel, name string, block []byte) tea.Msg {
	return func(root, rel, name string, block []byte) tea.Msg {
		if err := fn(root, rel, name, block); err != nil {
			return saveDoneMsg{rel: rel, err: err}
		}
		return saveDoneMsg{rel: rel}
	}
}

// FileSaver adapts a create-then-read into a file-loaded message: the
// UI thread parses and swaps from the carried bytes, never from disk.
func FileSaver(fn func(root, rel string, content []byte) ([]byte, error)) func(root, rel string, content []byte) tea.Msg {
	return func(root, rel string, content []byte) tea.Msg {
		raw, err := fn(root, rel, content)
		return fileLoadedMsg{rel: rel, content: raw, err: err}
	}
}

// FileLoader adapts a confined read into a file-loaded message.
func FileLoader(fn func(root, rel string) ([]byte, error)) func(root, rel string) tea.Msg {
	return func(root, rel string) tea.Msg {
		raw, err := fn(root, rel)
		return fileLoadedMsg{rel: rel, content: raw, err: err}
	}
}

// SecretSealer adapts a seal into a sealed message carrying the name
// only — never the value.
func SecretSealer(fn func(name, value string) error) func(name, value string) tea.Msg {
	return func(name, value string) tea.Msg {
		if err := fn(name, value); err != nil {
			return sealedMsg{name: name, err: err}
		}
		return sealedMsg{name: name}
	}
}

// Scratch reports blank-state boot: an in-memory Untitled Request with no
// file behind it yet.
func (m *Model) Scratch() bool { return m.scratch }

// Touched reports whether the scratchpad has been interacted with (run,
// edit, save, import): the welcome pane yields to the workbench then.
func (m *Model) Touched() bool { return m.touched }

// Doctor exposes the error doctor's remediation for gates.
func (m *Model) Doctor() string { return m.doctor }

// openComposer seeds the composer from the current request: name,
// method, URL, enabled headers, body, and the preserved var/assert/signer
// prefix. Disabled `#` headers and free comments inside the edited span
// have no AST representation and do not survive a save — documented in
// TUI_GUIDE.md ("Composer limits"), same boundary FormatCollection draws.
func (m *Model) openComposer() {
	if m.doc == nil || m.req == nil {
		m.toast = "nothing to compose (no request)"
		return
	}
	var headers []ast.PlainHeader
	for _, h := range m.req.Headers {
		name := m.doc.Str(h.NameStart, h.NameEnd)
		value := ""
		if h.ValStart >= 0 && h.ValEnd > h.ValStart {
			value = m.doc.Str(h.ValStart, h.ValEnd)
		}
		headers = append(headers, ast.PlainHeader{Name: name, Value: value})
	}
	var preserve strings.Builder
	for _, v := range m.doc.VarsOf(m.req) {
		preserve.WriteString("@" + m.doc.Str(v.NameStart, v.NameEnd) + " = " + m.doc.Str(v.ValStart, v.ValEnd) + "\n")
	}
	for _, a := range m.doc.AssertsOf(m.req) {
		preserve.WriteString("# @assert " + m.doc.Str(a.Start, a.End) + "\n")
	}
	if s := m.doc.WasmSign(m.req); s != "" {
		preserve.WriteString("@wasm-sign " + s + "\n")
	}
	m.editor.OpenFull(
		m.doc.RequestName(m.req),
		m.req.Method.String(),
		m.doc.Str(m.req.URLStart, m.req.URLEnd),
		headers,
		string(m.doc.BodyText(m.req.Body)),
		preserve.String(),
		2,
	)
	m.touched = true
}

// updateComposer dispatches keys while the composer is open. Letter keys
// reach text fields as text — row ops (a/e/d) and format (f) live only in
// headers list-nav focus, where no caret exists to steal from.
func (m *Model) updateComposer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	e := &m.editor
	s := tea.Key(msg).String()

	// The header line editor owns every key but its exits.
	if e.LineEditing() {
		switch s {
		case "esc":
			e.CancelHeaderEdit()
			return m, nil
		case "enter", "tab":
			if err := e.CommitHeaderLine(); err != nil {
				m.toast = firstLine(err.Error())
				return m, nil
			}
			if s == "tab" {
				e.Cycle()
			}
			return m, nil
		}
		e.Update(msg)
		return m, nil
	}

	switch s {
	case "esc":
		e.Close()
		return m, nil
	case "enter":
		return m, m.submitEditor()
	case "tab":
		e.Cycle()
		return m, nil
	case "ctrl+s":
		return m, m.saveComposerCmd()
	case "ctrl+f":
		if err := e.FormatBody(); err != nil {
			m.toast = firstLine(err.Error())
		} else {
			m.toast = "body formatted"
		}
		return m, nil
	case " ":
		switch {
		case e.Focus() == 1:
			e.CycleMethod()
		case e.HeaderNav():
			e.HeaderToggle()
		default:
			e.Update(msg)
		}
		return m, nil
	case "f":
		// In the body the letter types; everywhere else it formats.
		if e.BodyFocused() {
			e.Update(msg)
		} else if err := e.FormatBody(); err != nil {
			m.toast = firstLine(err.Error())
		} else {
			m.toast = "body formatted"
		}
		return m, nil
	case "w":
		if e.HeaderNav() {
			return m, m.saveComposerCmd()
		}
		e.Update(msg)
		return m, nil
	}

	if e.HeaderNav() {
		switch s {
		case "j", "down":
			e.HeaderMove(1)
			return m, nil
		case "k", "up":
			e.HeaderMove(-1)
			return m, nil
		case "a":
			e.HeaderAdd()
			return m, nil
		case "e":
			e.HeaderEdit()
			return m, nil
		case "d":
			e.HeaderDelete()
			return m, nil
		}
	}
	e.Update(msg)
	return m, nil
}

// saveComposerCmd persists the composer block through the injected saver
// in the background. Validation (empty name, unknown method) fails
// synchronously with a toast: no goroutine is spent on a block that
// cannot be written.
func (m *Model) saveComposerCmd() tea.Cmd {
	if m.saveReqFn == nil {
		m.toast = "save unbound (no workspace seam)"
		return nil
	}
	block, err := m.editor.BuildBlock()
	if err != nil {
		m.toast = firstLine(err.Error())
		return nil
	}
	name := strings.TrimSpace(m.editor.name.Value())
	rel, root := m.saveTarget, m.wsRoot
	save := m.saveReqFn
	return func() tea.Msg {
		if err := asError(save(root, rel, name, block)); err != nil {
			return saveDoneMsg{rel: rel, err: err}
		}
		return saveDoneMsg{rel: rel}
	}
}

// submitImport parses the pasted curl into the composer. ImportCurl is CPU
// over a small string, so it runs inline; secrets arrive as placeholders
// and are counted, never shown.
func (m *Model) submitImport() {
	text := m.imprt.Text()
	p, secrets, err := ast.ImportCurl(text)
	if err != nil {
		m.toast = "import rejected: " + firstLine(err.Error())
		return
	}
	m.imprt.Close()
	var preserve strings.Builder
	for _, v := range p.Vars {
		preserve.WriteString("@" + v.Name + " = " + v.Value + "\n")
	}
	for _, a := range p.Asserts {
		preserve.WriteString("# @assert " + a + "\n")
	}
	if p.WasmSign != "" {
		preserve.WriteString("@wasm-sign " + p.WasmSign + "\n")
	}
	name := p.Name
	if name == "" {
		name = "imported"
	}
	m.editor.OpenFull(name, p.Method, p.URL, p.Headers, p.Body, preserve.String(), 2)
	m.touched = true
	if len(secrets) == 0 {
		m.toast = "imported (no secrets found)"
	} else {
		names := make([]string, 0, len(secrets))
		for _, s := range secrets {
			names = append(names, "{{"+s.Placeholder+"}}")
		}
		m.toast = fmt.Sprintf("imported (%d secrets as %s — [v] to seal)", len(secrets), strings.Join(names, ", "))
	}
}

// sealVaultCmd seals the typed pair through the injected sealer in the
// background. The value never enters a message, a toast, or the view.
func (m *Model) sealVaultCmd() tea.Cmd {
	name, value := m.vault.Credentials()
	if m.sealFn == nil {
		m.toast = "vault unbound (no sealing seam)"
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" || value == "" {
		m.toast = "vault needs both a name and a value"
		return nil
	}
	m.vault.Close()
	seal := m.sealFn
	return func() tea.Msg {
		if err := asError(seal(name, value)); err != nil {
			return sealedMsg{name: name, err: err}
		}
		return sealedMsg{name: name}
	}
}

// createFileCmd creates the named collection file (one untitled request)
// and loads it back for editing, all through background seams.
func (m *Model) createFileCmd() tea.Cmd {
	if m.saveFileFn == nil {
		m.toast = "file creation unbound (no workspace seam)"
		return nil
	}
	rel := m.newfile.Filename()
	m.newfile.Close()
	root, save := m.wsRoot, m.saveFileFn
	content := NewFileTemplate()
	return func() tea.Msg { return save(root, rel, content) }
}

// starterCmd generates the starter collection into the current save
// target. An existing file is never overwritten: the saver fails with
// ErrFileExists and the toast says so.
func (m *Model) starterCmd() tea.Cmd {
	if m.saveFileFn == nil {
		m.toast = "starter unbound (no workspace seam)"
		return nil
	}
	root, rel, save := m.wsRoot, m.saveTarget, m.saveFileFn
	content := StarterTemplate()
	return func() tea.Msg { return save(root, rel, content) }
}

// yankCurrent renders the current request as a curl command and offers it
// to the clipboard. The model holds unresolved {{templates}}, so no
// secret value can pass through a yank; the toast reports bytes, never
// content.
func (m *Model) yankCurrent() {
	text := m.editor.CurlCommand()
	if !m.editor.Active() {
		text = curlFromDoc(m.doc, m.req)
	}
	if text == "" {
		m.toast = "nothing to yank (no request)"
		return
	}
	if err := clipboard.WriteAll(text); err != nil {
		m.toast = fmt.Sprintf("yank ready (%d bytes; clipboard unavailable: %s)", len(text), firstLine(err.Error()))
		return
	}
	m.toast = fmt.Sprintf("yanked curl (%d bytes)", len(text))
}

// curlFromDoc renders the selected document request as curl for yank when
// the composer is closed. Enabled headers and the raw body template cross
// over; disabled rows stay out of the flight.
func curlFromDoc(doc *ast.Document, req *ast.Request) string {
	if doc == nil || req == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("curl -X " + req.Method.String() + " '" + doc.Str(req.URLStart, req.URLEnd) + "'")
	for _, h := range req.Headers {
		name := doc.Str(h.NameStart, h.NameEnd)
		value := ""
		if h.ValStart >= 0 && h.ValEnd > h.ValStart {
			value = doc.Str(h.ValStart, h.ValEnd)
		}
		fmt.Fprintf(&b, " -H '%s: %s'", name, value)
	}
	if body := doc.BodyText(req.Body); len(body) > 0 {
		fmt.Fprintf(&b, " --data '%s'", body)
	}
	return b.String()
}

// swapDocument parses freshly loaded file bytes and makes them current:
// release the owned doc, rebuild the runner through the factory (a fixed
// runner fails the swap closed — new text with an old runner would slice
// the wrong arena), retarget saves, and reseed the tree.
func (m *Model) swapDocument(rel string, content []byte) {
	arena := ast.AcquireArena()
	arena.Reset(content)
	newDoc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		m.toast = "loaded file does not parse: " + firstLine(err.Error())
		return
	}
	if m.runnerFactory == nil {
		newDoc.Release()
		m.toast = "file held: runner is fixed (no factory)"
		return
	}
	if m.ownedDoc != nil {
		m.ownedDoc.Release()
	}
	m.ownedDoc = newDoc
	m.doc = newDoc
	if abs, aerr := filepath.Abs(filepath.Join(m.wsRoot, rel)); aerr == nil {
		m.doc.Path = abs
	}
	m.runner = m.runnerFactory(newDoc)
	m.saveTarget = rel
	m.snapshotTree()
	if !m.selectRequest(0) {
		m.toast = "file holds no requests"
		return
	}
	m.touched = true
	m.toast = "editing " + rel
}

// applyFailedRun records a terminal failure and, when the failure is a
// missing secret, injects the doctor's remediation: the variable named,
// the fix keyed. Non-secret failures keep their first line, unadorned.
func (m *Model) applyFailedRun(err error) {
	m.err = err
	m.doctor = doctorFor(err)
}

// asError unwraps a seam's tea.Msg when it already is an error: seams
// return purpose-built messages, but a fake in a gate may return the
// error itself, and robustness here is one type switch, not a contract.
func asError(msg tea.Msg) error {
	if msg == nil {
		return nil
	}
	if err, ok := msg.(error); ok {
		return err
	}
	return nil
}
