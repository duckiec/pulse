// Package env implements Git-aware environment contexts: the `.env.pulse`
// INI-style file, its branch selectors, and the bounded `git rev-parse`
// lookup that selects a block.
//
// # Why this is a package and not a flag on internal/secrets
//
// A `.env.pulse` file is configuration, not a secret. It lives in the
// repository (a branch-specific BASE_URL is exactly the kind of thing a team
// wants reviewed in a diff), while `internal/secrets` exists to keep values
// OUT of the repository. Keeping the parser here means the secret-resolution
// chain never grows git knowledge, and the file's values only enter the chain
// through the composition root (AGENTS.md §3.1).
//
// # Scope
//
// The format is deliberately tiny and total:
//
//	[default]
//	BASE_URL = http://localhost:8080
//
//	[branch:staging]
//	BASE_URL = https://staging.internal.net
//
//	[branch:feat/*]
//	BASE_URL = https://preview-{{GIT_BRANCH}}.internal.net
//
// A value may reference `{{GIT_BRANCH}}`; it is the only variable hydrated
// here. Everything else interpolates at request time through the resolver
// chain, exactly like a `.http` file's `{{var}}`.
//
// Every input either parses into one Document or fails closed with a line
// number. There is no silent skip: a typo'd section or assignment must not
// read as "the variable is missing" three layers later.
package env

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// FileName is the conventional name in a workspace root.
const FileName = ".env.pulse"

// BranchVar is the reserved variable hydrated with the current branch. It is
// never read from the file; the file may reference it.
const BranchVar = "GIT_BRANCH"

// defaultSection is the fallback block name.
const defaultSection = "default"

// branchPrefix marks a branch selector inside a section header.
const branchPrefix = "branch:"

// Block is one section of a `.env.pulse` file.
type Block struct {
	// Pattern is the branch selector after `branch:`. Empty for [default].
	Pattern string
	// IsDefault marks the fallback block.
	IsDefault bool
	// Vars holds the block's assignments. Map order is irrelevant: overlay
	// order is the document's, not the map's.
	Vars map[string]string
	// Line is the section header's line, for diagnostics.
	Line int
}

// Document is a parsed `.env.pulse`.
type Document struct {
	// Default is the [default] block (empty when the file omits it).
	Default map[string]string
	// Blocks holds every [branch:...] section in file order.
	Blocks []Block
}

// Parse reads a `.env.pulse` stream. It never writes anywhere.
func Parse(r io.Reader) (*Document, error) {
	doc := &Document{Default: map[string]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// cur is an INDEX, not a pointer: appending the next section can
	// reallocate doc.Blocks, and a stale pointer would silently drop every
	// assignment after the first growth. -1 means the [default] block.
	cur := -1
	seenDefault := false
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		if strings.HasPrefix(text, "[") {
			if !strings.HasSuffix(text, "]") {
				return nil, fmt.Errorf("env: .env.pulse line %d: unterminated section header %q", line, text)
			}
			selector := strings.TrimSpace(text[1 : len(text)-1])
			switch {
			case selector == defaultSection:
				if seenDefault {
					return nil, fmt.Errorf("env: .env.pulse line %d: duplicate [default] section", line)
				}
				seenDefault = true
				cur = -1
			case strings.HasPrefix(selector, branchPrefix):
				pattern := strings.TrimSpace(strings.TrimPrefix(selector, branchPrefix))
				if pattern == "" {
					return nil, fmt.Errorf("env: .env.pulse line %d: empty branch selector", line)
				}
				for _, b := range doc.Blocks {
					if b.Pattern == pattern {
						return nil, fmt.Errorf("env: .env.pulse line %d: duplicate [branch:%s] section", line, pattern)
					}
				}
				doc.Blocks = append(doc.Blocks, Block{Pattern: pattern, Vars: map[string]string{}, Line: line})
				cur = len(doc.Blocks) - 1
			default:
				return nil, fmt.Errorf("env: .env.pulse line %d: unknown section %q (want [default] or [branch:<pattern>])", line, text)
			}
			continue
		}

		text = strings.TrimPrefix(text, "export ")
		eq := strings.IndexByte(text, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("env: .env.pulse line %d: expected KEY=value", line)
		}
		key := strings.TrimSpace(text[:eq])
		if !validKey(key) {
			return nil, fmt.Errorf("env: .env.pulse line %d: invalid key %q", line, key)
		}
		if cur < 0 {
			if _, dup := doc.Default[key]; dup {
				return nil, fmt.Errorf("env: .env.pulse line %d: duplicate key %q in [default]", line, key)
			}
			doc.Default[key] = unquote(strings.TrimSpace(text[eq+1:]))
			continue
		}
		if _, dup := doc.Blocks[cur].Vars[key]; dup {
			return nil, fmt.Errorf("env: .env.pulse line %d: duplicate key %q in [branch:%s]", line, key, doc.Blocks[cur].Pattern)
		}
		doc.Blocks[cur].Vars[key] = unquote(strings.TrimSpace(text[eq+1:]))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("env: read .env.pulse: %w", err)
	}
	return doc, nil
}

