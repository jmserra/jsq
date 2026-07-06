package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// sidebar is the full-screen table-list buffer (its own screen, screenTables).
// There is no explicit filter mode: typing narrows the list as you go (same LIKE
// semantics as the grid column filter §7.1 — trailing % implied, case-insensitive),
// a purely client-side narrowing of the already-loaded names.
type sidebar struct {
	tables  []db.Table
	visible []int // display order → tables index (filter view)
	cursor  int   // index into visible
	off     int
	w, h    int

	filterVal string // current filter text (empty → whole list)
	label     string // placeholder shown in the search prompt ("tables" / "databases")
}

func (s *sidebar) setTables(t []db.Table) {
	s.tables, s.cursor, s.off = t, 0, 0
	s.filterVal = ""
	s.rebuildVisible()
}

// tableLabel is the schema-qualified display name; a "public." prefix is noise
// so it's dropped. Used for both rendering and filter matching.
func tableLabel(t db.Table) string {
	if sch := t.Schema; sch != "" && sch != "public" {
		return sch + "." + t.Name
	}
	return t.Name
}

// The list lays out in a column-major grid — items fill down each column then
// across, so ↑/↓ (index ∓1) move naturally within a column and ←/→ (index ∓rows)
// jump columns. Columns are sized to the widest visible name so nothing is cut;
// `off` is the first visible column (horizontal scroll for very long lists).

const listColGap = 2 // spaces between grid columns

// rebuildVisible narrows `visible` to tables whose label matches the filter.
// Empty filter shows all tables. Callers reset cursor/off before calling.
func (s *sidebar) rebuildVisible() {
	s.visible = s.visible[:0]
	pat := searchPattern(s.filterVal)
	re := likeToRegex(pat)
	for i := range s.tables {
		if pat == "" || re == nil || re.MatchString(tableLabel(s.tables[i])) {
			s.visible = append(s.visible, i)
		}
	}
	s.cursor = clamp(s.cursor, 0, len(s.visible)-1)
}

// hasFilter reports whether a filter is narrowing the list.
func (s *sidebar) hasFilter() bool { return s.filterVal != "" }

// rows is the grid height — one line is always taken by the search prompt.
func (s *sidebar) rows() int {
	if s.h > 1 {
		return s.h - 1
	}
	return 1
}

// cellWidth is a grid cell's width: the widest visible label + the 1-char cursor
// marker, capped at the screen width.
func (s *sidebar) cellWidth() int {
	m := 0
	for _, i := range s.visible {
		if w := runewidth.StringWidth(tableLabel(s.tables[i])); w > m {
			m = w
		}
	}
	w := m + 1
	if s.w > 0 && w > s.w {
		w = s.w
	}
	if w < 1 {
		w = 1
	}
	return w
}

// gridCols is how many columns fit across the screen.
func (s *sidebar) gridCols() int {
	if n := (s.w + listColGap) / (s.cellWidth() + listColGap); n > 1 {
		return n
	}
	return 1
}

func (s *sidebar) move(d int) {
	s.cursor = clamp(s.cursor+d, 0, len(s.visible)-1)
	// Keep the cursor's column within the visible window (horizontal scroll).
	col := s.cursor / s.rows()
	nc := s.gridCols()
	if col < s.off {
		s.off = col
	}
	if col >= s.off+nc {
		s.off = col - nc + 1
	}
}

func (s *sidebar) top()    { s.cursor, s.off = 0, 0 }
func (s *sidebar) bottom() { s.move(len(s.visible)) }

func (s *sidebar) selected() (db.Table, bool) {
	if s.cursor >= 0 && s.cursor < len(s.visible) {
		return s.tables[s.visible[s.cursor]], true
	}
	return db.Table{}, false
}

// --- filtering (type to narrow) ---

func (s *sidebar) filterInput(str string) {
	s.filterVal += str
	s.cursor, s.off = 0, 0
	s.rebuildVisible()
}

func (s *sidebar) filterBackspace() {
	if r := []rune(s.filterVal); len(r) > 0 {
		s.filterVal = string(r[:len(r)-1])
		s.cursor, s.off = 0, 0
		s.rebuildVisible()
	}
}

// clearFilter drops the filter and restores the full list.
func (s *sidebar) clearFilter() {
	s.filterVal = ""
	s.cursor, s.off = 0, 0
	s.rebuildVisible()
}

func (s *sidebar) View() string {
	var b strings.Builder
	// Search prompt: always shown — the list filters as you type, no `/` needed.
	// Empty → the label as a placeholder, so you can tell tables from databases.
	txt := "⌕" + s.filterVal + "▏"
	if s.filterVal == "" && s.label != "" {
		txt = "⌕ " + s.label
	}
	b.WriteString(filterStyle.Render(runewidth.Truncate(txt, s.w, "…")))
	b.WriteByte('\n')

	if len(s.visible) == 0 {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(" no matches"))
		return b.String()
	}
	nr, nc, cw := s.rows(), s.gridCols(), s.cellWidth()
	for r := 0; r < nr; r++ {
		if r > 0 {
			b.WriteByte('\n')
		}
		for c := s.off; c < s.off+nc; c++ {
			idx := c*nr + r
			if idx >= len(s.visible) {
				continue
			}
			prefix := " "
			if idx == s.cursor {
				prefix = "›"
			}
			cell := runewidth.FillRight(runewidth.Truncate(prefix+tableLabel(s.tables[s.visible[idx]]), cw, "…"), cw)
			if idx == s.cursor {
				cell = selStyle.Render(cell)
			}
			b.WriteString(cell)
			if c < s.off+nc-1 {
				b.WriteString(strings.Repeat(" ", listColGap))
			}
		}
	}
	return b.String()
}
