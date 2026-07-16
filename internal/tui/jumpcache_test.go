package tui

import (
	"context"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// jumpCacheApp opens a sqlite file with two small tables and lands the app on the
// grid for table t, cursor at top-left.
func jumpCacheApp(t *testing.T) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jc.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT)`)
	e.Exec(ctx, `INSERT INTO t (a, b) VALUES ('a1','b1'),('a2','b2'),('a3','b3'),('a4','b4'),('a5','b5')`)
	e.Exec(ctx, `CREATE TABLE u (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "c"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := app.selectTable(db.Table{Name: "t"})
	return update(t, m.(App), cmd())
}

// TestJumpRestoresPosition: move the cursor, leave the view, jump back — the
// cursor lands exactly where it was, restored instantly from the cache.
func TestJumpRestoresPosition(t *testing.T) {
	app := jumpCacheApp(t)

	// Move the cursor to row 2, column 2 on t.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if app.g().cursorR != 2 || app.g().cursorC != 2 {
		t.Fatalf("setup: cursor should be at 2,2, got %d,%d", app.g().cursorR, app.g().cursorC)
	}

	// Navigate away to u, then Ctrl-O back to t.
	m, cmd := app.selectTable(db.Table{Name: "u"})
	app = update(t, m.(App), cmd())
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	app = m.(App)
	if cmd != nil {
		t.Fatal("returning to a cached view should not reload")
	}
	if app.p().currentTable.Name != "t" {
		t.Fatalf("Ctrl-O should land on t, got %q", app.p().currentTable.Name)
	}
	if app.g().cursorR != 2 || app.g().cursorC != 2 {
		t.Fatalf("jump should restore cursor to 2,2, got %d,%d", app.g().cursorR, app.g().cursorC)
	}
	// The rows came from the cache, not a fresh query.
	if len(app.g().rows) != 5 {
		t.Fatalf("cached view should hold all 5 rows, got %d", len(app.g().rows))
	}
}

// TestJumpReloadsWhenEvicted: with the cached snapshot dropped (memory bound),
// the jump falls back to a reload and still repositions to the saved cursor.
func TestJumpReloadsWhenEvicted(t *testing.T) {
	app := jumpCacheApp(t)
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // row 2

	m, cmd := app.selectTable(db.Table{Name: "u"})
	app = update(t, m.(App), cmd())

	// Simulate eviction: drop the row cache on t's entry, keeping its metadata.
	app.p().views[0].snap = nil

	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	app = m.(App)
	if cmd == nil {
		t.Fatal("an evicted view must reload")
	}
	if app.pendingPos == nil || app.pendingPos.cursorR != 2 {
		t.Fatalf("reload should carry the saved cursor row, got %+v", app.pendingPos)
	}
	app = update(t, app, cmd()) // rowsMsg
	if app.p().currentTable.Name != "t" || app.g().cursorR != 2 {
		t.Fatalf("reload should reposition to row 2 on t, got %q row=%d", app.p().currentTable.Name, app.g().cursorR)
	}
	if app.pendingPos != nil {
		t.Fatal("pendingPos should be consumed by the load")
	}
}

// TestSnapshotCacheBounded: visiting many distinct table views keeps only the
// most-recent maxCachedViews snapshots resident.
func TestSnapshotCacheBounded(t *testing.T) {
	app := jumpCacheApp(t) // one view (t) already cached-eligible

	// Fan out across many synthetic views so eviction has to kick in. Each carries
	// its own snapshot; syncCurrent (via navigate) caches the outgoing one.
	for i := 0; i < maxCachedViews+5; i++ {
		m, cmd := app.selectTable(db.Table{Name: "t"})
		app = update(t, m.(App), cmd())
	}
	live := 0
	for i := range app.p().views {
		if app.p().views[i].snap != nil {
			live++
		}
	}
	if live > maxCachedViews {
		t.Fatalf("cache should retain at most %d snapshots, got %d", maxCachedViews, live)
	}
}
