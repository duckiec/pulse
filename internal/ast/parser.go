package ast

import (
	"fmt"
	"iter"
	"os"
	"sync"
)

// ---- AST node types ---------------------------------------------------------
//
// The AST is offset-based: every string is a [start,end) byte span into the
// arena-owned source copy (see arena.go). That is what keeps a 10,000-request
// document inside 2x its source size: the arena stores the source once, and
// nodes store compact int32 spans instead of 16-byte string headers.
//
// Zero-allocation discipline (ROADMAP.md §1.1.5): every slice below is carved
// from the arena (allocSlice); every string accessor is a view ([Document.Str]);
// Parse itself performs no heap allocation once the arena is warm.

// Method is an HTTP request method. The lexer admits only the seven standard
// keywords, so the enum is exhaustive (a missing value is a parser bug and
// surfaces as MethodUnknown in the golden snapshots).
type Method uint8

const (
	MethodUnknown Method = iota
	MethodGet
	MethodPost
	MethodPut
	MethodPatch
	MethodDelete
	MethodHead
	MethodOptions
)

// String implements fmt.Stringer.
func (m Method) String() string {
	switch m {
	case MethodGet:
		return "GET"
	case MethodPost:
		return "POST"
	case MethodPut:
		return "PUT"
	case MethodPatch:
		return "PATCH"
	case MethodDelete:
		return "DELETE"
	case MethodHead:
		return "HEAD"
	case MethodOptions:
		return "OPTIONS"
	default:
		return "UNKNOWN"
	}
}

// methodFromSpan maps a validated method lexeme to its enum without allocating
// (the compiler elides the string(b) copy in a switch). The lexer has already
// proven the word is one of the seven; MethodUnknown is the fail-closed default.
func methodFromSpan(src []byte, t Token) Method {
	switch string(src[t.Start:t.End]) {
	case "GET":
		return MethodGet
	case "POST":
		return MethodPost
	case "PUT":
		return MethodPut
	case "PATCH":
		return MethodPatch
	case "DELETE":
		return MethodDelete
	case "HEAD":
		return MethodHead
	case "OPTIONS":
		return MethodOptions
	default:
		return MethodUnknown
	}
}

// BodyKind classifies a request body.
type BodyKind uint8

const (
	BodyNone BodyKind = iota
	BodyJSON
	BodyXML
	BodyRaw
)

// String implements fmt.Stringer.
func (k BodyKind) String() string {
	switch k {
	case BodyJSON:
		return "JSON"
	case BodyXML:
		return "XML"
	case BodyRaw:
		return "RAW"
	default:
		return "NONE"
	}
}

// Body is a request body as a span into the source. For JSON and XML the span
// covers the whole body (open through close); for a raw body the single
// BODYOPEN token already carries the full span.
type Body struct {
	Start, End int32
	Kind       BodyKind
	Line       int32
}

// Header is one field line. ValStart/ValEnd are -1 when the value is empty;
// [Document.Str] renders that as "".
type Header struct {
	NameStart, NameEnd int32
	ValStart, ValEnd   int32
	Line               int32
}

// Assert is a `# @assert ...` directive attached to a request. The span covers
// the expression text after the directive.
type Assert struct {
	Start, End int32
	Line       int32
}

// Variable is a `@name = value` definition. Scope is determined by position:
// variables declared before the first request are file-scoped (Document.Vars);
// variables inside a request's header block are reachable via VarsOf.
type Variable struct {
	NameStart, NameEnd int32
	ValStart, ValEnd   int32
	Line               int32
}

// Request is one executable request. Headers is a sub-slice of the document's
// contiguous header array; Vars/Asserts are [Start,Start+Count) ranges into the
// document's variable/assert arrays.
type Request struct {
	NameStart, NameEnd   int32
	URLStart, URLEnd     int32
	Headers              []Header
	VarStart, VarCount   int32
	AsrtStart, AsrtCount int32
	// WasmSignStart/WasmSignEnd span the script path of a `@wasm-sign <path>`
	// directive attached to this request, or -1/-1 when the request asks for
	// no signing. Read via Document.WasmSign, never directly.
	WasmSignStart, WasmSignEnd int32
	// HookStart/HookEnd span the script path of a `# @wasm <path>` directive
	// attached to this request, or -1/-1 when the request asks for no hook.
	// Read via Document.WasmHook, never directly.
	HookStart, HookEnd int32
	Body               Body
	Method             Method
	Line               int32
}

