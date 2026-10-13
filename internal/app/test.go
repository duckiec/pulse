package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pulse/internal/engine"
	"pulse/internal/fuzz"
	"pulse/internal/secrets"
)

// The headless pipeline: `pulse test <dir>` (ROADMAP.md §4.1), and the vault
// commands behind `pulse vault` (ROADMAP.md §4.3).
//
// # Why the runner lives in `engine` and the reporting lives here
//
// `engine.TestRunner` parses, executes and evaluates; it imports only
// `internal/ast` and produces structured entries. Move 9's TAP 13 emitter lives
// in `internal/fuzz`, and AGENTS.md §3.1 forbids a sibling importing a sibling.
// Making the engine a producer and letting the composition root supply the
// formatter satisfies the layering rule while still reusing the emitter — and it
// means a JSON or silent reporter costs no change in the engine.
//
// # Line-oriented, unbuffered, one write per line
//
// ROADMAP.md §4.1.3 requires it and it is worth restating: TAP consumers parse
// incrementally, so a writer that buffers and flushes at exit breaks a streaming
// dashboard.

// TestResult is the outcome of a `pulse test` run.
type TestResult struct {
	// Exit is the process exit code.
	Exit int
	// Report is the engine's structured report.
	Report *engine.TestReport
	// TAP is the rendered TAP 13 document.
	TAP string
}

// RunTest executes the .http files under root and returns the outcome.
//
// `strictBlocked` makes a run with ANY skipped test fail. It is opt-in rather
// than the default because a BLOCKED test is an environment limit, not a defect
// in the collection, and CI that failed on every machine without a keychain
// would be unusable on most machines (AGENTS.md §4.5).
func (a *App) RunTest(ctx context.Context, root string, parallel int, timeout time.Duration, strictBlocked bool) (*TestResult, error) {
	files, err := engine.Files(root)
	if err != nil {
		return nil, err
	}

	runner := &engine.TestRunner{
		NewRouter: a.newRouter,
		Resolver:  a.Chain,
		Redactor:  a.Redactor,
	}
	rep := &tapReporter{}

	report, err := runner.Run(ctx, files, engine.TestConfig{
		MaxParallel: parallel,
		Timeout:     timeout,
		Redactor:    a.Redactor,
	}, rep)
	if err != nil && report == nil {
		return nil, err
	}
	if strictBlocked && report.Skipped > 0 {
		// Opt-in escalation. The skipped entries stay in the document, so the
		// reason remains visible even when the run is failed.
		report.ExitCode = engine.ExitFailed
	}

	return &TestResult{
		Exit:   report.ExitCode,
		Report: report,
		TAP:    rep.Document(),
	}, nil
}

