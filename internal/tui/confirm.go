package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// confirmView is the safe-mode confirmation overlay (connection safe=true). Every
// mutation is routed here before it runs: it names the target connection +
// database and shows the SQL, and only 'y' runs it — any other key cancels. run
// builds the exec command once confirmed; label is the activity/status word the
// header shows while it runs.
type confirmView struct {
	active bool
	conn   string
	db     string
	lines  []string // the SQL to preview, split into display lines
	run    func(context.Context) tea.Cmd
	label  string
	w, h   int
}

// ask arms the overlay for sql, remembering how to run it once confirmed.
func (c *confirmView) ask(conn, db, sql, label string, run func(context.Context) tea.Cmd, w, h int) {
	c.active = true
	c.conn, c.db, c.label, c.run = conn, db, label, run
	c.w, c.h = w, h
	c.lines = strings.Split(strings.ReplaceAll(strings.TrimRight(sql, "\n"), "\r", ""), "\n")
}

func (c *confirmView) View() string {
	target := c.conn
	if c.db != "" {
		target += " > " + c.db
	}

	var b strings.Builder
	head := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	b.WriteString(head.Render("This statement will run on: " + target))
	b.WriteByte('\n')

	// Reserve chrome: the header line above, one blank before the SQL, one blank
	// and the prompt below. Truncate an over-long preview so the overlay fits.
	maxSQL := c.h - 4
	if maxSQL < 1 {
		maxSQL = 1
	}
	lines, truncated := c.lines, false
	if len(lines) > maxSQL {
		lines, truncated = lines[:maxSQL], true
	}
	sqlStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	for _, ln := range lines {
		b.WriteByte('\n')
		b.WriteString(sqlStyle.Render(runewidth.Truncate(ln, c.w, "…")))
	}
	if truncated {
		b.WriteByte('\n')
		b.WriteString(lipgloss.NewStyle().Faint(true).Render("… (statement truncated in preview)"))
	}

	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Run it? "))
	b.WriteString(lipgloss.NewStyle().Faint(true).Render("y = yes — any other key cancels"))
	return b.String()
}
