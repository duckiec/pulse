package engine

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"pulse/internal/ast"
)

// Contract drift detection: compare a collection against an OpenAPI document
// without executing anything.
//
// # What it answers
//
// `pulse test` proves a collection still works against a live server. Drift
// asks a different question, offline: does the collection still describe
// endpoints and required inputs the contract actually declares? The three
// findings that matter in practice are a path+method the spec does not
// declare, an endpoint the spec has marked deprecated, and a request that
// omits a parameter the spec marks `required: true`.
//
// # Why the spec model is a flat operation list
//
// `internal/engine` may not import `internal/fuzz` (AGENTS.md §3.1: no sibling
// imports a sibling). The OpenAPI reader lives there, and the composition root
// translates its IR into the small [SpecOp] slice this package needs. That is
// also why this file contains no YAML, no `$ref` resolution and no schema
// traversal: those remain one implementation, in one package.
//
// # Matching is deliberately conservative
//
// A request target may still be a template (`{{base}}/users/42`). The origin
// anchor is dropped up to the first slash, an absolute URL contributes its
// path, and an unresolvable target is REPORTED rather than silently skipped —
// a drift run that checked nothing must not look like a drift run that found
// nothing.

// DriftKind classifies one finding.
type DriftKind string

const (
	// DriftUnknownEndpoint means no spec path+method matches the request.
	DriftUnknownEndpoint DriftKind = "unknown-endpoint"
	// DriftDeprecated means the matched operation is marked deprecated.
	DriftDeprecated DriftKind = "deprecated"
	// DriftMissingParam means a `required: true` query or header is absent.
	DriftMissingParam DriftKind = "missing-required-parameter"
	// DriftUnresolvedTarget means the request target could not be reduced to
	// a path, so it could not be checked at all.
	DriftUnresolvedTarget DriftKind = "unresolved-target"
)

// SpecOp is one operation the drift checker may match against. It is a
// projection of an OpenAPI operation: method, path, deprecation and the names
// of required query/header parameters. Everything else in the document is
// irrelevant to drift and is not carried.
type SpecOp struct {
	// Method is the HTTP method, e.g. "GET".
	Method string
	// Path is the spec path template, e.g. "/users/{id}".
	Path string
	// Deprecated mirrors the operation's `deprecated` flag.
	Deprecated bool
	// RequiredQuery lists query parameter names marked required.
	RequiredQuery []string
	// RequiredHeader lists header parameter names marked required.
	RequiredHeader []string
}

// DriftFinding is one violation, with the source line that caused it.
type DriftFinding struct {
	// File is the `.http` file path.
	File string
	// Line is the request line in that file.
	Line int32
	// Name is the request's `# @name`, when it has one.
	Name string
	// Method is the request method.
	Method string
	// Target is the request target as written (anchors intact).
	Target string
	// Kind classifies the finding.
	Kind DriftKind
	// Param is the missing parameter's name, for DriftMissingParam.
	Param string
	// In is the missing parameter's location ("query" or "header").
	In string
	// Detail is a complete, human-readable sentence.
	Detail string
}

// String renders one finding as `file:line: METHOD target [kind]: detail`.
// The kind is included so a CI log can be grouped without parsing prose.
func (f DriftFinding) String() string {
	return fmt.Sprintf("%s:%d: %s %s [%s]: %s", f.File, f.Line, f.Method, f.Target, f.Kind, f.Detail)
}

// DriftReport is the result of one drift run.
type DriftReport struct {
	// Files is the number of `.http` files checked.
	Files int
	// Requests is the number of requests checked.
	Requests int
	// Operations is the number of spec operations matched against.
	Operations int
	// Findings are ordered by file (sorted), then by source line.
	Findings []DriftFinding
}

// Clean reports whether the run found no violations.
func (r *DriftReport) Clean() bool { return len(r.Findings) == 0 }

