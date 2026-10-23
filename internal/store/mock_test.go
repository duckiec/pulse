package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pulse/internal/store"
)

func TestMockResponseRoundTripKeepsOneRowPerRoute(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	first := store.MockResponse{
		Method: "get", Route: "/users/{*}", StatusCode: 200,
		Header:     map[string][]string{"Content-Type": {"application/json"}},
		Replay:     `{"users":["ada"]}`,
		RecordedAt: now,
	}
	if err := s.PutMockResponse(ctx, first); err != nil {
		t.Fatalf("PutMockResponse: %v", err)
	}
	// A newer run for the same route replaces the row rather than appending:
	// --from-cache means "the latest success", and an append-only cache would
	// grow with every test run.
	second := first
	second.Replay = `{"users":["ada","grace"]}`
	second.RecordedAt = now.Add(time.Minute)
	if err := s.PutMockResponse(ctx, second); err != nil {
		t.Fatalf("PutMockResponse (replace): %v", err)
	}

	got, err := s.LatestMockResponse(ctx, "GET", "/users/{*}")
	if err != nil {
		t.Fatalf("LatestMockResponse: %v", err)
	}
	if got.Replay != second.Replay {
		t.Errorf("replay = %q, want the latest %q", got.Replay, second.Replay)
	}
	if got.StatusCode != 200 {
		t.Errorf("status = %d, want 200", got.StatusCode)
	}
	if got.Header["Content-Type"][0] != "application/json" {
		t.Errorf("header = %v, want the cached Content-Type", got.Header)
	}
	if !got.RecordedAt.Equal(second.RecordedAt) {
		t.Errorf("recorded_at = %s, want %s", got.RecordedAt, second.RecordedAt)
	}

	var rows int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mock_responses`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("cache holds %d rows for one route, want 1", rows)
	}
}

func TestMockResponseMissIsNotFound(t *testing.T) {
	s := openTemp(t)
	_, err := s.LatestMockResponse(context.Background(), "GET", "/absent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMockResponseRefusesNonSuccessAndOversize(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.PutMockResponse(ctx, store.MockResponse{
		Method: "GET", Route: "/boom", StatusCode: 500, Replay: "nope",
	}); !errors.Is(err, store.ErrMockNotSuccess) {
		t.Fatalf("caching a 500 returned %v, want ErrMockNotSuccess", err)
	}

	tooBig := strings.Repeat("x", store.MaxMockReplayBytes+1)
	if err := s.PutMockResponse(ctx, store.MockResponse{
		Method: "GET", Route: "/big", StatusCode: 200, Replay: tooBig,
	}); err == nil {
		t.Fatal("caching an oversize body succeeded; the replay cache must stay bounded")
	}
}
