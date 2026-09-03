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

// procEngine wraps a real engine with a process-list query the test database can
// actually answer (SQLite has none), so the `,` dispatch can be driven headlessly.
type procEngine struct {
	db.Engine
	sql string
}

func (e procEngine) ProcessListSQL() string { return e.sql }

// tableListApp opens a one-table sqlite db and leaves the model on the table
// list, which is where `,` lives.
func tableListApp(t *testing.T) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE things (id INTEGER PRIMARY KEY)`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "conn"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
	if app.screen != screenTables {
		t.Fatalf("setup: screen = %d, want the table list", app.screen)
	}
	return app
}

// TestProcessListRuns checks that `,` on the table list runs the engine's
// process-list query and shows it as an ad-hoc (read-only, re-runnable) result.
func TestProcessListRuns(t *testing.T) {
	app := tableListApp(t)
	app.engine = procEngine{Engine: app.engine, sql: "SELECT 1 AS id, 'Query' AS command"}

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(",")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`,` should dispatch the process-list query")
	}
	if !strings.Contains(app.status, "process list") {
		t.Fatalf("status = %q, want it to name the process list", app.status)
	}
	msg := cmd()
	if _, ok := msg.(queryResultMsg); !ok {
		t.Fatalf("want a queryResultMsg, got %T", msg)
	}
	app = update(t, app, msg)

	if app.screen != screenBrowse {
		t.Fatalf("screen = %d, want the grid", app.screen)
	}
	if !app.p().adHoc || len(app.g().rows) != 1 {
		t.Fatalf("adHoc=%v rows=%d, want an ad-hoc result with 1 row", app.p().adHoc, len(app.g().rows))
	}
	// `r` must re-run it, not reload a table (there is none behind this view).
	if app.p().adHocQuery != "SELECT 1 AS id, 'Query' AS command" {
		t.Fatalf("adHocQuery = %q, want the process-list SQL kept for r", app.p().adHocQuery)
	}
}

// TestProcessListUnsupported: SQLite has no server, so `,` reports that and
// stays put rather than running anything.
func TestProcessListUnsupported(t *testing.T) {
	app := tableListApp(t)

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(",")})
	app = m.(App)
	if cmd != nil {
		t.Fatal("sqlite has no process list; nothing should be dispatched")
	}
	if app.screen != screenTables {
		t.Fatalf("screen = %d, want to stay on the table list", app.screen)
	}
	if !strings.Contains(app.status, "no process list") {
		t.Fatalf("status = %q, want the unsupported note", app.status)
	}
	if app.activity != "" {
		t.Fatalf("activity = %q, want no op started", app.activity)
	}
}
