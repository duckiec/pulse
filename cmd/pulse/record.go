package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"

	"pulse/internal/app"
)

// runRecord is the `pulse record <listen> --target <url> [--out file]` entry.
//
// The proxy and the .http renderer belong to internal/engine; this function
// validates flags, opens the listener and prints what was wired. The banner is
// deliberately explicit about redaction: the generated file is meant to be
// committed, so a user must know credential headers became {{VARS}} and that
// other values are only scrubbed when they were registered secrets.
func runRecord(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "help" {
		printHelp(stdout, recordHelp)
		return exitOK
	}
	if wantsFlagHelp(args) {
		printHelp(stdout, recordHelp)
		return exitOK
	}
	args = shiftFlags(args, map[string]bool{"target": true, "out": true})
	fs := flag.NewFlagSet("pulse record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		target = fs.String("target", "", "upstream base URL to forward traffic to")
		out    = fs.String("out", "generated.http", "file to append captured requests to")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, recordHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	listen := fs.Arg(0)
	if listen == "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "pulse record: exactly one listen address is required, e.g. :9000")
		fmt.Fprintln(stderr, "run `pulse help record` for usage.")
		return exitUsage
	}
	if *target == "" {
		fmt.Fprintln(stderr, "pulse record: --target <url> is required")
		fmt.Fprintln(stderr, "run `pulse help record` for usage.")
		return exitUsage
	}

	rec, err := app.NewRecorder(appConfig(), *target, *out)
	if err != nil {
		fmt.Fprintf(stderr, "pulse record: %v\n", err)
		return exitUsage
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(stderr, "pulse record: listen on %s: %v\n", listen, err)
		return exitFail
	}

	fmt.Fprintln(stdout, "pulse record")
	fmt.Fprintf(stdout, "  listening on http://%s\n", ln.Addr().String())
	fmt.Fprintf(stdout, "  forwarding to %s\n", *target)
	fmt.Fprintf(stdout, "  appending captured requests to %s\n", *out)
	fmt.Fprintln(stdout, "  credential headers are redacted to {{AUTH_TOKEN}}-style variables;")
	fmt.Fprintln(stdout, "  review the file before sharing it. Ctrl-C to stop.")
	return serveUntilSignal(ctx, rec, ln, stdout, stderr, "pulse record")
}
