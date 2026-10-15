package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
)

// EditorModel is the request composer: name, method, URL, a headers list,
// and a multi-line body, opened with e and submitted with enter (apply +
// send) or persisted with ctrl+s (save to disk, no send).
//
// huh is deliberately NOT the implementation: it is absent from go.mod and
// the allowlist by prior decision (TASKS.md P3.2.2), and admitting a new
// dependency for form fields would trade a fetch, an allowlist entry, and
// a new import edge for nothing bubbles/textinput+textarea — already
// vendored and sanctioned — does not do. Recorded in ADL-0040.
//
// Focus order is 0 name, 1 method, 2 url, 3 headers list, 4 body. Focus 1
// (method) treats Space as "next method" rather than input — methods never
// contain spaces, so no value is untypeable. The headers list has two
// sub-modes: list-nav (a add, e edit, d delete, Space disable, j/k move)
// and line-edit (a single textinput committing one "Name: value" row).
type EditorModel struct {
	active bool
	name   textinput.Model
	method textinput.Model
	url    textinput.Model

	headers   []headerRow
	hdrCursor int
	hdrEdit   textinput.Model
	hdrEditOn bool
	// hdrEditIndex names the row HeaderEdit opened, or -1 for a fresh
	// row from HeaderAdd. Comparing values to tell the two apart would
	// misfire the moment a fresh row typed the same text as a neighbor.
	hdrEditIndex int

	body  textarea.Model
	focus int

	// preserve carries the lines the composer does not model — in-block
	// @var definitions, # @assert directives, @wasm-sign — captured at
	// open and prepended verbatim in Source/BuildBlock. Without it, saving
	// an edited request would silently drop its assertions, which is data
	// loss wearing an edit's clothes.
	preserve string
}

// headerRow is one headers-list row: text holds "Name: value" without any
// disabled marker; disabled renders (and emits) with a "# " prefix.
type headerRow struct {
	disabled bool
	text     string
}

// composerMethods is the Space-cycle order. It mirrors the lexer's seven;
// an unknown method stays as typed text and fails closed at parse.
var composerMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// NewEditorModel returns a closed composer with blank fields.
func NewEditorModel() EditorModel {
	mk := func(ph string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		ti.Prompt = ""
		return ti
	}
	ta := textarea.New()
	ta.Placeholder = "{...}"
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 1 << 20
	return EditorModel{
		name:   mk("untitled"),
		method: mk("GET"),
		url:    mk("https://..."),
		hdrEdit: func() textinput.Model {
			ti := mk("Name: value")
			return ti
		}(),
		body: ta,
	}
}

// Open shows the composer seeded from the current request, focusing field
// focus (0 name, 1 method, 2 url, 3 headers, 4 body). Headers, body, name
// and preserved lines reset to empty: stale rows from a previous open
// would otherwise attach to the wrong request.
func (e *EditorModel) Open(method, url string, focus int) {
	e.OpenFull("", method, url, nil, "", "", focus)
}

// OpenFull seeds every field, including the header rows, body text, and
// the preserved var/assert/signer prefix captured from the document.
func (e *EditorModel) OpenFull(name, method, url string, headers []ast.PlainHeader, body, preserve string, focus int) {
	e.active = true
	e.name.SetValue(name)
	e.method.SetValue(method)
	e.url.SetValue(url)
	e.headers = e.headers[:0]
	e.hdrCursor = 0
	for _, h := range headers {
		text := h.Name + ": " + h.Value
		if h.Value == "" {
			text = h.Name + ":"
		}
		e.headers = append(e.headers, headerRow{disabled: h.Disabled, text: text})
	}
	e.hdrEditOn = false
	e.hdrEditIndex = -1
	e.hdrEdit.SetValue("")
	e.body.SetValue(body)
	e.preserve = preserve
	e.focus = focus
	if e.focus < 0 || e.focus > 4 {
		e.focus = 2
	}
	e.syncFocus()
}

// Close hides the composer without applying.
func (e *EditorModel) Close() {
	e.active = false
	e.name.Blur()
	e.method.Blur()
	e.url.Blur()
	e.hdrEdit.Blur()
	e.body.Blur()
	e.hdrEditOn = false
	e.hdrEditIndex = -1
}

// Active reports visibility.
func (e EditorModel) Active() bool { return e.active }

