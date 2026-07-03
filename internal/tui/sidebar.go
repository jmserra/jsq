package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// sidebar is the flat, filterable table list (§7). `/` filters it in place as
// you type — a purely client-side narrowing of the already-loaded table names
// (no server round-trip, unlike the grid's two-phase column filter §7.1).
type sidebar struct {
	tables  []db.Table
	visible []int // display order → tables index (filter view)
	cursor  int   // index into visible
	off     int
	w, h    int

	filtering bool   // `/` filter is being typed
	filterVal string // in-progress filter text
}

func (s *sidebar) setTables(t []db.Table) {
	s.tables, s.cursor, s.off = t, 0, 0
	s.filtering, s.filterVal = false, ""
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

// rebuildVisible narrows `visible` to tables whose label matches the filter,
// using the same LIKE semantics as the grid column filter (§7.1): a trailing %
// is implied (prefix match), % and _ are wildcards, always case-insensitive.
// Empty filter shows all tables.
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
	if s.off > s.cursor {
		s.off = s.cursor
	}
}

// hasFilter reports an active filter — either being typed or committed and
// still narrowing the list.
func (s *sidebar) hasFilter() bool { return s.filtering || s.filterVal != "" }

// listHeight is the rows available for the list — one line is taken by the
// filter prompt whenever a filter is shown.
func (s *sidebar) listHeight() int {
	if s.hasFilter() && s.h > 0 {
		return s.h - 1
	}
	return s.h
}

func (s *sidebar) move(d int) {
	s.cursor = clamp(s.cursor+d, 0, len(s.visible)-1)
	h := s.listHeight()
	if s.cursor < s.off {
		s.off = s.cursor
	}
	if h > 0 && s.cursor >= s.off+h {
		s.off = s.cursor - h + 1
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

// --- filtering (§7) ---

// startFilter enters filter-typing mode, keeping any active pattern so you can
// refine it (like the grid, §7.1).
func (s *sidebar) startFilter() {
	s.filtering = true
	s.rebuildVisible()
}

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

// commitFilter stops typing but keeps the pattern active, so the narrowed list
// survives loading a table and returning to it with H.
func (s *sidebar) commitFilter() { s.filtering = false }

// clearFilter drops the active filter entirely and restores the full list.
func (s *sidebar) clearFilter() {
	s.filtering, s.filterVal = false, ""
	s.rebuildVisible()
}

func (s *sidebar) View(focused bool) string {
	var b strings.Builder
	if s.hasFilter() {
		txt := "⌕" + s.filterVal
		if s.filtering {
			txt += "▏" // cursor bar only while typing
		}
		prompt := runewidth.FillRight(runewidth.Truncate(txt, s.w, "…"), s.w)
		b.WriteString(filterStyle.Render(prompt))
		b.WriteByte('\n')
	}
	h := s.listHeight()
	for i := s.off; i < s.off+h && i < len(s.visible); i++ {
		name := tableLabel(s.tables[s.visible[i]])
		name = runewidth.Truncate(name, s.w, "…")
		name = runewidth.FillRight(name, s.w)

		if i == s.cursor {
			if focused {
				name = selStyle.Render(name)
			} else {
				name = lipgloss.NewStyle().Bold(true).Render(name)
			}
		}
		b.WriteString(name)
		if i < s.off+h-1 && i < len(s.visible)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