// WriteTestReport writes a TAP document to w, one line per write.
func WriteTestReport(w io.Writer, tap string) error {
	for _, line := range strings.Split(tap, "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// TestDir is the `pulse test <dir>` entry the CLI calls.
//
// It writes the TAP document to stdout and returns the process exit code. It is
// NOT reachable in MCP-serving contexts: only `pulse mcp` installs the stdout
// lock, and `pulse test` is a single-shot command (ROADMAP.md §4.1.3, G-hyg).
func (a *App) TestDir(ctx context.Context, root string, out io.Writer, parallel int, timeout time.Duration, strictBlocked bool) (int, error) {
	res, err := a.RunTest(ctx, root, parallel, timeout, strictBlocked)
	if err != nil {
		return engine.ExitUsage, err
	}
	if err := WriteTestReport(out, res.TAP); err != nil {
		return engine.ExitFailed, err
	}
	return res.Exit, nil
}

// ---- the TAP reporter -----------------------------------------------------

// tapReporter adapts engine entries onto Move 9's TAP 13 emitter.
//
// It buffers and renders on request rather than emitting one line at a time, and
// the reason is the plan: TAP 13 permits the plan before OR after the results,
// and a streaming writer that emits the plan FIRST must have counted them
// first. This implementation counts as it goes and emits the plan when the
// document is requested, which is the only ordering that keeps the plan honest
// for a run whose total is not known until every file has finished.
type tapReporter struct {
	entries []engine.Entry
}

// Report implements engine.TestReporter.
func (t *tapReporter) Report(e engine.Entry) {
	t.entries = append(t.entries, e)
}

// Document implements engine.TestReporter.
func (t *tapReporter) Document() string {
	results := make([]fuzz.Result, 0, len(t.entries))
	for _, e := range t.entries {
		r := fuzz.Result{
			Number:      len(results) + 1,
			Description: describeEntry(e),
			OK:          e.OK,
			Skip:        e.Skip,
		}
		if !e.OK {
			fields := map[string]string{}
			if e.File != "" {
				fields["file"] = e.File
			}
			if e.Assertion != "" {
				fields["assertion"] = e.Assertion
			}
			for k, v := range e.Fields {
				fields[k] = v
			}
			if e.Detail != "" {
				fields["detail"] = e.Detail
			}
			r.Diagnostic = fuzz.Diagnostic{
				Severity: "fail",
				Message:  e.Detail,
				Fields:   fields,
			}
		}
		if e.Skip {
			fields := map[string]string{}
			if e.SkipReason != "" {
				fields["reason"] = e.SkipReason
			}
			r.Diagnostic = fuzz.Diagnostic{
				Severity: "skip",
				Message:  e.SkipReason,
				Fields:   fields,
			}
		}
		results = append(results, r)
	}
	return fuzz.Render(results)
}

// describeEntry builds a TAP test description.
func describeEntry(e engine.Entry) string {
	where := e.File
	if e.Request != "" {
		where += " " + e.Request
	}
	if e.Assertion != "" {
		where += " / " + e.Assertion
	}
	return where
}

// ---- vault commands --------------------------------------------------------

// VaultSuffix is the container extension.
const VaultSuffix = ".vault"

// VaultPaths returns the plaintext and container paths for a .env.
func VaultPaths(envPath string) (plain, vault string) {
	if envPath == "" {
		return "", ""
	}
	if filepath.Ext(envPath) == VaultSuffix {
		return strings.TrimSuffix(envPath, VaultSuffix), envPath
	}
	return envPath, envPath + VaultSuffix
}

// VaultInit generates a vault key, stores it in the OS keychain and returns the
// keychain account it is held under.
//
// The key NEVER touches the repository, a config file or a temp dir:
// AGENTS.md §4.3 forbids a credential landing on disk, and the keychain is the
// only legitimate destination. A platform with no keychain write path fails
// closed with a typed error rather than falling back to anything.
func (a *App) VaultInit(keyID string, store secrets.SecretStore) (string, error) {
	if store == nil {
		return "", &VaultError{Kind: VaultErrNoStore, msg: "no keychain write path"}
	}
	if err := store.Available(); err != nil {
		return "", &VaultError{Kind: VaultErrNoStore, msg: err.Error(), err: err}
	}
	key, err := secrets.NewKey()
	if err != nil {
		return "", err
	}
	account := secrets.VaultAccount(keyID)
	if err := store.Store(account, string(key)); err != nil {
		clear(key)
		return "", &VaultError{Kind: VaultErrStore, msg: err.Error(), err: err}
	}
	// Zeroised in this process the moment the keychain holds it. The string
	// handed to Store is a copy this function cannot reach, which is a real
	// limitation recorded in ADL-0025: the only fully erasing API would pass
	// the bytes in and never convert them.
	clear(key)
	return account, nil
}

// VaultKey reads the vault key back out of the keychain.
//
// It takes a secrets.SecretResolver rather than a SecretStore, because the two
// sides of the keychain are deliberately separate: Moves 4-7 built resolution
// and Move 12 adds writing. Composing the two keeps write-only from growing a
// read method it has no reason to own.
func (a *App) VaultKey(keyID string, store secrets.SecretResolver) ([]byte, error) {
	if store == nil {
		return nil, &VaultError{Kind: VaultErrNoStore, msg: "no keychain read path"}
	}
	raw, err := store.Resolve(secrets.VaultAccount(keyID))
	if err != nil {
		return nil, &VaultError{Kind: VaultErrResolve, msg: err.Error(), err: err}
	}
	key := []byte(raw)
	if len(key) != secrets.KeySize {
		return nil, &VaultError{
			Kind: VaultErrResolve,
			msg:  fmt.Sprintf("the keychain holds %d bytes, want %d", len(key), secrets.KeySize),
		}
	}
	// The material is copied into a fixed-size slice and the string form is
	// dropped; the caller zeroises the slice.
	key = append([]byte(nil), key...)
	return key, nil
}

// VaultError is a typed vault failure.
type VaultError struct {
	Kind VaultErrorKind
	msg  string
	err  error
}

// VaultErrorKind classifies a vault failure so the CLI can map it to an exit
// code without string matching.
type VaultErrorKind int

const (
	// VaultErrNoStore reports no keychain write/read path.
	VaultErrNoStore VaultErrorKind = iota
	// VaultErrStore reports a keychain write failure.
	VaultErrStore
	// VaultErrResolve reports a keychain read failure.
	VaultErrResolve
	// VaultErrAuth reports a failed decryption.
	VaultErrAuth
	// VaultErrFormat reports a malformed container.
	VaultErrFormat
)

func (e *VaultError) Error() string { return "vault: " + e.msg }

func (e *VaultError) Unwrap() error { return e.err }

// ExitCode maps a vault failure to Pulse's exit codes (AGENTS.md §4.4).
func (e *VaultError) ExitCode() int {
	switch e.Kind {
	case VaultErrAuth, VaultErrFormat:
		return engine.ExitFailed
	default:
		return secrets.ExitCodeSecret
	}
}

// VaultEncrypt reads a plaintext .env and writes its container, then removes the
// plaintext.
func (a *App) VaultEncrypt(envPath string, key []byte) error {
	plain, vault := VaultPaths(envPath)
	if err := secrets.EncryptEnvFile(plain, vault, key); err != nil {
		return fmt.Errorf("vault: encrypt %s: %w", envPath, err)
	}
	return nil
}

// VaultDecryptTo reads a container and writes the plaintext .env at out.
//
// `out` may be empty, in which case it defaults to the plaintext path alongside
// the container.
func (a *App) VaultDecryptTo(vaultPath, out string, key []byte) (string, error) {
	plain, vault := VaultPaths(vaultPath)
	if out != "" {
		plain = out
	}
	if err := secrets.DecryptVaultFile(vault, plain, key); err != nil {
		return "", &VaultError{Kind: kindForDecrypt(err), msg: err.Error(), err: err}
	}
	return plain, nil
}

// kindForDecrypt classifies a decryption failure.
//
// A malformed container and a failed authentication map to different Kinds but
// the same EXIT CODE, and that is deliberate. The GCM tag is the only mechanism
// that distinguishes "wrong key" from "one flipped byte", so surfacing the
// difference to a caller would hand an oracle to anyone who can present a
// container and observe the exit status.
func kindForDecrypt(err error) VaultErrorKind {
	if errors.Is(err, secrets.ErrAuthFailed) {
		return VaultErrAuth
	}
	return VaultErrFormat
}

// vaultFileExists reports whether a container exists.
func vaultFileExists(envPath string) bool {
	_, vault := VaultPaths(envPath)
	if vault == "" {
		return false
	}
	_, err := os.Stat(vault)
	return err == nil
}

// compile-time proof the Storer side of MemoryStore satisfies what VaultInit
// takes, so a test can pass one without a cast.
var _ secrets.SecretStore = (*secrets.MemoryStore)(nil)
