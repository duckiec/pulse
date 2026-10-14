package ui

import (
	"fmt"
	"strings"
	"testing"
)

// TestStreamViewFiftyMegabyteFloodStaysBounded is the fast form of the 50 MiB
// gate: 50 MiB of 64-byte lines driven straight into a StreamView (no
// program, no socket, no frames), asserting exact arrival accounting with a
// bounded tail and retention under the 2 MiB ceiling. The full program gate
// (TestGFrameFiftyMegabyteStreamStaysBounded) proves the wiring; this one
// proves the module, in milliseconds, and it is what M89 bites.
//
// Mutation M89 disables the 1 MiB tail eviction in appendLine: retention
// grows with the flood and the ceiling assertion goes red immediately.
func TestStreamViewFiftyMegabyteFloodStaysBounded(t *testing.T) {
	s := NewStreamView(80, 24)
	const lineSize = 64
	const total = (50 << 20) / lineSize // 819200 lines = 50 MiB exactly

	// Feed 32 KiB chunks (512 lines) with a Flush after every chunk: the
	// backlog never overflows, so ALL 50 MiB flows through the tail and the
	// 1 MiB eviction — not the backlog — is what binds. A backlog-pressured
	// flood would let a tail-eviction mutant (M89) hide behind backlog
	// drops; this shape gives it nowhere to hide. 1600 viewport syncs keep
	// the gate at unit-test speed (~1s).
	var chunk []byte
	next := 0
	flushChunk := func() {
		s.Append(chunk)
		s.Flush()
		chunk = chunk[:0]
	}
	for i := 0; i < total; i++ {
		chunk = append(chunk, []byte(fmt.Sprintf("evt-%06d-", i%1000000))...)
		for len(chunk)%lineSize != lineSize-1 {
			chunk = append(chunk, 'x')
		}
		chunk = append(chunk, '\n')
		next++
		if next == 512 {
			flushChunk()
			next = 0
		}
	}
	if len(chunk) > 0 {
		flushChunk()
	}
	s.Finish()

	st := s.Stats()
	t.Logf("50 MiB direct: bytes=%d lines=%d droppedLines=%d droppedBytes=%d retained=%d",
		st.TotalBytes, st.TotalLines, st.DroppedLines, st.DroppedBytes, st.RetainedBytes)
	if st.TotalBytes != 50<<20 {
		t.Errorf("bytes = %d, want %d (arrival accounting lost bytes)", st.TotalBytes, 50<<20)
	}
	if st.TotalLines != total {
		t.Errorf("lines = %d, want %d", st.TotalLines, total)
	}
	if st.DroppedLines == 0 {
		t.Error("nothing dropped on a 50 MiB flood against a 1 MiB tail; the bound did not bind")
	}
	if st.RetainedBytes > 2<<20 {
		t.Errorf("retained = %d bytes, exceeds the 2 MiB invariant", st.RetainedBytes)
	}
	// The tail holds the END of the stream: newest lines survive eviction.
	lines := s.Lines()
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "evt-") {
		t.Error("tail does not end with the newest events")
	}
}

// TestStreamViewPauseFreezesTheFrameNotTheStream proves SetPaused semantics:
// while paused the viewport does not advance, but arrivals are still
// retained (bounded), so resume shows the stream rather than a gap.
func TestStreamViewPauseFreezesTheFrameNotTheStream(t *testing.T) {
	s := NewStreamView(80, 24)
	s.Append([]byte("one\ntwo\n"))
	s.Flush()
	before := s.Visible()

	s.SetPaused(true)
	s.Append([]byte("three\nfour\n"))
	s.Flush()
	if got := s.Visible(); got != before {
		t.Error("paused viewport advanced while paused")
	}
	if st := s.Stats(); !st.Paused || st.TotalLines != 4 {
		t.Errorf("pause must freeze the frame, not the accounting: %+v", st)
	}
	s.SetPaused(false)
	if got := s.Visible(); got == before {
		t.Error("resume did not resynchronize to the live stream")
	}
	if st := s.Stats(); st.Paused {
		t.Error("still paused after resume")
	}
}

// TestStreamViewSearchScansTheTail proves bounded in-buffer search: hits in
// line order with excerpts, capped at max, empty query matching nothing.
func TestStreamViewSearchScansTheTail(t *testing.T) {
	s := NewStreamView(80, 24)
	s.Append([]byte("alpha one\nbeta two\nalpha three\n"))
	s.Flush()

	hits := s.Search("alpha", 10)
	if len(hits) != 2 || hits[0].Line != 0 || hits[1].Line != 2 {
		t.Errorf("search hits = %+v, want lines 0 and 2", hits)
	}
	if !strings.Contains(hits[0].Excerpt, "alpha one") {
		t.Errorf("hit carries no excerpt: %+v", hits[0])
	}
	if capped := s.Search("alpha", 1); len(capped) != 1 {
		t.Errorf("max=1 returned %d hits", len(capped))
	}
	if s.Search("", 10) != nil {
		t.Error("empty query must match nothing")
	}
}
