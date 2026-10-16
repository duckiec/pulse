// Command pulse is the dual-mode API workbench: a high-framerate terminal UI
// for humans and a headless MCP server for local AI agents (AGENTS.md §1,
// ARCHITECTURE.md §2).
//
// Four modes are wired: `pulse mcp` serves the MCP server, `pulse test <dir>`
// runs a headless pipeline, `pulse vault ...` manages encrypted
// configuration, and `pulse <file.http>` starts the fullscreen TUI over the
// same composition root the other modes share.
//
// This file wires modes and maps outcomes to exit codes. It does not open a
// store, read a keychain or build a domain service: that is internal/app's work,
// and duplicating it here is how the two drift (AGENTS.md §3.1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"pulse/internal/app"
	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/secrets"
	"pulse/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

// Semantic exit codes (AGENTS.md §4.4).
const (
	exitOK    = 0 // success
	exitFail  = 1 // generic failure
	exitUsage = 2 // usage or parse error
	exitSec   = 3 // keychain / secret resolution failure
	exitIntr  = 130
)

// version is the release tag stamped by the release workflow
// (scripts/release/build.sh passes -X main.version=<tag>). A dev build
// reports 0.0.0-dev so an unstamped binary never claims a release.
var version = "0.0.0-dev"

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
			return runTest(ctx, args[1:], stdout, stderr)
		case "vault":
			return runVault(ctx, args[1:], stdin, stdout, stderr)
		case "init":
			return runInit(args[1:], stdout, stderr)
		case "doctor":
			return runDoctor(ctx, args[1:], stdin, stdout, stderr)
		case "sync":
			return runSync(ctx, args[1:], stdin, stdout, stderr)
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
		fmt.Fprintln(stderr, "       pulse <file.http>")
		fmt.Fprintln(stderr, "       pulse mcp")
		fmt.Fprintln(stderr, "       pulse test <dir>   [--parallel N] [--timeout D] [--strict-blocked] [--format tap|json] [--silent]")
		fmt.Fprintln(stderr, "       pulse vault init|encrypt|decrypt <file> [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault set <KEY> [VALUE]")
		fmt.Fprintln(stderr, "       pulse vault list")
		fmt.Fprintln(stderr, "       pulse vault unset <KEY>")
		fmt.Fprintln(stderr, "       pulse init [file] [--force]")
		fmt.Fprintln(stderr, "       pulse doctor [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse sync serve <dir>")
		fmt.Fprintln(stderr, "       pulse sync status")
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
		if len(fs.Args()) > 1 {
			fmt.Fprintln(stderr, "pulse: one .http file at a time (usage: pulse <file.http>)")
			return exitUsage
		}
		return runTUI(ctx, stdin, stdout, stderr, fs.Arg(0))
	}
}

// runTUI starts the fullscreen Bubble Tea program over one .http file —
// or, with no file, over the in-memory scratchpad (Phase 2.2: an empty
// workspace boots the welcome state, never an error).
//
// The TUI shares the composition root with every other mode: the same App
// builds the store, chain and redactor, and App.RouterFor hands the UI the
// same protocol router MCP and `pulse test` drive, so an interactive run and
// a headless run cannot disagree about resolution, redaction or timeout.
// Exit mapping: a missing file or an empty document is exit 1 (generic
// failure — the invocation was well-formed; the former is pinned by
// TestRunModeMatrix), a malformed .http file is exit 2 (parse error,
// AGENTS.md §4.4), and a too-small terminal is exit 2 (DESIGN_SYSTEM.md
// §3.2 refuses to render half-broken UI). A clean interactive session exits
// 0; the TUI reports verdicts on screen, not in its exit code.
func runTUI(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, file string) int {
	if file == "" {
		return runTUIScratch(ctx, stdin, stdout, stderr)
	}
	doc, err := ast.ParseFile(file)
	if err != nil {
		var se *ast.SyntaxError
		if errors.As(err, &se) {
			fmt.Fprintf(stderr, "pulse: %v\n", err)
			return exitUsage
		}
		fmt.Fprintf(stderr, "pulse: cannot read %s: %v\n", file, err)
		return exitFail
	}
	defer doc.Release()
	hasRequest := false
	for range doc.Requests() {
		hasRequest = true
		break
	}
	if !hasRequest {
		fmt.Fprintf(stderr, "pulse: %s holds no requests\n", file)
		return exitFail
	}
	// Refuse a half-broken frame before starting: below 80x24 the model
	// renders a refusal line, but entering fullscreen to show it would
	// flicker the terminal for no reason. Untestable outputs (anything that
	// is not an *os.File) skip the probe; production stdout is always one.
	if code := checkTUITermSize(stdout, stderr); code >= 0 {
		return code
	}

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

	if err := application.Boot(ctx); err != nil {
		fmt.Fprintf(stderr, "pulse: boot: %v\n", err)
		return exitFail
	}
	model := ui.New(ctx, application.RouterFor(doc), doc, true)
	wireTUIModel(ctx, application, model, workspaceOf(file), saveRelOf(file))
	return startTeaProgram(stdin, stdout, stderr, model)
}

