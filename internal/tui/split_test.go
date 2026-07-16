package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/jmserra/jsq/internal/db"
)

// leaderKey sends <space> then k.
func leaderKey(t *testing.T, app App, k rune) App {
	t.Helper()
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	if !app.leader {
		t.Fatal("space should arm the leader")
	}
	return update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k}})
}

// paneCols reports each pane's column, for asserting on the arrangement.
func paneColsOf(app App) []int {
	c := make([]int, len(app.panes))
	for i := range app.panes {
		c[i] = app.panes[i].col
	}
	return c
}

// splitTable is the shared setup: a loaded grid, then `<space>v`.
func splitTable(t *testing.T, app App) App {
	t.Helper()
	app = leaderKey(t, app, 'v')
	if len(app.panes) != 2 {
		t.Fatalf("<space>v should split, got %d panes", len(app.panes))
	}
	return app
}

func usersTable(e db.Engine) {
	ctx := context.Background()
	e.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	e.Exec(ctx, `INSERT INTO users (name) VALUES ('Ada'),('Linus'),('Grace')`)
}

// TestGridCloneRowAliasing is the one that matters: two grids cloned from one
// must not share a rows backing array. rows arrive from scanQuery's append loop
// with spare capacity, and appendRows appends in place — so a shared array means
// each pane's scroll silently overwrites the other's rows past len.
func TestGridCloneRowAliasing(t *testing.T) {
	g := newGrid()
	g.cols = []column{{name: "id"}}
	g.rows = make([][]any, 0, 8) // cap > len, exactly as scanQuery leaves it
	g.rows = append(g.rows, []any{int64(1)})
	g.rebuildVisible()

	c := g.clone()

	// Both scroll: each appends its own next window.
	g.appendRows([][]any{{int64(2)}}, false)
	c.appendRows([][]any{{int64(99)}}, false)

	if got := g.rows[1][0]; got != int64(2) {
		t.Fatalf("the clone's append overwrote the source's row: got %v, want 2", got)
	}
	if got := c.rows[1][0]; got != int64(99) {
		t.Fatalf("clone lost its own appended row: got %v, want 99", got)
	}
}

// TestGridCloneFilterAliasing: the filter maps are mutated in place, so a shared
// map would let one pane's committed filter rewrite the other's WHERE clause.
func TestGridCloneFilterAliasing(t *testing.T) {
	g := newGrid()
	g.filters[0] = "ada"
	g.filtersWide[0] = true

	c := g.clone()
	c.filters[0] = "linus"
	c.filtersWide[0] = false
	c.filters[1] = "extra"

	if g.filters[0] != "ada" || !g.filtersWide[0] {
		t.Fatalf("clone rewrote the source's filters: %v %v", g.filters, g.filtersWide)
	}
	if _, ok := g.filters[1]; ok {
		t.Fatal("clone leaked a new filter into the source")
	}
}

// TestSplitJumplistAliasing: views is deep-copied, so the two panes' jumplists
// are independent. syncCurrent writes views[viewIdx], and a clone starts at an
// identical viewIdx — a shared array would have the first navigation in either
// pane clobber the other's history at that index.
func TestSplitJumplistAliasing(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app)

	if len(app.panes[0].views) != 1 || len(app.panes[1].views) != 1 {
		t.Fatalf("both panes should start with the source's one view, got %d/%d",
			len(app.panes[0].views), len(app.panes[1].views))
	}
	// Rewrite the focused pane's entry; the other pane's must not move.
	app.panes[1].views[0].table = db.Table{Name: "clobbered"}
	if app.panes[0].views[0].table.Name != "users" {
		t.Fatalf("panes share a jumplist backing array: pane 0 now shows %q",
			app.panes[0].views[0].table.Name)
	}
}

// TestSplitDuplicatesView: `<space>v` shows exactly what was on screen, and the
// new pane is focused so navigating leaves the original behind.
func TestSplitDuplicatesView(t *testing.T) {
	app := loadTable(t, usersTable)
	app.g().moveRow(1) // put the cursor somewhere non-default
	app = splitTable(t, app)

	if app.focus != 1 {
		t.Fatalf("the split should focus the new pane, got focus=%d", app.focus)
	}
	src, dst := app.panes[0], app.panes[1]
	if dst.currentTable.Name != src.currentTable.Name {
		t.Fatalf("clone shows %q, source shows %q", dst.currentTable.Name, src.currentTable.Name)
	}
	if dst.grid.cursorR != src.grid.cursorR {
		t.Fatalf("clone should keep the cursor at %d, got %d", src.grid.cursorR, dst.grid.cursorR)
	}
	if len(dst.grid.rows) != len(src.grid.rows) {
		t.Fatalf("clone has %d rows, source has %d", len(dst.grid.rows), len(src.grid.rows))
	}
	if dst.id == src.id {
		t.Fatal("the clone must get its own pane id")
	}
}

