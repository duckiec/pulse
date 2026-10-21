package ui

// Geometry is computed once per size message and is the single source of every
// pane's width and height. Nothing in the render path invents a size: a pane
// that is given g.mainW cannot disagree with the pane above it, which is the
// structural reason the old layout clipped at odd widths.
//
// The screen is divided as:
//
//	omniboxRows  top bar (brand + live target)
//	bodyH         sidebar | main column
//	statusRows   unified footer
//
// and the main column as request (a third of the body, clamped) over response
// (the remainder). The clamps are what keep 80x24 usable: a six-row request
// pane is enough for the flight line plus asserts and headers, and leaves
// fourteen rows of response.
const (
	omniboxRows = 3
	statusRows  = 1

	minScreenW = 80
	minScreenH = 24

	// Sidebar width band. Below 20 cells a method badge plus a path is
	// unreadable; above 34 the response loses more than the list gains.
	sidebarMin = 20
	sidebarMax = 34
)

// geometry is the resolved grid for one frame.
type geometry struct {
	w, h     int
	sidebarW int
	mainW    int
	bodyH    int
	reqH     int
	resH     int
}

// computeGeometry resolves the grid for a w×h terminal. It is total: every
// input at or above the 80x24 floor yields positive pane sizes whose sum is
// exactly the screen.
func computeGeometry(w, h int) geometry {
	if w < minScreenW {
		w = minScreenW
	}
	if h < minScreenH {
		h = minScreenH
	}
	g := geometry{w: w, h: h}
	g.bodyH = h - omniboxRows - statusRows
	if g.bodyH < 6 {
		g.bodyH = 6
	}

	g.sidebarW = w / 4
	if g.sidebarW < sidebarMin {
		g.sidebarW = sidebarMin
	}
	if g.sidebarW > sidebarMax {
		g.sidebarW = sidebarMax
	}
	g.mainW = w - g.sidebarW

	g.reqH = g.bodyH / 4
	if g.reqH < 5 {
		g.reqH = 5
	}
	if g.reqH > 12 {
		g.reqH = 12
	}
	// The response pane must always keep its three header rows plus a
	// viewport row, otherwise a tall request pane would starve the stream.
	if g.reqH > g.bodyH-6 {
		g.reqH = g.bodyH - 6
	}
	if g.reqH < 4 {
		g.reqH = 4
	}
	g.resH = g.bodyH - g.reqH
	return g
}
