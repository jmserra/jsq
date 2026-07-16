package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// runeKey feeds a single-rune keypress into the model.
func runeKey(t *testing.T, app App, r rune) App {
	t.Helper()
	return update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
}

// TestVisualRowYank drives the V visual-select flow: V arms it, j extends the
// selection, o swaps the edge, y yanks the selected rows as a JSON array and
// exits the mode.
func TestVisualRowYank(t *testing.T) {
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
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	// V arms visual mode with the anchor on row 0.
	app = runeKey(t, app, 'V')
	if !app.grid.visualMode {
		t.Fatal("V should enter visual mode")
	}
	if app.grid.visualAnchor != 0 || app.grid.cursorR != 0 {
		t.Fatalf("anchor/cursor = %d/%d, want 0/0", app.grid.visualAnchor, app.grid.cursorR)
	}

	// j extends the selection down to row 1 (Ada + Linus selected).
	app = runeKey(t, app, 'j')
	if lo, hi := app.grid.visualRange(); lo != 0 || hi != 1 {
		t.Fatalf("range = %d..%d, want 0..1", lo, hi)
	}

	// o swaps the moving edge: cursor jumps to the anchor end (row 0), anchor
	// becomes row 1 — the range is unchanged.
	app = runeKey(t, app, 'o')
	if app.grid.cursorR != 0 || app.grid.visualAnchor != 1 {
		t.Fatalf("after o cursor/anchor = %d/%d, want 0/1", app.grid.cursorR, app.grid.visualAnchor)
	}
	if lo, hi := app.grid.visualRange(); lo != 0 || hi != 1 {
		t.Fatalf("range after o = %d..%d, want 0..1", lo, hi)
	}

	// The yank text is a JSON array of the two selected rows, column-ordered.
	text, n, ok := app.grid.yankSelectionJSON()
	if !ok || n != 2 {
		t.Fatalf("yankSelectionJSON ok=%v n=%d, want true/2", ok, n)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("yank is not a JSON array: %v\n%s", err, text)
	}
	// Default sort is PK descending, so rows load newest-first: Grace(3), Linus(2).
	if len(got) != 2 || got[0]["name"] != "Grace" || got[1]["name"] != "Linus" {
		t.Fatalf("unexpected yank payload: %s", text)
	}

	// y yanks and exits visual mode, reporting the row count.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("y in visual mode should return a yank command")
	}
	if app.grid.visualMode {
		t.Fatal("y should exit visual mode")
	}
	if !strings.Contains(app.status, "2 rows copied") {
		t.Fatalf("status = %q, want it to mention 2 rows copied", app.status)
	}
}

// TestVisualEscCancels checks Esc leaves visual mode without yanking.
func TestVisualEscCancels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	app = runeKey(t, app, 'V')
	app = runeKey(t, app, 'j')
	if !app.grid.visualMode {
		t.Fatal("expected visual mode active")
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.grid.visualMode {
		t.Fatal("Esc should exit visual mode")
	}
	// Screen must stay on the grid — Esc in visual mode is not a screen change.
	if app.screen != screenBrowse {
		t.Fatalf("screen = %v, want screenBrowse", app.screen)
	}
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

// TestGridFilterSubstringFallback: an accurate prefix that matches nothing widens
// to a substring match, both in the live preview and the committed server query.
func TestGridFilterSubstringFallback(t *testing.T) {
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

	// name column, filter "in": prefix "in%" matches no name → falls back to "%in%",
	// which matches only Linus.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("in")})
	if len(app.grid.visible) != 1 {
		t.Fatalf("substring preview matched %d rows, want 1 (Linus)", len(app.grid.visible))
	}

	// Commit → the decision is recorded and the server query uses the substring.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if !app.grid.filtersWide[1] {
		t.Fatal("column 1 should be flagged to widen to a substring match")
	}
	app = update(t, app, cmd())
	if len(app.grid.rows) != 1 {
		t.Fatalf("server substring filter returned %d rows, want 1", len(app.grid.rows))
	}
	if v := app.View(); !strings.Contains(v, "Linus") || strings.Contains(v, "Grace") {
		t.Fatalf("filtered view should show only Linus:\n%s", v)
	}
}

