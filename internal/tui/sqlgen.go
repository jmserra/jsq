package tui

import (
	"fmt"
	"strings"

	"github.com/jmserra/jsq/internal/db"
)

// sqlLiteral renders a scanned cell value as a SQL literal for the $EDITOR
// starting point. The full path (E) runs the raw SQL the user :wq's, so values
// are inlined here rather than bound: strings are single-quoted with ”
// escaping, nil is NULL, booleans TRUE/FALSE, numerics bare. It only has to be a
// good starting point — the user reviews and fixes it before running.
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return "'" + strings.ReplaceAll(fmt.Sprintf("%v", x), "'", "''") + "'"
	}
}

// isStringValue reports whether sqlLiteral will single-quote v (i.e. v is a
// string-ish literal), so the editor can select the text inside the quotes.
func isStringValue(v any) bool {
	switch v.(type) {
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return false
	default:
		return true
	}
}

// selectKind tells the editor how to pre-select the value under the cursor.
type selectKind int

const (
	selectNone         selectKind = iota // just place the cursor (empty / multi-line)
	selectToken                          // whole literal to end of line (NULL / number)
	selectInsideQuotes                   // the text between a string literal's quotes
)

// editorSeed is a generated statement plus where to put the cursor, so the
// editor can open on the value (and, for a single value, pre-select it).
type editorSeed struct {
	sql  string
	line int // 1-based cursor line (0 = leave at top)
	col  int // 1-based byte column to place the cursor
	kind selectKind

	// remember, when its Name is set, is the table whose scratch (s) query this
	// is: on submit the SQL is stored as that table's last query, so the next s
	// on it prefills your last query. Only s sets this; E/o/D/p leave it zero.
	remember db.Table
}

// previewEditSQL renders the quick-path (e) UPDATE with its values inlined, for
// the safe-mode confirmation overlay only. The statement that actually runs
// (execEditCmd) binds the same values as parameters — this is display text, not
// what's executed (cf. invariant 5).
func previewEditSQL(eng db.Engine, req editReq) string {
	preds := make([]string, len(req.keys))
	for i, k := range req.keys {
		preds[i] = eng.QuoteIdent(k.col) + " = " + sqlLiteral(k.val)
	}
	newVal := sqlLiteral(req.val)
	if req.null {
		newVal = "NULL"
	}
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s",
		eng.QualifiedName(req.table), eng.QuoteIdent(req.col), newVal,
		strings.Join(preds, " AND "))
}

// buildUpdateStmt is the E full-path starting point (the editing model in
// README): a full-PK-keyed UPDATE with the current value inlined at the end of
// the SET line so the editor can drop you straight onto it. To set NULL, change
// the value to a bare NULL; to abort, clear the buffer or :q!.
func buildUpdateStmt(eng db.Engine, table db.TableRef, col string, val any, keys []keyPred) editorSeed {
	lit := sqlLiteral(val)
	setLine := fmt.Sprintf("UPDATE %s SET %s = %s",
		eng.QualifiedName(table), eng.QuoteIdent(col), lit)
	litStart := len(setLine) - len(lit) + 1 // 1-based byte col of the literal

	preds := make([]string, len(keys))
	for i, k := range keys {
		preds[i] = eng.QuoteIdent(k.col) + " = " + sqlLiteral(k.val)
	}
	sql := setLine + "\nWHERE " + strings.Join(preds, " AND ") + ";\n"

	seed := editorSeed{sql: sql, line: 1, col: litStart, kind: selectToken}
	switch {
	case strings.Contains(lit, "\n"):
		seed.kind = selectNone // multi-line value: land on it, don't select
	case isStringValue(val):
		if len(lit) > 2 { // non-empty '...'
			seed.col = litStart + 1 // first char inside the quotes
			seed.kind = selectInsideQuotes
		} else { // empty '' — nothing to select, sit between the quotes
			seed.col = litStart + 1
			seed.kind = selectNone
		}
	}
	return seed
}

// insertLine is one column's slot in a generated INSERT: the quoted-less column
// name, its value literal, and an optional trailing ⚠/hint.
type insertLine struct{ name, val, warn string }

