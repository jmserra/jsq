package tui

import (
	"context"
	"path/filepath"
	"strings"
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
	app.connList.cursor = i
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

// TestReconnectShowsLoader: a mid-session connection switch shows the animated
// connecting loader (naming the target) even without a cmd/port to wait on, so a
// non-instant open doesn't feel like nothing is happening.
func TestReconnectShowsLoader(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{})
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	app = connectPicker(t, app, 0) // connect A (starts the perpetual spinner loop)
	app = openFirstTable(t, app)

	// c → picker, select B and dispatch the switch, but hold the connectedMsg: the
	// reconnect is now in flight.
	m, _ := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	app = m.(App)
	app.connList.cursor = 1
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("selecting another connection should dispatch a reconnect")
	}
	if !app.isConnecting() {
		t.Fatal("a mid-session reconnect should be flagged as connecting")
	}
	if v := app.View(); !strings.Contains(v, "connecting to B") {
		t.Fatalf("the connecting loader should name the target:\n%s", v)
	}
	// While it's in flight the loader owns the screen: a stray key is swallowed.
	before := app.screen
	m, _ = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	app = m.(App)
	if app.screen != before {
		t.Fatal("keys must be swallowed while the connecting loader is up")
	}
	// Delivering the connectedMsg dismisses the loader.
	app = update(t, app, cmd())
	if app.isConnecting() {
		t.Fatal("connectedMsg should dismiss the connecting loader")
	}
}

// TestPickerFirstConnectShowsLoader: the very first connect from the startup
// picker (no perpetual spinner loop yet) still shows the animated loader, so a
// slow open doesn't leave the picker sitting there with no feedback.
func TestPickerFirstConnectShowsLoader(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{}) // picker mode
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !app.ticking {
		t.Fatal("picker mode should reserve the spinner loop (Init dispatches it)")
	}

	// Select the first connection and dispatch the connect, holding the
	// connectedMsg so the connect is in flight.
	app.connList.cursor = 0
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("picker Enter should dispatch a connect")
	}
	if !app.isConnecting() {
		t.Fatal("the first picker connect should show the connecting loader")
	}
	if v := app.View(); !strings.Contains(v, "connecting to A") {
		t.Fatalf("the loader should name the target connection:\n%s", v)
	}
	app = update(t, app, cmd()) // connectedMsg
	if app.isConnecting() || app.screen != screenTables {
		t.Fatalf("connectedMsg should dismiss the loader and show tables; screen=%d", app.screen)
	}
}

// TestTableListBackspaceToPicker: on the table list with no filter, Backspace
// steps up to the connection picker (and Esc there returns to the table list).
// Esc on the table list does NOT go to the picker — with no grid it stays put.
func TestTableListBackspaceToPicker(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{})
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	app = connectPicker(t, app, 0) // lands on the table list, no table opened
	if app.screen != screenTables || app.currentTable.Name != "" {
		t.Fatalf("expected the table list with nothing opened, screen=%d table=%q",
			app.screen, app.currentTable.Name)
	}

	// Esc with no filter and no grid → stays on the table list (no double-Esc jump).
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.screen != screenTables {
		t.Fatalf("Esc on an empty table list should stay put, got screen=%d", app.screen)
	}

	// Backspace (no filter) → the connection picker.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenPicker {
		t.Fatalf("Backspace on the table list should go to the connection picker, got screen=%d", app.screen)
	}
	// Esc on the picker no longer navigates (it's the leftmost screen); it stays put.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.screen != screenPicker {
		t.Fatalf("Esc on the picker should stay put, got screen=%d", app.screen)
	}
	// Enter re-selects the current connection A (moving right) → the table list.
	app.connList.cursor = 0
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})
	if app.screen != screenTables {
		t.Fatalf("Enter on the current connection should return to its table list, got screen=%d", app.screen)
	}

	// In filter mode (entered with `/`), Backspace edits the pattern rather than
	// leaving the list.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app = typeRunes(t, app, "zz") // no match, but a live filter
	if !app.sidebar.hasFilter() {
		t.Fatal("typing in filter mode should build a filter")
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables || app.sidebar.filter.val != "z" {
		t.Fatalf("Backspace in filter mode should delete a char, got screen=%d filter=%q",
			app.screen, app.sidebar.filter.val)
	}
}

// TestPickerFilter: the connection picker navigates by default (a bare letter
// does not filter) and `/` enters filter mode; narrowing to one connection and
// pressing Enter connects to it.
func TestPickerFilter(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{})
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if app.screen != screenPicker {
		t.Fatalf("bare jsq should open the picker, got screen=%d", app.screen)
	}

	// A bare letter must not auto-filter — nav mode owns the keys.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if app.connList.hasFilter() || app.connList.filtering {
		t.Fatal("a letter in the picker must not start a filter")
	}

	// `/` enters filter mode; "B" narrows to connection B.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !app.connList.filtering {
		t.Fatal("/ should enter filter mode in the picker")
	}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if len(app.connList.visible) != 1 {
		t.Fatalf("filter should leave 1 connection, got %d", len(app.connList.visible))
	}

	// Enter connects to the highlighted connection and leaves filter mode.
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter should dispatch a connect")
	}
	app = update(t, app, cmd()) // connectedMsg
	if app.connName != "B" {
		t.Fatalf("should have connected to B, got %q", app.connName)
	}
	if app.connList.filtering {
		t.Fatal("connecting should leave filter mode")
	}
}

// TestCancelReconnect: Esc while connecting cancels the connect, rolls the
// identity back, returns to the previous page, and the in-flight connect's late
// result is discarded (the old engine stays usable).
func TestCancelReconnect(t *testing.T) {
	connA, connB := twoConns(t)
	app := New([]config.Conn{connA, connB}, config.Conn{})
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	app = connectPicker(t, app, 0) // connect A
	app = openFirstTable(t, app)
	engineA := app.engine

	// c → picker, select B, dispatch the switch (in flight, hold the result).
	m, _ := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	app = m.(App)
	app.connList.cursor = 1
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if !app.isConnecting() {
		t.Fatal("should be connecting to B")
	}
	if v := app.View(); !strings.Contains(v, "esc to cancel") {
		t.Fatalf("the loader should offer esc to cancel:\n%s", v)
	}

	// Esc cancels → back to the picker, identity rolled back to A.
	m, _ = app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	app = m.(App)
	if app.isConnecting() {
		t.Fatal("Esc should cancel the connect")
	}
	if app.screen != screenPicker {
		t.Fatalf("Esc should return to the previous page (the picker), got screen=%d", app.screen)
	}
	if app.connName != "A" {
		t.Fatalf("identity should roll back to A, got %q", app.connName)
	}

	// The in-flight connect finishes late: its result must be discarded, not applied.
	m, _ = app.Update(cmd())
	app = m.(App)
	if app.connName != "A" || app.engine != engineA {
		t.Fatalf("a cancelled connect's late result must not switch to B; conn=%q engineSwapped=%v",
			app.connName, app.engine != engineA)
	}
	if _, err := app.engine.Query(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("connection A should still be usable after cancelling B: %v", err)
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
	app.connList.cursor = 0
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd != nil {
		t.Fatal("re-selecting the current connection should not reconnect")
	}
	if app.screen != screenTables {
		t.Fatal("re-selecting the current connection should show its table list")
	}
}