// runTUIScratch boots the TUI with no file: an in-memory Untitled Request
// over the current workspace, saving to requests.http beside it. An empty
// directory is the blank-state welcome, never a usage error — pinned by
// TestRunModeMatrix (bare pulse quits cleanly) and the PTY empty-dir
// proof (a real terminal paints the welcome and quits on q).
func runTUIScratch(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pulse: workspace: %v\n", err)
		return exitFail
	}
	doc, err := ui.ParseScratch()
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return exitFail
	}
	// No Release: ownership passes to the model, which tracks the scratch
	// document as owned and recycles it on the first document swap.
	if code := checkTUITermSize(stdout, stderr); code >= 0 {
		return code
	}
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
	if err := application.Boot(ctx); err != nil {
		fmt.Fprintf(stderr, "pulse: boot: %v\n", err)
		return exitFail
	}
	model := ui.NewScratchpad(ctx, application.RouterFor(doc), doc, cwd)
	wireTUIModel(ctx, application, model, cwd, "requests.http")
	return startTeaProgram(stdin, stdout, stderr, model)
}

// checkTUITermSize refuses a half-broken frame before fullscreen starts.
// It returns -1 when the program may start, otherwise the exit code the
// caller returns directly. Non-file outputs skip the probe (production
// stdout is always an *os.File).
func checkTUITermSize(stdout, stderr io.Writer) int {
	if f, ok := stdout.(*os.File); ok {
		if w, h, serr := term.GetSize(int(f.Fd())); serr == nil && (w < 80 || h < 24) {
			fmt.Fprintf(stderr, "pulse: terminal is %dx%d; the TUI needs 80x24\n", w, h)
			return exitUsage
		}
	}
	return -1
}

// wireTUIModel binds the runner factory, the workspace address, and the
// persistence/sealing seams: the editor rebuilds the document on submit,
// the composer saves through ast's jailed atomic path, and the vault
// seals through the keychain-or-memory chain — all provided by the App,
// none constructed by the UI (AGENTS.md §3.1).
func wireTUIModel(_ context.Context, application *app.App, model *ui.Model, wsRoot, saveRel string) {
	model.SetRunnerFactory(func(d *ast.Document) ui.Runner { return application.RouterFor(d) })
	model.SetWorkspace(wsRoot, saveRel)
	model.SetSavers(
		ui.RequestSaver(application.UpsertUIRequest),
		ui.FileSaver(func(root, rel string, content []byte) ([]byte, error) {
			if err := application.CreateUIFile(root, rel, content); err != nil {
				return nil, err
			}
			return application.ReadUIFile(root, rel)
		}),
		ui.FileLoader(application.ReadUIFile),
		ui.SecretSealer(application.SealUISecret),
	)
}

// workspaceOf roots file-backed saves at the invocation directory: the
// jail confines rel paths beneath it, and absolute invocations fail
// closed with a toast rather than a write elsewhere.
func workspaceOf(file string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// saveRelOf names the save target for a file-backed boot: the invocation
// path made workspace-relative when it sits inside the workspace. An
// outside absolute path stays absolute and the saver jail refuses it —
// loudly, at save time, not silently elsewhere.
func saveRelOf(file string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return file
	}
	if rel, err := filepath.Rel(cwd, file); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return file
}

// startTeaProgram runs the model fullscreen until quit. Input is explicit
// (never an ambient os.Stdin): headless gates drive EOF through it and
// the program quits cleanly, while a real terminal drives keys.
func startTeaProgram(stdin io.Reader, stdout, stderr io.Writer, model *ui.Model) int {
	prog := tea.NewProgram(model, tea.WithAltScreen(), tea.WithInput(stdin), tea.WithOutput(stdout))
	model.Bind(prog.Send)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(stderr, "pulse: tui: %v\n", err)
		return exitFail
	}
	engine.ReleaseTelemetry(model.Report())
	return exitOK
}

