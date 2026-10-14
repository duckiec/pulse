// Command pulse is the dual-mode API workbench: a high-framerate terminal UI
// for humans and a headless MCP server for local AI agents (AGENTS.md §1,
// ARCHITECTURE.md §2).
//
// Three modes are wired: `pulse mcp` serves the MCP server, `pulse test <dir>`
// runs a headless pipeline, and `pulse vault ...` manages encrypted
// configuration. The TUI (ROADMAP.md Goal 1; TASKS.md Phase 3.1) is still a stub
// that fails closed.
//
// This file wires modes and maps outcomes to exit codes. It does not open a
// store, read a keychain or build a domain service: that is internal/app's work,
// and duplicating it here is how the two drift (AGENTS.md §3.1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"pulse/internal/app"
	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/secrets"
)

// Semantic exit codes (AGENTS.md §4.4).
const (
	exitOK    = 0 // success
	exitFail  = 1 // generic failure
	exitUsage = 2 // usage or parse error
	exitSec   = 3 // keychain / secret resolution failure
	exitIntr  = 130
)

const version = "0.0.0-dev"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
		term.IsTerminal(int(os.Stdout.Fd()))))
}

// run executes the mode-routing matrix (ARCHITECTURE.md §2.1). Precedence:
//
//  1. `pulse mcp` subcommand or `--mcp` flag  -> MCP mode (stdout locked)
//  2. `pulse test <dir>`                      -> headless pipeline
//  3. `pulse vault ...`                       -> vault commands
//  4. bare `pulse` with a TTY stdout          -> TUI mode
//  5. bare `pulse` with a pipe stdout         -> fail closed, exit 2
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, stdoutIsTTY bool) int {
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			return runMCP(ctx, args[1:], stderr)
		case "test":
			return runTest(ctx, args[1:], stderr)
		case "vault":
			return runVault(ctx, args[1:], stderr)
		}
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
		fmt.Fprintln(stderr, "       pulse test <dir>   [--parallel N] [--timeout D] [--strict-blocked] [--format tap|json] [--silent]")
		fmt.Fprintln(stderr, "       pulse vault init|encrypt|decrypt <file> [--key-id ID]")
		fmt.Fprintln(stderr, "mcp mode exposes: "+describeTools())
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

// describeTools is used only by `--help`, and exists so the CLI and the MCP
// registry cannot disagree about what Pulse offers: both read the same
// constants.
func describeTools() string {
	return strings.Join([]string{mcp.ToolExecuteRequest, mcp.ToolExecuteFuzz}, ", ")
}

// ---- pulse test ------------------------------------------------------------

// runTest is the `pulse test <dir>` entry.
//
// The exit code is a pure function of the run, mirrored from the engine's own
// mapping so the CLI and the runner cannot disagree (ROADMAP.md §4.1.4).
func runTest(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		parallel = fs.Int("parallel", engine.DefaultParallel,
			"maximum concurrent requests (default derives from a conservative cap, not core count)")
		timeout       = fs.Duration("timeout", engine.DefaultTestTimeout, "per-request timeout")
		strictBlocked = fs.Bool("strict-blocked", false,
			"fail the run when any test is BLOCKED and reported as a skip")
		formatFlag = fs.String("format", "tap", "output format: tap or json")
		silent     = fs.Bool("silent", false,
			"suppress all stdout output; the exit code still reflects the run")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse test <dir> [--format tap|json] [--silent]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// SIGINT cancels this context: the runner halts its queue, the partial
	// report renders, and the run exits 130 (ROADMAP.md §4.1.4). Deliberately
	// the stdlib call with no logic of its own — there is nothing to
	// unit-test here that would not mean sending a real signal, which is
	// flaky on Windows and CI; the cancellation contract is gated injectably
	// in the runner (TestSIGINTFlushesTheCompletedPointsAndExitsOneThirty).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	// An unknown format is a usage error: halt BEFORE any test executes so a
	// typo cannot report a green run.
	format, err := app.ParseTestFormat(*formatFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pulse test: %v\n", err)
		return exitUsage
	}
	root := fs.Arg(0)
	if root == "" {
		fmt.Fprintln(stderr, "pulse test: a directory of .http files is required")
		return exitUsage
	}

	// `pulse test` is a single-shot command and never installs the stdout
	// lock, so its report output (TAP or JSON) is ordinary stdout and there
	// is no JSON-RPC stream to corrupt (ROADMAP.md §4.1.3, G-hyg).
	application, err := app.New(ctx, appConfig())
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return app.SecretExitCode(err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		}
	}()
	for _, d := range application.Degradations {
		fmt.Fprintf(stderr, "pulse: degraded: %s\n", d)
	}

	code, err := application.TestDirEx(ctx, root, os.Stdout, *parallel, *timeout, *strictBlocked, format, *silent)
	if err != nil {
		fmt.Fprintf(stderr, "pulse test: %v\n", err)
		// A parse failure is exit 2, a secret failure exit 3, and anything
		// else the runner's own mapping.
		if app.SecretExitCode(err) != 0 {
			return exitSec
		}
		var vErr *app.VaultError
		if errors.As(err, &vErr) {
			return vErr.ExitCode()
		}
		if code == 0 {
			return exitUsage
		}
		return code
	}
	return code
}

