package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// cellView is the read-only full-value popup opened with Enter on a grid cell
// (§7 C1). First slice: a full-area panel rather than a floating overlay.
type cellView struct {
	active bool
	title  string
	lines  []string
	off    int
	w, h   int
}

func (c *cellView) open(col string, row int, v any, w, h int) {
	c.active, c.off, c.w, c.h = true, 0, w, h
	c.title = fmt.Sprintf("%s  (row %d)", col, row+1)
	c.lines = valueLines(v)
}

// valueLines renders a value as display lines, pretty-printing JSON or PHP
// serialize() output when either parses.
func valueLines(v any) []string {
	if v == nil {
		return []string{"NULL"}
	}
	s := fmt.Sprintf("%v", v)
	var js any
	if json.Unmarshal([]byte(s), &js) == nil {
		if b, err := json.MarshalIndent(js, "", "  "); err == nil {
			s = string(b)
		}
	} else if pretty, ok := prettyPHP(s); ok {
		s = pretty
	}
	return strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
}

func (c *cellView) rows() int {
	if n := c.h - 1; n > 0 { // title line
		return n
	}
	return 1
}

func (c *cellView) scroll(d int) {
	c.off = clamp(c.off+d, 0, max(0, len(c.lines)-c.rows()))
}

func (c *cellView) View() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(c.title))
	end := min(c.off+c.rows(), len(c.lines))
	for i := c.off; i < end; i++ {
		b.WriteByte('\n')
		b.WriteString(runewidth.Truncate(c.lines[i], c.w, "…"))
	}
	return b.String()
}