// describeTools is used only by `--help`, and exists so the CLI and the MCP
// registry cannot disagree about what Pulse offers: both read the same
// constants. Every tool the server can register is listed — a help text
// that names ten of sixteen is how an operator concludes the other six do
// not exist.
func describeTools() string {
	return strings.Join([]string{
		mcp.ToolExecuteRequest,
		mcp.ToolExecuteFuzz,
		mcp.ToolListCollections,
		mcp.ToolListRequests,
		mcp.ToolGetRequest,
		mcp.ToolGetRun,
		mcp.ToolGetHistory,
		mcp.ToolFuzzStart,
		mcp.ToolFuzzNext,
		mcp.ToolFuzzReport,
		mcp.ToolGetHTTPSyntaxSpec,
		mcp.ToolCreateCollection,
		mcp.ToolUpsertRequest,
		mcp.ToolDeleteRequest,
		mcp.ToolImportCurl,
		mcp.ToolValidateCollect,
	}, ", ")
}

// shiftFlags moves known --flags before the first positional argument.
//
// Go's flag package stops parsing at the first non-flag, so a command
// advertised as `pulse test <dir> [--parallel N]` would silently IGNORE
// every flag after the directory — the operator's tuning vanishing
// without an error. Shifting only KNOWN names keeps the failure mode
// for typos: an unknown --flag stays put and fails closed in Parse.
// takesValue names which flags consume the following argument; a value
// starting with '-' is never consumed (it is the next flag, not a path).
func shiftFlags(args []string, takesValue map[string]bool) []string {
	var pos, moved []string
	wantValue := false
	for _, a := range args {
		if wantValue && !strings.HasPrefix(a, "-") {
			moved = append(moved, a)
			wantValue = false
			continue
		}
		wantValue = false
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, _, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		takes, ok := takesValue[name]
		if !ok {
			pos = append(pos, a) // unknown: leave for Parse to reject
			continue
		}
		moved = append(moved, a)
		wantValue = takes && !hasVal
	}
	return append(moved, pos...)
}

// vaultKeyID resolves the keychain suffix: the explicit flag wins, the
// PULSE_VAULT_KEY_ID environment covers every vault invocation without
// repeating it, and empty selects the default. A flag that silently
// ignored the environment would keygen under one id and decrypt under
// another — the exact split this resolves.
func vaultKeyID(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("PULSE_VAULT_KEY_ID")
}

// ---- pulse test ------------------------------------------------------------

