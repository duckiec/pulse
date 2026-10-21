package main

import (
	"fmt"
	"io"

	"pulse/internal/app"
)

// runDrift is the `pulse drift <spec> <collection>` entry.
//
// It is deliberately not built on app.New: drift reads two plain files and
// talks to nothing, so it must work on a machine with no keychain, no store and
// no network. The CLI owns the report format; the engine owns the findings.
//
// Exit codes: 0 clean, 1 violations, 2 usage/spec/collection error.
func runDrift(args []string, stdout, stderr io.Writer) int {
	// Help everywhere: `pulse drift --help`, `pulse drift -h`, `pulse drift help`.
	if len(args) == 1 && args[0] == "help" {
		printHelp(stdout, driftHelp)
		return exitOK
	}
	if wantsFlagHelp(args) {
		printHelp(stdout, driftHelp)
		return exitOK
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, "pulse drift: an OpenAPI document and a .http file or directory are required")
		fmt.Fprintln(stderr, "run `pulse help drift` for usage.")
		return exitUsage
	}

	res, err := app.DriftSpec(args[0], args[1])
	if err != nil {
		// A missing spec, an unreadable collection, or a parse failure is an
		// input problem, not a drift finding: exit 2 so a CI job can tell
		// "your collection drifted" from "the check could not run".
		fmt.Fprintf(stderr, "pulse drift: %v\n", err)
		return exitUsage
	}

	rep := res.Report
	for _, f := range rep.Findings {
		fmt.Fprintln(stdout, f.String())
	}
	if rep.Clean() {
		fmt.Fprintf(stdout, "drift: no violations in %d request(s) across %d file(s); %d spec operation(s) checked (OpenAPI %s)\n",
			rep.Requests, rep.Files, rep.Operations, res.SpecVersion)
		return exitOK
	}
	fmt.Fprintf(stdout, "drift: %d violation(s) in %d request(s) across %d file(s); %d spec operation(s) checked (OpenAPI %s)\n",
		len(rep.Findings), rep.Requests, rep.Files, rep.Operations, res.SpecVersion)
	return exitFail
}
