package rag

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// Chunking policy. These are the numbers the whole heuristic is reasoned about,
// so they are constants rather than literals scattered through the code.
//
//	WindowLines   a decl longer than this is split
//	OverlapLines  adjacent windows share this many lines
//
// The overlap is not decoration. Without it a defect on the boundary line —
// the line that names the nil pointer, or the `}` that closes a function —
// lands in neither window when the split point moves, and the retrieval then
// ranks everything except the answer.
const (
	WindowLines  = 64
	OverlapLines = 16

	// maxDocBytes bounds what the Go parser is handed. go/parser builds a full
	// AST, which costs many times the source size; an unbounded file would
	// blow the memory budget on its own. Above this a file falls back to the
	// line-window chunker, which is cheap and never fails.
	maxDocBytes = 1 << 20 // 1 MiB
)

// chunkFile turns one file's bytes into chunks. It never returns an empty slice
// for a non-empty input: a file that produced no chunk means a bug, and the
// caller reports it rather than silently indexing nothing.
func chunkFile(rel, lang string, src []byte) ([]Chunk, error) {
	switch lang {
	case LangGo:
		return chunkGo(rel, src)
	case LangPy:
		return chunkPy(rel, src)
	case LangAstro:
		return chunkAstro(rel, src)
	}
	return nil, &CorpusError{Path: rel, Err: ErrUnsupportedLang(lang)}
}

// ErrUnsupportedLang is returned for a language with no chunker.
type ErrUnsupportedLang string

func (e ErrUnsupportedLang) Error() string { return "rag: no chunker for language " + string(e) }

// ---- Go ---------------------------------------------------------------------

// chunkGo cuts at top-level declaration boundaries using the standard library's
// own parser, so the boundaries are the compiler's boundaries rather than a
// guess: a `func`, a `type`, a `var`/`const`/`import` group.
//
// Using go/parser rather than scanning for the word `func` is not a
// sophistication preference. A `func` inside a string literal, a comment, or a
// nested function literal would each start a bogus chunk under scanning, and a
// chunk that begins mid-expression is worse than no chunk — it retrieves
// confidently and wrongly.
func chunkGo(rel string, src []byte) ([]Chunk, error) {
	if len(src) == 0 {
		return nil, &CorpusError{Path: rel, Err: ErrCorruptChunk}
	}
	if len(src) > maxDocBytes {
		return windowChunks(rel, LangGo, "", src), nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// A file that does not compile is still worth indexing — a stack trace
		// naming an identifier is exactly the case where the file is broken.
		// Fall back to line windows rather than losing it.
		return windowChunks(rel, LangGo, "", src), nil
	}

	base := fset.File(file.Pos()).Base()
	lines := lineIndex(src)

	var out []Chunk
	for _, decl := range file.Decls {
		end := fset.Position(decl.End()).Line - base

		// A declaration's Doc attaches to the DECLARATION, not to its first
		// statement: decl.Pos() is the `func` keyword, which is several lines
		// below the comment. Starting there drops the doc comment from every
		// chunk — and the doc comment is half of what makes a retrieved chunk
		// explain anything. Doc can be nil for a declaration with no comment.
		start := end
		if d := declDoc(decl); d != nil {
			start = fset.Position(d.Pos()).Line - base
		} else {
			start = fset.Position(decl.Pos()).Line - base
		}
		if end < start {
			start = end
		}
		text, s0, e0 := sliceLines(lines, src, start, end)
		if len(strings.TrimSpace(text)) == 0 {
			continue
		}
		symbol := goSymbol(decl)

		// A long declaration is windowed so no single chunk exceeds what a
		// model can hold and no single chunk dominates the ranking. The windows
		// are taken over THIS declaration's span, not the whole file: windowing
		// the file would re-emit every short declaration once per window and
		// produce N copies of the same chunk id.
		if e0-s0+1 > WindowLines {
			out = append(out, windowLines(rel, LangGo, symbol, src, lines, s0, e0)...)
			continue
		}
		out = append(out, Chunk{
			Path: rel, Lang: LangGo, Symbol: symbol,
			StartLn: s0 + 1, EndLn: e0 + 1, Text: text,
		})
	}

	// A file consisting only of a package clause and comments yields no decls.
	// Index it anyway rather than letting the file vanish from the index.
	if len(out) == 0 {
		out = windowChunks(rel, LangGo, "", src)
	}
	return out, nil
}

