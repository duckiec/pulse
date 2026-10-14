package ui

// Synctest bubbles for the UI frame budget (ROADMAP P3.3.6, AGENTS.md §2.2).
//
// The 16ms tick and the StreamView pause/resume are CONCURRENCY properties:
// wall-clock tests of them either sleep real milliseconds (slow, flaky under
// load) or assert nothing. These bubbles run the real Update loop against
// testing/synctest's synthetic clock — zero wall time, exact timing — and
// the wall-time fence at the end proves no real sleep leaked in.

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"pulse/internal/ast"
)

// bubbleDoc parses a minimal one-request document for frame tests through
// the same helper as the rest of the suite, so the bubble exercises the
// real document shape, not a stub.
func bubbleDoc(t *testing.T) *ast.Document {
	t.Helper()
	return parseDoc(t, "GET http://127.0.0.1:9/none HTTP/1.1\n\n###\n")
}

// TestBubbleFrameTickCoalescesBursts proves the frame budget: twenty chunks
// arriving 1ms apart (virtual) must NOT repaint twenty times. Nothing moves
// to the visible tail until the 16ms tick fires, and when it does exactly one
// frame carries the whole burst.
func TestBubbleFrameTickCoalescesBursts(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		m := New(context.Background(), nil, bubbleDoc(t), false)
		for i := 0; i < 20; i++ {
			updated, _ := m.Update(chunkMsg{data: []byte(fmt.Sprintf("line-%d\n", i))})
			m = updated.(*Model)
			time.Sleep(time.Millisecond)
		}
		if got := len(m.stream.Lines()); got != 0 {
			t.Errorf("tail filled with %d lines before any tick: frames are not coalesced", got)
		}
		if p := m.stream.Stats().Pending; p != 20 {
			t.Errorf("pending = %d, want 20 (burst must wait for the tick)", p)
		}
		before := m.Frames()
		updated, _ := m.Update(tickMsg(time.Now()))
		m = updated.(*Model)
		if got := len(m.stream.Lines()); got != 20 {
			t.Errorf("one tick flushed %d lines, want 20", got)
		}
		if m.Frames() != before+1 {
			t.Errorf("20 chunks cost %d frames, want 1", m.Frames()-before)
		}
	})
	if wall := time.Since(wallStart); wall > 5*time.Second {
		t.Errorf("bubble consumed %v wall time: a real sleep leaked into the frame path", wall)
	}
}

// TestBubblePauseHoldsFrameAcrossVirtualTime proves pause/resume: with the
// view paused, chunks keep appending (Stats move) but the visible frame is
// frozen across 25ms of virtual producer time; resuming resyncs to the tip.
func TestBubblePauseHoldsFrameAcrossVirtualTime(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		feed := make(chan []byte)
		go func() {
			defer close(feed)
			for i := 0; i < 10; i++ {
				feed <- []byte(fmt.Sprintf("evt-%d\n", i))
				time.Sleep(5 * time.Millisecond)
			}
		}()
		m := New(context.Background(), nil, bubbleDoc(t), false)
		for i := 0; i < 4; i++ {
			updated, _ := m.Update(chunkMsg{data: <-feed})
			m = updated.(*Model)
		}
		updated, _ := m.Update(tickMsg(time.Now()))
		m = updated.(*Model)
		frozen := m.stream.Visible()

		m.stream.SetPaused(true)
		for d := range feed {
			updated, _ := m.Update(chunkMsg{data: d})
			m = updated.(*Model)
		}
		updated, _ = m.Update(tickMsg(time.Now()))
		m = updated.(*Model)
		if m.stream.Visible() != frozen {
			t.Error("paused frame advanced while 25ms of virtual chunks arrived")
		}
		if st := m.stream.Stats(); st.TotalLines != 10 {
			t.Errorf("paused stream holds %d lines, want 10 (append must continue under pause)", st.TotalLines)
		}
		m.stream.SetPaused(false)
		if m.stream.Visible() == frozen {
			t.Error("resume did not resync the frame to the tip")
		}
	})
	if wall := time.Since(wallStart); wall > 5*time.Second {
		t.Errorf("bubble consumed %v wall time: a real sleep leaked into the pause path", wall)
	}
}
