package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// TreeItem is one navigable row: a request under its group (file path).
// Index is the request's position in the parsed document; selecting it
// makes that request the run target. Method/Target/Name are the row's
// display parts, snapshotted away from the arena for the same reason the
// model snapshots its own request: View must not slice per frame.
type TreeItem struct {
	Group  string
	Method string
	Target string
	Name   string
	Label  string
	Index  int
}

// shortTarget is the sidebar's reading of a URL: the path alone. Every row in
// a collection usually shares a host, so repeating it in a 20-column sidebar
// spends the one scarce resource — columns — on the one piece of information
// that never differs between rows. A templated target ({{base}}/notes) keeps
// its template, because there the host is not a constant.
func shortTarget(url string) string {
	i := strings.Index(url, "://")
	if i < 0 {
		return url
	}
	rest := url[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[j:]
	}
	return "/"
}

// TreeModel is the left-hand browse pane: collections/requests with keyboard
// navigation. Data is injected (SetItems) — the tree never reads the store
// or the disk itself; the owner snapshots the document after every parse.
type TreeModel struct {
	items  []TreeItem
	cursor int
}

// SetItems replaces the rows and clamps the cursor. An empty tree selects
// nothing rather than row zero of nothing.
func (t *TreeModel) SetItems(items []TreeItem) {
	t.items = append([]TreeItem(nil), items...)
	if t.cursor >= len(t.items) {
		t.cursor = len(t.items) - 1
	}
	if t.cursor < 0 && len(t.items) > 0 {
		t.cursor = 0
	}
}

// Move shifts the cursor by d, clamped to the rows.
func (t *TreeModel) Move(d int) {
	if len(t.items) == 0 {
		return
	}
	t.cursor += d
	if t.cursor < 0 {
		t.cursor = 0
	}
	if t.cursor >= len(t.items) {
		t.cursor = len(t.items) - 1
	}
}

// Selected returns the cursor row.
func (t *TreeModel) Selected() (TreeItem, bool) {
	if len(t.items) == 0 || t.cursor < 0 || t.cursor >= len(t.items) {
		return TreeItem{}, false
	}
	return t.items[t.cursor], true
}

// Cursor exposes the position for gates.
func (t *TreeModel) Cursor() int { return t.cursor }

// Len counts the rows.
func (t *TreeModel) Len() int { return len(t.items) }

// Rows renders the sidebar body: a group header, then one row per request.
// The selected row is a solid bar across the full inner width — the cursor in
// a list of thirty URLs is not findable as a single leading glyph, and a bar
// reads as a position rather than as a character.
//
// The rows are returned unframed, at exactly w columns, for the sidebar's
// framed box to place. Group headers repeat only when the group changes, so a
// file with twelve requests does not repeat its own path twelve times.
func (t *TreeModel) Rows(w, h int, focused, ascii bool) []string {
	if w < 4 {
		w = 4
	}
	if h < 1 {
		h = 1
	}
	cur, ell, groupMark := "▸", "…", "▾"
	if ascii {
		cur, ell, groupMark = ">", "...", "-"
	}
	rows := make([]string, 0, len(t.items)+1)
	lastGroup := ""
	for i, it := range t.items {
		if it.Group != lastGroup {
			lastGroup = it.Group
			rows = append(rows, styleMuted.Render(groupMark+" "+truncDisplay(it.Group, w-2, ell)))
		}
		mark := "  "
		if i == t.cursor {
			mark = cur + " "
		}
		// The @name is worth its columns on both row states: two requests to
		// the same path are otherwise indistinguishable in the list.
		label := it.Target
		if it.Name != "" {
			name := " @" + it.Name
			if lipgloss.Width(mark)+lipgloss.Width(it.Method)+1+lipgloss.Width(label)+lipgloss.Width(name) <= w {
				label += name
			}
		}
		if i == t.cursor {
			bar := mark + it.Method + " " + truncDisplay(label, max(1, w-lipgloss.Width(mark)-lipgloss.Width(it.Method)-1), ell)
			st := styleSelection
			if !focused {
				st = styleSelectionIdle
			}
			rows = append(rows, st.Render(padTo(bar, w)))
			continue
		}
		avail := max(1, w-lipgloss.Width(mark)-lipgloss.Width(it.Method)-1)
		rows = append(rows, styleMuted.Render(mark)+
			styleMethodText(it.Method).Render(it.Method)+" "+
			truncDisplay(label, avail, ell))
	}
	if len(t.items) == 0 {
		rows = append(rows, styleMuted.Render("(no requests)"))
	}
	return fitWithOverflow(rows, h, ell)
}

// SearchModel is the / in-buffer search: a query over the active StreamView
// tail with n/N match navigation. Matches are tail-relative line numbers;
// jumping scrolls the owned viewport to the match.
type SearchModel struct {
	active  bool
	query   string
	matches []SearchMatch
	sel     int
}

// Activate starts a search with an empty query.
func (s *SearchModel) Activate() {
	s.active = true
	s.query = ""
	s.matches = nil
	s.sel = 0
}

// Deactivate closes input, keeping the last matches navigable.
func (s *SearchModel) Deactivate() { s.active = false }

// Active reports input mode.
func (s *SearchModel) Active() bool { return s.active }

