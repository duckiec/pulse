package ui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// Frame and memory geometry. These constants are the entire UI-side memory
// argument; changing any of them changes the ceiling the G-frame gate
// asserts (ROADMAP.md §1.2.4, ARCHITECTURE.md §5.2 invariant 4).
const (
	// frameInterval is the 60 FPS governor: at most one coalesced View per
	// tick, no matter how many chunks arrived since the last one.
	frameInterval = 16 * time.Millisecond
	// maxBodyLines and maxBodyBytes bound the viewport tail (ROADMAP.md
	// §1.2.4: 10,000 rows / 1 MiB of decoded text). Overflow is
	// dropped-with-count, visible in the status bar — never silent.
	maxBodyLines = 10000
	maxBodyBytes = 1 << 20 // 1 MiB
	// maxPending bounds the chunk backlog between frames: 32 × 32 KiB =
	// 1 MiB transient. With the 1 MiB tail, total UI retention never exceeds
	// the 2 MiB in-flight ceiling. Overflow drops with a count.
	maxPending = 32
	// maxPartialLine caps a single undecided line carry: a chunk flood with
	// no newline is otherwise an unbounded buffer wearing a line's clothes.
	maxPartialLine = 1 << 20 // 1 MiB
)

// RunPhase is where one request run stands.
type RunPhase int

const (
	phaseIdle RunPhase = iota
	phaseRunning
	phaseDone
	phaseFailed
)

// String renders the phase for the status bar. No color here: styling is
// views.go's work, and a String that paints would paint in tests too.
func (p RunPhase) String() string {
	switch p {
	case phaseRunning:
		return "RUNNING"
	case phaseDone:
		return "DONE"
	case phaseFailed:
		return "FAILED"
	default:
		return "IDLE"
	}
}

// Runner is the execution seam: anything that can Run an AST request into a
// Sink. *engine.Engine and *engine.ProtocolRouter both satisfy it, and the
// model never learns which one it holds (ARCHITECTURE.md invariant 11).
type Runner interface {
	Run(ctx context.Context, req *ast.Request, sink engine.Sink) engine.RunHandle
}

// Messages. Engine callbacks never touch the model: they Send these, and the
// UI thread owns every transition (AGENTS.md §3.2).

// chunkMsg carries one UI-owned copy of a response chunk. The bytes are
// already detached from the engine pool when this is constructed — the pool
// buffer was recycled before Send, so engine memory never waits on a frame.
type chunkMsg struct {
	data  []byte
	final bool
}

// runStartedMsg marks the dispatch of a run.
type runStartedMsg struct{}

// runDoneMsg carries the terminal redacted summary.
type runDoneMsg struct {
	res *engine.RunResult
}

// runFailedMsg carries the terminal error.
type runFailedMsg struct {
	err error
}

// tickMsg is the frame governor. One tick coalesces every pending chunk into
// at most one View; stale ticks do not exist because each tick schedules
// exactly its successor when handled.
type tickMsg time.Time

// uiSink adapts engine callbacks onto tea messages. It runs on the engine's
// dispatch goroutine, serially, never on the UI thread — so its counters are
// plain fields, not atomics, and its only allocation is the UI-owned copy.
//
// Ownership discipline (ARCHITECTURE.md §5.1): the engine hands over each
// chunk exactly once; this sink copies what it keeps and recycles the pool
// buffer synchronously, before Send. A sink that kept the buffer without
// copying would read poison after recycle; a sink that kept it without
// recycling would starve the pool and wedge the socket reader.
type uiSink struct {
	send     func(tea.Msg)
	received int64
	recycled int64
}

func (s *uiSink) Chunk(c *engine.Chunk) {
	s.received++
	if c == nil || c.Final {
		if c != nil {
			c.Recycle()
			s.recycled++
		}
		s.send(chunkMsg{final: true})
		return
	}
	data := append([]byte(nil), c.Bytes()...)
	c.Recycle()
	s.recycled++
	s.send(chunkMsg{data: data})
}

// Done reports a completed run. Exactly one terminal callback per run.
func (s *uiSink) Done(res *engine.RunResult) { s.send(runDoneMsg{res: res}) }

// Failed reports a run that did not complete. Exactly one terminal callback.
func (s *uiSink) Failed(err error) { s.send(runFailedMsg{err: err}) }

