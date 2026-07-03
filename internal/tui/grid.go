package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

const (
	minColWidth     = 4
	maxColWidth     = 40
	colGap          = 1
	sortMarkerWidth = 2 // reserved for the "▲ " / "▼ " header prefix
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	filterStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	nullStyle   = lipgloss.NewStyle().Faint(true)
	selStyle    = lipgloss.NewStyle().Reverse(true)
)

type column struct {
	name  string
	width int
}

// filterSpec is one committed column filter, for building the server WHERE.
type filterSpec struct{ col, pattern string }

// grid is the fixed-width results grid (§7). Supports sort (J/K) and the
// two-phase column filter (§7.1): live client-side preview + server commit.
type grid struct {
	cols  []column
	rows  [][]any
	table db.TableRef

	visible []int // display order → rows index (client-side filter preview)

	cursorR, cursorC int
	rowOff, colOff   int
	w, h             int

	sortCol string
	sortAsc bool

	filters   map[int]string // colIndex → committed LIKE pattern (server-side)
	filtering int            // colIndex being edited, or -1
	filterVal string         // in-progress filter text

	hasMore bool // last window came back full → more rows may exist
	loading bool // a continuous-scroll fetch is in flight
}

// appendRows extends the buffer with the next window (continuous scroll).
func (g *grid) appendRows(rows [][]any, hasMore bool) {
	g.rows = append(g.rows, rows...)
	g.hasMore = hasMore
	g.loading = false
	g.rebuildVisible()
}

// wantMore reports whether the cursor is near the loaded edge and more rows
// exist — the trigger to fetch the next window. Never during a filter edit.
func (g *grid) wantMore() bool {
	if !g.hasMore || g.loading || g.filtering >= 0 {
		return false
	}
	return g.cursorR >= len(g.visible)-g.visibleRows()
}

func newGrid() grid {
	return grid{filtering: -1, filters: map[int]string{}}
}

func (g *grid) setResult(rs *db.ResultSet) {
	g.rows = rs.Rows
	if rs.Table != nil {
		g.table = *rs.Table
	}
	g.cols = make([]column, len(rs.Cols))
	for i, name := range rs.Cols {
		g.cols[i] = column{name: name, width: colWidthFor(name, rs.Rows, i)}
	}
	g.rebuildVisible()
	// Preserve cursor position across a re-sort/re-filter; clamp to bounds.
	g.cursorR = clamp(g.cursorR, 0, len(g.visible)-1)
	g.cursorC = clamp(g.cursorC, 0, len(g.cols)-1)
	if g.rowOff > g.cursorR {
		g.rowOff = g.cursorR
	}
	if g.colOff > g.cursorC {
		g.colOff = g.cursorC
	}
	g.ensureColVisible()
}

// reset returns the cursor to the top-left; used when loading a different table.
func (g *grid) reset() { g.cursorR, g.cursorC, g.rowOff, g.colOff = 0, 0, 0, 0 }

func (g *grid) setSize(w, h int)             { g.w, g.h = w, h }
func (g *grid) setSort(col string, asc bool) { g.sortCol, g.sortAsc = col, asc }

func (g *grid) rebuildVisible() {
	g.visible = g.visible[:0]
	for i := range g.rows {
		g.visible = append(g.visible, i)
	}
}

// --- cursor / navigation ---

func (g *grid) currentColName() (string, bool) {
	if g.cursorC >= 0 && g.cursorC < len(g.cols) {
		return g.cols[g.cursorC].name, true
	}
	return "", false
}

func (g *grid) currentCell() (any, string, bool) {
	if g.cursorR < len(g.visible) && g.cursorC < len(g.cols) {
		row := g.rows[g.visible[g.cursorR]]
		if g.cursorC < len(row) {
			return row[g.cursorC], g.cols[g.cursorC].name, true
		}
	}
	return nil, "", false
}

func (g *grid) visibleRows() int {
	if n := g.h - 1; n > 0 { // header line
		return n
	}
	return 1
}

