// Grid is a 2D character canvas with per-cell foreground coloring,
// purpose-built for drawing ASCII network topology diagrams. The
// existing tier-panel renderer in this package was list-of-text. This
// is the real one: boxes, lines, junctions, all positionally placed.
package netmap

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// cell is one terminal character + its foreground / background colors +
// bold flag. Bg of empty-string means "no background" (transparent).
type cell struct {
	r    rune
	fg   lipgloss.Color
	bg   lipgloss.Color
	bold bool
}

// Grid is a width × height matrix of cells. Origin (0,0) is top-left.
// All draw primitives are clipped to bounds, so a box drawn partially
// off-canvas is silently truncated rather than panicking.
type Grid struct {
	w, h  int
	cells [][]cell
}

// NewGrid creates an empty grid (all spaces, default fg) at the supplied
// pixel/character dimensions.
func NewGrid(w, h int) *Grid {
	g := &Grid{w: w, h: h}
	g.cells = make([][]cell, h)
	for y := 0; y < h; y++ {
		g.cells[y] = make([]cell, w)
		for x := 0; x < w; x++ {
			g.cells[y][x] = cell{r: ' '}
		}
	}
	return g
}

// Set writes one styled rune at (x,y). No-op when out of bounds.
func (g *Grid) Set(x, y int, r rune, fg lipgloss.Color) {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return
	}
	// Preserve any existing bg/bold so line-draw doesn't strip a
	// background-tinted region beneath it.
	prev := g.cells[y][x]
	g.cells[y][x] = cell{r: r, fg: fg, bg: prev.bg, bold: prev.bold}
}

// SetStyled is the full-featured single-cell write — fg + bg + bold.
func (g *Grid) SetStyled(x, y int, r rune, fg, bg lipgloss.Color, bold bool) {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return
	}
	g.cells[y][x] = cell{r: r, fg: fg, bg: bg, bold: bold}
}

// SetStr writes a string left-to-right starting at (x,y) in the given fg.
// Wide runes count as one cell (no double-width support — netmap content
// is plain ASCII + box-drawing only).
func (g *Grid) SetStr(x, y int, s string, fg lipgloss.Color) {
	for i, r := range s {
		g.Set(x+i, y, r, fg)
	}
}

// SetStrStyled writes a string with fg + bg + bold attributes. Use this
// for callout text like the "this device" status row where you want the
// styling to actually stand out against neighboring plain cells.
func (g *Grid) SetStrStyled(x, y int, s string, fg, bg lipgloss.Color, bold bool) {
	for i, r := range s {
		g.SetStyled(x+i, y, r, fg, bg, bold)
	}
}

// FillBg paints a background color across a rectangle without changing
// the existing characters or fg — useful for highlighting a row.
func (g *Grid) FillBg(x, y, w, h int, bg lipgloss.Color) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if xx < 0 || yy < 0 || xx >= g.w || yy >= g.h {
				continue
			}
			c := g.cells[yy][xx]
			c.bg = bg
			g.cells[yy][xx] = c
		}
	}
}

// HLine draws a horizontal line from (x1,y) to (x2,y) using ─. Existing
// junction characters at the endpoints are preserved correctly when
// composed with VLine via the Junction helper below.
func (g *Grid) HLine(x1, x2, y int, fg lipgloss.Color) {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	for x := x1; x <= x2; x++ {
		g.Set(x, y, '─', fg)
	}
}

// VLine draws a vertical line from (x,y1) to (x,y2) using │.
func (g *Grid) VLine(x, y1, y2 int, fg lipgloss.Color) {
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	for y := y1; y <= y2; y++ {
		g.Set(x, y, '│', fg)
	}
}

// Box draws a rectangle outline. Title is rendered centered on the top
// border with a single-cell padding on each side. Inner content is the
// caller's responsibility (use SetStr inside the box).
func (g *Grid) Box(x, y, w, h int, fg lipgloss.Color, title string) {
	if w < 2 || h < 2 {
		return
	}
	// Corners + edges
	g.Set(x, y, '┌', fg)
	g.Set(x+w-1, y, '┐', fg)
	g.Set(x, y+h-1, '└', fg)
	g.Set(x+w-1, y+h-1, '┘', fg)
	g.HLine(x+1, x+w-2, y, fg)
	g.HLine(x+1, x+w-2, y+h-1, fg)
	g.VLine(x, y+1, y+h-2, fg)
	g.VLine(x+w-1, y+1, y+h-2, fg)
	if title != "" {
		titleStr := " " + title + " "
		tx := x + (w-len(titleStr))/2
		if tx < x+1 {
			tx = x + 1
		}
		g.SetStr(tx, y, titleStr, fg)
	}
}

// Junction places the smart T/cross character at (x,y) given which sides
// are connected. Used at points where a vertical bus meets a horizontal
// bus or vice versa.
func (g *Grid) Junction(x, y int, up, down, left, right bool, fg lipgloss.Color) {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return
	}
	var r rune
	switch {
	case up && down && left && right:
		r = '┼'
	case up && down && right:
		r = '├'
	case up && down && left:
		r = '┤'
	case left && right && down:
		r = '┬'
	case left && right && up:
		r = '┴'
	case down && right:
		r = '┌'
	case down && left:
		r = '┐'
	case up && right:
		r = '└'
	case up && left:
		r = '┘'
	case up && down:
		r = '│'
	case left && right:
		r = '─'
	default:
		return
	}
	g.Set(x, y, r, fg)
}

// Render produces the final string with per-cell styling applied via
// lipgloss. Adjacent cells with identical fg+bg+bold attributes are
// collapsed into a single styled span so the output isn't drowned in
// escape codes.
func (g *Grid) Render() string {
	var b strings.Builder
	type style struct {
		fg, bg lipgloss.Color
		bold   bool
	}
	for y := 0; y < g.h; y++ {
		var span strings.Builder
		var cur style
		flush := func() {
			if span.Len() == 0 {
				return
			}
			s := lipgloss.NewStyle()
			if cur.fg != "" {
				s = s.Foreground(cur.fg)
			}
			if cur.bg != "" {
				s = s.Background(cur.bg)
			}
			if cur.bold {
				s = s.Bold(true)
			}
			b.WriteString(s.Render(span.String()))
			span.Reset()
		}
		for x := 0; x < g.w; x++ {
			c := g.cells[y][x]
			st := style{fg: c.fg, bg: c.bg, bold: c.bold}
			if st != cur {
				flush()
				cur = st
			}
			span.WriteRune(c.r)
		}
		flush()
		if y < g.h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