// Model is the Pulse TUI application state. Elm rules (DESIGN_SYSTEM.md §1):
// Update owns every transition, View is a pure function of this struct, and
// Init only schedules work. The struct holds no socket, no file, no clock —
// time enters through tickMsg, sizes through WindowSizeMsg, bytes through
// chunkMsg.
type Model struct {
	runner Runner
	doc    *ast.Document
	req    *ast.Request

	baseCtx context.Context
	send    func(tea.Msg)
	cancel  context.CancelFunc
	handle  engine.RunHandle
	sink    *uiSink

	method string
	url    string
	name   string

	phase RunPhase
	res   *engine.RunResult
	err   error

	report *engine.TelemetryReport

	// stream owns the entire streaming memory contract: tail, backlog,
	// accounting, and viewport sync (stream_view.go). The model holds no
	// byte of the stream itself — arrival becomes Append, the frame tick
	// becomes Flush, and every counter below delegates to Stats.
	stream *StreamView

	// Sub-models. Each owns one surface and none owns transport: the tree
	// browses injected rows, search scans the owned StreamView, the
	// palette filters injected labels, the editor composes .http source,
	// the import/vault/new-file modals feed it, and the fuzz dashboard
	// consumes bridged probe events.
	tree    TreeModel
	search  SearchModel
	palette PaletteModel
	editor  EditorModel
	imprt   ImportModel
	vault   VaultModel
	newfile NewFileModel
	fuzz    *FuzzModel

	// showFuzz swaps the right pane to the fuzz dashboard. probePoll
	// chains bridge polls after every consumed probe event.
	showFuzz  bool
	probePoll func() tea.Cmd

	// ascii degrades every glyph in the render path (DESIGN_SYSTEM.md
	// §4.2): the same model renders Unicode or ASCII on demand, which is
	// what makes glyph parity a golden test instead of a hope.
	ascii bool

	// runnerFactory rebuilds the runner when the editor swaps the
	// document: an engine reads through its own doc reference, so a fixed
	// runner would slice the wrong arena. Nil means the runner is fixed
	// and editor submit fails closed instead of dispatching.
	runnerFactory func(*ast.Document) Runner
	// ownedDoc is the editor-built document (nil while the borrowed
	// construction document is current). Only owned docs are released.
	ownedDoc *ast.Document

	width  int
	height int
	focus  int // 0 tree, 1 response

	// Phase 2.2 blank-state and authoring state. scratch marks the
	// in-memory Untitled boot; touched retires the welcome pane on first
	// interaction; wsRoot/saveTarget address workspace writes; doctor
	// holds the missing-secret remediation while it applies.
	scratch    bool
	touched    bool
	wsRoot     string
	saveTarget string
	doctor     string

	// Persistence and sealing seams (authoring.go): background functions
	// the composition root injects; nil fails each action closed.
	saveReqFn  func(root, rel, name string, block []byte) tea.Msg
	saveFileFn func(root, rel string, content []byte) tea.Msg
	loadFileFn func(root, rel string) tea.Msg
	sealFn     func(name, value string) tea.Msg

	toast          string
	framesRendered int64
	autoQuit       bool
	autoStart      bool
	showHelp       bool
	keys           KeyMap
}

// Compile-time proof the model is a Bubble Tea program.
var _ tea.Model = (*Model)(nil)

// New binds a runner, a parsed document and its first request into a model.
// The model snapshots the request's display text at construction: View must
// be pure, and re-slicing the arena per frame would make render cost a
// function of document size rather than viewport size.
func New(baseCtx context.Context, runner Runner, doc *ast.Document, autoStart bool) *Model {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	m := &Model{
		runner:    runner,
		doc:       doc,
		baseCtx:   baseCtx,
		autoStart: autoStart,
		keys:      DefaultKeyMap(),
		stream:    NewStreamView(80, 24),
		fuzz:      NewFuzzModel(80, 24),
		editor:    NewEditorModel(),
		imprt:     NewImportModel(),
		vault:     NewVaultModel(),
		newfile:   NewNewFileModel(),
	}
	if doc != nil {
		for r := range doc.Requests() {
			m.req = r
			m.snapshotReq(r)
			break
		}
	}
	m.snapshotTree()
	return m
}

// snapshotReq copies a request's display text: View must be pure, and
// re-slicing the arena per frame would make render cost a function of
// document size rather than viewport size.
func (m *Model) snapshotReq(r *ast.Request) {
	m.method = r.Method.String()
	m.url = m.doc.Str(r.URLStart, r.URLEnd)
	m.name = m.doc.Str(r.NameStart, r.NameEnd)
}

