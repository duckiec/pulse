package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"

	"pulse/internal/app"
	"pulse/internal/ast"
	"pulse/internal/secrets"
)

// This file is Phase 2.3's CLI onboarding: one-step `vault set/list/unset`
// with TTY-aware secret input, `pulse init` workspace scaffolding, and
// `pulse doctor` workspace diagnostics.
//
// All three commands are single-shot (never MCP-serving), so human-facing
// output goes to stderr and machine-copyable blocks (MCP JSON) to stdout.
// No code path here prints a secret value — only key names.

// ---- vault set input -------------------------------------------------------

// resolveVaultSecret returns the value for `vault set <KEY> [VALUE]`.
// An explicit VALUE wins; otherwise input comes from stdin: a TTY prompts
// securely without echo, a pipe is read to EOF. Empty input fails closed —
// a dummy or empty vault entry protects nothing. The value never appears
// in an error.
func resolveVaultSecret(stdin io.Reader, key, argValue string, stderr io.Writer) (string, error) {
	if argValue != "" {
		return argValue, nil
	}
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintf(stderr, "Enter value for %s: ", key)
			pw, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(stderr)
			if err != nil {
				return "", fmt.Errorf("pulse vault set: read the secret: %w", err)
			}
			v := strings.TrimSpace(string(pw))
			clear(pw)
			if v == "" {
				return "", fmt.Errorf("pulse vault set: refusing an empty value for %q (fail closed, never a dummy)", key)
			}
			return v, nil
		}
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("pulse vault set: read the secret from stdin: %w", err)
	}
	v := strings.TrimSpace(string(raw))
	clear(raw)
	if v == "" {
		return "", fmt.Errorf("pulse vault set: no value for %q (pass it as an argument or pipe it on stdin)", key)
	}
	return v, nil
}

// runVaultSet implements `pulse vault set <KEY> [VALUE]`.
func runVaultSet(ctx context.Context, application *app.App, kid string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] == "" {
		fmt.Fprintln(stderr, "pulse vault set: a KEY is required (usage: pulse vault set <KEY> [VALUE])")
		return exitUsage
	}
	if len(args) > 2 {
		fmt.Fprintln(stderr, "pulse vault set: at most KEY and VALUE (usage: pulse vault set <KEY> [VALUE])")
		return exitUsage
	}
	key := args[0]
	var argValue string
	if len(args) == 2 {
		argValue = args[1]
	}
	value, err := resolveVaultSecret(stdin, key, argValue, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	defer clear([]byte(value))

	vaultPath := app.ResolveVaultPath()
	store := secrets.NewStore()
	resolver := secrets.NewBackend()
	if err := application.VaultSet(vaultPath, kid, key, value, store, resolver); err != nil {
		fmt.Fprintf(stderr, "pulse vault set: %v\n", err)
		return vaultExit(err)
	}
	abs, err := filepath.Abs(vaultPath)
	if err != nil {
		abs = vaultPath
	}
	fmt.Fprintf(stderr, "pulse: sealed %q into %s\n", key, abs)
	return exitOK
}

// runVaultList implements `pulse vault list`: sorted key names on stdout,
// never values.
func runVaultList(ctx context.Context, application *app.App, kid string, stdout, stderr io.Writer) int {
	vaultPath := app.ResolveVaultPath()
	keys, err := application.VaultList(vaultPath, kid, secrets.NewBackend())
	if err != nil {
		fmt.Fprintf(stderr, "pulse vault list: %v\n", err)
		return vaultExit(err)
	}
	for _, k := range keys {
		fmt.Fprintln(stdout, k)
	}
	return exitOK
}

// runVaultUnset implements `pulse vault unset <KEY>`.
func runVaultUnset(ctx context.Context, application *app.App, kid string, args []string, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(stderr, "pulse vault unset: a KEY is required (usage: pulse vault unset <KEY>)")
		return exitUsage
	}
	vaultPath := app.ResolveVaultPath()
	if err := application.VaultUnset(vaultPath, kid, args[0], secrets.NewBackend()); err != nil {
		fmt.Fprintf(stderr, "pulse vault unset: %v\n", err)
		return vaultExit(err)
	}
	fmt.Fprintf(stderr, "pulse: removed %q from %s\n", args[0], vaultPath)
	return exitOK
}

