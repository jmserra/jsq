package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// export is the SQL-dump screen (`e` on the table list): the same full-screen
// list every other screen uses, in check mode, plus a settings footer. Tick the
// tables, set the two options, Enter writes one .sql file.
//
// It reuses `sidebar` rather than growing its own list so the export screen gets
// the `/` filter, the column-major grid and the movement keys for free — picking
// twenty tables out of four hundred is exactly the case the filter exists for.
//
// The whole of it is presentation; the write itself is exportCmd in cmd.go, like
// every other piece of DB work.
type export struct {
	list sidebar // the table list in check mode (marks non-nil)
	opts exportOpts
}

// exportField names the footer field currently taking keystrokes (none by
// default — the list has the keyboard).
type exportField int

const (
	exportFieldNone exportField = iota
	exportFieldRows
	exportFieldPath
)

// exportOpts is the settings footer's state. rows/path are textFields so they
// edit exactly like the filters do (caret, Ctrl-w, Home/End) through the one
// shared renderer.
type exportOpts struct {
	rows      textField // newest-N rows per table; empty → the whole table
	path      textField // output file
	structure bool      // emit DROP/CREATE (Postgres: DELETE) before the rows
	editing   exportField

	// pathSet records that the path was typed rather than generated, so the
	// default name stops following the selection once you've named the file.
	pathSet bool

	// orig is what the field being edited held when it opened, so Esc can put it
	// back (the hint promises a cancel, not just a way out of the field).
	orig string
}

// limit is the row cap per table (0 = no cap). Garbage in the field reads as 0
// rather than an error: the footer shows what it parsed to, so "all" is visible.
func (o exportOpts) limit() int {
	n, err := strconv.Atoi(strings.TrimSpace(o.rows.val))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// exportLines is the footer's height (settings, path, key hints).
const exportLines = 3

// defaultExportPath is the generated file name: where it came from, what's in
// it, the row cap, and a timestamp — enough to tell two dumps of the same tables
// apart. It follows the selection until the path is typed over.
func defaultExportPath(from string, tables []db.Table, limit int) string {
	name := "export"
	switch {
	case len(tables) == 0:
	case len(tables) <= 3:
		parts := make([]string, len(tables))
		for i, t := range tables {
			parts[i] = safeFileName(tableLabel(t))
		}
		name = strings.Join(parts, "_")
	default:
		name = fmt.Sprintf("%dtables", len(tables))
	}
	limitTag := ""
	if limit > 0 {
		limitTag = fmt.Sprintf("_last%d", limit)
	}
	if from = safeFileName(from); from != "" {
		from += "_"
	}
	return fmt.Sprintf("./%s%s%s_%s.sql",
		from, name, limitTag, time.Now().Format("20060102_1504"))
}

// expandHome turns a leading ~/ into the home directory: a typed path is the one
// place in jsq where a shell would have done it for you, and the failure without
// it ("no such file or directory: ~") reads like a bug.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// safeFileName reduces a connection or table name to filename-safe characters —
// a Postgres table arrives schema-qualified ("public.entries"), and a connection
// name is whatever the TOML section was called.
func safeFileName(s string) string {
	s = unsafeFileChars.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// open (re)seeds the screen from the live table list. Marks survive because
// they're keyed by label, so stepping out to the grid and back in keeps the
// selection — and a table that has since vanished simply stops being checked.
func (e *export) open(from string, tables []db.Table) {
	if e.list.marks == nil {
		e.list = sidebar{label: "tables to export", marks: map[string]bool{}}
	}
	e.opts.editing = exportFieldNone
	e.list.setTables(tables)
	e.refreshPath(from)
}

// refreshPath regenerates the default file name for the current selection,
// unless the user has typed one.
func (e *export) refreshPath(from string) {
	if e.opts.pathSet {
		return
	}
	e.opts.path.setVal(defaultExportPath(from, e.list.checkedTables(), e.opts.limit()))
}

// --- footer ---

// View is the check list with the settings footer beneath it. The list block is
// padded to its full height so the footer sits at the bottom of the screen even
// when the filter has narrowed the list to nothing.
func (e export) View(job *exportJob) string {
	list := lipgloss.NewStyle().Height(e.list.h).Render(e.list.View())
	return list + "\n" + e.footer(job)
}

// footer renders the settings under the list: what will be written, where, and
// the keys. The field being edited draws with the shared caret, so it looks like
// every other text input in the app.
func (e export) footer(job *exportJob) string {
	w, n := e.list.w, len(e.list.checkedTables())
	var b strings.Builder

	rows := "all"
	if l := e.opts.limit(); l > 0 {
		rows = fmt.Sprintf("newest %d", l)
	}
	structure := "no"
	if e.opts.structure {
		structure = "yes"
	}
	switch e.opts.editing {
	case exportFieldRows:
		b.WriteString(headerStyle.Render("rows: "))
		b.WriteString(renderCaretField(e.opts.rows.val, e.opts.rows.pos, 12, filterStyle))
		b.WriteString(faintStyle.Render("  (empty = all)"))
	default:
		b.WriteString(faintStyle.Render(fmt.Sprintf(
			"%d selected · rows: %s · structure: %s", n, rows, structure)))
	}
	b.WriteByte('\n')

	if e.opts.editing == exportFieldPath {
		b.WriteString(headerStyle.Render("→ "))
		b.WriteString(renderCaretField(e.opts.path.val, e.opts.path.pos, max(1, w-2), filterStyle))
	} else {
		b.WriteString(runewidth.Truncate("→ "+e.opts.path.val, w, "…"))
	}
	b.WriteByte('\n')

	hint := "space select · a all · n rows · S structure · o path · enter export · bksp back"
	switch {
	case e.opts.editing != exportFieldNone:
		hint = "enter accept · esc cancel"
	case job != nil:
		// The only place an export can be stopped, so it has to say so.
		hint = "export running — esc cancels it · bksp leaves it running"
	}
	b.WriteString(faintStyle.Render(runewidth.Truncate(hint, w, "…")))
	return b.String()
}

var faintStyle = lipgloss.NewStyle().Faint(true)

// exportSummary is the status line after a dump lands: what was written, where.
func exportSummary(path string, tables int, rows int64) string {
	return fmt.Sprintf("wrote %s — %d %s, %d %s",
		filepath.Base(path), tables, plural(tables, "table"), rows, plural(int(rows), "row"))
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// editField opens one of the footer's text fields, remembering what it held so
// Esc can put it back.
func (e *export) editField(f exportField) {
	e.opts.editing = f
	if f == exportFieldPath {
		e.opts.orig = e.opts.path.val
		e.opts.path.end()
		return
	}
	e.opts.orig = e.opts.rows.val
	e.opts.rows.end()
}
