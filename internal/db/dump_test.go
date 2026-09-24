package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamRawBytesAndTypes(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	mustExec(t, e, `CREATE TABLE b (id INTEGER PRIMARY KEY, txt TEXT, raw BLOB)`)
	mustExec(t, e, `INSERT INTO b (txt, raw) VALUES ('hi', X'00ff'), (NULL, NULL)`)

	var cols, types []string
	var rows [][]any
	err = e.Stream(ctx, `SELECT id, txt, raw FROM b ORDER BY id`, nil, RowStream{
		Head: func(c, ty []string) error { cols, types = c, ty; return nil },
		Row: func(v []any) error {
			cp := make([]any, len(v))
			copy(cp, v)
			rows = append(rows, cp)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cols, ",") != "id,txt,raw" {
		t.Fatalf("cols = %v", cols)
	}
	if len(types) != 3 || !isBinaryType(types[2]) {
		t.Errorf("types = %v — want the blob column reported as binary", types)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// The blob must arrive as raw bytes, NOT stringified the way scanQuery does
	// it — that distinction is the whole reason Stream exists.
	if b, ok := rows[0][2].([]byte); !ok || len(b) != 2 || b[0] != 0x00 || b[1] != 0xff {
		t.Errorf("raw = %#v — want []byte{0x00,0xff}", rows[0][2])
	}
	if rows[1][1] != nil || rows[1][2] != nil {
		t.Errorf("NULLs came back as %#v / %#v", rows[1][1], rows[1][2])
	}
}

func TestStreamRowErrorAborts(t *testing.T) {
	e := newTestDB(t)
	n := 0
	err := e.Stream(context.Background(), `SELECT * FROM users`, nil, RowStream{
		Row: func([]any) error { n++; return context.Canceled },
	})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n != 1 {
		t.Errorf("scanned %d rows, want to stop at the first", n)
	}
}

func TestSQLiteLiterals(t *testing.T) {
	e := newTestDB(t)
	for _, c := range []struct {
		v       any
		colType string
		want    string
	}{
		{nil, "TEXT", "NULL"},
		{int64(42), "INTEGER", "42"},
		{true, "INTEGER", "1"},
		{"it's", "TEXT", `'it''s'`},
		// SQLite string literals have no escape character, so a backslash is
		// itself — doubling it would write two where the data had one.
		{`C:\tmp`, "TEXT", `'C:\tmp'`},
		{[]byte{0xde, 0xad}, "BLOB", `X'dead'`},
		{[]byte{}, "BLOB", `X''`},
		{[]byte("plain"), "TEXT", `'plain'`},
	} {
		if got := e.SQLLiteral(c.v, c.colType); got != c.want {
			t.Errorf("SQLLiteral(%#v, %q) = %s, want %s", c.v, c.colType, got, c.want)
		}
	}
}

func TestBinaryBytesDetection(t *testing.T) {
	// A MySQL VARCHAR and a MySQL BLOB both arrive as []byte; only the type name
	// separates them — except when the content itself can't be a text literal.
	if binaryBytes([]byte("héllo"), "VARCHAR") {
		t.Error("utf-8 text in a VARCHAR treated as binary")
	}
	if !binaryBytes([]byte("héllo"), "BLOB") {
		t.Error("text in a BLOB must still dump as binary")
	}
	if !binaryBytes([]byte{0xff, 0xfe}, "VARCHAR") {
		t.Error("invalid utf-8 must dump as binary whatever the column says")
	}
	if !binaryBytes([]byte("a\x00b"), "TEXT") {
		t.Error("an embedded NUL must dump as binary — no dialect quotes one")
	}
}

func TestSQLiteStructureSQL(t *testing.T) {
	e := newTestDB(t)
	mustExec(t, e, `CREATE INDEX users_name ON users (name)`)
	got, err := e.StructureSQL(context.Background(), TableRef{Name: "users"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Create, `DROP TABLE IF EXISTS "users";`) {
		t.Errorf("structure does not start with the DROP:\n%s", got.Create)
	}
	if !strings.Contains(got.Create, "CREATE TABLE users") {
		t.Errorf("structure carries no CREATE TABLE:\n%s", got.Create)
	}
	// The index is dropped with the table, so it has to come back after the rows.
	if got.After != "CREATE INDEX users_name ON users (name);\n" {
		t.Errorf("After = %q, want the index", got.After)
	}
}

// TestSQLiteDumpColumnsSkipsGenerated: a generated column rejects an explicit
// value, so the dump neither reads nor inserts it.
func TestSQLiteDumpColumnsSkipsGenerated(t *testing.T) {
	e := newTestDB(t)
	mustExec(t, e, `CREATE TABLE g (id INTEGER PRIMARY KEY, a INT, b INT GENERATED ALWAYS AS (a * 2) STORED, c INT)`)
	cols, err := e.DumpColumns(context.Background(), TableRef{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cols, ",") != "id,a,c" {
		t.Errorf("DumpColumns = %v, want [id a c]", cols)
	}
}

// TestDumpInsertUpsert pins each dialect's upsert, including the two fallbacks:
// no primary key, and nothing but key columns.
func TestDumpInsertUpsert(t *testing.T) {
	lite, pg, my := &sqliteEngine{}, &pgEngine{}, &myEngine{}
	cols, pk := []string{"id", "name"}, []string{"id"}
	for _, c := range []struct {
		name       string
		e          Engine
		cols, pk   []string
		upsert     bool
		head, tail string
	}{
		{"plain", lite, cols, pk, false, `INSERT INTO t ("id", "name") VALUES (`, ");\n"},
		{"sqlite", lite, cols, pk, true, `INSERT INTO t ("id", "name") VALUES (`,
			`) ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name";` + "\n"},
		{"sqlite no pk", lite, cols, nil, true, "", ") ON CONFLICT DO NOTHING;\n"},
		{"sqlite all key", lite, []string{"a", "b"}, []string{"a", "b"}, true, "", ") ON CONFLICT DO NOTHING;\n"},
		{"pg", pg, cols, pk, true, `INSERT INTO t ("id", "name") OVERRIDING SYSTEM VALUE VALUES (`,
			`) ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name";` + "\n"},
		{"mysql", my, cols, pk, true, "INSERT INTO t (`id`, `name`) VALUES (",
			") ON DUPLICATE KEY UPDATE `name` = VALUES(`name`);\n"},
		{"mysql no pk", my, cols, nil, true, "",
			") ON DUPLICATE KEY UPDATE `id` = VALUES(`id`), `name` = VALUES(`name`);\n"},
		{"mysql all key", my, []string{"a"}, []string{"a"}, true, "", ") ON DUPLICATE KEY UPDATE `a` = `a`;\n"},
	} {
		head, tail := c.e.DumpInsert("t", c.cols, c.pk, c.upsert)
		if c.head != "" && head != c.head {
			t.Errorf("%s: head = %q, want %q", c.name, head, c.head)
		}
		if tail != c.tail {
			t.Errorf("%s: tail = %q, want %q", c.name, tail, c.tail)
		}
	}
}
