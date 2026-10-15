package ast

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrRequestNotFound reports an upsert/delete naming a request the file does
// not contain. It never echoes file content — only the missing name.
var ErrRequestNotFound = errors.New("ast: request not found")

// ErrPathEscape reports a workspace-relative path that reaches outside the
// root (`..` traversal, an absolute path elsewhere, or a symlink escape).
var ErrPathEscape = errors.New("ast: path escapes workspace root")

// ErrFileExists reports CreateCollectionFile on a path that already exists:
// creation never truncates; updates go through UpsertRequestInFile.
var ErrFileExists = errors.New("ast: collection file already exists")

// jailPath resolves rel inside root and fails closed on any escape: `..`
// that climbs out, absolute paths outside root, NUL bytes, and symlink
// escapes (both roots and targets EvalSymlinked before comparison). The
// returned path is absolute and confined.
func jailPath(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, rel)
	}
	if filepath.IsAbs(rel) {
		// Absolute paths are allowed only when already confined.
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("ast: resolve workspace: %w", err)
		}
		if err := checkConfined(rootAbs, rel); err != nil {
			return "", err
		}
		return rel, nil
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("ast: resolve workspace: %w", err)
	}
	abs := filepath.Join(rootAbs, clean)
	return abs, checkConfined(rootAbs, abs)
}

// checkConfined EvalSymlinks the target (or its parent when the target does
// not exist yet, so creation cannot plant a name inside a symlinked dir) and
// requires it to sit inside the EvalSymlinked root.
func checkConfined(rootAbs, abs string) error {
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("ast: resolve workspace: %w", err)
	}
	// Climb to the nearest existing ancestor: missing components cannot be
	// symlinks, so evaluating the ancestor and rejoining the remainder
	// lexically is exact (EvalSymlinks on a not-yet-created nested path
	// would fail and wrongly refuse every creation).
	anchor := abs
	for {
		if _, err := os.Lstat(anchor); err == nil {
			break
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return fmt.Errorf("ast: resolve path: %w", err)
		}
		anchor = parent
	}
	anchorEval, err := filepath.EvalSymlinks(anchor)
	if err != nil {
		return fmt.Errorf("ast: resolve path: %w", err)
	}
	remainder, err := filepath.Rel(anchor, abs)
	if err != nil {
		return fmt.Errorf("ast: resolve path: %w", err)
	}
	targetEval := filepath.Join(anchorEval, remainder)
	if targetEval != rootEval && !strings.HasPrefix(targetEval, rootEval+string(filepath.Separator)) {
		disp := abs
		if rel, err := filepath.Rel(rootAbs, abs); err == nil {
			disp = rel
		}
		return fmt.Errorf("%w: %q", ErrPathEscape, disp)
	}
	return nil
}

// atomicWrite persists data crash-safely: temp file in the same directory
// (same filesystem, so rename is atomic), fsync, then os.Rename. A crash
// never leaves a truncated collection behind.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".pulse-tmp-*")
	if err != nil {
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("ast: write %s: %w", path, err)
	}
	return nil
}

// parseBuffer validates content before it ever reaches disk: only files the
// parser accepts are written.
func parseBuffer(path string, src []byte) (*Document, error) {
	arena := AcquireArena()
	arena.Reset(src)
	doc, err := Parse(arena)
	if err != nil {
		arena.Release()
		if se, ok := err.(*SyntaxError); ok {
			se.Path = path
		}
		return nil, err
	}
	return doc, nil
}

// CreateCollectionFile writes content as a new `.http` file under root.
// Existing files are never truncated (see UpsertRequestInFile); parents are
// created inside the jail. Content must parse.
func CreateCollectionFile(root, rel string, content []byte) error {
	abs, err := jailPath(root, rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err == nil {
		return fmt.Errorf("%w: %q", ErrFileExists, rel)
	}
	doc, err := parseBuffer(rel, content)
	if err != nil {
		return err
	}
	doc.Release()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("ast: create %s: %w", rel, err)
	}
	return atomicWrite(abs, content)
}

