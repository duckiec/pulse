package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"pulse/internal/ast"
)

// This file is Phase 2.4's bidirectional AI authoring: six MCP tools that let
// a local agent read the grammar, write collections, and verify them — with
// every disk write passing the zero-trust chokepoint first.
//
// Layering: internal/mcp already imports internal/ast (the execute path
// parses with it), so the authoring tools reuse the Phase 2.1 write path
// (CreateCollectionFile / UpsertRequestInFile / DeleteRequestFromFile /
// ImportCurl / FormatPlainRequest) rather than reimplementing persistence.
// What mcp MUST NOT import is internal/secrets (AGENTS.md §3.1): the
// registered-secret probe arrives as an optional interface assertion, and
// the literal-credential heuristics are implemented here from the stdlib.

const (
	ToolGetHTTPSyntaxSpec = "get_http_syntax_spec"
	ToolCreateCollection  = "create_collection"
	ToolUpsertRequest     = "upsert_request"
	ToolDeleteRequest     = "delete_request"
	ToolImportCurl        = "import_curl"
	ToolValidateCollect   = "validate_collection"
)

// Authoring backs the six .http authoring tools. A nil *Authoring omits all
// six (an agent that cannot see a tool will not call it — the same
// degradation rule Config documents for the fuzz runner).
type Authoring struct {
	// Root is the workspace every relative path resolves inside. Empty
	// resolves to the process working directory per call, so tests may
	// chdir into a scratch workspace.
	Root string
	// Redactor scrubs tool output. It doubles as the registered-secret
	// probe behind the write chokepoint when it implements
	// Contains(string) bool (as *secrets.Redactor does).
	Redactor Redactor
	// Lookup probes whether a placeholder is sealed (keychain, env, or
	// vault tier). Nil means every non-file-local placeholder reports
	// missing — loud, never guessed present.
	Lookup func(name string) (string, error)
}

// secretScanner is the optional registered-secret probe. It is asserted,
// never required: *secrets.Redactor satisfies it, and so does any test
// double. A redactor without the probe still scrubs output; it just
// cannot answer "is this literal already a sealed value".
type secretScanner interface {
	Contains(s string) bool
}

// credentialRefused is the actionable refusal every chokepoint rejection
// carries verbatim. It names the remedy (placeholder + `pulse vault set`)
// because an agent that only learns "refused" will retry the same literal.
const credentialRefused = "Refused to write literal credential in header/body. " +
	"Replace the literal value with a {{SECRET_NAME}} placeholder and " +
	"ask the user to run pulse vault set SECRET_NAME."

// workspaceRoot resolves the workspace for one call.
func (a *Authoring) workspaceRoot() (string, error) {
	if a == nil || a.Root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", NewProtocolError(CodeInternalError, "the workspace directory could not be read")
		}
		return cwd, nil
	}
	return a.Root, nil
}

// confineAuthorPath resolves a workspace-relative collection path, failing
// closed on absolute paths, `..` escape, and NUL bytes — the same jail the
// TUI and CLI write paths enforce, so the agent cannot reach a stricter
// tool by choosing a more permissive one.
func confineAuthorPath(root, rel string) (string, error) {
	if rel == "" {
		return "", NewProtocolError(CodeInvalidParams, "path is required")
	}
	if strings.ContainsRune(rel, 0) {
		return "", NewProtocolError(CodeInvalidParams, "path carries a NUL byte")
	}
	if filepath.IsAbs(rel) {
		return "", NewProtocolError(CodeInvalidParams, "path must be workspace-relative")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", NewProtocolError(CodeInvalidParams, "path escapes the workspace")
	}
	return filepath.Join(root, clean), nil
}

// ---- the zero-trust write chokepoint ---------------------------------------

