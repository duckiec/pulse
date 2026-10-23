package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pulse/internal/ast"
)

// The air-gap replay server: `pulse mock <path> --from-cache`.
//
// # What it is
//
// A route table derived from the collection's own .http files, backed by the
// SQLite replay cache. A request that matches a defined request is answered
// with the most recent 2xx response Pulse recorded for that route; anything
// else is a 404 that says why. There is no proxying, no upstream, and no
// network access of any kind — the point is to develop against the last known
// good responses on a plane.
//
// # Why the route key is a path pattern, not a URL
//
// Recorded responses are keyed by `GET /users/{*}` rather than by
// `http://localhost:8080/users/42`: the host is an environment detail that
// changes between the run that populated the cache and the run that replays it,
// and a literal cache key would turn every port change into a cache miss.
// Dynamic segments are recognised structurally (an interpolation anchor) or by
// shape (numeric, UUID, long hex), which covers generated identifiers without a
// guessing game. Query strings are deliberately ignored: two requests that
// differ only by query share an endpoint, and the cache promises an endpoint's
// latest success.
//
// # Why a missing cache entry is not a 500
//
// "You have not run this yet" is the normal state of a fresh checkout. The
// server answers 404 with the exact command that fixes it, and never invents a
// response — a mock that silently served `{}` would let a client ship against a
// fiction.

// MockResponse is one cached success ready to replay.
type MockResponse struct {
	// Status is the original status code (always 2xx).
	Status int
	// Header holds the redacted response headers.
	Header http.Header
	// Body is the exact response payload.
	Body []byte
}

// MockSample is one successful response offered to the replay cache.
//
// Header is already the redacted copy from RunResult, and Body has passed
// through the redactor, so a sink never has to decide what is safe to persist:
// the capture side made that decision before the sample existed.
type MockSample struct {
	Method string
	URL    string
	Status int
	Header http.Header
	Body   string
}

// MockSource resolves the latest cached success for a route. internal/app
// implements it over internal/store; the engine declares it so the replay
// server can be exercised without a database (AGENTS.md §3.1).
type MockSource interface {
	// LatestMock returns the cached response, or ok=false when the route has
	// never produced one. An error means the cache itself could not be read.
	LatestMock(ctx context.Context, method, route string) (resp MockResponse, ok bool, err error)
}

// MockRoute is one defined request the replay server answers.
type MockRoute struct {
	// Method is the uppercase HTTP method.
	Method string
	// Route is the canonical path pattern, e.g. "/users/{*}".
	Route string
	// Name is the request's `# @name`, or "" when it has none.
	Name string
	// Target is the request target as written in the file, anchors intact.
	Target string
	// File is the .http file that defines the request.
	File string
	// Line is the request line in that file.
	Line int32
}

// Key is the (method, route) identity used for the cache and the router.
func (r MockRoute) Key() string { return mockKey(r.Method, r.Route) }

// mockKey is the canonical lookup key. Methods are uppercased because a .http
// file may spell `get` while net/http reports `GET`.
func mockKey(method, route string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + normalizePath(route)
}

// MockConfig configures a replay server.
type MockConfig struct {
	// Paths is a .http file or a directory of them, as given on the command
	// line. A directory is walked recursively for *.http.
	Paths []string
	// Source is the cache. Nil serves the route table and answers 404 for
	// every request, which is useful for checking that the routes loaded.
	Source MockSource
	// Logf receives non-fatal warnings (duplicate routes). Nil is silent.
	Logf func(format string, args ...any)
}

// MockServer is an http.Handler that replays cached responses.
type MockServer struct {
	routes   map[string]MockRoute
	keys     []string
	source   MockSource
	logf     func(format string, args ...any)
	warnings []string
}