// declDoc returns a declaration's doc comment group, or nil when it has none.
func declDoc(decl ast.Decl) *ast.CommentGroup {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}

// windowLines windows a byte-offset source over the inclusive line span
// [start, end].
func windowLines(rel, lang, symbol string, src []byte, lines []int, start, end int) []Chunk {
	var out []Chunk
	i := start
	for i <= end {
		j := min(i+WindowLines-1, end)
		text, s0, e0 := sliceLines(lines, src, i, j)
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{
				Path: rel, Lang: lang, Symbol: symbol,
				StartLn: s0 + 1, EndLn: e0 + 1, Text: text,
			})
		}
		if j >= end {
			break
		}
		next := j - OverlapLines + 1
		if next <= i { // a short window guarantees progress
			next = i + 1
		}
		i = next
	}
	return out
}

// goSymbol names a declaration, preferring the receiver-qualified form because
// two methods named `Serve` on different types are different bugs.
func goSymbol(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		name := d.Name.Name
		if d.Recv != nil && len(d.Recv.List) > 0 {
			var buf strings.Builder
			buf.WriteByte('(')
			writeExpr(&buf, d.Recv.List[0].Type)
			buf.WriteString(").")
			buf.WriteString(name)
			return buf.String()
		}
		return name
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			if len(d.Specs) > 0 {
				if ts, ok := d.Specs[0].(*ast.TypeSpec); ok {
					if ts.Name != nil {
						return ts.Name.Name
					}
				}
			}
			return "type"
		case token.CONST:
			return "const"
		case token.VAR:
			return "var"
		case token.IMPORT:
			return "import"
		}
	}
	return ""
}

// writeExpr renders a type expression compactly enough for a symbol name.
func writeExpr(buf *strings.Builder, e ast.Expr) {
	switch t := e.(type) {
	case *ast.Ident:
		buf.WriteString(t.Name)
	case *ast.StarExpr:
		buf.WriteByte('*')
		writeExpr(buf, t.X)
	case *ast.SelectorExpr:
		writeExpr(buf, t.X)
		buf.WriteByte('.')
		buf.WriteString(t.Sel.Name)
	case *ast.IndexExpr:
		writeExpr(buf, t.X)
	case *ast.IndexListExpr:
		writeExpr(buf, t.X)
	default:
		buf.WriteString("?")
	}
}

// ---- Python -----------------------------------------------------------------

// chunkPy cuts Python at indentation-defined blocks.
//
// There is no Python parser in the Go standard library and adding one is a
// dependency the stdlib-first rule (AGENTS.md §2.2) would rather avoid for a
// heuristic that does not need exactness. Indentation IS the language's own
// block structure, so reading it directly is faithful to the language rather
// than an approximation of it.
//
// The rules, in order: a chunk begins at a line that starts a `def`, `async
// def`, or `class` at any indentation, OR at its decorators; a `def` nested
// inside another `def` starts its own chunk, because a nested closure is where
// bugs like to live and is exactly what a stack frame names.
func chunkPy(rel string, src []byte) ([]Chunk, error) {
	rows := strings.Split(string(src), "\n")
	if len(rows) == 0 {
		return nil, &CorpusError{Path: rel, Err: ErrCorruptChunk}
	}

	// Mark the lines that BEGIN a block.
	//
	// Indentation is the structure, not a filter on top of it. A `def` at
	// column 4 is as much a block boundary as one at column 0 — filtering to
	// column 0 only would emit the entire class body as ONE chunk, which
	// retrieves the class when the stack trace named a method.
	start := make([]bool, len(rows))
	for i, raw := range rows {
		trimmed := strings.TrimSpace(stripPyComment(raw))
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "def "),
			strings.HasPrefix(trimmed, "async def "),
			strings.HasPrefix(trimmed, "class "),
			strings.HasPrefix(trimmed, "@"):
			start[i] = true
		}
	}

	var out []Chunk
	i := 0
	for i < len(rows) {
		if !start[i] {
			i++
			continue
		}

		// The block that a `def`/`class` OPENS is scanned for nested blocks
		// first. A top-level `class` has indent 0, so the dedent rule alone
		// would let it run to end of file — one chunk for the entire module,
		// which retrieves nothing specific. The nested blocks are emitted before
		// their parent so a short parent is not windowed into duplicates, and
		// each nested block carries its own symbol, which is what a stack frame
		// names.
		bodyStart := i
		if isDecoratorRow(rows[i]) {
			j := i + 1
			for j < len(rows) && strings.TrimSpace(stripPyComment(rows[j])) == "" {
				j++
			}
			if j < len(rows) {
				bodyStart = j
			}
		}

		end := pyBlockEnd(rows, bodyStart)
		emitPyBlock(rel, rows, start, i, end, pyBlockSymbol(rows, i, end), &out)

		// Advance past the emitted block.
		i = end
		if i < len(rows) && !start[i] && strings.TrimSpace(stripPyComment(rows[i])) == "" {
			i++
		}
	}

	if len(out) == 0 {
		out = windowChunks(rel, LangPy, "", src)
	}
	return out, nil
}

