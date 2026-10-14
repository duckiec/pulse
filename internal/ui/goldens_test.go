package ui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	tea "github.com/charmbracelet/bubbletea"
)

var updateGoldens = flag.Bool("update", false, "rewrite the UI golden fixtures")

// forceASCII pins the lipgloss profile for the duration of a golden: no
// terminal, no environment, no version-skewed color detection can change a
// byte. The color-token gate below covers the ANSI side separately.
func forceASCII(t *testing.T) {
	t.Helper()
	saved := termenv.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(saved) })
}

// goldenModel builds the fixture state: two requests, three stream lines,
// first request selected. Every dynamic strip (timing, telemetry) is empty
// by construction, so the render is a pure function of size and glyph mode.
func goldenModel(t *testing.T) *Model {
	t.Helper()
	doc := parseDoc(t, "GET http://127.0.0.1:9/one HTTP/1.1\n\n###\nPOST http://127.0.0.1:9/two HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	m.stream.Append([]byte("hello\nworld\n"))
	m.stream.Append([]byte("tail\n"))
	m.stream.Flush()
	return m
}

// TestGoldenBreakpoints pins the layout law across terminal sizes: below
// 80 columns a refusal in words, 80–99 the single pane, 100+ the dual pane
// with the tree. Byte-identical against fixtures; regenerate with -update.
func TestGoldenBreakpoints(t *testing.T) {
	forceASCII(t)
	cases := []struct {
		name   string
		width  int
		height int
		ascii  bool
	}{
		{"refusal-79", 79, 24, false},
		{"single-80", 80, 24, false},
		{"dual-100", 100, 30, false},
		{"dual-120", 120, 40, false},
		{"refusal-79-ascii", 79, 24, true},
		{"single-80-ascii", 80, 24, true},
		{"dual-100-ascii", 100, 30, true},
		{"dual-120-ascii", 120, 40, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := goldenModel(t)
			m.SetASCII(c.ascii)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: c.width, Height: c.height})
			m = updated.(*Model)
			got := m.View()
			assertGolden(t, c.name, got)

			if c.ascii {
				for i := 0; i < len(got); i++ {
					if got[i] >= 0x80 {
						t.Errorf("ascii render carries non-ASCII byte 0x%02x at offset %d", got[i], i)
						break
					}
				}
			}
		})
	}
}

// TestGoldenBreaksOnLayoutDrift is the structural companion: the refusal,
// single, and dual shapes must differ from each other, so a fixture
// comparison that passed because every size rendered the same string
// cannot hide here.
func TestGoldenBreaksOnLayoutDrift(t *testing.T) {
	forceASCII(t)
	render := func(w int) string {
		m := goldenModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		return updated.(*Model).View()
	}
	refusal, single, dual := render(79), render(80), render(120)
	if !strings.Contains(refusal, "too narrow") {
		t.Error("79 cols must refuse in words")
	}
	if strings.Contains(single, "too narrow") || !strings.Contains(single, "response") {
		t.Error("80 cols must render the single response pane")
	}
	if !strings.Contains(dual, "response") || !strings.Contains(dual, "GET") {
		t.Error("120 cols must render tree beside response")
	}
	if single == dual {
		t.Error("single and dual renders are identical; the breakpoint binds nothing")
	}
}

// TestColorTokensAreDistinct pins the palette (DESIGN_SYSTEM.md §4): the
// six text tokens render pairwise-distinct ANSI sequences under the 16-color
// profile, and the two border tokens differ from each other (the focus
// indicator). A border sharing its hue with a text token is fine — different
// visual channels — but two text states in one code would convey state by a
// color that collides. Monochrome safety comes from the words beside the
// colors, not from the codes; the codes just must not alias within a
// channel. This gate caught the first-draft palette red-handed (warn≡err at
// 91, muted≡accent at 94) and now pins the ladder-fixed values.
func TestColorTokensAreDistinct(t *testing.T) {
	saved := termenv.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(saved) })

	texts := map[string]string{
		"header": styleHeader.Render("x"),
		"muted":  styleMuted.Render("x"),
		"accent": styleAccent.Render("x"),
		"ok":     styleOK.Render("x"),
		"err":    styleErr.Render("x"),
		"warn":   styleWarn.Render("x"),
	}
	seen := map[string]string{}
	for name, out := range texts {
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("token %q emits no ANSI under the ANSI profile", name)
		}
		for other, prev := range seen {
			if out == prev {
				t.Errorf("color tokens %q and %q render identically: %q", name, other, out)
			}
		}
		seen[name] = out
	}
	if pane, focused := stylePane.Render("x"), styleFocused.Render("x"); pane == focused {
		t.Error("pane and focused borders render identically: exactly one focused pane must be visible")
	}
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s unreadable (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("golden %s drifted (%d vs %d bytes); diff with -update output and justify, or fix the layout",
			path, len(got), len(want))
	}
}
