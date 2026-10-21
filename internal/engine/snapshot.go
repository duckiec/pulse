package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Snapshot assertions: `# @assert snapshot` attached to a named request.
//
// # What a snapshot is, and why it is a file
//
// A snapshot is the response body a request produced last time it was accepted.
// It lives in an adjacent `<file>.http.snap` file so git can review it, diff it,
// and hold it as the database of record — the same reason the `.http` files
// themselves are plaintext (AGENTS.md §1).
//
// # Format
//
// The file is a tiny line-oriented envelope, not an opaque blob:
//
//	# pulse snapshot v1
//	### @name list-users
//	{"users":[{"id":1}]}
//	### @name get-user
//	{"id":1}
//
// `### @name <name>` starts a namespace, matching the request's `@name`. Bodies
// are stored verbatim (trailing blank lines trimmed). A body that itself
// contains a line starting with that marker cannot be represented; Set fails
// closed rather than writing a file that would misparse.
//
// # Diffing
//
// JSON responses are decoded and compared structurally, so key order and
// whitespace never produce a false failure and a changed value is reported at
// its path. Non-JSON responses fall back to a line diff. v1 is strict: there is
// no per-field selection of dynamic values. A timestamp that changes every run
// is a real difference, and the operator sees it rather than a green facade.

// SnapshotSuffix is appended to the `.http` file: `api.http` → `api.http.snap`.
const SnapshotSuffix = ".snap"

// SnapshotAssertExpr is the whole directive text after `# @assert`.
const SnapshotAssertExpr = "snapshot"

// snapshotHeader identifies the file format. A file without it is refused
// rather than parsed leniently: a snapshot file is machine-written, so an
// unrecognised one is either from another tool or corrupted.
const snapshotHeader = "# pulse snapshot v1"

// snapshotMarker starts one request's namespace.
const snapshotMarker = "### @name "

// MaxDiffLines bounds a rendered diff. A 10,000-line change is summarised
// rather than pasted into a terminal and a TAP comment.
const MaxDiffLines = 200

// Snapshot errors. All fail closed.
var (
	// ErrSnapshotFormat reports a snapshot file that is not in the documented
	// envelope (or is corrupted).
	ErrSnapshotFormat = errors.New("engine: snapshot file is malformed")

	// ErrSnapshotUnnamed reports `# @assert snapshot` on a request with no
	// `# @name`. The name IS the namespace, so there is nothing to key on.
	ErrSnapshotUnnamed = errors.New("engine: a snapshot assertion requires # @name")

	// ErrSnapshotTooLarge reports a body beyond MaxCollectedBytes. Writing a
	// truncated snapshot would make the next run fail for the wrong reason.
	ErrSnapshotTooLarge = errors.New("engine: response body is too large to snapshot")
)

// SnapshotAction records what a check did.
type SnapshotAction string

const (
	// SnapshotMatched means the live body equals the stored one.
	SnapshotMatched SnapshotAction = "matched"
	// SnapshotCreated means no snapshot existed and one was written.
	SnapshotCreated SnapshotAction = "created"
	// SnapshotUpdated means --update-snapshots replaced an existing snapshot.
	SnapshotUpdated SnapshotAction = "updated"
	// SnapshotMismatch means the live body differs and nothing was written.
	SnapshotMismatch SnapshotAction = "mismatch"
)

// SnapshotResult is the outcome of one snapshot assertion.
type SnapshotResult struct {
	// Name is the request's @name.
	Name string
	// Path is the snapshot file that holds (or would hold) it.
	Path string
	// Action records what happened.
	Action SnapshotAction
	// OK reports whether the assertion passes. Created and Updated are OK:
	// accepting a new baseline is not a failure.
	OK bool
	// Diff is a readable difference when OK is false; empty otherwise.
	Diff string
}

// IsSnapshotAssert reports whether an assertion expression is the snapshot
// directive. It is exact: `snapshot` is a directive, not an expression with an
// operand.
func IsSnapshotAssert(expr string) bool {
	return strings.TrimSpace(expr) == SnapshotAssertExpr
}