// TestGridFilterPreviewSkipsNull: the client preview must not match a NULL cell,
// because the server's LIKE never returns NULL rows — otherwise a NULL row shows
// in the live preview and then vanishes on commit. Typing "N" (NULL renders as
// the glyph "NULL") must match the real "Nora" row only, not the NULL one.
func TestGridFilterPreviewSkipsNull(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (id, name) VALUES (1,'Nora'),(2,NULL),(3,'Ada')`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter}) // load users
	app = m.(App)
	app = update(t, app, cmd())

	// name column, filter "N": prefix "N%" matches "Nora" only — the NULL row must
	// be skipped even though its cell renders as the glyph "NULL".
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("N")})
	if len(app.grid.visible) != 1 {
		t.Fatalf("preview matched %d rows, want 1 (Nora only, NULL skipped)", len(app.grid.visible))
	}

	// Commit → the server query agrees: exactly the one non-NULL match.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())
	if len(app.grid.rows) != 1 {
		t.Fatalf("server filter returned %d rows, want 1 (preview must equal commit)", len(app.grid.rows))
	}
}

// TestSidebarSubstringFallback: the list filter also widens to a substring match
// when the prefix finds nothing.
func TestSidebarSubstringFallback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"users", "orders", "order_items", "products"} {
		e.Exec(ctx, "CREATE TABLE "+name+" (id INTEGER PRIMARY KEY)")
	}
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	// "items" has no table with that prefix; "%items%" matches order_items.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("items")})
	if len(app.sidebar.visible) != 1 {
		t.Fatalf("substring fallback matched %d tables, want 1", len(app.sidebar.visible))
	}
	if sel, _ := app.sidebar.selected(); sel.Name != "order_items" {
		t.Fatalf("fallback should select order_items, got %q", sel.Name)
	}
}

// TestSidebarQualifiesPublicSchema guards the fix for tables vanishing from the
// list filter. Public tables are now shown schema-qualified like every other
// schema (tableLabel no longer strips "public."), so a bare-labelled public
// table can't prefix-match and suppress the substring fallback that a
// schema-qualified table depends on.
func TestSidebarQualifiesPublicSchema(t *testing.T) {
	var s sidebar
	s.setTables([]db.Table{
		{Schema: "public", Name: "items"},
		{Schema: "billing", Name: "invoices"},
	})
	if got := tableLabel(s.tables[0]); got != "public.items" {
		t.Fatalf("public table label = %q, want public.items", got)
	}

	// Filter for "i": neither label prefix-matches (both lead with a schema name),
	// so the substring fallback keeps BOTH. Before the fix, the public table's
	// bare label "items" prefix-matched and dropped billing.invoices.
	s.filter.val = "i"
	s.cursor, s.off = 0, 0
	s.rebuildVisible()
	if len(s.visible) != 2 {
		t.Fatalf("/i matched %d tables, want 2 (schema-qualified table vanished)", len(s.visible))
	}
}

// TestFilterCursorEditing: the filter input supports a moving caret (←/→) and
// Ctrl-W word delete, not just append/backspace.
func TestFilterCursorEditing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app = typeRunes(t, app, "foo bar")
	// Ctrl-W deletes the word before the caret (at the end): "bar".
	app = update(t, app, tea.KeyMsg{Type: tea.KeyCtrlW})
	if app.sidebar.filter.val != "foo " {
		t.Fatalf("Ctrl-W should leave %q, got %q", "foo ", app.sidebar.filter.val)
	}
	// Move the caret left and insert mid-string.
	app = typeRunes(t, app, "baz") // "foo baz"
	app = update(t, app, tea.KeyMsg{Type: tea.KeyLeft})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyLeft})
	app = typeRunes(t, app, "X") // caret between 'b' and 'a' → "foo bXaz"
	if app.sidebar.filter.val != "foo bXaz" {
		t.Fatalf("caret-aware insert should give %q, got %q", "foo bXaz", app.sidebar.filter.val)
	}
}

// TestListHLColumnJump: in the list, h/l jump columns exactly like ←/→.
func TestListHLColumnJump(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, _ := db.Open(ctx, path)
	for i := 0; i < 30; i++ { // enough rows to spill into a second grid column
		e.Exec(ctx, fmt.Sprintf("CREATE TABLE t%02d (id INTEGER PRIMARY KEY)", i))
	}
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	// l lands on the same cell as →.
	right := update(t, app, tea.KeyMsg{Type: tea.KeyRight}).sidebar.cursor
	lKey := update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")}).sidebar.cursor
	if right == 0 || right != lKey {
		t.Fatalf("l should jump a column like →: →=%d l=%d", right, lKey)
	}
	// From that column, h returns like ←.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	left := update(t, app, tea.KeyMsg{Type: tea.KeyLeft}).sidebar.cursor
	hKey := update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")}).sidebar.cursor
	if left != hKey {
		t.Fatalf("h should jump a column like ←: ←=%d h=%d", left, hKey)
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

// idsFrom pulls the id column (index 0) out of a moreRowsMsg's rows as int64s.
func idsFrom(t *testing.T, rows [][]any) []int64 {
	t.Helper()
	out := make([]int64, len(rows))
	for i, r := range rows {
		n, ok := r[0].(int64)
		if !ok {
			t.Fatalf("row %d id not int64: %T", i, r[0])
		}
		out[i] = n
	}
	return out
}

// TestKeysetStableUnderInsert is the point of keyset paging: a scroll fetch pages
// by the anchor row's key, not a row offset — so a row inserted at the top of the
// order (here a new highest id, default DESC sort) mid-scroll can't shift the
// window and cause a dup/skip. With OFFSET, `LIMIT 5 OFFSET 5` after the insert
// would re-return the anchor row (6) as a duplicate.
func TestKeysetStableUnderInsert(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Exec(ctx, `CREATE TABLE nums (id INTEGER PRIMARY KEY, v INTEGER)`)
	for i := 0; i < 10; i++ { // ids 1..10
		e.Exec(ctx, `INSERT INTO nums (v) VALUES (?)`, i)
	}

	// Simulate: the first window (default sort, id DESC) loaded ids 10..6, so the
	// anchor is the last loaded row, id=6. A concurrent write then inserts id 11.
	e.Exec(ctx, `INSERT INTO nums (v) VALUES (99)`) // id 11, "newest" → top of DESC
	anchor := map[string]any{"id": int64(6), "v": int64(5)}

	cmd := loadMoreCmd(context.Background(), 1, e, db.Table{Name: "nums"},
		"", false, nil, nil, anchor, 5 /*offset*/, 5 /*limit*/)
	msg, ok := cmd().(moreRowsMsg)
	if !ok {
		t.Fatalf("expected moreRowsMsg, got %T", cmd())
	}
	got := idsFrom(t, msg.rows)
	want := []int64{5, 4, 3, 2, 1} // strictly past id=6, no dup of 6, no skip
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keyset next window = %v, want %v (OFFSET would dup 6)", got, want)
	}
}

// TestKeysetTiebreakerNoSkip guards the composite cursor: with a composite PK
// (a, b) sorted on its first column, the second PK column is the tiebreaker, and
// an anchor mid-tie must still return the rest of the tie — a naive `a > 5` would
// skip it. (Sorting on a PK column keeps the ordering keyset-safe.)
func TestKeysetTiebreakerNoSkip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Exec(ctx, `CREATE TABLE t (a INTEGER, b INTEGER, PRIMARY KEY (a, b))`)
	// a=5 is a three-row tie broken by b; a=6 has two rows.
	e.Exec(ctx, `INSERT INTO t (a, b) VALUES (5,1),(5,2),(5,3),(6,4),(6,5)`)

	// Sort a ASC → order (a,b): (5,1),(5,2),(5,3),(6,4),(6,5). Say (5,1),(5,2)
	// loaded; anchor is a=5,b=2, mid-tie.
	anchor := map[string]any{"a": int64(5), "b": int64(2)}
	cmd := loadMoreCmd(context.Background(), 1, e, db.Table{Name: "t"},
		"a", true, nil, nil, anchor, 2, 10)
	msg := cmd().(moreRowsMsg)
	// Rows are (a,b); collect b (column index 1) — expect 3 (rest of the tie), 4, 5.
	var got []int64
	for _, r := range msg.rows {
		got = append(got, r[1].(int64))
	}
	want := []int64{3, 4, 5} // (5,3) not skipped, then the a=6 rows
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keyset tie window (b values) = %v, want %v", got, want)
	}
}

// TestKeysetFallsBackForNonPKSort guards the NULL-safety gate: an explicit sort
// on a non-PK column is not keyset-eligible (its NULL ordering could hide rows),
// so loadMoreCmd must page by OFFSET. Verified by giving a *wrong* anchor that a
// keyset cursor would honor but OFFSET ignores.
func TestKeysetFallsBackForNonPKSort(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`)
	e.Exec(ctx, `INSERT INTO t (id, v) VALUES (1,10),(2,20),(3,30),(4,40),(5,50)`)

	// Sort by v ASC (v is not the PK). A misleading anchor of v=999 would make a
	// keyset cursor (`v > 999`) return nothing; OFFSET 2 ignores the anchor and
	// returns rows 3..5 (v = 30,40,50) → ids 3,4,5. Getting those proves OFFSET.
	anchor := map[string]any{"id": int64(999), "v": int64(999)}
	cmd := loadMoreCmd(context.Background(), 1, e, db.Table{Name: "t"},
		"v", true, nil, nil, anchor, 2, 10)
	msg := cmd().(moreRowsMsg)
	got := idsFrom(t, msg.rows)
	want := []int64{3, 4, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("non-PK sort should page by OFFSET (ids %v), got %v", want, got)
	}
}

