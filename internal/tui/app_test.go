package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
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

	app := New(nil, config.Conn{URL: path, Name: "test"})

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
	if app.screen != screenBrowse {
		t.Fatal("selecting a table should switch to the grid screen")
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

	app := New(nil, config.Conn{URL: path, Name: "test"})
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

	app := New(nil, config.Conn{URL: path, Name: "test"})
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

	app := New(nil, config.Conn{URL: path, Name: "test"})
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

// TestSidebarFilter drives the full-screen table list: typing narrows it (no
// `/`), arrows move within matches, and Enter loads the highlighted table.
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

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if app.screen != screenTables {
		t.Fatalf("should land on the table list, got screen %d", app.screen)
	}
	if len(app.sidebar.tables) != 4 {
		t.Fatalf("table list should have 4 tables, got %d", len(app.sidebar.tables))
	}

	// Typing narrows immediately — no `/`. "order" → orders + order_items.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("order")})
	if len(app.sidebar.visible) != 2 {
		t.Fatalf("filter %q matched %d tables, want 2", "order", len(app.sidebar.visible))
	}
	if t0, _ := app.sidebar.selected(); t0.Name != "order_items" {
		t.Fatalf("cursor should sit on first match order_items, got %q", t0.Name)
	}

	// Backspace re-widens the match set.
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
		t.Fatal("Enter should load the highlighted table")
	}
	app = update(t, app, cmd())
	if app.status != "orders" {
		t.Fatalf("loaded table = %q, want orders", app.status)
	}
	if app.screen != screenBrowse {
		t.Fatal("loading a table should switch to the grid screen")
	}

	// t returns to the table list, keeping the filter narrowed.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if app.screen != screenTables {
		t.Fatal("t should return to the table list")
	}
	if len(app.sidebar.visible) != 2 {
		t.Fatalf("table list should keep the %q filter (2 matches), got %d", "orde", len(app.sidebar.visible))
	}
	// Esc clears the active filter and restores the full list.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.sidebar.hasFilter() || len(app.sidebar.visible) != 4 {
		t.Fatalf("Esc should clear the filter; hasFilter=%v visible=%d",
			app.sidebar.hasFilter(), len(app.sidebar.visible))
	}
	// Esc again (no filter) returns to the grid.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.screen != screenBrowse {
		t.Fatal("Esc with no filter should return to the grid")
	}
}

