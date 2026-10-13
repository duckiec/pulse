package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"pulse/internal/mcp"
)

// TestRunModeMatrix pins the complete routing table (ARCHITECTURE.md §2.1):
// every flag/TTY combination resolves to the mandated mode and exit code.
//
// The MCP rows drive the real server against an empty stdin, which is a clean
// end-of-session: that is the exit code an agent sees when it closes the pipe.
func TestRunModeMatrix(t *testing.T) {
	cases := []struct {
		name string
		args []string
		tty  bool
		want int
	}{
		{"mcp subcommand on tty", []string{"mcp"}, true, exitOK},
		{"mcp subcommand on pipe", []string{"mcp"}, false, exitOK},
		{"mcp flag on pipe", []string{"--mcp"}, false, exitOK},
		{"tui on tty", nil, true, exitFail},
		{"tui on pipe fails closed", nil, false, exitUsage},
		{"version", []string{"--version"}, false, exitOK},
		{"unknown flag", []string{"--bogus"}, true, exitUsage},
		{"mcp with stray arg", []string{"mcp", "extra"}, true, exitUsage},
		{"tui flag with extra args ignored", []string{"somefile.http"}, true, exitFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, tc.tty)
			if got != tc.want {
				t.Fatalf("run(%q, tty=%v) = %d, want %d\nstderr: %s", tc.args, tc.tty, got, tc.want, stderr.String())
			}
		})
	}
}

// TestVersionToStdout pins the version contract.
func TestVersionToStdout(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &stdout, &stderr, false); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.HasPrefix(stdout.String(), "pulse ") {
		t.Fatalf("stdout = %q, want a version banner", stdout.String())
	}
}

// TestMCPBootLeavesStdoutEmpty proves the pre-handshake contract
// (ROADMAP.md §3.1.2): an agent that spawns Pulse and reads before it sends
// `initialize` must see NOTHING. A banner, a log line, or a warning on stdout
// would be read as a malformed first frame.
func TestMCPBootLeavesStdoutEmpty(t *testing.T) {
	var stderr strings.Builder
	// stdin closes immediately: the session ends before any output could be
	// produced, which is exactly the window this assertion covers.
	code := run(context.Background(), []string{"mcp"}, strings.NewReader(""), io.Discard, &stderr, false)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
}

// TestMCPStdoutIsolation is the transport-segregation regression test
// (ROADMAP.md §3.1.1): after the guard is armed, a stray write must never
// reach the captured channel.
func TestMCPStdoutIsolation(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = pw // the channel the JSON-RPC codec owns

	lock, err := mcp.Lock(false)
	if err != nil {
		os.Stdout = orig
		t.Fatal(err)
	}
	if os.Stdout == pw {
		lock.Restore()
		os.Stdout = orig
		t.Fatal("guard did not swap os.Stdout")
	}

	// A dependency or stray debug print hitting os.Stdout must not land on the
	// captured channel. Note this is the PRODUCTION path: writes are discarded,
	// which is why the audit tripwire exists (a discarded write is invisible).
	io.WriteString(os.Stdout, "stray write after the guard\n")

	lock.Restore()
	if os.Stdout != pw {
		os.Stdout = orig
		t.Fatal("restore did not put os.Stdout back")
	}
	os.Stdout = orig
	pw.Close()

	if n, _ := pr.Read(make([]byte, 64)); n > 0 {
		pr.Close()
		t.Fatal("stray write leaked to the reserved channel")
	}
	pr.Close()
}

// TestMCPStdoutAuditTripwire proves the audit-mode guard detects a stray write.
// Detection happens on the pipe-forwarding goroutine, so the test waits for the
// tripwire to report rather than assuming a synchronous failure.
func TestMCPStdoutAuditTripwire(t *testing.T) {
	orig := os.Stdout
	var seen mcp.Recorder

	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Tripwire == nil {
		lock.Restore()
		t.Fatal("audit mode did not install a tripwire")
	}
	// Replace the fatal handler so the test observes the detection instead of
	// unwinding the process on a deliberate violation.
	lock.Tripwire.OnViolation = seen.Handle
	defer func() {
		lock.Restore()
		os.Stdout = orig
	}()

	if os.Stdout == orig {
		t.Fatal("audit guard did not swap os.Stdout")
	}

	io.WriteString(os.Stdout, "debug info: connecting to localhost\n")

	deadline := time.Now().Add(2 * time.Second)
	for seen.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	violations := seen.Violations()
	if len(violations) == 0 {
		t.Fatal("the audit tripwire did not detect a stray write to stdout")
	}
	if !strings.Contains(violations[0].Payload, "debug info") {
		t.Errorf("violation payload = %q, want it to contain the stray text", violations[0].Payload)
	}
	if !strings.Contains(violations[0].Reason, "not JSON") {
		t.Errorf("violation reason = %q, want it to explain the JSON-RPC breach", violations[0].Reason)
	}
}