// SnapshotPath returns the adjacent snapshot file for a `.http` file.
func SnapshotPath(httpFile string) string { return httpFile + SnapshotSuffix }

// SnapshotFile is a parsed snapshot file. The zero value is an empty file.
type SnapshotFile struct {
	path  string
	names []string // insertion order, for a stable error
	vals  map[string]string
}

// LoadSnapshotFile reads path. A missing file is an empty SnapshotFile, not an
// error: the first run is supposed to create it.
func LoadSnapshotFile(path string) (*SnapshotFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &SnapshotFile{path: path, vals: map[string]string{}}, nil
		}
		return nil, fmt.Errorf("engine: read snapshot %s: %w", path, err)
	}
	f, err := ParseSnapshotFile(raw, path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ParseSnapshotFile parses the snapshot envelope. It is exported so the TUI
// and tests can inspect a file without touching disk.
func ParseSnapshotFile(raw []byte, path string) (*SnapshotFile, error) {
	f := &SnapshotFile{path: path, vals: map[string]string{}}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	i := 0
	// An empty file is the pre-first-run state. Anything else must start
	// with the version marker: a comment-only or foreign file is refused
	// rather than treated as empty and silently overwritten.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) {
		return f, nil
	}
	if strings.TrimSpace(lines[i]) != snapshotHeader {
		return nil, fmt.Errorf("%w: %s:%d: expected %q", ErrSnapshotFormat, path, i+1, snapshotHeader)
	}
	i++

	var current string
	var body []string
	flush := func() error {
		if current == "" {
			return nil
		}
		bodyText := trimSnapshotBody(strings.Join(body, "\n"))
		if err := validateSnapshotBody(bodyText); err != nil {
			return fmt.Errorf("%w: %s: request %q: %v", ErrSnapshotFormat, path, current, err)
		}
		if _, dup := f.vals[current]; dup {
			return fmt.Errorf("%w: %s: duplicate snapshot for request %q", ErrSnapshotFormat, path, current)
		}
		f.vals[current] = bodyText
		f.names = append(f.names, current)
		return nil
	}
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, snapshotMarker) {
			if err := flush(); err != nil {
				return nil, err
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, snapshotMarker))
			if name == "" {
				return nil, fmt.Errorf("%w: %s:%d: empty snapshot name", ErrSnapshotFormat, path, i+1)
			}
			current = name
			body = body[:0]
			continue
		}
		if current == "" {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			return nil, fmt.Errorf("%w: %s:%d: content outside a request namespace", ErrSnapshotFormat, path, i+1)
		}
		body = append(body, line)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return f, nil
}

// Get returns a stored snapshot.
func (f *SnapshotFile) Get(name string) (string, bool) {
	if f == nil || f.vals == nil {
		return "", false
	}
	v, ok := f.vals[name]
	return v, ok
}

// Set stores a snapshot body. It fails closed on a body the envelope cannot
// represent, rather than writing a file that misparses on the next run.
func (f *SnapshotFile) Set(name, body string) error {
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	body = trimSnapshotBody(body)
	if err := validateSnapshotBody(body); err != nil {
		return err
	}
	if _, exists := f.vals[name]; !exists {
		f.names = append(f.names, name)
	}
	f.vals[name] = body
	return nil
}

// Names returns the namespaces in file order.
func (f *SnapshotFile) Names() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.names...)
}

// Encode renders the file deterministically: namespaces sorted by name, so a
// run that discovers requests in a different order still produces a
// byte-identical file.
func (f *SnapshotFile) Encode() string {
	var b strings.Builder
	b.WriteString(snapshotHeader + "\n")
	b.WriteString("# Generated by Pulse. Commit this file; accept changes with `pulse test -u`.\n")
	names := f.Names()
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(snapshotMarker + name + "\n")
		b.WriteString(f.vals[name])
		b.WriteString("\n")
	}
	return b.String()
}