// Document is the root AST node. It owns no memory of its own: every byte it
// references lives in the Arena, released by [Document.Release].
type Document struct {
	// Path is the source file, set by [ParseFile] (empty when parsed from a
	// buffer). Diagnostics use it in "path:line:col" form.
	Path string
	// Vars holds the file-scoped variables (those declared before the first
	// request) as a sub-slice of the document's variable array.
	Vars   []Variable
	reqs   []Request
	hdrs   []Header
	allVar []Variable
	asrts  []Assert
	arena  *Arena
}

// Requests yields every request in source order with zero allocation
// (ROADMAP.md §1.1.4). Breaking out of the range is safe.
func (d *Document) Requests() iter.Seq[*Request] {
	return func(yield func(*Request) bool) {
		for i := range d.reqs {
			if !yield(&d.reqs[i]) {
				return
			}
		}
	}
}

// Len returns the number of requests in the document.
func (d *Document) Len() int { return len(d.reqs) }

// Source returns the arena-owned source bytes (read-only; valid until Release).
func (d *Document) Source() []byte { return d.arena.src }

// Str returns the source text for the span [start,end) as a zero-copy view. An
// empty or inverted span yields "".
func (d *Document) Str(start, end int32) string { return d.arena.str(start, end) }

// BodyText returns the raw body bytes for b (nil when b has no body).
func (d *Document) BodyText(b Body) []byte { return d.arena.bytes(b.Start, b.End) }

// wasmSignDirective is the directive name (after '@') selecting Wasm signing.
const wasmSignDirective = "wasm-sign"

// hookDirective is the comment directive selecting a Wasm request hook. It is
// deliberately NOT `@wasm-sign`: signing appends headers to a canonical
// envelope, while a hook rewrites the whole pending request before the policy
// gate sees it. Two directives keep the narrower, append-only contract as the
// default for the common SigV4/HMAC case.
const hookDirective = "# @wasm"

// parseHookLine interprets a `# @wasm <path>` comment and returns the path
// span. ok is false for every other comment, including `# @wasm-sign ...`
// (the-next-byte rule below is what separates the two). An explicit `# @wasm`
// with no path is a hard error, never an empty hook: an empty path would
// resolve against the working directory by accident.
func parseHookLine(src []byte, start, end int32) (ps, pe int32, ok bool, err *SyntaxError) {
	s, e := trimSpan(src, start, end)
	if !hasPrefixSpan(src, s, e, hookDirective) {
		return 0, 0, false, nil
	}
	rest := s + int32(len(hookDirective))
	if rest < e && src[rest] != ' ' && src[rest] != '\t' {
		// `# @wasm-sign` continues with '-', not a blank: not this directive.
		// Any other suffix (`# @wasmly`) is likewise somebody else's comment.
		return 0, 0, false, nil
	}
	// A bare `# @wasm` reaches here: it names the directive with no path, so it
	// fails closed below rather than degrading to an inert comment.
	ps, pe = trimSpan(src, rest, e)
	if pe <= ps {
		return 0, 0, true, &SyntaxError{Kind: ErrMalformedVar, Offset: s, Expected: "wasm hook script path"}
	}
	return ps, pe, true, nil
}

// WasmHook returns the `# @wasm` script path attached to r, or "" when the
// request asks for no hook.
func (d *Document) WasmHook(r *Request) string {
	if r == nil || r.HookStart < 0 || r.HookEnd <= r.HookStart {
		return ""
	}
	return d.arena.str(r.HookStart, r.HookEnd)
}

// parseWasmSignLine interprets a `@...` line (without the '@') as
// `wasm-sign <path>` and returns the path span. ok is false when the line is
// some other directive — the caller keeps its own meaning for those. An
// explicit `wasm-sign` with no path is a hard error, never an empty signer:
// an empty path would resolve against the working directory by accident.
func parseWasmSignLine(src []byte, start, end int32) (ps, pe int32, ok bool, err *SyntaxError) {
	s, e := trimSpan(src, start, end)
	if !hasPrefixSpan(src, s, e, wasmSignDirective) {
		return 0, 0, false, nil
	}
	rest := s + int32(len(wasmSignDirective))
	if rest >= e || (src[rest] != ' ' && src[rest] != '\t') {
		// `wasm-signup = x` is a variable, not this directive: the name must
		// end at a blank. Anything else here is not wasm-sign at all.
		return 0, 0, false, nil
	}
	ps, pe = trimSpan(src, rest, e)
	if pe <= ps {
		return 0, 0, true, &SyntaxError{Kind: ErrMalformedVar, Offset: s, Expected: "wasm-sign script path"}
	}
	return ps, pe, true, nil
}

