package tui

import (
	"fmt"
	"strings"
	"time"

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
	case time.Time:
		// fmt's default (`… +0000 UTC`) is not a valid SQL literal; render a
		// standard datetime string instead so the seed is runnable as-is.
		return "'" + x.Format("2006-01-02 15:04:05.999999999") + "'"
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
	selectWord                           // the word under the cursor, whatever quotes it sits in
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

	// scratch marks a free-form s query with no table (the table-list `s`): on
	// submit it's still recorded in the connection's `b` history even though
	// there's no table to key a last-query against.
	scratch bool

	// after says what this write should refresh when it lands — the user list or
	// a grants view rather than the pane's table. It rides all the way through
	// editorSubmitMsg → execRawCmd → execDoneMsg, so a statement reopened from
	// the error modal still refreshes the right thing.
	after afterWrite
}

// afterWrite is what a full-path write refreshes once it has run.
type afterWrite int

const (
	afterWriteView   afterWrite = iota // the pane's own view (E/o/D/p/s — the default)
	afterWriteUsers                    // the user list (create user)
	afterWriteGrants                   // the grants view the write came from (grant/revoke)
)

// inlinedKeyPreds renders the "col = literal" WHERE predicates for a full-PK
// keyed statement, values inlined via sqlLiteral. Shared by the E/D full paths
// and the safe-mode quick-edit preview — all display/user-authored SQL, the
// documented invariant-5 exception (the executed quick path binds instead).
func inlinedKeyPreds(eng db.Engine, keys []keyPred) []string {
	preds := make([]string, len(keys))
	for i, k := range keys {
		preds[i] = eng.QuoteIdent(k.col) + " = " + sqlLiteral(k.val)
	}
	return preds
}

// previewEditSQL renders the quick-path (e) UPDATE with its values inlined, for
// the safe-mode confirmation overlay only. The statement that actually runs
// (execEditCmd) binds the same values as parameters — this is display text, not
// what's executed (cf. invariant 5).
func previewEditSQL(eng db.Engine, req editReq) string {
	newVal := sqlLiteral(req.val)
	if req.null {
		newVal = "NULL"
	}
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s",
		eng.QualifiedName(req.table), eng.QuoteIdent(req.col), newVal,
		strings.Join(inlinedKeyPreds(eng, req.keys), " AND "))
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

	sql := setLine + "\nWHERE " + strings.Join(inlinedKeyPreds(eng, keys), " AND ") + ";\n"

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
	sql := fmt.Sprintf("DELETE FROM %s WHERE %s;\n",
		eng.QualifiedName(table), strings.Join(inlinedKeyPreds(eng, keys), " AND "))
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

// newUserName is the placeholder account name in the create-user template — what the
// cursor lands on, pre-selected, so the first thing you type replaces it.
const newUserName = "newuser"

// buildCreateUserStmt is the create-user starting point (`o` on the user list,
// the same key as the grid's insert-a-row): the engine's
// template for a new account, headed by the connection and database it will be
// created on. Nothing here runs on its own — it opens in $EDITOR like every other
// full path, and :wq runs whatever the user made of it (on a safe connection,
// after the y/n confirmation), which is why the template can be opinionated about
// a narrow starting grant without deciding anything.
//
// Empty SQL means the engine has no users; the caller reports that.
func buildCreateUserStmt(eng db.Engine, connName, dbName string) editorSeed {
	body := eng.CreateUserSQL(newUserName, dbName)
	if body == "" {
		return editorSeed{}
	}
	// Where it will run — connection · database, skipping either if unnamed (a
	// direct DSN has no connection name, the way the status line shows "adhoc").
	var where []string
	for _, part := range []string{connName, dbName} {
		if part != "" {
			where = append(where, part)
		}
	}
	head := "-- new user"
	if len(where) > 0 {
		head += " on " + strings.Join(where, " · ")
	}
	head += " — edit, then :wq to run (:q! aborts)\n" +
		"-- ⚠ change the name and the password first: this runs exactly as written\n"
	sql := head + body

	// Land on the placeholder name and select it. It sits inside quotes on MySQL
	// and inside a quoted identifier on Postgres, so the selection is by word
	// rather than by quote — one kind covers both.
	line, col := locate(sql, newUserName)
	return editorSeed{sql: sql, line: line, col: col, kind: selectWord, after: afterWriteUsers}
}

// buildDropUserStmt is the `D` starting point on the user list: the statement
// that removes the selected account, headed by a warning naming it. Like every
// full path it only opens the editor — the drop happens when you :wq it (and,
// on a safe connection, confirm it). No selection: the statement is exact.
func buildDropUserStmt(eng db.Engine, u db.User) editorSeed {
	body := eng.DropUserSQL(u)
	if body == "" {
		return editorSeed{}
	}
	sql := "-- ⚠ DROP the user " + u.Label() + " — this cannot be undone; :q! aborts\n" + body
	return editorSeed{sql: sql, line: 2, col: 1, after: afterWriteUsers}
}

// buildGrantStmt is the `o` starting point on a grants view: the engine's GRANT
// template for the user whose privileges are on screen, with the privilege word
// selected — that is the word you are there to change. Empty SQL means the engine
// has nothing to template.
func buildGrantStmt(eng db.Engine, u db.User, dbName string) editorSeed {
	body := eng.GrantSQL(u, dbName)
	if body == "" {
		return editorSeed{}
	}
	sql := "-- more privileges for " + u.Label() + " — edit, then :wq to run (:q! aborts)\n" + body
	line, col := locate(sql, "GRANT ")
	return editorSeed{sql: sql, line: line, col: col + len("GRANT "), kind: selectWord, after: afterWriteGrants}
}

// buildRevokeStmt is the `D` starting point on a grants view: the statement that
// takes back the row under the cursor, derived from the row itself (scope +
// object + privilege). No selection — the statement is already exact; it opens
// for review, not for filling in. Empty SQL means the row is not revocable.
func buildRevokeStmt(eng db.Engine, u db.User, g db.Grant) editorSeed {
	body := eng.RevokeSQL(u, g)
	if body == "" {
		return editorSeed{}
	}
	what := g.Privilege
	if g.Object != "" {
		what += " on " + g.Object
	}
	sql := "-- take " + what + " away from " + u.Label() + " — :wq to run, :q! aborts\n" + body
	return editorSeed{sql: sql, line: 2, col: 1, after: afterWriteGrants}
}

// locate returns the 1-based line and byte column of the first occurrence of
// needle in sql (0, 0 when it does not occur).
func locate(sql, needle string) (int, int) {
	for i, l := range strings.Split(sql, "\n") {
		if c := strings.Index(l, needle); c >= 0 {
			return i + 1, c + 1
		}
	}
	return 0, 0
}
