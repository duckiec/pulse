package engine_test

// Synctest bubble for stream cancellation and worker draining
// (ROADMAP P3.3.6, AGENTS.md §2.2).
//
// Cancellation is a CONCURRENCY property: the existing wall-clock leak test
// sleeps a real 300ms and tolerates scheduling jitter. This bubble runs the
// real Run against a transport whose body unblocks only on context cancel,
// then cancels from inside the bubble — no network, no wall time. Bubble
// exit additionally proves the reader and dispatcher goroutines drained:
// synctest.Test fails if any bubble goroutine is still alive at the end.

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// unblockOnCancelBody is a response body that yields no bytes and returns
// only when the request context is cancelled — the deterministic shape of a
// hung server, with no socket and no sleep.
type unblockOnCancelBody struct{ ctx context.Context }

func (b unblockOnCancelBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b unblockOnCancelBody) Close() error { return nil }

// cancelRT answers every request with a hung body and no network.
type cancelRT struct{}

func (cancelRT) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    200,
		Header:        make(http.Header),
		Body:          unblockOnCancelBody{ctx: req.Context()},
		ContentLength: -1,
		Request:       req,
	}, nil
}

// bubbleSink records terminal delivery and recycles every chunk, so the
// recycle-every-acquire invariant is checked on the cancel path too.
type bubbleSink struct {
	chunks    int
	recycled  int
	terminals int
	failed    error
	doneRes   *engine.RunResult
	done      chan struct{}
}

func (s *bubbleSink) Chunk(c *engine.Chunk) {
	s.chunks++
	if c != nil {
		c.Recycle()
		s.recycled++
	}
}

func (s *bubbleSink) Done(r *engine.RunResult) {
	s.terminals++
	s.doneRes = r
	close(s.done)
}

func (s *bubbleSink) Failed(err error) {
	s.terminals++
	s.failed = err
	close(s.done)
}

// TestBubbleStreamCancelDrainsWorkers proves the cancel contract: once the
// run has reached the blocked body read, cancelling delivers exactly one
// terminal signal (Failed, never Done), recycles every chunk handed out, and
// drains every worker — bubble exit is the leak assertion.
func TestBubbleStreamCancelDrainsWorkers(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		doc, release := parseDoc(t, "GET http://127.0.0.1:9/slow HTTP/1.1\n\n###\n")
		defer release()
		var req *ast.Request
		for r := range doc.Requests() {
			req = r
			break
		}
		if req == nil {
			t.Fatal("fixture parsed zero requests")
		}
		e := engine.New(doc, engine.WithHTTPClient(&http.Client{Transport: cancelRT{}}))
		sink := &bubbleSink{done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		h := e.Run(ctx, req, sink)
		synctest.Wait()
		cancel()
		<-sink.done
		h.Wait()
		if sink.terminals != 1 {
			t.Errorf("cancel delivered %d terminal signals, want exactly 1", sink.terminals)
		}
		if sink.doneRes != nil {
			t.Error("cancelled run reported Done: cancel must surface as Failed")
		}
		if sink.failed == nil {
			t.Error("cancelled run reported no error: the cancel cause was swallowed")
		}
		if sink.chunks != sink.recycled {
			t.Errorf("%d chunks handed out, %d recycled: cancel leaked buffers", sink.chunks, sink.recycled)
		}
	})
	if wall := time.Since(wallStart); wall > 5*time.Second {
		t.Errorf("bubble consumed %v wall time: a real sleep leaked into the cancel path", wall)
	}
}
