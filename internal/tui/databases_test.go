package tui

import (
	"context"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// TestDatabaseSwitcher drives the T flow: the database list opens, filters, and
// Enter reconnects. (SQLite has no databases, so the list is fed directly; the
// reconnect reopens the same file, which is enough to exercise the model path.)
func TestDatabaseSwitcher(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE things (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "conn"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	// The database list opens from the fetched names.
	app = update(t, app, databasesMsg{names: []string{"maindb", "otherdb"}})
	if app.screen != screenDatabases || len(app.dbs.tables) != 2 {
		t.Fatalf("expected database list with 2 entries, got screen=%d n=%d", app.screen, len(app.dbs.tables))
	}

	// `/` enters filter mode, narrow down to otherdb, then Enter to switch.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("other")})
	if len(app.dbs.visible) != 1 {
		t.Fatalf("filter should leave 1 database, got %d", len(app.dbs.visible))
	}
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter should dispatch a reconnect")
	}
	app = update(t, app, cmd()) // connectedMsg from switchDBCmd
	if app.screen != screenTables {
		t.Fatalf("after switching, expected the table list, got screen %d", app.screen)
	}
	if app.engine == nil {
		t.Fatal("engine should be set after the switch")
	}

	// Backspace on the database list steps back to the table list.
	app = update(t, app, databasesMsg{names: []string{"maindb", "otherdb"}})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatal("Backspace should step back to the table list")
	}
}

// TestCrossDatabaseJump: the session jumplist spans databases — jumping to a view
// in another database reconnects first, and the list survives the switch.
func TestCrossDatabaseJump(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cd.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE t1 (id INTEGER PRIMARY KEY)`)
	e.Exec(ctx, `CREATE TABLE t2 (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "conn"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Open t1 in the current database, then inject a jumplist entry that lives in
	// another database (as if we'd T-switched and browsed there).
	m, cmd := app.selectTable(db.Table{Name: "t1"})
	app = update(t, m.(App), cmd())
	app.views = append(app.views, viewState{db: "elsewhere", table: db.Table{Name: "t2"}, sortAsc: true})

	// Jump forward to it: a foreign database → must reconnect via pendingView.
	m, cmd = app.jumpBy(1)
	app = m.(App)
	if app.pendingView == nil || app.pendingView.table.Name != "t2" {
		t.Fatalf("cross-db jump should stage a pendingView, got %+v", app.pendingView)
	}
	if cmd == nil {
		t.Fatal("cross-db jump should dispatch a reconnect")
	}

	// connectedMsg (from switchDBCmd) loads the pending view instead of the list.
	m, cmd = app.Update(cmd())
	app = m.(App)
	if app.pendingView != nil {
		t.Fatal("pendingView should be consumed")
	}
	app = update(t, app, cmd()) // rowsMsg
	if app.screen != screenBrowse || app.currentTable.Name != "t2" {
		t.Fatalf("cross-db jump should land on t2 in the grid, got screen=%d table=%q", app.screen, app.currentTable.Name)
	}
	if len(app.views) != 2 {
		t.Fatalf("jumplist should survive the switch, got %d entries", len(app.views))
	}
}

// TestNoDatabases: T on an engine with no databases (SQLite) just notices.
func TestNoDatabases(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "n.db")
	e, _ := db.Open(ctx, path)
	e.Exec(ctx, `CREATE TABLE x (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "conn"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})

	// d from the table list fetches databases (nil for SQLite).
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("d should dispatch a databases fetch")
	}
	app = update(t, app, cmd())
	if app.screen != screenTables {
		t.Fatal("no databases → stay on the table list")
	}
	if app.status == "" {
		t.Fatal("expected a notice about no databases")
	}
}