// ParseFile reads path. A missing file is reported as an error wrapping
// os.ErrNotExist so the caller can distinguish "not configured" from
// "malformed" without string matching.
func ParseFile(path string) (*Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env: open %s: %w", path, err)
	}
	defer f.Close()
	doc, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	return doc, nil
}

// Effective returns the variables that apply to branch: [default] first, then
// every glob block that matches in file order, then every exact block that
// matches in file order. Exact selectors therefore always beat globs, and a
// later block overrides an earlier one.
//
// A value that references {{GIT_BRANCH}} with no branch is OMITTED rather than
// hydrated with an empty string: an empty host in a URL is a silent
// misconfiguration, and the lookup that follows will fail closed instead.
func (d *Document) Effective(branch string) map[string]string {
	if d == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(d.Default))
	apply := func(vars map[string]string) {
		for k, raw := range vars {
			if v, ok := hydrate(raw, branch); ok {
				out[k] = v
			}
		}
	}
	apply(d.Default)
	for _, b := range d.Blocks {
		if !isExact(b.Pattern) && MatchBranch(b.Pattern, branch) {
			apply(b.Vars)
		}
	}
	for _, b := range d.Blocks {
		if isExact(b.Pattern) && b.Pattern == branch {
			apply(b.Vars)
		}
	}
	return out
}

// Resolve returns one effective variable.
func (d *Document) Resolve(branch, key string) (string, bool) {
	v, ok := d.Effective(branch)[key]
	return v, ok
}

// MatchBranch reports whether pattern selects branch. `*` matches any run of
// bytes (including `/`, because branches are paths) and `?` matches one byte.
// A pattern with no wildcard is an exact match.
//
// It is an iterative glob with backtracking: a recursive glob over hostile
// input is a stack-overflow shape, and branch names come from a repository
// that others can push.
func MatchBranch(pattern, branch string) bool {
	if pattern == branch {
		return true
	}
	var p, b int
	star, mark := -1, 0
	for b < len(branch) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, b
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == branch[b]):
			p++
			b++
		case star >= 0:
			p = star + 1
			mark++
			b = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// isExact reports whether pattern carries no wildcard.
func isExact(pattern string) bool {
	return !strings.ContainsAny(pattern, "*?")
}

// hydrate expands the one reserved variable. ok is false when the value needs
// a branch and none was supplied.
func hydrate(v, branch string) (string, bool) {
	token := "{{" + BranchVar + "}}"
	if !strings.Contains(v, token) {
		return v, true
	}
	if branch == "" {
		return "", false
	}
	return strings.ReplaceAll(v, token, branch), true
}

// validKey matches the `.env` key grammar: letters, digits, `_`, `.`, `-`.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c == '_' || c == '.' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// unquote removes one layer of matching quotes and expands the escapes the
// `.env` loader supports, so the two file formats cannot disagree about what
// a quoted value means.
func unquote(v string) string {
	if len(v) >= 2 {
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
		if v[0] == '"' && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			inner = strings.ReplaceAll(inner, `\n`, "\n")
			inner = strings.ReplaceAll(inner, `\t`, "\t")
			inner = strings.ReplaceAll(inner, `\"`, `"`)
			inner = strings.ReplaceAll(inner, `\\`, `\`)
			return inner
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}