// TestStatusPagingHint checks the status-line row-position hint: cursor/loaded,
// no ↓ when everything fits, and it tracks the cursor.
func TestStatusPagingHint(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus'),('Grace')`)
	})
	if got := app.statusLine(); !strings.Contains(got, "1/3") || strings.Contains(got, "↓") {
		t.Fatalf("small table hint: want 1/3 and no ↓, got %q", got)
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if got := app.statusLine(); !strings.Contains(got, "2/3") {
		t.Fatalf("after j: want 2/3, got %q", got)
	}
}

// TestStatusPagingHintMore checks the ↓ marker appears when the loaded window is
// only a prefix of the table (more rows exist below).
func TestStatusPagingHintMore(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE nums (id INTEGER PRIMARY KEY, v INTEGER)`)
		for i := 0; i < 500; i++ {
			e.Exec(ctx, `INSERT INTO nums (v) VALUES (?)`, i)
		}
	})
	if !app.grid.hasMore {
		t.Fatal("500 rows should exceed the initial window (hasMore)")
	}
	want := fmt.Sprintf("/%d↓", len(app.grid.rows))
	if got := app.statusLine(); !strings.Contains(got, want) {
		t.Fatalf("want %q in status line, got %q", want, got)
	}
}

// TestSidebarFilter drives the full-screen table list: `/` enters filter mode,
// typing narrows it, arrows move within matches, and Enter loads the highlighted
// table.
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

	// A bare letter must NOT auto-filter anymore — normal mode owns the keys.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if app.sidebar.hasFilter() || app.sidebar.filtering {
		t.Fatal("a letter in nav mode must not start a filter")
	}

	// `/` enters filter mode, then "order" → orders + order_items.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !app.sidebar.filtering {
		t.Fatal("/ should enter filter mode")
	}
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

	// Backspace returns to the table list, keeping the filter narrowed.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatal("Backspace should return to the table list")
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
	// Esc again (no filter) no longer navigates — it stays on the table list.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.screen != screenTables {
		t.Fatal("Esc with no filter should stay on the table list, not navigate")
	}
}

// TestGridBackspaceToTableList: Backspace while navigating the grid steps left to
// the table list.
func TestGridBackspaceToTableList(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})
	if app.screen != screenBrowse {
		t.Fatalf("setup should leave us on the grid, got screen=%d", app.screen)
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatalf("Backspace in the grid should go to the table list, got screen=%d", app.screen)
	}
}

// loadTable is the shared setup: open a fresh sqlite db, run schema/seed, and
// drive the model up to a loaded grid.
func loadTable(t *testing.T, seed func(e db.Engine)) App {
	return loadTableConn(t, config.Conn{Name: "test"}, seed)
}

// loadTableConn is loadTable with a caller-supplied connection (its URL is filled
// in), so tests can flip flags like safe.
func loadTableConn(t *testing.T, conn config.Conn, seed func(e db.Engine)) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seed(e)
	e.Close()

	conn.URL = path
	app := New(nil, conn)
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
	if app.grid.edit.val != "Linus" {
		t.Fatalf("overlay pre-fill = %q, want Linus", app.grid.edit.val)
	}

	// Clear "Linus" and type "Grace", then commit.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd}) // caret opens on the last char
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

// TestQuickEditWriteBackTargetsEditedCell guards the race where a quick edit's
// async result arrives after the cursor moved (or a new edit started): the
// in-memory write-back must land on the cell that was committed, not on whatever
// cell grid.editR/editC happen to point at when the editDoneMsg is handled.
func TestQuickEditWriteBackTargetsEditedCell(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// Default sort is PK descending → visible row 0 is id=2 (Linus), row 1 is
	// id=1 (Ada). Move to "name" and edit row 0's value.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd})
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "Grace")

	// Commit, but hold the async result instead of applying it immediately.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("committing a changed cell should run an UPDATE")
	}
	done := cmd() // runs the UPDATE; carries the target cell coordinates

	// Before the result lands, move to row 1 and start a fresh edit there — this
	// is what used to repoint grid.editR/editC at the wrong cell.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyDown})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if !app.grid.editing || app.grid.editR != 1 {
		t.Fatalf("expected to be editing row 1, editing=%v editR=%d", app.grid.editing, app.grid.editR)
	}

	// Now the first edit's result arrives while row 1 is being edited.
	app = update(t, app, done)

	// The write-back must have hit row 0 (Linus→Grace), not the cell under edit.
	if got := app.grid.rows[0][1]; got != "Grace" {
		t.Fatalf("row 0 name = %v, want Grace (write-back landed on the edited cell)", got)
	}
	if got := app.grid.rows[1][1]; got != "Ada" {
		t.Fatalf("row 1 name = %v, want Ada (must not be clobbered by row 0's edit)", got)
	}
}

// TestQuickEditNull drives the §8 quick path setting a cell to SQL NULL: `e`,
// clear it, type NULL, Enter → the cell becomes real NULL (bound nil), the grid
// shows it as NULL, and the DB row is actually NULL (not the string "NULL").
func TestQuickEditNull(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// PK-desc → row 0 is id=2 (Linus). Move to "name" and edit.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd}) // caret opens on the last char
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "NULL")

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("committing NULL should run an UPDATE")
	}
	app = update(t, app, cmd())

	// Grid shows a real NULL (nil cell), not the string "NULL".
	if got, _, _ := app.grid.currentCell(); got != nil {
		t.Fatalf("grid cell after NULL edit = %#v, want nil", got)
	}
	if !strings.Contains(app.status, "set name = NULL") {
		t.Fatalf("status should report a NULL set, got %q", app.status)
	}

	// And the DB row is SQL NULL, not the text "NULL".
	rs, err := app.engine.Query(context.Background(),
		`SELECT name IS NULL, name FROM users WHERE id = 2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rs.Rows))
	}
	// sqlite returns the boolean as int64(1) for a real NULL.
	if isNull := rs.Rows[0][0]; isNull != int64(1) {
		t.Fatalf("name IS NULL = %#v, want 1 (a real NULL); cell value = %#v", isNull, rs.Rows[0][1])
	}
}

