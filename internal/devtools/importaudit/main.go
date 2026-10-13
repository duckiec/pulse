// Command importaudit enforces the stdlib-first dependency policy (AGENTS.md
// §2.2, §4.1) and the MCP stdout-hygiene rule (AGENTS.md §3.3).
//
// The audit is static and offline by design: it parses go.mod and the Go
// sources with go/parser and never touches the network, so it runs identically
// on a plane and in CI (invariant I-3).
//
// Usage: go run ./internal/devtools/importaudit config/allowed-modules.txt
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// permanentlyBanned modules are rejected regardless of the allowlist
// (AGENTS.md §2.4 bans mattn/go-sqlite3; §2.2 bans router/web frameworks).
var permanentlyBanned = []string{
	"github.com/mattn/go-sqlite3",
	"github.com/gorilla/mux",
	"github.com/go-chi/chi",
	"github.com/labstack/echo",
	"github.com/gin-gonic/gin",
	"github.com/go-resty/resty",
	"github.com/zalando/go-keyring",
	"gopkg.in/yaml.v2",
	"gopkg.in/yaml.v3",
}

// stdoutHygieneDirs are the MCP-reachable packages where any stdout write
// corrupts the JSON-RPC wire (AGENTS.md §3.3).
var stdoutHygieneDirs = []string{"internal/mcp", "internal/app"}

// stdoutHygieneExemptFiles are files that legitimately manipulate os.Stdout
// as part of the isolation boundary itself. The guard in stdio.go is the ONE
// place that swaps os.Stdout; without it, every other file in the package
// would be free to write to the wire. The exemption is narrow: it covers
// only the guard file, and only the os.Stdout references within it.
var stdoutHygieneExemptFiles = map[string]bool{
	"internal/mcp/stdio.go": true,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "importaudit: FAIL\n%v\n", err)
		os.Exit(1)
	}
	fmt.Println("importaudit: PASS")
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	allow := loadAllowlist(filepath.Join(root, "config", "allowed-modules.txt"))

	var findings []string

	// 1. Direct requires in go.mod must be sanctioned.
	mods, err := directRequires(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	for _, m := range mods {
		if bad := banned(m); bad != "" {
			findings = append(findings, fmt.Sprintf("go.mod requires banned module %s (%s)", m, bad))
			continue
		}
		if !allowed(m, allow) {
			findings = append(findings, fmt.Sprintf("go.mod requires unsanctioned module %s", m))
		}
	}

	// 2. Every import in cmd/ and internal/ must be stdlib or sanctioned.
	var goFiles []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil // dir not created yet
				}
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") {
				goFiles = append(goFiles, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, path := range goFiles {
		rel, _ := filepath.Rel(root, path)
		imports, err := fileImports(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		for _, imp := range imports {
			if isStdlib(imp) {
				continue
			}
			if bad := banned(imp); bad != "" {
				findings = append(findings, fmt.Sprintf("%s imports banned module %s (%s)", rel, imp, bad))
				continue
			}
			if !allowed(imp, allow) {
				findings = append(findings, fmt.Sprintf("%s imports unsanctioned module %s", rel, imp))
			}
		}
		// 3. stdout hygiene in MCP-reachable packages.
		//
		// Test files are exempt, and the exemption is not a convenience: a
		// test that proves the isolation boundary holds MUST be able to swap
		// os.Stdout for a pipe and observe what crossed it. Test files are
		// also not linked into the shipped binary, so a stray print in one
		// cannot corrupt a client's wire. Holding tests to the production
		// rule would forbid writing the gate at all.
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		for _, dir := range stdoutHygieneDirs {
			if strings.HasPrefix(rel, dir+string(filepath.Separator)) {
				if stdoutHygieneExemptFiles[rel] {
					continue // the guard file is the boundary itself
				}
				off, err := stdoutWrites(path)
				if err != nil {
					return fmt.Errorf("parse %s: %w", rel, err)
				}
				for _, o := range off {
					findings = append(findings, fmt.Sprintf("%s:%d: %s in an MCP-reachable package (AGENTS.md §3.3)", rel, o.line, o.what))
				}
			}
		}
	}

	if len(findings) > 0 {
		return fmt.Errorf("%d finding(s):\n  - %s", len(findings), strings.Join(findings, "\n  - "))
	}
	return nil
}

// repoRoot walks up from the working directory to the go.mod root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func loadAllowlist(path string) map[string]bool {
	allow := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return allow
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allow[line] = true
	}
	return allow
}

// allowed reports whether mod is sanctioned. Entries ending in '/' are
// prefixes (e.g. golang.org/x/ matches every submodule).
func allowed(mod string, allow map[string]bool) bool {
	if allow[mod] {
		return true
	}
	for p := range allow {
		if strings.HasSuffix(p, "/") && strings.HasPrefix(mod, p) {
			return true
		}
	}
	return false
}

func banned(mod string) string {
	for _, b := range permanentlyBanned {
		if mod == b || strings.HasPrefix(mod, b+"/") {
			return "permanently banned"
		}
	}
	return ""
}

// isStdlib reports whether an import path belongs to the standard library:
// the first path element contains no dot.
func isStdlib(path string) bool {
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	return !strings.Contains(first, ".")
}

func fileImports(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out, nil
}

type offense struct {
	line int
	what string
}

// stdoutWrites finds fmt.Print*/os.Stdout uses in the given file.
func stdoutWrites(path string) ([]offense, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []offense
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok {
			switch {
			case pkg.Name == "os" && sel.Sel.Name == "Stdout":
				out = append(out, offense{fset.Position(sel.Pos()).Line, "os.Stdout reference"})
			case pkg.Name == "fmt" && (sel.Sel.Name == "Print" || sel.Sel.Name == "Printf" || sel.Sel.Name == "Println"):
				out = append(out, offense{fset.Position(sel.Pos()).Line, "fmt." + sel.Sel.Name + " call"})
			}
		}
		return true
	})
	return out, nil
}

// directRequires extracts the direct require paths from go.mod, including
// those inside require (...) blocks.
func directRequires(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mods []string
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if i := strings.Index(s, "//"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		switch {
		case strings.HasPrefix(s, "require ("):
			inBlock = true
		case inBlock && s == ")":
			inBlock = false
		case inBlock:
			if f := strings.Fields(s); len(f) >= 2 {
				mods = append(mods, f[0])
			}
		case strings.HasPrefix(s, "require "):
			if f := strings.Fields(s); len(f) >= 3 {
				mods = append(mods, f[1])
			}
		}
	}
	return mods, nil
}