// TestSplitDuplicatesAdHocQuery: splitting a free-form (s) query result must
// clone the RESULT, not silently fall back to the underlying table. This is why
// the clone goes through the live pane and not viewState (which has no adHoc).
func TestSplitDuplicatesAdHocQuery(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, queryResultMsg{
		rs:  &db.ResultSet{Cols: []string{"n"}, Rows: [][]any{{int64(7)}}},
		sql: "SELECT 7 AS n", gen: app.gen,
	})
	if !app.p().adHoc {
		t.Fatal("setup: the query result should be adHoc")
	}
	app = splitTable(t, app)

	if !app.p().adHoc || app.p().adHocQuery != "SELECT 7 AS n" {
		t.Fatalf("clone lost the query result: adHoc=%v sql=%q", app.p().adHoc, app.p().adHocQuery)
	}
	if len(app.g().rows) != 1 || app.g().rows[0][0] != int64(7) {
		t.Fatalf("clone should show the query's rows, got %v", app.g().rows)
	}
}

// TestSplitPanesNavigateIndependently: the whole point — moving in one pane
// leaves the other exactly where it was.
func TestSplitPanesNavigateIndependently(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app)

	before := app.panes[0].grid.cursorR
	for i := 0; i < 2; i++ {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	if app.panes[1].grid.cursorR == before {
		t.Fatal("j should have moved the focused pane's cursor")
	}
	if app.panes[0].grid.cursorR != before {
		t.Fatalf("the unfocused pane's cursor moved: %d -> %d", before, app.panes[0].grid.cursorR)
	}
}

// TestSplitFocusMovement: <space>h/l walk the pane row; j/k are inert with only
// vertical splits, and movement stops at the ends rather than wrapping.
func TestSplitFocusMovement(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app) // focus = 1 (the new right-hand pane)

	if app = leaderKey(t, app, 'h'); app.focus != 0 {
		t.Fatalf("<space>h should focus the left pane, got %d", app.focus)
	}
	if app = leaderKey(t, app, 'h'); app.focus != 0 {
		t.Fatalf("<space>h at the left edge should stay put, got %d", app.focus)
	}
	if app = leaderKey(t, app, 'l'); app.focus != 1 {
		t.Fatalf("<space>l should focus the right pane, got %d", app.focus)
	}
	if app = leaderKey(t, app, 'j'); app.focus != 1 {
		t.Fatalf("<space>j with no pane below should stay put, got %d", app.focus)
	}
}

// TestSplitHorizontal: <space>s stacks a copy below, in the same column and at
// full width, and it duplicates the view like <space>v does.
func TestSplitHorizontal(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 20})
	app = leaderKey(t, app, 's')

	if len(app.panes) != 2 || app.focus != 1 {
		t.Fatalf("<space>s should stack a focused pane, got %d panes focus=%d", len(app.panes), app.focus)
	}
	if got := paneColsOf(app); got[0] != 0 || got[1] != 0 {
		t.Fatalf("a horizontal split stays in one column, got cols=%v", got)
	}
	top, bot := app.panes[0], app.panes[1]
	if top.w != bot.w {
		t.Fatalf("stacked panes should be the same width, got %d and %d", top.w, bot.w)
	}
	if bot.y <= top.y {
		t.Fatalf("the new pane should sit below: top y=%d bottom y=%d", top.y, bot.y)
	}
	if bot.currentTable.Name != top.currentTable.Name {
		t.Fatalf("<space>s should duplicate the view, got %q vs %q",
			bot.currentTable.Name, top.currentTable.Name)
	}
}

