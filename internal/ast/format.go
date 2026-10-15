package ast

import "errors"

// errFormatUnknownMethod is returned when a request carries MethodUnknown.
// The lexer only admits the seven standard methods, so this fires solely on a
// hand-built Request — emitting "UNKNOWN" would produce a file that no longer
// parses, so formatting fails closed instead.
var errFormatUnknownMethod = errors.New("ast: format: unknown method")

// PlainVar is a document-independent `@name = value` definition, for callers
// (composer, cURL importer) that build requests without source spans.
type PlainVar struct {
	Name, Value string
}

// PlainHeader is a document-independent header line. Disabled renders as a
// `#`-commented line placed ahead of the request (never inside the header
// block: the parser rejects `#` lines between the method line and the blank
// line, so a disabled header there would not re-parse). Kept in the file,
// never sent.
type PlainHeader struct {
	Name, Value string
	Disabled    bool
}

// PlainRequest is a document-independent request, for callers that build
// requests without source spans. Vars holds in-block `@var` definitions,
// emitted ahead of the directives so a re-parse keeps them request-scoped.
type PlainRequest struct {
	Name     string
	Method   string
	URL      string
	WasmSign string
	Headers  []PlainHeader
	Vars     []PlainVar
	Asserts  []string
	Body     string
}

// RequestName returns the `# @name` of r, or "" when the request is unnamed
// (NameStart is -1: pending names initialize unset and only set on `# @name`).
func (d *Document) RequestName(r *Request) string {
	if r == nil || r.NameStart < 0 || r.NameEnd <= r.NameStart {
		return ""
	}
	return d.Str(r.NameStart, r.NameEnd)
}

// FormatRequest appends the canonical `.http` block for r (no trailing
// newline; the caller joins blocks with separators). Emission order is fixed:
// in-block vars, `# @name`, `# @assert` directives, `@wasm-sign`, the request
// line, headers, a blank line, then the body. Appends only — zero allocation
// beyond the growth of b itself.
func (d *Document) FormatRequest(b []byte, r *Request) ([]byte, error) {
	if r == nil || r.Method == MethodUnknown {
		return b, errFormatUnknownMethod
	}
	// Direct span emission: routing through PlainRequest slices would heap-
	// allocate one backing array per non-empty section, so the hot path
	// writes every field straight from its arena view into b.
	for _, v := range d.VarsOf(r) {
		b = appendVarLine(b, d.Str(v.NameStart, v.NameEnd), d.Str(v.ValStart, v.ValEnd))
	}
	if name := d.RequestName(r); name != "" {
		b = append(b, "# @name "...)
		b = append(b, name...)
		b = append(b, '\n')
	}
	for _, a := range d.AssertsOf(r) {
		b = append(b, "# @assert "...)
		b = append(b, d.Str(a.Start, a.End)...)
		b = append(b, '\n')
	}
	if s := d.WasmSign(r); s != "" {
		b = append(b, "@wasm-sign "...)
		b = append(b, s...)
		b = append(b, '\n')
	}
	b = append(b, r.Method.String()...)
	b = append(b, ' ')
	b = append(b, d.Str(r.URLStart, r.URLEnd)...)
	b = append(b, '\n')
	for _, h := range r.Headers {
		b = append(b, d.Str(h.NameStart, h.NameEnd)...)
		b = append(b, ':')
		if h.ValStart >= 0 && h.ValEnd > h.ValStart {
			b = append(b, ' ')
			b = append(b, d.Str(h.ValStart, h.ValEnd)...)
		}
		b = append(b, '\n')
	}
	b = append(b, '\n')
	if r.Body.Kind != BodyNone {
		b = append(b, d.BodyText(r.Body)...)
	}
	return b, nil
}

// appendVarLine emits one `@name = value` definition line.
func appendVarLine(b []byte, name, value string) []byte {
	b = append(b, '@')
	b = append(b, name...)
	b = append(b, " = "...)
	b = append(b, value...)
	return append(b, '\n')
}

// FormatPlainRequest appends the canonical block for a document-independent
// request. Unknown or empty methods fail closed: the method line is the one
// line a re-parse cannot recover from a guess.
func FormatPlainRequest(b []byte, p PlainRequest) ([]byte, error) {
	switch p.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return b, errFormatUnknownMethod
	}
	for _, h := range p.Headers {
		if !h.Disabled {
			continue
		}
		b = append(b, "# "...)
		b = append(b, h.Name...)
		b = append(b, ':')
		if h.Value != "" {
			b = append(b, ' ')
			b = append(b, h.Value...)
		}
		b = append(b, '\n')
	}
	for _, v := range p.Vars {
		b = append(b, '@')
		b = append(b, v.Name...)
		b = append(b, " = "...)
		b = append(b, v.Value...)
		b = append(b, '\n')
	}
	if p.Name != "" {
		b = append(b, "# @name "...)
		b = append(b, p.Name...)
		b = append(b, '\n')
	}
	for _, a := range p.Asserts {
		b = append(b, "# @assert "...)
		b = append(b, a...)
		b = append(b, '\n')
	}
	if p.WasmSign != "" {
		b = append(b, "@wasm-sign "...)
		b = append(b, p.WasmSign...)
		b = append(b, '\n')
	}
	b = append(b, p.Method...)
	b = append(b, ' ')
	b = append(b, p.URL...)
	b = append(b, '\n')
	for _, h := range p.Headers {
		if h.Disabled {
			continue
		}
		b = append(b, h.Name...)
		b = append(b, ':')
		if h.Value != "" {
			b = append(b, ' ')
			b = append(b, h.Value...)
		}
		b = append(b, '\n')
	}
	b = append(b, '\n')
	b = append(b, p.Body...)
	return b, nil
}

// FormatCollection appends the whole document in canonical form: file-scoped
// `@var` definitions, then every request joined by `###` separators, ending
// with a single newline. Disabled `#` headers and free comments have no AST
// representation, so they do not survive a full-document format — surgical
// edits that must preserve them belong to UpsertRequestInFile, which splices
// source spans instead.
func (d *Document) FormatCollection(b []byte) ([]byte, error) {
	for _, v := range d.Vars {
		b = appendVarLine(b, d.Str(v.NameStart, v.NameEnd), d.Str(v.ValStart, v.ValEnd))
	}
	if len(d.Vars) > 0 && len(d.reqs) > 0 {
		b = append(b, '\n')
	}
	for i := range d.reqs {
		if i > 0 {
			b = append(b, "###\n"...)
		}
		var err error
		if b, err = d.FormatRequest(b, &d.reqs[i]); err != nil {
			return b, err
		}
		b = append(b, '\n')
	}
	return b, nil
}