// NewMockServer parses every path and builds the route table.
//
// A file that does not parse, or a request whose target yields no path, stops
// construction: serving a partial route table would make a typo look like a
// missing cache entry, and the user would be told to run a request that does not
// exist.
func NewMockServer(cfg MockConfig) (*MockServer, error) {
	files, err := mockFiles(cfg.Paths)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &MockServer{
		routes: make(map[string]MockRoute),
		source: cfg.Source,
		logf:   logf,
	}
	for _, file := range files {
		doc, err := ast.ParseFile(file)
		if err != nil {
			return nil, err
		}
		for req := range doc.Requests() {
			target := strings.TrimSpace(doc.Str(req.URLStart, req.URLEnd))
			route, ok := RoutePath(target)
			if !ok {
				doc.Release()
				return nil, fmt.Errorf("mock: %s:%d: cannot determine a path from target %q; "+
					"use an absolute URL or a path beginning with /", file, req.Line, target)
			}
			r := MockRoute{
				Method: req.Method.String(),
				Route:  route,
				Name:   strings.TrimSpace(doc.Str(req.NameStart, req.NameEnd)),
				Target: target,
				File:   file,
				Line:   req.Line,
			}
			if prev, exists := m.routes[r.Key()]; exists {
				warning := fmt.Sprintf("mock: %s:%d repeats %s %s from %s:%d; the later definition wins",
					r.File, r.Line, r.Method, r.Route, prev.File, prev.Line)
				m.warnings = append(m.warnings, warning)
				logf("%s", warning)
			}
			m.routes[r.Key()] = r
		}
		doc.Release()
	}
	if len(m.routes) == 0 {
		return nil, fmt.Errorf("%w: no requests found in %s", ErrNoFiles, strings.Join(cfg.Paths, ", "))
	}
	m.keys = make([]string, 0, len(m.routes))
	for key := range m.routes {
		m.keys = append(m.keys, key)
	}
	sort.Strings(m.keys)
	return m, nil
}

// Warnings reports duplicate-route warnings raised while loading.
func (m *MockServer) Warnings() []string { return append([]string(nil), m.warnings...) }

// Routes returns the route table in deterministic order.
func (m *MockServer) Routes() []MockRoute {
	out := make([]MockRoute, 0, len(m.keys))
	for _, key := range m.keys {
		out = append(out, m.routes[key])
	}
	return out
}

// Banner renders the startup summary: the address and every route, with the
// file and line that defined it so a surprise is one `grep` away.
func (m *MockServer) Banner(addr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pulse mock --from-cache\n")
	fmt.Fprintf(&b, "  listening on http://%s\n", addr)
	fmt.Fprintf(&b, "  replaying %d route(s) from the local cache:\n", len(m.keys))
	for _, key := range m.keys {
		r := m.routes[key]
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "    %-6s %-30s %-20s %s:%d\n", r.Method, r.Route, name, r.File, r.Line)
	}
	fmt.Fprintf(&b, "  query strings are ignored when matching; only 2xx responses are replayed.\n")
	return b.String()
}

// ServeHTTP implements http.Handler.
func (m *MockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := m.lookup(r.Method, r.URL.Path)
	if !ok {
		if others := m.methodsFor(r.URL.Path); len(others) > 0 {
			w.Header().Set("Allow", strings.Join(others, ", "))
			writeMockError(w, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not defined for %s", strings.ToUpper(r.Method), normalizePath(r.URL.Path)),
				fmt.Sprintf("the collection defines %s for this path", strings.Join(others, ", ")))
			return
		}
		writeMockError(w, http.StatusNotFound,
			fmt.Sprintf("no .http request matches %s %s", strings.ToUpper(r.Method), normalizePath(r.URL.Path)),
			"add the request to the collection passed to `pulse mock`")
		return
	}
	if m.source == nil {
		writeMockError(w, http.StatusNotFound,
			fmt.Sprintf("no cached response for %s %s", route.Method, route.Route),
			"no replay cache is attached; run `pulse mock` through the Pulse CLI")
		return
	}
	resp, found, err := m.source.LatestMock(r.Context(), route.Method, route.Route)
	if err != nil {
		m.logf("mock: read cache for %s %s: %v", route.Method, route.Route, err)
		writeMockError(w, http.StatusInternalServerError,
			fmt.Sprintf("the cached response for %s %s could not be read", route.Method, route.Route),
			"the local store reported an error; see the terminal for details")
		return
	}
	if !found {
		writeMockError(w, http.StatusNotFound,
			fmt.Sprintf("%s %s has not been cached yet", route.Method, route.Route),
			"execute it once with `pulse test` or the MCP server, then retry — pulse mock only replays 2xx responses")
		return
	}

	for k, vals := range resp.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Pulse-Mock", "hit")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	status := resp.Status
	if status < 200 || status > 299 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(resp.Body)
	}
}