// renderInsert lays out an INSERT from per-column lines: one value per line with
// an aligned `-- col` comment (plus any warn), auto-generated columns already
// dropped by the caller. A table with no insertable columns falls back to
// DEFAULT VALUES. The cursor lands on the first value (line 3, indented 2).
func renderInsert(eng db.Engine, table db.TableRef, lines []insertLine) editorSeed {
	if len(lines) == 0 {
		return editorSeed{sql: fmt.Sprintf("INSERT INTO %s DEFAULT VALUES;\n", eng.QualifiedName(table))}
	}
	names := make([]string, len(lines))
	cells := make([]string, len(lines))
	maxw := 0
	for i, l := range lines {
		names[i] = eng.QuoteIdent(l.name)
		cells[i] = l.val
		if i < len(lines)-1 {
			cells[i] += ","
		}
		if len(cells[i]) > maxw {
			maxw = len(cells[i])
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO %s (%s)\nVALUES (\n", eng.QualifiedName(table), strings.Join(names, ", "))
	for i, l := range lines {
		comment := "-- " + l.name
		if l.warn != "" {
			comment += "   " + l.warn
		}
		fmt.Fprintf(&b, "  %s%s%s\n", cells[i], strings.Repeat(" ", maxw-len(cells[i])+1), comment)
	}
	b.WriteString(");\n")
	return editorSeed{sql: b.String(), line: 3, col: 3, kind: selectNone}
}

// buildInsertStmt is the o (blank insert) starting point: the insertable columns
// (auto-generated ones omitted so the DB assigns them), one NULL per line with a
// ⚠ on PK/UNIQUE columns (surfaced before :wq, not as a post-hoc DB error) or a
// note that a column has a default. Values are NULL rather than the DEFAULT
// keyword, which SQLite doesn't accept in a VALUES list.
func buildInsertStmt(eng db.Engine, table db.TableRef, cols []db.Column) editorSeed {
	var lines []insertLine
	for _, c := range cols {
		if c.AutoGenerated {
			continue // DB assigns it
		}
		warn := ""
		switch {
		case c.PrimaryKey:
			warn = "⚠ PRIMARY KEY — must be unique"
		case c.Unique:
			warn = "⚠ UNIQUE"
		case c.HasDefault:
			warn = "has default — delete this line to use it"
		}
		lines = append(lines, insertLine{name: c.Name, val: "NULL", warn: warn})
	}
	return renderInsert(eng, table, lines)
}

// buildDuplicateStmt is the p (duplicate) starting point: an INSERT pre-filled
// from the current row's values (vals, keyed by column name), same table only.
// The auto-generated PK is omitted so a fresh one is assigned; a natural PK is
// kept and flagged as the value to change. UNIQUE columns are flagged too, since
// copying an existing value would collide.
func buildDuplicateStmt(eng db.Engine, table db.TableRef, cols []db.Column, vals map[string]any) editorSeed {
	var lines []insertLine
	for _, c := range cols {
		if c.AutoGenerated {
			continue // let the DB assign a fresh one
		}
		warn := ""
		switch {
		case c.PrimaryKey:
			warn = "⚠ PRIMARY KEY — must be unique, change this"
		case c.Unique:
			warn = "⚠ UNIQUE — change before :wq"
		case c.HasDefault:
			warn = "has default — delete this line to use it"
		}
		lines = append(lines, insertLine{name: c.Name, val: sqlLiteral(vals[c.Name]), warn: warn})
	}
	return renderInsert(eng, table, lines)
}

// buildDeleteStmt is the D full-path starting point: a full-PK-keyed DELETE for
// the current row, opened in $EDITOR for review — :wq confirms and runs it, :q!
// (or an emptied buffer) aborts. Never a bare DELETE.
func buildDeleteStmt(eng db.Engine, table db.TableRef, keys []keyPred) editorSeed {
	preds := make([]string, len(keys))
	for i, k := range keys {
		preds[i] = eng.QuoteIdent(k.col) + " = " + sqlLiteral(k.val)
	}
	sql := fmt.Sprintf("DELETE FROM %s WHERE %s;\n", eng.QualifiedName(table), strings.Join(preds, " AND "))
	return editorSeed{sql: sql, line: 1, col: 1, kind: selectNone}
}

// selectTemplate is the S starting point: a bounded select of the current table.
func selectTemplate(eng db.Engine, t db.TableRef) string {
	return fmt.Sprintf("SELECT * FROM %s LIMIT 100;\n", eng.QualifiedName(t))
}

// leadingKeyword returns the upper-cased first SQL keyword, skipping leading
// line comments (-- …) and whitespace.
func leadingKeyword(sql string) string {
	for {
		sql = strings.TrimLeft(sql, " \t\r\n")
		if strings.HasPrefix(sql, "--") {
			i := strings.IndexByte(sql, '\n')
			if i < 0 {
				return ""
			}
			sql = sql[i+1:]
			continue
		}
		break
	}
	end := len(sql)
	for i, r := range sql {
		if strings.ContainsRune(" \t\r\n(;", r) {
			end = i
			break
		}
	}
	return strings.ToUpper(sql[:end])
}

// isReadSQL reports whether a free-form statement (s/S) is a read that should
// display its rows, vs a mutation that runs via Exec. Errs safe: only plainly
// read-only leading verbs count — notably WITH does not, since a data-modifying
// CTE (WITH … DELETE) also leads with WITH, and it must route to the write path.
func isReadSQL(sql string) bool {
	switch leadingKeyword(sql) {
	case "SELECT", "VALUES", "TABLE", "SHOW", "EXPLAIN", "PRAGMA", "DESCRIBE", "DESC":
		return true
	}
	return false
}

// isMultiStatement reports whether sql carries more than one statement (a ';'
// before the final one). Used to force a safe-mode confirmation on a "read" that
// smuggles a trailing write — e.g. "SELECT 1; DELETE FROM users;", which
// isReadSQL (leading keyword only) would otherwise wave through unconfirmed. It
// over-counts a ';' inside a string literal, but on a safe connection an extra
// confirmation is harmless.
func isMultiStatement(sql string) bool {
	s := strings.TrimRight(strings.TrimSpace(stripSQLComments(sql)), "; \t\r\n")
	return strings.Contains(s, ";")
}

// stripSQLComments drops -- line comments and blank lines. Used only to detect an
// emptied editor buffer (cleared → abort); the statement that actually runs is
// the file's full contents.
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		b.WriteString(t)
		b.WriteByte('\n')
	}
	return b.String()
}
