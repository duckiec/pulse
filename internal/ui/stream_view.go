package ui

import (
	"bytes"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
)

// StreamView owns the streaming memory contract: a bounded line tail, a
// bounded pending backlog, arrival accounting, and viewport synchronization.
//
// The five-method surface (Append/Flush/Finish/Visible/Stats plus SetPaused)
// is the whole contract the response pane and the fuzz dashboard program
// against. Three further methods exist because the contract cannot be
// honoured without them: Resize (the owned viewport needs a size),
// Search (querying the tail without exposing it), and Lines (a bounded
// snapshot for gates). Everything else is unexported.
//
// Memory argument (ROADMAP.md §1.2.4): the tail holds at most maxBodyLines /
// maxBodyBytes, the backlog at most maxPending chunks, the undecided carry
// at most maxPartialLine — so retention never exceeds the 2 MiB in-flight
// ceiling, and RetainedBytes() is how the gates prove it.
type StreamView struct {
	vp viewport.Model

	tail      []string
	tailBytes int
	partial   []byte
	pending   []streamChunk

	totalBytes   int64
	totalLines   int
	droppedLines int
	droppedBytes int64

	paused bool
	// ascii degrades chrome glyphs (excerpt ellipses); stream DATA passes
	// through byte-identical in both modes.
	ascii  bool
	width  int
	height int
}

// streamChunk is one arrived-but-uncoalesced unit. Final marks end-of-stream.
type streamChunk struct {
	data  []byte
	final bool
}

// StreamStats is the accounting snapshot. Totals are exact (arrived bytes
// and lines, whether rendered or dropped); retention is bounded.
type StreamStats struct {
	TotalBytes    int64
	TotalLines    int
	DroppedLines  int
	DroppedBytes  int64
	RetainedBytes int
	Pending       int
	Paused        bool
}

// SearchMatch is one bounded in-buffer hit: the tail-relative line number
// and a short excerpt. Excerpts are capped so a hostile query cannot pull
// the whole tail through the match list.
type SearchMatch struct {
	Line    int
	Excerpt string
}

// NewStreamView returns an empty view sized to w×h.
func NewStreamView(w, h int) *StreamView {
	s := &StreamView{}
	s.Resize(w, h)
	return s
}

// Resize fits the owned viewport. Narrow sizes clamp rather than fail: a
// mid-run shrink must degrade, not panic.
func (s *StreamView) Resize(w, h int) {
	if w < 20 {
		w = 20
	}
	if h < 5 {
		h = 5
	}
	s.width, s.height = w, h
	s.vp.Width = w
	s.vp.Height = h
}

// Append accounts one arrival and queues it for the next Flush. Accounting
// happens HERE, not at render time: a byte handed over has arrived even if
// the backlog later drops it, and accounting at render undercounts exactly
// when the bound binds. Backlog overflow drops WITH a count. A nil chunk
// marks end-of-stream (flushes the undecided carry at Flush time).
func (s *StreamView) Append(data []byte) {
	if data == nil {
		if len(s.pending) < maxPending {
			s.pending = append(s.pending, streamChunk{final: true})
		} else {
			s.flushPartialLocked()
		}
		return
	}
	s.totalBytes += int64(len(data))
	if len(s.pending) < maxPending {
		cp := append([]byte(nil), data...)
		s.pending = append(s.pending, streamChunk{data: cp})
		return
	}
	// Overflow: the dropped newlines count as carried lines AND as invisible
	// ones — a newline delimits a line whether or not it was rendered, so
	// the stream total stays exact.
	s.droppedBytes += int64(len(data))
	n := bytes.Count(data, []byte{'\n'})
	s.totalLines += n
	s.droppedLines += n
}

// Flush drains the backlog into the bounded tail and resynchronizes the
// viewport. It runs at most once per frame tick, so its cost is amortized
// across the arrival rate. While paused the tail still advances (bounded —
// resume must show the stream, not a gap) but the viewport stays frozen.
func (s *StreamView) Flush() {
	if len(s.pending) == 0 {
		return
	}
	for _, p := range s.pending {
		if p.final {
			s.flushPartialLocked()
			continue
		}
		s.appendBytes(p.data)
	}
	s.pending = s.pending[:0]
	if !s.paused {
		s.syncViewport()
	}
}

// Finish ends the stream: the undecided carry becomes its final line and the
// viewport synchronizes even while paused (the run is over; there is nothing
// left to hold still for).
func (s *StreamView) Finish() {
	s.flushPartialLocked()
	was := s.paused
	s.paused = false
	s.syncViewport()
	s.paused = was
}

// Visible renders the viewport: the bounded tail around the current scroll
// position, or the frozen frame while paused mid-stream.
func (s *StreamView) Visible() string {
	return s.vp.View()
}

// chunkSlack keeps one chunk of headroom above the partial cap so a line
// that ends exactly at the boundary is kept, not dropped.
func chunkSlack() int { return 32 << 10 }

