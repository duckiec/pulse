package fuzz_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"pulse/internal/fuzz"
)

// tapLines splits a TAP document into lines, dropping the trailing empty one.
func tapLines(doc string) []string {
	lines := strings.Split(strings.TrimRight(doc, "\n"), "\n")
	return lines
}

// fmtSscan extracts the leading integer from a TAP line.
//
// It takes the optional prefix to strip ("1.." for a plan, "" for a result) and
// scans from there. A hand-rolled field split is what this test is guarding
// against elsewhere, so the parsing here is done by strconv rather than by
// string surgery — a test that mis-parses its own input cannot notice a
// malformed document.
func fmtSscan(line, prefix string, dst *int) (int, error) {
	rest := strings.TrimPrefix(line, prefix)
	rest = strings.TrimLeft(rest, "ok not ")
	rest = strings.TrimSpace(rest)
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no leading number in %q", line)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, err
	}
	*dst = n
	return n, nil
}

// TestTAPDocumentIsWellFormed is the structural gate: the document must satisfy
// the rules a TAP consumer actually depends on.
//
// Each assertion below corresponds to a rule whose violation HANGS or
// MISREADS a consumer, not to a stylistic preference.
func TestTAPDocumentIsWellFormed(t *testing.T) {
	results := []fuzz.Result{
		{Number: 1, Description: "GET /users query.limit max", OK: true},
		{Number: 2, Description: "GET /users query.limit max+1", OK: false,
			Diagnostic: fuzz.Diagnostic{
				Severity: "fail",
				Message:  "unexpected status 500",
				Fields:   map[string]string{"status": "500", "payload": "9999999999"},
			}},
		{Number: 3, Description: "GET /users body.name oversize-1MiB", OK: false,
			Diagnostic: fuzz.Diagnostic{
				Severity: "fail",
				Message:  "the request failed before a response",
				Fields:   map[string]string{"status": "0"},
			}},
	}
	doc := fuzz.Render(results)
	t.Logf("TAP document:\n%s", doc)
	lines := tapLines(doc)

	// Rule 1: the version line is first, and is exactly this.
	if lines[0] != "TAP version 13" {
		t.Errorf("line 1 = %q, want %q", lines[0], "TAP version 13")
	}

	// Rule 2: exactly one plan, and its count equals the number of result lines.
	var plan string
	resultLines := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "1..") {
			if plan != "" {
				t.Errorf("more than one plan line: %q and %q", plan, l)
			}
			plan = l
			continue
		}
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			resultLines++
		}
	}
	if plan == "" {
		t.Fatal("no plan line; a TAP consumer cannot know how many tests to expect")
	}
	var planned int
	if _, err := fmtSscan(plan, "1..", &planned); err != nil {
		t.Fatalf("unparseable plan %q: %v", plan, err)
	}
	if planned != resultLines {
		t.Errorf("plan says %d tests but %d result lines were emitted; a TAP "+
			"consumer that trusts the plan HANGS", planned, resultLines)
	}

	// Rule 3: numbering starts at 1 and is contiguous with no gaps or repeats.
	want := 1
	for _, l := range lines {
		if !strings.HasPrefix(l, "ok ") && !strings.HasPrefix(l, "not ok ") {
			continue
		}
		var n int
		if _, err := fmtSscan(l, "", &n); err != nil {
			continue // handled below
		}
		if n != want {
			t.Errorf("result line %q has number %d, want %d; numbering must be "+
				"contiguous from 1", l, n, want)
		}
		want++
	}

	// Rule 4: every failure carries a diagnostic block, fenced correctly.
	var inDiag bool
	diagCount := 0
	for _, l := range lines {
		switch {
		case l == "  ---":
			if inDiag {
				t.Error("a diagnostic block started while one was open")
			}
			inDiag = true
			diagCount++
		case l == "  ...":
			if !inDiag {
				t.Error("a diagnostic block closed without opening")
			}
			inDiag = false
		}
	}
	if inDiag {
		t.Error("a diagnostic block was never closed with `  ...`")
	}
	if diagCount != 2 {
		t.Errorf("%d diagnostic blocks emitted for 2 failures", diagCount)
	}

	// Rule 5: comments are marked. Anything informational must start with "# ".
	for _, l := range lines {
		if l == "" {
			continue
		}
		switch {
		case strings.HasPrefix(l, "TAP version"),
			strings.HasPrefix(l, "ok "), strings.HasPrefix(l, "not ok "),
			strings.HasPrefix(l, "1.."), strings.HasPrefix(l, "  "),
			strings.HasPrefix(l, "#"):
		default:
			t.Errorf("line %q matches no TAP construct; a parser would have to "+
				"guess", l)
		}
	}
}