// ---- pulse init ------------------------------------------------------------

// quickstartTemplate is the scaffold `pulse init` writes. It must parse
// cleanly (gated by TestInitTemplateParses) and demonstrates every
// first-hour shape: a GET, a POST with a JSON body, an authenticated
// request whose token seals via `pulse vault set DEMO_TOKEN`, and a
// sandboxed `@wasm-sign` request.
const quickstartTemplate = `# pulse quickstart — created by ` + "`pulse init`" + `.
# Run it headless with: go run ./cmd/pulse test . --format tap
# Blocks are separated by ` + "`###`" + `. See docs/HTTP_SYNTAX.md for the full grammar.

@base = http://localhost:8080

# A first GET: no secrets, no body.
# @name hello
# @assert status == 200
GET {{base}}/hello HTTP/1.1
Accept: application/json

###

# A POST with a JSON payload.
# @name create-note
# @assert status == 201
POST {{base}}/notes HTTP/1.1
Content-Type: application/json

{"title": "hello", "body": "my first note"}

###

# An authenticated request. The token is never pasted here: seal it with
#   pulse vault set DEMO_TOKEN <value>
# and Pulse resolves {{DEMO_TOKEN}} at runtime from the vault (or the OS
# keychain, or an ephemeral .env) — then fails closed with exit 3 when it
# is missing anywhere.
# @name whoami
# @assert status == 200
GET {{base}}/me HTTP/1.1
Accept: application/json
Authorization: Bearer {{DEMO_TOKEN}}

###

# A signed request. The sandboxed WASM module signs the pre-flight
# envelope; it runs with no filesystem, no network, and no clock.
# @name signed-call
# @assert status == 200
@wasm-sign ./sign/auth.wasm
GET {{base}}/signed HTTP/1.1
Accept: application/json

###
`

// runInit implements `pulse init [file] [--force]`.
func runInit(args []string, stdout, stderr io.Writer) int {
	args = shiftFlags(args, map[string]bool{"force": false})
	fs := flag.NewFlagSet("pulse init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "overwrite the target file when it already exists")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse init [file] [--force]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	target := fs.Arg(0)
	if target == "" {
		target = "quickstart.http"
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "pulse init: one file at a time (usage: pulse init [file] [--force])")
		return exitUsage
	}
	if _, err := os.Stat(target); err == nil && !*force {
		fmt.Fprintf(stderr, "pulse init: %s already exists (pass --force to overwrite)\n", target)
		return exitFail
	} else if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "pulse init: stat %s: %v\n", target, err)
		return exitFail
	}
	if err := os.WriteFile(target, []byte(quickstartTemplate), 0o644); err != nil {
		fmt.Fprintf(stderr, "pulse init: write %s: %v\n", target, err)
		return exitFail
	}
	if _, err := ast.ParseFile(target); err != nil {
		fmt.Fprintf(stderr, "pulse init: the scaffold at %s does not parse: %v\n", target, err)
		return exitFail
	}
	fmt.Fprintf(stderr, "pulse: wrote %s\n", target)
	fmt.Fprintf(stderr, "pulse: next: pulse vault set DEMO_TOKEN <value>  # seal the demo token\n")
	return exitOK
}

// ---- pulse doctor ----------------------------------------------------------

// doctorResult is one diagnostic line.
type doctorResult struct {
	name   string
	ok     bool
	detail string
}