// lineStarts returns the offset of every 1-based line start in src, handling
// `\n`, `\r`, and `\r\n` (the lexer's isNewline admits `\r` too, so the
// splicer must agree on line boundaries).
func lineStarts(src []byte) []int32 {
	ls := []int32{0}
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\r':
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			ls = append(ls, int32(i+1))
		case '\n':
			ls = append(ls, int32(i+1))
		}
	}
	return ls
}

// isSeparatorLine reports a line that opens with `###` after spaces/tabs —
// the exact condition lexLineStart tests before emitting TokSeparator.
func isSeparatorLine(line []byte) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i+3 <= len(line) && line[i] == '#' && line[i+1] == '#' && line[i+2] == '#'
}

// lineEnd returns the exclusive end of the line containing off (stops before
// any `\r` or `\n`).
func lineEnd(src []byte, off int32) int32 {
	e := off
	for e < int32(len(src)) && src[e] != '\n' && src[e] != '\r' {
		e++
	}
	return e
}

// trimRightNewlines strips trailing line breaks so spliced regions join
// without accumulating blank lines.
func trimRightNewlines(src []byte, e int32) int32 {
	for e > 0 && (src[e-1] == '\n' || src[e-1] == '\r') {
		e--
	}
	return e
}

// offsetLine maps a byte offset to its 1-based line number via binary search.
func offsetLine(ls []int32, off int32) int32 {
	lo, hi := int32(0), int32(len(ls)-1)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ls[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

// reqSpan is the byte range owned by one request: from the first line of its
// leading attached directives (name, asserts, wasm path, in-block vars) to
// the end of its body — or, without a body, the end of its last header or
// method line.
type reqSpan struct {
	start, end int32
}

// requestSpans computes every request's owned range. Directive attachment is
// read from the parsed AST (name/assert/vars carry line numbers; the wasm
// path resolves through the line table), so file-scoped `@var` definitions
// and free comments are never absorbed into a request's region.
func requestSpans(doc *Document, src []byte, ls []int32) []reqSpan {
	spans := make([]reqSpan, 0, doc.Len())
	idx := 0
	for r := range doc.Requests() {
		minLine := r.Line
		if r.NameStart >= 0 {
			if l := offsetLine(ls, r.NameStart); l < minLine {
				minLine = l
			}
		}
		for _, a := range doc.AssertsOf(r) {
			if a.Line < minLine {
				minLine = a.Line
			}
		}
		for _, v := range doc.VarsOf(r) {
			if v.Line < minLine {
				minLine = v.Line
			}
		}
		if r.WasmSignStart >= 0 {
			if l := offsetLine(ls, r.WasmSignStart); l < minLine {
				minLine = l
			}
		}
		start := ls[minLine-1]
		var end int32
		if r.Body.Kind != BodyNone {
			end = trimRightNewlines(src, r.Body.End)
		} else {
			lastLine := r.Line
			for _, h := range r.Headers {
				if h.Line > lastLine {
					lastLine = h.Line
				}
			}
			// Header.Line is stamped after the value tokens advance, so it
			// may sit on the terminator line; clamp into range.
			if lastLine > int32(len(ls)) {
				lastLine = int32(len(ls))
			}
			end = lineEnd(src, ls[lastLine-1])
		}
		spans = append(spans, reqSpan{start: start, end: end})
		idx++
	}
	return spans
}

// section is one `###`-delimited region: head has no leading separator
// (sepStart < 0); every other section starts at its separator line.
type section struct {
	start, end int32
	sepStart   int32
}

// splitSections cuts src at separator lines using the lexer's own rule, so
// the splicer and the parser always agree on block boundaries.
func splitSections(src []byte, ls []int32) []section {
	var secs []section
	start := int32(0)
	sep := int32(-1)
	flush := func(end int32) {
		secs = append(secs, section{start: start, end: end, sepStart: sep})
	}
	for li, lstart := range ls {
		var lend int32
		if li+1 < len(ls) {
			lend = ls[li+1]
		} else {
			lend = int32(len(src))
		}
		if lend > lstart && isSeparatorLine(src[lstart:lend]) {
			flush(trimRightNewlines(src, lstart))
			start = lend
			sep = lstart
		}
	}
	flush(trimRightNewlines(src, int32(len(src))))
	return secs
}

// findRequest returns the index of the request named name (exact match;
// unnamed requests never match, and an empty name fails closed).
func findRequest(doc *Document, name string) (int, error) {
	if name == "" {
		return -1, fmt.Errorf("ast: upsert/delete requires a request name: %w", ErrRequestNotFound)
	}
	idx := 0
	for r := range doc.Requests() {
		if doc.RequestName(r) == name {
			return idx, nil
		}
		idx++
	}
	return -1, fmt.Errorf("ast: %w: %q", ErrRequestNotFound, name)
}

// UpsertRequestInFile replaces the named request's owned span with block, or
// appends block as a new `###` section when the name is absent. Everything
// outside the span — neighboring blocks, file-level comments and `@var`
// definitions, disabled headers — is preserved byte-identical. A missing file
// is created. Block must parse as a single request; the spliced result is
// re-validated before it reaches disk.
func UpsertRequestInFile(root, rel, name string, block []byte) error {
	if name == "" {
		return fmt.Errorf("ast: upsert requires a request name: %w", ErrRequestNotFound)
	}
	bdoc, err := parseBuffer(rel, block)
	if err != nil {
		return err
	}
	if bdoc.Len() != 1 {
		bdoc.Release()
		return fmt.Errorf("ast: upsert block must hold exactly one request, found %d", bdoc.Len())
	}
	bdoc.Release()

	abs, err := jailPath(root, rel)
	if err != nil {
		return err
	}
	trimmed := block[:trimRightNewlines(block, int32(len(block)))]
	raw, rerr := os.ReadFile(abs)
	if rerr != nil {
		if !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("ast: read %s: %w", rel, rerr)
		}
		out := append(append([]byte(nil), trimmed...), '\n')
		if _, err := parseBuffer(rel, out); err != nil {
			return err
		}
		return atomicWrite(abs, out)
	}
	doc, err := parseBuffer(rel, raw)
	if err != nil {
		return err
	}
	defer doc.Release()

	ls := lineStarts(raw)
	secs := splitSections(raw, ls)
	spans := requestSpans(doc, raw, ls)

	idx, ferr := findRequest(doc, name)
	var out []byte
	if ferr != nil {
		out = appendAppend(raw, secs, trimmed)
	} else {
		out = spliceReplace(raw, secs, spans, idx, trimmed)
	}
	if _, err := parseBuffer(rel, out); err != nil {
		return fmt.Errorf("ast: upsert produced invalid file: %w", err)
	}
	return atomicWrite(abs, out)
}

// appendAppend adds block as a trailing `###` section (or the whole file when
// nothing is there yet — vars-only files keep their file-scoped definitions
// by position either way).
func appendAppend(raw []byte, secs []section, block []byte) []byte {
	end := trimRightNewlines(raw, int32(len(raw)))
	out := make([]byte, 0, len(raw)+len(block)+8)
	out = append(out, raw[:end]...)
	if end > 0 {
		out = append(out, '\n')
		// A separator is required unless the file holds no request yet: vars
		// before the first request stay file-scoped by position.
		if fileHasRequest(secs, raw) {
			out = append(out, "###\n"...)
		}
	}
	out = append(out, block...)
	out = append(out, '\n')
	return out
}

// fileHasRequest reports whether any section already owns request content.
// Sections are separator-delimited; the head counts when it holds more than
// variables and comments. Determined textually: a section owns a request when
// it contains a line starting (after space/tab) with an uppercase method word
// followed by space, tab, or end of line.
func fileHasRequest(secs []section, raw []byte) bool {
	for _, s := range secs {
		if sectionOwnsRequest(raw[s.start:s.end]) {
			return true
		}
	}
	return false
}

// sectionOwnsRequest scans one section's lines for a method line.
func sectionOwnsRequest(sec []byte) bool {
	for _, l := range splitRawLines(sec) {
		i := 0
		for i < len(l) && (l[i] == ' ' || l[i] == '\t') {
			i++
		}
		j := i
		for j < len(l) && l[j] >= 'A' && l[j] <= 'Z' {
			j++
		}
		if j > i && (j == len(l) || l[j] == ' ' || l[j] == '\t') {
			switch string(l[i:j]) {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
				return true
			}
		}
	}
	return false
}

// splitRawLines cuts on \n, trimming a trailing \r per line.
func splitRawLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			lines = append(lines, bytesTrimCR(b[start:i]))
			start = i + 1
		}
	}
	lines = append(lines, bytesTrimCR(b[start:]))
	return lines
}

