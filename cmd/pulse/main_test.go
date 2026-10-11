package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunModeMatrix pins the complete routing table (ARCHITECTURE.md §2.1):
// every flag/TTY combination resolves to the mandated mode and exit code.
func TestRunModeMatrix(t *testing.T) {
	cases := []struct {
		name string
		args []string
		tty  bool
		want int
	}{
		{"mcp subcommand on tty", []string{"mcp"}, true, exitFail},
		{"mcp subcommand on pipe", []string{"mcp"}, false, exitFail},
		{"mcp flag on pipe", []string{"--mcp"}, false, exitFail},
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
			got := run(tc.args, strings.NewReader(""), &stdout, &stderr, tc.tty)
			if got != tc.want {
				t.Fatalf("run(%q, tty=%v) = %d, want %d\nstderr: %s", tc.args, tc.tty, got, tc.want, stderr.String())
			}
		})
	}
}

// TestVersionToStdout pins the version contract.
func TestVersionToStdout(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr, false); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.HasPrefix(stdout.String(), "pulse ") {
		t.Fatalf("stdout = %q, want a version banner", stdout.String())
	}
}

// TestMCPStdoutIsolation is the transport-segregation regression test
// (ROADMAP.md §3.1.1): after the guard is armed, a stray write must never
// reach the captured channel. The MCP server lands in Goal 3; the guard and
// its proof land here, in Move 0.
func TestMCPStdoutIsolation(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = pw // the channel the future JSON-RPC codec would own

	restore := redirectStdout(false)
	if os.Stdout == pw {
		t.Fatal("guard did not swap os.Stdout")
	}
	// A dependency or stray debug print hitting os.Stdout must not land on
	// the captured channel.
	io.WriteString(os.Stdout, "stray write after the guard\n")
	restore()
	if os.Stdout != pw {
		t.Fatal("restore did not put os.Stdout back")
	}
	os.Stdout = orig
	pw.Close()

	pr.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 64)
	if n, _ := pr.Read(buf); n > 0 {
		t.Fatalf("stray write leaked to the reserved channel: %q", buf[:n])
	}
	pr.Close()
}

// TestMCPStdoutAuditTripwire proves the audit-mode guard panics on any write.
// The panic happens on the tripwire goroutine, so the test asserts the swap
// and the tripwire writer plumbing; the panic path is exercised manually with
// PULSE_STDOUT_AUDIT=1.
func TestMCPStdoutAuditTripwire(t *testing.T) {
	orig := os.Stdout
	restore := redirectStdout(true)
	defer func() {
		restore()
		os.Stdout = orig
	}()
	if os.Stdout == orig {
		t.Fatal("audit guard did not swap os.Stdout")
	}
}
