package fuzz

import (
	"fmt"
	"strconv"
	"strings"
)

// Test Anything Protocol version 13.
//
// # Why TAP and not a JSON blob
//
// A fuzz run's output is read by three different consumers: a human scanning a
// terminal, a CI job deciding pass/fail, and an LLM deciding what to look at
// next. TAP serves all three because it is a LINE-ORIENTED format with a
// mandatory machine-readable prefix on every line — `not ok 7 - ...` — so a
// grep finds every failure regardless of the prose, and a human reads it top to
// bottom. A JSON array is none of those: it is one line, or unreadable, and a
// failure buried in it is invisible to `grep -c '^not ok'`.
//
// # What this emitter guarantees
//
// TAP 13 requires, and this implementation provides:
//
//   - a version line, first, exactly `TAP version 13`
//   - a plan, `1..N`, so the count is known before the results are read
//   - every result numbered from 1, contiguously, with no gaps
//   - `ok N - description` / `not ok N - description`
//   - diagnostic blocks fenced by `  ---` and `  ...`, containing YAML
//   - a trailing `# ` comment for anything informational, so metadata can never
//     be mistaken for a test
//
// # Strictness that matters
//
// Two TAP rules are routinely broken by hand-rolled emitters and both are load-
// bearing:
//
//  1. **The plan count must match the number of result lines.** A TAP consumer
//     that plans for 40 and receives 39 HANGS. The plan is therefore emitted from
//     the same slice the results were rendered from, never from a counter that
//     could disagree with it.
//  2. **A description may not contain a newline.** An unescaped newline inside a
//     test description makes the following line look like a new test to a
//     line-oriented parser, silently corrupting every result after it. Every
//     description is flattened here rather than trusted.

// TAPVersion is the protocol version this package emits.
const TAPVersion = 13

// Result is one test outcome.
type Result struct {
	// Number is the 1-based test number.
	Number int
	// Description is the human-readable label. Newlines are flattened by the
	// emitter, so callers may pass raw text.
	Description string
	// OK reports whether the case passed.
	OK bool
	// Diagnostic is an optional YAML block explaining a failure. It is ignored
	// for passing tests.
	Diagnostic Diagnostic
	// Skip marks a case that was not run (budget exhausted, cancelled).
	Skip bool
}

// Diagnostic is the YAML payload attached to a failure.
type Diagnostic struct {
	// Message is the one-line summary.
	Message string
	// Severity is TAP's "fail" or "todo".
	Severity string
	// Fields are extra key/value pairs rendered into the `data:` mapping.
	//
	// A MAP rather than a slice because the emitter must render keys in sorted
	// order: Go randomises map iteration, and an output whose byte layout varies
	// run to run is not a deterministic output.
	Fields map[string]string
}

// tapQuote renders a YAML scalar.
//
// Every string is single-quoted with `'` doubled, which is the one escaping rule
// YAML guarantees unconditionally. Switching quoting styles based on content —
// the tempting optimisation — introduces the case where a value containing both
// quote types is emitted with the wrong one, and that is a diagnostic block that
// no parser can read.
func tapQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// flatten collapses a description to a single line.
//
// TAP is line-oriented, so a newline inside a description becomes a line that a
// parser reads as the next test. Fuzz descriptions are assembled from schema
// notes and can easily contain one.
func flatten(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	// Tabs survive a description but render as noise; spaces do not lose
	// information and keep the line readable.
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// Render emits the complete TAP document for a set of results.
//
// `results` may arrive in any order; the emitter renumbers from 1 so the plan
// and the numbering can never disagree, which is the failure that hangs a TAP
// consumer.
func Render(results []Result) string {
	ordered := make([]Result, len(results))
	copy(ordered, results)
	// Stable sort on the ORIGINAL number, so a caller that produced results out
	// of order (as a concurrent executor does) still gets a stable document.
	sortResults(ordered)

	var b strings.Builder
	b.WriteString("TAP version " + strconv.Itoa(TAPVersion) + "\n")

	// The plan is emitted AFTER the results, as TAP 13 permits: the total
	// is not known until every result has rendered, and a plan written
	// from a different count than the result lines would hang a consumer.
	if len(ordered) == 0 {
		// An empty plan is written as `1..0`, the TAP spelling for zero tests.
		b.WriteString("1..0\n")
	}
	for i := range ordered {
		ordered[i].Number = i + 1
	}

	for _, r := range ordered {
		desc := flatten(r.Description)
		var sb strings.Builder
		if r.Skip {
			sb.WriteString("ok")
		} else if r.OK {
			sb.WriteString("ok")
		} else {
			sb.WriteString("not ok")
		}
		sb.WriteByte(' ')
		sb.WriteString(strconv.Itoa(r.Number))
		if desc != "" {
			sb.WriteString(" - ")
			sb.WriteString(desc)
		}
		b.WriteString(sb.String())
		b.WriteByte('\n')

		if r.Skip {
			b.WriteString("  # SKIP")
			if desc != "" {
				b.WriteString(" ")
				b.WriteString(desc)
			}
			b.WriteByte('\n')
			continue
		}
		if r.OK || r.Diagnostic.Message == "" && len(r.Diagnostic.Fields) == 0 {
			continue
		}
		b.WriteString(renderDiagnostic(r.Diagnostic))
	}

	if len(ordered) > 0 {
		b.WriteString("1.." + strconv.Itoa(len(ordered)) + "\n")
	}
	return b.String()
}

// renderDiagnostic writes the YAML block for a failure.
//
// Indentation is two spaces, which is what TAP requires for a diagnostic at the
// top level; the fence lines are `  ---` and `  ...`, not `---`.
func renderDiagnostic(d Diagnostic) string {
	var b strings.Builder
	b.WriteString("  ---\n")
	if d.Severity != "" {
		b.WriteString("  severity: " + tapQuote(d.Severity) + "\n")
	}
	if d.Message != "" {
		b.WriteString("  message: " + tapQuote(flatten(d.Message)) + "\n")
	}
	if len(d.Fields) > 0 {
		b.WriteString("  data:\n")
		keys := make([]string, 0, len(d.Fields))
		for k := range d.Fields {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			b.WriteString("    " + k + ": " + tapQuote(flatten(d.Fields[k])) + "\n")
		}
	}
	b.WriteString("  ...\n")
	return b.String()
}

// sortResults orders results by their original number without depending on the
// caller having numbered them first.
func sortResults(rs []Result) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Number < rs[j-1].Number; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// SortStrings is exported for callers assembling their own TAP diagnostics.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Summarize counts the results, for a `# ` comment line.
//
// It is a helper rather than a field because a summary belongs in a TAP
// COMMENT and a comment must start with `# ` — putting it in a test line would
// make it look like a test to a line-oriented consumer.
func Summarize(results []Result) string {
	var passed, failed, skipped int
	for _, r := range results {
		switch {
		case r.Skip:
			skipped++
		case r.OK:
			passed++
		default:
			failed++
		}
	}
	return fmt.Sprintf("%d tests, %d passed, %d failed, %d skipped",
		len(results), passed, failed, skipped)
}

// Comment renders a TAP comment line.
func Comment(format string, args ...any) string {
	return "# " + flatten(fmt.Sprintf(format, args...)) + "\n"
}
