package fuzz

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// This is a parser for the SUBSET of YAML that OpenAPI documents use: nested
// block mappings, block sequences, scalars, flow mappings and sequences, quoted
// strings, comments, and multiple documents separated by `---`.
//
// # Why a subset parser and not a library
//
// Both `gopkg.in/yaml.v2` and `gopkg.in/yaml.v3` are on the permanently-banned
// list (AGENTS.md §2.2). A dependency cannot be waved through here.
//
// # Why a REFUSING subset parser is the right shape
//
// A lenient parser that mis-nests a schema produces fuzz payloads for
// constraints nobody declared, and reports them as findings — so the failure
// mode is confident and wrong rather than loud. Every construct this parser does
// not implement produces a typed error naming the line, and the fuzzer refuses
// to run on a document it could not fully read. An incomplete index is
// indistinguishable from a clean one, which is the worst property a boundary
// tester can have.
//
// Explicitly unsupported, and each an error rather than a silent misparse:
// anchors and aliases (`&`, `*`), tags (`!!str`), block scalars (`|`, `>`), and
// complex keys (`? `). None appears in a machine-generated OpenAPI document.

// yamlLine is one significant input line.
type yamlLine struct {
	num    int    // 1-based, for diagnostics
	indent int    // leading spaces
	text   string // content with indentation and trailing comment stripped
}

// parseDocument decodes an OpenAPI document in JSON or YAML.
//
// The format is chosen by the first significant byte rather than by extension:
// a `.yaml` file containing JSON (or a `.json` file a generator mislabelled)
// must not need the caller to know which.
func parseDocument(raw []byte) (any, error) {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if trimmed == "" {
		return nil, &ErrUnsupportedDocument{Why: "the document is empty"}
	}
	switch trimmed[0] {
	case '{', '[':
		return parseJSONDocument(raw)
	}
	return parseYAMLDocument(raw)
}

// parseJSONDocument decodes with `encoding/json`.
//
// Numbers are decoded as float64 through the generic path, which is what the
// schema reader expects; using a decoder with UseNumber would require every
// constraint to handle json.Number, and the only place a number's exact type
// matters is the boundary generator, which normalises to float64 itself.
func parseJSONDocument(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, &ErrUnsupportedDocument{Why: "invalid JSON: " + err.Error()}
	}
	return v, nil
}

// parseYAMLDocument parses the supported YAML subset.
func parseYAMLDocument(raw []byte) (any, error) {
	lines, err := scanYAMLLines(string(raw))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, &ErrUnsupportedDocument{Why: "the document has no content"}
	}
	p := &yamlParser{lines: lines}
	node, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, &ErrUnsupportedDocument{
			Why: fmt.Sprintf("line %d: unexpected indentation", p.lines[p.pos].num),
		}
	}
	return node, nil
}

// scanYAMLLines strips comments, blanks and document markers, and records the
// indentation of each surviving line.
func scanYAMLLines(src string) ([]yamlLine, error) {
	var out []yamlLine
	inBlock := false // inside a multi-line quoted scalar
	var quote byte

	for i, raw := range strings.Split(src, "\n") {
		num := i + 1
		line := strings.TrimRight(raw, "\r")

		if inBlock {
			if strings.Contains(line, string(quote)) {
				inBlock, quote = false, 0
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			continue // document markers: only the first document is read
		}
		if strings.HasPrefix(trimmed, "&") || strings.HasPrefix(trimmed, "*") {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: YAML anchors and aliases are not supported", num),
			}
		}
		if strings.HasPrefix(trimmed, "? ") || trimmed == "?" {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: YAML complex keys are not supported", num),
			}
		}
		if trimmed == "|" || trimmed == ">" || strings.HasPrefix(trimmed, "|") ||
			strings.HasPrefix(trimmed, ">") {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: YAML block scalars are not supported", num),
			}
		}

		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if indent < len(line) && line[indent] == '\t' {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: tabs may not be used for indentation", num),
			}
		}
		content := stripYAMLComment(line[indent:])
		content = strings.TrimRight(content, " ")
		if content == "" {
			continue
		}
		out = append(out, yamlLine{num: num, indent: indent, text: content})
	}
	return out, nil
}

