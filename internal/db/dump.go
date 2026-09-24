package db

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// This file is the shared half of the SQL export (`x` on the table list): the
// streaming reader every engine inherits, and the literal rendering the three
// dialects specialise. The dialect-specific halves live in each engine file
// (SQLLiteral / DumpColumns / DumpInsert / StructureSQL / DumpPrologue /
// DumpEpilogue).
//
// A dump INLINES its values rather than binding them — the output is a file of
// statements for another client to run, not something jsq executes, so the
// bind-values invariant doesn't apply to it. What jsq itself runs here is the
// `SELECT * FROM <quoted>` behind the stream, which carries no values at all.

// RowStream is Stream's callback pair: Head once with the result's column names
// and their database type names as the driver reports them ("VARCHAR", "BLOB",
// "bytea"), then Row once per row, in order.
//
// A row's []byte values are handed over RAW, unlike scanQuery's — which turns
// every []byte into a string, since the grid only ever displays them. A dump has
// to tell a BLOB from a VARCHAR, and MySQL's driver returns both as []byte, so
// the type name from Head is the only thing that separates them.
type RowStream struct {
	Head func(cols, types []string) error
	Row  func(vals []any) error
}

// Stream runs a query and feeds it to s row by row, holding at most one row in
// memory. It is the read behind an export: a full table can be far larger than
// the grid's windows, so it never materialises a ResultSet the way Query does.
// An error from either callback aborts the scan and comes back unwrapped, as
// does a cancelled ctx.
func (e *stdEngine) Stream(ctx context.Context, query string, args []any, s RowStream) error {
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	types := make([]string, len(cols))
	if ct, err := rows.ColumnTypes(); err == nil {
		for i, c := range ct {
			types[i] = c.DatabaseTypeName()
		}
	}
	if s.Head != nil {
		if err := s.Head(cols, types); err != nil {
			return err
		}
	}

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if err := s.Row(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// sqlQuote renders s as a single-quoted SQL string literal, doubling embedded
// quotes.
//
// escBackslash additionally doubles backslashes. MySQL needs that — a backslash
// is an escape character there unless NO_BACKSLASH_ESCAPES is set. Postgres must
// NOT have it: the dump sets standard_conforming_strings=on, which makes a
// backslash an ordinary character, so doubling one would write two where the
// data had one. (That asymmetry is exactly what a literal shared between the two
// dialects gets wrong, so it's a parameter rather than a default.)
func sqlQuote(s string, escBackslash bool) string {
	if escBackslash {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// stdSQLLiteral renders the values every dialect writes the same way: NULL,
// numerics bare, times as a quoted standard datetime, and anything else through
// its %v form, quoted. []byte, bool and binary strings are dialect-specific and
// are handled by the engine before it falls back here.
func stdSQLLiteral(v any, escBackslash bool) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprintf("%v", x)
	case time.Time:
		// fmt's default (`… +0000 UTC`) is not a valid SQL literal. The dump pins
		// the session to UTC, so write the instant in it.
		return "'" + x.UTC().Format("2006-01-02 15:04:05.999999") + "'"
	default:
		return sqlQuote(fmt.Sprintf("%v", x), escBackslash)
	}
}

// binaryBytes reports whether b must be written as a binary literal rather than
// a quoted string: its column is a binary type, or the bytes simply aren't text
// (invalid UTF-8, or an embedded NUL — which no dialect accepts inside a quoted
// literal). MySQL hands back []byte for VARCHAR as readily as for BLOB, so the
// column type is what separates them; the content checks catch the rest.
func binaryBytes(b []byte, colType string) bool {
	return isBinaryType(colType) || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0
}

// isBinaryType reports whether a driver type name names a binary column.
func isBinaryType(name string) bool {
	up := strings.ToUpper(name)
	for _, s := range []string{"BLOB", "BINARY", "BYTEA", "GEOMETRY"} {
		if strings.Contains(up, s) {
			return true
		}
	}
	return false
}

// hexBytes is the hex body of a binary literal (prefix and quoting are the
// engine's, since all three spell it differently: 0x… / '\x…'::bytea / X'…').
func hexBytes(b []byte) string { return hex.EncodeToString(b) }

// insertHead is "INSERT INTO <qual> (<cols>) <mid>VALUES (" — the part of a
// dump's INSERT every dialect spells alike. mid is Postgres's OVERRIDING clause.
func insertHead(quote func(string) string, qual string, cols []string, mid string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quote(c)
	}
	return "INSERT INTO " + qual + " (" + strings.Join(quoted, ", ") + ") " + mid + "VALUES ("
}

// onConflictTail is the Postgres/SQLite upsert: on a primary-key clash the row
// is overwritten with the dumped values. A table whose columns are all key has
// nothing to overwrite, and one with no primary key has nothing to key on — both
// fall back to DO NOTHING, which still makes a rerun skip what a unique
// constraint already holds rather than fail on it.
func onConflictTail(quote func(string) string, cols, pk []string, upsert bool) string {
	if !upsert {
		return ");\n"
	}
	var sets []string
	for _, c := range nonKey(cols, pk) {
		sets = append(sets, quote(c)+" = EXCLUDED."+quote(c))
	}
	if len(pk) == 0 || len(sets) == 0 {
		return ") ON CONFLICT DO NOTHING;\n"
	}
	quoted := make([]string, len(pk))
	for i, c := range pk {
		quoted[i] = quote(c)
	}
	return ") ON CONFLICT (" + strings.Join(quoted, ", ") + ") DO UPDATE SET " + strings.Join(sets, ", ") + ";\n"
}

// nonKey is cols minus the primary-key columns, in order.
func nonKey(cols, pk []string) []string {
	key := map[string]bool{}
	for _, c := range pk {
		key[c] = true
	}
	var out []string
	for _, c := range cols {
		if !key[c] {
			out = append(out, c)
		}
	}
	return out
}