// TestQuickEditLiteralNullString guards that a *lowercase* null stays the string
// "null" — only exactly-uppercase NULL is the SQL-NULL sentinel.
func TestQuickEditLiteralNullString(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd}) // caret opens on the last char
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "null")
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	rs, err := app.engine.Query(context.Background(),
		`SELECT name IS NULL, name FROM users WHERE id = 2`)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Rows[0][0] == int64(1) {
		t.Fatal("lowercase null should stay the string \"null\", not become SQL NULL")
	}
	if rs.Rows[0][1] != "null" {
		t.Fatalf("cell = %#v, want string \"null\"", rs.Rows[0][1])
	}
}

// TestEditCursorMechanics unit-tests the in-cell cursor: insert at the caret,
// ←/→ movement, Home/End, backspace-before-caret, and Ctrl-W delete-word.
func TestEditCursorMechanics(t *testing.T) {
	g := grid{}
	g.edit.val, g.edit.pos = "hello world", len([]rune("hello world")) // caret at end (11)

	g.editLeft()
	g.editLeft()
	if g.edit.pos != 9 {
		t.Fatalf("after ←←, pos = %d, want 9", g.edit.pos)
	}
	g.editInput("X") // insert at the caret, not the end
	if g.edit.val != "hello worXld" || g.edit.pos != 10 {
		t.Fatalf("after insert: %q pos %d, want \"hello worXld\" pos 10", g.edit.val, g.edit.pos)
	}
	g.editDeleteWord() // Ctrl-W: removes "worX" back to the space
	if g.edit.val != "hello ld" || g.edit.pos != 6 {
		t.Fatalf("after Ctrl-W: %q pos %d, want \"hello ld\" pos 6", g.edit.val, g.edit.pos)
	}
	g.editHome()
	if g.edit.pos != 0 {
		t.Fatalf("Home pos = %d, want 0", g.edit.pos)
	}
	g.editEnd()
	if g.edit.pos != len([]rune(g.edit.val)) {
		t.Fatalf("End pos = %d, want %d", g.edit.pos, len([]rune(g.edit.val)))
	}

	// Backspace deletes the rune *before* the caret, not the last one.
	g.edit.val, g.edit.pos = "abc", 2
	g.editBackspace()
	if g.edit.val != "ac" || g.edit.pos != 1 {
		t.Fatalf("mid-string backspace: %q pos %d, want \"ac\" pos 1", g.edit.val, g.edit.pos)
	}

	// Del (forward delete) removes the rune *at* the caret; the caret stays put.
	g.edit.val, g.edit.pos = "abc", 1
	g.editDelete()
	if g.edit.val != "ac" || g.edit.pos != 1 {
		t.Fatalf("forward delete: %q pos %d, want \"ac\" pos 1", g.edit.val, g.edit.pos)
	}
	g.edit.val, g.edit.pos = "ab", 2 // at end → no-op on the text
	g.editDelete()
	if g.edit.val != "ab" {
		t.Fatalf("forward delete at end changed the text: %q", g.edit.val)
	}

	// The inverting caret overlays a char (or a trailing space at end), so the field
	// renders to exactly width w for every caret position — no shift, no short-fill.
	for _, pos := range []int{0, 2, 5} {
		if got := lipgloss.Width(renderEditCell("hello", pos, 6)); got != 6 {
			t.Fatalf("renderEditCell width at caret %d = %d, want 6", pos, got)
		}
	}
}

// TestRenderCaretFieldWidth: the shared caret renderer (filters + cell edit)
// always fills exactly width w — for every caret position and for text that
// overflows (trimmed to keep the caret visible).
func TestRenderCaretFieldWidth(t *testing.T) {
	base := lipgloss.NewStyle()
	cases := []struct {
		text   string
		pos, w int
	}{
		{"hello", 0, 6}, {"hello", 3, 6}, {"hello", 5, 6},
		{"", 0, 4},                             // empty → just the caret + padding
		{"a very long filter pattern", 26, 10}, // overflow, caret at end → trim left
		{"a very long filter pattern", 0, 10},  // overflow, caret at start → trim right
	}
	for _, c := range cases {
		if got := lipgloss.Width(renderCaretField(c.text, c.pos, c.w, base)); got != c.w {
			t.Fatalf("renderCaretField(%q, %d, %d) width = %d, want %d", c.text, c.pos, c.w, got, c.w)
		}
	}
}

