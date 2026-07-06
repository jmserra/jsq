package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// help is the read-only keybinding cheat sheet opened with `?` — a full-area
// panel like cellView. Its content is a static, grouped view of the keymap
// (there is no keymap struct; the bindings are hardcoded in app.go, so this is
// the single hand-kept mirror of them).
type help struct {
	active bool
	off    int
	w, h   int
}

func (hp *help) open(w, h int) { hp.active, hp.off, hp.w, hp.h = true, 0, w, h }

func (hp *help) rows() int {
	if n := hp.h - 1; n > 0 { // title line
		return n
	}
	return 1
}

func (hp *help) scroll(d int) {
	hp.off = clamp(hp.off+d, 0, max(0, len(helpLines)-hp.rows()))
}

// helpItem is one cheat-sheet entry: a section header (head set) or a binding.
type helpItem struct {
	head string
	key  string
	desc string
}

var helpItems = []helpItem{
	{head: "Move"},
	{key: "h j k l", desc: "move cursor"},
	{key: "g / G", desc: "first / last row"},
	{key: "0 / $", desc: "first / last column"},
	{head: "Sort & filter"},
	{key: "J / K", desc: "sort column ascending / descending"},
	{key: "/", desc: "filter column (type to preview)"},
	{key: "Esc", desc: "kill a running query, else clear the filter"},
	{head: "Inspect"},
	{key: "Enter", desc: "view the full cell value"},
	{key: "f", desc: "follow the foreign key on this column"},
	{key: "Ctrl-o / Ctrl-i", desc: "jump back / forward (Ctrl-i needs terminal support)"},
	{key: "`", desc: "jumplist picker — pick any visited view (j/k, enter)"},
	{head: "Edit"},
	{key: "e", desc: "quick-edit cell (keyed UPDATE, runs now)"},
	{key: "E", desc: "edit cell in $EDITOR"},
	{key: "o", desc: "insert a blank row"},
	{key: "D", desc: "delete the current row"},
	{key: "p", desc: "duplicate the current row"},
	{head: "SQL"},
	{key: "s", desc: "free-form SQL in $EDITOR"},
	{head: "Navigate"},
	{key: "t", desc: "go to the table list (type to filter, enter opens)"},
	{key: "T", desc: "go to the database list (jump to another database)"},
	{key: "c", desc: "connection picker (switch to another connection)"},
	{key: "Tab", desc: "jump forward through views (= Ctrl-i)"},
	{head: "General"},
	{key: "?", desc: "toggle this help"},
	{key: "Ctrl-c", desc: "quit"},
}

// helpLine is a rendered logical line: a header, a binding row, or a blank
// separator (empty text, head false).
type helpLine struct {
	text string
	head bool
}

// helpLines is the flat, display-ordered cheat sheet, built once from helpItems
// with a blank line before each section after the first.
var helpLines = buildHelpLines()

func buildHelpLines() []helpLine {
	out := make([]helpLine, 0, len(helpItems)+8)
	for _, it := range helpItems {
		if it.head != "" {
			if len(out) > 0 {
				out = append(out, helpLine{})
			}
			out = append(out, helpLine{text: it.head, head: true})
			continue
		}
		out = append(out, helpLine{text: fmt.Sprintf("  %-9s %s", it.key, it.desc)})
	}
	return out
}

func (hp *help) View() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Keybindings  —  ? / Esc to close"))
	end := min(hp.off+hp.rows(), len(helpLines))
	for i := hp.off; i < end; i++ {
		b.WriteByte('\n')
		// Truncate the plain text first, then style, so width accounting never
		// counts ANSI escapes.
		txt := runewidth.Truncate(helpLines[i].text, hp.w, "…")
		if helpLines[i].head {
			txt = headerStyle.Render(txt)
		} else {
			txt = lipgloss.NewStyle().Render(txt)
		}
		b.WriteString(txt)
	}
	return b.String()
}
