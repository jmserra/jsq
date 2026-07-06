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

// rebuildVisible narrows `visible` to tables whose label matches the filter.
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

// hasFilter reports whether a filter is narrowing the list.
func (s *sidebar) hasFilter() bool { return s.filterVal != "" }

// listHeight is the rows available for the list — one line is always taken by the
// search prompt at the top.
func (s *sidebar) listHeight() int {
	if s.h > 1 {
		return s.h - 1
	}
	return 1
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
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(" no tables match"))
		return b.String()
	}
	h := s.listHeight()
	for i := s.off; i < s.off+h && i < len(s.visible); i++ {
		name := tableLabel(s.tables[s.visible[i]])
		line := " " + name
		if i == s.cursor {
			line = selStyle.Render("›" + name)
		}
		b.WriteString(line)
		if i < s.off+h-1 && i < len(s.visible)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
