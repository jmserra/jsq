package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

// exportApp drives the model to the table list of a two-table database, ready
// for `x`. It stops short of loading a table — the export screen is reached from
// the list, not the grid. Exports land in a temp dir (exportDir).
func exportApp(t *testing.T) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(t, e, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, note TEXT)`)
	mustExecT(t, e, `CREATE INDEX users_name ON users (name)`)
	mustExecT(t, e, `INSERT INTO users (name, note) VALUES ('Ada','it''s fine'),('Linus',NULL),('Grace','C:\path')`)
	mustExecT(t, e, `CREATE TABLE logs (id INTEGER PRIMARY KEY, msg TEXT)`)
	mustExecT(t, e, `INSERT INTO logs (msg) VALUES ('one'),('two')`)
	e.Close()

	old := exportDir
	exportDir = t.TempDir()
	t.Cleanup(func() { exportDir = old })

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	return update(t, app, tea.WindowSizeMsg{Width: 80, Height: 24})
}

func mustExecT(t *testing.T, e db.Engine, q string) {
	t.Helper()
	if _, err := e.Exec(context.Background(), q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// runExportTo drives the export screen to completion and returns the file it
// wrote. keys are pressed on the check list before Enter.
func runExportTo(t *testing.T, app App, keys string) (App, string) {
	t.Helper()
	app = runeKey(t, app, 'x')
	if app.screen != screenExport {
		t.Fatalf("`x` on the table list should open the export screen, got screen %d", app.screen)
	}
	app = typeRunes(t, app, keys)

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter}) // export
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter on the export screen returned no command")
	}
	path := app.exportJob.path
	app, _ = drainExport(t, app, cmd)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	return app, string(body)
}

// TestExportWritesSelectedTables is the whole feature end to end: `x`, check two
// tables with <space>, Enter, and a generated file in exportDir holds the whole
// of both — recreated, then filled.
func TestExportWritesSelectedTables(t *testing.T) {
	app := exportApp(t)

	// The list is alphabetical (logs, users): check the first, move down, check
	// the second.
	app, body := runExportTo(t, app, " j ")

	for _, want := range []string{
		"PRAGMA foreign_keys=OFF;",
		`DROP TABLE IF EXISTS "logs";`,
		"CREATE TABLE logs",
		`INSERT INTO "logs" ("id", "msg") VALUES (1, 'one');`,
		`INSERT INTO "users" ("id", "name", "note") VALUES (1, 'Ada', 'it''s fine');`,
		`INSERT INTO "users" ("id", "name", "note") VALUES (2, 'Linus', NULL);`,
		// A backslash survives verbatim: SQLite string literals have no escape
		// character, so doubling it would corrupt the value on reload.
		`(3, 'Grace', 'C:\path');`,
		// The index is its own sqlite_master entry, which the DROP took with it.
		"CREATE INDEX users_name ON users (name);",
		"COMMIT;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export is missing %q:\n%s", want, body)
		}
	}
	// The DDL must precede the rows, or the reload inserts into a dropped table.
	if strings.Index(body, "CREATE TABLE logs") > strings.Index(body, `INSERT INTO "logs"`) {
		t.Errorf("CREATE TABLE comes after the INSERTs:\n%s", body)
	}
	// A full dump lands in a table it just created: plain INSERTs.
	if strings.Contains(body, "ON CONFLICT") {
		t.Errorf("a full dump should not upsert:\n%s", body)
	}
	files, _ := filepath.Glob(filepath.Join(exportDir, "test_logs_users_*.sql"))
	if len(files) != 1 {
		t.Errorf("want one generated file named for the connection and tables, got %v", files)
	}
	if !strings.Contains(app.status, "wrote test_logs_users_") {
		t.Errorf("status = %q, want it to name the file", app.status)
	}
	if !strings.Contains(app.status, "5 rows") {
		t.Errorf("status = %q, want the total row count", app.status)
	}
}

// capRows sets the row cap from the export screen.
func capRows(t *testing.T, app App, n string) App {
	t.Helper()
	app = runeKey(t, app, 'n')
	app = typeRunes(t, app, n)
	return update(t, app, tea.KeyMsg{Type: tea.KeyEnter})
}

// TestExportRowLimit checks the cap takes the NEWEST rows (primary key
// descending), still writes them oldest-first, and writes them as upserts with
// no DDL — a partial dump goes over the live table, it does not replace it.
func TestExportRowLimit(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace}) // check `logs`
	app = runeKey(t, app, 'j')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace}) // check `users`
	app = capRows(t, app, "2")
	if got := app.export.opts.limit(); got != 2 {
		t.Fatalf("limit = %d, want 2", got)
	}

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	path := app.exportJob.path
	// The generated name records the cap.
	if !strings.Contains(filepath.Base(path), "_last2_") {
		t.Errorf("generated path %q does not record the cap", path)
	}
	app, _ = drainExport(t, app, cmd)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	// users has three rows; the newest two are Linus (2) and Grace (3), written
	// in that order — not Ada.
	if strings.Contains(body, "'Ada'") {
		t.Errorf("the oldest row should have been left out:\n%s", body)
	}
	linus, grace := strings.Index(body, "'Linus'"), strings.Index(body, "'Grace'")
	if linus < 0 || grace < 0 {
		t.Fatalf("both newest rows should be present:\n%s", body)
	}
	if linus > grace {
		t.Errorf("rows were written newest-first; a replay needs insertion order:\n%s", body)
	}
	if !strings.Contains(body, "newest 2 by id desc") {
		t.Errorf("the dump does not say it is partial:\n%s", body)
	}
	if strings.Contains(body, "DROP TABLE") || strings.Contains(body, "CREATE TABLE") {
		t.Errorf("a partial dump must carry no DDL:\n%s", body)
	}
	if !strings.Contains(body, `ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name", "note" = EXCLUDED."note";`) {
		t.Errorf("a partial dump should upsert on the primary key:\n%s", body)
	}
}