// loadTable is the shared setup: open a fresh sqlite db, run schema/seed, and
// drive the model up to a loaded grid.
func loadTable(t *testing.T, seed func(e db.Engine)) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seed(e)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
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
	app := loadTable(t, func(e db.Engine) {
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

// TestQuickEditUntouchedNullNoop guards §8: a bare Enter on an untouched NULL
// cell is a no-op — it must not blank the value to an empty string.
func TestQuickEditUntouchedNullNoop(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
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

// TestFullEditRunsEditedSQL drives the E full path at the message boundary:
// fullEditTarget + buildUpdateStmt produce a keyed UPDATE; submitting edited SQL
// runs it verbatim and reloads the grid to reflect the change. The $EDITOR spawn
// lives in editorCmd and isn't driven here (no editor in tests).
func TestFullEditRunsEditedSQL(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	// Default PK-descending sort puts id=2 (Linus) on row 0; move to "name".
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})

	col, val, keys, ok := app.grid.fullEditTarget()
	if !ok || col != "name" || val != "Linus" {
		t.Fatalf("fullEditTarget = (%q, %v, ok=%v), want name/Linus", col, val, ok)
	}
	seed := buildUpdateStmt(app.engine, app.grid.table, col, val, keys)
	for _, want := range []string{"UPDATE", `SET "name" = 'Linus'`, `WHERE "id" = 2`} {
		if !strings.Contains(seed.sql, want) {
			t.Fatalf("generated UPDATE missing %q:\n%s", want, seed.sql)
		}
	}
	if strings.Contains(seed.sql, "-- jsq") {
		t.Fatalf("statement should have no leading comment:\n%s", seed.sql)
	}
	// The cursor lands on the first char inside the quotes, with the inner text
	// selected so `c` edits the string value in place.
	if seed.kind != selectInsideQuotes || seed.line != 1 {
		t.Fatalf("want inside-quotes selection on line 1, got kind=%d line=%d", seed.kind, seed.line)
	}
	if line0 := strings.SplitN(seed.sql, "\n", 2)[0]; seed.col-1 >= len(line0) || line0[seed.col-1] != 'L' {
		t.Fatalf("cursor col %d should sit on the 'L' of Linus in %q", seed.col, line0)
	}

	// Simulate editing the value in $EDITOR and :wq.
	edited := strings.Replace(seed.sql, "'Linus'", "'Neo'", 1)
	m, cmd := app.Update(editorSubmitMsg{sql: edited})
	app = m.(App)
	if cmd == nil {
		t.Fatal("submitting edited SQL should run it")
	}
	done := cmd() // execRawCmd → execDoneMsg
	if _, ok := done.(execDoneMsg); !ok {
		t.Fatalf("want execDoneMsg, got %T (%+v)", done, done)
	}
	m, cmd = app.Update(done) // → reload
	app = m.(App)
	if cmd == nil {
		t.Fatal("execDone should reload the view")
	}
	app = update(t, app, cmd()) // rowsMsg

	if got, _, _ := app.grid.currentCell(); got != "Neo" {
		t.Fatalf("grid cell after full edit = %v, want Neo", got)
	}
	if !strings.Contains(app.status, "affected") {
		t.Fatalf("status should confirm the exec, got %q", app.status)
	}
	rs, err := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 1 || rs.Rows[0][0] != "Neo" {
		t.Fatalf("db row = %+v, want name Neo", rs.Rows)
	}

	// An aborted editor runs nothing and says so.
	m, cmd = app.Update(editorAbortedMsg{})
	app = m.(App)
	if cmd != nil {
		t.Fatal("an aborted edit must not run anything")
	}
	if !strings.Contains(app.status, "cancel") {
		t.Fatalf("status should note the cancel, got %q", app.status)
	}
}

// TestEditorResult covers the run-vs-abort decision after $EDITOR exits.
func TestEditorResult(t *testing.T) {
	seed := "-- jsq: edit\nUPDATE t SET c = 'a' WHERE id = 1;\n"

	// Edited and saved → run the edited SQL.
	edited := strings.Replace(seed, "'a'", "'b'", 1)
	if got := editorResult(seed, edited, true); got != (editorSubmitMsg{sql: edited}) {
		t.Errorf("edited+save: got %#v, want submit(edited)", got)
	}
	// Run as-is (:wq, unchanged content but mtime bumped) → run the seed.
	if got := editorResult(seed, seed, true); got != (editorSubmitMsg{sql: seed}) {
		t.Errorf("as-is save: got %#v, want submit(seed)", got)
	}
	// Quit without saving (:q!, unchanged, no mtime bump) → abort.
	if _, ok := editorResult(seed, seed, false).(editorAbortedMsg); !ok {
		t.Errorf(":q! should abort")
	}
	// Buffer cleared (only comments/blank left) → abort even if saved.
	if _, ok := editorResult(seed, "-- gone\n\n", true).(editorAbortedMsg); !ok {
		t.Errorf("cleared buffer should abort")
	}
}

func TestEditorInvocation(t *testing.T) {
	// A non-vim editor with its own flag and no positioning.
	t.Setenv("EDITOR", "code -w")
	if n, args := editorInvocation("/x.sql", editorSeed{}); n != "code" ||
		len(args) != 2 || args[0] != "-w" || args[1] != "/x.sql" {
		t.Fatalf("code -w → %q %v", n, args)
	}
	// A zero seed adds no positioning even for a vim-family editor.
	t.Setenv("EDITOR", "")
	if n, args := editorInvocation("/x.sql", editorSeed{}); n != "vi" ||
		len(args) != 1 || args[0] != "/x.sql" {
		t.Fatalf("default vi, zero seed → %q %v, want vi [/x.sql]", n, args)
	}
	// A vim-family editor with a real seed gets cursor + Visual-select commands
	// before the path.
	t.Setenv("EDITOR", "nvim")
	n, args := editorInvocation("/x.sql", editorSeed{line: 1, col: 29, kind: selectToken})
	want := []string{"+call cursor(1,29)", `+call feedkeys("v$", "n")`, "/x.sql"}
	if n != "nvim" || len(args) != len(want) {
		t.Fatalf("nvim → %q %v", n, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("nvim arg[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// Inside-quotes selection uses vi'.
	if _, a := editorInvocation("/x.sql", editorSeed{line: 1, col: 5, kind: selectInsideQuotes}); a[1] != `+call feedkeys("vi'", "n")` {
		t.Fatalf("inside-quotes should feedkeys vi', got %q", a[1])
	}
}

// TestEditorSpawnRoundTrip exercises the real glue editorCmd owns — resolve
// $EDITOR, spawn it on the seed file, read the result back, decide — with a fake
// editor that rewrites the file (the tea.ExecProcess wrapper aside).
func TestEditorSpawnRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ed := filepath.Join(dir, "fakeed.sh")
	if err := os.WriteFile(ed, []byte("#!/bin/sh\nsed -i 's/Ada/Neo/' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", ed)

	seed := "UPDATE t SET c = 'Ada' WHERE id = 1;\n"
	path := filepath.Join(dir, "stmt.sql")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	name, args := editorInvocation(path, editorSeed{})
	if err := exec.Command(name, args...).Run(); err != nil {
		t.Fatalf("fake editor failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	msg := editorResult(seed, string(data), true)
	sub, ok := msg.(editorSubmitMsg)
	if !ok || !strings.Contains(sub.sql, "'Neo'") {
		t.Fatalf("round trip = %#v, want submit containing 'Neo'", msg)
	}
}

// TestBuildUpdateStmtSelection locks how each value type is targeted: numbers
// and NULL select the whole token; strings select inside the quotes; empty and
// multi-line values just place the cursor.
func TestBuildUpdateStmtSelection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, _ := db.Open(ctx, path)
	defer e.Close()
	tref := db.TableRef{Name: "t"}
	keys := []keyPred{{col: "id", val: int64(1)}}

	cases := []struct {
		name string
		val  any
		kind selectKind
		set  string
	}{
		{"number", int64(42), selectToken, `SET "c" = 42`},
		{"null", nil, selectToken, `SET "c" = NULL`},
		{"string", "Ada", selectInsideQuotes, `SET "c" = 'Ada'`},
		{"empty", "", selectNone, `SET "c" = ''`},
		{"multiline", "a\nb", selectNone, "SET \"c\" = 'a\nb'"},
	}
	for _, c := range cases {
		seed := buildUpdateStmt(e, tref, "c", c.val, keys)
		if seed.kind != c.kind {
			t.Errorf("%s: kind = %d, want %d", c.name, seed.kind, c.kind)
		}
		if !strings.Contains(seed.sql, c.set) {
			t.Errorf("%s: sql missing %q:\n%s", c.name, c.set, seed.sql)
		}
	}
}

// TestBuildInsertStmt covers the o (blank insert) generator: auto-generated
// columns omitted, DEFAULT vs NULL seeding, and PK/UNIQUE annotations.
func TestBuildInsertStmt(t *testing.T) {
	ctx := context.Background()
	e, _ := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()

	// Auto-gen PK omitted; unique flagged; default → DEFAULT; plain → NULL.
	seed := buildInsertStmt(e, db.TableRef{Name: "users"}, []db.Column{
		{Name: "id", PrimaryKey: true, AutoGenerated: true, Unique: true},
		{Name: "email", Unique: true},
		{Name: "name"},
		{Name: "created_at", HasDefault: true},
	})
	if !strings.Contains(seed.sql, `INSERT INTO "users" ("email", "name", "created_at")`) {
		t.Fatalf("column list wrong:\n%s", seed.sql)
	}
	if strings.Contains(seed.sql, `"id"`) {
		t.Fatalf("auto-generated id should be omitted:\n%s", seed.sql)
	}
	// Every insertable value is NULL (portable; SQLite rejects DEFAULT in VALUES);
	// a defaulted column is noted so you can delete the line to use its default.
	if strings.Contains(seed.sql, "DEFAULT") {
		t.Fatalf("should not emit the DEFAULT keyword (SQLite rejects it):\n%s", seed.sql)
	}
	for _, want := range []string{"-- email", "⚠ UNIQUE", "-- name", "-- created_at", "has default"} {
		if !strings.Contains(seed.sql, want) {
			t.Fatalf("missing %q:\n%s", want, seed.sql)
		}
	}
	if seed.kind != selectNone || seed.line != 3 {
		t.Fatalf("cursor kind=%d line=%d, want selectNone on line 3", seed.kind, seed.line)
	}

	// A natural (non-generated) PK is kept and flagged as the value to set.
	nat := buildInsertStmt(e, db.TableRef{Name: "countries"}, []db.Column{
		{Name: "code", PrimaryKey: true, Unique: true},
		{Name: "name"},
	})
	if !strings.Contains(nat.sql, `"code"`) || !strings.Contains(nat.sql, "⚠ PRIMARY KEY") {
		t.Fatalf("natural PK should be kept and flagged:\n%s", nat.sql)
	}

	// A table of only auto-generated columns falls back to DEFAULT VALUES.
	only := buildInsertStmt(e, db.TableRef{Name: "log"}, []db.Column{
		{Name: "id", PrimaryKey: true, AutoGenerated: true},
	})
	if !strings.Contains(only.sql, "DEFAULT VALUES") {
		t.Fatalf("all-auto table should use DEFAULT VALUES:\n%s", only.sql)
	}
}

// TestBlankInsert drives the o full path at the message boundary: o fetches
// columns and yields a seed, and submitting a filled INSERT runs it and reloads.
func TestBlankInsert(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("o should prepare an insert")
	}
	ready, ok := cmd().(editorReadyMsg)
	if !ok {
		t.Fatalf("want editorReadyMsg, got a different message")
	}
	if !strings.Contains(ready.seed.sql, `INSERT INTO "users" ("name")`) {
		t.Fatalf("insert seed wrong:\n%s", ready.seed.sql)
	}
	// editorReadyMsg → editorCmd (an ExecProcess we don't drive). Submit the
	// generated seed verbatim to prove it's valid runnable SQL (inline comments,
	// multi-line VALUES, NULL seeding) — as if the user just :wq'd it unchanged.
	app = update(t, app, ready)
	m, cmd = app.Update(editorSubmitMsg{sql: ready.seed.sql})
	app = m.(App)
	m, cmd = app.Update(cmd()) // execDone → reload
	app = m.(App)
	app = update(t, app, cmd()) // rowsMsg

	rs, _ := app.engine.Query(context.Background(), `SELECT id, name FROM users ORDER BY id`)
	if len(rs.Rows) != 2 || rs.Rows[0][1] != "Ada" || rs.Rows[1][1] != nil {
		t.Fatalf("db rows = %+v, want Ada + a NULL-name row", rs.Rows)
	}
	// Default PK-descending order puts the new (id=2) row on top of the reload.
	if len(app.grid.rows) != 2 {
		t.Fatalf("grid should show 2 rows after insert, got %d", len(app.grid.rows))
	}
}

// TestBuildDuplicateStmt covers the p generator: auto-generated PK omitted with
// copied values; a natural PK kept and flagged.
func TestBuildDuplicateStmt(t *testing.T) {
	ctx := context.Background()
	e, _ := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()

	// Auto-generated PK dropped; other values copied; UNIQUE flagged to change.
	dup := buildDuplicateStmt(e, db.TableRef{Name: "users"},
		[]db.Column{
			{Name: "id", PrimaryKey: true, AutoGenerated: true, Unique: true},
			{Name: "email", Unique: true},
			{Name: "name"},
		},
		map[string]any{"id": int64(1), "email": "ada@x.io", "name": "Ada"})
	if strings.Contains(dup.sql, `"id"`) {
		t.Fatalf("auto-generated id should be omitted:\n%s", dup.sql)
	}
	for _, want := range []string{
		`INSERT INTO "users" ("email", "name")`,
		"'ada@x.io'", "-- email", "⚠ UNIQUE — change before :wq", "'Ada'", "-- name",
	} {
		if !strings.Contains(dup.sql, want) {
			t.Fatalf("missing %q:\n%s", want, dup.sql)
		}
	}

	// A natural PK is kept, its value copied, and flagged as the value to change.
	nat := buildDuplicateStmt(e, db.TableRef{Name: "countries"},
		[]db.Column{
			{Name: "code", PrimaryKey: true, Unique: true},
			{Name: "name"},
		},
		map[string]any{"code": "US", "name": "United States"})
	if !strings.Contains(nat.sql, "'US'") || !strings.Contains(nat.sql, "⚠ PRIMARY KEY — must be unique, change this") {
		t.Fatalf("natural PK should be kept, copied, and flagged:\n%s", nat.sql)
	}
}

// TestDuplicateRow drives the p full path: p captures the current row, fetches
// columns, and yields a pre-filled INSERT; submitting it clones the row.
func TestDuplicateRow(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name, email) VALUES ('Ada','ada@x.io')`)
	})

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("p should prepare a duplicate")
	}
	ready, ok := cmd().(editorReadyMsg)
	if !ok {
		t.Fatalf("want editorReadyMsg")
	}
	// Values copied from the current row; auto-gen id omitted.
	for _, want := range []string{`INSERT INTO "users" ("name", "email")`, "'Ada'", "'ada@x.io'"} {
		if !strings.Contains(ready.seed.sql, want) {
			t.Fatalf("duplicate seed missing %q:\n%s", want, ready.seed.sql)
		}
	}
	// Submit the generated seed verbatim (proves it runs) — a fresh id is assigned.
	app = update(t, app, ready)
	m, cmd = app.Update(editorSubmitMsg{sql: ready.seed.sql})
	app = m.(App)
	m, cmd = app.Update(cmd()) // execDone → reload
	app = m.(App)
	app = update(t, app, cmd()) // rowsMsg

	rs, _ := app.engine.Query(context.Background(), `SELECT id, name, email FROM users ORDER BY id`)
	if len(rs.Rows) != 2 {
		t.Fatalf("want 2 rows after duplicate, got %+v", rs.Rows)
	}
	if rs.Rows[1][0] == rs.Rows[0][0] {
		t.Fatalf("clone should get a fresh id, got %+v", rs.Rows)
	}
	if rs.Rows[1][1] != "Ada" || rs.Rows[1][2] != "ada@x.io" {
		t.Fatalf("clone should copy name+email, got %+v", rs.Rows[1])
	}
}

// TestDeleteRow drives the D full path: D yields a PK-keyed DELETE, and
// submitting it (as if :wq) removes the row and reloads.
func TestDeleteRow(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	// Default PK-descending sort puts id=2 (Linus) on row 0.
	if got, _, _ := app.grid.currentCell(); got != int64(2) {
		t.Fatalf("row 0 should be id=2, got %v", got)
	}

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("D should open a DELETE in the editor")
	}
	// D builds its seed inline, so we can't read it from the ExecProcess cmd;
	// rebuild it the same way to submit verbatim (as if the user :wq'd).
	keys, ok := app.grid.rowKeys()
	if !ok {
		t.Fatal("row should be keyable")
	}
	seed := buildDeleteStmt(app.engine, app.grid.table, keys)
	if !strings.Contains(seed.sql, `DELETE FROM "users" WHERE "id" = 2;`) {
		t.Fatalf("delete seed wrong:\n%s", seed.sql)
	}

	m, cmd = app.Update(editorSubmitMsg{sql: seed.sql})
	app = m.(App)
	m, cmd = app.Update(cmd()) // execDone → reload
	app = m.(App)
	app = update(t, app, cmd()) // rowsMsg

	rs, _ := app.engine.Query(context.Background(), `SELECT id FROM users ORDER BY id`)
	if len(rs.Rows) != 1 || rs.Rows[0][0] != int64(1) {
		t.Fatalf("db rows = %+v, want only id=1", rs.Rows)
	}
	if len(app.grid.rows) != 1 {
		t.Fatalf("grid should show 1 row after delete, got %d", len(app.grid.rows))
	}
}

// TestBuildDeleteComposite checks a composite PK ANDs its predicates.
func TestBuildDeleteComposite(t *testing.T) {
	ctx := context.Background()
	e, _ := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	seed := buildDeleteStmt(e, db.TableRef{Name: "grades"}, []keyPred{
		{col: "student", val: int64(7)},
		{col: "course", val: "cs101"},
	})
	if !strings.Contains(seed.sql, `DELETE FROM "grades" WHERE "student" = 7 AND "course" = 'cs101';`) {
		t.Fatalf("composite delete wrong:\n%s", seed.sql)
	}
}

// TestActivityIndicator drives the header activity indicator + Esc-cancel: a DB
// op sets an in-flight label the header shows, the spinner ticks, and Esc kills
// the op (clearing the label) without surfacing an error.
func TestActivityIndicator(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	// A settled grid shows no indicator.
	if app.activity != "" || app.cancel != nil {
		t.Fatalf("loaded grid should be idle; activity=%q cancel=%v", app.activity, app.cancel != nil)
	}

	// J starts a sort reload; the op is now in flight and the header shows it.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("J should start a reload")
	}
	if app.activity != "sorting" || app.cancel == nil {
		t.Fatalf("J should mark an in-flight op; activity=%q cancel=%v", app.activity, app.cancel != nil)
	}
	view := app.View()
	if !strings.Contains(view, "sorting") || !strings.Contains(view, "esc") {
		t.Fatalf("header should show the activity + esc hint:\n%s", view)
	}

	// A tick advances the spinner and keeps ticking while busy.
	prev := app.spinner
	m, tcmd := app.Update(tickMsg{})
	app = m.(App)
	if app.spinner == prev || tcmd == nil {
		t.Fatalf("tick should advance the spinner (%d→%d) and reschedule", prev, app.spinner)
	}

	// Esc kills the in-flight op: label cleared, cancel released, no error.
	m, _ = app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	app = m.(App)
	if app.activity != "" || app.cancel != nil {
		t.Fatalf("Esc should clear the in-flight op; activity=%q cancel=%v", app.activity, app.cancel != nil)
	}
	if app.status != "cancelled" {
		t.Fatalf("Esc should report the cancel, got %q", app.status)
	}
	if app.View() == "" || strings.Contains(app.View(), "sorting") {
		t.Fatalf("cancelled header should drop the indicator:\n%s", app.View())
	}
}

// TestCanceledSwallowed verifies a command whose context was cancelled yields a
// nil message (no error screen) rather than an errMsg.
func TestCanceledSwallowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := dbErr(ctx, context.Canceled); got != nil {
		t.Fatalf("a cancelled op should swallow to nil, got %#v", got)
	}
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := dbErr(live, context.DeadlineExceeded).(errMsg); !ok {
		t.Fatalf("a real failure on a live ctx should be an errMsg")
	}
}

// TestHelpOverlay drives the `?` cheat sheet: it opens over the grid, shows the
// bindings, toggles shut, and — crucially — `?` typed into a filter stays
// literal rather than opening help.
func TestHelpOverlay(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	// `?` opens the panel; the view shows the title and a real binding.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !app.help.active {
		t.Fatal("`?` should open the help panel")
	}
	view := app.View()
	for _, want := range []string{"Keybindings", "quick-edit", "duplicate the current row"} {
		if !strings.Contains(view, want) {
			t.Fatalf("help view missing %q:\n%s", want, view)
		}
	}
	// The grid is hidden while help is up.
	if strings.Contains(view, "Ada") {
		t.Fatalf("help panel should replace the grid:\n%s", view)
	}

	// `?` again toggles it shut, back to the grid.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if app.help.active {
		t.Fatal("`?` should close the help panel")
	}
	if !strings.Contains(app.View(), "Ada") {
		t.Fatal("closing help should restore the grid")
	}

	// `?` typed into a column filter is literal — it must not open help.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if app.help.active {
		t.Fatal("`?` while typing a filter must stay literal, not open help")
	}
	if app.grid.filterVal != "?" {
		t.Fatalf("filter should contain the literal ?, got %q", app.grid.filterVal)
	}
}

func TestIsReadSQL(t *testing.T) {
	reads := []string{"SELECT 1", "  select * from t", "-- note\nSELECT 1", "PRAGMA x",
		"EXPLAIN SELECT 1", "SHOW TABLES", "VALUES (1)", "TABLE users"}
	for _, s := range reads {
		if !isReadSQL(s) {
			t.Errorf("isReadSQL(%q) = false, want true", s)
		}
	}
	// WITH is deliberately not a read (a data-modifying CTE also leads with WITH).
	writes := []string{"INSERT INTO t VALUES (1)", "update t set a=1", "DELETE FROM t",
		"WITH x AS (SELECT 1) SELECT * FROM x", "CREATE TABLE t (a int)", "", "-- only a comment"}
	for _, s := range writes {
		if isReadSQL(s) {
			t.Errorf("isReadSQL(%q) = true, want false", s)
		}
	}
}

func TestSelectTemplate(t *testing.T) {
	ctx := context.Background()
	e, _ := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if got, want := selectTemplate(e, db.TableRef{Name: "users"}), "SELECT * FROM \"users\" LIMIT 100;\n"; got != want {
		t.Fatalf("selectTemplate = %q, want %q", got, want)
	}
}

// TestScratchQuery drives the s read path: submitting a SELECT runs it and shows
// the rows as a read-only (non-editable, non-sortable) result pane.
func TestScratchQuery(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// s opens the editor (an ExecProcess we don't drive); simulate :wq of a SELECT.
	if _, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); cmd == nil {
		t.Fatal("s should open the editor")
	}
	m, cmd := app.Update(editorSubmitMsg{sql: "SELECT name FROM users ORDER BY name;"})
	app = m.(App)
	if cmd == nil {
		t.Fatal("a read submit should run a query")
	}
	qr, ok := cmd().(queryResultMsg)
	if !ok {
		t.Fatalf("want queryResultMsg")
	}
	app = update(t, app, qr)

	if !app.adHoc {
		t.Fatal("a query result should set adHoc")
	}
	if app.grid.editable() {
		t.Fatal("a query result must be non-editable")
	}
	if len(app.grid.rows) != 2 {
		t.Fatalf("query should show 2 rows, got %d", len(app.grid.rows))
	}
	view := app.View()
	if !strings.Contains(view, "Ada") || !strings.Contains(view, "Linus") {
		t.Fatalf("query view missing rows:\n%s", view)
	}
	// Sort/filter operate on the table, not the ad-hoc result, so they're disabled.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	app = m.(App)
	if cmd != nil {
		t.Fatal("J on a query result must not reload the table")
	}
	if !strings.Contains(app.status, "unavailable") {
		t.Fatalf("status should note sort is unavailable, got %q", app.status)
	}
}

// TestScratchRemembersLastQuery covers the edit-run-edit loop: s prefills the
// SELECT template first, then your last query on that table, keyed per table.
func TestScratchRemembersLastQuery(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	// First s: the SELECT template, tagged to remember the current table.
	first := app.scratchSeed()
	if !strings.Contains(first.sql, `SELECT * FROM "users" LIMIT 100`) {
		t.Fatalf("first scratch should be the template, got %q", first.sql)
	}
	if first.remember != app.currentTable {
		t.Fatalf("scratch should remember the current table")
	}

	// Running a custom query remembers it; the next s prefills it.
	app = update(t, app, editorSubmitMsg{sql: "SELECT name FROM users WHERE id > 0;", remember: app.currentTable})
	if got := app.scratchSeed().sql; got != "SELECT name FROM users WHERE id > 0;" {
		t.Fatalf("scratch should prefill the last query, got %q", got)
	}

	// It's per table: a different table gets the template, not this query.
	app.currentTable = db.Table{Name: "other"}
	if got := app.scratchSeed().sql; !strings.Contains(got, `SELECT * FROM "other" LIMIT 100`) {
		t.Fatalf("a different table should get the template, got %q", got)
	}
}

func TestSQLLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "NULL"},
		{int64(42), "42"},
		{3.5, "3.5"},
		{true, "TRUE"},
		{"ada@x.io", "'ada@x.io'"},
		{"O'Brien", "'O''Brien'"},
	}
	for _, c := range cases {
		if got := sqlLiteral(c.in); got != c.want {
			t.Errorf("sqlLiteral(%#v) = %q, want %q", c.in, got, c.want)
		}
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