// WasmSign returns the @wasm-sign script path attached to r, or "" when the
// request asks for no signing.
func (d *Document) WasmSign(r *Request) string {
	if r == nil || r.WasmSignStart < 0 || r.WasmSignEnd <= r.WasmSignStart {
		return ""
	}
	return d.arena.str(r.WasmSignStart, r.WasmSignEnd)
}

// VarsOf returns the request-scoped variables of r (a sub-slice; no copy).
func (d *Document) VarsOf(r *Request) []Variable {
	return d.allVar[r.VarStart : r.VarStart+r.VarCount : r.VarStart+r.VarCount]
}

// AssertsOf returns the assert directives of r (a sub-slice; no copy).
func (d *Document) AssertsOf(r *Request) []Assert {
	return d.asrts[r.AsrtStart : r.AsrtStart+r.AsrtCount : r.AsrtStart+r.AsrtCount]
}

// Release recycles the whole document's memory into the arena pool. One-shot:
// the Document and everything obtained from it are invalid afterwards.
func (d *Document) Release() {
	arena := d.arena
	*d = Document{}
	docPool.Put(d)
	arena.Release()
}

// docPool recycles the root Document. The Document is deliberately NOT
// arena-allocated: it is the one AST node holding Go pointers (Path, the node
// slices, and the arena itself), and the garbage collector does not trace
// pointers stored inside arena blocks. Arena-allocating it would make the
// arena's own lifetime invisible to the GC, letting it be collected while a
// Document still points at it. Keeping the root on the heap (pooled, so still
// zero-allocation) keeps the whole arena graph reachable and alive.
var docPool = sync.Pool{New: func() any { return new(Document) }}

// putDoc returns an unused document to the pool.
func putDoc(d *Document) {
	*d = Document{}
	docPool.Put(d)
}

// ---- diagnostics ------------------------------------------------------------

// SyntaxError is a parser diagnostic with the location of the offending token
// (AGENTS.md §4.4: structured, fail-closed, never swallowed).
type SyntaxError struct {
	Path      string
	Kind      ErrKind
	Offset    int32
	Line, Col int32
	Expected  string
	Found     string

	// internal marks a violated parser invariant rather than a source defect.
	// Such an error names no file position, because the source is not at fault,
	// and it is never surfaced to users as a diagnostic.
	internal bool
}

// Error implements error as "path:line:col: expected X, found Y".
func (e *SyntaxError) Error() string {
	loc := e.Path
	if loc == "" {
		loc = "<buffer>"
	}
	return fmt.Sprintf("ast: %s:%d:%d: %s (expected %s, found %s)",
		loc, e.Line, e.Col, e.Kind, e.Expected, e.Found)
}

// maxDiags caps ParseDiag accumulation so a pathological file cannot produce an
// unbounded diagnostic list.
const maxDiags = 32

// ---- parser -----------------------------------------------------------------

var (
	lexerPool  = sync.Pool{New: func() any { return new(Lexer) }}
	parserPool = sync.Pool{New: func() any { return new(parser) }}
)

func getLexer() *Lexer { return lexerPool.Get().(*Lexer) }
func putLexer(l *Lexer) {
	*l = Lexer{}
	lexerPool.Put(l)
}

func getParser() *parser { return parserPool.Get().(*parser) }

// putParser recycles the parser. Any collected diagnostic slice is detached by
// the struct reset below; parseSource takes ownership of p.diags before calling.
func putParser(p *parser) {
	*p = parser{}
	parserPool.Put(p)
}