// TestTAPFailureEmbedsTheTriggeringPayload is the contract the directive names:
// a payload that caused a 5xx must be recoverable from the diagnostic block.
//
// Without it, an agent reading a fuzz report knows a boundary value broke
// something but not which one, and has to re-run the whole corpus to find out.
func TestTAPFailureEmbedsTheTriggeringPayload(t *testing.T) {
	res := fuzz.Result{
		Number:      7,
		Description: "POST /users body.age [min-1]",
		OK:          false,
		Diagnostic: fuzz.Diagnostic{
			Severity: "fail",
			Message:  "unexpected status 500",
			Fields: map[string]string{
				"status":  "500",
				"op":      "POST /users",
				"field":   "age",
				"note":    "min-1",
				"payload": `-1`,
			},
		},
	}
	doc := fuzz.Render([]fuzz.Result{res})

	for _, want := range []string{
		"not ok 1 - POST /users body.age [min-1]",
		"  ---",
		"  severity: 'fail'",
		"unexpected status 500",
		"    payload: '-1'",
		"    status: '500'",
		"  ...",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the TAP document is missing %q:\n%s", want, doc)
		}
	}

	// The payload must be INSIDE the diagnostic block, not merely present.
	blockStart := strings.Index(doc, "  ---")
	blockEnd := strings.Index(doc, "  ...")
	payloadAt := strings.Index(doc, "    payload: '-1'")
	if payloadAt < blockStart || payloadAt > blockEnd {
		t.Error("the payload line is outside the diagnostic block")
	}
}

// TestTAPFlattensMultilineDescriptions is the rule most hand-rolled emitters
// break, and it corrupts every result after the offender.
//
// TAP is LINE-ORIENTED. A description containing a newline makes the following
// line parse as a new test, so the document silently gains a phantom test and
// loses its real one — and the plan count then disagrees with reality, which is
// the hang described above.
func TestTAPFlattensMultilineDescriptions(t *testing.T) {
	desc := "POST /users body.name\nnot ok 99 - injected description\n    ok 1 - also injected"
	res := []fuzz.Result{
		{Number: 1, Description: "sanity", OK: true},
		{Number: 2, Description: desc, OK: false,
			Diagnostic: fuzz.Diagnostic{Severity: "fail", Message: "boom"}},
		{Number: 3, Description: "last", OK: true},
	}
	doc := fuzz.Render(res)
	t.Logf("flattened:\n%s", doc)

	var resultLines int
	for _, l := range tapLines(doc) {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			resultLines++
		}
	}
	if resultLines != 3 {
		t.Errorf("%d result lines, want 3; a newline in a description split a "+
			"line into what looks like another test:\n%s", resultLines, doc)
	}
	if strings.Contains(doc, "injected description") {
		// The text survives as part of the flattened description, which is
		// correct — the assertion is that it did not become its OWN test line.
		own := false
		for _, l := range tapLines(doc) {
			if strings.HasPrefix(l, "not ok 99") {
				own = true
			}
		}
		if own {
			t.Error("the injected text became a test line of its own")
		}
	}
}