// CheckDrift parses every file and compares each request against spec.
//
// A file that does not parse is a hard error: the collection is the input, and
// checking the files that happened to parse would report a green run over a
// collection Pulse could not read (AGENTS.md §4.5).
func CheckDrift(spec []SpecOp, files []string) (*DriftReport, error) {
	if len(files) == 0 {
		return nil, ErrNoFiles
	}
	idx := newSpecIndex(spec)
	rep := &DriftReport{Operations: idx.count()}
	for _, file := range files {
		doc, err := ast.ParseFile(file)
		if err != nil {
			return nil, err
		}
		rep.Files++
		for req := range doc.Requests() {
			rep.Requests++
			rep.Findings = append(rep.Findings, idx.check(file, doc, req)...)
		}
		doc.Release()
	}
	return rep, nil
}

// specIndex is the lookup structure for one spec. Paths are normalized; literal
// paths win over templates, and templates are scanned in sorted path order so
// the same document always produces the same match.
type specIndex struct {
	byPath map[string]map[string]*SpecOp
	paths  []string
}

func newSpecIndex(ops []SpecOp) *specIndex {
	idx := &specIndex{byPath: make(map[string]map[string]*SpecOp, len(ops))}
	for _, op := range ops {
		method := strings.ToUpper(strings.TrimSpace(op.Method))
		if method == "" || strings.TrimSpace(op.Path) == "" {
			continue
		}
		op.Method = method
		op.Path = normalizePath(op.Path)
		op.RequiredQuery = sortedUnique(op.RequiredQuery)
		op.RequiredHeader = sortedUnique(op.RequiredHeader)
		m, ok := idx.byPath[op.Path]
		if !ok {
			m = make(map[string]*SpecOp)
			idx.byPath[op.Path] = m
			idx.paths = append(idx.paths, op.Path)
		}
		cp := op
		m[method] = &cp
	}
	sort.Strings(idx.paths)
	return idx
}

func (idx *specIndex) count() int {
	n := 0
	for _, m := range idx.byPath {
		n += len(m)
	}
	return n
}

// lookup returns the matching operation (nil when the path matches but the
// method does not) and the spec path that matched ("" when no path matches).
func (idx *specIndex) lookup(path, method string) (*SpecOp, string) {
	if m, ok := idx.byPath[path]; ok {
		if op := m[method]; op != nil {
			return op, path
		}
		return nil, path
	}
	for _, tmpl := range idx.paths {
		if !strings.Contains(tmpl, "{") || !matchPath(tmpl, path) {
			continue
		}
		return idx.byPath[tmpl][method], tmpl
	}
	return nil, ""
}

func (idx *specIndex) methods(path string) []string {
	m := idx.byPath[path]
	out := make([]string, 0, len(m))
	for method := range m {
		out = append(out, method)
	}
	sort.Strings(out)
	return out
}

// check compares one request with the spec.
func (idx *specIndex) check(file string, doc *ast.Document, req *ast.Request) []DriftFinding {
	target := strings.TrimSpace(doc.Str(req.URLStart, req.URLEnd))
	method := req.Method.String()
	name := strings.TrimSpace(doc.Str(req.NameStart, req.NameEnd))
	base := DriftFinding{File: file, Line: req.Line, Name: name, Method: method, Target: target}

	path, rawQuery, ok := targetPath(target)
	if !ok {
		f := base
		f.Kind = DriftUnresolvedTarget
		f.Detail = "cannot determine a path from this target; give it an absolute URL or a /path"
		return []DriftFinding{f}
	}
	op, specPath := idx.lookup(path, method)
	if op == nil {
		f := base
		f.Kind = DriftUnknownEndpoint
		if specPath == "" {
			f.Detail = fmt.Sprintf("no path in the spec matches %q", path)
		} else {
			f.Detail = fmt.Sprintf("the spec declares %s for %s, not %s",
				strings.Join(idx.methods(specPath), ", "), specPath, method)
		}
		return []DriftFinding{f}
	}

	var out []DriftFinding
	if op.Deprecated {
		f := base
		f.Kind = DriftDeprecated
		f.Detail = fmt.Sprintf("%s %s is marked deprecated in the spec", op.Method, op.Path)
		out = append(out, f)
	}

	query := queryNames(rawQuery)
	for _, param := range op.RequiredQuery {
		if query[param] {
			continue
		}
		f := base
		f.Kind = DriftMissingParam
		f.Param, f.In = param, "query"
		f.Detail = fmt.Sprintf("missing required query parameter %q", param)
		out = append(out, f)
	}
	headers := headerNames(doc, req)
	for _, param := range op.RequiredHeader {
		if headers[strings.ToLower(param)] {
			continue
		}
		f := base
		f.Kind = DriftMissingParam
		f.Param, f.In = param, "header"
		f.Detail = fmt.Sprintf("missing required header %q", param)
		out = append(out, f)
	}
	return out
}

