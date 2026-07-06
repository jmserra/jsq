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

	// Move the cursor onto author_id and follow. Resolution is synchronous (FKs
	// came with the load), so f navigates immediately and returns the reload cmd.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("f should dispatch a reload command")
	}
	if app.currentTable.Name != "authors" || len(app.basePreds) != 1 ||
		app.basePreds[0].col != "id" || fmt.Sprint(app.basePreds[0].val) != "2" {
		t.Fatalf("unexpected follow target: table=%q preds=%+v", app.currentTable.Name, app.basePreds)
	}
	app = update(t, app, cmd()) // rowsMsg

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

// TestJumplistBackForward follows a FK, then walks back (Ctrl-O) and forward.
func TestJumplistBackForward(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")
	e, _ := db.Open(ctx, path)
	for _, stmt := range []string{
		`CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO authors (id, name) VALUES (1, 'Ada'), (2, 'Linus')`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY, author_id INTEGER REFERENCES authors(id))`,
		`INSERT INTO books (id, author_id) VALUES (1, 2)`,
	} {
		if _, err := e.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "t"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 100, Height: 24})

	// Land on books, then follow author_id → authors (id = 2).
	m, cmd := app.selectTable(db.Table{Name: "books"})
	app = update(t, m.(App), cmd())
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	app = m.(App)
	app = update(t, app, cmd()) // rowsMsg
	if app.currentTable.Name != "authors" || app.baseNote == "" {
		t.Fatalf("expected filtered authors, got %q note=%q", app.currentTable.Name, app.baseNote)
	}

	// Ctrl-O → back to books (unfiltered).
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Ctrl-O should reload the previous view")
	}
	app = update(t, app, cmd())
	if app.currentTable.Name != "books" || app.baseNote != "" || len(app.basePreds) != 0 {
		t.Fatalf("back should land on unfiltered books, got %q note=%q", app.currentTable.Name, app.baseNote)
	}

	// Forward → authors filtered again.
	m, cmd = app.jumpBy(1)
	app = m.(App)
	if cmd == nil {
		t.Fatal("forward should reload the followed view")
	}
	app = update(t, app, cmd())
	if app.currentTable.Name != "authors" || len(app.basePreds) != 1 {
		t.Fatalf("forward should restore filtered authors, got %q preds=%v", app.currentTable.Name, app.basePreds)
	}

	// Nothing further forward.
	if _, c := app.jumpBy(1); c != nil {
		t.Fatal("no forward view should remain")
	}

	// While browsing the grid (sidebar hidden), Tab steps forward — this is where
	// a kitty Ctrl-I lands too. Go back first so there's a forward view.
	m, cmd = app.jumpBy(-1)
	app = update(t, m.(App), cmd())
	if app.currentTable.Name != "books" {
		t.Fatalf("setup: expected books, got %q", app.currentTable.Name)
	}
	if app.screen != screenBrowse {
		t.Fatal("should be on the grid screen while browsing")
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyTab})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Tab in the grid should step the jumplist forward")
	}
	app = update(t, app, cmd())
	if app.currentTable.Name != "authors" {
		t.Fatalf("Tab-forward should reach authors, got %q", app.currentTable.Name)
	}
}

// TestJumplistPicker opens the ` picker and jumps to an arbitrary entry.
func TestJumplistPicker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "j.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE a (id INTEGER PRIMARY KEY)`)
	e.Exec(ctx, `CREATE TABLE b (id INTEGER PRIMARY KEY)`)
	e.Exec(ctx, `CREATE TABLE c (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "t"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, name := range []string{"a", "b", "c"} {
		m, cmd := app.selectTable(db.Table{Name: name})
		app = update(t, m.(App), cmd())
	}
	if app.viewIdx != 2 || len(app.views) != 3 {
		t.Fatalf("expected 3 views at idx 2, got idx=%d len=%d", app.viewIdx, len(app.views))
	}

	// Open the picker, move to the first entry (a), and jump.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("`")})
	if !app.jumps.active || len(app.jumps.entries) != 3 {
		t.Fatalf("picker should be open with 3 entries, got active=%v n=%d", app.jumps.active, len(app.jumps.entries))
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")}) // top
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if app.jumps.active {
		t.Fatal("Enter should close the picker")
	}
	app = update(t, app, cmd())
	if app.currentTable.Name != "a" || app.viewIdx != 0 {
		t.Fatalf("jumped to wrong view: table=%q idx=%d", app.currentTable.Name, app.viewIdx)
	}
	// Navigating from the middle truncates the forward tail.
	m, cmd = app.selectTable(db.Table{Name: "b"})
	app = update(t, m.(App), cmd())
	if len(app.views) != 2 || app.viewIdx != 1 {
		t.Fatalf("new jump from middle should truncate forward: len=%d idx=%d", len(app.views), app.viewIdx)
	}
}

// TestFKColumnMarker checks the header flags FK columns and only those.
func TestFKColumnMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")
	e, _ := db.Open(ctx, path)
	for _, s := range []string{
		`CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO authors (id, name) VALUES (1, 'Ada')`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY,
		   author_id INTEGER REFERENCES authors(id), title TEXT)`,
		`INSERT INTO books (id, author_id, title) VALUES (1, 1, 'X')`,
	} {
		if _, err := e.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "t"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 100, Height: 24})
	m, cmd := app.selectTable(db.Table{Name: "books"})
	app = update(t, m.(App), cmd())

	fk := map[string]bool{}
	for _, c := range app.grid.cols {
		fk[c.name] = c.fk
	}
	if !fk["author_id"] || fk["id"] || fk["title"] {
		t.Fatalf("only author_id should be marked FK: %+v", fk)
	}
	if h := app.grid.renderHeader(); !strings.Contains(h, "author_id"+fkMarker) {
		t.Fatalf("header missing FK marker on author_id:\n%s", h)
	}
	// A non-FK table shows no markers.
	m, cmd = app.selectTable(db.Table{Name: "authors"})
	app = update(t, m.(App), cmd())
	if strings.Contains(app.grid.renderHeader(), fkMarker) {
		t.Fatalf("authors header should carry no FK marker:\n%s", app.grid.renderHeader())
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

	// Cursor on the "name" column, which has no FK: f is a no-op with a notice
	// in the status and no navigation.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	app = m.(App)
	if cmd != nil {
		t.Fatal("following a non-FK column should not reload")
	}
	if !strings.Contains(app.status, "no foreign key") {
		t.Fatalf("expected a no-FK notice, got %q", app.status)
	}
}