// TestExportPartialReruns replays a partial dump over the very database it came
// from — twice — and checks it overwrote rather than failed or duplicated: the
// point of writing upserts.
func TestExportPartialReruns(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = runeKey(t, app, 'j')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace}) // check `users`
	app = capRows(t, app, "2")
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	path := app.exportJob.path
	drainExport(t, app, cmd)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	e := app.engine
	mustExecT(t, e, `UPDATE users SET note = 'changed' WHERE id = 3`)
	for i := 0; i < 2; i++ {
		if _, err := e.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("replay %d over the live table: %v\n%s", i+1, err, raw)
		}
	}
	rs, err := e.Query(ctx, `SELECT id, note FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 3 {
		t.Fatalf("users has %d rows after two replays, want 3", len(rs.Rows))
	}
	if rs.Rows[2][1] != `C:\path` {
		t.Errorf("the replay did not put the dumped value back: %#v", rs.Rows[2][1])
	}
}

// TestExportFilterAndCheckAll checks that `a` marks what the filter is showing —
// the reason the export screen reuses the filtering list at all.
func TestExportFilterAndCheckAll(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = runeKey(t, app, '/')
	app = typeRunes(t, app, "log")
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter}) // commit the filter, do NOT export
	if app.screen != screenExport {
		t.Fatal("Enter out of filter mode must not leave the screen")
	}
	app = runeKey(t, app, 'a')

	got := app.export.list.checkedTables()
	if len(got) != 1 || got[0].Name != "logs" {
		t.Fatalf("`a` under a filter checked %v, want just logs", got)
	}
}

// TestExportNothingChecked refuses to write an empty dump and says why.
func TestExportNothingChecked(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd != nil {
		t.Fatal("Enter with nothing checked should not dispatch an export")
	}
	if !strings.Contains(app.status, "nothing checked") {
		t.Errorf("status = %q", app.status)
	}
}

// TestExportBackspaceReturns confirms the screen sits on the navigation chain:
// Backspace steps back to the table list, keeping the checks for a second run.
func TestExportBackspaceReturns(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatalf("Backspace should return to the table list, got screen %d", app.screen)
	}
	app = runeKey(t, app, 'x')
	if len(app.export.list.checkedTables()) != 1 {
		t.Error("reopening the export screen lost the selection")
	}
}

// TestExportView checks the screen renders its check boxes, the mode the cap
// implies, and the file it will write.
func TestExportView(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})

	view := lipgloss.NewStyle().Render(app.View())
	for _, want := range []string{"[x] logs", "[ ] users", "1 selected", "rows: all · drop + create", "test_logs_", ".sql"} {
		if !strings.Contains(view, want) {
			t.Errorf("export view missing %q:\n%s", want, view)
		}
	}
	app = capRows(t, app, "50")
	view = lipgloss.NewStyle().Render(app.View())
	for _, want := range []string{"newest 50 · upsert", "_last50_"} {
		if !strings.Contains(view, want) {
			t.Errorf("capped export view missing %q:\n%s", want, view)
		}
	}
}

// TestExportCancelledLeavesNoFile checks the .part/rename dance: a failed export
// must not leave a truncated file that looks finished.
func TestExportCancelledLeavesNoFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	mustExecT(t, e, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)

	out := filepath.Join(t.TempDir(), "dump.sql")
	req := exportReq{path: out, tables: []db.Table{{Name: "nosuchtable"}}, conn: "test", dbName: "t"}
	if _, err := writeDump(ctx, e, req, nil); err == nil {
		t.Fatal("exporting a missing table should fail")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a failed export left %s behind", out)
	}
	parts, _ := filepath.Glob(filepath.Join(filepath.Dir(out), "*.part"))
	if len(parts) != 0 {
		t.Errorf("a failed export left partial files behind: %v", parts)
	}
}

// TestExportReplays reloads a dump into a fresh database — the only test that
// proves the file is actually replayable rather than merely plausible.
func TestExportReplays(t *testing.T) {
	app := exportApp(t)
	_, body := runExportTo(t, app, " j ") // both tables, whole

	ctx := context.Background()
	fresh, err := db.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := fresh.Exec(ctx, body); err != nil {
		t.Fatalf("replaying the dump: %v\n%s", err, body)
	}

	rs, err := fresh.Query(ctx, `SELECT name, note FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 3 {
		t.Fatalf("restored %d users, want 3", len(rs.Rows))
	}
	if rs.Rows[0][1] != "it's fine" {
		t.Errorf("quote escaping did not survive the round trip: %#v", rs.Rows[0][1])
	}
	if rs.Rows[1][1] != nil {
		t.Errorf("NULL did not survive the round trip: %#v", rs.Rows[1][1])
	}
	if rs.Rows[2][1] != `C:\path` {
		t.Errorf("backslash did not survive the round trip: %#v", rs.Rows[2][1])
	}
}

