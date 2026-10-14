package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Tool names are snake_case per MCP convention (MEMORY.md ADL-0020). The issue
// text spells them kebab-case; the advertised names below win, because agents
// match on the advertised name exactly and the one built tool already sets the
// precedent (execute_request, execute_fuzz_run).
const (
	ToolListCollections = "list_collections"
	ToolListRequests    = "list_requests"
	ToolGetRequest      = "get_request"
	ToolGetRun          = "get_run"
	ToolGetHistory      = "get_history"
)

// CollectionInfo is one collection row as an agent sees it.
type CollectionInfo struct {
	ID        string `json:"id"`
	RootPath  string `json:"root_path"`
	CreatedAt string `json:"created_at"`
}

// RequestInfo is one stored request definition as an agent sees it.
//
// URLTemplate is the RAW template including {{var}} anchors: the store records
// what the file said, never what was sent, so no resolved secret can enter
// this field by construction.
type RequestInfo struct {
	ID           string `json:"id"`
	CollectionID string `json:"collection_id"`
	FilePath     string `json:"file_path"`
	Name         string `json:"name"`
	Method       string `json:"method"`
	URLTemplate  string `json:"url_template"`
	Ordinal      int    `json:"ordinal"`
}

// RunRecord is one executed request's metadata as an agent sees it.
//
// There is deliberately no body and no header map: the store keeps metadata
// and the response digest only (ARCHITECTURE.md §8), so history cannot leak
// what it never persisted. Every string this record carries still passes
// through the Redactor chokepoint before transmission — see scrubRunRecord.
type RunRecord struct {
	ID            int64  `json:"id"`
	RequestID     string `json:"request_id"`
	StartedAt     string `json:"started_at"`
	DurationMS    int64  `json:"duration_ms"`
	StatusCode    int    `json:"status_code"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	SHA256        string `json:"sha256"`
	Error         string `json:"error,omitempty"`
	RunSource     string `json:"run_source"`
}

// HistoryStore is the store query surface the history tools need.
//
// It is an interface so internal/mcp never imports internal/store (AGENTS.md
// §3.1): the composition root adapts *store.Store, and tests substitute a
// stub. The DTOs live here rather than in store for the same reason — the
// protocol layer owns what it advertises.
type HistoryStore interface {
	ListCollections(ctx context.Context) ([]CollectionInfo, error)
	ListRequests(ctx context.Context, collectionID string) ([]RequestInfo, error)
	GetRequest(ctx context.Context, id string) (RequestInfo, error)
	GetRun(ctx context.Context, id int64) (RunRecord, error)
	RecentHistory(ctx context.Context, limit, offset int) ([]RunRecord, error)
}

// scrubRunRecord is the unified Redactor chokepoint for historical payloads.
//
// EVERY response from get_run and get_history passes through it — no handler
// may construct a tool result from a RunRecord by any other path. A SHA-256
// digest is a hash and passes through untouched (scrubbing it would destroy
// the checksum without hiding anything); the error text and the provenance
// label are scrubbed, because an engine error can echo the request that
// triggered it and a run_source is caller-controlled text.
//
// Mutation M87 removes the scrub call in a handler; the Secret-Confinement
// Gate names the raw vault value in the response JSON.
func scrubRunRecord(red Redactor, r RunRecord) RunRecord {
	if red == nil {
		return r
	}
	r.Error = red.String(r.Error)
	r.RunSource = red.String(r.RunSource)
	return r
}

// RegisterHistoryTools installs the five collection/history tools.
//
// A nil store omits all five (an agent that cannot see a tool will not call
// it, which is honest — the same degradation rule Config documents for the
// fuzz runner). A nil redactor serves metadata unscrubbed by value while
// header-name redaction still applies elsewhere; production always supplies
// the vault's Redactor via the composition root.
func RegisterHistoryTools(srv *mcpserver.MCPServer, st HistoryStore, red Redactor) error {
	if srv == nil {
		return fmt.Errorf("mcp: nil server")
	}
	if st == nil {
		return nil
	}

	listCollections := mcp.NewTool(ToolListCollections,
		mcp.WithDescription("List the available .http collections (environments)."))
	srv.AddTool(listCollections, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cols, err := st.ListCollections(ctx)
		if err != nil {
			return errorResult(mapStoreError(err)), nil
		}
		if cols == nil {
			cols = []CollectionInfo{}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderCollections(cols))},
			StructuredContent: map[string]any{"collections": cols},
		}, nil
	})

	listRequests := mcp.NewTool(ToolListRequests,
		mcp.WithDescription("List the request definitions in a collection. Omit collection to list every request."),
		mcp.WithString("collection",
			mcp.Description("Collection id. Empty lists every request across all collections.")))
	srv.AddTool(listRequests, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		collection, err := stringArg(req.GetArguments(), "collection", false)
		if err != nil {
			return errorResult(err), nil
		}
		defs, err := st.ListRequests(ctx, collection)
		if err != nil {
			return errorResult(mapStoreError(err)), nil
		}
		if defs == nil {
			defs = []RequestInfo{}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderRequests(defs))},
			StructuredContent: map[string]any{"requests": defs},
		}, nil
	})

	getRequest := mcp.NewTool(ToolGetRequest,
		mcp.WithDescription("Return the stored definition of one request: method, URL template, file, and position."),
		mcp.WithString("ref",
			mcp.Required(),
			mcp.Description("Request id.")))
	srv.AddTool(getRequest, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ref, err := stringArg(req.GetArguments(), "ref", true)
		if err != nil {
			return errorResult(err), nil
		}
		def, err := st.GetRequest(ctx, ref)
		if err != nil {
			return errorResult(mapStoreError(err)), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderRequest(def))},
			StructuredContent: def,
		}, nil
	})

	getRun := mcp.NewTool(ToolGetRun,
		mcp.WithDescription("Return the execution details of one run by its run id. Secrets are redacted."),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("Run id as returned by get_history.")))
	srv.AddTool(getRun, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := intArg(req.GetArguments(), "run_id", true)
		if err != nil {
			return errorResult(err), nil
		}
		rec, err := st.GetRun(ctx, id)
		if err != nil {
			return errorResult(mapStoreError(err)), nil
		}
		rec = scrubRunRecord(red, rec)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderRun(rec))},
			StructuredContent: rec,
		}, nil
	})

	getHistory := mcp.NewTool(ToolGetHistory,
		mcp.WithDescription("Return recent executions newest-first. Secrets are redacted."),
		mcp.WithNumber("limit",
			mcp.Description("Maximum runs to return. Default 50.")),
		mcp.WithNumber("offset",
			mcp.Description("Runs to skip for pagination. Default 0.")))
	srv.AddTool(getHistory, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit, err := intArg(req.GetArguments(), "limit", false)
		if err != nil {
			return errorResult(err), nil
		}
		offset, err := intArg(req.GetArguments(), "offset", false)
		if err != nil {
			return errorResult(err), nil
		}
		recs, err := st.RecentHistory(ctx, int(limit), int(offset))
		if err != nil {
			return errorResult(mapStoreError(err)), nil
		}
		for i := range recs {
			recs[i] = scrubRunRecord(red, recs[i])
		}
		if recs == nil {
			recs = []RunRecord{}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderHistory(recs))},
			StructuredContent: map[string]any{"runs": recs},
		}, nil
	})
	return nil
}

// stringArg extracts an optional-or-required string argument. A present
// non-string is rejected rather than coerced: silently stringifying an object
// into a request id would look up nothing and report "not found" for a call
// that was malformed.
func stringArg(args map[string]any, key string, required bool) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		if required {
			return "", NewProtocolError(CodeInvalidParams, key+" is required")
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", NewProtocolError(CodeInvalidParams, key+" must be a string")
	}
	if required && s == "" {
		return "", NewProtocolError(CodeInvalidParams, key+" is required")
	}
	return s, nil
}

// intArg extracts an optional-or-required integer argument. JSON numbers
// arrive as float64; a fractional value is rejected rather than truncated, so
// run_id 3.5 cannot silently become run 3.
func intArg(args map[string]any, key string, required bool) (int64, error) {
	v, ok := args[key]
	if !ok || v == nil {
		if required {
			return 0, NewProtocolError(CodeInvalidParams, key+" is required")
		}
		return 0, nil
	}
	switch t := v.(type) {
	case float64:
		if t != float64(int64(t)) {
			return 0, NewProtocolError(CodeInvalidParams, key+" must be an integer")
		}
		return int64(t), nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	default:
		return 0, NewProtocolError(CodeInvalidParams, key+" must be a number")
	}
}

// mapStoreError translates a store failure into its JSON-RPC shape. A miss is
// an invalid-params error naming the id, never an empty result: an empty
// result tells the agent "nothing ran", which is false — something ran, the
// agent just asked for the wrong id.
func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	return WrapProtocolError(CodeInvalidParams, "the store has no such entry", err)
}

func renderCollections(cols []CollectionInfo) string {
	if len(cols) == 0 {
		return "no collections\n"
	}
	out := ""
	for _, c := range cols {
		out += c.ID + " " + c.RootPath + " (" + c.CreatedAt + ")\n"
	}
	return out
}

func renderRequests(defs []RequestInfo) string {
	if len(defs) == 0 {
		return "no requests\n"
	}
	out := ""
	for _, d := range defs {
		name := d.Name
		if name == "" {
			name = d.ID
		}
		out += d.ID + " " + d.Method + " " + d.URLTemplate + " [" + name + "] (" + d.FilePath + ")\n"
	}
	return out
}

func renderRequest(d RequestInfo) string {
	return d.Method + " " + d.URLTemplate + "\nfile: " + d.FilePath + "\n"
}

func renderRun(r RunRecord) string {
	out := "run " + fmt.Sprint(r.ID) + " " + r.RequestID + " -> " + fmt.Sprint(r.StatusCode) + "\n"
	out += "started: " + r.StartedAt + " duration_ms: " + fmt.Sprint(r.DurationMS) + "\n"
	out += "bytes: " + fmt.Sprint(r.RequestBytes) + "/" + fmt.Sprint(r.ResponseBytes) + " sha256: " + r.SHA256 + "\n"
	if r.Error != "" {
		out += "error: " + singleLine(r.Error) + "\n"
	}
	return out
}

func renderHistory(recs []RunRecord) string {
	if len(recs) == 0 {
		return "no runs\n"
	}
	out := ""
	for _, r := range recs {
		out += "run " + fmt.Sprint(r.ID) + " " + r.RequestID + " -> " + fmt.Sprint(r.StatusCode)
		if r.Error != "" {
			out += " error: " + singleLine(r.Error)
		}
		out += " (" + r.StartedAt + ")\n"
	}
	return out
}

// formatTime renders a timestamp for agent display. UTC with second precision:
// an agent comparing two runs wants ordering, not monotonic nanoseconds.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