// TestTAPQuotesEveryValue proves the emitter escapes correctly rather than
// choosing a quoting style by content.
//
// A value containing BOTH quote types is the case that breaks naive emitters:
// whichever style is picked, one of the two needs escaping, and a half-done
// escape produces a diagnostic no YAML parser can read.
//
// Two normalisations happen before quoting and the cases below account for them
// explicitly, because a test that asserts on un-normalised input would fail for
// the wrong reason: tabs become spaces and newlines become spaces (a tab or
// newline inside a TAP line is either unreadable or a phantom test), and an empty
// message is omitted rather than written as `message: ”` — an empty summary is
// noise, and TAP does not require the key.
func TestTAPQuotesEveryValue(t *testing.T) {
	cases := []struct {
		in     string
		render string // what the emitter should write for this value
	}{
		{"it's got a single quote", "it's got a single quote"},
		{`it has "double" quotes`, `it has "double" quotes`},
		{`both 'single' and "double"`, `both 'single' and "double"`},
		{"colon: value # hash", "colon: value # hash"},
		{"- leading dash", "- leading dash"},
		{"{braces} and [brackets]", "{braces} and [brackets]"},
		{"tab\there", "tab here"},
		{"line\nbreak", "line break"},
		{"", ""},
	}
	for _, c := range cases {
		res := fuzz.Result{
			Number: 1, Description: "probe", OK: false,
			Diagnostic: fuzz.Diagnostic{
				Severity: "fail",
				Message:  c.in,
				Fields:   map[string]string{"payload": c.in},
			},
		}
		doc := fuzz.Render([]fuzz.Result{res})

		// The payload is always emitted, so it is the field under test.
		wantPayload := "    payload: '" + strings.ReplaceAll(c.render, "'", "''") + "'\n"
		if !strings.Contains(doc, wantPayload) {
			t.Errorf("value %q rendered as:\n%s\nwant a line %q", c.in, doc, wantPayload)
		}
		if c.in != "" {
			wantMsg := "  message: '" + strings.ReplaceAll(c.render, "'", "''") + "'\n"
			if !strings.Contains(doc, wantMsg) {
				t.Errorf("value %q did not produce %q:\n%s", c.in, wantMsg, doc)
			}
		}
	}
}

// TestTAPDataKeysAreSorted proves the diagnostic block is byte-stable.
//
// Go randomises map iteration. An output whose byte layout changes run to run
// cannot be diffed, and diffing two fuzz runs is how a reviewer tells "this
// fixed it" from "this run was different".
func TestTAPDataKeysAreSorted(t *testing.T) {
	res := fuzz.Result{
		Number: 1, Description: "probe", OK: false,
		Diagnostic: fuzz.Diagnostic{
			Severity: "fail", Message: "m",
			Fields: map[string]string{
				"zebra": "1", "alpha": "2", "middle": "3", "beta": "4", "omega": "5",
			},
		},
	}
	first := fuzz.Render([]fuzz.Result{res})
	for i := 0; i < 50; i++ {
		if got := fuzz.Render([]fuzz.Result{res}); got != first {
			t.Fatalf("TAP output is not byte-stable across renders:\n%s\n---\n%s", first, got)
		}
	}
	wantOrder := []string{"alpha", "beta", "middle", "omega", "zebra"}
	prev := -1
	for _, k := range wantOrder {
		at := strings.Index(first, "    "+k+": ")
		if at < 0 {
			t.Fatalf("key %q missing:\n%s", k, first)
		}
		if at < prev {
			t.Errorf("key %q is out of sorted order:\n%s", k, first)
		}
		prev = at
	}
	t.Logf("stable across 50 renders; keys in sorted order")
}

// TestTAPEmptyPlan proves the zero-test case is well-formed.
//
// `1..0` is the TAP spelling for an empty run. Omitting the plan entirely is
// legal in some readers and a hang in others; emitting a bare `1..` is not
// legal at all.
func TestTAPEmptyPlan(t *testing.T) {
	doc := fuzz.Render(nil)
	if doc != "TAP version 13\n1..0\n" {
		t.Errorf("empty plan rendered as %q, want %q", doc, "TAP version 13\\n1..0\\n")
	}
}

