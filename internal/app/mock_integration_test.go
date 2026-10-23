package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/app"
)

// TestExecutorCachesSuccessForMockReplay walks the whole path the feature
// exists for: an executed request whose 2xx response lands in SQLite (with its
// secret scrubbed), then a mock server built from that database replaying the
// exact body and headers with no upstream involved.
func TestExecutorCachesSuccessForMockReplay(t *testing.T) {
	const secret = "pulse-mock-cache-secret-a91c"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("upstream Authorization = %q, want the resolved secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Echo", secret)
		_, _ = fmt.Fprintf(w, `{"id":42,"echo":%q}`, secret)
	}))
	defer upstream.Close()

	home := t.TempDir()
	ctx := context.Background()
	a, err := app.New(ctx, app.Config{
		Home:              home,
		SkipKeychainProbe: true,
		SkipSync:          true,
		EnvFile:           writeEnvFile(t, map[string]string{"MOCK_TOKEN": secret}),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	payload := fmt.Sprintf("GET %s/users/42\nAuthorization: Bearer {{MOCK_TOKEN}}\n", upstream.URL)
	out, err := a.Executor.Execute(ctx, payload, "<inline>", 0, 5*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.Body, secret) {
		t.Fatalf("the relayed outcome leaked the secret: %s", out.Body)
	}

	row, err := a.Store.LatestMockResponse(ctx, http.MethodGet, "/users/{*}")
	if err != nil {
		t.Fatalf("LatestMockResponse: %v", err)
	}
	if strings.Contains(row.Replay, secret) {
		t.Errorf("the cached body holds the raw secret: %s", row.Replay)
	}
	if !strings.Contains(row.Replay, `"id":42`) {
		t.Errorf("cached body = %q, want the response payload", row.Replay)
	}
	if got := row.Header["X-Echo"]; len(got) == 0 || strings.Contains(got[0], secret) {
		t.Errorf("cached X-Echo = %v, want the redacted value", got)
	}

	// The raw database files are the real assertion: a replay cache that
	// stores secrets is a secret on disk, whatever the API returns.
	for _, path := range []string{a.DBPath, a.DBPath + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("%s contains the secret verbatim", filepath.Base(path))
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close the app: %v", err)
	}

	collection := filepath.Join(t.TempDir(), "api.http")
	if err := os.WriteFile(collection, []byte("# @name get-user\nGET {{base}}/users/42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mock, err := app.OpenMock(ctx, app.Config{Home: home}, []string{collection})
	if err != nil {
		t.Fatalf("OpenMock: %v", err)
	}
	defer func() { _ = mock.Close() }()
	ts := httptest.NewServer(mock.Server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/users/42")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"id":42`) {
		t.Errorf("replayed body = %q, want the cached payload", body)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("replayed body leaked the secret: %s", body)
	}
	if resp.Header.Get("X-Pulse-Mock") != "hit" {
		t.Errorf("replay is missing the X-Pulse-Mock marker")
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want the cached value", resp.Header.Get("Content-Type"))
	}
}

// TestTestRunCachesSuccessForMockReplay covers the second capture path: the
// command developers already run (`pulse test`) is what makes --from-cache
// useful, so it must feed the cache without a separate step.
func TestTestRunCachesSuccessForMockReplay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	collection := filepath.Join(dir, "api.http")
	src := fmt.Sprintf("# @name health\n# @assert status == 200\nGET %s/health\n", upstream.URL)
	if err := os.WriteFile(collection, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	res, err := a.RunTest(context.Background(), dir, 1, 5*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.Report.Failed != 0 {
		t.Fatalf("test run failed: %+v", res.Report.Entries)
	}

	ctx := context.Background()
	row, err := a.Store.LatestMockResponse(ctx, http.MethodGet, "/health")
	if err != nil {
		t.Fatalf("the test run did not populate the replay cache: %v", err)
	}
	if row.Replay != "hello from upstream" {
		t.Errorf("cached body = %q, want the live response", row.Replay)
	}
	if row.StatusCode != 200 {
		t.Errorf("cached status = %d, want 200", row.StatusCode)
	}

	mock, err := app.OpenMock(ctx, app.Config{DBPath: a.DBPath}, []string{collection})
	if err != nil {
		t.Fatalf("OpenMock: %v", err)
	}
	defer func() { _ = mock.Close() }()
	ts := httptest.NewServer(mock.Server)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello from upstream" {
		t.Errorf("replay = %d %q, want the cached 200 body", resp.StatusCode, body)
	}
}
