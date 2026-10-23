package engine

import (
	"strings"
	"testing"

	"pulse/internal/ast"
)

// mergeDocs runs Merge and, for a clean result, re-parses the output: a merge
// driver that writes a document Pulse itself cannot read has not solved
// anything.
func mergeDocs(t *testing.T, base, ours, theirs string) MergeResult {
	t.Helper()
	res, err := Merge([]byte(base), []byte(ours), []byte(theirs))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Conflicts == 0 {
		arena := ast.AcquireArena()
		arena.Reset(res.Text)
		doc, perr := ast.Parse(arena)
		if perr != nil {
			arena.Release()
			t.Fatalf("a clean merge produced a document that does not parse: %v\n%s", perr, res.Text)
		}
		// A successful Parse hands the arena to the document; releasing both
		// would return the same arena to the pool twice and corrupt the next
		// test's memory.
		doc.Release()
	}
	return res
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("merged text is missing %q:\n%s", want, text)
	}
}

func mustNotContain(t *testing.T, text, bad string) {
	t.Helper()
	if strings.Contains(text, bad) {
		t.Fatalf("merged text unexpectedly contains %q:\n%s", bad, text)
	}
}

// TestMergeCombinesIndependentFields is the feature's headline: A adds a
// header, B adds an assertion, and the merge keeps both with no conflict.
func TestMergeCombinesIndependentFields(t *testing.T) {
	base := "# @name list\n# @assert status == 200\nGET https://api.example.com/items\nAccept: application/json\n"
	ours := "# @name list\n# @assert status == 200\nGET https://api.example.com/items\nAccept: application/json\nX-Trace: 1\n"
	theirs := "# @name list\n# @assert status == 200\n# @assert body contains items\nGET https://api.example.com/items\nAccept: application/json\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "X-Trace: 1")
	mustContain(t, out, "# @assert status == 200")
	mustContain(t, out, "# @assert body contains items")
	mustContain(t, out, "Accept: application/json")
	mustNotContain(t, out, mergeOursStart)
}

// TestMergeDoesNotDropAnIncomingHeader is a regression gate for the verbatim
// copy path. When ours changed only an assertion and theirs changed only a
// header, every SCALAR field equal to ours, so a "did anything change?" test
// that ignored the keyed fields copied ours and silently lost the header.
func TestMergeDoesNotDropAnIncomingHeader(t *testing.T) {
	base := "# @name list\nGET https://api.example.com/items\nAccept: application/json\n"
	ours := "# @name list\n# @assert status == 200\nGET https://api.example.com/items\nAccept: application/json\n"
	theirs := "# @name list\nGET https://api.example.com/items\nAccept: application/json\nX-Trace: b\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "X-Trace: b")
	mustContain(t, out, "# @assert status == 200")
}

// TestMergeConflictsOnTheSameHeaderValue is the fallback: both sides change one
// header to different values, so the driver cannot know which is right.
func TestMergeConflictsOnTheSameHeaderValue(t *testing.T) {
	base := "GET https://api.example.com/items\nAccept: application/json\n"
	ours := "GET https://api.example.com/items\nAccept: application/vnd.pulse+json\n"
	theirs := "GET https://api.example.com/items\nAccept: text/plain\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, mergeOursStart)
	mustContain(t, out, mergeSeparator)
	mustContain(t, out, mergeTheirsEnd)
	mustContain(t, out, "Accept: application/vnd.pulse+json")
	mustContain(t, out, "Accept: text/plain")
}

// TestMergeKeepsAdditionsFromBothSides covers new requests: one added by each
// branch survives, and the current branch's order comes first.
func TestMergeKeepsAdditionsFromBothSides(t *testing.T) {
	base := "# @name one\nGET https://api.example.com/one\n"
	ours := base + "\n###\n# @name ours\nGET https://api.example.com/ours\n"
	theirs := base + "\n###\n# @name theirs\nGET https://api.example.com/theirs\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "# @name ours")
	mustContain(t, out, "# @name theirs")
	if res.Requests != 3 {
		t.Fatalf("merged %d request(s), want 3:\n%s", res.Requests, out)
	}
}

