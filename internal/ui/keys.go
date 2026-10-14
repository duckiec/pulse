package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Binding is one action with every key that triggers it. The footer and the
// help overlay render from these entries, so a binding without an entry is
// undiscoverable — and per DESIGN_SYSTEM.md §5, fails review.
type Binding struct {
	// Keys are tea.Key.String() forms ("q", "ctrl+s", "tab", "?").
	Keys []string
	// Help is the short footer token ("q quit").
	Help string
	// Desc is the one-line help-overlay description.
	Desc string
}

// KeyMap is the central key registry (DESIGN_SYSTEM.md §5). Uniformity law:
// the same key does the same thing in every pane, so matching is global and
// pane focus only changes scroll targets, never bindings.
type KeyMap struct {
	Quit       Binding
	Send       Binding
	Cancel     Binding
	Focus      Binding
	Down       Binding
	Up         Binding
	Top        Binding
	Bottom     Binding
	HalfDown   Binding
	HalfUp     Binding
	FullDown   Binding
	FullUp     Binding
	Pause      Binding
	Search     Binding
	SearchNext Binding
	Palette    Binding
	Editor     Binding
	EditorAlt  Binding
	FuzzTab    Binding
	Select     Binding
	Help       Binding
}

// DefaultKeyMap is the complete binding set. Every entry names a wired
// action: a keymap entry for an unwired action would be a lie the help
// overlay tells with confidence.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit:       Binding{Keys: []string{"q", "ctrl+c"}, Help: "q quit", Desc: "Quit (cancels any in-flight run)"},
		Send:       Binding{Keys: []string{"ctrl+s"}, Help: "^S send", Desc: "Send the current request"},
		Cancel:     Binding{Keys: []string{"ctrl+x"}, Help: "^X cancel", Desc: "Cancel the in-flight request"},
		Focus:      Binding{Keys: []string{"tab"}, Help: "tab pane", Desc: "Cycle pane focus"},
		Down:       Binding{Keys: []string{"j", "down"}, Help: "j/k scroll", Desc: "Scroll down one line"},
		Up:         Binding{Keys: []string{"k", "up"}, Help: "", Desc: "Scroll up one line"},
		Top:        Binding{Keys: []string{"g", "home"}, Help: "g/G top/bot", Desc: "Jump to top"},
		Bottom:     Binding{Keys: []string{"G", "end"}, Help: "", Desc: "Jump to bottom"},
		HalfDown:   Binding{Keys: []string{"ctrl+d", "pgdown"}, Help: "^D/^U page", Desc: "Half-page down"},
		HalfUp:     Binding{Keys: []string{"ctrl+u", "pgup"}, Help: "", Desc: "Half-page up"},
		FullDown:   Binding{Keys: []string{"ctrl+f"}, Help: "", Desc: "Full page down"},
		FullUp:     Binding{Keys: []string{"ctrl+b"}, Help: "", Desc: "Full page up"},
		Pause:      Binding{Keys: []string{"p"}, Help: "p pause", Desc: "Pause/resume the stream"},
		Search:     Binding{Keys: []string{"/"}, Help: "/ find", Desc: "Search the stream tail"},
		SearchNext: Binding{Keys: []string{"n"}, Help: "n/N next", Desc: "Next search hit (N: previous)"},
		Palette:    Binding{Keys: []string{"ctrl+p"}, Help: "^P palette", Desc: "Command palette"},
		Editor:     Binding{Keys: []string{"i"}, Help: "i edit", Desc: "Edit request (URL first)"},
		EditorAlt:  Binding{Keys: []string{"e"}, Help: "", Desc: "Edit request (headers first)"},
		FuzzTab:    Binding{Keys: []string{"f"}, Help: "f fuzz", Desc: "Toggle fuzz dashboard"},
		Select:     Binding{Keys: []string{"enter"}, Help: "", Desc: "Activate tree/palette selection"},
		Help:       Binding{Keys: []string{"?"}, Help: "? help", Desc: "Toggle this help overlay"},
	}
}

// Match reports whether msg triggers b. Matching is on the canonical
// Key.String() form, never on runes or codes directly, so "ctrl+s" means
// the same thing here as in the footer that advertises it.
func (k KeyMap) Match(b Binding, msg tea.KeyMsg) bool {
	s := tea.Key(msg).String()
	for _, key := range b.Keys {
		if strings.EqualFold(s, key) {
			return true
		}
	}
	return false
}

// FooterHelp returns the footer tokens in display order, skipping bindings
// with no Help text (secondary keys that share a token with their primary).
func (k KeyMap) FooterHelp() []string {
	out := []string{}
	for _, b := range []Binding{k.Quit, k.Send, k.Cancel, k.Focus, k.Down, k.Top, k.HalfDown, k.Pause, k.Search, k.SearchNext, k.Palette, k.Editor, k.FuzzTab, k.Help} {
		if b.Help != "" {
			out = append(out, b.Help)
		}
	}
	return out
}

// All returns every binding, for the help overlay and the completeness gate:
// a binding the overlay cannot see is a binding that does not exist.
func (k KeyMap) All() []Binding {
	return []Binding{k.Quit, k.Send, k.Cancel, k.Focus, k.Down, k.Up, k.Top, k.Bottom, k.HalfDown, k.HalfUp, k.FullDown, k.FullUp, k.Pause, k.Search, k.SearchNext, k.Palette, k.Editor, k.EditorAlt, k.FuzzTab, k.Select, k.Help}
}
