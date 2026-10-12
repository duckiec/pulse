package ast

import "iter"

// lexState is the position in the .http grammar (ROADMAP.md §1.1.2). The
// transition table is implemented by [Lexer.next]'s dispatch plus the state
// assignments in each handler:
//
//	stLineStart --method--> stRequestLine --target--> stHeaders
//	stHeaders --blank line--> stBody{JSON,XML,Raw} --close--> stDone
//	stDone --delegate--> stLineStart (a body is always followed by a
//	separator, a new request, or end of input)
type lexState uint8

const (
	stLineStart   lexState = iota // beginning of a line, outside any request
	stRequestLine                 // after a method; expecting the target
	stHeaders                     // field lines until a blank line
	stBodyJSON                    // inside a '{' body, brace-depth scanned
	stBodyXML                     // inside a '<' body, tag-depth scanned
	stBodyRaw                     // undelimited body, until blank line/###/EOF
	stDone                        // after a body; delegates to stLineStart
)

// Lexer tokenizes a .http source buffer. One Lexer is allocated per document;
// the per-token hot path ([Lexer.Tokens]) allocates nothing.
//
// Invariants enforced structurally and verified by tests:
//
//   - every next() call either consumes at least one byte or returns TokEOF
//     (termination: a stream over N bytes yields at most N+1 tokens)
//   - every token except TokEOF satisfies Start < End
//   - the first TokIllegal stops the stream; [Lexer.Diag] carries the reason
type Lexer struct {
	src  []byte
	off  int // next byte to consume
	line int // 1-based line of off
	col  int // 1-based byte column of off

	state       lexState
	atLineStart bool // off is at the beginning of a line

	bodyDepth     int // JSON brace depth / XML tag depth (0 outside a body)
	bodyStart     int // offset of the first body content byte (-1: not yet known)
	bodyStartLine int
	bodyStartCol  int
	bodyOpenAt    int // offset of the opening delimiter ('{' or root '<')
	bodyOpenLine  int
	bodyOpenCol   int

	hasPending bool // a follow-up token must be emitted before scanning resumes
	pending    Token

	// pushed holds one token of pushback so a parser can peek without
	// consuming, keeping exactly one lookahead token in frame.
	pushed    bool
	pushedTok Token

	// Value segmentation: single-line values (targets, header values) are
	// emitted as alternating literal runs and TokInterp anchors.
	segActive       bool
	segKind         TokenKind // literal kind for the current value
	segStopAtSpace  bool      // targets end at SP (a version may follow)
	wantHTTPVersion bool      // target segment ended at SP; version may follow
	wantHeaderValue bool      // header name emitted; ':' and value follow

	diag    Diag
	hasDiag bool
}

// New returns a lexer over src. The caller retains ownership of src; the lexer
// only reads it and stores offsets into it.
func New(src []byte) *Lexer {
	return &Lexer{src: src, line: 1, col: 1, state: stLineStart, atLineStart: true}
}

// Reset rebinds the lexer to a new source buffer without allocating, so a
// single lexer can be reused across documents.
func (l *Lexer) Reset(src []byte) {
	*l = Lexer{src: src, line: 1, col: 1, state: stLineStart, atLineStart: true}
}

// Diag returns the diagnostic recorded for the first illegal token, if any.
func (l *Lexer) Diag() (Diag, bool) { return l.diag, l.hasDiag }

// Tokens yields the token stream (ROADMAP.md §1.1.4). The sequence stops at
// TokEOF or at the first TokIllegal: fail closed, never resync-and-continue
// inside the lexer (AGENTS.md §4.4). Breaking out of the range is safe; the
// lexer may be Reset and reused.
func (l *Lexer) Tokens() iter.Seq[Token] {
	return func(yield func(Token) bool) {
		for {
			tok := l.NextToken()
			if !yield(tok) {
				return
			}
			if tok.Kind == TokEOF || tok.Kind == TokIllegal {
				return
			}
		}
	}
}

// NextToken returns the next token from the stream without Tokens()'s
// stop-at-illegal rule: the parser performs its own fail-closed handling and
// resync, driving the lexer one token at a time. Allocates nothing.
func (l *Lexer) NextToken() Token {
	if l.pushed {
		l.pushed = false
		return l.pushedTok
	}
	return l.next()
}

// Push returns the most recently produced token to the stream so the next call
// to NextToken yields it again. At most one token may be pushed at a time;
// pushing twice without an intervening read overwrites the first push.
func (l *Lexer) Push(tok Token) {
	l.pushedTok = tok
	l.pushed = true
}

