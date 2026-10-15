package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pulse/internal/ast"
	"pulse/internal/secrets"
)

// This file is the TUI authoring seam: workspace persistence and secret
// sealing for Phase 2.2's composer. It lives HERE — the composition root —
// because internal/ui may not touch disk or the keychain itself (AGENTS.md
// §3.1, §3.2): the UI injects these as background functions and never
// learns whether the keychain, the ephemeral tier, or a test fake answers.
//
// Every write funnels into internal/ast's jailed, atomic, re-validated
// path — the composer cannot invent a persistence semantics the CLI lacks.

// UpsertUIRequest persists one composer block: replace the named request's
// span, or append a trailing section when the name is new. A missing file
// is created. The block must hold exactly one request.
func (a *App) UpsertUIRequest(root, rel, name string, block []byte) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("app: upsert requires a request name: %w", ast.ErrRequestNotFound)
	}
	return ast.UpsertRequestInFile(root, rel, name, block)
}

// CreateUIFile writes a whole new collection file (starter, new-file
// template). Existing files are never truncated: use UpsertUIRequest for
// updates. Content must parse.
func (a *App) CreateUIFile(root, rel string, content []byte) error {
	return ast.CreateCollectionFile(root, rel, content)
}

// ReadUIFile reads one workspace file back for the UI thread to parse and
// swap. Reads are confined to the root like writes: the check mirrors
// ast's jail (lexical clean, then EvalSymlinks confinement of the nearest
// existing ancestor) so a symlinked path cannot pull the TUI outside the
// workspace it displays.
func (a *App) ReadUIFile(root, rel string) ([]byte, error) {
	abs, err := confineUIPath(root, rel)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("app: read %s: %w", rel, err)
	}
	return raw, nil
}

// confineUIPath resolves rel inside root, failing closed on NUL bytes,
// `..` escape, and symlink escape. Absolute rel paths are rejected
// outright: the TUI only ever names workspace-relative collections.
func confineUIPath(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q", ast.ErrPathEscape, rel)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ast.ErrPathEscape, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ast.ErrPathEscape, rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("app: resolve workspace: %w", err)
	}
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("app: resolve workspace: %w", err)
	}
	abs := filepath.Join(rootAbs, clean)
	anchor := abs
	for {
		if _, err := os.Lstat(anchor); err == nil {
			break
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return "", fmt.Errorf("app: resolve path: %q", rel)
		}
		anchor = parent
	}
	anchorEval, err := filepath.EvalSymlinks(anchor)
	if err != nil {
		return "", fmt.Errorf("app: resolve path: %w", err)
	}
	remainder, err := filepath.Rel(anchor, abs)
	if err != nil {
		return "", fmt.Errorf("app: resolve path: %w", err)
	}
	target := filepath.Join(anchorEval, remainder)
	if target != rootEval && !strings.HasPrefix(target, rootEval+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ast.ErrPathEscape, rel)
	}
	return abs, nil
}

// SealUISecret stores one token for the in-TUI vault helper: the OS
// keychain write side first, the chain's memory-only Seal tier when no
// write path exists (headless CI, unsupported platform). It never writes
// a file, and a missing write path degrades to memory rather than to an
// error — the send that follows resolves through the same chain either
// way, and the failure to persist is honest in the message.
func (a *App) SealUISecret(name, value string) error {
	if a.Chain == nil {
		return fmt.Errorf("app: seal %q: no resolution chain", name)
	}
	store := secrets.NewStore()
	if err := store.Available(); err == nil {
		if err := store.Store(name, value); err == nil {
			a.Chain.Redactor().Register(value)
			return nil
		}
	}
	if err := a.Chain.Seal(name, value); err != nil {
		return fmt.Errorf("app: seal %q: %w", name, err)
	}
	return nil
}
