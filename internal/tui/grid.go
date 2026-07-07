package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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
	// editCaretStyle is the block caret inside the edit field: a light block (the
	// char under it stays dark and readable) that reads as a cursor without
	// inserting an extra column the way a bar glyph does.
	editCaretStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("15"))
)

type column struct {
	name  string
	width int
	fk    bool // part of a foreign key → header carries the fkMarker
}

// fkMarker is the one-char suffix flagging a foreign-key column in the header.
const fkMarker = "→"

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

	pk  []string        // PK column names (from the ResultSet), for keyed edits (§8)
	fks []db.ForeignKey // foreign keys on the current table (header marker + follow)

	filters   map[int]string // colIndex → committed LIKE pattern (server-side)
	filtering int            // colIndex being edited, or -1
	filterVal string         // in-progress filter text

	// Quick-path cell edit (§8): a single-line overlay on the cursor cell.
	editing      bool
	editVal      string // in-progress text
	editPos      int    // cursor position within editVal, as a rune index
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
	} else {
		g.table = db.TableRef{} // ad-hoc query result: no provenance → not editable
	}
	g.pk = rs.PK
	g.fks = rs.FKs
	isFK := map[string]bool{}
	for _, fk := range g.fks {
		for _, c := range fk.Columns {
			isFK[c] = true
		}
	}
	g.cols = make([]column, len(rs.Cols))
	for i, name := range rs.Cols {
		w := colWidthFor(name, rs.Rows, i)
		if isFK[name] {
			w += runewidth.StringWidth(fkMarker) // room for the marker
		}
		g.cols[i] = column{name: name, width: w, fk: isFK[name]}
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

// gridPos is the grid's cursor + scroll position — enough to land a jump exactly
// where it left off (captured into a jumplist viewState).
type gridPos struct{ cursorR, cursorC, rowOff, colOff int }

func (g *grid) pos() gridPos { return gridPos{g.cursorR, g.cursorC, g.rowOff, g.colOff} }

// setPos restores a saved position, clamped to the loaded bounds and nudged so
// the cursor stays on screen (the saved rowOff/colOff may not fit if fewer rows
// loaded this time). Used both by the cache restore and the reload-then-reposition
// path in the jumplist.
func (g *grid) setPos(p gridPos) {
	g.cursorR = clamp(p.cursorR, 0, max(len(g.visible)-1, 0))
	g.cursorC = clamp(p.cursorC, 0, max(len(g.cols)-1, 0))
	g.rowOff = clamp(p.rowOff, 0, g.cursorR)
	if vr := g.visibleRows(); g.cursorR >= g.rowOff+vr {
		g.rowOff = g.cursorR - vr + 1
	}
	g.colOff = clamp(p.colOff, 0, g.cursorC)
	g.ensureColVisible()
}

// gridSnapshot is a value copy of the grid's loaded result + view state, cached
// on a jumplist entry so returning to it is instant (no DB round-trip). rows/cols
// are shared by reference (an in-place edit stays reflected in the cache); the
// mutated-in-place slices (visible) and the filters map are deep-copied so the
// live grid can't corrupt a stored snapshot.
type gridSnapshot struct {
	cols                             []column
	rows                             [][]any
	table                            db.TableRef
	visible                          []int
	pk                               []string
	fks                              []db.ForeignKey
	filters                          map[int]string
	sortCol                          string
	sortAsc                          bool
	hasMore                          bool
	cursorR, cursorC, rowOff, colOff int
}

func (g *grid) snapshot() *gridSnapshot {
	filters := make(map[int]string, len(g.filters))
	for k, v := range g.filters {
		filters[k] = v
	}
	return &gridSnapshot{
		cols:    g.cols,
		rows:    g.rows,
		table:   g.table,
		visible: append([]int(nil), g.visible...),
		pk:      g.pk,
		fks:     g.fks,
		filters: filters,
		sortCol: g.sortCol,
		sortAsc: g.sortAsc,
		hasMore: g.hasMore,
		cursorR: g.cursorR, cursorC: g.cursorC, rowOff: g.rowOff, colOff: g.colOff,
	}
}

// restore loads a snapshot back into the grid, deep-copying the slices/map the
// grid mutates in place so the cached snapshot stays intact. It also clears the
// transient edit/filter-typing state (a jump never lands mid-edit).
func (g *grid) restore(s *gridSnapshot) {
	g.cols, g.rows, g.table = s.cols, s.rows, s.table
	g.visible = append([]int(nil), s.visible...)
	g.pk, g.fks = s.pk, s.fks
	g.filters = make(map[int]string, len(s.filters))
	for k, v := range s.filters {
		g.filters[k] = v
	}
	g.sortCol, g.sortAsc, g.hasMore = s.sortCol, s.sortAsc, s.hasMore
	g.cursorR, g.cursorC, g.rowOff, g.colOff = s.cursorR, s.cursorC, s.rowOff, s.colOff
	g.filtering, g.filterVal = -1, ""
	g.editing, g.editVal, g.editDirty, g.loading = false, "", false, false
}

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
// When null is set the cell is set to SQL NULL (val is ignored / bound as nil).
// rowIdx/colIdx pin the target cell (index into grid.rows and grid.cols) so the
// in-memory write-back lands on the right cell even if the cursor moved or a new
// edit started while this one was in flight — never on live grid.editR/editC.
type editReq struct {
	table  db.TableRef
	col    string
	val    string
	null   bool
	keys   []keyPred
	rowIdx int // index into grid.rows (underlying, not the visible order)
	colIdx int // index into grid.cols
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
	// Open with the caret on the last character (0 for an empty / NULL cell).
	if n := len([]rune(g.editVal)); n > 0 {
		g.editPos = n - 1
	} else {
		g.editPos = 0
	}
	return true
}

// editInput inserts s at the cursor and advances past it.
func (g *grid) editInput(s string) {
	r, ins := []rune(g.editVal), []rune(s)
	r = append(r[:g.editPos:g.editPos], append(ins, r[g.editPos:]...)...)
	g.editVal = string(r)
	g.editPos += len(ins)
	g.editDirty = true
}

func (g *grid) cancelEdit() {
	g.editing, g.editVal, g.editDirty, g.editPos = false, "", false, 0
}

// editBackspace deletes the rune before the cursor.
func (g *grid) editBackspace() {
	if g.editPos > 0 {
		r := []rune(g.editVal)
		r = append(r[:g.editPos-1], r[g.editPos:]...)
		g.editVal = string(r)
		g.editPos--
	}
	g.editDirty = true
}

// editDelete removes the rune at the caret (forward delete / Del); a no-op at
// end-of-text. The caret stays put.
func (g *grid) editDelete() {
	r := []rune(g.editVal)
	if g.editPos < len(r) {
		r = append(r[:g.editPos], r[g.editPos+1:]...)
		g.editVal = string(r)
	}
	g.editDirty = true
}

// editLeft / editRight move the cursor within the value; editHome / editEnd jump
// to the ends. Bounded to [0, len].
func (g *grid) editLeft() {
	if g.editPos > 0 {
		g.editPos--
	}
}

func (g *grid) editRight() {
	if g.editPos < len([]rune(g.editVal)) {
		g.editPos++
	}
}

func (g *grid) editHome() { g.editPos = 0 }
func (g *grid) editEnd()  { g.editPos = len([]rune(g.editVal)) }

// editDeleteWord deletes from the start of the word before the cursor up to the
// cursor (Ctrl-W): trailing spaces first, then the run of non-spaces.
func (g *grid) editDeleteWord() {
	r := []rune(g.editVal)
	i := g.editPos
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	r = append(r[:i], r[g.editPos:]...)
	g.editVal = string(r)
	g.editPos = i
	g.editDirty = true
}

// commitEdit ends edit mode and returns the update to run. ok is false when
// nothing should run: a bare Enter with no typing (untouched cell, including an
// untouched NULL which stays NULL — §8), or the row can't be keyed.
func (g *grid) commitEdit() (editReq, bool) {
	val, dirty := g.editVal, g.editDirty
	r, c := g.editR, g.editC
	g.cancelEdit()
	if !dirty {
		return editReq{}, false
	}
	if r < 0 || r >= len(g.visible) {
		return editReq{}, false
	}
	keys, ok := g.keyPredsAt(r)
	if !ok {
		return editReq{}, false
	}
	req := editReq{table: g.table, col: g.cols[c].name, val: val, keys: keys, rowIdx: g.visible[r], colIdx: c}
	// Typing exactly NULL (uppercase) sets SQL NULL, not the string "NULL" — the
	// only way to null a cell on the quick path (the string "NULL" needs E).
	if val == "NULL" {
		req.null, req.val = true, ""
	}
	return req, true
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
	if !g.editable() {
		return nil, false
	}
	return g.currentRowMap()
}

// fkFor returns the foreign key that column col is part of, if any (from the
// FKs fetched at load time — no query on follow).
func (g *grid) fkFor(col string) (db.ForeignKey, bool) {
	for _, fk := range g.fks {
		for _, c := range fk.Columns {
			if c == col {
				return fk, true
			}
		}
	}
	return db.ForeignKey{}, false
}

// currentRowMap returns the cursor row as a column→value map, with no editability
// requirement — used to follow a foreign key from any table.
func (g *grid) currentRowMap() (map[string]any, bool) {
	if g.cursorR >= len(g.visible) {
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

// yankCell returns the cursor cell's value as plain text for the y yank — the
// raw driver value, without the display substitutions cellString makes. NULL
// yanks as an empty string. ok is false when there's no cell under the cursor.
func (g *grid) yankCell() (string, bool) {
	v, _, ok := g.currentCell()
	if !ok {
		return "", false
	}
	return valueText(v), true
}

// currentRowJSON renders the cursor row as a JSON object for the Y yank, keeping
// the column order (a plain map would sort the keys). ok is false when there's
// no row under the cursor.
func (g *grid) currentRowJSON() (string, bool) {
	if g.cursorR >= len(g.visible) {
		return "", false
	}
	row := g.rows[g.visible[g.cursorR]]
	var compact strings.Builder
	compact.WriteByte('{')
	for i, c := range g.cols {
		if i > 0 {
			compact.WriteByte(',')
		}
		key, _ := json.Marshal(c.name)
		compact.Write(key)
		compact.WriteByte(':')
		var v any
		if i < len(row) {
			v = row[i]
		}
		if bs, ok := v.([]byte); ok {
			v = string(bs)
		}
		val, err := json.Marshal(v)
		if err != nil {
			val, _ = json.Marshal(valueText(v))
		}
		compact.Write(val)
	}
	compact.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(compact.String()), "", "  "); err != nil {
		return compact.String(), true
	}
	return out.String(), true
}

// valueText is the plain-text form of a driver value for yanking: the raw string
// for []byte, empty for NULL, else fmt's default. No newline/tab substitution.
func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// applyEdit writes the committed value back into the in-memory row so the grid
// reflects the change immediately, without a server round-trip. rowIdx/colIdx
// pin the exact cell (from the editReq), so a cursor move or a fresh edit started
// while the save was in flight can't misdirect the write-back. The typed value
// keeps the prior cell's driver type when it parses cleanly (coerceLike), so an
// edited cell — e.g. an integer PK — stays the same type for later keyed edits.
func (g *grid) applyEdit(rowIdx, colIdx int, val string) {
	if row, ok := g.cellAt(rowIdx, colIdx); ok {
		row[colIdx] = coerceLike(row[colIdx], val)
	}
}

// applyEditNull writes SQL NULL into the just-edited cell (in-memory), so it
// renders as faint NULL immediately without a reload.
func (g *grid) applyEditNull(rowIdx, colIdx int) {
	if row, ok := g.cellAt(rowIdx, colIdx); ok {
		row[colIdx] = nil
	}
}

// cellAt returns the underlying row backing (rowIdx, colIdx) if both indexes are
// in bounds, so the write-back helpers can share the guard.
func (g *grid) cellAt(rowIdx, colIdx int) ([]any, bool) {
	if rowIdx < 0 || rowIdx >= len(g.rows) {
		return nil, false
	}
	row := g.rows[rowIdx]
	if colIdx < 0 || colIdx >= len(row) {
		return nil, false
	}
	return row, true
}

// coerceLike converts the edited string back to prev's type when it parses
// cleanly, so an edited cell keeps the driver type it had rather than turning
// into a string. Falls back to the raw string (and to string for a prior NULL).
func coerceLike(prev any, val string) any {
	switch prev.(type) {
	case int, int8, int16, int32, int64:
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			return n
		}
	case uint, uint8, uint16, uint32, uint64:
		if n, err := strconv.ParseUint(val, 10, 64); err == nil {
			return n
		}
	case float32, float64:
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	case bool:
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return val
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

// renderEditCell renders the edit field of width w with a block caret at rune
// index pos: the character under the caret (or a trailing space at end-of-text)
// is drawn inverted, and the rest of the field keeps editStyle. The caret
// overlays a cell rather than inserting one, so moving it never shifts the text
// or shows a phantom space. w is assumed ≥ value width + 1 (see effWidth).
func renderEditCell(val string, pos, w int) string {
	r := []rune(val)
	pos = clamp(pos, 0, len(r))
	before := string(r[:pos])
	caret, after := " ", ""
	if pos < len(r) {
		caret, after = string(r[pos]), string(r[pos+1:])
	}
	pad := w - runewidth.StringWidth(before) - runewidth.StringWidth(caret) - runewidth.StringWidth(after)
	if pad < 0 {
		pad = 0
	}
	return editStyle.Render(before) + editCaretStyle.Render(caret) +
		editStyle.Render(after+strings.Repeat(" ", pad))
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
			} else if g.cols[c].fk {
				cell += fkMarker // e.g. author_id→
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
		w := g.effWidth(c)
		if isEdit {
			// Block caret overlays a cell instead of inserting a bar glyph.
			b.WriteString(renderEditCell(g.editVal, g.editPos, w))
			b.WriteString(strings.Repeat(" ", colGap))
			x += w + colGap
			if x >= g.w {
				break
			}
			continue
		}
		if c < len(row) {
			v := row[c]
			isNull = v == nil
			cell = cellString(v)
		}
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
