package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// errView is the modal shown when a user-authored statement fails — a free-form
// (s) query or a quick-path cell edit. It surfaces the full, untruncated engine
// error (Postgres position/HINT/DETAIL and all) alongside the statement that
// produced it, and `e`/Enter reopens that statement in $EDITOR, so a failed
// query can be fixed and re-run instead of being lost to a one-line status.
// Like confirmView it can be armed from any screen (a table-list scratch can
// fail), so View() renders it before the screen switch. Scrollable like
// cellView, since an error + statement can outrun the viewport.
type errView struct {
	active   bool
	errText  string     // the raw error, for yanking
	lines    []string   // error lines, a blank, then the failed statement
	sqlStart int        // index in lines where the statement begins (-1 if none)
	seed     editorSeed // reopened in $EDITOR on e/Enter
	off      int
	w, h     int
}

// arm builds the display from err and the statement's seed (its sql is shown
// and reopened for editing).
func (v *errView) arm(err error, seed editorSeed, w, h int) {
	v.active, v.off, v.w, v.h = true, 0, w, h
	v.seed = seed
	v.errText = err.Error()
	lines := strings.Split(strings.ReplaceAll(v.errText, "\r", ""), "\n")
	v.sqlStart = -1
	sql := strings.TrimRight(strings.ReplaceAll(seed.sql, "\r", ""), "\n")
	if sql != "" {
		lines = append(lines, "")
		v.sqlStart = len(lines)
		lines = append(lines, strings.Split(sql, "\n")...)
	}
	v.lines = lines
}

// rows is the scrollable body height, reserving the title + hint chrome.
func (v *errView) rows() int {
	if n := v.h - 2; n > 0 {
		return n
	}
	return 1
}

func (v *errView) scroll(d int) {
	v.off = clamp(v.off+d, 0, max(0, len(v.lines)-v.rows()))
}

func (v *errView) View() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render("query failed"))
	b.WriteByte('\n')
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(
		"e / enter — edit & re-run · y — yank error · esc — dismiss"))

	sqlStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	end := min(v.off+v.rows(), len(v.lines))
	for i := v.off; i < end; i++ {
		b.WriteByte('\n')
		ln := runewidth.Truncate(v.lines[i], v.w, "…")
		if v.sqlStart >= 0 && i >= v.sqlStart {
			ln = sqlStyle.Render(ln)
		}
		b.WriteString(ln)
	}
	return b.String()
}
