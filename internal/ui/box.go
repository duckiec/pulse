package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// This file is the workbench's geometry substrate: one frame renderer whose
// output is exact by construction.
//
// The contract is the whole point. render returns exactly h rows, each exactly
// w display cells wide, for any w >= 4 and h >= 2, whatever the caller hands
// it: short content, long content, styled content, empty content, wide runes,
// or ANSI escapes mid-string. That is what makes "borders never clip" a
// property of the type rather than a hope — the old layout concatenated
// independent bordered strings and let lipgloss pad each one to its own idea
// of the width, so the frames only lined up when the content happened to.
//
// Titles are *inlaid* in the top border (the ATAC/lazygit/superfile
// convention): `╭─ [1] Requests (4) ───────╮`. The border is one display
// channel, the title another, so the title is rendered as its own span and
// the rule glyphs are counted around it — never as content that pushes the
// border.

// asciiBorder is the degradation-ladder border: lipgloss's NormalBorder is
// still box-drawing (square corners, not ASCII), so a genuinely ASCII frame
// needs its own preset. The border is a glyph like any other, and the ASCII
// gate asserts zero non-ASCII bytes in the whole frame.
var asciiBorder = lipgloss.Border{
	Top: "-", Bottom: "-", Left: "|", Right: "|",
	TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
	MiddleLeft: "+", MiddleRight: "+", Middle: "+",
	MiddleTop: "+", MiddleBottom: "+",
}

// framed is one region of the workbench: a border, an inlaid title, an
// optional inlaid right-hand label (a live counter, a phase, a hotkey), and
// an active flag that selects the accent border.
type framed struct {
	w, h   int
	title  string
	right  string
	active bool
	ascii  bool
	// titleBright keeps the title at full contrast on a chrome panel that
	// is never a focus target (the omnibox). Focus still only tints
	// borders, so "bright title" can never be mistaken for "has focus".
	titleBright bool
}

func (f framed) border() lipgloss.Border {
	if f.ascii {
		return asciiBorder
	}
	return lipgloss.RoundedBorder()
}

func (f framed) borderStyle() lipgloss.Style {
	if f.active {
		return styleBorderActive
	}
	return styleBorderIdle
}

func (f framed) titleStyle() lipgloss.Style {
	if f.active || f.titleBright {
		return styleHeader
	}
	return styleMuted
}

// render draws the frame around content. Content rows beyond h-2 are dropped
// (the frame wins: the screen is the fixed resource), short content is padded
// with blank rows, and every returned row is exactly w cells.
func (f framed) render(content []string) string {
	w, h := f.w, f.h
	if w < 4 {
		w = 4
	}
	if h < 2 {
		h = 2
	}
	br := f.border()
	bs := f.borderStyle()
	inner := w - 2

	rows := make([]string, 0, h)
	rows = append(rows, f.topRow(br, bs, inner))
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(content) {
			line = content[i]
		}
		rows = append(rows, bs.Render(br.Left)+padTo(line, inner)+bs.Render(br.Right))
	}
	rows = append(rows, bs.Render(br.BottomLeft+strings.Repeat(br.Bottom, inner)+br.BottomRight))
	return strings.Join(rows, "\n")
}

// topRow builds the inlaid top border: one rule glyph after the corner, the
// title (if any), fill, the right label (if any), one rule glyph before the
// corner. When both inlays cannot fit, the right label yields first — it is
// always the secondary reading order — then the title truncates.
func (f framed) topRow(br lipgloss.Border, bs lipgloss.Style, inner int) string {
	title, right := f.title, f.right
	// Two cells are spent on the corner-adjacent rule glyphs.
	avail := inner - 2
	span := func(s string) int {
		if s == "" {
			return 0
		}
		return lipgloss.Width(s) + 2
	}
	if span(title)+span(right) > avail {
		right = ""
	}
	if t := avail - 2; span(title) > avail && t > 0 {
		title = truncDisplay(title, t, "…")
	}
	fill := avail - span(title) - span(right)
	if fill < 0 {
		fill = 0
	}

	var b strings.Builder
	b.WriteString(bs.Render(br.TopLeft + br.Top))
	if title != "" {
		b.WriteString(f.titleStyle().Render(" " + title + " "))
	}
	if fill > 0 {
		b.WriteString(bs.Render(strings.Repeat(br.Top, fill)))
	}
	if right != "" {
		b.WriteString(styleMuted.Render(" " + right + " "))
	}
	b.WriteString(bs.Render(br.Top + br.TopRight))
	return b.String()
}

// padTo pads s with spaces to w display cells, or clips it to fit. Display
// width, never byte length: a styled badge and a CJK label both measure by
// what the terminal draws.
func padTo(s string, w int) string {
	if w <= 0 {
		return ""
	}
	switch d := w - lipgloss.Width(s); {
	case d > 0:
		return s + strings.Repeat(" ", d)
	case d < 0:
		return clipANSI(s, w)
	default:
		return s
	}
}

// clipANSI cuts a string to w display cells without cutting an escape
// sequence in half, and closes any style it left open. A clipped styled span
// that loses its reset bleeds into the rest of the frame — the classic
// "one red cell turns the whole row red" bug — so the reset is part of the
// contract, not a nicety.
func clipANSI(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	open := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			open = true
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cw := runeWidth(r)
		if used+cw > w {
			break
		}
		b.WriteString(s[i : i+size])
		used += cw
		i += size
	}
	if open {
		b.WriteString("\x1b[0m")
	}
	if d := w - used; d > 0 {
		b.WriteString(strings.Repeat(" ", d))
	}
	return b.String()
}

// ellipsis is the truncation marker for a glyph mode: Unicode default, ASCII
// on demand. It is a function rather than a constant so no render path can
// reach for the pretty one under --ascii and break the glyph gate.
func ellipsis(ascii bool) string {
	if ascii {
		return "..."
	}
	return "…"
}

// runeWidth is one rune's cell width: 1 for ASCII (the overwhelming majority
// of frame content), the terminal's own answer otherwise.
func runeWidth(r rune) int {
	if r < 0x80 {
		if r < 0x20 || r == 0x7f {
			return 0
		}
		return 1
	}
	return lipgloss.Width(string(r))
}

// truncDisplay shortens s to n display cells with the given ellipsis.
func truncDisplay(s string, n int, ell string) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	limit := n - lipgloss.Width(ell)
	if limit < 1 {
		return ell
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		cw := runeWidth(r)
		if used+cw > limit {
			break
		}
		b.WriteRune(r)
		used += cw
	}
	return b.String() + ell
}

// splitRows splits rendered content into exactly n rows, padding with blanks
// and dropping overflow. The frame already enforces this; callers use it to
// guarantee the *screen* count too, so a mistake in one pane cannot shift
// everything below it by a row.
func splitRows(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	rows := strings.Split(s, "\n")
	for len(rows) < n {
		rows = append(rows, "")
	}
	return rows[:n]
}

// fitRows makes exactly n rows from a captured block (a viewport frame, a
// modal body): short blocks pad with blanks, long ones truncate.
func fitRows(rows []string, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, n)
	copy(out, rows)
	return out
}
