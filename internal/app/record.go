package app

import (
	"context"
	"fmt"
	"time"

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
}