// runDoctor implements `pulse doctor`: keyring reachability, vault
// integrity, SQLite health, .http syntax plus secret cross-check, and the
// MCP client configuration block.
//
// Exit mapping: 0 healthy; 2 workspace syntax errors (AGENTS.md §4.4);
// 3 a referenced secret is set nowhere; 1 any infrastructure failure
// (unreadable store, corrupt vault). A missing keychain or a missing vault
// is a WARNING, never a failure: headless machines degrade by design and a
// fresh workspace has no vault yet.
func runDoctor(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	args = shiftFlags(args, map[string]bool{"key-id": true})
	fs := flag.NewFlagSet("pulse doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyID := fs.String("key-id", "", "keychain account suffix; empty selects the default")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pulse doctor [--key-id ID]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pulse doctor: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	kid := vaultKeyID(*keyID)

	application, err := app.New(ctx, appConfig())
	if err != nil {
		fmt.Fprintf(stderr, "pulse doctor: %v\n", err)
		return app.SecretExitCode(err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse: shutdown: %v\n", err)
		}
	}()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pulse doctor: workspace: %v\n", err)
		return exitFail
	}
	vaultPath := app.ResolveVaultPath()
	vaultAbs, err := filepath.Abs(vaultPath)
	if err != nil {
		vaultAbs = vaultPath
	}

	var lines []doctorResult
	infraFail := false

	// 1. OS keyring reachability (warning, not failure).
	if err := secrets.NewStore().Available(); err != nil {
		lines = append(lines, doctorResult{"keyring write", false, fmt.Sprintf("WARN: no keychain write path (%v)", err)})
	} else {
		lines = append(lines, doctorResult{"keyring write", true, fmt.Sprintf("ok (%s)", secrets.NewStore().Backend())})
	}
	if err := secrets.NewBackend().Available(); err != nil {
		lines = append(lines, doctorResult{"keyring read", false, fmt.Sprintf("WARN: no keychain read path (%v)", err)})
	} else {
		lines = append(lines, doctorResult{"keyring read", true, fmt.Sprintf("ok (%s)", secrets.NewBackend().Backend())})
	}

	// 2. Vault integrity (absent = warning; corrupt = failure).
	vaultKeys := map[string]bool{}
	if _, err := os.Stat(vaultPath); os.IsNotExist(err) {
		lines = append(lines, doctorResult{"vault", false, fmt.Sprintf("WARN: no vault at %s (run `pulse vault set <KEY> <VALUE>` to create one)", vaultAbs)})
	} else if err != nil {
		lines = append(lines, doctorResult{"vault", false, fmt.Sprintf("FAIL: stat %s: %v", vaultAbs, err)})
		infraFail = true
	} else if keys, set, err := app.VaultKeyNames(vaultPath, kid, secrets.NewBackend()); err != nil {
		lines = append(lines, doctorResult{"vault", false, fmt.Sprintf("FAIL: %s does not open: %v", vaultAbs, err)})
		infraFail = true
	} else {
		vaultKeys = set
		lines = append(lines, doctorResult{"vault", true, fmt.Sprintf("ok (%s holds %d keys)", vaultAbs, len(keys))})
	}

	// 3. SQLite index health (failure).
	if application.Store == nil {
		lines = append(lines, doctorResult{"sqlite", false, "FAIL: no store is open"})
		infraFail = true
	} else if err := sqliteHealth(ctx, application); err != nil {
		lines = append(lines, doctorResult{"sqlite", false, fmt.Sprintf("FAIL: %s: %v", application.Store.Path(), err)})
		infraFail = true
	} else {
		lines = append(lines, doctorResult{"sqlite", true, fmt.Sprintf("ok (%s)", application.Store.Path())})
	}

	// 4. Workspace syntax + secret cross-check.
	files, err := collectHTTPFiles(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "pulse doctor: walk %s: %v\n", cwd, err)
		return exitFail
	}
	if len(files) == 0 {
		lines = append(lines, doctorResult{"collections", false, "WARN: no .http files in the workspace (run `pulse init` to scaffold one)"})
	}
	var syntaxErrs []string
	defined := map[string]bool{}
	placeholders := map[string][]string{} // var -> files using it
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			syntaxErrs = append(syntaxErrs, fmt.Sprintf("%s: unreadable: %v", relOf(cwd, f), err))
			continue
		}
		for _, v := range fileDefinedVars(string(raw)) {
			defined[v] = true
		}
		if _, err := ast.ParseFile(f); err != nil {
			syntaxErrs = append(syntaxErrs, fmt.Sprintf("%s: %v", relOf(cwd, f), err))
		}
		for _, v := range secrets.ExtractVariables(string(raw)) {
			placeholders[v] = append(placeholders[v], relOf(cwd, f))
		}
	}
	var missing []string
	for name := range placeholders {
		if defined[name] || vaultKeys[name] {
			continue
		}
		if _, err := application.Chain.Resolve(name); err == nil {
			continue
		}
		filesUsing := placeholders[name]
		sort.Strings(filesUsing)
		missing = append(missing, fmt.Sprintf("%s (referenced in %s)", name, strings.Join(filesUsing, ", ")))
	}
	sort.Strings(missing)
	sort.Strings(syntaxErrs)

	for _, l := range lines {
		fmt.Fprintf(stderr, "pulse doctor: %-12s %s\n", l.name, l.detail)
	}
	fmt.Fprintf(stderr, "pulse doctor: %d .http file(s) in %s\n", len(files), cwd)
	for _, e := range syntaxErrs {
		fmt.Fprintf(stderr, "pulse doctor: syntax: %s\n", e)
	}
	for _, m := range missing {
		fmt.Fprintf(stderr, "pulse doctor: missing secret: %s is set nowhere (seal it with `pulse vault set %s <value>`)\n", m, strings.SplitN(m, " ", 2)[0])
	}

	// 5. MCP client configuration (always printed: it is useful healthy or not).
	printMCPBlock(stdout, stderr)

	switch {
	case len(syntaxErrs) > 0:
		return exitUsage
	case len(missing) > 0:
		return exitSec
	case infraFail:
		return exitFail
	default:
		fmt.Fprintf(stderr, "pulse doctor: healthy\n")
		return exitOK
	}
}