func (g *grid) moveRow(d int) {
	g.cursorR = clamp(g.cursorR+d, 0, len(g.visible)-1)
	vr := g.visibleRows()
	if g.cursorR < g.rowOff {
		g.rowOff = g.cursorR
	}
	if g.cursorR >= g.rowOff+vr {
		g.rowOff = g.cursorR - vr + 1
	}
}

func (g *grid) moveCol(d int) {
	g.cursorC = clamp(g.cursorC+d, 0, len(g.cols)-1)
	g.ensureColVisible()
}

func (g *grid) top()      { g.cursorR, g.rowOff = 0, 0 }
func (g *grid) bottom()   { g.moveRow(len(g.visible)) }
func (g *grid) firstCol() { g.cursorC, g.colOff = 0, 0 }
func (g *grid) lastCol()  { g.cursorC = len(g.cols) - 1; g.ensureColVisible() }

func (g *grid) ensureColVisible() {
	if g.cursorC < g.colOff {
		g.colOff = g.cursorC
		return
	}
	for g.colOff < g.cursorC {
		w := 0
		for c := g.colOff; c <= g.cursorC; c++ {
			w += g.effWidth(c) + colGap
		}
		if w <= g.w {
			break
		}
		g.colOff++
	}
}

// effWidth is the column's render width, expanded while its filter is being
// typed so the whole "⌕pattern" plus cursor stays visible (+1 slack).
func (g *grid) effWidth(c int) int {
	w := g.cols[c].width
	if c == g.filtering {
		if need := runewidth.StringWidth("⌕"+g.filterVal) + 1; need > w {
			return need
		}
	}
	return w
}

// --- filtering (§7.1) ---

func (g *grid) startFilter() {
	if len(g.cols) == 0 {
		return
	}
	g.filtering = g.cursorC
	g.filterVal = g.filters[g.cursorC] // pre-fill any existing pattern
	g.applyPreview()
}

func (g *grid) filterInput(s string) { g.filterVal += s; g.applyPreview() }

func (g *grid) filterBackspace() {
	if r := []rune(g.filterVal); len(r) > 0 {
		g.filterVal = string(r[:len(r)-1])
		g.applyPreview()
	}
}

// commitFilter stores the in-progress pattern (empty clears it) and ends editing.
func (g *grid) commitFilter() {
	if g.filterVal == "" {
		delete(g.filters, g.filtering)
	} else {
		g.filters[g.filtering] = g.filterVal
	}
	g.endEdit()
}

// clearFilter removes the edited column's filter and ends editing.
func (g *grid) clearFilter() {
	delete(g.filters, g.filtering)
	g.endEdit()
}

func (g *grid) endEdit() {
	g.filtering, g.filterVal = -1, ""
	g.rebuildVisible()
	g.cursorR = clamp(g.cursorR, 0, len(g.visible)-1)
}

func (g *grid) clearFilters() {
	g.filters = map[int]string{}
	g.filtering, g.filterVal = -1, ""
}

// clearCurrentFilter clears the committed filter on the cursor's column (if any)
// while navigating; returns true if something was cleared (caller reloads).
func (g *grid) clearCurrentFilter() bool {
	if _, ok := g.filters[g.cursorC]; !ok {
		return false
	}
	delete(g.filters, g.cursorC)
	g.rebuildVisible()
	g.cursorR = clamp(g.cursorR, 0, len(g.visible)-1)
	return true
}

func (g *grid) filterSpecs() []filterSpec {
	out := make([]filterSpec, 0, len(g.filters))
	for ci, pat := range g.filters {
		out = append(out, filterSpec{col: g.cols[ci].name, pattern: searchPattern(pat)})
	}
	return out
}

// searchPattern turns the raw typed text into the LIKE pattern: a trailing % is
// always appended (prefix search); a leading % is present only if you typed one.
func searchPattern(v string) string {
	if v == "" {
		return ""
	}
	if strings.HasSuffix(v, "%") {
		return v
	}
	return v + "%"
}

