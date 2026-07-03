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

	app := New(nil, path, "test", false)

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

	app := New(nil, path, "test", false)
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

	app := New(nil, path, "test", false)
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

	app := New(nil, path, "test", false)
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

// TestSidebarFilter drives the table-list filter (§7): `/` narrows the list as
// you type, arrows move within matches, and Enter loads the highlighted table.
func TestSidebarFilter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"users", "orders", "order_items", "products"} {
		if _, err := e.Exec(ctx, "CREATE TABLE "+name+" (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()

	app := New(nil, path, "test", false)
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if len(app.sidebar.tables) != 4 {
		t.Fatalf("sidebar should list 4 tables, got %d", len(app.sidebar.tables))
	}

	// `/` enters filter mode; typing "order" narrows to orders + order_items.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !app.sidebar.filtering {
		t.Fatal("`/` should enter filter mode")
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("order")})
	if len(app.sidebar.visible) != 2 {
		t.Fatalf("filter %q matched %d tables, want 2", "order", len(app.sidebar.visible))
	}
	if t0, _ := app.sidebar.selected(); t0.Name != "order_items" {
		t.Fatalf("cursor should sit on first match order_items, got %q", t0.Name)
	}

	// Backspace re-widens the match set; a non-matching filter empties it.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if len(app.sidebar.visible) != 2 { // "orde" still matches both
		t.Fatalf("after backspace, matched %d, want 2", len(app.sidebar.visible))
	}

	// Arrow down moves to the second match, Enter loads it.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyDown})
	sel, _ := app.sidebar.selected()
	if sel.Name != "orders" {
		t.Fatalf("after ↓, selected = %q, want orders", sel.Name)
	}
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter in filter mode should load the highlighted table")
	}
	if app.sidebar.filtering {
		t.Fatal("Enter should leave filter mode")
	}
	app = update(t, app, cmd())
	if app.status != "orders" {
		t.Fatalf("loaded table = %q, want orders", app.status)
	}
	if app.showSidebar {
		t.Fatal("sidebar should auto-hide after loading a table")
	}

	// Reopening the sidebar keeps the filter active (narrowed, no longer typing).
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'H'}})
	if app.sidebar.filtering {
		t.Fatal("reopened sidebar should not be in typing mode")
	}
	if len(app.sidebar.visible) != 2 {
		t.Fatalf("reopened sidebar should keep the %q filter (2 matches), got %d", "orde", len(app.sidebar.visible))
	}
	// Esc in normal nav clears the active filter and restores the full list.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.sidebar.hasFilter() || len(app.sidebar.visible) != 4 {
		t.Fatalf("Esc should clear the active filter; hasFilter=%v visible=%d",
			app.sidebar.hasFilter(), len(app.sidebar.visible))
	}

	// Esc cancels an in-progress filter without loading anything.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("prod")})
	if len(app.sidebar.visible) != 1 {
		t.Fatalf("filter %q matched %d, want 1", "prod", len(app.sidebar.visible))
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.sidebar.filtering || len(app.sidebar.visible) != 4 {
		t.Fatalf("Esc should cancel filter and restore all 4 tables, filtering=%v visible=%d",
			app.sidebar.filtering, len(app.sidebar.visible))
	}
}

// loadTable is the shared setup: open a fresh sqlite db, run schema/seed, and
// drive the model up to a loaded grid.
func loadTable(t *testing.T, readOnly bool, seed func(e db.Engine)) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seed(e)
	e.Close()

	app := New(nil, path, "test", readOnly)
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter}) // load the (only) table
	app = m.(App)
	app = update(t, app, cmd())
	return app
}

func typeRunes(t *testing.T, app App, s string) App {
	t.Helper()
	for _, r := range s {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return app
}

// TestQuickEditCell drives the §8 quick path: `e` on a cell → overwrite the
// value → Enter runs the keyed UPDATE, the grid reflects it, and the DB is
// actually changed.
func TestQuickEditCell(t *testing.T) {
	app := loadTable(t, false, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// Default sort is PK descending → row 0 is id=2 (Linus). Move to "name".
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})

	// `e` opens the overlay pre-filled with the current value.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if !app.grid.editing {
		t.Fatal("`e` should start editing")
	}
	if app.grid.editVal != "Linus" {
		t.Fatalf("overlay pre-fill = %q, want Linus", app.grid.editVal)
	}

	// Clear "Linus" and type "Grace", then commit.
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "Grace")
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if app.grid.editing {
		t.Fatal("Enter should leave edit mode")
	}
	if cmd == nil {
		t.Fatal("committing a changed cell should run an UPDATE")
	}
	app = update(t, app, cmd())

	// Grid reflects the new value immediately.
	if got, _, _ := app.grid.currentCell(); got != "Grace" {
		t.Fatalf("grid cell after edit = %v, want Grace", got)
	}
	if !strings.Contains(app.status, "set name") {
		t.Fatalf("status should confirm the edit, got %q", app.status)
	}

	// And the row really changed in the DB (id=2 was Linus).
	rs, err := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 1 || rs.Rows[0][0] != "Grace" {
		t.Fatalf("db row = %+v, want name Grace", rs.Rows)
	}
}

// TestQuickEditReadOnly verifies a read-only connection refuses the edit key.
func TestQuickEditReadOnly(t *testing.T) {
	app := loadTable(t, true, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if app.grid.editing {
		t.Fatal("read-only connection must not enter edit mode")
	}
	if !strings.Contains(app.status, "read-only") {
		t.Fatalf("status should explain the refusal, got %q", app.status)
	}
}

// TestQuickEditUntouchedNullNoop guards §8: a bare Enter on an untouched NULL
// cell is a no-op — it must not blank the value to an empty string.
func TestQuickEditUntouchedNullNoop(t *testing.T) {
	app := loadTable(t, false, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (id, name) VALUES (1, NULL)`)
	})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if !app.grid.editing || app.grid.editVal != "" {
		t.Fatalf("editing a NULL cell should open an empty overlay; editing=%v val=%q",
			app.grid.editing, app.grid.editVal)
	}
	// Bare Enter, no typing → no command, cell stays NULL.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd != nil {
		t.Fatal("bare Enter on an untouched NULL cell must not run an UPDATE")
	}
	if got, _, _ := app.grid.currentCell(); got != nil {
		t.Fatalf("cell should remain NULL, got %v", got)
	}
	rs, _ := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 1`)
	if rs.Rows[0][0] != nil {
		t.Fatalf("db value should stay NULL, got %v", rs.Rows[0][0])
	}
}

func TestDatabaseName(t *testing.T) {
	cases := map[string]string{
		"./demo.db":                   "demo",
		"sqlite:///Users/jm/notes.db": "notes",
		"postgres://jm@host:5432/appdev?sslmode=disable": "appdev",
		"mysql://jm@localhost:3306/scratch":              "scratch",
	}
	for dsn, want := range cases {
		if got := db.DatabaseName(dsn); got != want {
			t.Errorf("DatabaseName(%q) = %q, want %q", dsn, got, want)
		}
	}
}
