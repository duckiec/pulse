package ui

import (
	"context"
	"fmt"
	"strings"

	"pulse/internal/ast"
)

// This file holds the blank-state scratchpad: the in-memory Untitled
// Request a fresh workspace boots into, the starter collection the [g]
// action generates, the error doctor's remediation text, and the yank
// register's curl builder. None touches disk or the network.

// UntitledTemplate is the in-memory request an empty workspace boots
// into. It targets loopback health: the cheapest possible first send,
// and a URL NormalizeURL accepts without rewriting.
func UntitledTemplate() string {
	return "# @name untitled\nGET http://localhost:8080/health HTTP/1.1\n\n###\n"
}

// NewFileTemplate seeds a freshly created collection: one untitled
// request so the file parses on creation and the tree has a row.
func NewFileTemplate() []byte {
	return []byte(UntitledTemplate())
}

// StarterTemplate is the [g] action's output: a health check plus a
// JSON create against a {{API_TOKEN}} the vault helper seals. Two
// requests, both named, both loopback — a greybeard's skeleton and a
// beginner's first green run in one file.
func StarterTemplate() []byte {
	return []byte(`@base = http://localhost:8080

# @name health
# @assert status == 200
GET {{base}}/health HTTP/1.1

### Create
# @name create-note
# @assert status == 201
POST {{base}}/notes HTTP/1.1
Content-Type: application/json
Authorization: Bearer {{API_TOKEN}}

{"title": "hello"}

###
`)
}

// ParseScratch builds the scratchpad document: the untitled template in
// a fresh arena the caller owns (release when the model is done with
// it). It cannot fail — the template is a constant proven by the
// scratchpad gate — but it returns the error rather than panicking, so
// a future template edit fails closed at boot instead of crashing it.
func ParseScratch() (*ast.Document, error) {
	src := UntitledTemplate()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		return nil, fmt.Errorf("ui: scratch template does not parse: %w", err)
	}
	doc.Path = ""
	return doc, nil
}

// NewScratchpad binds an in-memory untitled document into a model flagged
// for the welcome boot: tree with one row, request snapshotted, nothing
// to run against yet. The runner may be nil — sending with no runner
// fails closed in startRun like every other unbound state.
func NewScratchpad(baseCtx context.Context, runner Runner, doc *ast.Document, wsRoot string) *Model {
	m := New(baseCtx, runner, doc, false)
	m.scratch = true
	m.wsRoot = wsRoot
	m.saveTarget = "requests.http"
	// The scratch document is arena-owned like any editor-built one:
	// a later swap releases it, so it must be tracked as owned.
	m.ownedDoc = doc
	return m
}

// extractVar scans text for the first {{NAME}} anchor: the error
// doctor's diagnosis. It is a local 15-line scan rather than an import
// of internal/secrets because siblings never import siblings (AGENTS.md
// §3.1) — and a brace scan needs no secret handling to be correct.
func extractVar(text string) string {
	i := strings.Index(text, "{{")
	if i < 0 {
		return ""
	}
	rest := text[i+2:]
	j := strings.Index(rest, "}}")
	if j <= 0 {
		return ""
	}
	name := strings.TrimSpace(rest[:j])
	for _, r := range name {
		if !(r == '_' || r == '-' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return name
}

// doctorFor returns the plain-English remediation for a failed run, or ""
// when the failure is not a missing secret. It names only the variable —
// never a value, because failures carry names and must never carry values.
func doctorFor(err error) string {
	if err == nil {
		return ""
	}
	name := extractVar(err.Error())
	if name == "" {
		return ""
	}
	return fmt.Sprintf("Secret {{%s}} is missing. Press [v] to set it.", name)
}

// doctorVar returns the missing variable for vault prefill, or "".
func doctorVar(err error) string {
	if err == nil {
		return ""
	}
	return extractVar(err.Error())
}