// stripYAMLComment removes a trailing `#` comment that is outside quotes.
//
// A `#` inside a quoted scalar is data. Stripping it would corrupt every
// pattern and example value in the document that happens to contain one, and
// the corruption would be invisible.
func stripYAMLComment(s string) string {
	var inS, inD bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '\\':
			if inD {
				i++ // skip the escaped character
			}
		case '#':
			if !inS && !inD && (i == 0 || s[i-1] == ' ') {
				return s[:i]
			}
		}
	}
	return s
}

// yamlParser walks the scanned lines.
type yamlParser struct {
	lines []yamlLine
	pos   int
}

// parseBlock parses every line at exactly the given indent into either a mapping
// or a sequence.
func (p *yamlParser) parseBlock(indent int) (any, error) {
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	if strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-" {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

// parseMap parses `key: value` lines at the given indent.
func (p *yamlParser) parseMap(indent int) (any, error) {
	out := map[string]any{}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: unexpected indentation", ln.num),
			}
		}
		key, rest, err := splitYAMLKey(ln.text, ln.num)
		if err != nil {
			return nil, err
		}
		p.pos++

		if rest != "" {
			v, err := parseScalarOrFlow(rest, ln.num)
			if err != nil {
				return nil, err
			}
			out[key] = v
			continue
		}
		// The value is a nested block, or explicitly null.
		if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			child, err := p.parseBlock(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			out[key] = child
			continue
		}
		if p.pos < len(p.lines) && p.lines[p.pos].indent == indent &&
			(strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-") {
			// A sequence may be written at its parent key's indentation, which
			// is legal YAML and common in generated OpenAPI documents.
			child, err := p.parseSeq(indent)
			if err != nil {
				return nil, err
			}
			out[key] = child
			continue
		}
		out[key] = nil
	}
	return out, nil
}

// parseSeq parses `- ...` lines at the given indent.
func (p *yamlParser) parseSeq(indent int) (any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent || (!strings.HasPrefix(ln.text, "- ") && ln.text != "-") {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: unexpected entry in a sequence", ln.num),
			}
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))

		if rest == "" {
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.parseBlock(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, child)
				continue
			}
			out = append(out, nil)
			continue
		}

		// `- key: value` opens a MAPPING whose first key is written on the dash
		// line. The dash and its following space are pure layout, so the item is
		// equivalent to a line at indent+2 whose text is the remainder, and the
		// item's remaining lines are already at that indent. Rewriting the dash
		// line in place and re-parsing handles multi-key items, nested mappings
		// and nested sequences through one code path — the alternative, splicing
		// line slices, is where the off-by-one in the original lived.
		if key, _, err := tryYAMLKey(rest); err == nil && key != "" {
			itemIndent := ln.indent + 2
			p.lines[p.pos] = yamlLine{num: ln.num, indent: itemIndent, text: rest}
			child, err := p.parseMap(itemIndent)
			if err != nil {
				return nil, err
			}
			out = append(out, child)
			continue
		}

		p.pos++
		v, err := parseScalarOrFlow(rest, ln.num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// splitYAMLKey splits `key: value`, honouring quoted keys.
func splitYAMLKey(text string, num int) (string, string, error) {
	k, rest, err := tryYAMLKey(text)
	if err != nil {
		return "", "", &ErrUnsupportedDocument{
			Why: fmt.Sprintf("line %d: %v", num, err),
		}
	}
	if k == "" {
		return "", "", &ErrUnsupportedDocument{
			Why: fmt.Sprintf("line %d: %q is not a `key: value` pair", num, text),
		}
	}
	return k, rest, nil
}

// tryYAMLKey returns the key and the remainder of a `key: value` line, or an
// empty key when the line is not a mapping entry.
func tryYAMLKey(text string) (string, string, error) {
	var inS, inD bool
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '\\':
			if inD {
				i++
			}
		case ':':
			if inS || inD {
				continue
			}
			// A colon is a separator only when followed by a space or EOL.
			if i+1 < len(text) && text[i+1] != ' ' {
				continue
			}
			key := strings.TrimSpace(text[:i])
			if key == "" {
				return "", "", nil
			}
			if unq, err := unquoteYAML(key); err == nil {
				key = unq
			}
			return key, strings.TrimSpace(text[i+1:]), nil
		}
	}
	return "", "", nil
}