// drainExport runs an export job to completion the way the runtime does: the
// batch's first command writes the file, the second waits for the job's next
// message, and each message's handler hands back the next waiter. It returns the
// finished App and every progress message seen.
func drainExport(t *testing.T, app App, cmd tea.Cmd) (App, []exportProgressMsg) {
	t.Helper()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("export should dispatch a run+wait batch, got %T", cmd())
	}
	go batch[0]() // the writer
	next := batch[1]

	var seen []exportProgressMsg
	for i := 0; i < 10000; i++ {
		msg := next()
		if p, ok := msg.(exportProgressMsg); ok {
			seen = append(seen, p)
		}
		m, c := app.Update(msg)
		app = m.(App)
		if done, ok := msg.(exportDoneMsg); ok {
			if done.err != nil && !errors.Is(done.err, context.Canceled) {
				t.Fatalf("export failed: %v", done.err)
			}
			return app, seen
		}
		if c == nil {
			t.Fatal("a progress message must re-arm the waiter")
		}
		next = c
	}
	t.Fatal("export never finished")
	return app, seen
}

// startExport drives the screen up to a dispatched export of everything and
// returns the still-running job's batch command.
func startExport(t *testing.T, app App) (App, tea.Cmd) {
	t.Helper()
	app = runeKey(t, app, 'x')
	app = runeKey(t, app, 'a') // check every table
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return m.(App), cmd
}

