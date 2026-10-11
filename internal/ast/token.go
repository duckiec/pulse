// Package ast implements the .http lexer, parser, and zero-allocation AST.
//
// Move 1 scope: the lexer only (ROADMAP.md §1.1). Tokens are 32-byte values
// carrying int32 offsets into a caller-owned source buffer; the token stream is
// produced through iter.Seq[Token] (Go 1.27) and performs zero heap
// allocations per token. The parser (Move 2) consumes this stream.
//
// Grammar targeted (JetBrains REST Client / .http standard):
//
//	document  := request*
//	request   := (variable | comment | separator)* requestline header* blankline body?
//	requestline := METHOD SP target (SP HTTP/x.y)?
//	header    := name ":" value
//	body      := json-body | xml-body | raw-body
package ast

// TokenKind classifies a lexeme produced by [Lexer].
type TokenKind uint32

const (
	// TokEOF is the zero-width terminal token emitted at the end of the source.
	// It is the only token permitted to have End == Start.
	TokEOF TokenKind = iota
	// TokSeparator is a request boundary: a line starting with "###".
	TokSeparator
	// TokComment is a comment line ("# ..."), including "# @name" and
	// "# @assert" directives, which the parser interprets (ROADMAP.md §1.1.3).
	TokComment
	// TokVariable is a variable definition line ("@name = value").
	TokVariable
	// TokMethod is an HTTP method keyword (GET, POST, PUT, PATCH, DELETE,
	// HEAD, OPTIONS).
	TokMethod
	// TokURL is a literal run inside a request target. {{...}} interpolations
	// inside the target are emitted as separate TokInterp tokens.
	TokURL
	// TokHTTPVersion is the optional "HTTP/x.y" suffix of a request line.
	TokHTTPVersion
	// TokHeaderName is a header field name, up to but excluding ':'.
	TokHeaderName
	// TokHeaderValue is a literal run inside a header value. {{...}}
	// interpolations inside the value are emitted as separate TokInterp tokens.
	TokHeaderValue
	// TokBodyOpen opens a structured body: '{' for JSON, '<' for XML. For raw
	// bodies it carries the entire body span and is the only body token
	// emitted for that request.
	TokBodyOpen
	// TokBodyClose closes a structured body: the matching '}' or the root
	// element's closing tag. For a self-closing XML root it spans the root tag.
	TokBodyClose
	// TokInterp is a "{{name}}" interpolation anchor. Emitted only inside
	// single-line values (request targets and header values); body anchors are
	// scanned by the parser from body spans (ROADMAP.md §1.1.7).
	TokInterp
	// TokNewline terminates a line that carried content.
	TokNewline
	// TokBlankLine is an empty line. In the headers state it terminates the
	// header block and introduces the body.
	TokBlankLine
	// TokIllegal marks a lexing failure. The stream stops at the first illegal
	// token; callers MUST inspect [Lexer.Diag] and fail closed (AGENTS.md §4.4).
	TokIllegal
)

// String implements fmt.Stringer for test output and diagnostics.
func (k TokenKind) String() string {
	switch k {
	case TokEOF:
		return "EOF"
	case TokSeparator:
		return "SEPARATOR"
	case TokComment:
		return "COMMENT"
	case TokVariable:
		return "VARIABLE"
	case TokMethod:
		return "METHOD"
	case TokURL:
		return "URL"
	case TokHTTPVersion:
		return "HTTPVERSION"
	case TokHeaderName:
		return "HEADERNAME"
	case TokHeaderValue:
		return "HEADERVALUE"
	case TokBodyOpen:
		return "BODYOPEN"
	case TokBodyClose:
		return "BODYCLOSE"
	case TokInterp:
		return "INTERP"
	case TokNewline:
		return "NEWLINE"
	case TokBlankLine:
		return "BLANKLINE"
	case TokIllegal:
		return "ILLEGAL"
	default:
		return "UNKNOWN"
	}
}

// Token is a lexeme record. It is exactly 32 bytes (asserted by tests):
// eight 4-byte fields, no padding. All positions are byte offsets or 1-based
// line/column coordinates into the source buffer passed to [New].
//
//	Start..End  half-open byte span; Start < End for every kind except TokEOF
//	Line/Col    position of Start
//	EndLine/EndCol  position of End (the byte following the lexeme)
//	Depth       body nesting depth at the token; 0 outside bodies
//
// The zero-allocation contract (ROADMAP.md §1.1.5): the lexer never allocates
// per token. Use [Token.Text] for allocation-free access to lexeme bytes.
type Token struct {
	Kind    TokenKind
	Start   int32
	End     int32
	Line    int32
	Col     int32
	EndLine int32
	EndCol  int32
	Depth   int32
}

// Text returns the token's source bytes as a sub-slice of src. It allocates
// nothing; the result aliases src and must not outlive it.
func (t Token) Text(src []byte) []byte { return src[t.Start:t.End] }

// ErrKind classifies a lexing diagnostic. Diagnostics are value types so that
// reporting one never allocates (ROADMAP.md §1.1.7).
type ErrKind uint8

const (
	// ErrNone is the zero value: no diagnostic.
	ErrNone ErrKind = iota
	// ErrIllegalByte is a byte sequence the grammar does not admit.
	ErrIllegalByte
	// ErrUnknownMethod is an uppercase word that is not an HTTP method.
	ErrUnknownMethod
	// ErrBadHeader is a header line without a well-formed name/colon.
	ErrBadHeader
	// ErrUnterminatedBody is a structured body whose closing delimiter is
	// missing before end of input.
	ErrUnterminatedBody
)

// String implements fmt.Stringer.
func (k ErrKind) String() string {
	switch k {
	case ErrIllegalByte:
		return "illegal byte"
	case ErrUnknownMethod:
		return "unknown method"
	case ErrBadHeader:
		return "malformed header line"
	case ErrUnterminatedBody:
		return "unterminated body"
	default:
		return "none"
	}
}

// Diag describes the first lexing failure. The offending lexeme is the source
// span [Start, End); callers slice it from the buffer they handed to [New].
type Diag struct {
	Kind       ErrKind
	Start, End int32
	Line, Col  int32
}

// Error implements error so diagnostics satisfy fail-closed call sites
// (AGENTS.md §4.4) without allocation.
func (d Diag) Error() string {
	return "ast: " + d.Kind.String()
}
