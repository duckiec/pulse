package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/engine"
)

type fakeMockSource map[string]engine.MockResponse

func (f fakeMockSource) LatestMock(_ context.Context, method, route string) (engine.MockResponse, bool, error) {
	resp, ok := f[strings.ToUpper(method)+" "+route]
	return resp, ok, nil
}

const mockCollection = `# @name list-users
GET {{base}}/users/42

###
# @name add-pet
POST {{base}}/users/42/pets
Content-Type: application/json

{"name":"rex"}
`

func newMockFixture(t *testing.T) *engine.MockServer {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "api.http")
	if err := os.WriteFile(path, []byte(mockCollection), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := engine.NewMockServer(engine.MockConfig{
		Paths: []string{path},
		Source: fakeMockSource{
			"GET /users/{*}": {
				Status: 200,
				Header: http.Header{
					"Content-Type": {"application/json"},
					"X-Cached":     {"yes"},
				},
				Body: []byte(`{"id":42,"name":"ada"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMockServer: %v", err)
	}
	return srv
}

func TestMockReplaysCachedResponseForDynamicRoute(t *testing.T) {
	srv := newMockFixture(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, path := range []string{"/users/42", "/users/7"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
		}
		if string(body) != `{"id":42,"name":"ada"}` {
			t.Errorf("GET %s body = %q", path, body)
		}
		if resp.Header.Get("X-Pulse-Mock") != "hit" {
			t.Errorf("GET %s is missing the X-Pulse-Mock marker", path)
		}
		if resp.Header.Get("X-Cached") != "yes" {
			t.Errorf("GET %s did not replay the cached header set", path)
		}
	}

	// A query string must not change the endpoint match.
	resp, err := http.Get(ts.URL + "/users/42?expand=pets")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET with a query = %d, want 200 (query strings are ignored)", resp.StatusCode)
	}
}

func TestMockReportsUncachedRouteWithTheFix(t *testing.T) {
	srv := newMockFixture(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/users/42/pets", "application/json", strings.NewReader(`{"name":"rex"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("uncached POST = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode 404 body: %v", err)
	}
	if !strings.Contains(payload.Error, "POST /users/{*}") {
		t.Errorf("error = %q, want the unmatched route", payload.Error)
	}
	if !strings.Contains(payload.Hint, "pulse test") {
		t.Errorf("hint = %q, want it to name the command that populates the cache", payload.Hint)
	}
}

func TestMockRejectsUnknownRouteAndMethod(t *testing.T) {
	srv := newMockFixture(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no .http request matches") {
		t.Errorf("unknown route = %d %q, want a 404 naming the missing request", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/users/42", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

func TestMockBannerListsRoutes(t *testing.T) {
	srv := newMockFixture(t)
	banner := srv.Banner("127.0.0.1:8080")
	for _, want := range []string{
		"pulse mock --from-cache",
		"http://127.0.0.1:8080",
		"GET    /users/{*}",
		"list-users",
		"POST   /users/{*}/pets",
		"add-pet",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q:\n%s", want, banner)
		}
	}
}

func TestRoutePathNormalizesIdentifiers(t *testing.T) {
	cases := []struct {
		target string
		want   string
		ok     bool
	}{
		{"https://api.example.com/users/42", "/users/{*}", true},
		{"http://localhost:8080/users/42?x=1", "/users/{*}", true},
		{"{{base}}/users/{{id}}", "/users/{*}", true},
		{"{{base}}/v2/users", "/v2/users", true},
		{"https://api.example.com/users/550e8400-e29b-41d4-a716-446655440000", "/users/{*}", true},
		{"https://api.example.com/users/deadbeefdeadbeef", "/users/{*}", true},
		{"https://api.example.com/users/ada", "/users/ada", true},
		{"{{base}}/", "/", true},
		{"localhost:8080/users", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := engine.RoutePath(tc.target)
		if ok != tc.ok || got != tc.want {
			t.Errorf("RoutePath(%q) = %q, %v; want %q, %v", tc.target, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMockRefusesAnUnroutableRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.http")
	if err := os.WriteFile(path, []byte("GET localhost:8080/users\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewMockServer(engine.MockConfig{Paths: []string{path}}); err == nil {
		t.Fatal("NewMockServer accepted a target it cannot route")
	}
}
