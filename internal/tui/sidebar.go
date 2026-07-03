package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// sidebar is the flat, filterable table list (§7). First slice: no filter yet.
type sidebar struct {
	tables []db.Table
	cursor int
	off    int
	w, h   int
}

func (s *sidebar) setTables(t []db.Table) { s.tables, s.cursor, s.off = t, 0, 0 }

func (s *sidebar) move(d int) {
	s.cursor = clamp(s.cursor+d, 0, len(s.tables)-1)
	if s.cursor < s.off {
		s.off = s.cursor
	}
	if s.h > 0 && s.cursor >= s.off+s.h {
		s.off = s.cursor - s.h + 1
	}
}

func (s *sidebar) top()    { s.cursor, s.off = 0, 0 }
func (s *sidebar) bottom() { s.move(len(s.tables)) }

func (s *sidebar) selected() (db.Table, bool) {
	if s.cursor >= 0 && s.cursor < len(s.tables) {
		return s.tables[s.cursor], true
	}
	return db.Table{}, false
}

func (s *sidebar) View(focused bool) string {
	var b strings.Builder
	for i := s.off; i < s.off+s.h && i < len(s.tables); i++ {
		name := s.tables[i].Name
		// Show a schema prefix only for non-public schemas; "public." is noise.
		if sch := s.tables[i].Schema; sch != "" && sch != "public" {
			name = sch + "." + name
		}
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
		if i < s.off+s.h-1 && i < len(s.tables)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
