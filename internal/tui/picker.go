package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/config"
)

// picker is the connection picker shown for a bare `jsq` (§4).
type picker struct {
	conns  []config.Conn
	cursor int
}

func (p *picker) move(d int) { p.cursor = clamp(p.cursor+d, 0, len(p.conns)-1) }

func (p *picker) selected() (config.Conn, bool) {
	if p.cursor >= 0 && p.cursor < len(p.conns) {
		return p.conns[p.cursor], true
	}
	return config.Conn{}, false
}

func (p *picker) View() string {
	var b strings.Builder
	b.WriteString(" " + lipgloss.NewStyle().Faint(true).Render("connections"))
	b.WriteString("\n")
	for i, c := range p.conns {
		line := " " + c.Name
		if i == p.cursor {
			line = selStyle.Render("›" + c.Name)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
