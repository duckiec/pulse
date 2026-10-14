package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"pulse/internal/store"
)

// TestCollectionAndRunQueriesRoundTrip proves the query surface the MCP
// history tools read through: collections list, requests list and fetch,
// single-run fetch by id (with the ID populated), and paginated
// newest-first history with offsets.
func TestCollectionAndRunQueriesRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.UpsertCollection(ctx, store.Collection{ID: "col-a", RootPath: "/tmp/a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCollection(ctx, store.Collection{ID: "col-b", RootPath: "/tmp/b"}); err != nil {
		t.Fatal(err)
	}
	for i, r := range []store.Request{
		{ID: "req-1", CollectionID: "col-a", FilePath: "/tmp/a/1.http", Name: "one", Method: "GET", URLTemplate: "https://x/1", Ordinal: 1},
		{ID: "req-2", CollectionID: "col-a", FilePath: "/tmp/a/2.http", Name: "two", Method: "POST", URLTemplate: "https://x/2", Ordinal: 0},
		{ID: "req-3", CollectionID: "col-b", FilePath: "/tmp/b/1.http", Method: "GET", URLTemplate: "https://x/3"},
	} {
		_ = i
		if err := s.UpsertRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	cols, err := s.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0].ID != "col-a" || cols[1].ID != "col-b" {
		t.Errorf("collections not ordered by id: %+v", cols)
	}

	defs, err := s.ListRequests(ctx, "col-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].ID != "req-2" || defs[1].ID != "req-1" {
		t.Errorf("requests not in ordinal order: %+v", defs)
	}
	all, err := s.ListRequests(ctx, "")
	if err != nil || len(all) != 3 {
		t.Errorf("empty collection must list all: %d, %v", len(all), err)
	}

	got, err := s.GetRequest(ctx, "req-1")
	if err != nil || got.Method != "GET" || got.URLTemplate != "https://x/1" {
		t.Errorf("GetRequest: %+v, %v", got, err)
	}
	if _, err := s.GetRequest(ctx, "req-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRequest miss must be ErrNotFound, got %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.InsertHistory(ctx, store.HistoryEntry{
			RequestID: "req-1", StartedAt: time.UnixMilli(int64(i+1) * 1000),
			Duration: 5 * time.Millisecond, StatusCode: 200, RunSource: "tui",
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.RecentHistory(ctx, 2, 0)
	if err != nil || len(page) != 2 {
		t.Fatalf("RecentHistory page: %d, %v", len(page), err)
	}
	if page[0].ID == 0 || page[0].StartedAt.Before(page[1].StartedAt) {
		t.Errorf("history must be newest-first with populated ids: %+v", page)
	}
	// Pages must not overlap: the second page holds the remaining row.
	tail, err := s.RecentHistory(ctx, 10, 2)
	if err != nil || len(tail) != 1 || tail[0].ID == page[0].ID || tail[0].ID == page[1].ID {
		t.Errorf("paged history overlaps: %+v vs %+v", page, tail)
	}

	one, err := s.GetRun(ctx, page[0].ID)
	if err != nil || one.RequestID != "req-1" || one.Duration != 5*time.Millisecond {
		t.Errorf("GetRun: %+v, %v", one, err)
	}
	if _, err := s.GetRun(ctx, 1<<40); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRun miss must be ErrNotFound, got %v", err)
	}
}
