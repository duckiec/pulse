package ui

import (
	"fmt"
	"strings"
)

// TreeItem is one navigable row: a request under its group (file path).
// Index is the request's position in the parsed document; selecting it
// makes that request the run target.
type TreeItem struct {
	Group string
	Label string
	Index int
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

// View renders grouped rows with a cursor glyph. Glyphs degrade via ascii:
// "▸"/"─" become ">" / "-".
func (t *TreeModel) View(focused bool, w int, ascii bool) string {
	cur, norm, ell := ">", "-", "..."
	if !ascii {
		cur, norm, ell = "▸", "─", "…"
	}
	var b strings.Builder
	lastGroup := ""
	for i, it := range t.items {
		if it.Group != lastGroup {
			lastGroup = it.Group
			fmt.Fprintf(&b, "%s %s\n", norm, truncFor(it.Group, w-2, ell))
		}
		mark := " "
		if i == t.cursor {
			mark = cur
		}
		fmt.Fprintf(&b, "%s %s\n", mark, truncFor(it.Label, w-2, ell))
	}
	if len(t.items) == 0 {
		b.WriteString("(no requests)\n")
	}
	body := strings.TrimRight(b.String(), "\n")
	return paneStyle(focused, ascii).Width(w).Render(body)
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

// View renders the palette: filter line plus up to 8 rows.
func (p *PaletteModel) View(w int, ascii bool) string {
	mark, ell := "▸", "…"
	if ascii {
		mark, ell = ">", "..."
	}
	var b strings.Builder
	b.WriteString("palette: " + p.query + "\n")
	n := len(p.filtered)
	if n > 8 {
		n = 8
	}
	for i := 0; i < n; i++ {
		m := " "
		if i == p.cursor {
			m = mark
		}
		fmt.Fprintf(&b, "%s %s\n", m, truncFor(p.items[p.filtered[i]].Label, w-2, ell))
	}
	if len(p.filtered) == 0 {
		b.WriteString("(no match)\n")
	}
	return paneStyle(true, ascii).Width(w).Render(strings.TrimRight(b.String(), "\n"))
}
