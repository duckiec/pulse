// P4.1.3 mode-immutability and headless-isolation gate (Wave 1).
//
// Mode is selected once at startup by run() and is immutable for the process
// lifetime: run() is a pure function of (args, tty) with no retained state,
// so there is no dispatch loop in which a mode could change mid-flight. The
// headless domain packages must never import the TUI stack — internal/ui or
// bubbletea — so no headless path can initialize a tea.Program or touch
// alt-screen state. cmd/pulse (the wiring layer) is the only place both
// stacks meet.
package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// headlessPkgs are the domain packages that must stay TTY-free. internal/ui
// is the TUI stack; cmd/pulse is deliberately absent (it wires modes).
var headlessPkgs = []string{
	"internal/app",
	"internal/ast",
	"internal/engine",
	"internal/fuzz",
	"internal/mcp",
	"internal/rag",
	"internal/secrets",
	"internal/store",
	"internal/wasm",
}

// forbiddenImports are the TTY/alt-screen drivers. bubbles/lipgloss are UI
// styling primitives that only make sense attached to a tea.Program.
var forbiddenImports = []string{
	"pulse/internal/ui",
	"charmbracelet/bubbletea",
	"charmbracelet/bubbles",
	"charmbracelet/lipgloss",
	"charmbracelet/huh",
}

// TestModeImmutabilityAndHeadlessIsolation proves P4.1.3 in three parts:
// (1) mode selection is a pure, repeatable function of startup inputs with
// no retained state between runs; (2) no headless package imports the TUI
// stack; (3) headless runs emit no alt-screen control sequences.
func TestModeImmutabilityAndHeadlessIsolation(t *testing.T) {
	t.Run("selection is pure and stateless", func(t *testing.T) {
		emptyDir := t.TempDir()
		modes := []struct {
			name string
			args []string
			tty  bool
		}{
			{"mcp subcommand", []string{"mcp"}, false},
			{"mcp flag", []string{"--mcp"}, false},
			{"version", []string{"--version"}, false},
			{"bare pipe fails closed", nil, false},
			{"test empty dir", []string{"test", emptyDir}, false},
			{"vault usage", []string{"vault"}, false},
		}
		for _, m := range modes {
			first := invokeRun(m.args, m.tty)
			second := invokeRun(m.args, m.tty)
			if first != second {
				t.Fatalf("mode %q is not immutable: first run exit %d, second %d",
					m.name, first, second)
			}
		}
		// Interleaving modes must not interfere: there is no retained
		// selection for a later run to inherit.
		a := invokeRun([]string{"mcp"}, false)
		b := invokeRun([]string{"--version"}, false)
		c := invokeRun([]string{"mcp"}, false)
		if a != exitOK || b != exitOK || c != exitOK {
			t.Fatalf("interleaved modes interfered: mcp=%d version=%d mcp=%d", a, b, c)
		}
	})

	t.Run("headless packages never import the TUI stack", func(t *testing.T) {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		// thisFile is <root>/cmd/pulse/mode_immutability_test.go.
		root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
		for _, pkg := range headlessPkgs {
			dir := filepath.Join(root, filepath.FromSlash(pkg))
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read %s: %v", pkg, err)
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatalf("read %s/%s: %v", pkg, e.Name(), err)
				}
				for _, line := range strings.Split(string(raw), "\n") {
					line = strings.TrimSpace(strings.Trim(line, `"`))
					for _, forbidden := range forbiddenImports {
						if strings.Contains(line, forbidden) {
							t.Errorf("headless package %s imports %q (%s): %q",
								pkg, forbidden, e.Name(), strings.TrimSpace(line))
						}
					}
				}
			}
		}
	})

	t.Run("headless runs never touch alt-screen state", func(t *testing.T) {
		emptyDir := t.TempDir()
		runs := [][]string{
			{"mcp"},
			{"--mcp"},
			{"test", emptyDir},
			{"vault"},
			{"vault", "init"},
			{"--version"},
		}
		for _, args := range runs {
			var stdout, stderr strings.Builder
			run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, false)
			for _, stream := range []struct {
				name string
				text string
			}{{"stdout", stdout.String()}, {"stderr", stderr.String()}} {
				for _, seq := range []string{"\x1b[?1049h", "\x1b[?1049l", "\x1b[?25l", "\x1b[?25h"} {
					if strings.Contains(stream.text, seq) {
						t.Errorf("run(%q) %s contains alt-screen sequence %q",
							args, stream.name, seq)
					}
				}
			}
		}
	})
}

func invokeRun(args []string, tty bool) int {
	var stdout, stderr strings.Builder
	return run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, tty)
}