// Type appends a rune to the query.
func (s *SearchModel) Type(r rune) { s.query += string(r) }

// Backspace deletes one rune (byte-wise; queries are search text, not prose).
func (s *SearchModel) Backspace() {
	if len(s.query) > 0 {
		s.query = s.query[:len(s.query)-1]
	}
}

// Query exposes the text for gates.
func (s *SearchModel) Query() string { return s.query }

// Run executes the query against sv, selecting the first hit.
func (s *SearchModel) Run(sv *StreamView) {
	s.matches = sv.Search(s.query, 200)
	s.sel = 0
}

// Next cycles forward; Prev cycles backward. Both wrap: a search with one
// hit stays on it rather than falling off either end.
func (s *SearchModel) Next() {
	if len(s.matches) == 0 {
		return
	}
	s.sel = (s.sel + 1) % len(s.matches)
}

// Prev cycles backward.
func (s *SearchModel) Prev() {
	if len(s.matches) == 0 {
		return
	}
	s.sel = (s.sel - 1 + len(s.matches)) % len(s.matches)
}

// Current returns the selected hit.
func (s *SearchModel) Current() (SearchMatch, bool) {
	if len(s.matches) == 0 {
		return SearchMatch{}, false
	}
	return s.matches[s.sel], true
}

// Count counts the hits.
func (s *SearchModel) Count() int { return len(s.matches) }

// PaletteModel is the Ctrl+P command palette: fuzzy-filtered workspace
// requests plus commands. Filtering is subsequence matching over the label —
// no ranking model, just containment in order, which is predictable enough
// to gate exactly.
type PaletteModel struct {
	active   bool
	query    string
	items    []PaletteItem
	filtered []int
	cursor   int
}

// PaletteItem is one activatable row. Request rows carry RequestIndex >= 0;
// command rows carry Command and RequestIndex -1.
type PaletteItem struct {
	Label        string
	RequestIndex int
	Command      string
}

// Activate opens the palette over items.
func (p *PaletteModel) Activate(items []PaletteItem) {
	p.active = true
	p.query = ""
	p.items = append([]PaletteItem(nil), items...)
	p.refilter()
}

// Deactivate closes it.
func (p *PaletteModel) Deactivate() { p.active = false }

// Active reports visibility.
func (p *PaletteModel) Active() bool { return p.active }

// Type filters; Backspace widens.
func (p *PaletteModel) Type(r rune) {
	p.query += string(r)
	p.refilter()
}

// Backspace deletes one rune of filter text.
func (p *PaletteModel) Backspace() {
	if len(p.query) > 0 {
		p.query = p.query[:len(p.query)-1]
		p.refilter()
	}
}

// Move shifts the selection, clamped.
func (p *PaletteModel) Move(d int) {
	if len(p.filtered) == 0 {
		return
	}
	p.cursor += d
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.filtered) {
		p.cursor = len(p.filtered) - 1
	}
}

// Chosen returns the selected row.
func (p *PaletteModel) Chosen() (PaletteItem, bool) {
	if len(p.filtered) == 0 || p.cursor < 0 || p.cursor >= len(p.filtered) {
		return PaletteItem{}, false
	}
	return p.items[p.filtered[p.cursor]], true
}

// Visible exposes the filtered labels for gates.
func (p *PaletteModel) Visible() []string {
	out := make([]string, 0, len(p.filtered))
	for _, i := range p.filtered {
		out = append(out, p.items[i].Label)
	}
	return out
}

// refilter applies subsequence matching, preserving item order.
func (p *PaletteModel) refilter() {
	p.filtered = p.filtered[:0]
	p.cursor = 0
	q := strings.ToLower(p.query)
	for i, it := range p.items {
		if fuzzyContains(strings.ToLower(it.Label), q) {
			p.filtered = append(p.filtered, i)
		}
	}
}

// fuzzyContains reports whether every rune of q appears in s in order.
func fuzzyContains(s, q string) bool {
	if q == "" {
		return true
	}
	qr := []rune(q)
	j := 0
	for _, r := range s {
		if r == qr[j] {
			j++
			if j == len(qr) {
				return true
			}
		}
	}
	return false
}

// View renders the palette body: a filter line with a live caret, then up to
// eight rows, the cursor as a solid bar. Returned unframed for the modal box.
func (p *PaletteModel) View(w int, ascii bool) string {
	ell := "…"
	if ascii {
		ell = "..."
	}
	var b strings.Builder
	caret := "▏"
	if ascii {
		caret = "|"
	}
	b.WriteString(styleMuted.Render("filter: ") + styleHeader.Render(p.query) + caret + "\n")
	n := len(p.filtered)
	if n > 8 {
		n = 8
	}
	for i := 0; i < n; i++ {
		label := truncDisplay(p.items[p.filtered[i]].Label, max(1, w-2), ell)
		if i == p.cursor {
			b.WriteString(styleSelection.Render(padTo(" "+label, w)))
		} else {
			b.WriteString(" " + label)
		}
		b.WriteByte('\n')
	}
	if len(p.filtered) == 0 {
		b.WriteString(styleMuted.Render("(no match)"))
	}
	return strings.TrimRight(b.String(), "\n")
}