func isDecoratorRow(row string) bool {
	return strings.HasPrefix(strings.TrimSpace(stripPyComment(row)), "@")
}

// pyBlockSymbol resolves the name of a block whose first row may be a
// decorator: the name lives on the declaration row below it, not on the
// decorator.
func pyBlockSymbol(rows []string, start, end int) string {
	if !isDecoratorRow(rows[start]) {
		return pySymbol(rows[start])
	}
	for k := start + 1; k < end; k++ {
		if strings.TrimSpace(stripPyComment(rows[k])) == "" {
			continue
		}
		return pySymbol(rows[k])
	}
	return ""
}

// emitPyBlock writes the chunks for the block rows[start:end].
//
// Nested blocks are emitted FIRST and are excluded from the parent's span. That
// ordering is what makes a class produce one chunk per method rather than one
// chunk containing every method: the parent then holds only its header and any
// statements that are not inside a nested def, which is what actually
// identifies the class.
func emitPyBlock(rel string, rows []string, allStart []bool, start, end int, symbol string, out *[]Chunk) {
	baseIndent := indentOf(rows[start])

	var nested []int
	for k := start + 1; k < end; k++ {
		if allStart[k] && indentOf(rows[k]) > baseIndent {
			nested = append(nested, k)
		}
	}

	// Walk [start,end) once, emitting each maximal segment of this block's own
	// lines and delegating each nested block to its own recursion. The loop is
	// explicit rather than a mutually-recursive partition because a recursive
	// partition has to find "cuts strictly inside this segment" from the full
	// nested list on every call, and a nested block that starts before the
	// segment it belongs to makes that search re-derive cuts forever.
	var cursor = start
	for _, n := range nested {
		if n < cursor || n >= end {
			continue
		}
		emitPySegment(rel, rows, cursor, n, symbol, out)
		nEnd := pyBlockEnd(rows, n)
		emitPyBlock(rel, rows, allStart, n, nEnd, pyBlockSymbol(rows, n, nEnd), out)
		cursor = nEnd
	}
	emitPySegment(rel, rows, cursor, end, symbol, out)
}

// emitPySegment writes the chunk for rows[from:to], windowing it if long.
func emitPySegment(rel string, rows []string, from, to int, symbol string, out *[]Chunk) {
	if to <= from {
		return
	}
	text := strings.Join(rows[from:to], "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	if to-from > WindowLines {
		*out = append(*out, windowChunkRange(rel, LangPy, symbol, rows, from, to)...)
		return
	}
	*out = append(*out, Chunk{
		Path: rel, Lang: LangPy, Symbol: symbol,
		StartLn: from + 1, EndLn: to, Text: text,
	})
}

// pyBlockEnd returns the index one past the end of the block opened at row i.
//
// The block ends at the next content-bearing line that is EITHER dedented
// below the block's own indentation OR at the SAME indentation and starts a new
// block. The second condition is the one that matters and is easy to miss: a
// top-level `class` has indent 0, so "lower indentation" can never occur, and
// the class would swallow the entire rest of the file — one chunk for the whole
// module, which retrieves nothing specific.
func pyBlockEnd(rows []string, i int) int {
	baseIndent := indentOf(rows[i])
	j := i + 1
	for j < len(rows) {
		if strings.TrimSpace(stripPyComment(rows[j])) == "" {
			j++
			continue
		}
		ind := indentOf(rows[j])
		if ind < baseIndent {
			break
		}
		if ind == baseIndent && startsPyBlock(rows[j]) {
			break
		}
		j++
	}
	return j
}