// snapshotTree rebuilds the browse rows from the current document: one row
// per request, grouped by file, in document order.
func (m *Model) snapshotTree() {
	if m.doc == nil {
		m.tree.SetItems(nil)
		return
	}
	group := m.doc.Path
	if group == "" {
		group = "requests"
	}
	var items []TreeItem
	idx := 0
	for r := range m.doc.Requests() {
		label := r.Method.String() + " " + m.doc.Str(r.URLStart, r.URLEnd)
		if name := m.doc.Str(r.NameStart, r.NameEnd); name != "" {
			label += " @" + name
		}
		items = append(items, TreeItem{Group: group, Label: label, Index: idx})
		idx++
	}
	m.tree.SetItems(items)
}

// selectRequest makes the idx-th document request the run target.
func (m *Model) selectRequest(idx int) bool {
	if m.doc == nil {
		return false
	}
	i := 0
	for r := range m.doc.Requests() {
		if i == idx {
			m.req = r
			m.snapshotReq(r)
			return true
		}
		i++
	}
	return false
}

// SetRunnerFactory installs the editor's dispatch rebuild (see the field
// doc). Production passes App.RouterFor; a nil factory is a fixed runner.
func (m *Model) SetRunnerFactory(f func(*ast.Document) Runner) { m.runnerFactory = f }

// SetASCII switches glyph degradation (Unicode default, ASCII on demand),
// propagated to every owned viewport so no sub-model renders a stray glyph.
func (m *Model) SetASCII(ascii bool) {
	m.ascii = ascii
	m.stream.SetASCII(ascii)
	m.fuzz.SetASCII(ascii)
}

// SetProbePoll chains bridge polls after every probe event.
func (m *Model) SetProbePoll(poll func() tea.Cmd) { m.probePoll = poll }

// BindProgress installs a ProgressBridge: the model polls it, converts each
// verdict into a probeMsg, and routes it to the fuzz dashboard. This is the
// whole FuzzOptions.Progress → UI path, and it stays non-blocking and
// stdout-free by construction (M90 disconnects the bridge itself).
func (m *Model) BindProgress(b *ProgressBridge) {
	m.SetProbePoll(b.PollCmd)
}

// submitEditor parses the editor source, swaps the document, rebuilds the
// runner, and dispatches. Any step failing fails the whole submit closed:
// a half-swapped document (new text, old runner) would slice the wrong
// arena, so the model keeps the old request rather than a broken one.
func (m *Model) submitEditor() tea.Cmd {
	src := m.editor.Source()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	newDoc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		m.editor.Close()
		m.toast = "edit rejected: " + firstLine(err.Error())
		return nil
	}
	if m.runnerFactory == nil {
		newDoc.Release()
		m.editor.Close()
		m.toast = "edit held: runner is fixed (no factory)"
		return nil
	}
	if m.ownedDoc != nil {
		m.ownedDoc.Release()
	}
	m.ownedDoc = newDoc
	m.doc = newDoc
	m.runner = m.runnerFactory(newDoc)
	m.snapshotTree()
	if !m.selectRequest(0) {
		m.editor.Close()
		m.toast = "edit parsed nothing to run"
		return nil
	}
	m.editor.Close()
	return m.startRunCmd()
}

// Bind connects the model to a running program. It must precede Run: the
// sink Sends through this function, and a run started unbound fails closed
// rather than dropping the stream into the void.
func (m *Model) Bind(send func(tea.Msg)) { m.send = send }

// Report exposes the owned telemetry report for release by the owner after
// the program ends. The model Ends it on terminal; the owner Releases it.
func (m *Model) Report() *engine.TelemetryReport { return m.report }

// RetainedBytes is the UI-side memory accounting the G-frame gate asserts.
// It delegates to the StreamView that owns the bytes, so the assertion and
// the invariant cannot drift apart.
func (m *Model) RetainedBytes() int { return m.stream.Stats().RetainedBytes }

// Init schedules the frame governor and, in autoStart mode, the run. Both
// are tea.Cmd values — background goroutines owned by the runtime — so Init
// performs no I/O itself (DESIGN_SYSTEM.md §2.1).
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	if m.autoStart {
		cmds = append(cmds, m.startRunCmd())
	}
	return tea.Batch(cmds...)
}