// TestExportBlocksUI checks that a running export owns the screen: its loader
// replaces the list, every key but Esc is swallowed, and Esc cancels it.
func TestExportBlocksUI(t *testing.T) {
	app := exportApp(t)
	cancelled := false
	app.screen = screenExport
	app.exportJob = &exportJob{id: 1, cancel: func() { cancelled = true }, path: "./x.sql",
		table: "entries", index: 2, total: 7, rows: 12345}

	view := lipgloss.NewStyle().Render(app.View())
	for _, want := range []string{"exporting to ./x.sql", "2/7 entries", "12.3k rows", "esc to cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("loader missing %q:\n%s", want, view)
		}
	}

	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyBackspace}, {Type: tea.KeyEnter}, {Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune("a")}, {Type: tea.KeyRunes, Runes: []rune("?")},
	} {
		m, cmd := app.Update(k)
		app = m.(App)
		if cmd != nil || app.screen != screenExport || app.help.active {
			t.Fatalf("%q reached the UI behind a running export", k.String())
		}
	}
	if len(app.export.list.checkedTables()) != 0 {
		t.Error("a key behind the loader changed the selection")
	}
	if cancelled {
		t.Fatal("only Esc may cancel the export")
	}

	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled {
		t.Error("Esc did not cancel the export")
	}
	if view := app.View(); !strings.Contains(view, "cancelling") {
		t.Errorf("the loader should say it is cancelling:\n%s", view)
	}
}

// TestExportCancelReports runs a real export, cancels it, and checks the job
// clears with a status rather than vanishing — and leaves no file.
func TestExportCancelReports(t *testing.T) {
	app := exportApp(t)
	app, cmd := startExport(t, app)
	path := app.exportJob.path
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc}) // cancel before it runs
	app, _ = drainExport(t, app, cmd)
	if app.exportJob != nil {
		t.Fatal("the job should be cleared once the cancel lands")
	}
	if app.status != "export cancelled" {
		t.Errorf("status = %q", app.status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a cancelled export left %s behind", path)
	}
	if !strings.Contains(app.View(), "export cancelled") {
		t.Errorf("the outcome screen should say it was cancelled:\n%s", app.View())
	}
	// A cancel goes back to the selection, intact, so a retry is one Enter.
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})
	if app.screen != screenExport || len(app.export.list.checkedTables()) != 2 {
		t.Errorf("after a cancel: screen %d, %d checked; want the export screen, selection kept",
			app.screen, len(app.export.list.checkedTables()))
	}
}

// TestCleanupExports is the Ctrl-C backstop: a temp file registered by a running
// export is removed on the way out, since the goroutine's own defer never runs.
func TestCleanupExports(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "dump.sql.1234.part")
	if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	trackExportTemp(tmp)
	CleanupExports()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("CleanupExports left %s behind", tmp)
	}
}

// TestExportOutcomeScreen checks the loader turns into a result screen that
// waits for a key, and that dismissing a success lands on the table list with
// the selection cleared.
func TestExportOutcomeScreen(t *testing.T) {
	app := exportApp(t)
	app, _ = runExportTo(t, app, " j ")
	if app.exportDone == nil {
		t.Fatal("a finished export should hold its outcome on screen")
	}
	view := app.View()
	for _, want := range []string{"export complete", "test_logs_users_", "size", " B", "tables   2", "rows     5", "any key"} {
		if !strings.Contains(view, want) {
			t.Errorf("outcome screen missing %q:\n%s", want, view)
		}
	}
	app = runeKey(t, app, 'q')
	if app.exportDone != nil || app.screen != screenTables {
		t.Fatalf("a key should dismiss to the table list, got screen %d", app.screen)
	}
	app = runeKey(t, app, 'x')
	if n := len(app.export.list.checkedTables()); n != 0 {
		t.Errorf("the selection should be cleared after a successful export, %d still checked", n)
	}
}