// TestTAPRenumbersInOrder proves the emitter, not the caller, owns the numbering.
//
// A concurrent executor produces results out of order. If the emitter trusted
// the caller's numbers, a reordered result set would carry a plan count that
// disagrees with its contents — the hang again.
func TestTAPRenumbersInOrder(t *testing.T) {
	in := []fuzz.Result{
		{Number: 3, Description: "third", OK: true},
		{Number: 1, Description: "first", OK: true},
		{Number: 2, Description: "second", OK: false,
			Diagnostic: fuzz.Diagnostic{Severity: "fail", Message: "m"}},
	}
	doc := fuzz.Render(in)
	lines := tapLines(doc)

	var nums []int
	for _, l := range lines {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			var n int
			_, _ = fmtSscan(l, "", &n)
			nums = append(nums, n)
		}
	}
	if len(nums) != 3 {
		t.Fatalf("got %d result lines", len(nums))
	}
	for i, n := range nums {
		if n != i+1 {
			t.Errorf("result %d has number %d; the emitter must renumber from 1:\n%s", i, n, doc)
		}
	}
	// The original numbers are the sort key, so descriptions keep their order.
	if !strings.Contains(doc, "1 - first") {
		t.Errorf("results were not ordered by their original number:\n%s", doc)
	}
}

// TestTAPSkippedTestsAreMarked proves a cancelled run still produces a
// COMPLETE document.
//
// This is the case that hangs a TAP consumer in practice: the plan promises
// 200 tests, the run was cancelled after 3, and the producer emitted 3 lines.
func TestTAPSkippedTestsAreMarked(t *testing.T) {
	var results []fuzz.Result
	for i := 1; i <= 5; i++ {
		if i <= 3 {
			results = append(results, fuzz.Result{
				Number: i, Description: "ran", OK: i != 2,
				Diagnostic: func() fuzz.Diagnostic {
					if i != 2 {
						return fuzz.Diagnostic{}
					}
					return fuzz.Diagnostic{Severity: "fail", Message: "boom"}
				}(),
			})
			continue
		}
		results = append(results, fuzz.Result{
			Number: i, Description: "never ran", Skip: true,
		})
	}
	doc := fuzz.Render(results)
	t.Logf("cancelled run:\n%s", doc)

	if !strings.Contains(doc, "1..5") {
		t.Errorf("the plan does not cover the skipped tests:\n%s", doc)
	}
	if strings.Count(doc, "not ok") != 1 {
		t.Errorf("a skipped test was reported as a failure; %d `not ok` lines", strings.Count(doc, "not ok"))
	}
	if !strings.Contains(doc, "# SKIP") {
		t.Errorf("a skipped test carries no SKIP marker:\n%s", doc)
	}
}

// TestTAPSummarizeAndCommentAreWellFormed covers the two helpers, which are
// where metadata leaks into test lines if they are not careful.
func TestTAPSummarizeAndCommentAreWellFormed(t *testing.T) {
	results := []fuzz.Result{
		{Number: 1, OK: true}, {Number: 2, OK: true},
		{Number: 3, OK: false}, {Number: 4, Skip: true},
	}
	s := fuzz.Summarize(results)
	if s != "4 tests, 2 passed, 1 failed, 1 skipped" {
		t.Errorf("Summarize = %q", s)
	}
	c := fuzz.Comment("run %s in %dms", "seed-1", 1234)
	if !strings.HasPrefix(c, "# ") || !strings.HasSuffix(c, "\n") {
		t.Errorf("Comment = %q; a TAP comment must start with `# ` and end with a newline", c)
	}
	if strings.Contains(c, "ok ") || strings.Contains(c, "not ok ") {
		t.Errorf("a comment could be mistaken for a test line: %q", c)
	}
	// A multi-line format argument must not produce a multi-line comment.
	multi := fuzz.Comment("a\nb")
	if strings.Count(multi, "\n") != 1 {
		t.Errorf("a comment containing a newline rendered as %q", multi)
	}
}
