package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"

	"pulse/internal/app"
)

// runMock is the `pulse mock <path> --from-cache [--port N]` entry.
//
// The command owns the listener and the signal handling; the route table, the
// cache lookup and the response shape belong to internal/engine and
// internal/app. Booting the full App would be wrong here: an air-gap replay
// server must start on a plane, so it opens the SQLite cache and nothing else
// (no keychain probe, no resolver, no sync network).
func runMock(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "help" {
		printHelp(stdout, mockHelp)
		return exitOK
	}
	if wantsFlagHelp(args) {
		printHelp(stdout, mockHelp)
		return exitOK
	}
	args = shiftFlags(args, map[string]bool{
		"from-cache": false, "port": true, "host": true,
	})
	fs := flag.NewFlagSet("pulse mock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		fromCache = fs.Bool("from-cache", false,
			"replay the last successful responses recorded in the local cache")
		port = fs.Int("port", 8080, "TCP port to listen on")
		host = fs.String("host", "127.0.0.1",
			"interface to bind (127.0.0.1 keeps the cache on this machine; 0.0.0.0 exposes it to the LAN)")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, mockHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	path := fs.Arg(0)
	if path == "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "pulse mock: exactly one .http file or directory is required")
		fmt.Fprintln(stderr, "run `pulse help mock` for usage.")
		return exitUsage
	}
	if !*fromCache {
		// Fail closed rather than silently doing something else: a future
		// live-upstream mode is a different feature with different guarantees,
		// and pretending this flag is optional would hide that.
		fmt.Fprintln(stderr, "pulse mock: --from-cache is required (it is the only replay mode)")
		fmt.Fprintln(stderr, "run `pulse help mock` for usage.")
		return exitUsage
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "pulse mock: --port %d is not a valid TCP port\n", *port)
		return exitUsage
	}

	mock, err := app.OpenMock(ctx, appConfig(), []string{path})
	if err != nil {
		fmt.Fprintf(stderr, "pulse mock: %v\n", err)
		return exitFail
	}
	defer func() {
		if err := mock.Close(); err != nil {
			fmt.Fprintf(stderr, "pulse mock: close the cache: %v\n", err)
		}
	}()
	for _, warning := range mock.Server.Warnings() {
		fmt.Fprintf(stderr, "pulse mock: %s\n", warning)
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "pulse mock: listen on %s: %v\n", addr, err)
		return exitFail
	}
	fmt.Fprint(stdout, mock.Server.Banner(ln.Addr().String()))
	fmt.Fprintf(stdout, "  cache: %s\n", mock.DBPath)
	fmt.Fprintln(stdout, "  Ctrl-C to stop.")
	return serveUntilSignal(ctx, mock.Server, ln, stdout, stderr, "pulse mock")
}
