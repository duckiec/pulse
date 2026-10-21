package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Body colouring for the response viewport. A JSON body is the single most
// common thing a request client shows, and the difference between scanning a
// 400-line nesting and finding the one field you care about is four colours.
//
// The scope is deliberately narrow: this is a *highlighter*, not a parser. It
// never validates, never reformats, never reorders — a line it does not
// recognise passes through byte-identical, and every token it does colour is
// emitted verbatim between two escape sequences. Display width is therefore
// unchanged, which is what keeps the frame's arithmetic correct: the padding
// was computed before the colour was applied.
//
// Cost is bounded by the viewport, not the body: the caller passes only the
// visible rows (at most ~60), never the 1 MiB tail.

var (
	styleJSONKey = lipgloss.NewStyle().Foreground(lipgloss.Color("#7EE787"))
	styleJSONStr = lipgloss.NewStyle().Foreground(lipgloss.Color("#A5D6FF"))
	styleJSONNum = lipgloss.NewStyle().Foreground(lipgloss.Color("#79C0FF"))
	styleJSONLit = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF7B72"))
)

// highlightBody colourises the rows that look like JSON and returns the rest
// untouched. It returns the same slice length, so the caller's row arithmetic
// is unaffected.
func highlightBody(rows []string) []string {
	for i, row := range rows {
		if looksJSON(row) {
			rows[i] = highlightJSON(row)
		}
	}
	return rows
}

// looksJSON is the cheap gate: a line is JSON-ish when its first non-space
// byte opens a structure, starts a string (an object member), or begins a
// number. Plain-text and HTML bodies fall straight through.
func looksJSON(line string) bool {
	s := strings.TrimLeft(line, " \t")
	if s == "" {
		return false
	}
	switch c := s[0]; {
	case c == '{', c == '[', c == '"':
		return true
	case c == '-', c >= '0' && c <= '9':
		return true
	}
	return false
}

// highlightJSON colours one line: keys green, string values blue, numbers
// blue-white, true/false/null red, structural punctuation muted.
func highlightJSON(line string) string {
	var b strings.Builder
	b.Grow(len(line) + 32)
	n := len(line)
	for i := 0; i < n; {
		c := line[i]
		switch {
		case c == '"':
			j := i + 1
			for j < n {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					j++
					break
				}
				j++
			}
			if j > n {
				j = n
			}
			tok := line[i:j]
			// A string followed by a colon is a member name, not a value:
			// the one distinction that makes a nested body skimmable.
			k := j
			for k < n && (line[k] == ' ' || line[k] == '\t') {
				k++
			}
			if k < n && line[k] == ':' {
				b.WriteString(styleJSONKey.Render(tok))
			} else {
				b.WriteString(styleJSONStr.Render(tok))
			}
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < n && (line[j] == '-' || line[j] == '+' || line[j] == '.' ||
				line[j] == 'e' || line[j] == 'E' || (line[j] >= '0' && line[j] <= '9')) {
				j++
			}
			b.WriteString(styleJSONNum.Render(line[i:j]))
			i = j
		case strings.HasPrefix(line[i:], "true"):
			b.WriteString(styleJSONLit.Render("true"))
			i += 4
		case strings.HasPrefix(line[i:], "false"):
			b.WriteString(styleJSONLit.Render("false"))
			i += 5
		case strings.HasPrefix(line[i:], "null"):
			b.WriteString(styleJSONLit.Render("null"))
			i += 4
		case c == '{' || c == '}' || c == '[' || c == ']' || c == ',' || c == ':':
			b.WriteString(styleMuted.Render(string(c)))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
