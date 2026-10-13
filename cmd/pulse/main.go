// Command pulse is the dual-mode API workbench: a high-framerate terminal UI
// for humans and a headless MCP server for local AI agents (AGENTS.md §1,
// ARCHITECTURE.md §2).
//
// MCP mode is fully wired: the composition root in internal/app builds the
// store, the resolver chain, the redactor, the Local RAG retriever and both
// tools, and this file only chooses a mode and maps an outcome to an exit code.
// The TUI (ROADMAP.md Goal 1; TASKS.md Phase 3.1) is still a stub that fails
// closed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"pulse/internal/app"
	"pulse/internal/mcp"
)

// Semantic exit codes (AGENTS.md §4.4).
const (
	exitOK    = 0 // success
	exitFail  = 1 // generic failure
	exitUsage = 2 // usage or parse error
	exitSec   = 3 // keychain / secret resolution failure
)

const version = "0.0.0-dev"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
		term.IsTerminal(int(os.Stdout.Fd()))))
}

// run executes the mode-routing matrix (ARCHITECTURE.md §2.1). Precedence:
//
//  1. `pulse mcp` subcommand or `--mcp` flag  -> MCP mode (stdout locked)
//  2. bare `pulse` with a TTY stdout         -> TUI mode
//  3. bare `pulse` with a pipe stdout        -> fail closed, exit 2
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, stdoutIsTTY bool) int {
	if len(args) > 0 && args[0] == "mcp" {
		return runMCP(ctx, args[1:], stderr)
	}

	fs := flag.NewFlagSet("pulse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		mcpFlag = fs.Bool("mcp", false, "run in MCP mode (JSON-RPC over stdio)")
		verFlag = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse [flags]")
		fmt.Fprintln(stderr, "       pulse mcp")
		fmt.Fprintf(stderr, "\nmcp mode exposes: %s\n", describeTools())
		fmt.Fprintln(stderr, "\nenvironment: PULSE_HOME, PULSE_ENV_FILE, PULSE_RAG_ROOT,")
		fmt.Fprintln(stderr, "            PULSE_RAG_CONCURRENCY, PULSE_RUN_TIMEOUT,")
		fmt.Fprintln(stderr, "            PULSE_STDOUT_AUDIT")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	switch {
	case *verFlag:
		fmt.Fprintln(stdout, "pulse "+version)
		return exitOK
	case *mcpFlag:
		return runMCP(ctx, fs.Args(), stderr)
	case !stdoutIsTTY:
		// Fail closed: never attach a TUI to a pipe, and never treat a
		// non-TTY as an implicit MCP invitation (ARCHITECTURE.md §2.1).
		fmt.Fprintln(stderr, "pulse: refusing to start the TUI on a non-terminal stdout")
		fmt.Fprintln(stderr, "pulse: use `pulse mcp` for MCP mode (json-rpc over stdio)")
		return exitUsage
	default:
		return runTUI(stdin, stdout, stderr)
	}
}

func runTUI(_ io.Reader, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "pulse: tui mode is not yet implemented (ROADMAP.md Goal 1; TASKS.md Phase 3)")
	return exitFail
}

func runMCP(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "pulse: unexpected argument %q after `pulse mcp`\n", args[0])
		return exitUsage
	}

	// Transport segregation (ROADMAP.md §3.1.1, AGENTS.md §3.3). The guard runs
	// FIRST, before the App or anything it transitively imports is *used*: a
	// package that captured os.Stdout at init has already missed the swap, and
	// the audit tripwire is what catches that case.
	//
	// The ordering is the whole point. Lock is deliberately not tucked inside
	// app.New: a convenience wrapper that looks safe to call late is how a boot
	// log ends up on the JSON-RPC channel.
	lock, err := mcp.Lock(os.Getenv("PULSE_STDOUT_AUDIT") == "1")
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return exitFail
	}
	defer lock.Restore()

	// Everything after this point that writes to stdout is a bug, and in audit
	// mode it is a loud one. The JSON-RPC codec is handed lock.Real and nothing
	// else is given it.
	application, err := app.New(ctx, appConfig())
	if err != nil {
		// A construction failure is reported on stderr and mapped by KIND, not
		// by string matching: a keychain failure is exit 3 everywhere it
		// appears, however many layers deep it was wrapped (AGENTS.md §4.4.5).
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		if code := app.SecretExitCode(err); code != 0 {
			return code
		}
		return exitFail
	}
	defer func() {
		if err := application.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		}
	}()

	// Boot diagnostics are emitted BEFORE the session starts, and they go to
	// stderr. They are also summarised rather than dumped: a DEGRADED line is
	// something the agent's user needs to know, not something that belongs in
	// the protocol stream.
	for _, d := range application.Degradations {
		fmt.Fprintf(stderr, "pulse: degraded: %s\n", d)
	}

	err = application.ServeMCP(ctx, os.Stdin, lock.Real)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.Canceled), errors.Is(err, io.EOF):
		return exitOK
	default:
		fmt.Fprintf(stderr, "pulse: mcp session failed: %v\n", err)
		if code := app.SecretExitCode(err); code != 0 {
			return code
		}
		return exitFail
	}
}

// appConfig reads PULSE_* environment variables into a Config.
//
// Environment rather than flags: every one of these is a deployment detail
// (where the data lives, which vault to use, what to index) that an MCP client
// launching `pulse mcp` sets once, and putting them on the command line would
// invite an agent to vary them per call. An unparsable value is reported rather
// than ignored — a typo in PULSE_HOME silently writing the index somewhere
// unexpected is worse than a refusal to start.
func appConfig() app.Config {
	cfg := app.Config{
		Home:    os.Getenv("PULSE_HOME"),
		EnvFile: os.Getenv("PULSE_ENV_FILE"),
	}
	if v := os.Getenv("PULSE_RAG_ROOT"); v != "" {
		cfg.CorpusRoot = v
	}
	if v := os.Getenv("PULSE_RAG_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			mcp.Logf("PULSE_RAG_CONCURRENCY=%q is not a positive integer; "+
				"the package default is used", v)
		} else {
			cfg.IndexConcurrency = n
		}
	}
	if v := os.Getenv("PULSE_RUN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			mcp.Logf("PULSE_RUN_TIMEOUT=%q is not a positive duration; "+
				"the default is used", v)
		} else {
			cfg.RunTimeout = d
		}
	}
	return cfg
}

// describeTools is used only by `--help`, and exists so the CLI and the MCP
// registry cannot disagree about what Pulse offers: both read the same
// constants.
func describeTools() string {
	return strings.Join([]string{mcp.ToolExecuteRequest, mcp.ToolExecuteFuzz}, ", ")
}
