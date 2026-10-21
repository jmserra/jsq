package tui

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/db"
	"github.com/mattn/go-runewidth"
)

// export is the SQL-dump screen (`x` on the table list): the same full-screen
// list every other screen uses, in check mode, plus a settings footer. Tick the
// tables, optionally cap the rows, Enter writes one .sql file.
//
// It reuses `sidebar` rather than growing its own list so the export screen gets
// the `/` filter, the column-major grid and the movement keys for free — picking
// twenty tables out of four hundred is exactly the case the filter exists for.
//
// There is nothing else to set. The file is named for you (defaultExportPath),
// and its shape follows the cap: the whole table is recreated (DROP + CREATE +
// INSERT), a capped one is upserted with no DDL, so it can be rerun over the
// live table. The whole of it is presentation; the write itself is exportCmd in
// cmd.go, like every other piece of DB work.
type export struct {
	list sidebar // the table list in check mode (marks non-nil)
	opts exportOpts

	// links is every foreign key in the database as a table pair, read when the
	// screen opens (tableLinksCmd); nil until it lands, or if it couldn't be
	// read, which only costs the boundary warning.
	links []db.TableLink
}

// exportOpts is the settings footer's state: the one field there is. rows is a
// textField so it edits exactly like the filters do (caret, Ctrl-w, Home/End)
// through the one shared renderer.
type exportOpts struct {
	rows    textField // newest-N rows per table; empty → the whole table
	editing bool      // the rows field has the keyboard

	// orig is what the field held when it opened, so Esc can put it back (the
	// hint promises a cancel, not just a way out of the field).
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

// exportLines is the footer's height (settings, links, file, key hints).
const exportLines = 4

// defaultExportPath is the file an export writes, in the directory jsq was
// started from: where it came from, what's in it, the row cap, and a timestamp —
// enough to tell two dumps of the same tables apart, and to never overwrite one.
func defaultExportPath(from string, tables []db.Table, limit int, now time.Time) string {
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
	return fmt.Sprintf("%s/%s%s%s_%s.sql",
		exportDir, from, name, limitTag, now.Format("20060102_150405"))
}

// exportDir is where exports land: the directory jsq was started from. A var so
// a test can point it at a temp dir.
var exportDir = "."

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
func (e *export) open(tables []db.Table) {
	if e.list.marks == nil {
		e.list = sidebar{label: "tables to export", marks: map[string]bool{}}
	}
	e.opts.editing = false
	e.list.setTables(tables)
}

// --- footer ---

// View is the check list with the settings footer beneath it. The list block is
// padded to its full height so the footer sits at the bottom of the screen even
// when the filter has narrowed the list to nothing. from names the file.
func (e export) View(from string) string {
	list := lipgloss.NewStyle().Height(e.list.h).Render(e.list.View())
	return list + "\n" + e.footer(from)
}

// footer renders the settings under the list: what will be written and where,
// and the keys. The rows field, while open, draws with the shared caret, so it
// looks like every other text input in the app.
func (e export) footer(from string) string {
	w, tables := e.list.w, e.list.checkedTables()
	var b strings.Builder

	if e.opts.editing {
		b.WriteString(headerStyle.Render("rows: "))
		b.WriteString(renderCaretField(e.opts.rows.val, e.opts.rows.pos, 12, filterStyle))
		b.WriteString(faintStyle.Render("  (newest N per table · empty = all)"))
	} else {
		mode := "rows: all · drop + create + insert"
		if l := e.opts.limit(); l > 0 {
			mode = fmt.Sprintf("rows: newest %d · upsert, no ddl", l)
		}
		b.WriteString(faintStyle.Render(runewidth.Truncate(
			fmt.Sprintf("%d selected · %s", len(tables), mode), w, "…")))
	}
	b.WriteByte('\n')

	b.WriteString(e.linksLine(tables, w))
	b.WriteByte('\n')

	// The name is stamped when the export starts; this shows its shape.
	path := defaultExportPath(from, tables, e.opts.limit(), time.Now())
	b.WriteString(faintStyle.Render(runewidth.Truncate("→ "+path, w, "…")))
	b.WriteByte('\n')

	hint := "space select · a all · n rows · enter export · bksp back"
	if e.opts.editing {
		hint = "enter accept · esc cancel"
	}
	b.WriteString(faintStyle.Render(runewidth.Truncate(hint, w, "…")))
	return b.String()
}

var faintStyle = lipgloss.NewStyle().Faint(true)

// crossing is a foreign key with one end in the selection and one outside it:
// the dump's rows on one side, the target's own rows on the other, and nothing
// to say they agree. far is the end outside; out says it is the referenced end
// (a key out of the selection), which is the only kind `+` follows.
type crossing struct {
	link db.TableLink
	far  db.Table
	out  bool
}

// crossings lists the links that leave the selection. Which ones matter follows
// the file's shape:
//   - a key out of a checked table (sessions → users) always does: the dump's
//     rows may name a parent the target doesn't have;
//   - a key into one (feedback → sessions) only on a full export, whose DROP …
//     CASCADE takes it and replaces the rows it pointed at. A capped export
//     upserts and deletes nothing, so what pointed at the old rows still does.
func (e export) crossings(checked []db.Table) []crossing {
	in := map[string]bool{}
	for _, t := range checked {
		in[tableLabel(t)] = true
	}
	full := e.opts.limit() == 0
	var out []crossing
	for _, l := range e.links {
		from, to := db.Table(l.From), db.Table(l.To)
		switch {
		case in[tableLabel(from)] && !in[tableLabel(to)]:
			out = append(out, crossing{l, to, true})
		case full && in[tableLabel(to)] && !in[tableLabel(from)]:
			out = append(out, crossing{l, from, false})
		}
	}
	return out
}

// linksLine is the footer's warning about links that leave the selection, or a
// blank line when there are none (or nothing is checked, or the links are still
// loading) — the footer keeps its height either way, so the list doesn't jump.
func (e export) linksLine(checked []db.Table, w int) string {
	cs := e.crossings(checked)
	if len(cs) == 0 {
		return ""
	}
	listed, far := e.listed(), map[string]bool{}
	pairs := make([]string, len(cs))
	for i, c := range cs {
		if c.out && listed[tableLabel(c.far)] {
			far[tableLabel(c.far)] = true
		}
		pairs[i] = tableLabel(db.Table(c.link.From)) + " → " + tableLabel(db.Table(c.link.To))
	}
	add := ""
	if len(far) > 0 {
		add = fmt.Sprintf(" · + checks %d %s they point at", len(far), plural(len(far), "table"))
	}
	s := fmt.Sprintf("⚠ %d %s outside the selection%s · %s",
		len(cs), plural(len(cs), "link"), add, strings.Join(pairs, ", "))
	return filterStyle.Render(runewidth.Truncate(s, w, "…"))
}

// listed is the set of labels the list holds — the only tables a mark can
// export. A link can reach one it doesn't show (an extension's, say).
func (e export) listed() map[string]bool {
	m := make(map[string]bool, len(e.list.tables))
	for _, t := range e.list.tables {
		m[tableLabel(t)] = true
	}
	return m
}

// addLinked checks the tables the selection's keys point at but leaves out,
// and returns their labels. It follows keys OUT of the selection only, to the
// parents its rows need: every table a full export checks is dropped and
// replaced in the target, and following keys INTO the selection would sweep up
// each table that references it (check `users`, get half the schema) — rows
// the user never chose to overwrite. Those stay on the warning line for the
// user to check by hand. One hop at a time for the same reason: the parents it
// adds have keys of their own, which the line then shows.
func (e *export) addLinked() []string {
	listed := e.listed()
	var added []string
	for _, c := range e.crossings(e.list.checkedTables()) {
		if lbl := tableLabel(c.far); c.out && listed[lbl] && !e.list.marks[lbl] {
			e.list.marks[lbl] = true
			added = append(added, lbl)
		}
	}
	return added
}

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

// editRows opens the row-cap field, remembering what it held so Esc can put it
// back.
func (e *export) editRows() {
	e.opts.editing = true
	e.opts.orig = e.opts.rows.val
	e.opts.rows.end()
}
