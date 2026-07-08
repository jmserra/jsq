package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// sidebar is the full-screen list buffer shared by the table, database, and
// connection screens. Navigation is the default mode (arrows / j-k live in the
// caller, ←/→ jump columns, g/G to the ends); pressing `/` enters filter mode,
// where typing narrows the list live (same LIKE semantics as the grid column
// filter §7.1 — trailing % implied, case-insensitive, a purely client-side
// narrowing of the already-loaded names). Enter keeps the narrowed list and drops
// back to navigation; Esc clears it. This mirrors the grid's two-phase filter so
// the whole app has one filter model.
type sidebar struct {
	tables  []db.Table
	visible []int // display order → tables index (filter view)
	cursor  int   // index into visible
	off     int
	w, h    int

	filtering bool      // in filter-input mode (entered with `/`); else navigation
	filter    textField // current filter text + caret (empty → whole list)
	label     string    // placeholder shown in the search prompt ("tables" / "databases")
}

func (s *sidebar) setTables(t []db.Table) {
	s.tables, s.cursor, s.off = t, 0, 0
	s.filtering = false
	s.filter.clear()
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

// rebuildVisible narrows `visible` to tables whose label matches the filter,
// trying an accurate prefix match first and widening to a substring match only
// when the prefix finds nothing (filterPatterns). Empty filter shows all tables.
// Callers reset cursor/off before calling.
func (s *sidebar) rebuildVisible() {
	for _, pat := range filterPatterns(s.filter.val) {
		s.matchInto(pat)
		if len(s.visible) > 0 || pat == "" {
			break
		}
	}
	s.cursor = clamp(s.cursor, 0, len(s.visible)-1)
}

// matchInto sets `visible` to the tables whose label matches pat (all when empty).
func (s *sidebar) matchInto(pat string) {
	s.visible = s.visible[:0]
	re := likeToRegex(pat)
	for i := range s.tables {
		if pat == "" || re == nil || re.MatchString(tableLabel(s.tables[i])) {
			s.visible = append(s.visible, i)
		}
	}
}

// hasFilter reports whether a filter is narrowing the list.
func (s *sidebar) hasFilter() bool { return s.filter.val != "" }

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

func (s *sidebar) selected() (db.Table, bool) {
	if s.cursor >= 0 && s.cursor < len(s.visible) {
		return s.tables[s.visible[s.cursor]], true
	}
	return db.Table{}, false
}

// top / bottom jump the cursor to the ends of the visible list (g / G).
func (s *sidebar) top()    { s.cursor, s.off = 0, 0 }
func (s *sidebar) bottom() { s.move(len(s.visible)) }

// --- filtering (`/` to enter, type to narrow) ---

// startFilter enters filter-input mode, keeping any existing pattern so `/` can
// resume editing an active filter (mirrors grid.startFilter).
func (s *sidebar) startFilter() { s.filtering = true }

// commitFilter keeps the narrowed list and returns to navigation (Enter).
func (s *sidebar) commitFilter() { s.filtering = false }

// The text-editing ops re-narrow the list (and reset the cursor to the first
// match); the caret-movement ops only move within the input.
func (s *sidebar) filterInput(str string) { s.filter.insert(str); s.afterEdit() }
func (s *sidebar) filterBackspace()       { s.filter.backspace(); s.afterEdit() }
func (s *sidebar) filterDelete()          { s.filter.del(); s.afterEdit() }
func (s *sidebar) filterDeleteWord()      { s.filter.deleteWord(); s.afterEdit() }
func (s *sidebar) filterLeft()            { s.filter.left() }
func (s *sidebar) filterRight()           { s.filter.right() }
func (s *sidebar) filterHome()            { s.filter.home() }
func (s *sidebar) filterEnd()             { s.filter.end() }

func (s *sidebar) afterEdit() {
	s.cursor, s.off = 0, 0
	s.rebuildVisible()
}

// clearFilter drops the filter, exits filter mode, and restores the full list.
func (s *sidebar) clearFilter() {
	s.filtering = false
	s.filter.clear()
	s.cursor, s.off = 0, 0
	s.rebuildVisible()
}

func (s *sidebar) View() string {
	var b strings.Builder
	// Prompt line, tri-state (mirrors the grid header): filter-input mode shows the
	// live pattern with a caret; a committed filter shows the pattern as a standing
	// hint; otherwise the label placeholder, with a nudge that `/` filters.
	var txt string
	switch {
	case s.filtering:
		txt = s.filter.render("⌕")
	case s.filter.val != "":
		txt = "⌕" + s.filter.val
	default:
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