// applyPreview narrows the loaded rows client-side by the in-progress pattern,
// using the same LIKE semantics as the eventual server query.
func (g *grid) applyPreview() {
	g.visible = g.visible[:0]
	pat := searchPattern(g.filterVal)
	re := likeToRegex(pat)
	for i := range g.rows {
		if pat == "" {
			g.visible = append(g.visible, i)
			continue
		}
		var cell string
		if g.filtering < len(g.rows[i]) {
			cell = cellString(g.rows[i][g.filtering])
		}
		if re == nil || re.MatchString(cell) {
			g.visible = append(g.visible, i)
		}
	}
	g.cursorR = clamp(g.cursorR, 0, len(g.visible)-1)
	if g.rowOff > g.cursorR {
		g.rowOff = g.cursorR
	}
}

// likeToRegex converts a SQL LIKE pattern (%,_ wildcards) to a case-insensitive
// anchored regex for the client-side preview.
func likeToRegex(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?is)^")
	for _, r := range p {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// --- rendering ---

func colWidthFor(name string, rows [][]any, i int) int {
	// Data width is capped at maxColWidth; the name (plus the sort-marker slot)
	// always fits so the column header is never truncated and never resizes when
	// you sort it.
	nameW := runewidth.StringWidth(name) + sortMarkerWidth
	dataW := 0
	for r := 0; r < len(rows) && r < 200; r++ {
		if i < len(rows[r]) {
			if cw := runewidth.StringWidth(cellString(rows[r][i])); cw > dataW {
				dataW = cw
			}
		}
	}
	if dataW > maxColWidth {
		dataW = maxColWidth
	}
	return max(minColWidth, max(nameW, dataW))
}

func cellString(v any) string {
	if v == nil {
		return "NULL"
	}
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "\n", "↵")
	s = strings.ReplaceAll(s, "\t", "→")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

func (g *grid) View() string {
	if len(g.cols) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(g.renderHeader())
	vr := g.visibleRows()
	for r := g.rowOff; r < g.rowOff+vr && r < len(g.visible); r++ {
		b.WriteByte('\n')
		b.WriteString(g.renderRow(r))
	}
	return b.String()
}

func (g *grid) renderHeader() string {
	var b strings.Builder
	x := 0
	for c := g.colOff; c < len(g.cols); c++ {
		var cell string
		style := headerStyle
		switch {
		case g.filtering == c:
			cell = "⌕" + g.filterVal + "▏"
			style = filterStyle
		default:
			cell = g.cols[c].name
			if pat, ok := g.filters[c]; ok {
				cell = "⌕" + pat
				style = filterStyle
			}
			if g.sortCol != "" && g.cols[c].name == g.sortCol {
				if g.sortAsc {
					cell = "▲ " + cell
				} else {
					cell = "▼ " + cell
				}
			}
		}
		w := g.effWidth(c)
		cell = runewidth.FillRight(runewidth.Truncate(cell, w, "…"), w)
		b.WriteString(style.Render(cell))
		b.WriteString(strings.Repeat(" ", colGap))
		x += w + colGap
		if x >= g.w {
			break
		}
	}
	return b.String()
}

func (g *grid) renderRow(r int) string {
	row := g.rows[g.visible[r]]
	var b strings.Builder
	x := 0
	for c := g.colOff; c < len(g.cols); c++ {
		var cell string
		isNull := false
		if c < len(row) {
			v := row[c]
			isNull = v == nil
			cell = cellString(v)
		}
		w := g.effWidth(c)
		cell = runewidth.FillRight(runewidth.Truncate(cell, w, "…"), w)
		switch {
		case r == g.cursorR && c == g.cursorC:
			cell = selStyle.Render(cell)
		case isNull:
			cell = nullStyle.Render(cell)
		}
		b.WriteString(cell)
		b.WriteString(strings.Repeat(" ", colGap))
		x += w + colGap
		if x >= g.w {
			break
		}
	}
	return b.String()
}
