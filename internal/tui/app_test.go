package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/db"
)

// TestBrowseFlow drives the whole model headlessly: connect → list tables →
// Enter loads the table → the grid renders real rows.
func TestBrowseFlow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'), ('Linus')`); err != nil {
		t.Fatal(err)
	}
	e.Close()

	app := New(nil, path, "test")

	// Init dispatches connectCmd; run it and feed the result back.
	msg := app.Init()()
	if _, ok := msg.(connectedMsg); !ok {
		t.Fatalf("expected connectedMsg, got %T (%+v)", msg, msg)
	}
	app = update(t, app, msg)
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	if len(app.sidebar.tables) != 1 {
		t.Fatalf("sidebar should list 1 table, got %d", len(app.sidebar.tables))
	}

	// Enter on the sidebar loads the table.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter on sidebar should return a load command")
	}
	app = update(t, app, cmd())

	view := app.View()
	for _, want := range []string{"id", "name", "Ada", "Linus"} {
		if !strings.Contains(view, want) {
			t.Fatalf("grid view missing %q:\n%s", want, view)
		}
	}
	if app.showSidebar {
		t.Fatal("sidebar should auto-hide after selecting a table")
	}
}

func update(t *testing.T, app App, msg tea.Msg) App {
	t.Helper()
	m, _ := app.Update(msg)
	return m.(App)
}

// TestSortUsesCurrentColumn guards the bug where J/K always sorted column 0
// because a re-sort reset the cursor.
func TestSortUsesCurrentColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name, email) VALUES ('Ada','a'),('Linus','b')`)
	e.Close()

	app := New(nil, path, "test")
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	// Load table.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	// Move cursor to the 2nd column ("name") and sort descending with K.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if got, _ := app.grid.currentColName(); got != "name" {
		t.Fatalf("after moving right, current column = %q, want name", got)
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
	app = m.(App)
	if app.sortCol != "name" || app.sortAsc {
		t.Fatalf("K should sort current column name desc; got %q asc=%v", app.sortCol, app.sortAsc)
	}
	if cmd == nil {
		t.Fatal("K should trigger a reload command")
	}
	app = update(t, app, cmd())
	// Cursor column preserved across the re-sort, and header marks "name".
	if app.grid.cursorC != 1 {
		t.Fatalf("cursor column not preserved across re-sort: %d", app.grid.cursorC)
	}
	if app.grid.sortCol != "name" {
		t.Fatalf("header sort marker = %q, want name", app.grid.sortCol)
	}
}

// TestColumnFilter drives the two-phase filter: type a pattern on a column,
// commit with Enter, and verify the server re-query narrows the rows.
func TestColumnFilter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus'),('Grace')`)
	e.Close()

	app := New(nil, path, "test")
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter}) // load users
	app = m.(App)
	app = update(t, app, cmd())

	// Move to the "name" column, open the filter, type "%a%" (contains 'a').
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if app.grid.filtering != 1 {
		t.Fatalf("filtering column = %d, want 1", app.grid.filtering)
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("%a%")})
	// Live preview (client-side): Ada + Grace match, Linus doesn't.
	if len(app.grid.visible) != 2 {
		t.Fatalf("live preview matched %d rows, want 2", len(app.grid.visible))
	}

	// Commit → server re-query.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("committing a filter should reload")
	}
	app = update(t, app, cmd())
	if len(app.grid.rows) != 2 {
		t.Fatalf("server filter returned %d rows, want 2", len(app.grid.rows))
	}
	view := app.View()
	if strings.Contains(view, "Linus") {
		t.Fatalf("filtered view should not contain Linus:\n%s", view)
	}
}

// TestContinuousScroll verifies that reaching the loaded edge fetches more rows.
func TestContinuousScroll(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE nums (id INTEGER PRIMARY KEY, v INTEGER)`)
	for i := 0; i < 500; i++ {
		e.Exec(ctx, `INSERT INTO nums (v) VALUES (?)`, i)
	}
	e.Close()

	app := New(nil, path, "test")
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	initial := len(app.grid.rows)
	if initial == 0 || !app.grid.hasMore {
		t.Fatalf("initial load = %d rows, hasMore=%v", initial, app.grid.hasMore)
	}

	// Jump to the bottom of the loaded window → should trigger a fetch.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("G at the loaded edge should trigger a load-more fetch")
	}
	app = update(t, app, cmd())
	if len(app.grid.rows) <= initial {
		t.Fatalf("rows did not grow after scroll: %d → %d", initial, len(app.grid.rows))
	}
}

func TestDatabaseName(t *testing.T) {
	cases := map[string]string{
		"./demo.db":                    "demo",
		"sqlite:///Users/jm/notes.db":  "notes",
		"postgres://jm@host:5432/appdev?sslmode=disable": "appdev",
		"mysql://jm@localhost:3306/scratch":              "scratch",
	}
	for dsn, want := range cases {
		if got := db.DatabaseName(dsn); got != want {
			t.Errorf("DatabaseName(%q) = %q, want %q", dsn, got, want)
		}
	}
}