// Focus reports the focused field index (0 name, 1 method, 2 url,
// 3 headers, 4 body).
func (e EditorModel) Focus() int { return e.focus }

// HeaderNav reports whether the headers list (not its line editor) holds
// focus: the only state where single-letter row ops are live.
func (e EditorModel) HeaderNav() bool { return e.active && e.focus == 3 && !e.hdrEditOn }

// LineEditing reports whether a header row is being typed.
func (e EditorModel) LineEditing() bool { return e.active && e.hdrEditOn }

// BodyFocused reports whether the body textarea holds focus.
func (e EditorModel) BodyFocused() bool { return e.active && e.focus == 4 }

// Cycle moves field focus.
func (e *EditorModel) Cycle() {
	if e.hdrEditOn {
		e.hdrEditOn = false
	}
	e.focus = (e.focus + 1) % 5
	e.syncFocus()
}

// CycleMethod advances the method field to the next standard method,
// wrapping. Unknown text restarts at GET rather than being preserved:
// the cycle is a picker, and free text stays free-typed.
func (e *EditorModel) CycleMethod() {
	cur := strings.ToUpper(strings.TrimSpace(e.method.Value()))
	next := composerMethods[0]
	for i, m := range composerMethods {
		if m == cur {
			next = composerMethods[(i+1)%len(composerMethods)]
			break
		}
	}
	e.method.SetValue(next)
}

func (e *EditorModel) syncFocus() {
	e.name.Blur()
	e.method.Blur()
	e.url.Blur()
	e.hdrEdit.Blur()
	e.body.Blur()
	if !e.active {
		return
	}
	switch {
	case e.hdrEditOn:
		e.hdrEdit.Focus()
	case e.focus == 0:
		e.name.Focus()
	case e.focus == 1:
		e.method.Focus()
	case e.focus == 2:
		e.url.Focus()
	case e.focus == 3:
		// List-nav: no text caret anywhere; keystrokes are row ops.
	case e.focus == 4:
		e.body.Focus()
	}
}

// HeaderMove shifts the row cursor, clamped. A no-row list holds still.
func (e *EditorModel) HeaderMove(d int) {
	if len(e.headers) == 0 {
		return
	}
	e.hdrCursor += d
	if e.hdrCursor < 0 {
		e.hdrCursor = 0
	}
	if e.hdrCursor >= len(e.headers) {
		e.hdrCursor = len(e.headers) - 1
	}
}

// HeaderAdd opens the line editor for a fresh row appended after the
// cursor (or as the first row when the list is empty).
func (e *EditorModel) HeaderAdd() {
	e.hdrEdit.SetValue("")
	e.hdrEditOn = true
	e.hdrEditIndex = -1
	e.syncFocus()
}

// HeaderEdit opens the line editor over the selected row.
func (e *EditorModel) HeaderEdit() {
	if len(e.headers) == 0 {
		return
	}
	e.hdrEdit.SetValue(e.headers[e.hdrCursor].text)
	e.hdrEditOn = true
	e.hdrEditIndex = e.hdrCursor
	e.syncFocus()
}

// CancelHeaderEdit abandons the line editor, keeping the rows as they were:
// a cancelled add appends nothing, a cancelled edit restores nothing
// because nothing was yet changed.
func (e *EditorModel) CancelHeaderEdit() {
	e.hdrEditOn = false
	e.hdrEditIndex = -1
	e.hdrEdit.SetValue("")
	e.syncFocus()
}

// HeaderDelete drops the selected row, clamping the cursor.
func (e *EditorModel) HeaderDelete() {
	if len(e.headers) == 0 || e.hdrEditOn {
		return
	}
	e.headers = append(e.headers[:e.hdrCursor], e.headers[e.hdrCursor+1:]...)
	if e.hdrCursor >= len(e.headers) {
		e.hdrCursor = len(e.headers) - 1
	}
}

// HeaderToggle disables (comments out) or re-enables the selected row.
// Disabled rows stay in the file and out of the flight.
func (e *EditorModel) HeaderToggle() {
	if len(e.headers) == 0 || e.hdrEditOn {
		return
	}
	e.headers[e.hdrCursor].disabled = !e.headers[e.hdrCursor].disabled
}

