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
// the list, not the grid.
func exportApp(t *testing.T) App {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(t, e, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, note TEXT)`)
	mustExecT(t, e, `INSERT INTO users (name, note) VALUES ('Ada','it''s fine'),('Linus',NULL),('Grace','C:\path')`)
	mustExecT(t, e, `CREATE TABLE logs (id INTEGER PRIMARY KEY, msg TEXT)`)
	mustExecT(t, e, `INSERT INTO logs (msg) VALUES ('one'),('two')`)
	e.Close()

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
func runExportTo(t *testing.T, app App, out string, keys string) (App, string) {
	t.Helper()
	app = runeKey(t, app, 'x')
	if app.screen != screenExport {
		t.Fatalf("`x` on the table list should open the export screen, got screen %d", app.screen)
	}
	app = typeRunes(t, app, keys)

	// Name the output file: `o`, clear the generated name, type ours.
	app = runeKey(t, app, 'o')
	for range app.export.opts.path.val {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, out)
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter}) // accept the path

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter}) // export
	app = m.(App)
	if cmd == nil {
		t.Fatal("Enter on the export screen returned no command")
	}
	app, _ = drainExport(t, app, cmd)

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	return app, string(body)
}

// TestExportWritesSelectedTables is the whole feature end to end: `e`, check two
// tables with <space>, Enter, and the file holds replayable INSERTs for both.
func TestExportWritesSelectedTables(t *testing.T) {
	app := exportApp(t)
	out := filepath.Join(t.TempDir(), "dump.sql")

	// The list is alphabetical (logs, users): check the first, move down, check
	// the second.
	app, body := runExportTo(t, app, out, " j ")

	for _, want := range []string{
		"PRAGMA foreign_keys=OFF;",
		`INSERT INTO "logs" ("id", "msg") VALUES (1, 'one');`,
		`INSERT INTO "users" ("id", "name", "note") VALUES (1, 'Ada', 'it''s fine');`,
		`INSERT INTO "users" ("id", "name", "note") VALUES (2, 'Linus', NULL);`,
		// A backslash survives verbatim: SQLite string literals have no escape
		// character, so doubling it would corrupt the value on reload.
		`(3, 'Grace', 'C:\path');`,
		"COMMIT;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export is missing %q:\n%s", want, body)
		}
	}
	// Structure is off by default — data only, into an existing schema.
	if strings.Contains(body, "CREATE TABLE") || strings.Contains(body, "DROP TABLE") {
		t.Errorf("structure is off but the dump carries DDL:\n%s", body)
	}
	if !strings.Contains(app.status, "wrote dump.sql") {
		t.Errorf("status = %q, want it to name the file", app.status)
	}
	if !strings.Contains(app.status, "5 rows") {
		t.Errorf("status = %q, want the total row count", app.status)
	}
}

// TestExportStructureToggle checks that `S` puts the engine's own DDL in front
// of each table's rows.
func TestExportStructureToggle(t *testing.T) {
	app := exportApp(t)
	out := filepath.Join(t.TempDir(), "dump.sql")
	_, body := runExportTo(t, app, out, " S") // check `logs`, turn structure on

	if !strings.Contains(body, `DROP TABLE IF EXISTS "logs";`) {
		t.Errorf("no DROP TABLE:\n%s", body)
	}
	if !strings.Contains(body, "CREATE TABLE logs") {
		t.Errorf("no CREATE TABLE:\n%s", body)
	}
	// The DDL must precede the rows, or the reload inserts into a dropped table.
	if strings.Index(body, "CREATE TABLE logs") > strings.Index(body, `INSERT INTO "logs"`) {
		t.Errorf("CREATE TABLE comes after the INSERTs:\n%s", body)
	}
}

// TestExportRowLimit checks the cap takes the NEWEST rows (primary key
// descending) and still writes them oldest-first, so a replay inserts in
// insertion order.
func TestExportRowLimit(t *testing.T) {
	app := exportApp(t)
	out := filepath.Join(t.TempDir(), "dump.sql")

	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace}) // check `logs`
	app = runeKey(t, app, 'j')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace}) // check `users`
	app = runeKey(t, app, 'n')                           // the row cap
	app = typeRunes(t, app, "2")
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})

	if got := app.export.opts.limit(); got != 2 {
		t.Fatalf("limit = %d, want 2", got)
	}
	// The generated name follows the cap.
	if !strings.Contains(app.export.opts.path.val, "_last2_") {
		t.Errorf("generated path %q does not record the cap", app.export.opts.path.val)
	}

	app = runeKey(t, app, 'o')
	for range app.export.opts.path.val {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, out)
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	app, _ = drainExport(t, app, cmd)

	raw, err := os.ReadFile(out)
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

// TestExportView checks the screen renders its check boxes and settings.
func TestExportView(t *testing.T) {
	app := exportApp(t)
	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})

	view := lipgloss.NewStyle().Render(app.View())
	for _, want := range []string{"[x] logs", "[ ] users", "1 selected", "rows: all", "structure: no", ".sql"} {
		if !strings.Contains(view, want) {
			t.Errorf("export view missing %q:\n%s", want, view)
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
	out := filepath.Join(t.TempDir(), "dump.sql")
	_, body := runExportTo(t, app, out, " j S") // both tables, with structure

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

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := expandHome("~/dump.sql"); got != filepath.Join(home, "dump.sql") {
		t.Errorf("expandHome(~/dump.sql) = %q", got)
	}
	// Only a leading ~/ is a home reference; anything else is a real name.
	for _, p := range []string{"./dump.sql", "/tmp/dump.sql", "~weird/dump.sql"} {
		if got := expandHome(p); got != p {
			t.Errorf("expandHome(%q) = %q, want it untouched", p, got)
		}
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

// startExport drives the screen up to a dispatched export of everything, writing
// to out, and returns the still-running job's batch command.
func startExport(t *testing.T, app App, out string) (App, tea.Cmd) {
	t.Helper()
	app = runeKey(t, app, 'x')
	app = runeKey(t, app, 'a') // check every table
	app = runeKey(t, app, 'o')
	for range app.export.opts.path.val {
		app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	app = typeRunes(t, app, out)
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEnter})

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return m.(App), cmd
}

// TestExportSurvivesNavigation is the point of the job slot: an export must not
// be cancelled by whatever you do next. Opening a table takes the op slot, which
// would cancel any ordinary DB command — the export has to be untouched.
func TestExportSurvivesNavigation(t *testing.T) {
	app := exportApp(t)
	out := filepath.Join(t.TempDir(), "dump.sql")
	app, cmd := startExport(t, app, out)

	if app.exportJob == nil {
		t.Fatal("no export job was recorded")
	}
	job := app.exportJob.id

	// Walk away: back to the table list, then open a table (a real op).
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatalf("Backspace should return to the table list, got %d", app.screen)
	}
	m, loadCmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if app.exportJob == nil || app.exportJob.id != job {
		t.Fatal("opening a table cancelled the running export")
	}
	if app.activity == "" {
		t.Fatal("the table load should have taken the op slot")
	}
	app = update(t, app, loadCmd()) // let the load land

	app, _ = drainExport(t, app, cmd)
	if app.exportJob != nil {
		t.Error("the job should be cleared once it finishes")
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the export did not survive: %v", err)
	}
	if !strings.Contains(app.status, "wrote dump.sql") {
		t.Errorf("status = %q", app.status)
	}
}

// TestExportProgress checks that a long export reports as it goes, rather than
// going silent until it lands.
func TestExportProgress(t *testing.T) {
	old := exportProgressEvery
	exportProgressEvery = 10
	defer func() { exportProgressEvery = old }()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(t, e, `CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExecT(t, e, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 60)
	                 INSERT INTO big (v) SELECT 'row ' || i FROM n`)
	e.Close()

	app := New(nil, config.Conn{URL: path, Name: "test"})
	app = update(t, app, app.Init()())
	app = update(t, app, tea.WindowSizeMsg{Width: 100, Height: 24})

	out := filepath.Join(t.TempDir(), "dump.sql")
	app, cmd := startExport(t, app, out)
	app, seen := drainExport(t, app, cmd)

	if len(seen) < 2 {
		t.Fatalf("got %d progress messages for a 60-row table, want several", len(seen))
	}
	last := seen[len(seen)-1]
	if last.table != "big" || last.total != 1 {
		t.Errorf("progress names the wrong table: %+v", last)
	}
	// Progress must actually advance, not just repeat the starting state.
	if last.rows == 0 {
		t.Errorf("progress never reported any rows: %+v", seen)
	}
	if !strings.Contains(app.status, "60 rows") {
		t.Errorf("final status = %q", app.status)
	}
}

// TestExportHeaderShowsJob checks the running export is visible from any screen —
// it outlives the screen that started it, so the header is the only thing that
// can say it is still going.
func TestExportHeaderShowsJob(t *testing.T) {
	app := exportApp(t)
	app.exportJob = &exportJob{id: 1, cancel: func() {}, table: "entries", index: 2, total: 7, rows: 12345}
	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace}) // leave the export screen

	view := lipgloss.NewStyle().Render(app.View())
	for _, want := range []string{"export 2/7 entries", "12.3k rows"} {
		if !strings.Contains(view, want) {
			t.Errorf("header missing %q:\n%s", want, view)
		}
	}
}

// TestExportCancelOnlyFromItsScreen checks that Esc stops the job where the
// footer promises it does — and nowhere else, so a stray Esc elsewhere can't
// kill a long export.
func TestExportCancelOnlyFromItsScreen(t *testing.T) {
	app := exportApp(t)
	cancelled := false
	app.exportJob = &exportJob{id: 1, cancel: func() { cancelled = true }, total: 1}

	// On the table list, Esc must leave it alone.
	app.screen = screenTables
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if cancelled {
		t.Fatal("Esc on the table list cancelled the background export")
	}

	// On the export screen it cancels.
	app.screen = screenExport
	app = update(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled {
		t.Error("Esc on the export screen did not cancel the export")
	}
	if !strings.Contains(app.status, "cancelling") {
		t.Errorf("status = %q", app.status)
	}
}

// TestExportRefusesSecond keeps it to one job at a time.
func TestExportRefusesSecond(t *testing.T) {
	app := exportApp(t)
	app.exportJob = &exportJob{id: 1, cancel: func() {}, total: 1}
	app = runeKey(t, app, 'x')
	app = update(t, app, tea.KeyMsg{Type: tea.KeySpace})
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd != nil {
		t.Fatal("a second export should not dispatch while one runs")
	}
	if !strings.Contains(app.status, "already running") {
		t.Errorf("status = %q", app.status)
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
