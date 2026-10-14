package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// EditorModel is the request editor: form mode over method, URL, and
// headers, opened with i/e and submitted with enter (apply + send) or
// abandoned with esc.
//
// huh is deliberately NOT the implementation: it is absent from go.mod and
// the allowlist by prior decision (TASKS.md P3.2.2), and admitting a new
// dependency for three text fields would trade a fetch, an allowlist entry,
// and a new import edge for nothing bubbles/textinput — already vendored
// and sanctioned — does not do. Recorded in ADL-0040.
type EditorModel struct {
	active  bool
	method  textinput.Model
	url     textinput.Model
	headers textinput.Model
	focus   int
}

// NewEditorModel returns a closed editor with blank fields.
func NewEditorModel() EditorModel {
	mk := func(ph string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		ti.Prompt = ""
		return ti
	}
	return EditorModel{
		method:  mk("GET"),
		url:     mk("https://..."),
		headers: mk("Name: value"),
	}
}

// Open shows the editor seeded from the current request, focusing field
// focus (0 method, 1 url, 2 headers).
func (e *EditorModel) Open(method, url string, focus int) {
	e.active = true
	e.method.SetValue(method)
	e.url.SetValue(url)
	e.focus = focus
	if e.focus < 0 || e.focus > 2 {
		e.focus = 1
	}
	e.syncFocus()
}

// Close hides the editor without applying.
func (e *EditorModel) Close() {
	e.active = false
	e.method.Blur()
	e.url.Blur()
	e.headers.Blur()
}

// Active reports visibility.
func (e EditorModel) Active() bool { return e.active }

// Cycle moves field focus.
func (e *EditorModel) Cycle() {
	e.focus = (e.focus + 1) % 3
	e.syncFocus()
}

func (e *EditorModel) syncFocus() {
	e.method.Blur()
	e.url.Blur()
	e.headers.Blur()
	switch e.focus {
	case 0:
		e.method.Focus()
	case 1:
		e.url.Focus()
	default:
		e.headers.Focus()
	}
}

// Update routes a key to the focused field.
func (e *EditorModel) Update(msg tea.KeyMsg) {
	var cmd tea.Cmd
	switch e.focus {
	case 0:
		e.method, cmd = e.method.Update(msg)
	case 1:
		e.url, cmd = e.url.Update(msg)
	default:
		e.headers, cmd = e.headers.Update(msg)
	}
	_ = cmd
}

// Values exposes the fields for gates and for submit.
func (e EditorModel) Values() (method, url, headers string) {
	return e.method.Value(), e.url.Value(), e.headers.Value()
}

// Source rebuilds a minimal .http document from the fields: one request,
// headers verbatim, sealed with a separator. An empty method or URL is the
// caller's error to surface — the parser fails closed on it, never the
// editor by guessing a default.
func (e EditorModel) Source() string {
	var b strings.Builder
	method := strings.TrimSpace(e.method.Value())
	url := strings.TrimSpace(e.url.Value())
	fmt.Fprintf(&b, "%s %s HTTP/1.1\n", method, url)
	for _, line := range strings.Split(e.headers.Value(), "\n") {
		if strings.TrimSpace(line) != "" {
			b.WriteString(strings.TrimRight(line, "\r") + "\n")
		}
	}
	b.WriteString("\n###\n")
	return b.String()
}

// View renders the form with the focused field carrying the accent.
func (e EditorModel) View(w int, ascii bool) string {
	row := func(name string, idx int, ti textinput.Model) string {
		v := ti.View()
		if idx == e.focus {
			return styleAccent.Render(name+": ") + v
		}
		return styleMuted.Render(name+": ") + v
	}
	body := row("method ", 0, e.method) + "\n" +
		row("url    ", 1, e.url) + "\n" +
		row("headers", 2, e.headers) +
		styleMuted.Render("\nenter apply+send   esc cancel   tab next field")
	return paneStyle(true, ascii).Width(w).Render(body)
}