// ClearDiag forgets the recorded diagnostic so a ParseDiag loop can continue
// collecting multiple errors. Callers record their own diagnostics.
func (l *Lexer) ClearDiag() { l.hasDiag = false }

// next is the state machine dispatcher.
func (l *Lexer) next() Token {
	if l.hasPending {
		t := l.pending
		l.hasPending = false
		return t
	}
	if l.segActive {
		return l.nextSegment()
	}
	// Only line-oriented states emit a line terminator here. Body states
	// (JSON/XML/raw) scan their own bytes between tokens and must never leak
	// the newlines inside a body into the token stream.
	switch l.state {
	case stLineStart, stRequestLine, stHeaders, stDone:
		if !l.atLineStart && !l.eof() && isNewline(l.cur()) {
			return l.newlineToken(TokNewline)
		}
	}
	switch l.state {
	case stRequestLine:
		return l.lexRequestLine()
	case stHeaders:
		return l.lexHeaders()
	case stBodyJSON:
		return l.lexBodyJSON()
	case stBodyXML:
		return l.lexBodyXML()
	case stBodyRaw:
		return l.lexBodyRaw()
	case stDone:
		l.state = stLineStart
		return l.lexLineStart()
	default: // stLineStart
		return l.lexLineStart()
	}
}

// ---- state handlers ---------------------------------------------------------

func (l *Lexer) lexLineStart() Token {
	l.skipSpaceTab()
	if l.eof() {
		return l.mk(TokEOF, l.off, l.line, l.col)
	}
	switch c := l.cur(); {
	case isNewline(c):
		return l.newlineToken(TokBlankLine)
	case c == '#':
		s, sl, sc := l.off, l.line, l.col
		kind := TokComment
		if l.hasPrefix("###") {
			kind = TokSeparator
		}
		l.skipToEOL()
		l.atLineStart = false
		return l.mk(kind, s, sl, sc)
	case c == '@':
		s, sl, sc := l.off, l.line, l.col
		l.skipToEOL()
		l.atLineStart = false
		return l.mk(TokVariable, s, sl, sc)
	case c >= 'A' && c <= 'Z':
		s, sl, sc := l.off, l.line, l.col
		l.consumeMethodWord()
		if !l.eof() && !isSpaceTab(l.cur()) && !isNewline(l.cur()) {
			return l.illegal(ErrUnknownMethod, s, sl, sc)
		}
		if !isMethodName(l.src[s:l.off]) {
			return l.illegal(ErrUnknownMethod, s, sl, sc)
		}
		if l.eof() || isNewline(l.cur()) {
			// A method line must carry its target on the same line.
			return l.illegal(ErrIllegalByte, s, sl, sc)
		}
		l.atLineStart = false
		l.state = stRequestLine
		return l.mk(TokMethod, s, sl, sc)
	default:
		s, sl, sc := l.off, l.line, l.col
		return l.illegal(ErrIllegalByte, s, sl, sc)
	}
}

func (l *Lexer) lexRequestLine() Token {
	entryOff, entryLine, entryCol := l.off, l.line, l.col
	if l.wantHTTPVersion {
		l.wantHTTPVersion = false // consume the pending state exactly once
		l.skipSpaceTab()
		if l.eof() {
			l.state = stHeaders
			return l.mk(TokEOF, l.off, l.line, l.col)
		}
		if isNewline(l.cur()) {
			l.state = stHeaders
			return l.newlineToken(TokNewline)
		}
		s, sl, sc := l.off, l.line, l.col
		if !l.hasPrefix("HTTP/") {
			return l.illegal(ErrIllegalByte, s, sl, sc)
		}
		l.skipToEOL()
		l.atLineStart = false
		l.state = stHeaders
		return l.mk(TokHTTPVersion, s, sl, sc)
	}
	l.skipSpaceTab()
	if l.eof() || isNewline(l.cur()) {
		// A method line must carry its target on the same line; span the
		// error from the byte where the line's content began so the token is
		// never zero-width at end of input.
		return l.illegal(ErrIllegalByte, entryOff, entryLine, entryCol)
	}
	l.segActive = true
	l.segKind = TokURL
	l.segStopAtSpace = true
	return l.nextSegment()
}

