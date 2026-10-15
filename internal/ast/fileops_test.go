package ast

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workspace creates a temp workspace root with a seed collection.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const seedCollection = `# Workspace notes stay untouched.
@base = https://api.example.com

# @name first
# @assert status == 200
GET {{base}}/one
Accept: application/json

### Second block
# @name second
POST {{base}}/two HTTP/1.1
Content-Type: application/json

{"k": "v"}

###
`

// TestJailMatrix proves confinement: traversal, absolute-outside, symlink
// escape, and NUL all fail with ErrPathEscape; nested creation inside root
// succeeds.
func TestJailMatrix(t *testing.T) {
	root := workspace(t, map[string]string{"api.http": seedCollection})
	evil := map[string]string{
		"dotdot":      "../evil.http",
		"deep":        "sub/../../evil.http",
		"absolute":    "/tmp/evil.http",
		"nul":         "a\x00b.http",
		"clean-trick": "sub/../..",
	}
	for name, rel := range evil {
		if err := CreateCollectionFile(root, rel, []byte("GET https://x.local/\n")); !errors.Is(err, ErrPathEscape) {
			t.Errorf("%s (%q): err = %v, want ErrPathEscape", name, rel, err)
		}
		if err := UpsertRequestInFile(root, rel, "x", []byte("# @name x\nGET https://x.local/\n")); !errors.Is(err, ErrPathEscape) {
			t.Errorf("%s upsert (%q): err = %v, want ErrPathEscape", name, rel, err)
		}
	}
	// Symlink escape: link inside root pointing at /tmp must not be writable.
	if err := os.Symlink(os.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Skipf("no symlink support: %v", err)
	}
	if err := CreateCollectionFile(root, "link/evil.http", []byte("GET https://x.local/\n")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("symlink escape: err = %v, want ErrPathEscape", err)
	}
	// Legit nested creation works and parses.
	if err := CreateCollectionFile(root, "team/api.http", []byte("# @name n\nGET https://x.local/n\n")); err != nil {
		t.Errorf("nested create: %v", err)
	}
	if _, err := ParseFile(filepath.Join(root, "team/api.http")); err != nil {
		t.Errorf("created file does not parse: %v", err)
	}
	// Creation never truncates.
	if err := CreateCollectionFile(root, "api.http", []byte("GET https://x.local/\n")); !errors.Is(err, ErrFileExists) {
		t.Errorf("re-create: err = %v, want ErrFileExists", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "api.http"))
	if string(raw) != seedCollection {
		t.Error("re-create truncated the existing file")
	}
}

// TestUpsertPreservesNeighbors is the lossless contract: replacing the middle
// request keeps every surrounding byte identical — file vars, top comments,
// the other blocks, separators, and the trailing marker.
func TestUpsertPreservesNeighbors(t *testing.T) {
	root := workspace(t, map[string]string{"api.http": seedCollection})
	block := "# @name second\n# @assert status == 202\nPUT https://api.example.com/two\n\n{\"k2\": 2}"
	if err := UpsertRequestInFile(root, "api.http", "second", []byte(block)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "api.http"))
	got := string(raw)
	head := "# Workspace notes stay untouched.\n@base = https://api.example.com\n\n# @name first\n# @assert status == 200\nGET {{base}}/one\nAccept: application/json\n"
	if !strings.HasPrefix(got, head) {
		t.Errorf("head not preserved:\n%s", got)
	}
	if !strings.Contains(got, "### Second block\n"+block+"\n") {
		t.Errorf("replaced block malformed:\n%s", got)
	}
	if !strings.HasSuffix(got, "###\n") {
		t.Errorf("trailing separator lost:\n%s", got)
	}
	if strings.Contains(got, `"k": "v"`) || strings.Contains(got, "POST {{base}}/two") {
		t.Errorf("stale second-block content survived:\n%s", got)
	}
	// The spliced file re-parses with both requests intact.
	doc, err := ParseFile(filepath.Join(root, "api.http"))
	if err != nil {
		t.Fatalf("spliced file does not parse: %v", err)
	}
	defer doc.Release()
	if doc.Len() != 2 {
		t.Fatalf("requests = %d, want 2", doc.Len())
	}
	found := map[string]bool{}
	for r := range doc.Requests() {
		found[doc.RequestName(r)] = true
	}
	if !found["first"] || !found["second"] {
		t.Errorf("names after splice: %v", found)
	}
}