func bytesTrimCR(l []byte) []byte {
	if len(l) > 0 && l[len(l)-1] == '\r' {
		return l[:len(l)-1]
	}
	return l
}

// spliceReplace swaps spans[idx] for block. The replacement limit is the next
// request's start inside the same section, else the section end — so adjacent
// requests in one section survive untouched.
func spliceReplace(raw []byte, secs []section, spans []reqSpan, idx int, block []byte) []byte {
	start := spans[idx].start
	limit := int32(len(raw))
	// Owning section end caps the region.
	for _, s := range secs {
		if s.start <= start && start < s.end || s.start == start {
			limit = s.end
			break
		}
		if s.start > start {
			break
		}
	}
	// A following request in the same section caps it tighter.
	for j := idx + 1; j < len(spans); j++ {
		if spans[j].start > start && spans[j].start <= limit {
			limit = spans[j].start
			break
		}
	}
	out := make([]byte, 0, len(raw)+len(block))
	out = append(out, raw[:start]...)
	out = append(out, block...)
	out = append(out, '\n')
	out = append(out, raw[trimLeftNewlines(raw, limit):]...)
	return out
}

// trimLeftNewlines skips leading line breaks so the join leaves no blank gap.
func trimLeftNewlines(raw []byte, s int32) int32 {
	for s < int32(len(raw)) && (raw[s] == '\n' || raw[s] == '\r') {
		s++
	}
	return s
}

