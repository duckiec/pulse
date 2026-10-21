package env

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePulse = `# team defaults
[default]
BASE_URL = http://localhost:8080
TIMEOUT = 10

[branch:staging]
BASE_URL = https://staging.internal.net

[branch:feat/*]
BASE_URL = https://preview-{{GIT_BRANCH}}.internal.net
PREVIEW = yes
`

func parseSample(t *testing.T) *Document {
	t.Helper()
	doc, err := Parse(strings.NewReader(samplePulse))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// TestDefaultBlockAppliesOutsideMatchingBranches pins the fallback the mandate
// requires: a branch with no block (and no repository at all) reads [default].
func TestDefaultBlockAppliesOutsideMatchingBranches(t *testing.T) {
	doc := parseSample(t)
	for _, branch := range []string{"", "main", "release/1.0"} {
		got := doc.Effective(branch)
		if got["BASE_URL"] != "http://localhost:8080" {
			t.Errorf("branch %q BASE_URL = %q, want the default", branch, got["BASE_URL"])
		}
		if got["TIMEOUT"] != "10" {
			t.Errorf("branch %q TIMEOUT = %q, want 10", branch, got["TIMEOUT"])
		}
	}
}

// TestExactBranchOverridesDefault pins the whole point of the feature: a
// matching branch block replaces the default, key by key.
func TestExactBranchOverridesDefault(t *testing.T) {
	doc := parseSample(t)
	got := doc.Effective("staging")
	if got["BASE_URL"] != "https://staging.internal.net" {
		t.Errorf("BASE_URL = %q, want the staging block", got["BASE_URL"])
	}
	if got["TIMEOUT"] != "10" {
		t.Errorf("TIMEOUT = %q, want the inherited default", got["TIMEOUT"])
	}
}

// TestGlobBranchHydratesGITBRANCH pins the reserved variable, including the
// slash in a branch name (`feat/*` must match `feat/login`, not just `feat`).
func TestGlobBranchHydratesGITBRANCH(t *testing.T) {
	doc := parseSample(t)
	cases := map[string]string{
		"feat/login":        "https://preview-feat/login.internal.net",
		"feat/ui/cards":     "https://preview-feat/ui/cards.internal.net",
		"feat/":             "https://preview-feat/.internal.net",
		"feature/not-quite": "http://localhost:8080",
	}
	for branch, want := range cases {
		if got := doc.Effective(branch)["BASE_URL"]; got != want {
			t.Errorf("branch %q BASE_URL = %q, want %q", branch, got, want)
		}
	}
}

// TestExactBeatsGlobEvenWhenTheGlobComesLast pins the selection rule: a
// literal selector is more specific than a pattern, whatever the file order.
func TestExactBeatsGlobEvenWhenTheGlobComesLast(t *testing.T) {
	doc, err := Parse(strings.NewReader(`
[branch:feat/*]
BASE_URL = glob
[branch:feat/hotfix]
BASE_URL = exact
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := doc.Effective("feat/hotfix")["BASE_URL"]; got != "exact" {
		t.Errorf("BASE_URL = %q, want exact", got)
	}
	if got := doc.Effective("feat/other")["BASE_URL"]; got != "glob" {
		t.Errorf("BASE_URL = %q, want glob", got)
	}

	// The reverse order must give the same answer: a single file-order pass
	// would let the later glob win here, so this case pins the two-pass rule.
	reversed, err := Parse(strings.NewReader(`
[branch:feat/hotfix]
BASE_URL = exact
[branch:feat/*]
BASE_URL = glob
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := reversed.Effective("feat/hotfix")["BASE_URL"]; got != "exact" {
		t.Errorf("exact-first BASE_URL = %q, want exact", got)
	}
}

// TestNoBranchOmitsHydratedKeys pins fail-closed hydration: with no branch, a
// value that needs {{GIT_BRANCH}} is omitted, so resolution errors instead of
// sending an empty host.
func TestNoBranchOmitsHydratedKeys(t *testing.T) {
	doc := parseSample(t)
	if got, ok := doc.Effective("")["TIMEOUT"]; !ok || got != "10" {
		// A value with no token is present even with no branch.
		t.Fatalf("TIMEOUT = %q, %v; want 10, true", got, ok)
	}
	onlyToken, err := Parse(strings.NewReader("[default]\nURL = https://x/{{GIT_BRANCH}}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := onlyToken.Resolve("", "URL"); ok {
		t.Error("URL with an unresolved GIT_BRANCH was hydrated with an empty branch")
	}
	if got, ok := onlyToken.Resolve("main", "URL"); !ok || got != "https://x/main" {
		t.Errorf("Resolve(main) = %q, %v", got, ok)
	}
}

// TestQuotesCommentsAndExport pins the surface shared with .env, so the two
// files cannot disagree about what a line means.
func TestQuotesCommentsAndExport(t *testing.T) {
	doc, err := Parse(strings.NewReader(`
# a comment
; another comment
export PLAIN = value # inline
SINGLE = 'a # b'
DOUBLE = "line\nnext"
EMPTY =
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := doc.Effective("main")
	want := map[string]string{
		"PLAIN":  "value",
		"SINGLE": "a # b",
		"DOUBLE": "line\nnext",
		"EMPTY":  "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestMalformedFilesFailClosed is the total-parser gate: every failure names
// the line and no partial document escapes.
func TestMalformedFilesFailClosed(t *testing.T) {
	cases := map[string]string{
		"unterminated section": "[default\nA = 1\n",
		"unknown section":      "[production]\nA = 1\n",
		"empty branch":         "[branch: ]\nA = 1\n",
		"missing equals":       "[default]\nNOPE\n",
		"invalid key":          "[default]\nA B = 1\n",
		"duplicate key":        "[default]\nA = 1\nA = 2\n",
		"duplicate section":    "[default]\nA = 1\n[default]\nB = 2\n",
		"duplicate duplicate":  "[branch:main]\nA = 1\n[branch:main]\nA = 2\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse(strings.NewReader(src))
			if err == nil {
				t.Fatalf("Parse accepted malformed input: %+v", doc)
			}
			if doc != nil {
				t.Fatalf("Parse returned a partial document alongside %v", err)
			}
			if !strings.Contains(err.Error(), "line") {
				t.Errorf("error %q names no line", err)
			}
		})
	}
}

// TestManyBlocksKeepTheirValues pins section growth: the parser must not lose
// assignments when the block slice reallocates.
func TestManyBlocksKeepTheirValues(t *testing.T) {
	var src strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&src, "[branch:release/%d]\nKEY_%d = value-%d\n", i, i, i)
	}
	doc, err := Parse(strings.NewReader(src.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for i := 0; i < 32; i++ {
		branch := fmt.Sprintf("release/%d", i)
		key := fmt.Sprintf("KEY_%d", i)
		want := fmt.Sprintf("value-%d", i)
		if got, ok := doc.Resolve(branch, key); !ok || got != want {
			t.Errorf("%s %s = %q, %v; want %q", branch, key, got, ok, want)
		}
	}
}

// TestMatchBranch pins the glob semantics directly, including the cases a
// backtracking loop gets wrong.
func TestMatchBranch(t *testing.T) {
	cases := []struct {
		pattern, branch string
		want            bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"feat/*", "feat/x", true},
		{"feat/*", "feat/x/y", true},
		{"feat/*", "feat/", true},
		{"feat/*", "fix/x", false},
		{"*", "", true},
		{"*", "anything/at/all", true},
		{"release/?", "release/1", true},
		{"release/?", "release/12", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"**", "a/b", true},
	}
	for _, c := range cases {
		if got := MatchBranch(c.pattern, c.branch); got != c.want {
			t.Errorf("MatchBranch(%q, %q) = %v, want %v", c.pattern, c.branch, got, c.want)
		}
	}
}

// TestParseFileMissingWrapsNotExist pins the distinction the app relies on:
// an absent file is "not configured", a malformed one is a degradation.
func TestParseFileMissingWrapsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.pulse")
	if _, err := ParseFile(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ParseFile(missing) error = %v, want fs.ErrNotExist", err)
	}
}

// TestParseFileRoundTrip proves the on-disk path works, not only the reader.
func TestParseFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(samplePulse), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := doc.Effective("staging")["BASE_URL"]; got != "https://staging.internal.net" {
		t.Errorf("BASE_URL = %q", got)
	}
}
