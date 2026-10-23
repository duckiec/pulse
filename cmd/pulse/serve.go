package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// serverShutdownTimeout bounds graceful shutdown. In-flight proxied requests
// get this long to finish before the listener is torn down; a replay server
// with nothing in flight closes immediately.
const serverShutdownTimeout = 5 * time.Second

// serveUntilSignal runs handler on ln until SIGINT/SIGTERM, then shuts the
// server down gracefully.
//
// Both `pulse mock` and `pulse record` are foreground servers, so they share one
// lifecycle: no daemonizing, no pid file, and an interrupt is a clean stop
// (exit 0) rather than the 130 `pulse test` reserves for an interrupted run.
// The listener is closed by Shutdown, which is why neither caller defers a
// Close on it.
func serveUntilSignal(ctx context.Context, handler http.Handler, ln net.Listener, stdout, stderr io.Writer, name string) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler: handler,
		// A stalled client that never sends headers must not pin a
		// connection slot forever. It is a local development server, so a
		// short bound is enough and cheaper than a configurable one.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "%s: serve: %v\n", name, err)
			return exitFail
		}
		return exitOK
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "%s: shutdown: %v\n", name, err)
			return exitFail
		}
		fmt.Fprintf(stdout, "%s: stopped\n", name)
		return exitOK
	}
}