// DeleteRequestFromFile removes the named request's owned span. Whole
// single-request sections (separator included) vanish; file-level variables,
// comments, and neighboring requests survive byte-identical. The result is
// re-validated before it reaches disk.
func DeleteRequestFromFile(root, rel, name string) error {
	abs, err := jailPath(root, rel)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("ast: read %s: %w", rel, err)
	}
	doc, err := parseBuffer(rel, raw)
	if err != nil {
		return err
	}
	defer doc.Release()

	idx, err := findRequest(doc, name)
	if err != nil {
		return err
	}
	ls := lineStarts(raw)
	secs := splitSections(raw, ls)
	spans := requestSpans(doc, raw, ls)

	out := spliceDelete(raw, secs, spans, idx)
	if _, err := parseBuffer(rel, out); err != nil {
		return fmt.Errorf("ast: delete produced invalid file: %w", err)
	}
	return atomicWrite(abs, out)
}

// spliceDelete removes spans[idx]. When the request is alone in a
// non-head section, the section's separator line goes with it; otherwise only
// the owned span is cut, capped by the next request or the section end.
func spliceDelete(raw []byte, secs []section, spans []reqSpan, idx int) []byte {
	start, limit := spans[idx].start, int32(len(raw))
	ownSec := -1
	for si, s := range secs {
		if s.start <= start && (start < s.end || s.start == start && s.end == start || start == s.end && s.end == int32(len(raw))) {
			ownSec = si
			limit = s.end
			break
		}
		if s.start > start {
			break
		}
	}
	for j := idx + 1; j < len(spans); j++ {
		if spans[j].start > start && spans[j].start <= limit {
			limit = spans[j].start
			break
		}
	}
	cut := start
	if ownSec > 0 {
		alone := true
		for j := range spans {
			if j != idx && spans[j].start >= secs[ownSec].start && spans[j].start < secs[ownSec].end {
				alone = false
				break
			}
		}
		if alone && limit == secs[ownSec].end {
			cut = secs[ownSec].sepStart
		}
	}
	out := make([]byte, 0, len(raw))
	out = append(out, raw[:cut]...)
	out = append(out, raw[trimLeftNewlines(raw, limit):]...)
	end := trimRightNewlines(out, int32(len(out)))
	out = out[:end]
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}