// Write atomically replaces the snapshot file: temp file in the same
// directory, then rename. A crash never leaves a half-written baseline.
func (f *SnapshotFile) Write(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pulse-snapshot-*")
	if err != nil {
		return fmt.Errorf("engine: create snapshot temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.WriteString(tmp, f.Encode()); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("engine: write snapshot %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("engine: chmod snapshot %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("engine: close snapshot %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("engine: replace snapshot %s: %w", path, err)
	}
	f.path = path
	return nil
}

// SnapshotOptions is one snapshot assertion check.
type SnapshotOptions struct {
	// HTTPFile is the collection file the request came from. The snapshot
	// file is adjacent to it.
	HTTPFile string
	// Request is the request's @name. It is mandatory.
	Request string
	// Body is the live response body.
	Body string
	// Update accepts the live body as the new baseline even when one exists.
	Update bool
}

// CheckSnapshot evaluates one `# @assert snapshot`.
//
// First run (or Update): write the body and pass. Later runs: compare, pass on
// equality, fail with a readable diff otherwise. A malformed snapshot file is
// an error, never a silent overwrite — the file is a baseline, and clobbering
// a corrupted one would erase the evidence of what changed.
func CheckSnapshot(opts SnapshotOptions) (SnapshotResult, error) {
	name := strings.TrimSpace(opts.Request)
	if name == "" {
		return SnapshotResult{}, ErrSnapshotUnnamed
	}
	if strings.TrimSpace(opts.HTTPFile) == "" {
		return SnapshotResult{}, fmt.Errorf("engine: snapshot %q: no collection file", name)
	}
	if len(opts.Body) > MaxCollectedBytes {
		return SnapshotResult{}, fmt.Errorf("%w: %d bytes (limit %d)", ErrSnapshotTooLarge, len(opts.Body), MaxCollectedBytes)
	}
	path := SnapshotPath(opts.HTTPFile)
	file, err := LoadSnapshotFile(path)
	if err != nil {
		return SnapshotResult{}, err
	}
	res := SnapshotResult{Name: name, Path: path}
	want, exists := file.Get(name)
	body := trimSnapshotBody(opts.Body)

	switch {
	case !exists:
		if err := file.Set(name, body); err != nil {
			return SnapshotResult{}, err
		}
		if err := file.Write(path); err != nil {
			return SnapshotResult{}, err
		}
		res.Action, res.OK = SnapshotCreated, true
	case opts.Update:
		if snapshotEqual(want, body) {
			res.Action, res.OK = SnapshotMatched, true
			break
		}
		if err := file.Set(name, body); err != nil {
			return SnapshotResult{}, err
		}
		if err := file.Write(path); err != nil {
			return SnapshotResult{}, err
		}
		res.Action, res.OK = SnapshotUpdated, true
	case snapshotEqual(want, body):
		res.Action, res.OK = SnapshotMatched, true
	default:
		res.Action, res.OK = SnapshotMismatch, false
		res.Diff = DiffSnapshot(want, body)
	}
	return res, nil
}

// snapshotEqual is semantic equality: byte-identical, or two JSON documents
// with the same structure. Reformatting a JSON response must not fail a
// snapshot, and it must not report a mismatch with an empty diff.
func snapshotEqual(a, b string) bool {
	a = trimSnapshotBody(a)
	b = trimSnapshotBody(b)
	if a == b {
		return true
	}
	aj, aok := decodeJSON(a)
	bj, bok := decodeJSON(b)
	if !aok || !bok {
		return false
	}
	return reflect.DeepEqual(aj, bj)
}

// DiffSnapshot renders a readable difference between a stored and a live body.
// Both JSON documents are compared structurally; anything else is compared
// line by line.
func DiffSnapshot(want, got string) string {
	want = trimSnapshotBody(want)
	got = trimSnapshotBody(got)
	if want == got {
		return ""
	}
	wantJSON, wantOK := decodeJSON(want)
	gotJSON, gotOK := decodeJSON(got)
	var lines []string
	if wantOK && gotOK {
		diffJSON(wantJSON, gotJSON, "$", &lines)
	} else {
		lines = diffLines(want, got)
	}
	if len(lines) == 0 {
		// Structurally equal but byte-different (whitespace/key order) is
		// not a failure for JSON. The caller compares normalized bodies,
		// so this can only be reached for a non-JSON pair whose line diff
		// found nothing, which means they are equal.
		return ""
	}
	if len(lines) > MaxDiffLines {
		dropped := len(lines) - MaxDiffLines
		lines = lines[:MaxDiffLines]
		lines = append(lines, fmt.Sprintf("… (%d more differences)", dropped))
	}
	return strings.Join(lines, "\n")
}

// decodeJSON parses exactly one JSON value. Trailing garbage means "not JSON",
// which routes the diff to the line path instead of silently ignoring it.
func decodeJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// diffJSON walks two decoded values and records path-qualified differences.
func diffJSON(want, got any, path string, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: snapshot %s, live %s", path, kindOf(want), kindOf(got)))
			return
		}
		keys := make([]string, 0, len(w)+len(g))
		seen := make(map[string]bool, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
			seen[k] = true
		}
		for k := range g {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			wv, inWant := w[k]
			gv, inGot := g[k]
			child := path + "." + k
			switch {
			case inWant && !inGot:
				*out = append(*out, fmt.Sprintf("%s: missing from the live response (snapshot had %s)", child, compactJSON(wv)))
			case !inWant && inGot:
				*out = append(*out, fmt.Sprintf("%s: unexpected in the live response (%s)", child, compactJSON(gv)))
			default:
				diffJSON(wv, gv, child, out)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: snapshot %s, live %s", path, kindOf(want), kindOf(got)))
			return
		}
		n := len(w)
		if len(g) > n {
			n = len(g)
		}
		for i := 0; i < n; i++ {
			child := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(g):
				*out = append(*out, fmt.Sprintf("%s: missing from the live response (snapshot had %s)", child, compactJSON(w[i])))
			case i >= len(w):
				*out = append(*out, fmt.Sprintf("%s: unexpected in the live response (%s)", child, compactJSON(g[i])))
			default:
				diffJSON(w[i], g[i], child, out)
			}
		}
	default:
		if !jsonEqual(want, got) {
			*out = append(*out, fmt.Sprintf("%s: snapshot %s, live %s", path, compactJSON(want), compactJSON(got)))
		}
	}
}