func (l *Lexer) lexHeaders() Token {
	if l.wantHeaderValue {
		l.wantHeaderValue = false
		if !l.eof() && l.cur() == ':' {
			l.advance()
		}
		l.skipSpaceTab()
		l.segActive = true
		l.segKind = TokHeaderValue
		l.segStopAtSpace = false
		return l.nextSegment()
	}
	l.skipSpaceTab()
	if l.eof() {
		return l.mk(TokEOF, l.off, l.line, l.col)
	}
	switch c := l.cur(); {
	case isNewline(c):
		// Blank line: the header block ends here. The body kind is probed
		// without consuming, so an empty body costs no tokens.
		tok := l.newlineToken(TokBlankLine)
		l.probeBody()
		return tok
	case c == '#':
		s, sl, sc := l.off, l.line, l.col
		kind := TokComment
		if l.hasPrefix("###") {
			kind = TokSeparator
		}
		l.skipToEOL()
		l.atLineStart = false
		if kind == TokSeparator {
			l.state = stLineStart
		}
		return l.mk(kind, s, sl, sc)
	case c == '@':
		s, sl, sc := l.off, l.line, l.col
		l.skipToEOL()
		l.atLineStart = false
		return l.mk(TokVariable, s, sl, sc)
	default:
		s, sl, sc := l.off, l.line, l.col
		for !l.eof() && l.cur() != ':' && !isNewline(l.cur()) {
			l.advance()
		}
		if l.eof() || isNewline(l.cur()) || l.off == s {
			return l.illegal(ErrBadHeader, s, sl, sc)
		}
		tok := l.mk(TokHeaderName, s, sl, sc)
		l.wantHeaderValue = true
		l.atLineStart = false
		return tok
	}
}

func (l *Lexer) lexBodyJSON() Token {
	if l.bodyDepth == 0 {
		l.skipSpaceTab()
		if l.eof() || isNewline(l.cur()) {
			l.state = stLineStart // empty body: no token, resume at line level
			return l.next()
		}
		if l.cur() != '{' {
			s, sl, sc := l.off, l.line, l.col
			return l.illegal(ErrIllegalByte, s, sl, sc)
		}
		s, sl, sc := l.off, l.line, l.col
		l.advance()
		l.bodyDepth = 1
		l.bodyOpenAt, l.bodyOpenLine, l.bodyOpenCol = s, sl, sc
		l.bodyStart, l.bodyStartLine, l.bodyStartCol = l.off, l.line, l.col
		l.atLineStart = false
		return l.mk(TokBodyOpen, s, sl, sc)
	}
	for !l.eof() {
		switch l.cur() {
		case '"':
			l.skipJSONString()
		case '{':
			l.bodyDepth++
			l.advance()
		case '}':
			l.bodyDepth--
			if l.bodyDepth == 0 {
				s, sl, sc := l.off, l.line, l.col
				l.advance()
				l.state = stDone
				l.atLineStart = false
				return l.mk(TokBodyClose, s, sl, sc)
			}
			l.advance()
		default:
			l.advance()
		}
	}
	l.state = stDone
	return l.illegalUnterminated()
}