// CommitHeaderLine validates the typed "Name: value" line into the list:
// an empty line cancels, a line without ':' is refused with its text
// intact (fail closed, never a half-row), otherwise it replaces the
// edited row or appends a fresh one.
func (e *EditorModel) CommitHeaderLine() error {
	line := strings.TrimRight(e.hdrEdit.Value(), "\r")
	if strings.TrimSpace(line) == "" {
		e.hdrEditOn = false
		e.syncFocus()
		return nil
	}
	trimmed := strings.TrimSpace(line)
	// A disabled-row remnant ("# Name: v") pasted into the line editor is
	// accepted by stripping the marker: the row's disabled bit lives on
	// the row, not in the text.
	trimmed = strings.TrimPrefix(trimmed, "#")
	trimmed = strings.TrimSpace(trimmed)
	if !strings.Contains(trimmed, ":") {
		return fmt.Errorf("ui: header %q has no ':'", line)
	}
	if e.hdrEditIndex >= 0 && e.hdrEditIndex < len(e.headers) {
		// Editing in place: the row keeps its disabled bit.
		e.headers[e.hdrEditIndex].text = trimmed
		e.hdrCursor = e.hdrEditIndex
	} else {
		at := e.hdrCursor + 1
		if len(e.headers) == 0 {
			at = 0
		}
		if at > len(e.headers) {
			at = len(e.headers)
		}
		e.headers = append(e.headers, headerRow{})
		copy(e.headers[at+1:], e.headers[at:])
		e.headers[at] = headerRow{text: trimmed}
		e.hdrCursor = at
	}
	e.hdrEditOn = false
	e.hdrEditIndex = -1
	e.hdrEdit.SetValue("")
	e.syncFocus()
	return nil
}

// Update routes a key to the focused field.
func (e *EditorModel) Update(msg tea.KeyMsg) {
	var cmd tea.Cmd
	switch {
	case e.hdrEditOn:
		e.hdrEdit, cmd = e.hdrEdit.Update(msg)
	case e.focus == 0:
		e.name, cmd = e.name.Update(msg)
	case e.focus == 1:
		e.method, cmd = e.method.Update(msg)
	case e.focus == 2:
		e.url, cmd = e.url.Update(msg)
	case e.focus == 4:
		e.body, cmd = e.body.Update(msg)
	}
	_ = cmd
}

// Values exposes method, URL and headers for gates and for submit. The
// headers render one per line; disabled rows carry their "# " marker so a
// re-parse keeps them out of the flight, matching file semantics.
func (e EditorModel) Values() (method, url, headers string) {
	var b strings.Builder
	for i, h := range e.headers {
		if i > 0 {
			b.WriteByte('\n')
		}
		if h.disabled {
			b.WriteString("# ")
		}
		b.WriteString(h.text)
	}
	return e.method.Value(), e.url.Value(), b.String()
}

// Plain converts the composer fields to a document-independent request for
// the canonical emitter. URL scheme normalization (Phase 2.1) applies
// here, at the boundary where typing becomes source: a beginner's
// `localhost:8080/health` is repaired once, visibly, never at send time
// where a rewrite would be silent.
func (e EditorModel) Plain() (ast.PlainRequest, error) {
	method := strings.ToUpper(strings.TrimSpace(e.method.Value()))
	url := strings.TrimSpace(e.url.Value())
	if norm, err := ast.NormalizeURL(url); err == nil {
		url = norm
	}
	var headers []ast.PlainHeader
	for _, h := range e.headers {
		name, value := splitHeaderRow(h.text)
		headers = append(headers, ast.PlainHeader{Name: name, Value: value, Disabled: h.disabled})
	}
	return ast.PlainRequest{
		Name:    strings.TrimSpace(e.name.Value()),
		Method:  method,
		URL:     url,
		Headers: headers,
		Body:    e.body.Value(),
	}, nil
}

// BuildBlock renders the composer as one canonical .http block for the
// workspace saver: preserved var/assert/signer lines first, then the
// canonical request. An empty name or an unparsable block fails closed
// before any byte reaches the saver.
func (e EditorModel) BuildBlock() ([]byte, error) {
	p, err := e.Plain()
	if err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("ui: name the request before saving (first field)")
	}
	var b []byte
	b = append(b, e.preserve...)
	out, err := ast.FormatPlainRequest(b, p)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Source rebuilds a minimal .http document from the fields: one request,