func tickCmd() tea.Cmd {
	return tea.Tick(frameInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// startRunCmd dispatches the request on a runtime-owned goroutine. The engine
// owns the socket from here; the UI thread only sees the resulting messages.
func (m *Model) startRunCmd() tea.Cmd {
	return func() tea.Msg { return m.startRun() }
}

func (m *Model) startRun() tea.Msg {
	if m.runner == nil || m.req == nil {
		return runFailedMsg{err: errors.New("ui: nothing to run (no runner or no request)")}
	}
	if m.send == nil {
		return runFailedMsg{err: errors.New("ui: program not bound (Bind before Run)")}
	}
	if m.phase == phaseRunning {
		return runStartedMsg{}
	}
	m.report = engine.AcquireTelemetry()
	m.doctor = ""
	m.touched = true
	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	ctx = engine.Attach(ctx, m.report)
	m.sink = &uiSink{send: m.send}
	m.handle = m.runner.Run(ctx, m.req, m.sink)
	m.phase = phaseRunning
	m.toast = ""
	return runStartedMsg{}
}

// cancelRun aborts the in-flight run. Cancellation unwinds through the
// engine's teardown path; the terminal Failed message still arrives and
// still owns the phase transition, so a cancel can never strand the model
// in RUNNING.
func (m *Model) cancelRun() {
	if m.phase != phaseRunning {
		return
	}
	m.handle.Cancel()
	m.toast = "cancelled"
}

// Update owns every state transition. Law: no I/O, no blocking syscalls, no
// time calls — and nothing slower than the frame budget. A 100 ms sleep here
// is not a slowdown but a correctness defect: 10k msgs/s behind a 100 ms
// handler is an unbounded backlog, which is exactly what M79 proves.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.updateKey(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sizeViewport()
		return m, nil
	case chunkMsg:
		// Arrival becomes Append; accounting lives inside the StreamView
		// (at arrival, not at render — see StreamView.Append). A final
		// marker arrives as a nil append: end-of-stream, not bytes.
		if msg.final {
			m.stream.Append(nil)
		} else {
			m.stream.Append(msg.data)
		}
		return m, nil
	case probeMsg:
		// One bridged fuzz verdict per message, chained back onto the
		// bridge poll while one is installed: non-blocking both ways,
		// stdout never in the call graph.
		m.fuzz.UpdateProbe(msg.ev)
		if m.probePoll != nil {
			return m, m.probePoll()
		}
		return m, nil
	case tickMsg:
		m.stream.Flush()
		m.framesRendered++
		return m, tickCmd()
	case runStartedMsg:
		return m, nil
	case runDoneMsg:
		m.stream.Flush()
		m.stream.Finish()
		m.res = msg.res
		m.doctor = ""
		m.phase = phaseDone
		if m.report != nil {
			m.report.End()
		}
		if m.autoQuit {
			return m, tea.Quit
		}
		return m, nil
	case saveDoneMsg:
		if msg.err != nil {
			m.toast = "save failed: " + firstLine(msg.err.Error())
		} else {
			m.touched = true
			m.toast = "saved " + msg.rel
		}
		return m, nil
	case fileLoadedMsg:
		if msg.err != nil {
			m.toast = "file failed: " + firstLine(msg.err.Error())
		} else {
			m.swapDocument(msg.rel, msg.content)
		}
		return m, nil
	case sealedMsg:
		if msg.err != nil {
			m.toast = "seal failed: " + firstLine(msg.err.Error())
		} else {
			m.touched = true
			m.toast = "sealed {{" + msg.name + "}}"
			if doctorVar(m.err) == msg.name {
				m.doctor = ""
				m.toast += " — resend to retry"
			}
		}
		return m, nil
	case runFailedMsg:
		m.stream.Flush()
		m.stream.Finish()
		m.applyFailedRun(msg.err)
		m.phase = phaseFailed
		if m.report != nil {
			m.report.End()
		}
		if m.autoQuit {
			return m, tea.Quit
		}
		return m, nil
	default:
		return m, nil
	}
}

// sizeViewport fits the response pane to the window. Narrow terminals collapse
// the request pane (DESIGN_SYSTEM.md §3.2); a terminal under 80 columns is
// refused by the CLI before the program starts, and renders a refusal here
// if it shrinks mid-run.
func (m *Model) sizeViewport() {
	w, h := m.width-4, m.height-9
	if w < 20 {
		w = 20
	}
	if h < 5 {
		h = 5
	}
	m.stream.Resize(w, h)
	m.fuzz.Resize(w, h)
}

