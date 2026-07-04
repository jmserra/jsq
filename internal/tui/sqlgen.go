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

// updateSeed is a generated UPDATE plus where its value sits, so the editor can
// open with the cursor on the value and the value pre-selected in Visual mode.
type updateSeed struct {
	sql  string
	line int // 1-based cursor line of the value
	col  int // 1-based byte column to place the cursor
	kind selectKind
}

// buildUpdateStmt is the E full-path starting point (the editing model in
// README): a full-PK-keyed UPDATE with the current value inlined at the end of
// the SET line so the editor can drop you straight onto it. To set NULL, change
// the value to a bare NULL; to abort, clear the buffer or :q!.
func buildUpdateStmt(eng db.Engine, table db.TableRef, col string, val any, keys []keyPred) updateSeed {
	lit := sqlLiteral(val)
	setLine := fmt.Sprintf("UPDATE %s SET %s = %s",
		eng.QualifiedName(table), eng.QuoteIdent(col), lit)
	litStart := len(setLine) - len(lit) + 1 // 1-based byte col of the literal

	preds := make([]string, len(keys))
	for i, k := range keys {
		preds[i] = eng.QuoteIdent(k.col) + " = " + sqlLiteral(k.val)
	}
	sql := setLine + "\nWHERE " + strings.Join(preds, " AND ") + ";\n"

	seed := updateSeed{sql: sql, line: 1, col: litStart, kind: selectToken}
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
