package tui

import (
	"context"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// twoConns makes two SQLite files as two distinct connections.
func twoConns(t *testing.T) (config.Conn, config.Conn) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	pa := filepath.Join(dir, "a.db")
	pb := filepath.Join(dir, "b.db")
	ea, _ := db.Open(ctx, pa)
	ea.Exec(ctx, `CREATE TABLE ta (id INTEGER PRIMARY KEY)`)
	ea.Close()
	eb, _ := db.Open(ctx, pb)
	eb.Exec(ctx, `CREATE TABLE tb (id INTEGER PRIMARY KEY)`)
	eb.Close()
	return config.Conn{Name: "A", URL: pa}, config.Conn{Name: "B", URL: pb}
}

// selectPickerConn moves the picker cursor to index i and connects.
func connectPicker(t *testing.T, app App, i int) App {
	t.Helper()
	app.picker.cursor = i
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("picker Enter should dispatch a connect")
	}
	return update(t, app, cmd()) // connectedMsg
}

// openTable selects the first table and loads it.
func openFirstTable(t *testing.T, app App) App {
	t.Helper()
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter on the table list should load a table")
	}
	return update(t, app, cmd()) // rowsMsg
}

// TestMultiConnectionJump: open A, switch to B via c, and Ctrl-O back to A — the
// jumplist spans connections and reconnects.
func TestMultiConnectionJump(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{}) // picker mode
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if app.screen != screenPicker {
		t.Fatalf("bare jsq should start on the picker, got screen %d", app.screen)
	}

	// Connect A, open ta.
	app = connectPicker(t, app, 0)
	if app.connName != "A" || app.screen != screenTables {
		t.Fatalf("expected to land on A's table list, got conn=%q screen=%d", app.connName, app.screen)
	}
	if !app.tunneled["A"] {
		t.Fatal("A should be marked tunneled after connecting")
	}
	app = openFirstTable(t, app)
	if app.currentTable.Name != "ta" {
		t.Fatalf("expected ta, got %q", app.currentTable.Name)
	}

	// c → picker, connect B, open tb.
	m, _ := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	app = m.(App)
	if app.screen != screenPicker {
		t.Fatal("c should open the connection picker")
	}
	app = connectPicker(t, app, 1)
	if app.connName != "B" {
		t.Fatalf("expected to switch to B, got %q", app.connName)
	}
	app = openFirstTable(t, app)
	if app.currentTable.Name != "tb" {
		t.Fatalf("expected tb, got %q", app.currentTable.Name)
	}

	// The jumplist spans both connections.
	if len(app.views) != 2 || app.views[0].conn != "A" || app.views[1].conn != "B" {
		t.Fatalf("jumplist should span A and B, got %+v", app.views)
	}

	// Ctrl-O: cross-connection jump back to A/ta (reconnects A). A's view was
	// cached when we left it, so once reconnected it restores from memory with no
	// further rows fetch.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	app = m.(App)
	if app.pendingView == nil {
		t.Fatal("cross-connection jump should stage a pendingView")
	}
	m, cmd = app.Update(cmd()) // connectedMsg for A → instant cache restore
	app = m.(App)
	if cmd != nil {
		t.Fatal("a cached view should restore without a further rows fetch")
	}
	if app.connName != "A" || app.currentTable.Name != "ta" || app.screen != screenBrowse {
		t.Fatalf("Ctrl-O should land on A/ta, got conn=%q table=%q screen=%d",
			app.connName, app.currentTable.Name, app.screen)
	}
}

// TestReselectSameConnection: choosing the current connection just shows its tables.
func TestReselectSameConnection(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{})
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	app = connectPicker(t, app, 0)
	app = openFirstTable(t, app)

	// c, then re-pick A (index 0): no reconnect, just its table list.
	m, _ := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	app = m.(App)
	app.picker.cursor = 0
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd != nil {
		t.Fatal("re-selecting the current connection should not reconnect")
	}
	if app.screen != screenTables {
		t.Fatal("re-selecting the current connection should show its table list")
	}
}
