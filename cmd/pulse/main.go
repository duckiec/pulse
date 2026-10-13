// Command pulse is the dual-mode API workbench: a high-framerate terminal UI
// for humans and a headless MCP server for local AI agents (AGENTS.md §1,
// ARCHITECTURE.md §2).
//
// Move 0 scope: mode routing, exit codes, and the MCP stdout guard only. The
// TUI (ROADMAP.md Goal 3... Phase 3 of TASKS.md) and the MCP server
// (ROADMAP.md Goal 3) are stubs that fail closed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

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
	// FIRST, before the MCP package or anything it imports is used: a package
	// that captured os.Stdout at init has already missed the swap, and the audit
	// tripwire is what catches that case.
	lock, err := mcp.Lock(os.Getenv("PULSE_STDOUT_AUDIT") == "1")
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return exitFail
	}
	defer lock.Restore()

	// Everything after this point that writes to stdout is a bug, and in audit
	// mode it is a loud one. The JSON-RPC codec is handed lock.Real and nothing
	// else is given it.
	code, serveErr := mcp.ServeLocked(ctx, lock.Real, os.Stdin)
	if serveErr != nil {
		fmt.Fprintf(stderr, "pulse: mcp session failed: %v\n", serveErr)
		return code
	}
	return code
}