// TestSplitGridFocusMovement: v then s gives a full-height left column and a
// stacked right one. Focus must move by geometry — in particular `j` from the
// left pane must not jump into the right column's lower pane just because it
// sits lower, and `h` from either right-hand pane reaches the left one.
func TestSplitGridFocusMovement(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 20})
	app = leaderKey(t, app, 'v') // panes: 0 | 1        focus=1
	app = leaderKey(t, app, 's') // panes: 0 | 1,2      focus=2

	if got := paneColsOf(app); len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("expected one pane left and two right, got cols=%v", got)
	}
	// From the bottom-right pane: k reaches the top-right, h reaches the left.
	if app = leaderKey(t, app, 'k'); app.focus != 1 {
		t.Fatalf("<space>k should reach the pane above, got %d", app.focus)
	}
	if app = leaderKey(t, app, 'h'); app.focus != 0 {
		t.Fatalf("<space>h should reach the left column, got %d", app.focus)
	}
	// The left pane spans the full height, so nothing is above or below it: j must
	// stay put rather than jumping across to the right column's lower pane.
	if app = leaderKey(t, app, 'j'); app.focus != 0 {
		t.Fatalf("<space>j must not cross columns, got %d", app.focus)
	}
	// l from the left lands on the nearest overlapping pane — the top-right one.
	if app = leaderKey(t, app, 'l'); app.focus != 1 {
		t.Fatalf("<space>l should reach the top-right pane, got %d", app.focus)
	}
}

// TestSplitCloseCollapsesColumn: closing a column's last pane must not leave a
// hole — paneCols indexes by col, so a gap would render as an empty column.
func TestSplitCloseCollapsesColumn(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 20})
	app = leaderKey(t, app, 'v') // panes: 0 | 1   focus=1
	app = leaderKey(t, app, 'v') // panes: 0 | 1 | 2, focus=2 (middle-right)
	if got := paneColsOf(app); len(got) != 3 || got[2] != 2 {
		t.Fatalf("expected three columns, got cols=%v", got)
	}
	// Close the middle column; the right one must slide left into the gap.
	app = leaderKey(t, app, 'h')
	if app.focus != 1 {
		t.Fatalf("setup: expected focus on the middle column, got %d", app.focus)
	}
	app = leaderKey(t, app, 'q')
	got := paneColsOf(app)
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("closing a column should leave contiguous columns, got %v", got)
	}
}

// TestSplitClose: <space>q drops the focused pane; the last one can't be closed.
func TestSplitClose(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app)

	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if len(app.panes) != 1 || app.focus != 0 {
		t.Fatalf("<space>q should leave one focused pane, got %d panes focus=%d", len(app.panes), app.focus)
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if len(app.panes) != 1 {
		t.Fatal("the last pane must not be closable")
	}
}

// TestSplitRefusesDatabaseSwitch: panes share one engine, so a conn/db change
// while split would pull the database out from under the other panes.
func TestSplitRefusesDatabaseSwitch(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app)

	m, cmd := app.switchDatabase("elsewhere")
	app = m.(App)
	if cmd != nil {
		t.Fatal("a database switch while split must not dispatch a reconnect")
	}
	if !strings.Contains(app.status, "close the split") {
		t.Fatalf("the refusal should say how to proceed, got %q", app.status)
	}
	// With one pane it goes through as before.
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if _, cmd := app.switchDatabase("elsewhere"); cmd == nil {
		t.Fatal("unsplit, a database switch should dispatch a reconnect again")
	}
}

// TestUnsplitHeaderUnchanged: the split must cost nothing when you aren't using
// it — one pane renders the same single header line (conn > db > table + paging)
// and no pane header.
func TestUnsplitHeaderUnchanged(t *testing.T) {
	app := loadTable(t, usersTable)
	h := ansi.Strip(app.statusLine())
	if !strings.Contains(h, "users") || !strings.Contains(h, "1/3") {
		t.Fatalf("unsplit header should carry the table crumb and paging: %q", h)
	}
	body := app.panesView()
	if strings.Contains(body, "│") {
		t.Fatalf("unsplit body should have no divider:\n%s", body)
	}
}

