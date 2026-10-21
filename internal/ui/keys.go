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
// pane focus only changes scroll targets, never bindings. Modals take
// precedence in opacity order (vault/new-file/import over composer over
// palette over search), and each modal documents its own local keys in its
// footer line — a letter that types text in one focus and acts in another
// is resolved by focus, never by pane.
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
	Import     Binding
	Vault      Binding
	Yank       Binding
	NewFile    Binding
	Starter    Binding
	Save       Binding
	FuzzTab    Binding
	Select     Binding
	Help       Binding
}

// DefaultKeyMap is the complete binding set. Every entry names a wired
// action: a keymap entry for an unwired action would be a lie the help
// overlay tells with confidence.
//
// Phase 2.2 assignments: e opens the composer (the old i), i imports a
// pasted curl, g generates the starter collection (top/bottom keep
// home/end; vim-style g/G yield to the blank-state verbs), n creates a
// file (search-next moves to ctrl+n, previous stays N), v seals a secret,
// y yanks curl, f stays the fuzz tab globally and formats JSON inside the
// composer, ctrl+s sends globally and saves inside the composer.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit:       Binding{Keys: []string{"q", "ctrl+c"}, Help: "q quit", Desc: "Quit (cancels any in-flight run)"},
		Send:       Binding{Keys: []string{"ctrl+s"}, Help: "^S send", Desc: "Send the current request"},
		Cancel:     Binding{Keys: []string{"ctrl+x"}, Help: "^X cancel", Desc: "Cancel the in-flight request"},
		Focus:      Binding{Keys: []string{"tab"}, Help: "tab pane", Desc: "Cycle pane focus"},
		Down:       Binding{Keys: []string{"j", "down"}, Help: "j/k scroll", Desc: "Scroll down one line"},
		Up:         Binding{Keys: []string{"k", "up"}, Help: "", Desc: "Scroll up one line"},
		Top:        Binding{Keys: []string{"home"}, Help: "home/end", Desc: "Jump to top"},
		Bottom:     Binding{Keys: []string{"end"}, Help: "", Desc: "Jump to bottom"},
		HalfDown:   Binding{Keys: []string{"ctrl+d", "pgdown"}, Help: "^D/^U page", Desc: "Half-page down"},
		HalfUp:     Binding{Keys: []string{"ctrl+u", "pgup"}, Help: "", Desc: "Half-page up"},
		FullDown:   Binding{Keys: []string{"ctrl+f"}, Help: "", Desc: "Full page down"},
		FullUp:     Binding{Keys: []string{"ctrl+b"}, Help: "", Desc: "Full page up"},
		Pause:      Binding{Keys: []string{"p"}, Help: "p pause", Desc: "Pause/resume the stream"},
		Search:     Binding{Keys: []string{"/"}, Help: "/ find", Desc: "Search the stream tail"},
		SearchNext: Binding{Keys: []string{"ctrl+n"}, Help: "^N/N next", Desc: "Next search hit (N: previous)"},
		Palette:    Binding{Keys: []string{"ctrl+p"}, Help: "^P palette", Desc: "Command palette"},
		Editor:     Binding{Keys: []string{"e"}, Help: "e edit", Desc: "Compose the request (method, URL, headers, body)"},
		Import:     Binding{Keys: []string{"i"}, Help: "i curl", Desc: "Import a pasted curl command into the composer"},
		Vault:      Binding{Keys: []string{"v"}, Help: "v vault", Desc: "Seal a secret without leaving the TUI"},
		Yank:       Binding{Keys: []string{"y"}, Help: "y yank", Desc: "Yank the request as a curl command"},
		NewFile:    Binding{Keys: []string{"n"}, Help: "n new", Desc: "New collection file in the workspace"},
		Starter:    Binding{Keys: []string{"g"}, Help: "g start", Desc: "Generate the starter collection"},
		Save:       Binding{Keys: []string{"ctrl+s", "w"}, Help: "", Desc: "Save the composer request to disk (composer only)"},
		FuzzTab:    Binding{Keys: []string{"f"}, Help: "f fuzz", Desc: "Toggle fuzz dashboard (formats JSON in composer)"},
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

// FooterHelp returns the status bar's keycap pool in priority order, skipping
// bindings with no token (secondary keys that share a token with their
// primary). Order is priority, not history: the bar fills left to right until
// it runs out of room, so bailing out (quit), the two run keys, and help must
// never be the entries that get dropped on an 80-column terminal.
func (k KeyMap) FooterHelp() []string {
	out := []string{}
	for _, b := range []Binding{
		k.Quit, k.Send, k.Cancel, k.Focus, k.Editor, k.Search, k.Help, k.Palette,
		k.Import, k.NewFile, k.Starter, k.FuzzTab, k.Vault, k.Yank,
		k.Down, k.Top, k.HalfDown, k.Pause, k.SearchNext,
	} {
		if b.Help != "" {
			out = append(out, b.Help)
		}
	}
	return out
}

// ModeKeys is the status bar's keycap set for one input mode. The footer is
// the only discovery surface on a workbench with no menus, so the tokens are
// honest by construction: every one names a key this keymap (or the owning
// modal) actually dispatches in that mode, and anything missing from the
// visible line is still in the ? overlay.
func (k KeyMap) ModeKeys(mode string) []string {
	switch mode {
	case "EDIT":
		return []string{"enter apply", "ctrl+s save", "f fmt", "tab next", "esc cancel"}
	case "VAULT":
		return []string{"tab field", "enter seal", "esc cancel"}
	case "IMPORT":
		return []string{"enter import", "esc cancel"}
	case "NEW FILE":
		return []string{"enter create", "esc cancel"}
	case "PALETTE":
		return []string{"enter open", "j/k move", "esc close"}
	case "SEARCH":
		return []string{"enter run", "ctrl+n next", "N prev", "esc close"}
	case "HELP":
		return []string{"? close", "q quit"}
	case "FUZZ":
		return []string{"f back", "ctrl+s send", "? help"}
	default:
		return k.FooterHelp()
	}
}

// All returns every binding, for the help overlay and the completeness gate:
// a binding the overlay cannot see is a binding that does not exist.
func (k KeyMap) All() []Binding {
	return []Binding{k.Quit, k.Send, k.Cancel, k.Focus, k.Down, k.Up, k.Top, k.Bottom, k.HalfDown, k.HalfUp, k.FullDown, k.FullUp, k.Pause, k.Search, k.SearchNext, k.Palette, k.Editor, k.Import, k.Vault, k.Yank, k.NewFile, k.Starter, k.Save, k.FuzzTab, k.Select, k.Help}
}