// TestMergeDeleteVersusEditConflicts is the modify/delete case: silently
// deleting the edited request (or resurrecting it) would both lose work.
func TestMergeDeleteVersusEditConflicts(t *testing.T) {
	base := "# @name one\nGET https://api.example.com/one\nAccept: */*\n\n###\n# @name two\nGET https://api.example.com/two\n"
	ours := "# @name one\nGET https://api.example.com/one\nAccept: */*\n"
	theirs := "# @name one\nGET https://api.example.com/one\nAccept: */*\n\n###\n# @name two\nGET https://api.example.com/two\nAccept: application/json\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, mergeOursStart)
	mustContain(t, out, "# @name two")
	mustContain(t, out, "Accept: application/json")
}

// TestMergeDeletesUntouchedRequest is the other half of the delete rule: a
// request the other branch did not touch is removed without ceremony.
func TestMergeDeletesUntouchedRequest(t *testing.T) {
	base := "# @name one\nGET https://api.example.com/one\n\n###\n# @name two\nGET https://api.example.com/two\n"
	ours := "# @name one\nGET https://api.example.com/one\n"
	theirs := base

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	mustNotContain(t, string(res.Text), "# @name two")
}

// TestMergeBodiesAndAssertionsCombine proves field-level independence inside
// one request: B rewrites the body, A adds an assertion, and both land.
func TestMergeBodiesAndAssertionsCombine(t *testing.T) {
	base := "# @name create\nPOST https://api.example.com/items\nContent-Type: application/json\n\n{\"a\":1}\n"
	ours := "# @name create\n# @assert status == 201\nPOST https://api.example.com/items\nContent-Type: application/json\n\n{\"a\":1}\n"
	theirs := "# @name create\nPOST https://api.example.com/items\nContent-Type: application/json\n\n{\"a\":2}\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "# @assert status == 201")
	mustContain(t, out, `{"a":2}`)
}

// TestMergeFileScopedVariables comes from the preamble path: a variable added
// by the incoming side must survive a merge that also edits a request.
func TestMergeFileScopedVariables(t *testing.T) {
	base := "@base = https://api.example.com\n\n# @name one\nGET {{base}}/one\n"
	ours := "@base = https://api.example.com\n\n# @name one\nGET {{base}}/one\nX-Ours: 1\n"
	theirs := "@base = https://api.example.com\n@token = t0ken\n\n# @name one\nGET {{base}}/one\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "@token = t0ken")
	mustContain(t, out, "X-Ours: 1")
}

// TestMergeIdenticalSidesIsByteIdentical is the no-op guarantee: when only one
// side changed, the other side's request text is copied, not reformatted.
func TestMergeIdenticalSidesIsByteIdentical(t *testing.T) {
	ours := "@base = https://api.example.com\n\n# a free comment\n# @name one\nGET {{base}}/one\nAccept: */*   \n\n{\"a\": 1}\n"
	res := mergeDocs(t, ours, ours, ours)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", res.Conflicts)
	}
	if string(res.Text) != ours {
		t.Fatalf("merge of identical sides changed the text:\n got: %q\nwant: %q", res.Text, ours)
	}
}

// TestMergeRefusesUnparseableInput: a driver must not guess a merge for input
// it cannot understand.
func TestMergeRefusesUnparseableInput(t *testing.T) {
	if _, err := Merge([]byte("GET https://x.local/a\n"), []byte("this is not http\n"), []byte("GET https://x.local/a\n")); err == nil {
		t.Fatal("a malformed current side was accepted")
	} else if !strings.Contains(err.Error(), "current") {
		t.Fatalf("error = %v, want it to name the side", err)
	}
}

// TestMergeCarriesTheHookDirective guards the interaction between the two
// Phase 4 features: a hook directive added on one side survives a merge of an
// unrelated field on the other.
func TestMergeCarriesTheHookDirective(t *testing.T) {
	base := "# @name one\nGET https://api.example.com/one\n"
	ours := "# @name one\n# @wasm ./hooks/auth.wasm\nGET https://api.example.com/one\nAccept: */*\n"
	theirs := "# @name one\n# @assert status == 200\nGET https://api.example.com/one\n"

	res := mergeDocs(t, base, ours, theirs)
	if res.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0:\n%s", res.Conflicts, res.Text)
	}
	out := string(res.Text)
	mustContain(t, out, "# @wasm ./hooks/auth.wasm")
	mustContain(t, out, "# @assert status == 200")
}