// preserved lines first, sealed with a separator. An empty method or URL
// is the caller's error to surface — the parser fails closed on it, never
// the composer by guessing a default.
func (e EditorModel) Source() string {
	var b strings.Builder
	b.WriteString(e.preserve)
	method := strings.TrimSpace(e.method.Value())
	url := strings.TrimSpace(e.url.Value())
	if name := strings.TrimSpace(e.name.Value()); name != "" {
		fmt.Fprintf(&b, "# @name %s\n", name)
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\n", method, url)
	_, _, headers := e.Values()
	for _, line := range strings.Split(headers, "\n") {
		if strings.TrimSpace(line) != "" {
			b.WriteString(strings.TrimRight(line, "\r") + "\n")
		}
	}
	b.WriteString("\n")
	if body := e.body.Value(); body != "" {
		b.WriteString(strings.TrimRight(body, "\r\n") + "\n")
	}
	b.WriteString("###\n")
	return b.String()
}

// FormatBody pretty-prints a JSON body in place. Non-JSON bodies are
// refused with the body untouched: reformatting what is not understood
// is corruption with a shortcut key.
func (e *EditorModel) FormatBody() error {
	raw := strings.TrimSpace(e.body.Value())
	if raw == "" {
		return fmt.Errorf("ui: nothing to format (empty body)")
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fmt.Errorf("ui: body is not JSON: %w", err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("ui: format JSON: %w", err)
	}
	e.body.SetValue(strings.TrimRight(out.String(), "\n"))
	return nil
}

// splitHeaderRow cuts "Name: value" at the first colon. A colon-less row
// keeps its text as the name with an empty value; CommitHeaderLine refuses
// such rows at entry, so this is a display backstop, never an intake.
func splitHeaderRow(row string) (name, value string) {
	if i := strings.IndexByte(row, ':'); i >= 0 {
		return strings.TrimSpace(row[:i]), strings.TrimSpace(row[i+1:])
	}
	return strings.TrimSpace(row), ""
}

// CurlCommand renders the composer's request as a copy-pasteable curl
// invocation for the yank register. Placeholders stay placeholders: the
// model holds unresolved templates, so no secret value can leak through
// a yank.
func (e EditorModel) CurlCommand() string {
	p, _ := e.Plain()
	var b strings.Builder
	b.WriteString("curl -X " + p.Method + " '" + p.URL + "'")
	for _, h := range p.Headers {
		if h.Disabled {
			continue
		}
		fmt.Fprintf(&b, " -H '%s: %s'", h.Name, h.Value)
	}
	if p.Body != "" {
		fmt.Fprintf(&b, " --data '%s'", p.Body)
	}
	return b.String()
}

// View renders the form with the focused field carrying the accent.
func (e EditorModel) View(w int, ascii bool) string {
	row := func(label string, focused bool, v string) string {
		if focused {
			return styleAccent.Render(label+": ") + v
		}
		return styleMuted.Render(label+": ") + v
	}
	var b strings.Builder
	b.WriteString(row("name  ", e.focus == 0, e.name.View()) + "\n")
	b.WriteString(row("method", e.focus == 1, e.method.View()+"  (space: next)") + "\n")
	b.WriteString(row("url   ", e.focus == 2, e.url.View()) + "\n")
	b.WriteString(styleMuted.Render("headers") + "  " + styleAccent.Render("a") + "dd " +
		styleAccent.Render("e") + "dit " + styleAccent.Render("d") + "el " +
		styleAccent.Render("space") + " on/off\n")
	for i, h := range e.headers {
		mark := " "
		if i == e.hdrCursor && e.focus == 3 && !e.hdrEditOn {
			mark = "▸"
			if ascii {
				mark = ">"
			}
		}
		text := h.text
		if h.disabled {
			text = styleMuted.Render("# " + h.text + "  (off)")
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, text)
	}
	if e.hdrEditOn {
		b.WriteString("  edit: " + e.hdrEdit.View() + "\n")
	}
	b.WriteString(row("body  ", e.focus == 4, "") + "\n" + e.body.View() + "\n")
	b.WriteString(styleMuted.Render("enter apply+send   ctrl+s/w save   f fmt JSON   tab next   esc cancel"))
	return paneStyle(true, ascii).Width(w).Render(b.String())
}
