package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// TestFollowForeignKey drives the whole follow flow: load books, put the cursor
// on the author_id FK column, press f, and confirm the grid switches to authors
// filtered to the referenced row.
func TestFollowForeignKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO authors (id, name) VALUES (1, 'Ada'), (2, 'Linus')`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY,
		   author_id INTEGER REFERENCES authors(id), title TEXT)`,
		`INSERT INTO books (id, author_id, title) VALUES (1, 2, 'Kernel Notes')`,
	} {
		if _, err := e.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "t"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 100, Height: 24})

	// Load the books table (it sorts after authors alphabetically).
	app.currentTable = db.Table{Name: "books"}
	m, cmd := app.selectTable(app.currentTable)
	app = m.(App)
	app = update(t, app, cmd())
	if got := app.grid.cols[1].name; got != "author_id" {
		t.Fatalf("expected column 1 to be author_id, got %q", got)
	}

	// Move the cursor onto author_id and follow.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("f should dispatch a follow command")
	}

	// followFKCmd → followReadyMsg → loadCurrentCmd → rowsMsg.
	msg := cmd()
	ready, ok := msg.(followReadyMsg)
	if !ok {
		t.Fatalf("expected followReadyMsg, got %T (%+v)", msg, msg)
	}
	if ready.refTable.Name != "authors" || len(ready.preds) != 1 ||
		ready.preds[0].col != "id" || fmt.Sprint(ready.preds[0].val) != "2" {
		t.Fatalf("unexpected follow target: %+v", ready)
	}
	m, cmd = app.Update(ready)
	app = m.(App)
	app = update(t, app, cmd())

	// The grid now shows authors, filtered to id = 2 (Linus only).
	if app.currentTable.Name != "authors" {
		t.Fatalf("expected to be on authors, got %q", app.currentTable.Name)
	}
	if n := len(app.grid.rows); n != 1 {
		t.Fatalf("expected 1 row (the referenced author), got %d", n)
	}
	view := app.View()
	if !strings.Contains(view, "Linus") || strings.Contains(view, "Ada") {
		t.Fatalf("followed view should show only Linus:\n%s", view)
	}
	if !strings.Contains(app.status, "id = 2") {
		t.Fatalf("status should note the FK predicate, got %q", app.status)
	}
}

// TestFollowNoForeignKey confirms following a non-FK column just notices.
func TestFollowNoForeignKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nofk.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO t (name) VALUES ('x')`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "t"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	app.currentTable = db.Table{Name: "t"}
	m, cmd := app.selectTable(app.currentTable)
	app = m.(App)
	app = update(t, app, cmd())

	// Cursor on the "name" column, which has no FK.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	_, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if _, ok := cmd().(noticeMsg); !ok {
		t.Fatal("following a non-FK column should return a noticeMsg")
	}
}
