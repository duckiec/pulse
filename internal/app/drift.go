package app

import (
	"errors"
	"fmt"
	"os"

	"pulse/internal/engine"
	"pulse/internal/fuzz"
)

// Contract drift is the composition root's job for one reason: the OpenAPI
// reader lives in `internal/fuzz`, the matcher lives in `internal/engine`, and
// neither package may import the other (AGENTS.md §3.1). This file is the only
// place both are visible, so it is where the IR is projected onto the engine's
// flat operation list.
//
// It is a package-level function rather than an App method on purpose: drift
// reads two files and talks to nobody. Booting the store, the keychain and the
// sync subsystem to answer "is this collection stale?" would make an offline
// static check pay for runtime machinery it never uses.

// DriftResult is one drift run plus the identity of the document it was checked
// against.
type DriftResult struct {
	// Report is the violations and counts.
	Report *engine.DriftReport
	// SpecPath is the OpenAPI document that was read.
	SpecPath string
	// SpecVersion is the document's declared `openapi:` value.
	SpecVersion string
}

// DriftSpec loads an OpenAPI document and checks every request in a `.http`
// file or a directory tree against it.
//
// A missing spec, an unreadable collection or a `.http` file that does not
// parse is an error: a drift run over a collection Pulse could not fully read
// must not report "no violations".
func DriftSpec(specPath, collectionPath string) (*DriftResult, error) {
	if specPath == "" {
		return nil, fmt.Errorf("an OpenAPI document path is required")
	}
	if collectionPath == "" {
		return nil, fmt.Errorf("a .http file or directory is required")
	}
	ir, err := fuzz.LoadOpenAPI(specPath)
	if err != nil {
		// The CLI prints this verbatim, so the sentence names the file rather
		// than relaying the reader's package prefixes.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the OpenAPI document %s does not exist", specPath)
		}
		return nil, fmt.Errorf("cannot read %s as an OpenAPI document: %v", specPath, err)
	}
	ops := make([]engine.SpecOp, 0, len(ir.Ops))
	for _, op := range ir.Ops {
		so := engine.SpecOp{Method: op.Method, Path: op.Path, Deprecated: op.Deprecated}
		for _, p := range op.Params {
			if !p.Required {
				continue
			}
			switch p.In {
			case "query":
				so.RequiredQuery = append(so.RequiredQuery, p.Name)
			case "header":
				so.RequiredHeader = append(so.RequiredHeader, p.Name)
			}
		}
		ops = append(ops, so)
	}
	files, err := engine.Files(collectionPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the collection path %s does not exist", collectionPath)
		}
		return nil, err
	}
	rep, err := engine.CheckDrift(ops, files)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", collectionPath, err)
	}
	return &DriftResult{Report: rep, SpecPath: specPath, SpecVersion: ir.Version}, nil
}
