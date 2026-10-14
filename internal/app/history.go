package app

import (
	"context"

	"pulse/internal/mcp"
	"pulse/internal/store"
)

// storeHistory adapts *store.Store to the mcp.HistoryStore query surface.
//
// It lives HERE rather than in internal/mcp because this is the one package
// allowed to know both sides (AGENTS.md §3.1): mcp must never import store,
// so the adapter — and the proof it satisfies the contract — belongs to the
// composition root. The mapping copies values, never secrets: the store keeps
// metadata and digests only, and the error text is scrubbed downstream by the
// bridge's scrubRunRecord chokepoint before any agent sees it.
type storeHistory struct {
	db *store.Store
}

var _ mcp.HistoryStore = (*storeHistory)(nil)

func (h *storeHistory) ListCollections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	cols, err := h.db.ListCollections(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.CollectionInfo, 0, len(cols))
	for _, c := range cols {
		out = append(out, mcp.CollectionInfo{
			ID:        c.ID,
			RootPath:  c.RootPath,
			CreatedAt: c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	return out, nil
}

func (h *storeHistory) ListRequests(ctx context.Context, collectionID string) ([]mcp.RequestInfo, error) {
	defs, err := h.db.ListRequests(ctx, collectionID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RequestInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, mcp.RequestInfo{
			ID: d.ID, CollectionID: d.CollectionID, FilePath: d.FilePath,
			Name: d.Name, Method: d.Method, URLTemplate: d.URLTemplate,
			Ordinal: d.Ordinal,
		})
	}
	return out, nil
}

func (h *storeHistory) GetRequest(ctx context.Context, id string) (mcp.RequestInfo, error) {
	d, err := h.db.GetRequest(ctx, id)
	if err != nil {
		return mcp.RequestInfo{}, err
	}
	return mcp.RequestInfo{
		ID: d.ID, CollectionID: d.CollectionID, FilePath: d.FilePath,
		Name: d.Name, Method: d.Method, URLTemplate: d.URLTemplate,
		Ordinal: d.Ordinal,
	}, nil
}

func (h *storeHistory) GetRun(ctx context.Context, id int64) (mcp.RunRecord, error) {
	return toRunRecord(h.db.GetRun(ctx, id))
}

func (h *storeHistory) RecentHistory(ctx context.Context, limit, offset int) ([]mcp.RunRecord, error) {
	entries, err := h.db.RecentHistory(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RunRecord, 0, len(entries))
	for _, e := range entries {
		rec, err := toRunRecord(e, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func toRunRecord(e store.HistoryEntry, err error) (mcp.RunRecord, error) {
	if err != nil {
		return mcp.RunRecord{}, err
	}
	return mcp.RunRecord{
		ID: e.ID, RequestID: e.RequestID,
		StartedAt:  e.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		DurationMS: e.Duration.Milliseconds(), StatusCode: e.StatusCode,
		RequestBytes: e.RequestBytes, ResponseBytes: e.ResponseBytes,
		SHA256: e.SHA256, Error: e.Error, RunSource: e.RunSource,
	}, nil
}