// credentialHeaders are header names whose values carry credential material
// by construction. The set unions the redactor's name list, the engine's,
// and the curl importer's sensitive map — three lists that must agree here
// or a value refused by one path sails through another.
var credentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true,
	"cookie": true, "set-cookie": true,
	"x-api-key": true, "api-key": true, "apikey": true,
	"x-auth-token": true, "x-token": true, "token": true,
}

// suspiciousParamNames marks query keys whose values are secrets by
// position, whatever they look like (mirrors the importer).
var suspiciousParamNames = []string{
	"token", "api_key", "apikey", "api-key", "secret", "password",
	"passwd", "auth", "access_key", "client_secret",
}

// stripPlaceholders removes every `{{...}}` span from s. What remains is
// literal author text: if anything but whitespace remains in a credential
// position, the value is (partly) literal and the write is refused.
func stripPlaceholders(s string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+2:]
		if j := strings.Index(rest, "}}"); j >= 0 {
			rest = rest[j+2:]
		} else {
			b.WriteString("{{")
			b.WriteString(rest)
			break
		}
	}
	return b.String()
}

// looksLikeDotEnvBlock reports the smuggled-plaintext shape: two or more
// UPPER_SNAKE=nonempty lines. One KEY=value line is a coincidence (a query
// string, a header fragment); two is a file being written through the agent.
func looksLikeDotEnvBlock(s string) bool {
	hits := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := line[:eq]
		if !isEnvKeyCaps(key) {
			continue
		}
		if strings.TrimSpace(line[eq+1:]) == "" {
			continue
		}
		hits++
		if hits >= 2 {
			return true
		}
	}
	return false
}

// isEnvKeyCaps admits strict capitals/digits/underscore, the shape of a
// committed secret name rather than prose.
func isEnvKeyCaps(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// hasBearerLiteral reports `Bearer <literal>` / `Basic <literal>` outside
// placeholders: the exact shape of a pasted token.
func hasBearerLiteral(s string) bool {
	lower := strings.ToLower(s)
	for _, scheme := range []string{"bearer ", "basic "} {
		from := 0
		for {
			i := strings.Index(lower[from:], scheme)
			if i < 0 {
				break
			}
			tail := strings.TrimSpace(s[from+i+len(scheme):])
			if tail != "" && !strings.HasPrefix(tail, "{{") {
				return true
			}
			from += i + len(scheme)
		}
	}
	return false
}

// scanWritePart runs every leg over one URL/header/body fragment. Any leg
// refusing aborts the whole disk write before a byte lands.
func (a *Authoring) scanWritePart(fragment string) error {
	if fragment == "" {
		return nil
	}
	// Leg 1: the registered-secret probe. A literal that equals a sealed
	// value is a credential even when no heuristic fires.
	if a != nil {
		if scanner, ok := a.Redactor.(secretScanner); ok && scanner != nil {
			if scanner.Contains(fragment) {
				return NewProtocolError(CodeInvalidParams, credentialRefused)
			}
		}
	}
	// Leg 2: credential headers with literal (non-placeholder) values.
	for _, line := range strings.Split(fragment, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t\"'<>") {
			continue // not a header line (request line, body prose, ...)
		}
		if !credentialHeaders[strings.ToLower(name)] {
			continue
		}
		// The value passes only when nothing but placeholders (and the
		// Bearer/Basic scheme word that frames them) remains: `Bearer
		// {{TOKEN}}` is the sanitized shape, `Bearer abc123` is a
		// pasted credential.
		fields := strings.Fields(stripPlaceholders(value))
		if len(fields) > 0 && (strings.EqualFold(fields[0], "bearer") || strings.EqualFold(fields[0], "basic")) {
			fields = fields[1:]
		}
		if len(fields) > 0 {
			return NewProtocolError(CodeInvalidParams, credentialRefused)
		}
	}
	// Leg 3: a smuggled .env payload.
	if looksLikeDotEnvBlock(fragment) {
		return NewProtocolError(CodeInvalidParams, credentialRefused)
	}
	// Leg 4: a pasted Bearer/Basic token.
	if hasBearerLiteral(fragment) {
		return NewProtocolError(CodeInvalidParams, credentialRefused)
	}
	// Leg 5: userinfo in a URL (https://user:pass@host is a credential in
	// the address itself). Placeholder userinfo ({{...}}) passes. Fields
	// are scanned rather than lines because the URL rides inside a
	// `METHOD URL` request line, not on its own.
	for _, field := range strings.Fields(fragment) {
		if !strings.HasPrefix(field, "http://") && !strings.HasPrefix(field, "https://") {
			continue
		}
		after := strings.SplitN(field, "://", 2)[1]
		authority := after
		if i := strings.Index(authority, "/"); i >= 0 {
			authority = authority[:i]
		}
		if at := strings.Index(authority, "@"); at >= 0 {
			if userinfo := authority[:at]; strings.Contains(userinfo, ":") && !strings.Contains(userinfo, "{{") {
				return NewProtocolError(CodeInvalidParams, credentialRefused)
			}
		}
	}
	return nil
}