func (l *Lexer) lexBodyXML() Token {
	if l.bodyDepth == 0 {
		l.skipSpaceTab()
		// A body was probed: the first non-space byte after the headers was
		// '<'. Record the probe position for diagnostics, then skip prolog
		// constructs and the newlines around them until the root element.
		l.bodyOpenAt, l.bodyOpenLine, l.bodyOpenCol = l.off, l.line, l.col
		l.bodyStart = -1
		for !l.eof() {
			if isNewline(l.cur()) {
				l.advance()
				continue
			}
			if !l.isXMLProlog() {
				break
			}
			switch {
			case l.hasPrefix("<!--"):
				l.skipPast("-->")
			case l.hasPrefix("<![CDATA["):
				l.skipPast("]]>")
			case l.hasPrefix("<?"):
				l.skipPast("?>")
			default:
				l.skipPast(">") // <!DOCTYPE ...>; internal subsets unsupported
			}
		}
		if l.eof() {
			l.state = stDone
			return l.illegalUnterminated()
		}
		if l.cur() != '<' {
			s, sl, sc := l.off, l.line, l.col
			return l.illegal(ErrIllegalByte, s, sl, sc)
		}
		tag := l.scanXMLTag()
		if !tag.complete {
			l.state = stDone
			return l.illegalUnterminated()
		}
		l.atLineStart = false
		if tag.selfClosing {
			// A self-closing root completes the body in one tag. The open
			// token spans '<tag' and the close token spans '/>', so the two
			// spans tile the tag without overlapping.
			l.state = stDone
			openTok := Token{
				Kind:    TokBodyOpen,
				Start:   tag.span.Start,
				End:     int32(tag.slashStart),
				Line:    tag.span.Line,
				Col:     tag.span.Col,
				EndLine: int32(tag.slashLine),
				EndCol:  int32(tag.slashCol),
			}
			closeTok := Token{
				Kind:    TokBodyClose,
				Start:   int32(tag.slashStart),
				End:     tag.span.End,
				Line:    int32(tag.slashLine),
				Col:     int32(tag.slashCol),
				EndLine: tag.span.EndLine,
				EndCol:  tag.span.EndCol,
			}
			l.emitPending(closeTok)
			return openTok
		}
		l.bodyDepth = 1
		l.bodyStart, l.bodyStartLine, l.bodyStartCol = int(tag.span.End), int(tag.span.EndLine), int(tag.span.EndCol)
		span := tag.span
		span.Depth = 1 // depth after the root open tag, matching JSON bodies
		return span
	}
	for !l.eof() {
		switch {
		case l.hasPrefix("<!--"):
			l.skipPast("-->")
		case l.hasPrefix("<![CDATA["):
			l.skipPast("]]>")
		case l.hasPrefix("<?"):
			l.skipPast("?>")
		case l.hasPrefix("</"):
			s, sl, sc := l.off, l.line, l.col
			if !l.skipPast(">") {
				l.state = stDone
				return l.illegalUnterminated()
			}
			l.bodyDepth--
			if l.bodyDepth == 0 {
				l.state = stDone
				l.atLineStart = false
				return l.mk(TokBodyClose, s, sl, sc)
			}
		case l.cur() == '<':
			tag := l.scanXMLTag()
			if !tag.complete {
				l.state = stDone
				return l.illegalUnterminated()
			}
			if !tag.selfClosing {
				l.bodyDepth++
			}
		default:
			l.advance()
		}
	}
	l.state = stDone
	return l.illegalUnterminated()
}

// illegalUnterminated reports a body whose closing delimiter never arrived.
// The span covers the body content; when input ends immediately after the
// opening delimiter there is no content to span, so the diagnostic falls back
// to spanning the delimiter itself. Either way the token is non-zero-width.
func (l *Lexer) illegalUnterminated() Token {
	start, sl, sc := l.bodyStart, l.bodyStartLine, l.bodyStartCol
	if l.bodyStart < 0 || l.off <= l.bodyStart {
		start, sl, sc = l.bodyOpenAt, l.bodyOpenLine, l.bodyOpenCol
	}
	return l.illegal(ErrUnterminatedBody, start, sl, sc)
}

func (l *Lexer) lexBodyRaw() Token {
	l.skipSpaceTab()
	if l.eof() || isNewline(l.cur()) || l.hasPrefix("###") {
		l.state = stLineStart // empty body: no token
		l.atLineStart = true
		return l.next()
	}
	bodyStart, bsl, bsc := l.off, l.line, l.col
	bodyEnd := bodyStart
	for !l.eof() {
		if l.hasPrefix("###") { // separator terminates the body, unconsumed
			break
		}
		contentStart := l.off
		for !l.eof() && !isNewline(l.cur()) {
			l.advance()
		}
		if l.off > contentStart {
			bodyEnd = l.off
		}
		if l.eof() {
			break
		}
		l.advance() // consume the line terminator
		l.atLineStart = true
		// The body ends at a blank line, a separator, or end of input. The
		// terminator itself is not body content and is left unconsumed.
		i := l.off
		for i < len(l.src) && isSpaceTab(l.src[i]) {
			i++
		}
		if i >= len(l.src) || isNewline(l.src[i]) {
			break
		}
		if i+2 < len(l.src) && l.src[i] == '#' && l.src[i+1] == '#' && l.src[i+2] == '#' {
			break
		}
	}
	l.state = stLineStart
	l.atLineStart = true
	return l.mkSpan(TokBodyOpen, bodyStart, bsl, bsc, bodyEnd)
}