// lookup finds the route for a concrete method and path. An exact key wins;
// templates are scanned in sorted order so the same request always matches the
// same definition.
func (m *MockServer) lookup(method, path string) (MockRoute, bool) {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = normalizePath(path)
	if r, ok := m.routes[mockKey(method, path)]; ok {
		return r, true
	}
	for _, key := range m.keys {
		r := m.routes[key]
		if r.Method == method && matchPath(r.Route, path) {
			return r, true
		}
	}
	return MockRoute{}, false
}

// methodsFor lists the methods defined for a path, for a 405 response.
func (m *MockServer) methodsFor(path string) []string {
	path = normalizePath(path)
	var out []string
	seen := map[string]bool{}
	for _, key := range m.keys {
		r := m.routes[key]
		if seen[r.Method] || !matchPath(r.Route, path) {
			continue
		}
		seen[r.Method] = true
		out = append(out, r.Method)
	}
	sort.Strings(out)
	return out
}

// RoutePath reduces a request target to the canonical route key.
//
// Absolute URLs contribute their path, an unresolved anchor is dropped up to
// the following slash, and a bare `/path` is used as is. Segments that are
// interpolation anchors or look like generated identifiers become `{*}`.
// Anything else cannot be routed, and says so rather than guessing.
func RoutePath(target string) (string, bool) {
	path, _, ok := targetPath(target)
	if !ok {
		return "", false
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, seg := range segs {
		if isDynamicSegment(seg) || looksLikeIdentifier(seg) {
			segs[i] = "{*}"
		}
	}
	return normalizePath("/" + strings.Join(segs, "/")), true
}

// looksLikeIdentifier reports whether one path segment is a generated value
// rather than a literal resource name. The shapes are deliberately narrow —
// digits, UUIDs, and long hexadecimal runs — because widening this is how a
// literal `/v2` or a hash-named static asset would silently collapse two
// different routes into one cache entry.
func looksLikeIdentifier(seg string) bool {
	if seg == "" {
		return false
	}
	digits, hex := true, true
	for _, r := range seg {
		if r < '0' || r > '9' {
			digits = false
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			hex = false
		}
	}
	if digits {
		return true
	}
	if len(seg) == 36 && isUUID(seg) {
		return true
	}
	return hex && len(seg) >= 16
}

func isUUID(s string) bool {
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// mockFiles expands the command-line paths into the .http files to parse.
func mockFiles(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: no .http file or directory was given", ErrNoFiles)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("mock: %w", err)
		}
		if !info.IsDir() {
			abs := filepath.Clean(p)
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
			continue
		}
		files, err := Files(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			abs := filepath.Clean(f)
			if seen[abs] {
				continue
			}
			seen[abs] = true
			out = append(out, abs)
		}
	}
	sort.Strings(out)
	return out, nil
}

// writeMockError renders the one error shape this server returns: a JSON
// object with what failed and what to do about it. Clients get a parseable
// body instead of net/http's plain text.
func writeMockError(w http.ResponseWriter, status int, msg, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
		Hint  string `json:"hint,omitempty"`
	}{Error: msg, Hint: hint})
}

// isHopByHopHeader reports whether a header is meaningful only for one
// connection and must not be replayed to a different client.
func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade", "proxy-authenticate":
		return true
	}
	return false
}