// TestUpsertAppendsMissing proves the create leg: an unknown name becomes a
// new trailing section, and upsert into a missing file creates it.
func TestUpsertAppendsMissing(t *testing.T) {
	root := workspace(t, map[string]string{"api.http": seedCollection})
	block := "# @name third\nDELETE https://api.example.com/three\n"
	if err := UpsertRequestInFile(root, "api.http", "third", []byte(block)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "api.http"))
	if !strings.Contains(string(raw), "###\n"+block) {
		t.Errorf("appended section malformed:\n%s", raw)
	}
	if err := UpsertRequestInFile(root, "new.http", "n", []byte("# @name n\nGET https://x.local/n\n")); err != nil {
		t.Fatalf("upsert-create: %v", err)
	}
	doc, err := ParseFile(filepath.Join(root, "new.http"))
	if err != nil {
		t.Fatalf("created file does not parse: %v", err)
	}
	doc.Release()
	// A block holding two requests is rejected before touching disk.
	before, _ := os.ReadFile(filepath.Join(root, "api.http"))
	two := "# @name a\nGET https://x.local/a\n\n###\n# @name b\nGET https://x.local/b\n"
	if err := UpsertRequestInFile(root, "api.http", "a", []byte(two)); err == nil {
		t.Error("two-request block accepted, want error")
	}
	after, _ := os.ReadFile(filepath.Join(root, "api.http"))
	if string(before) != string(after) {
		t.Error("rejected upsert modified the file")
	}
}

// TestDeleteRequest covers whole-section removal (separator goes with it),
// head-section surgery (file vars survive), and unknown names.
func TestDeleteRequest(t *testing.T) {
	root := workspace(t, map[string]string{"api.http": seedCollection})
	if err := DeleteRequestFromFile(root, "api.http", "second"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "api.http"))
	got := string(raw)
	if strings.Contains(got, "second") || strings.Contains(got, "Second block") {
		t.Errorf("deleted block survived:\n%s", got)
	}
	if strings.Count(got, "###") != 1 {
		t.Errorf("want exactly the trailing separator left, got:\n%s", got)
	}
	if !strings.Contains(got, "@base = https://api.example.com") {
		t.Errorf("file vars lost:\n%s", got)
	}
	doc, err := ParseFile(filepath.Join(root, "api.http"))
	if err != nil {
		t.Fatalf("file after delete does not parse: %v", err)
	}
	if doc.Len() != 1 {
		t.Errorf("requests = %d, want 1", doc.Len())
	}
	doc.Release()

	// Deleting the head request keeps file vars and comments.
	if err := DeleteRequestFromFile(root, "api.http", "first"); err != nil {
		t.Fatalf("delete head: %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(root, "api.http"))
	if !strings.Contains(string(raw), "@base =") || !strings.Contains(string(raw), "Workspace notes") {
		t.Errorf("head delete ate file scope:\n%s", raw)
	}
	if err := DeleteRequestFromFile(root, "api.http", "nope"); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("unknown delete: err = %v, want ErrRequestNotFound", err)
	}
	if err := DeleteRequestFromFile(root, "missing.http", "x"); err == nil {
		t.Error("delete from missing file accepted, want error")
	}
}

// TestSpliceAdjacentRequests proves neighboring sections splice
// independently: two requests in back-to-back `###` sections, delete the
// first. (One section cannot hold two requests: a blank line after the
// headers opens request a's body and swallows the rest, while no blank line
// leaves the lexer in the header block so the second method line mislexes
// as a header name — probed both ways during development. The canonical
// emitter always separates with `###`, so adjacent sections are the true
// adjacency case.)
func TestSpliceAdjacentRequests(t *testing.T) {
	src := "### First\n# @name a\nGET https://x.local/a\n\n### Second\n# @name b\nGET https://x.local/b\n"
	root := workspace(t, map[string]string{"s.http": src})
	if err := DeleteRequestFromFile(root, "s.http", "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "s.http"))
	if strings.Contains(string(raw), "@name a") || !strings.Contains(string(raw), "@name b") {
		t.Errorf("adjacent splice wrong:\n%s", raw)
	}
	doc, err := ParseFile(filepath.Join(root, "s.http"))
	if err != nil {
		t.Fatalf("spliced file does not parse: %v", err)
	}
	defer doc.Release()
	if doc.Len() != 1 {
		t.Fatalf("requests = %d, want 1", doc.Len())
	}
	for r := range doc.Requests() {
		if doc.RequestName(r) != "b" {
			t.Errorf("survivor = %q, want b", doc.RequestName(r))
		}
	}
}
