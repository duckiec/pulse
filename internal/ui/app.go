package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
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

	body      []string
	bodyBytes int
	partial   []byte
	pending   []chunkMsg

	totalBytes   int64
	totalLines   int
	droppedLines int
	droppedBytes int64

	vp     viewport.Model
	width  int
	height int
	focus  int // 0 request, 1 response

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
		vp:        viewport.New(80, 24),
	}
	if doc != nil {
		for r := range doc.Requests() {
			m.req = r
			m.method = r.Method.String()
			m.url = doc.Str(r.URLStart, r.URLEnd)
			m.name = doc.Str(r.NameStart, r.NameEnd)
			break
		}
	}
	return m
}

// Bind connects the model to a running program. It must precede Run: the
// sink Sends through this function, and a run started unbound fails closed
// rather than dropping the stream into the void.
func (m *Model) Bind(send func(tea.Msg)) { m.send = send }

// Report exposes the owned telemetry report for release by the owner after
// the program ends. The model Ends it on terminal; the owner Releases it.
func (m *Model) Report() *engine.TelemetryReport { return m.report }

// RetainedBytes is the UI-side memory accounting the G-frame gate asserts:
// tail buffer + undecided line carry + uncoalesced backlog. It never exceeds
// 2 MiB by construction (1 MiB tail + 1 MiB transient), and this method is
// how the test proves it rather than trusting the construction.
func (m *Model) RetainedBytes() int {
	n := m.bodyBytes + len(m.partial)
	for _, p := range m.pending {
		n += len(p.data)
	}
	return n
}

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
		// Arrival accounting happens HERE, not in coalesce: a byte the sink
		// handed over has arrived even if the backlog later drops it, and
		// accounting it at render time undercounts exactly when the bound
		// binds — the one moment the numbers matter.
		m.totalBytes += int64(len(msg.data))
		if len(m.pending) < maxPending {
			m.pending = append(m.pending, msg)
		} else {
			// Backlog overflow drops WITH a count: the status bar shows it,
			// and the G-frame gate asserts the bound rather than the absence.
			// The dropped newlines count as carried lines (totalLines) AND
			// as invisible ones (droppedLines): a newline in the byte stream
			// delimits a line whether or not it was rendered, so the stream
			// total stays exact. Edge: a chunk ending mid-line leaves a
			// trailing fragment uncounted here — at most one line per
			// overflow event, and zero for line-aligned floods.
			m.droppedBytes += int64(len(msg.data))
			n := bytes.Count(msg.data, []byte{'\n'})
			m.totalLines += n
			m.droppedLines += n
		}
		return m, nil
	case tickMsg:
		m.coalesce()
		m.framesRendered++
		return m, tickCmd()
	case runStartedMsg:
		return m, nil
	case runDoneMsg:
		m.coalesce()
		m.res = msg.res
		m.phase = phaseDone
		if m.report != nil {
			m.report.End()
		}
		if m.autoQuit {
			return m, tea.Quit
		}
		return m, nil
	case runFailedMsg:
		m.coalesce()
		m.err = msg.err
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

// coalesce drains the backlog into the bounded tail. It runs at most once
// per frame tick (plus once on terminal), so its cost is amortized across
// the arrival rate: thousands of chunks per second become one bounded pass
// per 16 ms.
func (m *Model) coalesce() {
	if len(m.pending) == 0 {
		return
	}
	for _, p := range m.pending {
		if p.final {
			m.flushPartial()
			continue
		}
		m.appendBytes(p.data)
	}
	m.pending = m.pending[:0]
	m.refreshViewport()
}

// appendBytes splits a chunk on newlines, carrying a split line across
// chunks in partial. Every retained byte is counted; every dropped byte is
// counted louder.
func (m *Model) appendBytes(data []byte) {
	if len(m.partial)+len(data) > maxPartialLine+chunkSlack() {
		// A line that never ends is not a line: flush what we hold and
		// count the rest as dropped rather than growing without bound.
		m.flushPartial()
		m.droppedBytes += int64(len(data))
		m.droppedLines += bytes.Count(data, []byte{'\n'}) + 1
		return
	}
	buf := append(m.partial[:len(m.partial):len(m.partial)], data...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			m.partial = append(m.partial[:0], buf...)
			return
		}
		m.appendLine(string(buf[:i]))
		buf = buf[i+1:]
	}
}

// chunkSlack keeps one chunk of headroom above the partial cap so a line
// that ends exactly at the boundary is kept, not dropped.
func chunkSlack() int { return 32 << 10 }

// appendLine retains one decoded line inside the tail budget, trimming the
// oldest lines first. The viewport shows the tail; the counters remember
// everything.
func (m *Model) appendLine(line string) {
	m.totalLines++
	for (len(m.body) >= maxBodyLines || m.bodyBytes+len(line) > maxBodyBytes) && len(m.body) > 0 {
		m.bodyBytes -= len(m.body[0])
		m.body[0] = ""
		m.body = m.body[1:]
		m.droppedLines++
	}
	m.body = append(m.body, line)
	m.bodyBytes += len(line)
}

// flushPartial retains the undecided carry at end-of-stream.
func (m *Model) flushPartial() {
	if len(m.partial) == 0 {
		return
	}
	m.appendLine(string(m.partial))
	m.partial = m.partial[:0]
}

// refreshViewport rebuilds the visible content from the bounded tail. It runs
// only when coalesce had work, never per chunk and never per View.
func (m *Model) refreshViewport() {
	m.vp.SetContent(strings.Join(m.body, "\n"))
	m.vp.GotoBottom()
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
	m.vp.Width = w
	m.vp.Height = h
}

// updateKey dispatches the central keymap. Uniformity law: focus selects the
// scroll target, never the binding.
func (m *Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys
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
	case k.Match(k.Down, msg):
		m.vp.LineDown(1)
		return m, nil
	case k.Match(k.Up, msg):
		m.vp.LineUp(1)
		return m, nil
	case k.Match(k.Top, msg):
		m.vp.GotoTop()
		return m, nil
	case k.Match(k.Bottom, msg):
		m.vp.GotoBottom()
		return m, nil
	case k.Match(k.HalfDown, msg):
		m.vp.HalfViewDown()
		return m, nil
	case k.Match(k.HalfUp, msg):
		m.vp.HalfViewUp()
		return m, nil
	default:
		return m, nil
	}
}

// Outcome summarises a finished run for owners that need an exit code.
func (m *Model) Outcome() (phase RunPhase, err error) { return m.phase, m.err }

// LineCount and ByteCount expose the stream accounting for gates.
func (m *Model) LineCount() int        { return m.totalLines }
func (m *Model) ByteCount() int64      { return m.totalBytes }
func (m *Model) Dropped() (int, int64) { return m.droppedLines, m.droppedBytes }

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
