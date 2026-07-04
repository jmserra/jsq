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
	editStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3"))
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

	pk []string // PK column names (from the ResultSet), for keyed edits (§8)

	filters   map[int]string // colIndex → committed LIKE pattern (server-side)
	filtering int            // colIndex being edited, or -1
	filterVal string         // in-progress filter text

	// Quick-path cell edit (§8): a single-line overlay on the cursor cell.
	editing      bool
	editVal      string // in-progress text
	editR, editC int    // visible-row / column index being edited
	editOrigNull bool   // the original cell was SQL NULL
	editDirty    bool   // the user typed at least one key

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
	g.pk = rs.PK
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
	if g.editing && c == g.editC {
		if need := runewidth.StringWidth(g.editVal) + 1; need > w {
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

// --- quick-path cell edit (§8) ---

// keyPred is one "col = val" predicate for a keyed UPDATE's WHERE.
type keyPred struct {
	col string
	val any
}

// editReq is a resolved single-cell update: SET col = val WHERE <full PK>.
type editReq struct {
	table db.TableRef
	col   string
	val   string
	keys  []keyPred
}

// colIndex returns the display index of the named column, or -1.
func (g *grid) colIndex(name string) int {
	for i, c := range g.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// editable reports whether the grid can be edited: rows came from a single-table
// select with a resolved PK, and every PK column is present in the result (§8).
func (g *grid) editable() bool {
	if g.table.Name == "" || len(g.pk) == 0 {
		return false
	}
	for _, name := range g.pk {
		if g.colIndex(name) < 0 {
			return false
		}
	}
	return true
}

// startEdit opens the inline overlay on the cursor cell, pre-filled with its
// current value (empty for a NULL cell). Returns false if editing isn't possible.
func (g *grid) startEdit() bool {
	if !g.editable() || g.cursorR >= len(g.visible) || g.cursorC >= len(g.cols) {
		return false
	}
	var v any
	if row := g.rows[g.visible[g.cursorR]]; g.cursorC < len(row) {
		v = row[g.cursorC]
	}
	g.editing = true
	g.editR, g.editC = g.cursorR, g.cursorC
	g.editOrigNull = v == nil
	g.editDirty = false
	if v == nil {
		g.editVal = ""
	} else {
		g.editVal = fmt.Sprintf("%v", v)
	}
	return true
}

func (g *grid) editInput(s string) { g.editVal += s; g.editDirty = true }
func (g *grid) cancelEdit()        { g.editing, g.editVal, g.editDirty = false, "", false }

func (g *grid) editBackspace() {
	if r := []rune(g.editVal); len(r) > 0 {
		g.editVal = string(r[:len(r)-1])
	}
	g.editDirty = true
}

// commitEdit ends edit mode and returns the update to run. ok is false when
// nothing should run: a bare Enter with no typing (untouched cell, including an
// untouched NULL which stays NULL — §8), or the row can't be keyed.
func (g *grid) commitEdit() (editReq, bool) {
	val, dirty := g.editVal, g.editDirty
	g.cancelEdit()
	if !dirty {
		return editReq{}, false
	}
	keys, ok := g.keyPreds()
	if !ok {
		return editReq{}, false
	}
	return editReq{table: g.table, col: g.cols[g.editC].name, val: val, keys: keys}, true
}

// keyPreds builds the WHERE predicates from the edited row's PK values (quick
// path). Returns false if any PK column is missing or NULL.
func (g *grid) keyPreds() ([]keyPred, bool) { return g.keyPredsAt(g.editR) }

// keyPredsAt builds the WHERE predicates from the PK values of the given
// visible-row index. Returns false if any PK column is missing or NULL (can't
// safely key the statement).
func (g *grid) keyPredsAt(vr int) ([]keyPred, bool) {
	if len(g.pk) == 0 || vr < 0 || vr >= len(g.visible) {
		return nil, false
	}
	row := g.rows[g.visible[vr]]
	preds := make([]keyPred, 0, len(g.pk))
	for _, name := range g.pk {
		ci := g.colIndex(name)
		if ci < 0 || ci >= len(row) || row[ci] == nil {
			return nil, false
		}
		preds = append(preds, keyPred{col: name, val: row[ci]})
	}
	return preds, true
}

// fullEditTarget returns the current cell's column, value, and the full-PK
// predicates for the E full-path UPDATE. Unlike startEdit it opens no overlay —
// E builds SQL and hands off to $EDITOR. ok is false when the grid isn't
// editable or the row can't be keyed.
func (g *grid) fullEditTarget() (col string, val any, keys []keyPred, ok bool) {
	if !g.editable() || g.cursorR >= len(g.visible) || g.cursorC >= len(g.cols) {
		return "", nil, nil, false
	}
	keys, ok = g.keyPredsAt(g.cursorR)
	if !ok {
		return "", nil, nil, false
	}
	if row := g.rows[g.visible[g.cursorR]]; g.cursorC < len(row) {
		val = row[g.cursorC]
	}
	return g.cols[g.cursorC].name, val, keys, true
}

// rowKeys returns the full-PK predicates of the row under the cursor, for the D
// full-path DELETE. ok is false when the grid isn't editable or can't be keyed.
func (g *grid) rowKeys() ([]keyPred, bool) {
	if !g.editable() || g.cursorR >= len(g.visible) {
		return nil, false
	}
	return g.keyPredsAt(g.cursorR)
}

// currentRowValues returns the row under the cursor keyed by column name, for the
// p full-path duplicate. ok is false when the grid isn't editable or has no row.
func (g *grid) currentRowValues() (map[string]any, bool) {
	if !g.editable() || g.cursorR >= len(g.visible) {
		return nil, false
	}
	row := g.rows[g.visible[g.cursorR]]
	vals := make(map[string]any, len(g.cols))
	for i, c := range g.cols {
		if i < len(row) {
			vals[c.name] = row[i]
		}
	}
	return vals, true
}

// applyEdit writes the committed value back into the in-memory row so the grid
// reflects the change immediately, without a server round-trip.
func (g *grid) applyEdit(val string) {
	if g.editR < len(g.visible) {
		row := g.rows[g.visible[g.editR]]
		if g.editC < len(row) {
			row[g.editC] = val
		}
	}
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
		isEdit := g.editing && r == g.editR && c == g.editC
		if isEdit {
			cell = g.editVal + "▏"
		} else if c < len(row) {
			v := row[c]
			isNull = v == nil
			cell = cellString(v)
		}
		w := g.effWidth(c)
		cell = runewidth.FillRight(runewidth.Truncate(cell, w, "…"), w)
		switch {
		case isEdit:
			cell = editStyle.Render(cell)
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