// TestSplitLayoutFillsWidth: every rendered line spans the terminal exactly, the
// body is exactly as tall as the screen allows, and the divider sits in one
// straight column all the way down. Measured in display cells, not runes — the
// sort marker (▼) and the more-rows arrow (↓) are one rune but two columns.
//
// Run over each arrangement: a plain vertical split, a plain horizontal one, and
// the v-then-s grid where a full-height column sits beside a stacked one.
func TestSplitLayoutFillsWidth(t *testing.T) {
	arrangements := map[string][]rune{
		"v":   {'v'},
		"s":   {'s'},
		"v+s": {'v', 's'},
		"s+v": {'s', 'v'},
	}
	for _, size := range []struct{ w, h int }{{80, 24}, {160, 40}, {90, 12}} {
		for name, keys := range arrangements {
			app := loadTable(t, usersTable)
			app = update(t, app, tea.WindowSizeMsg{Width: size.w, Height: size.h})
			for _, k := range keys {
				app = leaderKey(t, app, k)
			}

			lines := strings.Split(app.View(), "\n")
			if len(lines) != size.h {
				t.Fatalf("%s @%dx%d: rendered %d lines, want %d", name, size.w, size.h, len(lines), size.h)
			}
			col := -1
			for i, ln := range lines {
				plain := ansi.Strip(ln)
				if w := ansi.StringWidth(plain); i > 0 && w != size.w {
					t.Fatalf("%s @%dx%d: line %d is %d cells wide, want %d: %q",
						name, size.w, size.h, i, w, size.w, plain)
				}
				idx := strings.IndexRune(plain, '│')
				if idx < 0 {
					continue
				}
				at := ansi.StringWidth(plain[:idx])
				if col < 0 {
					col = at
				} else if at != col {
					t.Fatalf("%s @%dx%d: divider jumps to column %d (expected %d) on line %d: %q",
						name, size.w, size.h, at, col, i, plain)
				}
			}
			// A horizontal-only split has no columns, so no divider to check.
			if col < 0 && name != "s" {
				t.Fatalf("%s @%dx%d: no divider rendered", name, size.w, size.h)
			}
		}
	}
}

// TestExecReloadsWritingPane: a write's reload must go to the pane the write ran
// against, even if focus moved while it was in flight. Reloading whatever is
// focused now would both miss the change and stomp an unrelated view.
func TestExecReloadsWritingPane(t *testing.T) {
	app := loadTable(t, usersTable)
	app = splitTable(t, app) // focus = 1

	// Point the two panes at different tables so the reload target is visible.
	app.panes[0].currentTable = db.Table{Name: "authors"}
	app.panes[1].currentTable = db.Table{Name: "users"}

	// A write dispatched from pane 1...
	ctx := app.begin("running", app.panes[1].id)
	_ = ctx
	gen := app.gen
	// ...then focus moves to pane 0 before the result lands.
	app.focus = 0

	m, cmd := app.Update(execDoneMsg{sql: "UPDATE users SET name='x'", affected: 1, gen: gen})
	app = m.(App)
	if cmd == nil {
		t.Fatal("a write should trigger a reload")
	}
	if app.opPane != app.panes[1].id {
		t.Fatalf("the reload should target the pane that wrote (id %d), got opPane=%d",
			app.panes[1].id, app.opPane)
	}
	// And the reload must carry that pane's table, not the focused pane's.
	msg := cmd()
	rows, ok := msg.(rowsMsg)
	if !ok {
		t.Fatalf("expected a rowsMsg, got %T", msg)
	}
	if rows.table.Name != "users" {
		t.Fatalf("reloaded %q, want the writing pane's table users", rows.table.Name)
	}
}

// TestLeaderDroppedWhenScreenChanges: the leader is consumed before the modal
// captures, so a message landing between <space> and the next key must not let
// the leader steal a key aimed at whatever is now on screen.
func TestLeaderDroppedWhenModalArms(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	if !app.leader {
		t.Fatal("setup: space should arm the leader")
	}
	// A slow query fails and arms the failure modal before the next key.
	seed := editorSeed{sql: "SELECT boom"}
	app = update(t, app, errMsg{err: context.DeadlineExceeded, seed: &seed, gen: app.gen})
	if !app.errView.active {
		t.Fatal("setup: the errMsg should arm the failure modal")
	}
	// 'v' now belongs to the modal, not the leader: it must not split.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	if len(app.panes) != 1 {
		t.Fatalf("the leader must not split from behind a modal, got %d panes", len(app.panes))
	}
	if app.leader {
		t.Fatal("the leader should be spent either way")
	}
}

// TestLeaderNotArmedWhileTyping: <space> is a literal character inside a filter
// or a cell edit, never the split leader.
func TestLeaderNotArmedWhileTyping(t *testing.T) {
	app := loadTable(t, usersTable)
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}) // filter the column
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	if app.leader {
		t.Fatal("space inside a column filter must not arm the leader")
	}
	if !strings.Contains(app.g().filter.val, " ") {
		t.Fatalf("space should have been typed into the filter, got %q", app.g().filter.val)
	}
}