// ---- pulse vault -----------------------------------------------------------

// runVault is the `pulse vault ...` entry.
//
// The key never reaches the repository, a config file or a temp dir at any
// point: it is generated, written to the keychain, read back, and zeroised
// (AGENTS.md §3.4).
func runVault(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse vault", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		keyID  = fs.String("key-id", "", "keychain account suffix; empty selects the default")
		envOut = fs.String("out", "", "plaintext output path for decrypt (default: the .env path)")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse vault init [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault keygen [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault encrypt <path.env> [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault decrypt <path.env.vault> [--key-id ID] [--out PATH]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cmd := fs.Arg(0)
	if cmd == "" {
		fmt.Fprintln(stderr, "pulse vault: init, keygen, encrypt or decrypt is required")
		return exitUsage
	}

	application, err := app.New(ctx, appConfig())
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return app.SecretExitCode(err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		}
	}()

	store := secrets.NewStore()
	resolver := secrets.NewBackend()

	switch cmd {
	case "init":
		account, err := application.VaultInit(*keyID, store)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault init: %v\n", err)
			return vaultExit(err)
		}
		fmt.Fprintf(os.Stderr, "pulse: vault key stored in the keychain as %q\n", account)
		return exitOK

	case "keygen":
		decryptPub, signPub, err := application.VaultKeygen(*keyID, store)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault keygen: %v\n", err)
			return vaultExit(err)
		}
		// Shareable output goes to stdout: `vault keygen` is a single-shot
		// command like `vault init`, never an MCP-serving path, so there is
		// no JSON-RPC stream to corrupt — and the public halves are designed
		// to be copied, which stderr would discourage.
		fmt.Fprintln(os.Stdout, decryptPub)
		fmt.Fprintln(os.Stdout, signPub)
		return exitOK

	case "encrypt", "decrypt":
		path := fs.Arg(1)
		if path == "" {
			fmt.Fprintf(stderr, "pulse vault %s: a .env path is required\n", cmd)
			return exitUsage
		}
		key, err := application.VaultKey(*keyID, resolver)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault %s: %v\n", cmd, err)
			return vaultExit(err)
		}
		defer clear(key)

		if cmd == "encrypt" {
			if err := application.VaultEncrypt(path, key); err != nil {
				fmt.Fprintf(stderr, "pulse vault encrypt: %v\n", err)
				return vaultExit(err)
			}
			_, vault := app.VaultPaths(path)
			fmt.Fprintf(os.Stderr, "pulse: wrote %s and removed the plaintext\n", vault)
			return exitOK
		}

		out := *envOut
		if out == "" {
			out = path
		}
		plain, err := application.VaultDecryptTo(path, out, key)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault decrypt: %v\n", err)
			return vaultExit(err)
		}
		fmt.Fprintf(os.Stderr, "pulse: wrote %s\n", plain)
		return exitOK

	default:
		fmt.Fprintf(stderr, "pulse vault: unknown subcommand %q\n", cmd)
		return exitUsage
	}
}

// vaultExit maps a vault error to an exit code.
func vaultExit(err error) int {
	var vErr *app.VaultError
	if errors.As(err, &vErr) {
		return vErr.ExitCode()
	}
	if app.SecretExitCode(err) != 0 {
		return exitSec
	}
	return exitFail
}

// ---- pulse mcp -------------------------------------------------------------

// runMCP boots the composition root and serves the MCP session.
//
// `mcp.Lock()` is the FIRST statement of the path, before anything the App
// transitively imports is *used*: a package that captured os.Stdout at init has
// already missed the swap by the time we get here, and the audit tripwire is
// what catches that case. Folding the lock into a convenience constructor would
// look safe to call late, which is how a boot log ends up on the JSON-RPC
// channel (AGENTS.md §3.3).
func runMCP(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "pulse: unexpected argument %q after `pulse mcp`\n", args[0])
		return exitUsage
	}

	lock, err := mcp.Lock(os.Getenv("PULSE_STDOUT_AUDIT") == "1")
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return exitFail
	}
	defer lock.Restore()

	application, err := app.New(ctx, appConfig())
	if err != nil {
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

	// Boot diagnostics go to stderr, and are summarised rather than dumped.
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
// Environment rather than flags: every one of these is a deployment detail an
// MCP client sets once, and putting them on the command line would invite an
// agent to vary them per call. An unparsable value is reported rather than
// ignored — a typo in PULSE_HOME silently writing the index somewhere unexpected
// is worse than a refusal to start.
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
			mcp.Logf("PULSE_RAG_CONCURRENCY=%q is not a positive integer; the package default is used", v)
		} else {
			cfg.IndexConcurrency = n
		}
	}
	if v := os.Getenv("PULSE_RUN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			mcp.Logf("PULSE_RUN_TIMEOUT=%q is not a positive duration; the default is used", v)
		} else {
			cfg.RunTimeout = d
		}
	}
	return cfg
}