// parser is the LL(1) recursive-descent state (ROADMAP.md §1.1.3): exactly one
// token of lookahead lives in cur; the lexer supplies one-token pushback for
// peeking.
type parser struct {
	arena *Arena
	doc   *Document
	lx    *Lexer
	cur   Token
	prev  Token // the token before cur, for one-token unget
	src   []byte

	nReq, nHdr, nVar, nAsrt int

	// Block scoping: everything declared after a separator belongs to the next
	// request in that block. Variables and asserts seen before the first
	// separator are file-scoped (Document.Vars); the rest are reachable via
	// VarsOf/AssertsOf with ranges anchored at blockVarStart/blockAsrtStart.
	blockVarStart  int
	blockAsrtStart int
	fileVarEnd     int
	seenSeparator  bool

	pendingNameStart, pendingNameEnd int32 // -1 when unset
	pendingWasmStart, pendingWasmEnd int32 // -1 when unset: @wasm-sign path for the next request
	pendingHookStart, pendingHookEnd int32 // -1 when unset: @wasm hook path for the next request

	collect bool          // ParseDiag mode: record and resync, don't stop
	diags   []SyntaxError // accumulated diagnostics (collect mode)
	path    string
}

// Parse builds a Document from the arena's current source. Fail-closed: on any
// error it returns (nil, err) — never a partial or corrupted executable AST
// (AGENTS.md §4.4). The caller owns the arena and releases it (or, on success,
// releases the Document).
func Parse(arena *Arena) (*Document, error) {
	doc, _, err := parseSource(arena, false, "")
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// ParseDiag parses and accumulates every recoverable diagnostic, resyncing at
// request separators so a file with several errors reports all of them (the
// editor/TUI path). The returned error is the first diagnostic; the document
// is never returned, because a file with any diagnostic has no valid AST.
func ParseDiag(arena *Arena) ([]SyntaxError, error) {
	_, diags, err := parseSource(arena, true, "")
	return diags, err
}

// ParseFile reads path, parses it, and returns a self-contained Document whose
// Path is set. On error the arena is released internally.
func ParseFile(path string) (*Document, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ast: read %s: %w", path, err)
	}
	arena := AcquireArena()
	arena.Reset(src)
	doc, err := Parse(arena)
	if err != nil {
		if se, ok := err.(*SyntaxError); ok {
			se.Path = path
		}
		arena.Release()
		return nil, err
	}
	doc.Path = path
	return doc, nil
}

// ParseFileDiag is the ParseDiag counterpart of [ParseFile].
func ParseFileDiag(path string) ([]SyntaxError, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ast: read %s: %w", path, err)
	}
	arena := AcquireArena()
	arena.Reset(src)
	diags, perr := ParseDiag(arena)
	arena.Release()
	for i := range diags {
		diags[i].Path = path
	}
	return diags, perr
}

// parseSource is the shared engine. It runs the counting pre-pass (to size the
// arena exactly), then the fill pass.
func parseSource(arena *Arena, collect bool, path string) (*Document, []SyntaxError, error) {
	src := arena.src
	nReq, nHdr, nVar, nAsrt := countNodes(src)

	doc := docPool.Get().(*Document)
	doc.reqs = allocSlice[Request](arena, nReq)
	doc.hdrs = allocSlice[Header](arena, nHdr)
	doc.allVar = allocSlice[Variable](arena, nVar)
	doc.asrts = allocSlice[Assert](arena, nAsrt)
	doc.arena = arena

	p := getParser()
	p.arena, p.doc, p.path = arena, doc, path
	p.src = src
	p.collect = collect
	p.lx = getLexer()
	p.lx.Reset(src)
	p.pendingNameStart, p.pendingNameEnd = -1, -1
	p.pendingWasmStart, p.pendingWasmEnd = -1, -1
	p.pendingHookStart, p.pendingHookEnd = -1, -1
	p.advance()

	err := p.parseDocument()

	var diags []SyntaxError
	if collect {
		diags, p.diags = p.diags, nil
	}
	if p.lx != nil {
		putLexer(p.lx)
		p.lx = nil
	}

	if err != nil {
		putParser(p)
		putDoc(doc)
		return nil, diags, err
	}
	if collect && len(diags) > 0 {
		// Collected diagnostics mean the file has no valid AST: report the
		// first one and never hand back a document (fail-closed).
		first := diags[0]
		putParser(p)
		putDoc(doc)
		return nil, diags, &first
	}
	doc.Vars = doc.allVar[:p.fileVarEnd:p.fileVarEnd]
	putParser(p)
	return doc, diags, nil
}