// paletteItems builds the palette rows: every document request plus the
// command verbs. Rebuilt on every open so edits are visible immediately.
func (m *Model) paletteItems() []PaletteItem {
	var out []PaletteItem
	if m.doc != nil {
		idx := 0
		for r := range m.doc.Requests() {
			label := r.Method.String() + " " + m.doc.Str(r.URLStart, r.URLEnd)
			if name := m.doc.Str(r.NameStart, r.NameEnd); name != "" {
				label += " @" + name
			}
			out = append(out, PaletteItem{Label: label, RequestIndex: idx})
			idx++
		}
	}
	for _, c := range []string{"send", "cancel", "pause", "fuzz", "help"} {
		out = append(out, PaletteItem{Label: ":" + c, RequestIndex: -1, Command: c})
	}
	return out
}

// runPaletteCommand executes a :command row.
func (m *Model) runPaletteCommand(cmd string) tea.Cmd {
	m.palette.Deactivate()
	switch cmd {
	case "send":
		return m.startRunCmd()
	case "cancel":
		m.cancelRun()
	case "pause":
		m.togglePause()
	case "fuzz":
		m.showFuzz = !m.showFuzz
	case "help":
		m.showHelp = !m.showHelp
	}
	return nil
}

// togglePause flips the stream viewport hold (Stream tab pause/resume).
func (m *Model) togglePause() {
	paused := !m.stream.Stats().Paused
	m.stream.SetPaused(paused)
	if paused {
		m.toast = "stream paused"
	} else {
		m.toast = ""
	}
}

// jumpToCurrent scrolls the stream to the selected search hit.
func (m *Model) jumpToCurrent() {
	if hit, ok := m.search.Current(); ok {
		m.stream.JumpTo(hit.Line)
	}
}

