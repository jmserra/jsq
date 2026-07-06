package tui

import (
	"testing"

	"github.com/jmserra/jsq/internal/db"
)

func TestTableListMultiColumn(t *testing.T) {
	var s sidebar
	var tabs []db.Table
	for _, n := range []string{"a", "bb", "ccc", "dddd", "ee", "ff", "gg", "hh"} {
		tabs = append(tabs, db.Table{Name: n})
	}
	s.setTables(tabs)
	s.w, s.h = 40, 6 // rows = 5

	// A wide screen lays out multiple columns, each wide enough for the longest
	// name ("dddd" = 4) plus the cursor marker — nothing is cut.
	if s.gridCols() < 2 {
		t.Fatalf("wide screen should give multiple columns, got %d", s.gridCols())
	}
	if cw := s.cellWidth(); cw < 5 {
		t.Fatalf("cell must fit the widest name (4)+marker, got %d", cw)
	}

	// Right arrow jumps a whole column (index += rows).
	s.move(s.rows())
	if s.cursor != s.rows() {
		t.Fatalf("column jump should land at index %d, got %d", s.rows(), s.cursor)
	}

	// A narrow screen collapses to a single column.
	s.w = 8
	if s.gridCols() != 1 {
		t.Fatalf("narrow screen should be single column, got %d", s.gridCols())
	}
}