// countNodes is the file-scope pre-pass: a cheap scan of the token stream
// (offsets only, zero allocation) that sizes every arena array exactly, so the
// fill pass never grows or overruns.
//
// It drives the lexer with NextToken, not Tokens, because that is what the fill
// pass consumes: Tokens stops at the first illegal token, so on a recoverable
// file it would under-count every request after the damage and the fill pass
// would then write past its arrays. Counting the identical stream is what makes
// "the counts can never disagree with the fill pass" true on malformed input
// too (AGENTS.md §4.4: fail closed, never guess).
func countNodes(src []byte) (nReq, nHdr, nVar, nAsrt int) {
	lx := getLexer()
	lx.Reset(src)
	for {
		tok := lx.NextToken()
		if tok.Kind == TokEOF {
			break
		}
		switch tok.Kind {
		case TokMethod:
			nReq++
		case TokHeaderName:
			nHdr++
		case TokVariable:
			nVar++
		case TokComment:
			if isAssertComment(src, tok) {
				nAsrt++
			}
		}
	}
	putLexer(lx)
	return nReq, nHdr, nVar, nAsrt
}

// parseDocument drives the top-level grammar:
// (variable | comment | separator | request)*.
func (p *parser) parseDocument() error {
	for {
		done, err := p.step()
		if err != nil {
			if !p.collect || !p.record(err) {
				// Fail-closed: a non-recoverable error (or one raised outside
				// collection mode) stops the parse here.
				return err
			}
			p.resync()
			if p.cur.Kind == TokEOF || len(p.diags) >= maxDiags {
				return nil
			}
			continue
		}
		if done {
			return nil
		}
		p.advance()
	}
}

// step consumes exactly one grammar unit and leaves cur at the last token it
// consumed. done is true at end of input.
func (p *parser) step() (bool, error) {
	switch p.cur.Kind {
	case TokEOF:
		return true, nil
	case TokIllegal:
		return false, p.errorAt(ErrIllegalByte, "valid token", p.cur)
	case TokVariable:
		return false, p.parseVariable()
	case TokComment:
		return false, p.parseComment()
	case TokSeparator:
		if !p.seenSeparator {
			p.fileVarEnd = p.nVar
			p.seenSeparator = true
		}
		p.blockVarStart = p.nVar
		p.blockAsrtStart = p.nAsrt
		p.pendingNameStart, p.pendingNameEnd = -1, -1
		p.pendingWasmStart, p.pendingWasmEnd = -1, -1
		p.pendingHookStart, p.pendingHookEnd = -1, -1
		return false, nil
	case TokMethod:
		return false, p.parseRequest()
	case TokBlankLine, TokNewline:
		return false, nil
	default:
		return false, p.errorAt(ErrSyntax, "request, separator, comment, or variable", p.cur)
	}
}

// parseVariable consumes `@name = value`. A definition line without '=' is a
// hard, fail-closed error — with ONE exception: the `@wasm-sign <path>`
// directive, which takes a bare path rather than an assignment and attaches
// to the next request (pending slot, like `@name`). A `@wasm-sign` inside a
// request's header block is handled by parseRequest instead.
func (p *parser) parseVariable() error {
	tok := p.cur
	eq := int32(-1)
	for i := tok.Start; i < tok.End; i++ {
		if p.src[i] == '=' {
			eq = i
			break
		}
	}
	if eq < 0 {
		ps, pe, ok, serr := parseWasmSignLine(p.src, tok.Start+1, tok.End)
		if serr != nil {
			serr.Line, serr.Col = tok.Line, tok.Col
			return serr
		}
		if !ok {
			return p.errorAt(ErrMissingAssign, "variable name '=' value", tok)
		}
		if p.pendingWasmStart >= 0 {
			return p.errorAt(ErrSyntax, "one @wasm-sign per request", tok)
		}
		p.pendingWasmStart, p.pendingWasmEnd = ps, pe
		return nil
	}
	nameStart, nameEnd := trimSpan(p.src, tok.Start+1, eq)
	valStart, valEnd := trimSpan(p.src, eq+1, tok.End)
	if nameEnd <= nameStart {
		return p.errorAt(ErrMalformedVar, "variable name", tok)
	}
	// `@wasm-sign = x` is a directive misspelled as an assignment. Accepting
	// it as a variable would silently NOT sign; refuse with direction.
	if nameEnd-nameStart == int32(len(wasmSignDirective)) &&
		hasPrefixSpan(p.src, nameStart, nameEnd, wasmSignDirective) {
		return p.errorAt(ErrSyntax, "@wasm-sign <path> (no '=')", tok)
	}
	if p.nVar >= len(p.doc.allVar) {
		return p.internalErr("variable capacity")
	}
	p.doc.allVar[p.nVar] = Variable{
		NameStart: nameStart, NameEnd: nameEnd,
		ValStart: valStart, ValEnd: valEnd,
		Line: tok.Line,
	}
	p.nVar++
	return nil
}

