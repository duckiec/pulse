package ast

import (
	"strings"
	"testing"
)

func TestTokenizeCurl(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"simple", `curl https://x.local/a`, []string{"curl", "https://x.local/a"}},
		{"single", `curl -H 'A: b c' https://x.local/`, []string{"curl", "-H", "A: b c", "https://x.local/"}},
		{"double-escape", `curl -d "{\"a\": 1}" https://x.local/`, []string{"curl", "-d", `{"a": 1}`, "https://x.local/"}},
		{"continuation", "curl \\\n  https://x.local/", []string{"curl", "https://x.local/"}},
		{"ansi-c", `curl --data-binary $'a\nb' https://x.local/`, []string{"curl", "--data-binary", "a\nb", "https://x.local/"}},
		{"empty-quotes", `curl -H '' https://x.local/`, []string{"curl", "-H", "", "https://x.local/"}},
		{"equals-form", `curl --header='X: 1' https://x.local/`, []string{"curl", "--header=X: 1", "https://x.local/"}},
	}
	for _, c := range cases {
		got, err := tokenizeCurl(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: word %d = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
	if _, err := tokenizeCurl(`curl -H 'abc`); err == nil {
		t.Error("unterminated single quote accepted, want error")
	}
	if _, err := tokenizeCurl(`curl -d "abc`); err == nil {
		t.Error("unterminated double quote accepted, want error")
	}
}

// TestImportChromeStyle pastes a realistic DevTools "Copy as cURL" command:
// multiline continuations, compressed/location flags, single-quoted headers,
// and a bearer secret that must not survive in the request.
func TestImportChromeStyle(t *testing.T) {
	cmd := "curl 'https://api.example.com/v1/users?page=2' \\\n" +
		"  -H 'Accept: application/json' \\\n" +
		"  -H 'Authorization: Bearer sk-live-0123456789abcdef' \\\n" +
		"  --compressed -L"
	req, secrets, err := ImportCurl(cmd)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if req.Method != "GET" {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.URL != "https://api.example.com/v1/users?page=2" {
		t.Errorf("url = %q", req.URL)
	}
	for _, h := range req.Headers {
		if strings.Contains(h.Value, "sk-live-0123456789abcdef") {
			t.Errorf("raw secret in header %q: %q", h.Name, h.Value)
		}
	}
	found := false
	for _, s := range secrets {
		if s.Placeholder == "BEARER_TOKEN" && s.Value == "sk-live-0123456789abcdef" && s.Source == "header:Authorization" {
			found = true
		}
		if strings.Contains(s.Value, "{{") {
			t.Errorf("placeholder leaked into secret value: %+v", s)
		}
	}
	if !found {
		t.Errorf("bearer secret not extracted: %+v", secrets)
	}
	var auth string
	for _, h := range req.Headers {
		if h.Name == "Authorization" {
			auth = h.Value
		}
	}
	if auth != "Bearer {{BEARER_TOKEN}}" {
		t.Errorf("auth header = %q, want placeholder", auth)
	}
}

// TestImportInfersPostAndJSON covers -d inference, --json content-type, -u
// basic synthesis, cookie extraction, and query-secret extraction.
func TestImportInfersPostAndJSON(t *testing.T) {
	cmd := `curl -u alice:hunter2 --json '{"a": 1}' -b 'sess=abc123' 'api.example.com/v1/t?api_key=ZZZZ'`
	req, secrets, err := ImportCurl(cmd)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if req.Method != "POST" {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL != "https://api.example.com/v1/t?api_key=ZZZZ" && !strings.HasPrefix(req.URL, "https://api.example.com/v1/t?") {
		t.Errorf("url = %q", req.URL)
	}
	byName := map[string]string{}
	for _, h := range req.Headers {
		byName[h.Name] = h.Value
		if h.Value == "alice:hunter2" || h.Value == "hunter2" || strings.Contains(h.Value, "sess=abc123") && h.Name == "Cookie" {
			t.Errorf("raw secret in header %q", h.Name)
		}
	}
	if byName["Content-Type"] != "application/json" {
		t.Errorf("content-type = %q", byName["Content-Type"])
	}
	if byName["Authorization"] != "Basic {{BASIC_AUTH}}" {
		t.Errorf("auth = %q", byName["Authorization"])
	}
	if byName["Cookie"] != "{{COOKIE}}" {
		t.Errorf("cookie = %q", byName["Cookie"])
	}
	if strings.Contains(req.URL, "ZZZZ") {
		t.Errorf("raw query secret in URL %q", req.URL)
	}
	want := map[string]string{"BASIC_AUTH": "alice:hunter2", "COOKIE": "sess=abc123", "QUERY_TOKEN": "ZZZZ"}
	for _, s := range secrets {
		if w, ok := want[s.Placeholder]; ok && w == s.Value {
			delete(want, s.Placeholder)
		}
	}
	for ph := range want {
		t.Errorf("secret %q not extracted: %+v", ph, secrets)
	}
}

// TestImportFailsClosed pins every rejection: unknown flags, @file bodies,
// missing values, missing/double URLs, non-curl commands.
func TestImportFailsClosed(t *testing.T) {
	cases := []struct{ name, cmd string }{
		{"unknown-long", "curl --http2 https://x.local/"},
		{"unknown-short", "curl -k https://x.local/"},
		{"at-file", "curl --data-binary @body.json https://x.local/"},
		{"dangling-X", "curl -X"},
		{"no-url", "curl -H 'A: b'"},
		{"two-urls", "curl https://a.local/ https://b.local/"},
		{"not-curl", "wget https://x.local/"},
		{"bad-header", "curl -H 'no-colon' https://x.local/"},
		{"bad-method", "curl -X FETCH https://x.local/"},
		{"empty", ""},
	}
	for _, c := range cases {
		if _, _, err := ImportCurl(c.cmd); err == nil {
			t.Errorf("%s: accepted, want error", c.name)
		}
	}
}

// TestImportDataUrlencode proves --data-urlencode escapes the value side.
func TestImportDataUrlencode(t *testing.T) {
	req, _, err := ImportCurl(`curl --data-urlencode 'q=a b&c' https://x.local/s`)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if req.Method != "POST" {
		t.Errorf("method = %q", req.Method)
	}
	if strings.Contains(req.Body, " ") {
		t.Errorf("unescaped body %q", req.Body)
	}
}
