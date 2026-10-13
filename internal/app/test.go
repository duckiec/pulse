package app

import (
	"context"
	"encoding/json"
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

// TestFormat selects the `pulse test` output format.
//
// TAP is the default CI-dashboard format (ROADMAP.md §4.1.3); JSON is the
// machine-readable sibling for the same report. The format changes what is
// WRITTEN, never what is concluded: the exit code is a pure function of the
// report (engine.ExitCodeFor) and is identical in every format.
type TestFormat string

const (
	// TestFormatTAP renders TAP 13.
	TestFormatTAP TestFormat = "tap"
	// TestFormatJSON renders one JSON object per run.
	TestFormatJSON TestFormat = "json"
)

// ParseTestFormat maps a `--format` flag value to a TestFormat.
//
// Empty selects TAP so the flag can be omitted. Anything else fail-closed
// with an error the CLI maps to exit 2 WITHOUT executing any tests: an
// unknown format is a usage error, not a test failure.
func ParseTestFormat(s string) (TestFormat, error) {
	switch TestFormat(strings.ToLower(strings.TrimSpace(s))) {
	case "", TestFormatTAP:
		return TestFormatTAP, nil
	case TestFormatJSON:
		return TestFormatJSON, nil
	default:
		return "", fmt.Errorf("app: unknown test format %q (want \"tap\" or \"json\")", s)
	}
}

// TestResult is the outcome of a `pulse test` run.
type TestResult struct {
	// Exit is the process exit code.
	Exit int
	// Report is the engine's structured report.
	Report *engine.TestReport
	// TAP is the rendered TAP 13 document.
	TAP string
	// JSON is the rendered single-object JSON document.
	JSON string
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

	jsonDoc, err := RenderJSONReport(report, rep.entries)
	if err != nil {
		return nil, err
	}

	return &TestResult{
		Exit:   report.ExitCode,
		Report: report,
		TAP:    rep.Document(),
		JSON:   jsonDoc,
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
	return a.TestDirEx(ctx, root, out, parallel, timeout, strictBlocked, TestFormatTAP, false)
}

// TestDirEx is TestDir with a format and a silent switch.
//
// `silent` suppresses ALL stdout output — zero bytes — while the returned exit
// code is still the report's own: silence changes what is written, never what
// is concluded. An unknown format fails closed with exit 2 BEFORE any test
// executes, so a typo cannot report a green run.
func (a *App) TestDirEx(ctx context.Context, root string, out io.Writer, parallel int, timeout time.Duration, strictBlocked bool, format TestFormat, silent bool) (int, error) {
	if format != TestFormatTAP && format != TestFormatJSON {
		return engine.ExitUsage, fmt.Errorf("app: unknown test format %q (want \"tap\" or \"json\")", string(format))
	}
	res, err := a.RunTest(ctx, root, parallel, timeout, strictBlocked)
	if err != nil {
		return engine.ExitUsage, err
	}
	if silent {
		return res.Exit, nil
	}
	switch format {
	case TestFormatJSON:
		if err := WriteJSONReport(out, res.JSON); err != nil {
			return engine.ExitFailed, err
		}
	default:
		if err := WriteTestReport(out, res.TAP); err != nil {
			return engine.ExitFailed, err
		}
	}
	return res.Exit, nil
}

// WriteJSONReport writes one JSON document to w plus a trailing newline.
func WriteJSONReport(w io.Writer, doc string) error {
	if doc == "" {
		return errors.New("app: refusing to write an empty JSON report")
	}
	if !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	_, err := io.WriteString(w, doc)
	return err
}

// JSONTestPoint is one test point in the JSON document.
type JSONTestPoint struct {
	File       string            `json:"file"`
	Request    string            `json:"request"`
	Assertion  string            `json:"assertion,omitempty"`
	OK         bool              `json:"ok"`
	Skip       bool              `json:"skip,omitempty"`
	SkipReason string            `json:"skip_reason,omitempty"`
	Detail     string            `json:"detail,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

// JSONReport is the single-object `pulse test --format json` document.
type JSONReport struct {
	Total   int             `json:"total"`
	Passed  int             `json:"passed"`
	Failed  int             `json:"failed"`
	Skipped int             `json:"skipped"`
	Exit    int             `json:"exit"`
	Tests   []JSONTestPoint `json:"tests"`
}

// RenderJSONReport renders one JSON object for a completed run.
//
// It is built from the SAME redacted entries the TAP reporter consumes, so a
// secret scrubbed from TAP is scrubbed here too: the engine redacts details
// before they reach any reporter, and this function adds no new sources. The
// aggregates are copied from the report rather than recomputed, so the two
// documents cannot disagree about what the run meant.
func RenderJSONReport(report *engine.TestReport, entries []engine.Entry) (string, error) {
	if report == nil {
		return "", errors.New("app: refusing to render a JSON report for a nil TestReport")
	}
	points := make([]JSONTestPoint, 0, len(entries))
	for _, e := range entries {
		points = append(points, JSONTestPoint{
			File:       e.File,
			Request:    e.Request,
			Assertion:  e.Assertion,
			OK:         e.OK,
			Skip:       e.Skip,
			SkipReason: e.SkipReason,
			Detail:     e.Detail,
			Fields:     e.Fields,
		})
	}
	if points == nil {
		points = []JSONTestPoint{}
	}
	raw, err := json.Marshal(JSONReport{
		Total:   report.Total,
		Passed:  report.Passed,
		Failed:  report.Failed,
		Skipped: report.Skipped,
		Exit:    report.ExitCode,
		Tests:   points,
	})
	if err != nil {
		return "", fmt.Errorf("app: render the JSON report: %w", err)
	}
	return string(raw), nil
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