// jsonEqual compares two decoded scalars without coercion: `1` and `"1"` are
// different, and so are `1` and `1.0` (json.Number compares as text).
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av.String() == bv.String()
	default:
		return false
	}
}

// kindOf names a decoded value's shape for a type-change line.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "value"
	}
}

// compactJSON renders one value on one line, capped so a huge nested value
// cannot take over the diff.
func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(raw)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// diffLines renders a line diff, trimming the common prefix and suffix and
// showing the changed middle with unified-style markers.
func diffLines(want, got string) []string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	if want == "" {
		w = nil
	}
	if got == "" {
		g = nil
	}
	pre := 0
	for pre < len(w) && pre < len(g) && w[pre] == g[pre] {
		pre++
	}
	suf := 0
	for suf < len(w)-pre && suf < len(g)-pre && w[len(w)-1-suf] == g[len(g)-1-suf] {
		suf++
	}
	var out []string
	out = append(out, fmt.Sprintf("@@ snapshot lines %d..%d, live lines %d..%d @@",
		pre+1, len(w)-suf, pre+1, len(g)-suf))
	for _, line := range w[pre : len(w)-suf] {
		out = append(out, "- "+line)
	}
	for _, line := range g[pre : len(g)-suf] {
		out = append(out, "+ "+line)
	}
	return out
}

// trimSnapshotBody normalises line endings and trims the blank lines the file
// envelope itself uses. Interior blank lines are preserved.
func trimSnapshotBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	s = strings.TrimPrefix(s, "\n")
	return s
}

// validateSnapshotBody refuses a body the envelope cannot represent.
func validateSnapshotBody(body string) error {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, snapshotMarker) {
			return fmt.Errorf("body contains the snapshot namespace marker %q", snapshotMarker)
		}
	}
	return nil
}
