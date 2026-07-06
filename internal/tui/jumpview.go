package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// jumpView is the jumplist picker overlay (opened with `): a full-area list of
// visited views, oldest→newest, with the current one marked and Enter to jump.
// A snapshot of the labels is taken at open time (nothing navigates while it's up).
type jumpView struct {
	active  bool
	entries []string // display labels, oldest→newest
	curIdx  int      // the currently-displayed view
	cursor  int      // highlighted row
	w, h    int
}

func (j *jumpView) open(entries []string, cur, w, h int) {
	j.active = true
	j.entries = entries
	j.curIdx = cur
	j.cursor = cur
	j.w, j.h = w, h
}

func (j *jumpView) move(d int) {
	j.cursor = clamp(j.cursor+d, 0, len(j.entries)-1)
}

func (j *jumpView) rows() int {
	if n := j.h - 1; n > 0 { // title line
		return n
	}
	return 1
}

func (j *jumpView) View() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("jumplist") +
		lipgloss.NewStyle().Faint(true).Render("  enter jumps · esc closes"))

	vr := j.rows()
	off := 0
	if j.cursor >= vr {
		off = j.cursor - vr + 1
	}
	end := min(off+vr, len(j.entries))
	for i := off; i < end; i++ {
		b.WriteByte('\n')
		prefix := "  "
		if i == j.curIdx {
			prefix = "▸ " // the view currently on screen
		}
		line := runewidth.Truncate(prefix+j.entries[i], j.w, "…")
		if i == j.cursor {
			line = selStyle.Render(line)
		}
		b.WriteString(line)
	}
	return b.String()
}