// TestQuickEditCursorKeys checks the key wiring: ←/Ctrl-W reach the edit overlay
// and a mid-string insert commits the right value.
func TestQuickEditCursorKeys(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	// PK-desc → row 0 is id=2 (Linus). Edit "name": caret opens at the end.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if app.grid.edit.val != "Linus" || app.grid.edit.pos != 5 {
		t.Fatalf("start: %q pos %d, want Linus pos 5 (at end)", app.grid.edit.val, app.grid.edit.pos)
	}
	// ← twice into the middle, insert, then Ctrl-W wipes back to the word start.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyLeft})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyLeft})
	if app.grid.edit.pos != 3 {
		t.Fatalf("after End ←←, pos = %d, want 3", app.grid.edit.pos)
	}
	app = typeRunes(t, app, "X") // "LinXus", caret after X
	if app.grid.edit.val != "LinXus" {
		t.Fatalf("mid insert = %q, want LinXus", app.grid.edit.val)
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyCtrlW})
	if app.grid.edit.val != "us" || app.grid.edit.pos != 0 {
		t.Fatalf("after Ctrl-W: %q pos %d, want \"us\" pos 0", app.grid.edit.val, app.grid.edit.pos)
	}
	// Del removes the rune at the caret.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyDelete})
	if app.grid.edit.val != "s" || app.grid.edit.pos != 0 {
		t.Fatalf("after Del: %q pos %d, want \"s\" pos 0", app.grid.edit.val, app.grid.edit.pos)
	}
}

// safeUsers seeds a two-row users table for the safe-mode tests.
func safeUsers(e db.Engine) {
	ctx := context.Background()
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (id, name) VALUES (1,'Ada'),(2,'Linus')`)
}

// TestSafeConfirmQuickEdit verifies that on a safe=true connection a quick-path
// edit is held behind the confirmation overlay, then runs when confirmed with 'y'.
func TestSafeConfirmQuickEdit(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	// Row 0 is id=2 (Linus, PK desc). Edit "name" → "Grace" → Enter.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd}) // caret opens on the last char
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "Grace")

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	// Enter arms the overlay instead of running: no command, nothing changed yet.
	if cmd != nil {
		t.Fatal("safe mode must not run the UPDATE before confirmation")
	}
	if !app.confirm.active {
		t.Fatal("safe mode should open the confirmation overlay")
	}
	view := app.View()
	for _, want := range []string{"prod", "UPDATE", "Grace"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm overlay missing %q:\n%s", want, view)
		}
	}
	rs, _ := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 2`)
	if rs.Rows[0][0] != "Linus" {
		t.Fatalf("row must be untouched before confirming, got %v", rs.Rows[0][0])
	}

	// 'y' runs it.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = m.(App)
	if app.confirm.active {
		t.Fatal("'y' should dismiss the overlay")
	}
	if cmd == nil {
		t.Fatal("'y' should dispatch the UPDATE")
	}
	app = update(t, app, cmd())
	rs, _ = app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 2`)
	if rs.Rows[0][0] != "Grace" {
		t.Fatalf("db row after confirm = %v, want Grace", rs.Rows[0][0])
	}
}

// TestSafeCancelQuickEdit verifies any non-'y' key cancels the mutation and
// leaves the row untouched.
func TestSafeCancelQuickEdit(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd}) // caret opens on the last char
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "Grace")
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})
	if !app.confirm.active {
		t.Fatal("expected the confirmation overlay")
	}

	// 'n' (any non-'y' key) cancels.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	app = m.(App)
	if app.confirm.active {
		t.Fatal("a non-'y' key should dismiss the overlay")
	}
	if cmd != nil {
		t.Fatal("cancelling must not run the UPDATE")
	}
	if !strings.Contains(app.status, "cancel") {
		t.Fatalf("status should note the cancellation, got %q", app.status)
	}
	rs, _ := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 2`)
	if rs.Rows[0][0] != "Linus" {
		t.Fatalf("cancelled edit must not change the row, got %v", rs.Rows[0][0])
	}
}

// TestSafeConfirmFullPath verifies a full-path (editor-authored) mutation is also
// gated by the confirmation overlay on a safe connection.
func TestSafeConfirmFullPath(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	m, cmd := app.Update(editorSubmitMsg{sql: `UPDATE users SET name = 'X' WHERE id = 1;`})
	app = m.(App)
	if cmd != nil {
		t.Fatal("safe mode must hold a full-path mutation for confirmation")
	}
	if !app.confirm.active {
		t.Fatal("expected the confirmation overlay for a full-path mutation")
	}

	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("'y' should run the full-path mutation")
	}
	if _, ok := cmd().(execDoneMsg); !ok {
		t.Fatal("confirming a mutation should exec it")
	}
	rs, _ := app.engine.Query(context.Background(), `SELECT name FROM users WHERE id = 1`)
	if rs.Rows[0][0] != "X" {
		t.Fatalf("db row after confirm = %v, want X", rs.Rows[0][0])
	}
}

// TestSafeConfirmFromTableList: a write scratch (s) submitted from the table list
// on a safe connection arms the confirmation overlay AND renders it — regression
// for the overlay having drawn only on the grid screen.
func TestSafeConfirmFromTableList(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	// Go to the table list and open a scratch query there.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatalf("Backspace should open the table list, got screen=%d", app.screen)
	}
	if _, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); cmd == nil {
		t.Fatal("s on the table list should open the editor")
	}

	// Submitting a write arms the confirm overlay, still on the table-list screen.
	m, cmd := app.Update(editorSubmitMsg{sql: "UPDATE users SET name = 'Z' WHERE id = 1;", scratch: true})
	app = m.(App)
	if cmd != nil {
		t.Fatal("safe mode must not run the write before confirmation")
	}
	if !app.confirm.active || app.screen != screenTables {
		t.Fatalf("confirm should be armed on the table list; active=%v screen=%d", app.confirm.active, app.screen)
	}
	// The dialog must actually render on the table-list screen (the bug: it didn't).
	view := app.View()
	for _, want := range []string{"prod", "UPDATE", "Run it?"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm overlay missing %q on the table list:\n%s", want, view)
		}
	}
	// 'y' dismisses it and dispatches the write.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = m.(App)
	if app.confirm.active || cmd == nil {
		t.Fatal("'y' should dismiss the overlay and run the write")
	}
}

// TestSafeReadRunsWithoutConfirm verifies a safe connection still runs free-form
// reads directly — only mutations are gated.
func TestSafeReadRunsWithoutConfirm(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	m, cmd := app.Update(editorSubmitMsg{sql: `SELECT * FROM users;`})
	app = m.(App)
	if app.confirm.active {
		t.Fatal("a read must not trigger the confirmation overlay")
	}
	if cmd == nil {
		t.Fatal("a read should run immediately")
	}
	if _, ok := cmd().(queryResultMsg); !ok {
		t.Fatal("a read submit should produce a query result")
	}
}

