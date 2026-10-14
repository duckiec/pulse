package app

// White-box gates for the readLoop TOFU-observability branch (sync.go):
// a dialed peer whose connection dies before the first frame must be
// LOUD (the TLS 1.3 server alert lands on our first Read, after our Dial
// already succeeded — this Read error is the only observable half of the
// rejection), while a peer that dies mid-stream stays silent (gossip is
// loss-tolerant; the drop counter, not the log, is the signal there).
//
// Both are proven through mcp.Stderr, the same sink production logs to,
// with node ids unique per test so concurrent suites cannot cross-talk.

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"pulse/internal/mcp"
	syncpkg "pulse/internal/sync"
)

// stderrCapture swaps the MCP stderr sink for a locked buffer. Logf writes
// through the package var on every call, so no restart or reboot is needed
// to observe one readLoop's verdict.
type stderrCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *stderrCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *stderrCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureStderr(t *testing.T) *stderrCapture {
	t.Helper()
	cap := &stderrCapture{}
	old := mcp.Stderr
	mcp.Stderr = cap
	t.Cleanup(func() { mcp.Stderr = old })
	return cap
}

// TestReadLoopLogsPreFirstFrameDeath feeds EOF before any frame: the
// dialed peer refused us (TOFU alert, dead listener, RST) and the loop
// must say which peer and why. M94 removes the Logf and goes red here.
func TestReadLoopLogsPreFirstFrameDeath(t *testing.T) {
	out := captureStderr(t)
	node := "peer-x-pre-frame-1"
	server, client := net.Pipe()
	_ = client.Close() // EOF on first Read: zero frames delivered
	s := &SyncSubsystem{conns: map[string]net.Conn{node: server}, inbox: make(chan syncpkg.Inbound, 8)}
	s.wg.Add(1)
	done := make(chan struct{})
	go func() { s.readLoop(context.Background(), server, node); close(done) }()
	<-done
	got := out.String()
	if !strings.Contains(got, "sync: dial "+node) {
		t.Fatalf("pre-first-frame death logged nothing for %q: %q", node, got)
	}
	s.mu.Lock()
	_, still := s.conns[node]
	s.mu.Unlock()
	if still {
		t.Fatalf("dead conn for %q left in the map", node)
	}
}

// TestReadLoopStaysSilentAfterFirstFrame delivers one valid frame, then
// EOF: the peer talked and then vanished mid-stream — loss-tolerant
// gossip absorbs that via the drop counter, and the log stays quiet.
// M94's inverse (logging every death) would spam every flaky peer here.
func TestReadLoopStaysSilentAfterFirstFrame(t *testing.T) {
	out := captureStderr(t)
	node := "peer-x-post-frame-2"
	server, client := net.Pipe()
	s := &SyncSubsystem{conns: map[string]net.Conn{node: server}, inbox: make(chan syncpkg.Inbound, 8)}
	s.wg.Add(1)
	done := make(chan struct{})
	go func() { s.readLoop(context.Background(), server, node); close(done) }()
	raw, err := syncpkg.MarshalFrame(syncpkg.Frame{From: node, Type: syncpkg.MsgDigest, Digest: &syncpkg.Digest{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncpkg.WriteFrame(client, raw); err != nil {
		t.Fatal(err)
	}
	_ = client.Close() // EOF after one frame: mid-stream death
	<-done
	if got := out.String(); strings.Contains(got, "sync: dial "+node) {
		t.Fatalf("mid-stream death was loud for %q: %q", node, got)
	}
	select {
	case in := <-s.inbox:
		if in.From != node {
			t.Fatalf("inbox sender %q, want %q", in.From, node)
		}
	default:
		t.Fatal("the delivered frame never reached the inbox")
	}
}

var _ = sync.Mutex{}