// runTest is the `pulse test <dir>` entry.
//
// The exit code is a pure function of the run, mirrored from the engine's own
// mapping so the CLI and the runner cannot disagree (ROADMAP.md §4.1.4).
func runTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	args = shiftFlags(args, map[string]bool{
		"parallel": true, "timeout": true, "strict-blocked": false,
		"format": true, "silent": false,
	})
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

	if err := application.Boot(ctx); err != nil {
		fmt.Fprintf(stderr, "pulse test: boot: %v\n", err)
		return exitFail
	}
	code, err := application.TestDirEx(ctx, root, stdout, *parallel, *timeout, *strictBlocked, format, *silent)
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
func runVault(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	args = shiftFlags(args, map[string]bool{
		"key-id": true, "out": true, "recipients": true,
	})
	fs := flag.NewFlagSet("pulse vault", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		keyID      = fs.String("key-id", "", "keychain account suffix; empty selects the default")
		envOut     = fs.String("out", "", "plaintext output path for decrypt (default: the .env path)")
		recipients = fs.String("recipients", secrets.DefaultRecipientsFile, "recipient list for rotate")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse vault init [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault keygen [--key-id ID]")
		fmt.Fprintln(stderr, "       pulse vault encrypt <path.env> [--key-id ID] [--recipients FILE]")
		fmt.Fprintln(stderr, "       pulse vault decrypt <path.env.vault> [--key-id ID] [--out PATH]")
		fmt.Fprintln(stderr, "       pulse vault add-recipient <pulse-pub-...> [--recipients FILE]")
		fmt.Fprintln(stderr, "       pulse vault rotate <path.env> [--key-id ID] [--recipients FILE]")
		fmt.Fprintln(stderr, "       pulse vault set <KEY> [VALUE]")
		fmt.Fprintln(stderr, "       pulse vault list")
		fmt.Fprintln(stderr, "       pulse vault unset <KEY>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// The flag and the environment agree on the key id (neither may
	// silently win): an explicit --key-id overrides PULSE_VAULT_KEY_ID.
	kid := vaultKeyID(*keyID)
	cmd := fs.Arg(0)
	if cmd == "" {
		fmt.Fprintln(stderr, "pulse vault: init, keygen, encrypt, decrypt, add-recipient, rotate, set, list or unset is required")
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
		account, err := application.VaultInit(kid, store)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault init: %v\n", err)
			return vaultExit(err)
		}
		fmt.Fprintf(stderr, "pulse: vault key stored in the keychain as %q\n", account)
		return exitOK

	case "keygen":
		decryptPub, signPub, err := application.VaultKeygen(kid, store)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault keygen: %v\n", err)
			return vaultExit(err)
		}
		// Shareable output goes to stdout: `vault keygen` is a single-shot
		// command like `vault init`, never an MCP-serving path, so there is
		// no JSON-RPC stream to corrupt — and the public halves are designed
		// to be copied, which stderr would discourage.
		fmt.Fprintln(stdout, decryptPub)
		fmt.Fprintln(stdout, signPub)
		return exitOK

	case "add-recipient":
		pub := fs.Arg(1)
		if pub == "" {
			fmt.Fprintln(stderr, "pulse vault add-recipient: a pulse-pub-... key is required")
			return exitUsage
		}
		if err := secrets.AddRecipient(*recipients, pub); err != nil {
			fmt.Fprintf(stderr, "pulse vault add-recipient: %v\n", err)
			return vaultExit(err)
		}
		fmt.Fprintf(stderr, "pulse: recipient listed in %s\n", *recipients)
		return exitOK

	case "rotate":
		path := fs.Arg(1)
		if path == "" {
			fmt.Fprintln(stderr, "pulse vault rotate: a .env path is required")
			return exitUsage
		}
		if err := application.VaultRotate(path, kid, *recipients, resolver); err != nil {
			fmt.Fprintf(stderr, "pulse vault rotate: %v\n", err)
			return vaultExit(err)
		}
		_, vault := app.VaultPaths(path)
		fmt.Fprintf(stderr, "pulse: re-sealed %s for the current recipient list\n", vault)
		return exitOK

	case "encrypt", "decrypt":
		path := fs.Arg(1)
		if path == "" {
			fmt.Fprintf(stderr, "pulse vault %s: a .env path is required\n", cmd)
			return exitUsage
		}

		if cmd == "encrypt" {
			if err := application.VaultEncrypt(path, kid, *recipients, resolver); err != nil {
				fmt.Fprintf(stderr, "pulse vault encrypt: %v\n", err)
				return vaultExit(err)
			}
			_, vault := app.VaultPaths(path)
			fmt.Fprintf(stderr, "pulse: wrote %s and removed the plaintext\n", vault)
			return exitOK
		}

		out := *envOut
		if out == "" {
			out = path
		}
		plain, err := application.VaultDecryptTo(path, out, kid, resolver)
		if err != nil {
			fmt.Fprintf(stderr, "pulse vault decrypt: %v\n", err)
			return vaultExit(err)
		}
		fmt.Fprintf(stderr, "pulse: wrote %s\n", plain)
		return exitOK

	case "set":
		return runVaultSet(ctx, application, kid, fs.Args()[1:], stdin, stdout, stderr)

	case "list":
		if fs.NArg() > 1 {
			fmt.Fprintf(stderr, "pulse vault list: unexpected argument %q\n", fs.Arg(1))
			return exitUsage
		}
		return runVaultList(ctx, application, kid, stdout, stderr)

	case "unset":
		return runVaultUnset(ctx, application, kid, fs.Args()[1:], stderr)

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

// ---- pulse sync ------------------------------------------------------------

// runSync serves or inspects the P2P layer.
//
// `serve <dir>` boots the App with dir as the publish source and blocks
// until SIGINT or stdin EOF: gossip rounds, dir re-convergence, and
// snapshot persistence all run in the background while it waits. `status`
// never joins the network — it reads the last persisted snapshot and
// reports the digest, so convergence is proven by equal digests, not by
// a connection count that flaps with the LAN.
func runSync(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "pulse sync: serve <dir> or status is required")
		return exitUsage
	}
	switch args[0] {
	case "serve":
		if len(args) < 2 || args[1] == "" {
			fmt.Fprintln(stderr, "pulse sync serve: a collection directory is required")
			return exitUsage
		}
		return runSyncServe(ctx, args[1], stdin, stderr)
	case "status":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "pulse: unexpected argument %q after `pulse sync status`\n", args[1])
			return exitUsage
		}
		return runSyncStatus(ctx, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pulse sync: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

func runSyncServe(ctx context.Context, dir string, stdin io.Reader, stderr io.Writer) int {
	cfg := appConfig()
	cfg.SyncPublishDir = dir
	application, err := app.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return app.SecretExitCode(err)
	}
	if err := application.Boot(ctx); err != nil {
		fmt.Fprintf(stderr, "pulse sync serve: boot: %v\n", err)
		_ = application.Close()
		return exitFail
	}
	sub := application.Sync()
	if sub == nil {
		// Serving without sync is a refusal, not a mode: every failure
		// already degraded loudly into Degradations, and echoing them
		// here tells the operator exactly what to fix.
		for _, d := range application.Degradations {
			fmt.Fprintf(stderr, "pulse: degraded: %s\n", d)
		}
		fmt.Fprintln(stderr, "pulse sync serve: sync did not boot; refusing to serve")
		_ = application.Close()
		return exitFail
	}
	fmt.Fprintf(stderr, "pulse: sync serving %s as %s\n", dir, sub.LiveSyncStatus().NodeID)

	// Block until the operator stops us or stdin ends (piped stdin EOF is
	// how a supervisor says goodbye). Either way the deferred Close
	// persists the final snapshot and withdraws the advertisement.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		stop()
	}()
	<-sigCtx.Done()
	if err := application.Close(); err != nil {
		fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		return exitFail
	}
	return exitOK
}