// startsPyBlock reports whether a line opens a new Python block.
func startsPyBlock(row string) bool {
	t := strings.TrimSpace(stripPyComment(row))
	return strings.HasPrefix(t, "def ") ||
		strings.HasPrefix(t, "async def ") ||
		strings.HasPrefix(t, "class ") ||
		strings.HasPrefix(t, "@")
}

// pyChunk builds one chunk over rows[start:end] under the given symbol.
func pyChunk(rel string, rows []string, start, end int, symbol string) Chunk {
	return Chunk{
		Path: rel, Lang: LangPy, Symbol: symbol,
		StartLn: start + 1, EndLn: end, Text: strings.Join(rows[start:end], "\n"),
	}
}

// pySymbol reads the name out of a def/class/decorator line.
//
// A decorator row yields "" because the NAME is on the next line; the caller
// that pairs a decorator with its declaration reads the symbol from there.
func pySymbol(row string) string {
	t := strings.TrimSpace(stripPyComment(row))
	if t == "" || isDecoratorRow(row) {
		return ""
	}
	fields := strings.FieldsFunc(t, func(r rune) bool { return r == ' ' || r == '(' || r == '\t' })
	for _, f := range fields {
		switch f {
		case "def", "async", "class", "@":
			continue
		}
		return strings.Trim(f, ":")
	}
	return ""
}

// stripPyComment removes a trailing `#` comment, respecting string literals.
// A `#` inside a string is data; treating it as a comment would misreport a
// block's extent.
func stripPyComment(s string) string {
	var inS, inD, tick bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && (inS || inD || tick):
			i++
		case c == '\'' && !inD && !tick:
			inS = !inS
		case c == '"' && !inS && !tick:
			inD = !inD
		case c == '`' && !inS && !inD:
			tick = !tick
		case c == '#' && !inS && !inD && !tick:
			return s[:i]
		}
	}
	return s
}

func indentOf(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
			continue
		}
		if r == '\t' {
			n += 4 - n%4 // tabs are 8 in Python; 4 is close enough for boundaries
			continue
		}
		break
	}
	return n
}

// ---- Astro ------------------------------------------------------------------

// chunkAstro cuts an Astro component at its structural fences: front matter,
// `<script>`, `<style>`, and the template body.
//
// Astro is HTML with islands, so "functional boundary" means the blocks a
// component author also thinks of as units. Treating the file as one template
// blob would make every retrieval return the whole component, which is the
// failure mode chunking exists to prevent.
func chunkAstro(rel string, src []byte) ([]Chunk, error) {
	rows := strings.Split(string(src), "\n")
	if len(rows) == 0 {
		return nil, &CorpusError{Path: rel, Err: ErrCorruptChunk}
	}

	var out []Chunk
	inFence, inScript, inStyle := false, false, false
	open := 0
	var kind string

	// flush closes the CURRENT fenced block. It is a no-op when kind is empty,
	// which is the state between blocks: without that guard the trailing flush
	// would emit the template under whatever the last block's label was, so the
	// template would appear twice — once mislabelled as script, once as
	// template — and a search could return either.
	flush := func(end int) {
		if kind == "" || end <= open {
			return
		}
		text := strings.Join(rows[open:end], "\n")
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{
				Path: rel, Lang: LangAstro, Symbol: kind,
				StartLn: open + 1, EndLn: end, Text: text,
			})
		}
	}

	for i, raw := range rows {
		t := strings.TrimSpace(raw)
		switch {
		case !inFence && t == "---":
			flush(i)
			kind = "frontmatter"
			open, inFence = i, true
		case inFence && t == "---":
			flush(i + 1)
			open, inFence, kind = i+1, false, ""
		case !inScript && hasTagOpen(t, "script"):
			flush(i)
			kind = "script"
			open, inScript = i, true
		case inScript && strings.Contains(t, "</script"):
			flush(i + 1)
			open, inScript, kind = i+1, false, ""
		case !inStyle && hasTagOpen(t, "style"):
			flush(i)
			kind = "style"
			open, inStyle = i, true
		case inStyle && strings.Contains(t, "</style"):
			flush(i + 1)
			open, inStyle, kind = i+1, false, ""
		}
	}
	flush(len(rows)) // unterminated tail: an authoring mistake, not our problem

	// Whatever is between the fences is the template. Window it.
	for i := open; i < len(rows); {
		j := min(i+WindowLines, len(rows))
		text := strings.Join(rows[i:j], "\n")
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{
				Path: rel, Lang: LangAstro, Symbol: "template",
				StartLn: i + 1, EndLn: j, Text: text,
			})
		}
		if j-overlap(i) >= len(rows) {
			break
		}
		i = j - OverlapLines
		if i < 0 {
			i = 0
		}
		if j <= i { // pathological tiny file: guarantee progress
			i = j
		}
	}

	if len(out) == 0 {
		out = windowChunks(rel, LangAstro, "", src)
	}
	return out, nil
}