// nextSegment emits one literal run or interpolation anchor of a single-line
// value. Values end at a newline, at a space (targets only), or at EOF.
func (l *Lexer) nextSegment() Token {
	s, sl, sc := l.off, l.line, l.col
	for !l.eof() {
		c := l.cur()
		if isNewline(c) || (l.segStopAtSpace && c == ' ') {
			if l.off > s {
				tok := l.mk(l.segKind, s, sl, sc)
				l.endValue(c)
				return tok
			}
			l.endValue(c)
			return l.next() // empty value: resume at the state machine
		}
		if c == '{' {
			if p, ok := l.peek(1); ok && p == '{' {
				if l.off > s {
					return l.mk(l.segKind, s, sl, sc) // literal run before the anchor
				}
				return l.interpToken()
			}
		}
		l.advance()
	}
	if l.off > s {
		tok := l.mk(l.segKind, s, sl, sc)
		l.endValue(0)
		return tok
	}
	l.endValue(0)
	return l.mk(TokEOF, l.off, l.line, l.col)
}

func (l *Lexer) endValue(terminator byte) {
	l.segActive = false
	if l.segStopAtSpace && terminator == ' ' {
		l.wantHTTPVersion = true // an HTTP/x.y suffix may follow the target
		return
	}
	l.state = stHeaders
}

func (l *Lexer) interpToken() Token {
	s, sl, sc := l.off, l.line, l.col
	l.advance() // '{'
	l.advance() // '{'
	for !l.eof() {
		if l.cur() == '}' {
			if p, ok := l.peek(1); ok && p == '}' {
				l.advance()
				l.advance()
				return l.mk(TokInterp, s, sl, sc)
			}
		}
		if isNewline(l.cur()) { // anchors are single-line
			return l.illegal(ErrIllegalByte, s, sl, sc)
		}
		l.advance()
	}
	return l.illegal(ErrIllegalByte, s, sl, sc) // unterminated anchor
}

// probeBody decides the body kind after the header-terminating blank line,
// without consuming input. A "###" line or end of input means "no body".
func (l *Lexer) probeBody() {
	i := l.off
	for i < len(l.src) && isSpaceTab(l.src[i]) {
		i++
	}
	if i >= len(l.src) {
		l.state = stLineStart
		return
	}
	switch l.src[i] {
	case '{':
		l.state = stBodyJSON
		l.bodyDepth = 0
	case '<':
		l.state = stBodyXML
		l.bodyDepth = 0
	case '#':
		if i+2 < len(l.src) && l.src[i] == '#' && l.src[i+1] == '#' && l.src[i+2] == '#' {
			l.state = stLineStart // separator: no body
			return
		}
		l.state = stBodyRaw
	default:
		l.state = stBodyRaw
	}
}

// xmlTag is the result of scanning one XML tag.
type xmlTag struct {
	span        Token // span of the entire tag (used for the root element)
	slashStart  int   // offset of a trailing '/', or -1
	slashLine   int
	slashCol    int
	selfClosing bool // the tag ends with "/>"
	complete    bool // the tag was terminated before end of input
}

// isXMLProlog reports whether off is at an XML prolog construct (comment,
// CDATA section, processing instruction, or declaration) rather than an element.
func (l *Lexer) isXMLProlog() bool {
	if l.eof() || l.cur() != '<' {
		return false
	}
	return l.hasPrefix("<!--") || l.hasPrefix("<![CDATA[") || l.hasPrefix("<?") || l.hasPrefix("<!")
}

// scanXMLTag consumes one tag beginning at '<'. A tag is self-closing when its
// '>' is immediately preceded by '/'.
func (l *Lexer) scanXMLTag() xmlTag {
	s, sl, sc := l.off, l.line, l.col
	l.advance() // '<'
	slashStart, slashLine, slashCol := -1, 0, 0
	for !l.eof() {
		switch c := l.cur(); c {
		case '"', '\'':
			l.skipQuoted(c)
		case '/':
			slashStart, slashLine, slashCol = l.off, l.line, l.col
			l.advance()
		case '>':
			l.advance()
			return xmlTag{
				span:        l.mk(TokBodyOpen, s, sl, sc),
				slashStart:  slashStart,
				slashLine:   slashLine,
				slashCol:    slashCol,
				selfClosing: slashStart >= 0 && slashStart == l.off-2,
				complete:    true,
			}
		default:
			l.advance()
		}
	}
	return xmlTag{
		span:       l.mk(TokBodyOpen, s, sl, sc),
		slashStart: slashStart,
		slashLine:  slashLine,
		slashCol:   slashCol,
		complete:   false,
	}
}

// ---- primitives -------------------------------------------------------------

func (l *Lexer) eof() bool { return l.off >= len(l.src) }

func (l *Lexer) cur() byte { return l.src[l.off] }

// peek returns the byte n positions ahead of off, if any.
func (l *Lexer) peek(n int) (byte, bool) {
	if l.off+n < len(l.src) {
		return l.src[l.off+n], true
	}
	return 0, false
}