func runSyncStatus(ctx context.Context, stdout, stderr io.Writer) int {
	cfg := appConfig()
	cfg.SkipSync = true // observe, never join: no ports, no announce
	application, err := app.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "pulse: %v\n", err)
		return app.SecretExitCode(err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		}
	}()
	st, err := application.SyncStatus(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "pulse sync status: %v\n", err)
		return exitFail
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "pulse sync status: %v\n", err)
		return exitFail
	}
	fmt.Fprintln(stdout, string(raw))
	return exitOK
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

	if err := application.Boot(ctx); err != nil {
		fmt.Fprintf(stderr, "pulse: boot: %v\n", err)
		return exitFail
	}
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
		Home:       os.Getenv("PULSE_HOME"),
		EnvFile:    os.Getenv("PULSE_ENV_FILE"),
		VaultFile:  os.Getenv("PULSE_VAULT_FILE"),
		VaultKeyID: os.Getenv("PULSE_VAULT_KEY_ID"),
	}
	if v := os.Getenv("PULSE_RAG_ROOT"); v != "" {
		cfg.CorpusRoot = v
	}
	if v := os.Getenv("PULSE_SYNC_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			mcp.Logf("PULSE_SYNC_PORT=%q is not an integer; the default port is used", v)
		} else {
			cfg.SyncPort = n
		}
	}
	if v := os.Getenv("PULSE_SYNC_PEERS_FILE"); v != "" {
		cfg.SyncPeersFile = v
	}
	// Manual backend (ROADMAP §4.2.3): "node=host:port,node=host:port".
	// Malformed entries are reported and skipped, never fatal — and an
	// address without a ledger pin is never dialed (the pin gate holds).
	if v := os.Getenv("PULSE_SYNC_STATIC_PEERS"); v != "" {
		peers := map[string]string{}
		for _, e := range strings.Split(v, ",") {
			node, addr, ok := strings.Cut(strings.TrimSpace(e), "=")
			if !ok || node == "" || addr == "" {
				mcp.Logf("PULSE_SYNC_STATIC_PEERS entry %q is not node=host:port; skipped", e)
				continue
			}
			peers[node] = addr
		}
		cfg.SyncStaticPeers = peers
	}
	if v := os.Getenv("PULSE_SKIP_SYNC"); v == "1" || strings.EqualFold(v, "true") {
		cfg.SkipSync = true
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
