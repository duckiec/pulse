package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/secrets"
	"pulse/internal/store"
)

// execRecorder is the composition root's answer to "who remembers runs":
// it files every EngineExecutor outcome into the store's collections,
// requests, and history tables, which is what list_collections,
// list_requests, get_request, get_run, and get_history serve.
//
// Secrets never reach the tables: names, URL templates, and error text
// pass through the redactor first (URL templates keep their {{anchors}} —
// the store records what the file SAID, and anchors are names, not
// values). A recording failure is logged and dropped, never returned:
// losing one history row is recoverable, failing the request is not.
type execRecorder struct {
	db  *store.Store
	red *secrets.Redactor
}

func (r *execRecorder) RecordExecution(ctx context.Context, rec mcp.ExecutionRecord) {
	if r == nil || r.db == nil {
		return
	}
	scrub := func(s string) string {
		if r.red == nil {
			return s
		}
		return r.red.String(s)
	}
	colID := rec.Origin
	if colID == "" {
		colID = "<inline payload>"
	}
	if err := r.db.UpsertCollection(ctx, store.Collection{ID: colID, RootPath: colID}); err != nil {
		mcp.Logf("history: record collection %q: %v", colID, err)
		return
	}
	reqID := fmt.Sprintf("%s#%d", colID, rec.Index)
	if err := r.db.UpsertRequest(ctx, store.Request{
		ID: reqID, CollectionID: colID, FilePath: colID,
		Name: scrub(rec.Name), Method: rec.Method,
		URLTemplate: scrub(rec.URLTemplate), Ordinal: rec.Index,
	}); err != nil {
		mcp.Logf("history: record request %q: %v", reqID, err)
		return
	}
	started := time.Now().Add(-time.Duration(rec.ElapsedMS) * time.Millisecond)
	if err := r.db.InsertHistory(ctx, store.HistoryEntry{
		RequestID: reqID, StartedAt: started,
		Duration:      time.Duration(rec.ElapsedMS) * time.Millisecond,
		StatusCode:    rec.Status,
		RequestBytes:  rec.RequestBytes,
		ResponseBytes: rec.ResponseBytes,
		SHA256:        rec.Digest,
		Error:         scrub(rec.Err),
		RunSource:     "mcp",
	}); err != nil {
		mcp.Logf("history: record run for %q: %v", reqID, err)
	}
	cacheMockResponse(ctx, r.db, r.red, rec.Method, rec.URLTemplate, rec.Status,
		rec.Headers, rec.Body, rec.BodyTruncated)
}

// mockCacheSink adapts the replay cache to the test runner's capture hook.
// Nil when no store is wired, which is honest: capture is unavailable, not
// silently discarded.
func (a *App) mockCacheSink() func(engine.MockSample) {
	if a == nil || a.Store == nil {
		return nil
	}
	return func(s engine.MockSample) {
		cacheMockResponse(context.Background(), a.Store, a.Redactor,
			s.Method, s.URL, s.Status, s.Header, s.Body, false)
	}
}

// cacheMockResponse keeps the latest successful response for one route so
// `pulse mock --from-cache` can replay it offline.
//
// Two things are enforced here rather than trusted from callers: a non-2xx run
// is never cached (the replay server only promises successful traffic), and both
// headers and body pass through the redactor again. The second pass is not
// redundant with the capture side: a response body is the one payload where a
// missed registration would be permanent on disk.
func cacheMockResponse(ctx context.Context, db *store.Store, red *secrets.Redactor,
	method, rawURL string, status int, header http.Header, body string, truncated bool) {
	if db == nil || truncated || status < 200 || status > 299 {
		return
	}
	route, ok := engine.RoutePath(rawURL)
	if !ok {
		return
	}
	safe := header
	if red != nil {
		if len(header) > 0 {
			safe = red.Headers(header)
		}
		body = red.String(body)
	}
	if err := db.PutMockResponse(ctx, store.MockResponse{
		Method: method, Route: route, StatusCode: status,
		Header: map[string][]string(safe), Replay: body,
		RecordedAt: time.Now(),
	}); err != nil {
		// A cache write that fails must not fail the run: the response already
		// reached the caller, and a missing replay row is recoverable.
		mcp.Logf("mock cache: %s %s: %v", method, route, err)
	}
}