// TestSafeMultiStatementReadHeld guards the safe-mode read bypass: a submission
// that leads with a read verb but carries a trailing write must still be held for
// confirmation on a safe connection, not run unconfirmed via the read path.
func TestSafeMultiStatementReadHeld(t *testing.T) {
	app := loadTableConn(t, config.Conn{Name: "prod", Safe: true}, safeUsers)

	m, cmd := app.Update(editorSubmitMsg{sql: `SELECT 1; DELETE FROM users;`})
	app = m.(App)
	if !app.confirm.active {
		t.Fatal("a multi-statement submit on a safe connection must be held for confirmation")
	}
	if cmd != nil {
		t.Fatal("nothing should run before the multi-statement submit is confirmed")
	}
}

// TestQueryHistoryBuffer drives the `b` buffer: an s query is recorded on submit,
// its row count filled in when the result lands, the buffer lists it, Enter
// re-runs a read, and `s` opens it in $EDITOR.
func TestQueryHistoryBuffer(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'), ('Linus')`)
	})

	// Submit an s query (remember set → it enters the connection history).
	m, cmd := app.Update(editorSubmitMsg{sql: "SELECT * FROM users LIMIT 1;", remember: db.Table{Name: "users"}})
	app = m.(App)
	if got := len(app.history["test"]); got != 1 {
		t.Fatalf("submit should record 1 history entry, got %d", got)
	}
	app = update(t, app, cmd()) // queryResultMsg → count filled in

	e := app.history["test"][0]
	if !e.ran || !e.read || e.count != 1 {
		t.Fatalf("history entry outcome wrong: %+v", e)
	}
	// The count equals the query's own LIMIT → the badge hints there may be more.
	if b := histBadge(e); b != "1+ rows" {
		t.Fatalf("badge = %q, want %q", b, "1+ rows")
	}

	// `b` opens the buffer; it lists the query and its badge.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if !app.histView.active {
		t.Fatal("`b` should open the query-history buffer")
	}
	view := app.View()
	for _, want := range []string{"query history", "SELECT * FROM users LIMIT 1", "1+ rows"} {
		if !strings.Contains(view, want) {
			t.Fatalf("history view missing %q:\n%s", want, view)
		}
	}

	// Enter on a read re-runs it directly (returns a query command).
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if app.histView.active {
		t.Fatal("Enter should close the buffer")
	}
	if cmd == nil {
		t.Fatal("Enter on a read entry should run it")
	}
	if _, ok := cmd().(queryResultMsg); !ok {
		t.Fatal("Enter on a read entry should produce a query result")
	}

	// `s` on an entry opens it in $EDITOR (spawns an editor command).
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	app = m.(App)
	if app.histView.active {
		t.Fatal("`s` should close the buffer")
	}
	if cmd == nil {
		t.Fatal("`s` on an entry should open $EDITOR")
	}
}

// TestQueryHistoryWriteOpensEditor guards that Enter on a non-read history entry
// opens it in $EDITOR for review rather than running it unseen.
func TestQueryHistoryWriteOpensEditor(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})
	// A write query submitted via s runs and is recorded.
	m, cmd := app.Update(editorSubmitMsg{sql: "UPDATE users SET name = 'Bob' WHERE id = 1;", remember: db.Table{Name: "users"}})
	app = m.(App)
	app = update(t, app, cmd()) // execDoneMsg → reload command
	if got := len(app.history["test"]); got != 1 {
		t.Fatalf("write submit should record 1 history entry, got %d", got)
	}

	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if app.histView.active {
		t.Fatal("Enter should close the buffer")
	}
	if cmd == nil {
		t.Fatal("Enter on a write entry should open $EDITOR")
	}
	// It opens the editor (an exec spawn), not the exec path — the write must not
	// run unseen.
	if _, ok := cmd().(execDoneMsg); ok {
		t.Fatal("Enter on a write entry must not run it directly")
	}
}