// parseComment records a pending `# @name` or `# @wasm` (both attached to the
// next request) and appends `# @assert` directives. A capacity overflow here
// means the counting pre-pass and this pass disagree — an invariant violation,
// reported rather than silently truncated (AGENTS.md §4.4: fail closed, never
// drop an AST node).
func (p *parser) parseComment() error {
	tok := p.cur
	if ps, pe, ok, serr := parseHookLine(p.src, tok.Start, tok.End); serr != nil {
		serr.Line, serr.Col = tok.Line, tok.Col
		return serr
	} else if ok {
		if p.pendingHookStart >= 0 {
			return p.errorAt(ErrSyntax, "one @wasm per request", tok)
		}
		p.pendingHookStart, p.pendingHookEnd = ps, pe
		return nil
	}
	if hasPrefixSpan(p.src, tok.Start, tok.End, nameDirective) {
		p.pendingNameStart, p.pendingNameEnd = trimSpan(p.src, tok.Start+int32(len(nameDirective)), tok.End)
		return nil
	}
	if hasPrefixSpan(p.src, tok.Start, tok.End, assertDirective) {
		if p.nAsrt >= len(p.doc.asrts) {
			return p.internalErr("assert capacity")
		}
		s, e := trimSpan(p.src, tok.Start+int32(len(assertDirective)), tok.End)
		p.doc.asrts[p.nAsrt] = Assert{Start: s, End: e, Line: tok.Line}
		p.nAsrt++
	}
	return nil
}