// Scrolling forwards. The viewport is owned here, so scroll targets cross
// this boundary — the model never touches viewport state directly.
func (s *StreamView) LineDown(n int) { s.vp.LineDown(n) }
func (s *StreamView) LineUp(n int)   { s.vp.LineUp(n) }
func (s *StreamView) GotoTop()       { s.vp.GotoTop() }
func (s *StreamView) GotoBottom()    { s.vp.GotoBottom() }
func (s *StreamView) HalfViewDown()  { s.vp.HalfViewDown() }
func (s *StreamView) HalfViewUp()    { s.vp.HalfViewUp() }

// PageDown and PageUp move a full viewport height: the Ctrl+f/b complement
// to the half-page Ctrl+d/u pair.
func (s *StreamView) PageDown() { s.vp.LineDown(s.height) }
func (s *StreamView) PageUp()   { s.vp.LineUp(s.height) }

// JumpTo scrolls to an absolute tail-relative line (search-hit navigation).
// From the top, LineDown lands exactly: the viewport content is the tail
// joined in order, so offset equals line number.
func (s *StreamView) JumpTo(line int) {
	s.vp.GotoTop()
	if line > 0 {
		s.vp.LineDown(line)
	}
}

// Stats snapshots the accounting.
func (s *StreamView) Stats() StreamStats {
	retained := s.tailBytes + len(s.partial)
	for _, p := range s.pending {
		retained += len(p.data)
	}
	return StreamStats{
		TotalBytes:    s.totalBytes,
		TotalLines:    s.totalLines,
		DroppedLines:  s.droppedLines,
		DroppedBytes:  s.droppedBytes,
		RetainedBytes: retained,
		Pending:       len(s.pending),
		Paused:        s.paused,
	}
}

// SetPaused freezes (true) or resumes (false) the viewport. Arrival and tail
// accounting are unaffected — pause holds the frame still, it never drops
// the stream. Resume resynchronizes to the live bottom.
func (s *StreamView) SetPaused(paused bool) {
	s.paused = paused
	if !paused {
		s.syncViewport()
	}
}

// SetASCII degrades chrome glyphs to ASCII. Stream data is unaffected.
func (s *StreamView) SetASCII(ascii bool) { s.ascii = ascii }

// Search scans the retained tail for query, returning at most max hits in
// line order. An empty query matches nothing: a search with no text is not
// a search.
func (s *StreamView) Search(query string, max int) []SearchMatch {
	if query == "" || max <= 0 {
		return nil
	}
	ell := "…"
	if s.ascii {
		ell = "..."
	}
	var out []SearchMatch
	for i, line := range s.tail {
		if strings.Contains(line, query) {
			ex := line
			if len(ex) > 120 {
				ex = ex[:119] + ell
			}
			out = append(out, SearchMatch{Line: i, Excerpt: ex})
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

// Lines snapshots the retained tail. The copy is bounded by the tail budget
// (≤1 MiB), and it exists for gates and for panes that must quote the
// stream — per-frame rendering uses Visible, never this.
func (s *StreamView) Lines() []string {
	return append([]string(nil), s.tail...)
}

// appendBytes splits a chunk on newlines, carrying a split line across
// chunks in partial. Every retained byte is counted; every dropped byte is
// counted louder.
func (s *StreamView) appendBytes(data []byte) {
	if len(s.partial)+len(data) > maxPartialLine+chunkSlack() {
		// A line that never ends is not a line: flush what we hold and
		// count the rest as dropped rather than growing without bound.
		s.flushPartialLocked()
		s.droppedBytes += int64(len(data))
		s.droppedLines += bytes.Count(data, []byte{'\n'}) + 1
		return
	}
	buf := append(s.partial[:len(s.partial):len(s.partial)], data...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			s.partial = append(s.partial[:0], buf...)
			return
		}
		s.appendLine(string(buf[:i]))
		buf = buf[i+1:]
	}
}

// appendLine retains one decoded line inside the tail budget, trimming the
// oldest lines first. The viewport shows the tail; the counters remember
// everything.
func (s *StreamView) appendLine(line string) {
	s.totalLines++
	for (len(s.tail) >= maxBodyLines || s.tailBytes+len(line) > maxBodyBytes) && len(s.tail) > 0 {
		s.tailBytes -= len(s.tail[0])
		s.tail[0] = ""
		s.tail = s.tail[1:]
		s.droppedLines++
	}
	s.tail = append(s.tail, line)
	s.tailBytes += len(line)
}

// flushPartialLocked retains the undecided carry at end-of-stream.
func (s *StreamView) flushPartialLocked() {
	if len(s.partial) == 0 {
		return
	}
	s.appendLine(string(s.partial))
	s.partial = s.partial[:0]
}

// syncViewport rebuilds the visible content from the bounded tail. It runs
// only on Flush with work (or Finish/unpause), never per chunk and never
// per View.
func (s *StreamView) syncViewport() {
	s.vp.SetContent(strings.Join(s.tail, "\n"))
	s.vp.GotoBottom()
}