// hasPrefix reports whether the source at off starts with s.
func (l *Lexer) hasPrefix(s string) bool {
	if l.off+len(s) > len(l.src) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if l.src[l.off+i] != s[i] {
			return false
		}
	}
	return true
}

// advance consumes one byte, maintaining 1-based line/column coordinates.
// A CRLF pair is consumed as a single newline.
func (l *Lexer) advance() {
	if l.off >= len(l.src) {
		return
	}
	switch c := l.src[l.off]; c {
	case '\n':
		l.off++
		l.line++
		l.col = 1
	case '\r':
		l.off++
		if l.off < len(l.src) && l.src[l.off] == '\n' {
			l.off++
		}
		l.line++
		l.col = 1
	default:
		l.off++
		l.col++
	}
}

func (l *Lexer) skipSpaceTab() {
	for !l.eof() && isSpaceTab(l.cur()) {
		l.advance()
	}
}

func (l *Lexer) skipToEOL() {
	for !l.eof() && !isNewline(l.cur()) {
		l.advance()
	}
}

func (l *Lexer) consumeMethodWord() {
	for !l.eof() && l.cur() >= 'A' && l.cur() <= 'Z' {
		l.advance()
	}
}

// skipJSONString consumes a JSON string literal including its quotes, honoring
// backslash escapes. An unterminated string consumes to end of input; the
// caller's end-of-input check reports the failure.
func (l *Lexer) skipJSONString() {
	l.advance() // opening quote
	for !l.eof() {
		c := l.cur()
		if c == '\\' {
			l.advance()
			if !l.eof() {
				l.advance()
			}
			continue
		}
		if c == '"' {
			l.advance()
			return
		}
		l.advance()
	}
}

// skipQuoted consumes a run delimited by q (XML attribute values; no escapes).
func (l *Lexer) skipQuoted(q byte) {
	l.advance() // opening quote
	for !l.eof() && l.cur() != q {
		l.advance()
	}
	if !l.eof() {
		l.advance() // closing quote
	}
}

// skipPast consumes input through the next occurrence of end and reports
// whether it was found. It always makes progress.
func (l *Lexer) skipPast(end string) bool {
	for !l.eof() {
		if l.hasPrefix(end) {
			for i := 0; i < len(end); i++ {
				l.advance()
			}
			return true
		}
		l.advance()
	}
	return false
}

// mk builds a token spanning [start, l.off).
func (l *Lexer) mk(kind TokenKind, start, startLine, startCol int) Token {
	return l.mkSpan(kind, start, startLine, startCol, l.off)
}

// mkSpan builds a token spanning [start, end) with the recorded start position.
func (l *Lexer) mkSpan(kind TokenKind, start, startLine, startCol, end int) Token {
	return Token{
		Kind:    kind,
		Start:   int32(start),
		End:     int32(end),
		Line:    int32(startLine),
		Col:     int32(startCol),
		EndLine: int32(l.line),
		EndCol:  int32(l.col),
		Depth:   int32(l.bodyDepth),
	}
}

func (l *Lexer) newlineToken(kind TokenKind) Token {
	s, sl, sc := l.off, l.line, l.col
	l.advance()
	l.atLineStart = true
	return l.mk(kind, s, sl, sc)
}

// illegal records the first diagnostic and returns a TokIllegal token. It
// forces at least one byte of progress so that no illegal token is zero-width
// (the sole zero-width token in the grammar is TokEOF).
func (l *Lexer) illegal(kind ErrKind, start, startLine, startCol int) Token {
	if l.off <= start {
		l.advance()
	}
	if !l.hasDiag {
		l.diag = Diag{
			Kind:  kind,
			Start: int32(start),
			End:   int32(l.off),
			Line:  int32(startLine),
			Col:   int32(startCol),
		}
		l.hasDiag = true
	}
	return l.mk(TokIllegal, start, startLine, startCol)
}

func (l *Lexer) emitPending(t Token) {
	l.pending = t
	l.hasPending = true
}

// ---- free helpers -----------------------------------------------------------

func isNewline(c byte) bool { return c == '\n' || c == '\r' }

func isSpaceTab(c byte) bool { return c == ' ' || c == '\t' }

// isMethodName reports whether b is a recognized HTTP method keyword. The
// switch over string(b) does not allocate (compiler optimization).
func isMethodName(b []byte) bool {
	switch string(b) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}