// parseRequest consumes one request: method, target, optional version, header
// block, blank line, and body.
func (p *parser) parseRequest() error {
	meth := p.cur
	if p.nReq >= len(p.doc.reqs) {
		return p.internalErr("request capacity")
	}
	if !p.seenSeparator {
		// Everything declared before the first separator (or, failing that,
		// before the first request) is file-scoped; this request claims only
		// what follows.
		p.fileVarEnd = p.nVar
		p.blockVarStart = p.nVar
		p.seenSeparator = true
	}
	req := &p.doc.reqs[p.nReq]
	hdrStart := p.nHdr
	*req = Request{
		Method:    methodFromSpan(p.src, meth),
		Line:      meth.Line,
		NameStart: p.pendingNameStart, NameEnd: p.pendingNameEnd,
		VarStart: int32(p.blockVarStart), AsrtStart: int32(p.blockAsrtStart),
		WasmSignStart: p.pendingWasmStart, WasmSignEnd: p.pendingWasmEnd,
		HookStart: p.pendingHookStart, HookEnd: p.pendingHookEnd,
	}
	p.nReq++
	p.pendingNameStart, p.pendingNameEnd = -1, -1
	p.pendingWasmStart, p.pendingWasmEnd = -1, -1
	p.pendingHookStart, p.pendingHookEnd = -1, -1

	// Target: at least one URL/INTERP token is mandatory.
	p.advance()
	if p.cur.Kind != TokURL && p.cur.Kind != TokInterp {
		return p.errorAt(ErrMissingTarget, "request target", p.cur)
	}
	req.URLStart, req.URLEnd = p.cur.Start, p.cur.End
	for p.cur.Kind == TokURL || p.cur.Kind == TokInterp {
		req.URLEnd = p.cur.End
		p.advance()
	}
	if p.cur.Kind == TokHTTPVersion {
		p.advance()
	}

	// Header block and body.
	for {
		switch p.cur.Kind {
		case TokVariable:
			// The ONLY `@` line legal inside a header block is the signing
			// directive: anything else keeps its historic syntax error, so a
			// misplaced `@var = v` is still caught rather than silently scoped.
			vtok := p.cur
			ps, pe, ok, serr := parseWasmSignLine(p.src, vtok.Start+1, vtok.End)
			if serr != nil {
				serr.Line, serr.Col = vtok.Line, vtok.Col
				return serr
			}
			if !ok {
				return p.errorAt(ErrSyntax, "header name, blank line, or separator", vtok)
			}
			if req.WasmSignStart >= 0 {
				return p.errorAt(ErrSyntax, "one @wasm-sign per request", vtok)
			}
			req.WasmSignStart, req.WasmSignEnd = ps, pe
			p.advance()
		case TokHeaderName:
			if p.nHdr >= len(p.doc.hdrs) {
				return p.internalErr("header capacity")
			}
			nameStart, nameEnd := p.cur.Start, p.cur.End
			p.advance()
			valStart, valEnd := int32(-1), int32(-1)
			if p.cur.Kind == TokHeaderValue || p.cur.Kind == TokInterp {
				valStart, valEnd = p.cur.Start, p.cur.End
				for p.cur.Kind == TokHeaderValue || p.cur.Kind == TokInterp {
					valEnd = p.cur.End
					p.advance()
				}
			}
			p.doc.hdrs[p.nHdr] = Header{
				NameStart: nameStart, NameEnd: nameEnd,
				ValStart: valStart, ValEnd: valEnd,
				Line: p.cur.Line,
			}
			p.nHdr++
			p.advance() // step to the line terminator (or the block boundary)

		case TokNewline:
			p.advance()
			switch p.cur.Kind {
			case TokHeaderName, TokNewline, TokBlankLine:
				continue
			}
			// An adjacent `@wasm-sign` line after an in-block attach would
			// otherwise seal the request and become a forward-pending
			// directive that EOF silently drops — a second signer claim on
			// this request that vanishes instead of erroring. Peek: if the
			// boundary token claims a signer this request already has,
			// refuse. (A blank-line-separated directive stays forward
			// pending for the next request, like `@name`.)
			if p.cur.Kind == TokVariable && req.WasmSignStart >= 0 {
				if _, _, ok, serr := parseWasmSignLine(p.src, p.cur.Start+1, p.cur.End); serr != nil || ok {
					if serr != nil {
						serr.Line, serr.Col = p.cur.Line, p.cur.Col
						return serr
					}
					return p.errorAt(ErrSyntax, "one @wasm-sign per request", p.cur)
				}
			}
			p.unget() // the boundary token belongs to the document loop
			return p.finishRequest(req, hdrStart)

		case TokBlankLine:
			p.advance()
			if err := p.parseBody(req); err != nil {
				return err
			}
			return p.finishRequest(req, hdrStart)

		case TokSeparator, TokMethod, TokEOF, TokIllegal:
			p.unget()
			return p.finishRequest(req, hdrStart)

		default:
			return p.errorAt(ErrSyntax, "header name, blank line, or separator", p.cur)
		}
	}
}

// parseBody attaches the body to req. A JSON/XML body is open+close; a raw body
// is a single BODYOPEN token carrying the whole span.
func (p *parser) parseBody(req *Request) error {
	if p.cur.Kind != TokBodyOpen {
		req.Body = Body{Kind: BodyNone, Line: p.cur.Line}
		p.unget() // no body: this token belongs to the document level
		return nil
	}
	openTok := p.cur
	p.advance()
	switch p.cur.Kind {
	case TokBodyClose:
		closeTok := p.cur
		kind := BodyRaw
		if p.src[openTok.Start] == '{' {
			kind = BodyJSON
		} else if p.src[openTok.Start] == '<' {
			kind = BodyXML
		}
		req.Body = Body{Kind: kind, Start: openTok.Start, End: closeTok.End, Line: openTok.Line}
		return nil // cur is the body's last token
	case TokBodyOpen:
		return p.errorAt(ErrSyntax, "body close", p.cur)
	default:
		// Raw body: the open token carries the full span; hand the following
		// token back so cur stays the body's last token.
		req.Body = Body{Kind: BodyRaw, Start: openTok.Start, End: openTok.End, Line: openTok.Line}
		p.unget()
		return nil
	}
}

