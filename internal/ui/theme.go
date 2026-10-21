package ui

import (
	"github.com/charmbracelet/lipgloss"
)

// The palette. Every colour is a named token here — never a literal at a call
// site — because the degradation ladder (DESIGN_SYSTEM.md §4.2) is decided per
// token: what a colour quantizes to at 16 colours is a property of the token,
// not of the pane that happened to use it.
//
// Muted gray is #6E7681 rather than the spec's first-draft #8B949E: the two
// are near-twins in truecolor, but under the 16-colour profile the draft
// quantizes to bright-blue (94) — the accent's own code — while this one lands
// on cyan (36), distinct from every other token. The colour-token gate pins
// all six text codes.
var (
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C9D1D9"))
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6E7681"))
	styleAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("#58A6FF"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FB950"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F85149"))
	// Warning amber is #E3B341 rather than #D29922 for the same ladder
	// reason: the draft quantizes to bright-red (91) — the error code —
	// while this one lands on yellow (33). Same family, honest at 16.
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("#E3B341"))
)

// Frame tokens. The two border colours are the focus indicator: exactly one
// pane in a column can carry the accent edge, so "where do my keys go" is
// answerable without reading a single word. They are deliberately a different
// visual channel from the text tokens — a border sharing a hue with a text
// state is fine, two text states in one code is not.
var (
	styleBorderIdle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#30363D"))
	styleBorderActive = lipgloss.NewStyle().Foreground(lipgloss.Color("#58A6FF"))
)

// Chrome tokens: the mode chip, keycaps, and selection bar. Solid fills with
// contrasting text are the whole "pill badge" idea — a badge reads as a
// label at a glance, where coloured text reads as emphasis and has to be
// parsed. Backgrounds degrade to nothing at the Ascii profile, which is why
// every badge also carries its word ("GET", "200 OK", "NORMAL").
var (
	styleModeChip = lipgloss.NewStyle().
			Background(lipgloss.Color("#21262D")).
			Foreground(lipgloss.Color("#C9D1D9")).
			Bold(true)
	styleModeActive = lipgloss.NewStyle().
			Background(lipgloss.Color("#1F6FEB")).
			Foreground(lipgloss.Color("#F0F6FC")).
			Bold(true)
	styleKeyCap = lipgloss.NewStyle().
			Background(lipgloss.Color("#21262D")).
			Foreground(lipgloss.Color("#8B949E"))
	styleSelection = lipgloss.NewStyle().
			Background(lipgloss.Color("#1F6FEB")).
			Foreground(lipgloss.Color("#F0F6FC")).
			Bold(true)
	// The unfocused selection is still a bar — the cursor must stay findable
	// while the keys scroll the response — but it recedes so the accent
	// border keeps meaning "this is where input goes".
	styleSelectionIdle = lipgloss.NewStyle().
				Background(lipgloss.Color("#21262D")).
				Foreground(lipgloss.Color("#C9D1D9"))
)

// styleMethod is the per-verb badge fill. A method is the one thing a request
// row leads with, so it gets a solid pill in the list and in the omnibox;
// the fill is what distinguishes GET from POST at a glance in a column of
// thirty rows.
func styleMethod(method string) lipgloss.Style {
	bg := "#484F58"
	switch method {
	case "GET":
		bg = "#1F6FEB"
	case "POST":
		bg = "#238636"
	case "PUT":
		bg = "#9E6A03"
	case "PATCH":
		bg = "#8957E5"
	case "DELETE":
		bg = "#DA3633"
	case "HEAD", "OPTIONS":
		bg = "#484F58"
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color(bg)).
		Foreground(lipgloss.Color("#F0F6FC")).
		Bold(true)
}

// styleMethodText is the text-only variant for dense rows where a fill would
// fight the selection bar: same hue family, no background.
func styleMethodText(method string) lipgloss.Style {
	fg := "#8B949E"
	switch method {
	case "GET":
		fg = "#58A6FF"
	case "POST":
		fg = "#3FB950"
	case "PUT":
		fg = "#E3B341"
	case "PATCH":
		fg = "#BC8CFF"
	case "DELETE":
		fg = "#F85149"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fg)).Bold(true)
}

// styleStatus is the response-status badge fill: 2xx green, 3xx blue, 4xx
// amber, 5xx red. The words ("429 Too Many Requests") are always present, so
// a reader who cannot see the fill loses nothing.
func styleStatus(code int) lipgloss.Style {
	bg := "#484F58"
	switch {
	case code >= 200 && code < 300:
		bg = "#238636"
	case code >= 300 && code < 400:
		bg = "#1F6FEB"
	case code >= 400 && code < 500:
		bg = "#9E6A03"
	case code >= 500:
		bg = "#DA3633"
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color(bg)).
		Foreground(lipgloss.Color("#F0F6FC")).
		Bold(true)
}
