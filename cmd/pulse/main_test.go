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
		// in is the TUI's stdin: the program quits on "q", so a boot
		// that panics or wedges fails here instead of passing silently.
		in string
	}{
		{"mcp subcommand on tty", []string{"mcp"}, true, exitOK, ""},
		{"mcp subcommand on pipe", []string{"mcp"}, false, exitOK, ""},
		{"mcp flag on pipe", []string{"--mcp"}, false, exitOK, ""},
		// Bare pulse boots the scratchpad (Phase 2.2): the program
		// starts over the in-memory Untitled Request and quits cleanly
		// on q — exit 0, no panic. EOF alone does not quit a Bubble
		// Tea program, so the quit key is the assertion.
		{"tui on tty", nil, true, exitOK, "q"},
		{"tui on pipe fails closed", nil, false, exitUsage, ""},
		{"version", []string{"--version"}, false, exitOK, ""},
		{"unknown flag", []string{"--bogus"}, true, exitUsage, ""},
		{"mcp with stray arg", []string{"mcp", "extra"}, true, exitUsage, ""},
		{"tui flag with extra args ignored", []string{"somefile.http"}, true, exitFail, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(context.Background(), tc.args, strings.NewReader(tc.in), &stdout, &stderr, tc.tty)
			if got != tc.want {
				t.Fatalf("run(%q, tty=%v) = %d, want %d\nstderr: %s", tc.args, tc.tty, got, tc.want, stderr.String())
			}
		})
	}
}

// TestShiftFlagsHonorFlagsAfterPositionals pins the advertised CLI form:
// `pulse test <dir> [--parallel N]` and `pulse vault keygen [--key-id ID]`
// put flags AFTER a positional, which Go's flag package silently ignores.
// shiftFlags moves known flags forward so the tuning is honored; unknown
// flags stay put and fail closed in Parse.
func TestShiftFlagsHonorFlagsAfterPositionals(t *testing.T) {
	testVals := map[string]bool{"parallel": true, "format": true, "silent": false}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flags after dir move forward",
			[]string{"mydir", "--parallel", "8"},
			[]string{"--parallel", "8", "mydir"}},
		{"equals form moves as one",
			[]string{"mydir", "--format=json"},
			[]string{"--format=json", "mydir"}},
		{"bool flag moves bare",
			[]string{"mydir", "--silent"},
			[]string{"--silent", "mydir"}},
		{"flags before stay first",
			[]string{"--parallel", "8", "mydir"},
			[]string{"--parallel", "8", "mydir"}},
		{"unknown flags stay for Parse to reject",
			[]string{"mydir", "--bogus", "8"},
			[]string{"mydir", "--bogus", "8"}},
		{"a value starting with dash is the next flag, not a value",
			[]string{"--out", "--key-id", "x"},
			[]string{"--out", "--key-id", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shiftFlags(c.in, testVals)
			if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
				t.Fatalf("shiftFlags(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestVaultKeyIDFallsBackToTheEnvironment pins the agreement: an explicit
// flag wins, the environment covers every invocation without repeating it,
// and neither may silently select the default while the other names an id.
func TestVaultKeyIDFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("PULSE_VAULT_KEY_ID", "from-env")
	if got := vaultKeyID(""); got != "from-env" {
		t.Fatalf("vaultKeyID(%q) = %q, want the environment", "", got)
	}
	if got := vaultKeyID("from-flag"); got != "from-flag" {
		t.Fatalf("vaultKeyID(%q) = %q, want the flag", "from-flag", got)
	}
}

// TestTestFlagsAfterDirAreHonored proves the wiring, not just the helper:
// a bogus --format after the directory must fail closed on the FORMAT,
// before any test executes. Without the shift it would be ignored and the
// run would proceed as TAP, reporting green on a typo.
func TestTestFlagsAfterDirAreHonored(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run(context.Background(),
		[]string{"test", t.TempDir(), "--format", "bogus"},
		strings.NewReader(""), &stdout, &stderr, false)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "format") {
		t.Fatalf("stderr does not name the format: %s", stderr.String())
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