// sqliteHealth runs PRAGMA integrity_check through the open store. The
// store exposes its handle (DB()) precisely so diagnostics need no second
// open — two opens could disagree about which file is healthy.
func sqliteHealth(ctx context.Context, application *app.App) error {
	var v string
	if err := application.Store.DB().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&v); err != nil {
		return err
	}
	if v != "ok" {
		return fmt.Errorf("integrity_check reports %q", v)
	}
	return nil
}

// collectHTTPFiles returns the workspace's .http files, recursive and
// sorted, so doctor output diffs cleanly between runs.
func collectHTTPFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".http") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// fileDefinedVars collects `@name = value` definitions from raw file text
// so the cross-check does not mistake a file-local variable for a missing
// secret. It mirrors the grammar's `=` requirement: a `@var` line without
// `=` defines nothing.
func fileDefinedVars(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "@") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "@"))
		name, _, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// relOf renders an absolute path workspace-relative for stable output.
func relOf(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return p
}

// printMCPBlock prints a ready-to-copy mcpServers JSON block whose command
// is the absolute path of the running pulse binary, plus where each of the
// four named clients reads it. All four share the mcpServers schema (a
// command plus an argv of ["mcp"]), so one block serves every client.
func printMCPBlock(stdout, stderr io.Writer) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		fmt.Fprintln(stderr, "pulse doctor: mcp: cannot locate the running binary; reinstall pulse and re-run doctor")
		return
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	block := map[string]any{
		"mcpServers": map[string]any{
			"pulse": map[string]any{
				"command": exe,
				"args":    []string{"mcp"},
			},
		},
	}
	raw, err := json.MarshalIndent(block, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "pulse doctor: mcp: render the config block: %v\n", err)
		return
	}
	fmt.Fprintln(stdout, string(raw))
	fmt.Fprintln(stderr, "pulse doctor: mcp: paste the block above into:")
	fmt.Fprintln(stderr, "pulse doctor: mcp:   Claude Desktop → <config>/claude_desktop_config.json")
	fmt.Fprintln(stderr, "pulse doctor: mcp:   Cursor           → ~/.cursor/mcp.json (project: .cursor/mcp.json)")
	fmt.Fprintln(stderr, "pulse doctor: mcp:   Windsurf         → ~/.codeium/windsurf/mcp_config.json")
	fmt.Fprintln(stderr, "pulse doctor: mcp:   OpenCode         → opencode.json (`\"mcp\"` section)")
}