// finishRequest seals the request's sub-ranges. It re-checks every arena bound:
// if the counting pre-pass and this pass ever disagreed, we fail closed instead
// of writing past an allocation.
func (p *parser) finishRequest(req *Request, hdrStart int) error {
	if p.nHdr > len(p.doc.hdrs) || p.nVar > len(p.doc.allVar) || p.nAsrt > len(p.doc.asrts) {
		return p.internalErr("arena capacity")
	}
	req.Headers = p.doc.hdrs[hdrStart:p.nHdr:p.nHdr]
	req.VarCount = int32(p.nVar) - req.VarStart
	req.AsrtCount = int32(p.nAsrt) - req.AsrtStart
	return nil
}

// ---- token plumbing ---------------------------------------------------------

// advance moves to the next token, remembering the current one so unget can
// restore it.
func (p *parser) advance() {
	p.prev = p.cur
	p.cur = p.lx.NextToken()
}

// unget returns the current token to the stream and rewinds cur to the token
// before it: the next advance() yields the token we are standing on. Used when
// a request ends on a token that belongs to the document level (a separator, the
// next method, end of input).
func (p *parser) unget() {
	cur := p.cur
	p.cur = p.prev
	p.lx.Push(cur)
}

// ---- diagnostics helpers ----------------------------------------------------

// errorAt builds a source diagnostic for the token the parser is standing on.
// It does NOT record: parseDocument is the single recording point, so every
// error is reported exactly once.
func (p *parser) errorAt(kind ErrKind, expected string, tok Token) *SyntaxError {
	return &SyntaxError{
		Path:     p.path,
		Kind:     kind,
		Offset:   tok.Start,
		Line:     tok.Line,
		Col:      tok.Col,
		Expected: expected,
		Found:    foundName(tok),
	}
}

// internalErr reports a violated parser invariant (a count/fill disagreement,
// never bad input). It is never recorded as a source diagnostic: it names no
// file position because the source is not at fault.
func (p *parser) internalErr(what string) *SyntaxError {
	return &SyntaxError{
		Path:     p.path,
		Kind:     ErrSyntax,
		Expected: "internal parser invariant",
		Found:    what,
		internal: true,
	}
}

// record appends a recoverable diagnostic and reports whether it did. Internal
// invariant errors and non-collecting mode return false, which makes the caller
// halt instead of resyncing past a violation.
func (p *parser) record(err error) bool {
	if !p.collect {
		return false
	}
	se, ok := err.(*SyntaxError)
	if !ok || se.internal {
		return false
	}
	p.diags = append(p.diags, *se)
	return true
}

// resync skips to the next request boundary after an error, so ParseDiag can
// keep collecting instead of stopping at the first one.
func (p *parser) resync() {
	p.pendingNameStart, p.pendingNameEnd = -1, -1
	p.pendingWasmStart, p.pendingWasmEnd = -1, -1
	for {
		switch p.cur.Kind {
		case TokSeparator:
			p.advance()
			return
		case TokMethod, TokEOF:
			return
		}
		p.advance()
	}
}

func foundName(tok Token) string {
	switch tok.Kind {
	case TokEOF:
		return "end of file"
	case TokIllegal:
		return "illegal token"
	case TokURL, TokHeaderValue, TokInterp:
		return "value"
	case TokNewline:
		return "end of line"
	case TokBlankLine:
		return "blank line"
	case TokBodyOpen, TokBodyClose:
		return "body"
	default:
		return tok.Kind.String()
	}
}

// ---- shared span helpers ----------------------------------------------------

const (
	nameDirective   = "# @name"
	assertDirective = "# @assert"
)

// isAssertComment is the single source of truth for what counts as an assert
// directive; the counting pre-pass and the fill pass both use it, so their
// counts can never diverge.
func isAssertComment(src []byte, tok Token) bool {
	return hasPrefixSpan(src, tok.Start, tok.End, assertDirective)
}

// hasPrefixSpan reports whether src[start:end] starts with p.
func hasPrefixSpan(src []byte, start, end int32, p string) bool {
	if int(end)-int(start) < len(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if src[start+int32(i)] != p[i] {
			return false
		}
	}
	return true
}

// trimSpan trims spaces and tabs from both ends of the byte span and returns
// the trimmed bounds (no copy).
func trimSpan(src []byte, start, end int32) (int32, int32) {
	for start < end && isSpaceTab(src[start]) {
		start++
	}
	for end > start && isSpaceTab(src[end-1]) {
		end--
	}
	return start, end
}