// updateKey dispatches the central keymap. Modal panes take precedence in
// opacity order — editor, palette, search — then the global bindings.
// Uniformity law: focus selects the scroll target, never the binding.
func (m *Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys

	// Modal precedence (opacity order): new-file, vault, import, composer,
	// then palette, then search. A letter that types text in one focus
	// and acts in another is resolved by focus inside updateComposer.
	if m.newfile.Active() {
		switch msg.String() {
		case "esc":
			m.newfile.Close()
			return m, nil
		case "enter":
			return m, m.createFileCmd()
		}
		m.newfile.Update(msg)
		return m, nil
	}

	if m.vault.Active() {
		switch msg.String() {
		case "esc":
			m.vault.Close()
			return m, nil
		case "tab":
			m.vault.Toggle()
			return m, nil
		case "enter":
			return m, m.sealVaultCmd()
		}
		m.vault.Update(msg)
		return m, nil
	}

	if m.imprt.Active() {
		switch msg.String() {
		case "esc":
			m.imprt.Close()
			return m, nil
		case "enter":
			m.submitImport()
			return m, nil
		}
		m.imprt.Update(msg)
		return m, nil
	}

	if m.editor.Active() {
		return m.updateComposer(msg)
	}

	if m.palette.Active() {
		switch msg.String() {
		case "esc":
			m.palette.Deactivate()
			return m, nil
		case "enter":
			item, ok := m.palette.Chosen()
			if !ok {
				m.palette.Deactivate()
				return m, nil
			}
			if item.RequestIndex >= 0 {
				m.palette.Deactivate()
				if m.selectRequest(item.RequestIndex) {
					m.toast = "selected " + item.Label
				}
				return m, nil
			}
			return m, m.runPaletteCommand(item.Command)
		case "up", "ctrl+p":
			m.palette.Move(-1)
			return m, nil
		case "down", "ctrl+n":
			m.palette.Move(1)
			return m, nil
		case "backspace":
			m.palette.Backspace()
			return m, nil
		}
		if msg.Type == tea.KeyRunes {
			for _, r := range msg.Runes {
				m.palette.Type(r)
			}
		}
		return m, nil
	}

	if m.search.Active() {
		switch msg.String() {
		case "esc":
			m.search.Deactivate()
			return m, nil
		case "enter":
			m.search.Run(m.stream)
			m.search.Deactivate()
			m.jumpToCurrent()
			return m, nil
		case "backspace":
			m.search.Backspace()
			return m, nil
		}
		if msg.Type == tea.KeyRunes {
			for _, r := range msg.Runes {
				m.search.Type(r)
			}
		}
		return m, nil
	}

	// Capital N steps to the previous hit (n moved to new-file in Phase
	// 2.2; next is ctrl+n). The exact form is checked before the map so
	// a shifted rune never falls through to another binding.
	if tea.Key(msg).String() == "N" {
		m.search.Prev()
		m.jumpToCurrent()
		return m, nil
	}
	switch {
	case k.Match(k.Quit, msg):
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case k.Match(k.Send, msg):
		return m, m.startRunCmd()
	case k.Match(k.Cancel, msg):
		m.cancelRun()
		return m, nil
	case k.Match(k.Focus, msg):
		m.focus = (m.focus + 1) % 2
		return m, nil
	case k.Match(k.Help, msg):
		m.showHelp = !m.showHelp
		return m, nil
	case k.Match(k.Pause, msg):
		m.togglePause()
		return m, nil
	case k.Match(k.FuzzTab, msg):
		m.showFuzz = !m.showFuzz
		return m, nil
	case k.Match(k.Search, msg):
		m.search.Activate()
		return m, nil
	case k.Match(k.SearchNext, msg):
		m.search.Next()
		m.jumpToCurrent()
		return m, nil
	case k.Match(k.Palette, msg):
		m.palette.Activate(m.paletteItems())
		return m, nil
	case k.Match(k.Editor, msg):
		m.openComposer()
		return m, nil
	case k.Match(k.Import, msg):
		m.imprt.Open()
		return m, nil
	case k.Match(k.Vault, msg):
		m.vault.Open(doctorVar(m.err))
		return m, nil
	case k.Match(k.Yank, msg):
		m.yankCurrent()
		return m, nil
	case k.Match(k.NewFile, msg):
		m.newfile.Open()
		return m, nil
	case k.Match(k.Starter, msg):
		return m, m.starterCmd()
	case k.Match(k.Select, msg):
		if m.focus == 0 {
			if item, ok := m.tree.Selected(); ok {
				if m.selectRequest(item.Index) {
					m.toast = "selected " + item.Label
				}
			}
		}
		return m, nil
	case k.Match(k.Down, msg):
		if m.focus == 0 {
			m.tree.Move(1)
		} else {
			m.stream.LineDown(1)
		}
		return m, nil
	case k.Match(k.Up, msg):
		if m.focus == 0 {
			m.tree.Move(-1)
		} else {
			m.stream.LineUp(1)
		}
		return m, nil
	case k.Match(k.Top, msg):
		m.stream.GotoTop()
		return m, nil
	case k.Match(k.Bottom, msg):
		m.stream.GotoBottom()
		return m, nil
	case k.Match(k.HalfDown, msg):
		m.stream.HalfViewDown()
		return m, nil
	case k.Match(k.HalfUp, msg):
		m.stream.HalfViewUp()
		return m, nil
	case k.Match(k.FullDown, msg):
		m.stream.PageDown()
		return m, nil
	case k.Match(k.FullUp, msg):
		m.stream.PageUp()
		return m, nil
	default:
		return m, nil
	}
}

// Outcome summarises a finished run for owners that need an exit code.
func (m *Model) Outcome() (phase RunPhase, err error) { return m.phase, m.err }

// Toast exposes the status toast for gates.
func (m *Model) Toast() string { return m.toast }

// LineCount and ByteCount expose the stream accounting for gates.
func (m *Model) LineCount() int   { return m.stream.Stats().TotalLines }
func (m *Model) ByteCount() int64 { return m.stream.Stats().TotalBytes }
func (m *Model) Dropped() (int, int64) {
	st := m.stream.Stats()
	return st.DroppedLines, st.DroppedBytes
}

// Frames counts coalesced frames, for the G-frame gate: frames rendered is
// proof the governor ticked under load, not merely that bytes arrived.
func (m *Model) Frames() int64 { return m.framesRendered }

// SinkStats exposes the engine-side transfer accounting: every chunk the
// engine handed over must have been recycled exactly once. A gap here is a
// pool leak that would wedge the socket reader on a long stream.
func (m *Model) SinkStats() (received, recycled int64) {
	if m.sink == nil {
		return 0, 0
	}
	return m.sink.received, m.sink.recycled
}