// scanWrite is the chokepoint every disk-writing authoring tool calls BEFORE
// any write: URL, headers, and body each pass all five legs, and the first
// refusal aborts the whole operation.
func (a *Authoring) scanWrite(parts ...string) error {
	for _, p := range parts {
		if err := a.scanWritePart(p); err != nil {
			return err
		}
	}
	return nil
}

// ---- tool registration -----------------------------------------------------

// RegisterAuthoringTools installs the six bidirectional-authoring tools. A
// nil authoring config omits all six; a config with an empty Root scopes to
// the process working directory per call.
func RegisterAuthoringTools(srv *mcpserver.MCPServer, a *Authoring) error {
	if srv == nil {
		return fmt.Errorf("mcp: nil server")
	}
	if a == nil {
		return nil
	}

	spec := mcp.NewTool(ToolGetHTTPSyntaxSpec,
		mcp.WithDescription("Return the authoritative .http file grammar and {{VAR}} rules. Consult this before writing any collection so generated syntax never drifts."))
	srv.AddTool(spec, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(httpSyntaxSpec)},
			StructuredContent: map[string]any{"spec": httpSyntaxSpec},
		}, nil
	})

	create := mcp.NewTool(ToolCreateCollection,
		mcp.WithDescription("Create a new .http collection file inside the workspace. Refuses literal credentials; use {{SECRET_NAME}} placeholders and ask the user to run pulse vault set SECRET_NAME."),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Workspace-relative .http file to create (never overwritten).")),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("Full .http source. Must parse; literal secrets are refused.")))
	srv.AddTool(create, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		rel, err := stringArg(args, "path", true)
		if err != nil {
			return errorResult(err), nil
		}
		content, err := stringArg(args, "content", true)
		if err != nil {
			return errorResult(err), nil
		}
		if len(content) > maxPayloadBytes {
			return errorResult(WrapProtocolError(CodeInvalidParams, "content exceeds the size limit",
				fmt.Errorf("%d bytes; the limit is %d", len(content), maxPayloadBytes))), nil
		}
		root, err := a.workspaceRoot()
		if err != nil {
			return errorResult(err), nil
		}
		if _, err := confineAuthorPath(root, rel); err != nil {
			return errorResult(err), nil
		}
		if err := a.scanWrite(content); err != nil {
			return errorResult(err), nil
		}
		if err := ast.CreateCollectionFile(root, rel, []byte(content)); err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent("created " + rel + "\n")},
			StructuredContent: map[string]any{"path": rel, "bytes": len(content)},
		}, nil
	})

	upsert := mcp.NewTool(ToolUpsertRequest,
		mcp.WithDescription("Create or replace one named request block (exactly one request) in a workspace .http file. Refuses literal credentials; use {{SECRET_NAME}} placeholders."),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Workspace-relative .http file (created when absent).")),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("The # @name of the request to replace, or the name a new trailing block takes.")),
		mcp.WithString("block",
			mcp.Required(),
			mcp.Description("Exactly one .http request block, including its # @name line.")))
	srv.AddTool(upsert, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		rel, err := stringArg(args, "path", true)
		if err != nil {
			return errorResult(err), nil
		}
		name, err := stringArg(args, "name", true)
		if err != nil {
			return errorResult(err), nil
		}
		block, err := stringArg(args, "block", true)
		if err != nil {
			return errorResult(err), nil
		}
		if len(block) > maxPayloadBytes {
			return errorResult(WrapProtocolError(CodeInvalidParams, "block exceeds the size limit",
				fmt.Errorf("%d bytes; the limit is %d", len(block), maxPayloadBytes))), nil
		}
		root, err := a.workspaceRoot()
		if err != nil {
			return errorResult(err), nil
		}
		if _, err := confineAuthorPath(root, rel); err != nil {
			return errorResult(err), nil
		}
		if err := a.scanWrite(block); err != nil {
			return errorResult(err), nil
		}
		if err := ast.UpsertRequestInFile(root, rel, name, []byte(block)); err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent("upserted " + name + " in " + rel + "\n")},
			StructuredContent: map[string]any{"path": rel, "name": name},
		}, nil
	})

	remove := mcp.NewTool(ToolDeleteRequest,
		mcp.WithDescription("Delete one named request block from a workspace .http file."),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Workspace-relative .http file.")),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("The # @name of the request to delete.")))
	srv.AddTool(remove, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		rel, err := stringArg(args, "path", true)
		if err != nil {
			return errorResult(err), nil
		}
		name, err := stringArg(args, "name", true)
		if err != nil {
			return errorResult(err), nil
		}
		root, err := a.workspaceRoot()
		if err != nil {
			return errorResult(err), nil
		}
		if _, err := confineAuthorPath(root, rel); err != nil {
			return errorResult(err), nil
		}
		if err := ast.DeleteRequestFromFile(root, rel, name); err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent("deleted " + name + " from " + rel + "\n")},
			StructuredContent: map[string]any{"path": rel, "name": name},
		}, nil
	})

	importCurl := mcp.NewTool(ToolImportCurl,
		mcp.WithDescription("Convert a raw curl command into a sanitized request block written to a workspace .http file. Credentials become {{PLACEHOLDER}} references; the raw values are never written, only their placeholder names are returned so the user can seal them with pulse vault set."),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("The raw curl command to import.")),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Workspace-relative .http file to write into (created when absent).")),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("The # @name the imported block takes.")))
	srv.AddTool(importCurl, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		command, err := stringArg(args, "command", true)
		if err != nil {
			return errorResult(err), nil
		}
		rel, err := stringArg(args, "path", true)
		if err != nil {
			return errorResult(err), nil
		}
		name, err := stringArg(args, "name", true)
		if err != nil {
			return errorResult(err), nil
		}
		if len(command) > maxPayloadBytes {
			return errorResult(WrapProtocolError(CodeInvalidParams, "command exceeds the size limit",
				fmt.Errorf("%d bytes; the limit is %d", len(command), maxPayloadBytes))), nil
		}
		root, err := a.workspaceRoot()
		if err != nil {
			return errorResult(err), nil
		}
		if _, err := confineAuthorPath(root, rel); err != nil {
			return errorResult(err), nil
		}
		plain, extracted, err := ast.ImportCurl(command)
		if err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		plain.Name = name
		block, err := ast.FormatPlainRequest(nil, plain)
		if err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		// Defense in depth: the importer already lifts credentials to
		// placeholders, and the chokepoint proves it before any byte lands.
		if err := a.scanWrite(string(block)); err != nil {
			return errorResult(err), nil
		}
		if err := ast.UpsertRequestInFile(root, rel, name, block); err != nil {
			return errorResult(mapAuthoringError(err)), nil
		}
		sealed := make([]map[string]string, 0, len(extracted))
		for _, s := range extracted {
			// Placeholder names and sources travel back; raw values NEVER do.
			sealed = append(sealed, map[string]string{
				"placeholder": s.Placeholder,
				"source":      s.Source,
				"seal_with":   "pulse vault set " + s.Placeholder,
			})
		}
		if sealed == nil {
			sealed = []map[string]string{}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{mcp.NewTextContent("imported " + name + " into " + rel + "\n")},
			StructuredContent: map[string]any{
				"path": rel, "name": name, "extracted_secrets": sealed,
			},
		}, nil
	})

	validate := mcp.NewTool(ToolValidateCollect,
		mcp.WithDescription("Validate workspace .http files: syntax diagnostics plus a present-vs-missing {{SECRET}} cross-check (key names only, never values). Omit path to check the whole workspace."),
		mcp.WithString("path",
			mcp.Description("Workspace-relative .http file or directory to check. Default: the whole workspace.")))
	srv.AddTool(validate, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		rel, err := stringArg(args, "path", false)
		if err != nil {
			return errorResult(err), nil
		}
		root, err := a.workspaceRoot()
		if err != nil {
			return errorResult(err), nil
		}
		target := root
		if rel != "" {
			abs, err := confineAuthorPath(root, rel)
			if err != nil {
				return errorResult(err), nil
			}
			target = abs
		}
		report, err := a.validateTarget(target, root)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(report.text)},
			StructuredContent: report.structured,
		}, nil
	})
	return nil
}

