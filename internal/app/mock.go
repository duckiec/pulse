package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/store"
)

// Mock is one `pulse mock --from-cache` session: the route table, the store
// the table is backed by, and the lifecycle the CLI must close.
//
// It lives in the composition root because the engine may not import the store
// and cmd may not open one (AGENTS.md §3.1). The CLI only starts a listener.
type Mock struct {
	// Server replays cached responses; it is an http.Handler.
	Server *engine.MockServer
	// Store backs the replay cache and is closed by Close.
	Store *store.Store
	// DBPath is where the cache lives, for the startup banner.
	DBPath string
}

// OpenMock opens the local replay cache and builds the route table from the
// given .http file or directory.
//
// The store is opened rather than read: SQLite needs it to see WAL contents
// written by a still-running TUI or MCP server, and opening is the only
// supported way to read them. A missing database is created empty, and every
// route then reports "not cached yet" — which is the truthful answer for a
// fresh checkout, not an error.
func OpenMock(ctx context.Context, cfg Config, paths []string) (*Mock, error) {
	dbPath, err := mockDBPath(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("app: create the data directory for %s: %w", dbPath, err)
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	srv, err := engine.NewMockServer(engine.MockConfig{
		Paths:  paths,
		Source: mockCache{st: st},
		Logf:   mcp.Logf,
	})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return &Mock{Server: srv, Store: st, DBPath: dbPath}, nil
}

// Close releases the cache handle.
func (m *Mock) Close() error {
	if m == nil || m.Store == nil {
		return nil
	}
	return m.Store.Close()
}

// mockDBPath resolves the database the same way app.New does, without opening
// the rest of the application: a replay server needs no keychain, no resolver,
// no RAG index and no sync network.
func mockDBPath(cfg Config) (string, error) {
	if cfg.DBPath != "" {
		return cfg.DBPath, nil
	}
	home := cfg.Home
	if home == "" {
		var err error
		home, err = DefaultHome()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(home, DefaultDBName), nil
}

// mockCache adapts store.MockResponse to engine.MockSource.
//
// ErrNotFound maps to ok=false, not to an error: the engine has one 404 for
// "never cached" with a hint, and a different 500 for "the cache is broken".
// Collapsing them would tell a user with a corrupt database to run the request
// again.
type mockCache struct{ st *store.Store }

func (c mockCache) LatestMock(ctx context.Context, method, route string) (engine.MockResponse, bool, error) {
	row, err := c.st.LatestMockResponse(ctx, method, route)
	if errors.Is(err, store.ErrNotFound) {
		return engine.MockResponse{}, false, nil
	}
	if err != nil {
		return engine.MockResponse{}, false, err
	}
	return engine.MockResponse{
		Status: row.StatusCode,
		Header: http.Header(row.Header),
		Body:   []byte(row.Replay),
	}, true, nil
}