// hasTagOpen reports a line that OPENS an HTML tag with the given name. It
// avoids matching a closing tag, an attribute value containing "script", or a
// self-closing tag.
//
// The check requires the tag name to END at the line's boundary. Without it,
// `<scripted>` would match the `script` prefix and swallow the rest of the
// component as script content — a plausible tag name that would silently
// corrupt every chunk after it.
func hasTagOpen(line, tag string) bool {
	lt := strings.IndexByte(line, '<')
	if lt < 0 {
		return false
	}
	rest := strings.TrimPrefix(line[lt+1:], "/")
	if !strings.HasPrefix(rest, tag) {
		return false
	}
	rest = rest[len(tag):]
	if rest == "" {
		return true
	}
	c := rest[0]
	// `>` closes the tag; a space introduces attributes. Anything else means
	// this is a longer tag name that merely starts with `tag`.
	return c == ' ' || c == '\t' || c == '>'
}

func overlap(i int) int { return i - OverlapLines }

// ---- shared line helpers ----------------------------------------------------

// lineIndex maps a 0-based line number to its byte offset in src.
func lineIndex(src []byte) []int {
	offsets := []int{0}
	for i, b := range src {
		if b == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// sliceLines returns the text and 0-based span of lines [start, end] inclusive.
func sliceLines(offsets []int, src []byte, start, end int) (string, int, int) {
	if start < 0 {
		start = 0
	}
	if start >= len(offsets) {
		return "", 0, 0
	}
	if end >= len(offsets) {
		end = len(offsets) - 1
	}
	if end < start {
		return "", 0, 0
	}
	s := offsets[start]
	e := len(src)
	if end+1 < len(offsets) {
		e = offsets[end+1]
	}
	return string(bytes.TrimRight(src[s:e], "\r\n")), start, end
}

// windowChunks splits a whole file into overlapping windows. It is the
// fallback for content no parser could cut, and for oversize declarations.
func windowChunks(rel, lang, symbol string, src []byte) []Chunk {
	return windowChunkRange(rel, lang, symbol, strings.Split(string(src), "\n"), 0, 0)
}

// windowChunkRange windows rows[start:end]; end<=0 means "to the end".
func windowChunkRange(rel, lang, symbol string, rows []string, start, end int) []Chunk {
	if end <= 0 || end > len(rows) {
		end = len(rows)
	}
	if start < 0 {
		start = 0
	}
	if start >= end {
		return nil
	}
	var out []Chunk
	for i := start; i < end; {
		j := min(i+WindowLines, end)
		text := strings.Join(rows[i:j], "\n")
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{
				Path: rel, Lang: lang, Symbol: symbol,
				StartLn: i + 1, EndLn: j, Text: text,
			})
		}
		if j >= end {
			break
		}
		next := j - OverlapLines
		if next <= i { // a short window guarantees progress
			next = i + 1
		}
		i = next
	}
	return out
}

// e0i reports the line span of rows[start:end), clamped.
func e0i(rows []string, start, end int) int {
	if end > len(rows) {
		end = len(rows)
	}
	if end < start {
		end = start
	}
	return end - start
}

func hasSuffixFold(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