// mapAuthoringError translates a domain failure into its coded shape. Parse
// failures carry the parse code with the file:line detail; jail violations
// and missing entries are invalid params; anything else is internal.
func mapAuthoringError(err error) error {
	if err == nil {
		return nil
	}
	var se *ast.SyntaxError
	if errors.As(err, &se) {
		return WrapProtocolError(CodeParseFailed, "the .http source did not parse", err)
	}
	if errors.Is(err, ast.ErrRequestNotFound) || errors.Is(err, ast.ErrPathEscape) || errors.Is(err, ast.ErrFileExists) {
		return WrapProtocolError(CodeInvalidParams, err.Error(), nil)
	}
	return WrapProtocolError(CodeInternalError, "the workspace write failed", err)
}

// validationReport is the validate_collection result.
type validationReport struct {
	text       string
	structured map[string]any
}

// validateTarget checks one file or every .http file under a directory:
// syntax diagnostics plus the present-vs-missing placeholder cross-check.
// Only key names cross the boundary — never values.
func (a *Authoring) validateTarget(target, root string) (*validationReport, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, WrapProtocolError(CodeInvalidParams, "the validation target could not be read", redactPathErr(err))
	}
	var files []string
	if fi.IsDir() {
		files, err = collectAuthorHTTPFiles(target)
		if err != nil {
			return nil, WrapProtocolError(CodeInternalError, "the workspace could not be walked", err)
		}
	} else {
		files = []string{target}
	}
	var (
		diags    []string
		present  []string
		missing  []string
		seenP    = map[string]bool{}
		seenM    = map[string]bool{}
		fileList []string
	)
	for _, f := range files {
		rel := f
		if r, err := filepath.Rel(root, f); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = r
		}
		fileList = append(fileList, rel)
		raw, err := os.ReadFile(f)
		if err != nil {
			diags = append(diags, rel+": unreadable")
			continue
		}
		if _, perr := ast.ParseFile(f); perr != nil {
			diags = append(diags, rel+": "+perr.Error())
		}
		defined := map[string]bool{}
		for _, v := range authorDefinedVars(string(raw)) {
			defined[v] = true
		}
		for _, v := range extractPlaceholders(string(raw)) {
			if defined[v] {
				if !seenP[v] {
					seenP[v] = true
					present = append(present, v)
				}
				continue
			}
			if a.Lookup != nil {
				if _, lerr := a.Lookup(v); lerr == nil {
					if !seenP[v] {
						seenP[v] = true
						present = append(present, v)
					}
					continue
				}
			}
			if !seenM[v] {
				seenM[v] = true
				missing = append(missing, v)
			}
		}
	}
	sort.Strings(fileList)
	sort.Strings(diags)
	sort.Strings(present)
	sort.Strings(missing)
	if diags == nil {
		diags = []string{}
	}
	if present == nil {
		present = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) checked\n", len(files))
	for _, d := range diags {
		fmt.Fprintf(&b, "syntax: %s\n", d)
	}
	for _, p := range present {
		fmt.Fprintf(&b, "present: %s\n", p)
	}
	for _, m := range missing {
		fmt.Fprintf(&b, "missing: %s (seal with `pulse vault set %s <value>`)\n", m, m)
	}
	if len(diags) == 0 && len(missing) == 0 {
		b.WriteString("valid\n")
	}
	return &validationReport{
		text: b.String(),
		structured: map[string]any{
			"files":       fileList,
			"diagnostics": diags,
			"present":     present,
			"missing":     missing,
		},
	}, nil
}