// targetPath reduces a request target to its normalized path and raw query.
//
// Absolute URLs are parsed; an unresolved anchor followed by a slash is dropped
// up to that slash (`{{base}}/users` -> `/users`); a bare `/path` is used as is.
// Anything else cannot be checked, and says so rather than guessing.
func targetPath(target string) (path, rawQuery string, ok bool) {
	t := strings.TrimSpace(target)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	if t == "" {
		return "", "", false
	}
	if strings.Contains(t, "://") {
		u, err := url.Parse(t)
		if err != nil || u.Host == "" {
			return "", "", false
		}
		return normalizePath(u.Path), u.RawQuery, true
	}
	if strings.HasPrefix(t, "/") {
		if i := strings.IndexByte(t, '?'); i >= 0 {
			return normalizePath(t[:i]), t[i+1:], true
		}
		return normalizePath(t), "", true
	}
	if strings.HasPrefix(t, "{{") {
		end := strings.Index(t, "}}")
		if end >= 0 {
			rest := t[end+2:]
			if i := strings.IndexByte(rest, '#'); i >= 0 {
				rest = rest[:i]
			}
			query := ""
			if i := strings.IndexByte(rest, '?'); i >= 0 {
				rest, query = rest[:i], rest[i+1:]
			}
			switch {
			case rest == "":
				return "/", query, true
			case strings.HasPrefix(rest, "/"):
				return normalizePath(rest), query, true
			}
		}
	}
	return "", "", false
}

// normalizePath gives every path a leading slash and no trailing slash (except
// the root). `/users` and `/users/` are the same endpoint; treating them as
// different would produce a drift finding for a trailing-slash style choice.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// matchPath reports whether a concrete path matches a spec template. A segment
// written `{id}` matches any non-empty segment; a segment still carrying an
// interpolation anchor (`{{id}}`) is treated the same way.
func matchPath(tmpl, got string) bool {
	ts := pathSegments(tmpl)
	gs := pathSegments(got)
	if len(ts) != len(gs) {
		return false
	}
	for i := range ts {
		if isDynamicSegment(ts[i]) {
			if gs[i] == "" {
				return false
			}
			continue
		}
		if ts[i] != gs[i] {
			return false
		}
	}
	return true
}

func pathSegments(p string) []string {
	return strings.Split(strings.TrimPrefix(normalizePath(p), "/"), "/")
}

func isDynamicSegment(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "{{") {
		return true
	}
	return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && len(s) > 2
}

// queryNames returns the set of query parameter names, presence-only. A
// parameter with an empty value (`?limit=`) is present: the contract asked for
// the name, not for a value this checker can validate.
func queryNames(raw string) map[string]bool {
	out := map[string]bool{}
	if raw == "" {
		return out
	}
	if vals, err := url.ParseQuery(raw); err == nil {
		for k := range vals {
			out[k] = true
		}
		return out
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '='); i >= 0 {
			part = part[:i]
		}
		if name, err := url.QueryUnescape(part); err == nil {
			out[name] = true
		} else {
			out[part] = true
		}
	}
	return out
}

// headerNames returns the lowercase header names the request declares.
func headerNames(doc *ast.Document, req *ast.Request) map[string]bool {
	out := make(map[string]bool, len(req.Headers))
	for _, h := range req.Headers {
		name := strings.ToLower(strings.TrimSpace(doc.Str(h.NameStart, h.NameEnd)))
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
