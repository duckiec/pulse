package app

// White-box gate for the execution recorder: RecordExecution must file
// the collection, the request, and the run — and a resolved secret in
// the error text must arrive scrubbed, never raw.

import (
	"context"
	"strings"
	"testing"

	"pulse/internal/mcp"
	"pulse/internal/secrets"
	"pulse/internal/store"
)

func TestRecordExecutionFilesCollectionRequestAndRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir+"/pulse.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	red := secrets.NewRedactor()
	const token = "record-gate-token-9f2c"
	red.Register(token)

	r := &execRecorder{db: db, red: red}
	r.RecordExecution(ctx, mcp.ExecutionRecord{
		Origin: "coll/orders.http", Index: 0, Name: "create",
		Method: "POST", URLTemplate: "http://x/v1/orders",
		Status: 200, RequestBytes: 41, ResponseBytes: 18,
		Digest: "abc123", ElapsedMS: 7,
		Err: "upstream said " + token,
	})

	cols, err := db.ListCollections(ctx)
	if err != nil || len(cols) != 1 || cols[0].ID != "coll/orders.http" {
		t.Fatalf("collections = %+v, err = %v", cols, err)
	}
	got, err := db.GetRequest(ctx, "coll/orders.http#0")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if got.Name != "create" || got.Method != "POST" {
		t.Fatalf("request = %+v", got)
	}
	runs, err := db.RecentHistory(ctx, 10, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("history = %+v, err = %v", runs, err)
	}
	if runs[0].StatusCode != 200 || runs[0].RequestID != "coll/orders.http#0" {
		t.Fatalf("run = %+v", runs[0])
	}
	if strings.Contains(runs[0].Error, token) {
		t.Fatalf("resolved secret reached the history table: %q", runs[0].Error)
	}
	if !strings.Contains(runs[0].Error, secrets.Placeholder) {
		t.Fatalf("error text lost its shape: %q", runs[0].Error)
	}
}

// TestRecordExecutionWithoutAStoreIsSilent pins the degradation rule: a
// nil store must not panic and must not fail anything — there is no run
// to fail here, only a row to lose.
func TestRecordExecutionWithoutAStoreIsSilent(t *testing.T) {
	r := &execRecorder{}
	r.RecordExecution(context.Background(), mcp.ExecutionRecord{Origin: "x", Index: 0})
	var nilR *execRecorder
	nilR.RecordExecution(context.Background(), mcp.ExecutionRecord{Origin: "x", Index: 0})
}
