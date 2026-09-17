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
	got, err := e.StructureSQL(context.Background(), TableRef{Name: "users"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `DROP TABLE IF EXISTS "users";`) {
		t.Errorf("structure does not start with the DROP:\n%s", got)
	}
	if !strings.Contains(got, "CREATE TABLE users") {
		t.Errorf("structure carries no CREATE TABLE:\n%s", got)
	}
}