// collectAuthorHTTPFiles returns the .http files under root, recursive and
// sorted, skipping version-control and dependency directories.
func collectAuthorHTTPFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".http") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// extractPlaceholders returns the distinct `{{name}}` anchors in s, in
// first-appearance order. It is local (not secrets.ExtractVariables) because
// internal/mcp must not import internal/secrets (AGENTS.md §3.1).
func extractPlaceholders(s string) []string {
	var out []string
	seen := map[string]bool{}
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return out
		}
		rest = rest[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return out
		}
		name := rest[:j]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		rest = rest[j+2:]
	}
}

// authorDefinedVars collects `@name = value` definitions so the cross-check
// never mistakes a file-local variable for a missing secret. A `@var` line
// without `=` defines nothing.
func authorDefinedVars(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "@") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "@"))
		name, _, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// httpSyntaxSpec is the authoritative grammar the agent reads before
// writing. It condenses docs/HTTP_SYNTAX.md to the rules an author needs;
// the doc remains the human reference, this string the machine one.
const httpSyntaxSpec = `Pulse .http grammar (authoritative for AI-written collections):

Separator: a line starting with '###' ends a request block.
Variables: '@name = value' defines a variable. Before the first '###' the
  scope is the whole file; after a separator the scope is the requests that
  follow. A '@var' line without '=' is a syntax error.
Names: '# @name my-request' attaches a name to the next request. Every
  block written by upsert_request MUST carry one (it is the edit key).
Assertions: '# @assert <expr>' attaches a check to the next request.
  Subjects: status, code, body, header <name>, duration.
  Operators: ==, !=, <, <=, >, >=, in lo..hi (inclusive), contains.
  Example: '# @assert status == 200'.
Request line: 'METHOD URL' with an optional 'HTTP/1.1' version token.
  Methods: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS.
Headers: 'Name: value' lines after the request line. A '#'-commented
  header is disabled (kept, never sent).
Body: after exactly one blank line, the raw body runs to the next '###'
  separator or end of file.
Interpolation: '{{var}}' in URLs, headers, and bodies resolves at runtime
  from the OS keychain, then the ephemeral .env, then the sealed vault —
  else a hard error. NEVER write a literal secret: use a {{SECRET_NAME}}
  placeholder and ask the user to run 'pulse vault set SECRET_NAME'.
Signing: a bare '@wasm-sign ./sign/auth.wasm' line before a request
  attaches a sandboxed signer; at most one per request.
Canonical order per block: @var lines, '# @name', '# @assert' lines,
  '@wasm-sign', the request line, headers, one blank line, then the body.
`