// TestFailedQueryArmsErrView drives a failing free-form (s) query: instead of
// collapsing to a one-line status, it arms the errView modal showing the full
// engine error alongside the query, and `e` reopens it in $EDITOR to fix and
// re-run.
func TestFailedQueryArmsErrView(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	const bad = "SELECT * FROM nope;"
	m, cmd := app.Update(editorSubmitMsg{sql: bad, remember: db.Table{Name: "users"}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("submitting a query should run it")
	}
	// The run fails → an errMsg carrying the re-edit seed → the modal.
	msg := cmd()
	if em, ok := msg.(errMsg); !ok || em.seed == nil {
		t.Fatalf("a failed query should return errMsg with a seed, got %#v", msg)
	}
	app = update(t, app, msg)
	if !app.errView.active {
		t.Fatal("a failed query should arm the errView modal")
	}

	// The modal shows the full error and the failing statement.
	view := app.View()
	for _, want := range []string{"query failed", "nope", bad} {
		if !strings.Contains(view, want) {
			t.Fatalf("errView missing %q:\n%s", want, view)
		}
	}

	// `e` closes the modal and reopens the query in $EDITOR (spawns a command),
	// preserving the s remember marker so a re-run continues the edit loop.
	if app.errView.seed.sql != bad || app.errView.seed.remember.Name != "users" {
		t.Fatalf("re-edit seed = %+v, want the failing sql + remember users", app.errView.seed)
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = m.(App)
	if app.errView.active {
		t.Fatal("`e` should close the modal")
	}
	if cmd == nil {
		t.Fatal("`e` should reopen the query in $EDITOR")
	}

	// Re-arm via another failure; Esc just dismisses the modal.
	m, cmd = app.Update(editorSubmitMsg{sql: bad})
	app = update(t, m.(App), cmd())
	if !app.errView.active {
		t.Fatal("the second failure should re-arm the modal")
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.errView.active {
		t.Fatal("Esc should dismiss the modal")
	}
}

// TestFailedQuickEditArmsErrView guards that a failing quick-path cell edit also
// surfaces the modal, reopening as the equivalent E full-path UPDATE.
func TestFailedQuickEditArmsErrView(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT UNIQUE)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// Move to the "name" column and rename row 0 (id=2, Linus) to "Ada" — a UNIQUE
	// violation.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnd})
	for range "Linus" {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, "Ada")
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("committing a changed cell should run an UPDATE")
	}
	app = update(t, app, cmd()) // the UPDATE fails on the UNIQUE constraint

	if !app.errView.active {
		t.Fatal("a failed quick edit should arm the errView modal")
	}
	// It reopens as an editable, PK-keyed UPDATE with the attempted value.
	seed := app.errView.seed.sql
	for _, want := range []string{"UPDATE", "SET", "WHERE", "'Ada'"} {
		if !strings.Contains(seed, want) {
			t.Fatalf("re-edit seed missing %q: %q", want, seed)
		}
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
	if !app.grid.editing || app.grid.edit.val != "" {
		t.Fatalf("editing a NULL cell should open an empty overlay; editing=%v val=%q",
			app.grid.editing, app.grid.edit.val)
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
	if got := dbErr(ctx, 1, context.Canceled); got != nil {
		t.Fatalf("a cancelled op should swallow to nil, got %#v", got)
	}
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := dbErr(live, 1, context.DeadlineExceeded).(errMsg); !ok {
		t.Fatalf("a real failure on a live ctx should be an errMsg")
	}
}

// TestMidSessionErrorRecoverable guards that a recoverable async failure (a
// typo'd ad-hoc query being the common case) surfaces in the status line and
// leaves the grid usable, rather than trapping on a dead-end error screen.
func TestMidSessionErrorRecoverable(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// A mid-session op fails (e.g. a bad SELECT run via `s`).
	app.begin("running query")
	m, _ := app.Update(errMsg{err: errors.New("near \"SLECT\": syntax error"), gen: app.gen})
	app = m.(App)

	if !strings.Contains(app.status, "syntax error") {
		t.Fatalf("the failure should show in the status line, got %q", app.status)
	}
	if app.screen != screenBrowse {
		t.Fatalf("a recoverable error must not leave the grid; screen=%v", app.screen)
	}
	if v := app.View(); strings.Contains(v, "press ctrl-c to quit") {
		t.Fatalf("a recoverable error must not show the dead-end error screen:\n%s", v)
	}
	// The grid is still interactive: a cursor move works and the rows survive.
	if len(app.grid.rows) != 2 {
		t.Fatalf("the loaded rows should survive the error, got %d", len(app.grid.rows))
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if app.grid.cursorR != 1 {
		t.Fatalf("the grid should stay interactive after an error; cursorR=%d", app.grid.cursorR)
	}
}

// TestMoveDuringOpKeepsInFlight guards the scroll-vs-op race: a cursor move while
// another DB op is running must not start a continuous-scroll fetch, because that
// would cancel the in-flight op (begin) and append onto stale rows.
func TestMoveDuringOpKeepsInFlight(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})
	// Pretend more rows exist and an op (e.g. a sort) is already in flight.
	app.grid.hasMore = true
	app.begin("sorting")
	app.activity = "sorting"
	opGen := app.gen

	// A cursor move must not supersede the running op with a load-more.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if app.activity != "sorting" {
		t.Fatalf("a move during an op must not start a load-more; activity=%q", app.activity)
	}
	if app.gen != opGen {
		t.Fatalf("a move during an op must not bump gen (cancel it): gen %d != %d", app.gen, opGen)
	}
	if app.grid.loading {
		t.Fatal("a move during an op must not flag a scroll fetch as loading")
	}
}

// TestStaleResultIgnored guards the gen-token fix: a result carrying an old gen
// (a superseded op that finished late) must neither apply its rows over the
// current view nor cancel the in-flight op.
func TestStaleResultIgnored(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus')`)
	})

	// Arm a fresh in-flight op; remember its gen and activity label.
	app.begin("loading")
	app.activity = "loading"
	curGen := app.gen
	rowsBefore := len(app.grid.rows)

	// A rowsMsg stamped with a superseded gen must be dropped whole.
	stale := rowsMsg{
		table: app.grid.table,
		rs:    &db.ResultSet{Cols: []string{"id"}, Rows: [][]any{{int64(99)}}},
		full:  false,
		gen:   curGen - 1,
	}
	app = update(t, app, stale)

	if app.activity != "loading" {
		t.Fatalf("a stale result must not clear the in-flight activity, got %q", app.activity)
	}
	if app.cancel == nil {
		t.Fatal("a stale result must not cancel the current op")
	}
	if len(app.grid.rows) != rowsBefore {
		t.Fatalf("a stale result must not replace the grid rows: got %d, want %d", len(app.grid.rows), rowsBefore)
	}

	// The matching gen still applies.
	app = update(t, app, rowsMsg{
		table: app.grid.table,
		rs:    &db.ResultSet{Cols: []string{"id"}, Rows: [][]any{{int64(99)}}},
		gen:   curGen,
	})
	if app.activity != "" {
		t.Fatalf("the current op's result should clear the activity, got %q", app.activity)
	}
	if len(app.grid.rows) != 1 {
		t.Fatalf("the current op's result should apply, got %d rows", len(app.grid.rows))
	}
}

