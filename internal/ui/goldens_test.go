package ui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/engine"
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

// TestGoldenBreakpoints pins the workbench at the three reference terminal
// sizes the redesign is specified against (80x24, 120x36, 160x50), plus the
// refusal floor and the ASCII ladder. Byte-identical against fixtures;
// regenerate with -update.
func TestGoldenBreakpoints(t *testing.T) {
	forceASCII(t)
	cases := []struct {
		name   string
		width  int
		height int
		ascii  bool
	}{
		{"refusal-79", 79, 24, false},
		{"workbench-80", 80, 24, false},
		{"workbench-120", 120, 36, false},
		{"workbench-160", 160, 50, false},
		{"refusal-79-ascii", 79, 24, true},
		{"workbench-80-ascii", 80, 24, true},
		{"workbench-120-ascii", 120, 36, true},
		{"workbench-160-ascii", 160, 50, true},
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

// TestGoldenResponseState pins the finished-run surface: a request pane with
// asserts, headers and a body, a response pane with its telemetry header, and
// a JSON body in the viewport. The idle goldens cover the boot state; this is
// the state the workbench exists for.
func TestGoldenResponseState(t *testing.T) {
	forceASCII(t)
	doc := parseDoc(t, "# @name create-note\n# @assert status == 201\nPOST http://127.0.0.1:9/notes HTTP/1.1\nContent-Type: application/json\nAuthorization: Bearer {{API_TOKEN}}\n\n{\"title\": \"hello\"}\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	m.stream.Append([]byte("{\n  \"id\": 7,\n  \"ok\": true,\n  \"tags\": [\"a\", \"b\"]\n}\n"))
	m.stream.Flush()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	m = updated.(*Model)
	m.phase = phaseDone
	m.res = &engine.RunResult{
		Proto: "HTTP/1.1", Status: 201, StatusText: "201 Created",
		Bytes: 64, Elapsed: 12300 * time.Microsecond,
	}
	out := m.View()
	assertGolden(t, "response-120", out)
	assertExactFrame(t, 120, 36, out, "response")
}

// TestFrameGeometryIsExact is the border-clipping law: for every supported
// size and every surface (workbench, help, palette, composer, fuzz), the
// render is exactly height rows of exactly width display cells. A single row
// one cell short shifts every border on the screen; this is the test that
// catches a pane whose padding and whose frame disagree.
func TestFrameGeometryIsExact(t *testing.T) {
	forceASCII(t)
	sizes := [][2]int{
		{80, 24}, {81, 24}, {99, 30}, {100, 24}, {120, 36},
		{133, 41}, {160, 50}, {200, 60},
	}
	states := []struct {
		name  string
		apply func(*Model)
	}{
		{"workbench", func(*Model) {}},
		{"help", func(m *Model) { m.showHelp = true }},
		{"palette", func(m *Model) { m.palette.Activate(m.paletteItems()) }},
		{"composer", func(m *Model) { m.openComposer() }},
		{"fuzz", func(m *Model) { m.showFuzz = true }},
	}
	for _, size := range sizes {
		for _, state := range states {
			m := goldenModel(t)
			m.width, m.height = size[0], size[1]
			state.apply(m)
			assertExactFrame(t, size[0], size[1], m.View(), state.name)
		}
	}
}

// TestFrameGeometrySurvivesLiveRun checks the same law with telemetry and a
// response badge present, which is the widest the header ever gets.
func TestFrameGeometrySurvivesLiveRun(t *testing.T) {
	forceASCII(t)
	m := goldenModel(t)
	m.width, m.height = 80, 24
	m.phase = phaseDone
	m.res = &engine.RunResult{
		Proto: "HTTP/1.1", Status: 200, StatusText: "200 OK",
		Bytes: 2048, Elapsed: 1500 * time.Microsecond,
	}
	m.err = nil
	assertExactFrame(t, 80, 24, m.View(), "done")
}

// assertExactFrame fails with the first offending row, quoting it, because
// "one of eighty rows is 79 cells" is otherwise an impossible bug to find.
func assertExactFrame(t *testing.T, w, h int, out, state string) {
	t.Helper()
	rows := strings.Split(out, "\n")
	if len(rows) != h {
		t.Fatalf("%s at %dx%d: %d rows, want %d", state, w, h, len(rows), h)
	}
	for i, row := range rows {
		if got := lipgloss.Width(row); got != w {
			t.Fatalf("%s at %dx%d: row %d is %d cells, want %d\n%q",
				state, w, h, i, got, w, row)
		}
	}
}

// TestGoldenBreaksOnLayoutDrift is the structural companion: the refusal and
// the workbench must differ, both workbench sizes must carry every named
// pane, and the sizes must actually differ from each other — a fixture
// comparison that passed because every size rendered the same string cannot
// hide here.
func TestGoldenBreaksOnLayoutDrift(t *testing.T) {
	forceASCII(t)
	render := func(w, h int) string {
		m := goldenModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		return updated.(*Model).View()
	}
	refusal, small, large := render(79, 24), render(80, 24), render(160, 50)
	if !strings.Contains(refusal, "too narrow") {
		t.Error("79 cols must refuse in words")
	}
	for _, want := range []string{"Requests (2)", "Request", "Response", "GET", "NORMAL"} {
		if !strings.Contains(small, want) {
			t.Errorf("80x24 workbench omits %q", want)
		}
		if !strings.Contains(large, want) {
			t.Errorf("160x50 workbench omits %q", want)
		}
	}
	if small == large {
		t.Error("80x24 and 160x50 renders are identical; the layout ignores its size")
	}
	if strings.Contains(small, "welcome to pulse") {
		t.Error("the welcome wall is back; the workbench must boot directly")
	}
}

// TestColorTokensAreDistinct pins the palette (DESIGN_SYSTEM.md §4): the six
// text tokens render pairwise-distinct ANSI sequences under the 16-color
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
	if idle, active := styleBorderIdle.Render("x"), styleBorderActive.Render("x"); idle == active {
		t.Error("idle and active borders render identically: exactly one focused pane must be visible")
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
