package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// keyMsg builds a key press for the given canonical name.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func updateKeys(t *testing.T, m *Model, keys ...string) *Model {
	t.Helper()
	for _, k := range keys {
		updated, _ := m.Update(keyMsg(k))
		m = updated.(*Model)
	}
	return m
}

// TestTreeBrowsesAndSelects proves the left pane: rows snapshot from the
// document, j/k move the cursor, enter makes the row the run target.
func TestTreeBrowsesAndSelects(t *testing.T) {
	doc := parseDoc(t, "GET http://127.0.0.1:9/one HTTP/1.1\n\n###\nPOST http://127.0.0.1:9/two HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)

	if m.tree.Len() != 2 {
		t.Fatalf("tree holds %d rows, want 2", m.tree.Len())
	}
	if m.method != "GET" {
		t.Errorf("initial request = %s, want GET", m.method)
	}
	m = updateKeys(t, m, "j", "enter")
	if m.method != "POST" {
		t.Errorf("after j+enter request = %s, want POST", m.method)
	}
	if got := m.Toast(); !strings.Contains(got, "POST") {
		t.Errorf("selection toast = %q, want the new target named", got)
	}
	m = updateKeys(t, m, "k", "enter")
	if m.method != "GET" {
		t.Errorf("after k+enter request = %s, want GET", m.method)
	}
}

// TestSearchFindsAndJumps proves / search: type a query, enter runs it
// against the stream tail, ctrl+n cycles hits and scrolls to them.
func TestSearchFindsAndJumps(t *testing.T) {
	doc := parseDoc(t, "GET http://127.0.0.1:9/none HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*Model)
	m.stream.Append([]byte("alpha one\nbeta two\nalpha three\n"))
	m.stream.Flush()

	m = updateKeys(t, m, "/", "a", "l", "p", "h", "a", "enter")
	if m.search.Count() != 2 {
		t.Fatalf("search hits = %d, want 2", m.search.Count())
	}
	if m.search.Active() {
		t.Error("enter must close search input")
	}
	m = updateKeys(t, m, "ctrl+n")
	hit, ok := m.search.Current()
	if !ok || hit.Line != 2 {
		t.Errorf("n moved to %+v, want line 2", hit)
	}
	if out := m.View(); !strings.Contains(out, "/alpha (2)") {
		t.Errorf("status bar names neither query nor count: %q", lastLine(out))
	}
}

// TestPaletteFiltersAndDispatches proves Ctrl+P: filter narrows, enter on a
// request selects it, :command rows run verbs.
func TestPaletteFiltersAndDispatches(t *testing.T) {
	doc := parseDoc(t, "GET http://127.0.0.1:9/one HTTP/1.1\n\n###\nPOST http://127.0.0.1:9/two HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)

	m = updateKeys(t, m, "ctrl+p")
	if !m.palette.Active() {
		t.Fatal("palette did not open")
	}
	if got := len(m.palette.Visible()); got != 7 {
		t.Fatalf("palette shows %d rows, want 2 requests + 5 commands", got)
	}
	m = updateKeys(t, m, "t", "w", "o")
	vis := m.palette.Visible()
	if len(vis) != 1 || !strings.Contains(vis[0], "POST") {
		t.Errorf("filter 'two' shows %v, want only the POST row", vis)
	}
	m = updateKeys(t, m, "enter")
	if m.palette.Active() {
		t.Error("enter must close the palette")
	}
	if m.method != "POST" {
		t.Errorf("palette selection = %s, want POST", m.method)
	}

	m = updateKeys(t, m, "ctrl+p", ":", "f", "u", "z", "z", "enter")
	if !m.showFuzz {
		t.Error(":fuzz did not open the fuzz tab")
	}
}

// TestEditorSubmitsAndDispatches proves form mode end to end: i opens the
// editor seeded from the current request, typing replaces the URL, enter
// re-parses, rebuilds the runner through the factory, and dispatches — the
// test server counts exactly one hit on the NEW target, and a fixed runner
// (no factory) fails closed with zero dispatch.
func TestEditorSubmitsAndDispatches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	doc := parseDoc(t, "GET http://127.0.0.1:9/old HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	m.SetRunnerFactory(func(d *ast.Document) Runner { return engine.New(d) })

	sent := make(chan tea.Msg, 64)
	m.Bind(func(msg tea.Msg) { sent <- msg })

	m = updateKeys(t, m, "e")
	if !m.editor.Active() {
		t.Fatal("e did not open the editor")
	}
	// The URL field arrives seeded; clear it, then type the new target.
	for i := 0; i < 40; i++ {
		m = updateKeys(t, m, "backspace")
	}
	for _, r := range srv.URL + "/edited" {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(*Model)
	}
	updated, cmd := m.Update(keyMsg("enter"))
	m = updated.(*Model)
	if m.editor.Active() {
		t.Fatal("enter did not submit the editor")
	}
	if cmd == nil {
		t.Fatalf("submit held the edit: %q", m.Toast())
	}
	if msg := cmd(); msg == nil {
		t.Fatal("start command produced no message")
	}
	// The engine runs async; the terminal message arrives on sent.
	timeout := time.After(10 * time.Second)
	for m.phase != phaseDone && m.phase != phaseFailed {
		select {
		case msg := <-sent:
			updated, _ := m.Update(msg)
			m = updated.(*Model)
		case <-timeout:
			t.Fatalf("edited run never terminated (phase=%s)", m.phase)
		}
	}
	if m.phase != phaseDone {
		t.Fatalf("edited run failed: %v", m.err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want exactly 1 on the edited target", hits)
	}
	if !strings.Contains(m.url, "/edited") {
		t.Errorf("model URL = %q, want the edited target", m.url)
	}
}

// TestEditorWithoutFactoryFailsClosed proves the fixed-runner rule: with no
// factory the submit swaps nothing and dispatches nothing — a half-swapped
// document (new text, old runner) would slice the wrong arena.
func TestEditorWithoutFactoryFailsClosed(t *testing.T) {
	doc := parseDoc(t, "GET http://127.0.0.1:9/old HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	m.Bind(func(tea.Msg) {})
	m = updateKeys(t, m, "e", "enter")
	if m.editor.Active() {
		t.Error("submit must close the editor even when held")
	}
	if m.url != "http://127.0.0.1:9/old" {
		t.Errorf("fixed-runner submit swapped the request to %q", m.url)
	}
	if m.phase == phaseRunning {
		t.Error("fixed-runner submit dispatched a run")
	}
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
