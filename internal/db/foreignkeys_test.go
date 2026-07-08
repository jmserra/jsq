package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteForeignKeys(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	for _, stmt := range []string{
		`CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY,
		   author_id INTEGER REFERENCES authors(id), title TEXT)`,
		`CREATE TABLE reviews (id INTEGER PRIMARY KEY,
		   book_id INTEGER REFERENCES books, body TEXT)`, // column-less: implicit PK
	} {
		if _, err := e.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	fks, err := e.ForeignKeys(ctx, TableRef{Name: "books"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fks) != 1 {
		t.Fatalf("want 1 fk on books, got %d: %+v", len(fks), fks)
	}
	fk := fks[0]
	if len(fk.Columns) != 1 || fk.Columns[0] != "author_id" ||
		fk.RefTable.Name != "authors" || fk.RefColumns[0] != "id" {
		t.Fatalf("unexpected fk: %+v", fk)
	}

	// The column-less `REFERENCES books` must resolve to books' primary key.
	rfks, err := e.ForeignKeys(ctx, TableRef{Name: "reviews"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rfks) != 1 || rfks[0].RefTable.Name != "books" || rfks[0].RefColumns[0] != "id" {
		t.Fatalf("implicit-PK ref not resolved: %+v", rfks)
	}

	// A table with no FKs returns none.
	if got, _ := e.ForeignKeys(ctx, TableRef{Name: "authors"}); len(got) != 0 {
		t.Fatalf("authors should have no FKs, got %+v", got)
	}
}

// TestSQLiteCompositeForeignKeyOrder: a composite FK's local and referenced
// columns come back paired in definition (seq) order, so `follow` builds correct
// predicates. Guards the explicit (id, seq) sort in ForeignKeys.
func TestSQLiteCompositeForeignKeyOrder(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, filepath.Join(t.TempDir(), "cfk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	for _, stmt := range []string{
		`CREATE TABLE parent (a INTEGER, b INTEGER, PRIMARY KEY (a, b))`,
		`CREATE TABLE child (x INTEGER, y INTEGER,
		   FOREIGN KEY (x, y) REFERENCES parent(a, b))`,
	} {
		if _, err := e.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	fks, err := e.ForeignKeys(ctx, TableRef{Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fks) != 1 {
		t.Fatalf("want 1 composite fk, got %d: %+v", len(fks), fks)
	}
	fk := fks[0]
	if len(fk.Columns) != 2 || fk.Columns[0] != "x" || fk.Columns[1] != "y" {
		t.Fatalf("local columns out of order: %+v", fk.Columns)
	}
	if len(fk.RefColumns) != 2 || fk.RefColumns[0] != "a" || fk.RefColumns[1] != "b" {
		t.Fatalf("ref columns out of order: %+v", fk.RefColumns)
	}
}