// TestCoerceLikeKeepsType guards that a quick edit keeps the cell's driver type
// (so an integer PK stays int64 for later keyed edits) rather than turning into
// a bare string.
func TestCoerceLikeKeepsType(t *testing.T) {
	cases := []struct {
		prev any
		val  string
		want any
	}{
		{int64(1), "42", int64(42)},
		{float64(1.5), "2.5", float64(2.5)},
		{true, "false", false},
		{"txt", "other", "other"},      // string stays string
		{int64(1), "notnum", "notnum"}, // unparseable → raw string
		{nil, "x", "x"},                // prior NULL → string
	}
	for _, c := range cases {
		if got := coerceLike(c.prev, c.val); got != c.want {
			t.Errorf("coerceLike(%#v, %q) = %#v, want %#v", c.prev, c.val, got, c.want)
		}
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
	// Tall enough to show the whole cheat sheet without scrolling.
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 40})

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
	if app.grid.filter.val != "?" {
		t.Fatalf("filter should contain the literal ?, got %q", app.grid.filter.val)
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
// TestTableListScratchQuery: `s` on the table list opens an empty scratch buffer
// (a conn/db comment, no table template), runs the submitted query, and records
// it in the connection's `b` history even though no table is selected.
func TestTableListScratchQuery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if app.screen != screenTables {
		t.Fatalf("should be on the table list, got screen=%d", app.screen)
	}

	// The blank seed is a conn/db comment over an empty body — no SQL, scratch set.
	seed := app.blankScratchSeed()
	if !strings.HasPrefix(seed.sql, "-- test") {
		t.Fatalf("scratch seed should start with a conn/db comment, got %q", seed.sql)
	}
	if !seed.scratch {
		t.Fatal("blank scratch seed should set scratch=true")
	}
	if strings.Contains(seed.sql, "SELECT") {
		t.Fatalf("blank scratch seed should carry no SQL, got %q", seed.sql)
	}

	// s on the table list opens the editor (an ExecProcess we don't drive).
	if _, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); cmd == nil {
		t.Fatal("s on the table list should open the editor")
	}

	// Submitting a read runs it and files it in the connection's history.
	m, cmd := app.Update(editorSubmitMsg{sql: "SELECT name FROM users;", scratch: true})
	app = m.(App)
	if cmd == nil {
		t.Fatal("a read submit should run a query")
	}
	app = update(t, app, cmd()) // queryResultMsg
	if !app.adHoc || len(app.grid.rows) != 1 {
		t.Fatalf("scratch query should show the adHoc result; adHoc=%v rows=%d", app.adHoc, len(app.grid.rows))
	}
	if hist := app.history["test"]; len(hist) != 1 || hist[0].sql != "SELECT name FROM users;" {
		t.Fatalf("scratch query should enter the connection history, got %+v", hist)
	}
}

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

// TestReloadTable verifies `r` re-runs the current table load, picking up rows
// written out-of-band since the first load.
func TestReloadTable(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})
	if len(app.grid.rows) != 1 {
		t.Fatalf("initial load = %d rows, want 1", len(app.grid.rows))
	}

	// A row appears from elsewhere; the grid doesn't know yet.
	if _, err := app.engine.Exec(context.Background(), `INSERT INTO users (name) VALUES ('Grace')`); err != nil {
		t.Fatal(err)
	}

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`r` should dispatch a reload")
	}
	app = update(t, app, cmd())
	if len(app.grid.rows) != 2 {
		t.Fatalf("after reload = %d rows, want 2", len(app.grid.rows))
	}
	if !strings.Contains(app.View(), "Grace") {
		t.Fatalf("reloaded view should include the new row:\n%s", app.View())
	}
}

// TestReloadQuery verifies `r` re-runs the ad-hoc query behind an s/S result.
func TestReloadQuery(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	// Show an ad-hoc query result.
	m, cmd := app.Update(editorSubmitMsg{sql: "SELECT name FROM users ORDER BY id;"})
	app = m.(App)
	app = update(t, app, cmd())
	if !app.adHoc || len(app.grid.rows) != 1 {
		t.Fatalf("query result: adHoc=%v rows=%d", app.adHoc, len(app.grid.rows))
	}

	// Another row lands, then reload re-runs the same query.
	if _, err := app.engine.Exec(context.Background(), `INSERT INTO users (name) VALUES ('Grace')`); err != nil {
		t.Fatal(err)
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`r` should re-run the ad-hoc query")
	}
	msg := cmd()
	if _, ok := msg.(queryResultMsg); !ok {
		t.Fatalf("reload of an ad-hoc view should produce a queryResultMsg, got %T", msg)
	}
	app = update(t, app, msg)
	if !app.adHoc || len(app.grid.rows) != 2 {
		t.Fatalf("after query reload: adHoc=%v rows=%d, want adHoc + 2 rows", app.adHoc, len(app.grid.rows))
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

// TestScratchQueryScopedToDatabase guards that a remembered scratch query is keyed
// by connection+database as well as table, so a same-named table in another
// database (jumplist spans databases) doesn't inherit an unrelated last query.
func TestScratchQueryScopedToDatabase(t *testing.T) {
	app := loadTable(t, func(e db.Engine) {
		ctx := context.Background()
		e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
		e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada')`)
	})

	// Remember a custom query for users on the current database.
	app = update(t, app, editorSubmitMsg{sql: "SELECT name FROM users;", remember: app.currentTable})
	if got := app.scratchSeed().sql; got != "SELECT name FROM users;" {
		t.Fatalf("same db+table should prefill the last query, got %q", got)
	}

	// Same table name, different database → back to the template, not the query.
	app.dbName = "otherdb"
	if got := app.scratchSeed().sql; !strings.Contains(got, `SELECT * FROM "users" LIMIT 100`) {
		t.Fatalf("same table in another database should get the template, got %q", got)
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

// TestYank verifies y copies the cell value and Y the row as JSON, that both
// dispatch a (clipboard) command and set a status hint, and that the grid
// helpers produce the expected text (raw cell value; column-ordered JSON).
func TestYank(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'), ('Linus')`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app = update(t, app, cmd())

	// Cursor is on col 0 (id) — cell yank is the raw value, no display mangling.
	cell, ok := app.grid.yankCell()
	if !ok {
		t.Fatal("yankCell returned !ok")
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("y should dispatch a clipboard command")
	}
	if !strings.Contains(app.status, "copied cell") {
		t.Fatalf("y status = %q, want a 'copied cell' hint", app.status)
	}

	// Row yank is column-ordered JSON; its id matches the cell just yanked.
	got, ok := app.grid.currentRowJSON()
	if !ok {
		t.Fatal("currentRowJSON returned !ok")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("row JSON does not parse: %v\n%s", err, got)
	}
	if fmt.Sprintf("%v", obj["id"]) != cell {
		t.Fatalf("row JSON id = %v, want %q (the yanked cell)", obj["id"], cell)
	}
	if n := fmt.Sprintf("%v", obj["name"]); n != "Ada" && n != "Linus" {
		t.Fatalf("row JSON name = %q, want Ada or Linus", n)
	}
	if idx := strings.Index(got, `"name"`); idx < strings.Index(got, `"id"`) {
		t.Fatalf("row JSON should keep column order (id before name): %s", got)
	}
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Y'}})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Y should dispatch a clipboard command")
	}
	if !strings.Contains(app.status, "copied row") {
		t.Fatalf("Y status = %q, want a 'copied row' hint", app.status)
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