// unquoteYAML removes surrounding quotes and unescapes a double-quoted scalar.
func unquoteYAML(s string) (string, error) {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return s, nil
		}
		return out, nil
	}
	return s, nil
}

// parseScalarOrFlow parses an inline value: a flow mapping, a flow sequence, or
// a scalar.
func parseScalarOrFlow(s string, num int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "{"):
		v, rest, err := parseFlow(s, '{', '}')
		if err != nil {
			return nil, &ErrUnsupportedDocument{Why: fmt.Sprintf("line %d: %v", num, err)}
		}
		if strings.TrimSpace(string(rest)) != "" {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: trailing text after a flow mapping", num),
			}
		}
		return v, nil
	case strings.HasPrefix(s, "["):
		v, rest, err := parseFlow(s, '[', ']')
		if err != nil {
			return nil, &ErrUnsupportedDocument{Why: fmt.Sprintf("line %d: %v", num, err)}
		}
		if strings.TrimSpace(string(rest)) != "" {
			return nil, &ErrUnsupportedDocument{
				Why: fmt.Sprintf("line %d: trailing text after a flow sequence", num),
			}
		}
		return v, nil
	}
	if unq, err := unquoteYAML(s); err == nil {
		return scalarValue(unq), nil
	}
	return scalarValue(s), nil
}

// parseFlow parses a flow collection, returning the value and the unconsumed
// remainder.
func parseFlow(s string, open, close byte) (any, string, error) {
	var items []string
	i := 1
	depth := 0
	var cur strings.Builder
	var inS, inD bool
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			items = append(items, t)
		}
		cur.Reset()
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case inS:
			cur.WriteByte(c)
			if c == '\'' {
				inS = false
			}
			continue
		case inD:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if c == '"' {
				inD = false
			}
			continue
		case c == '\'':
			inS = true
			cur.WriteByte(c)
			continue
		case c == '"':
			inD = true
			cur.WriteByte(c)
			continue
		case c == open || c == '{' || c == '[':
			depth++
			cur.WriteByte(c)
			continue
		case c == '}' || c == ']':
			if c == close && depth == 0 {
				flush()
				return flowValue(items, open), s[i+1:], nil
			}
			depth--
			cur.WriteByte(c)
			continue
		case c == ',' && depth == 0:
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	return nil, "", fmt.Errorf("unterminated flow collection")
}

// flowValue converts the collected items into a map or a slice.
func flowValue(items []string, open byte) any {
	if open == '{' {
		out := map[string]any{}
		for _, it := range items {
			k, v, err := tryYAMLKey(it)
			if err != nil || k == "" {
				continue
			}
			if val, err := parseScalarOrFlow(v, 0); err == nil {
				out[k] = val
			}
		}
		return out
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		if val, err := parseScalarOrFlow(it, 0); err == nil {
			out = append(out, val)
		}
	}
	return out
}

// scalarValue applies YAML's core scalar resolution to a bare token.
func scalarValue(s string) any {
	if s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL" {
		return nil
	}
	if s == "true" || s == "True" || s == "TRUE" || s == "yes" || s == "Yes" || s == "on" || s == "On" {
		return true
	}
	if s == "false" || s == "False" || s == "FALSE" || s == "no" || s == "No" || s == "off" || s == "Off" {
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return float64(n)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && looksNumeric(s) {
		return f
	}
	return s
}

// looksNumeric guards ParseFloat, which accepts forms YAML does not treat as
// numbers (`Inf`, `NaN`, and hex float literals).
func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' ||
			c == 'e' || c == 'E' {
			continue
		}
		return false
	}
	return strings.ContainsAny(s, "0123456789")
}
