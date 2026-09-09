package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/db"
)

// userEngine wraps a real engine with users and a grants query the test database
// can actually answer (SQLite has neither), so the `u` flow can be driven
// headlessly. The grants query binds its argument, like the real ones do — that
// is what proves the args survive into the pane and back out again on `r`.
type userEngine struct {
	db.Engine
	users []db.User
}

func (e userEngine) Users(context.Context) ([]db.User, error) { return e.users, nil }

func (e userEngine) GrantsSQL(_ context.Context, u db.User) (string, []any, error) {
	return `SELECT 'global' AS scope, ? AS object, 'SELECT' AS privilege, 'NO' AS grantable`,
		[]any{u.Label()}, nil
}

// usersApp puts the model on the table list with an engine that has two users.
func usersApp(t *testing.T) App {
	t.Helper()
	app := tableListApp(t)
	app.engine = userEngine{Engine: app.engine, users: []db.User{
		{Name: "alice", Host: "%"},
		{Name: "bob", Host: "localhost"},
	}}
	return app
}

// openUserList presses `u` and folds in the usersMsg it dispatches.
func openUserList(t *testing.T, app App) App {
	t.Helper()
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`u` should dispatch the user list")
	}
	app = update(t, app, cmd())
	if app.screen != screenUsers {
		t.Fatalf("screen = %d, want the user list", app.screen)
	}
	return app
}

// TestUserListOpens checks that `u` lists the server's users, labelled the way
// the engine renders them (user@host on MySQL), and that Backspace steps back to
// the table list like every other list screen.
func TestUserListOpens(t *testing.T) {
	app := openUserList(t, usersApp(t))

	view := app.View()
	for _, want := range []string{"alice@%", "bob@localhost"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}

	app = update(t, app, tea.KeyMsg{Type: tea.KeyBackspace})
	if app.screen != screenTables {
		t.Fatalf("backspace → screen %d, want the table list", app.screen)
	}
}

// TestUserGrantsOpen drives Enter on a user: the privileges land in the grid as a
// read-only ad-hoc result, captioned with whose they are, with the bind args kept
// so `r` can re-run the same query.
func TestUserGrantsOpen(t *testing.T) {
	app := openUserList(t, usersApp(t))

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("enter on a user should dispatch its grants")
	}
	msg := cmd()
	res, ok := msg.(queryResultMsg)
	if !ok {
		t.Fatalf("want a queryResultMsg, got %T (%v)", msg, msg)
	}
	app = update(t, app, res)

	if app.screen != screenBrowse {
		t.Fatalf("screen = %d, want the grid", app.screen)
	}
	if !app.p().adHoc {
		t.Error("grants should show as an ad-hoc result (read-only, not a table)")
	}
	if !strings.Contains(app.status, "grants for alice@%") {
		t.Errorf("status = %q, want it to name the user", app.status)
	}
	// The bound argument is the user, and it has to ride along in the pane: a
	// re-run with no args would fail on the unbound placeholder.
	if got := app.p().adHocArgs; len(got) != 1 || got[0] != "alice@%" {
		t.Errorf("adHocArgs = %v, want [alice@%%]", got)
	}
	if !strings.Contains(app.View(), "alice@%") {
		t.Errorf("the grid should show the grant rows:\n%s", app.View())
	}

	// `r` re-runs it with the same SQL and args.
	m, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`r` should re-run the grants query")
	}
	if _, ok := cmd().(queryResultMsg); !ok {
		t.Error("the re-run should come back with rows, not an error")
	}
}

// TestUserListFromGrid checks `u` also works from the grid, like `d` does.
func TestUserListFromGrid(t *testing.T) {
	app := usersApp(t)
	app.screen = screenBrowse
	app = openUserList(t, app)
	if app.screen != screenUsers {
		t.Fatalf("screen = %d, want the user list", app.screen)
	}
}

// TestNoUsersOnSQLite checks the engine-has-no-users path: `u` reports it and
// leaves the screen alone rather than showing an empty list.
func TestNoUsersOnSQLite(t *testing.T) {
	app := tableListApp(t) // the real sqlite engine: Users returns nil
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	app = m.(App)
	if cmd == nil {
		t.Fatal("`u` should still dispatch (the engine answers, not the key)")
	}
	app = update(t, app, cmd())
	if app.screen != screenTables {
		t.Fatalf("screen = %d, want to stay on the table list", app.screen)
	}
	if !strings.Contains(app.status, "no users") {
		t.Errorf("status = %q, want it to report there are none", app.status)
	}
}

// TestUserListFilter checks the `/` filter narrows the user list and Enter opens
// the match — the same two-phase filter every list screen has.
func TestUserListFilter(t *testing.T) {
	app := openUserList(t, usersApp(t))

	app = runeKey(t, app, '/')
	if !app.users.filtering {
		t.Fatal("`/` should start the filter")
	}
	for _, r := range "bob" {
		app = runeKey(t, app, r)
	}
	if !strings.Contains(app.View(), "bob@localhost") || strings.Contains(app.View(), "alice@%") {
		t.Fatalf("the filter should leave only bob:\n%s", app.View())
	}

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("enter from filter mode should open the match")
	}
	if app.users.filtering {
		t.Error("enter should commit the filter and leave nav mode")
	}
	if !strings.Contains(app.status, "bob@localhost") {
		t.Errorf("status = %q, want the filtered user's grants", app.status)
	}
}

// TestCreateUserSeed checks what `o` puts in the editor: the engine's template,
// headed by where it will run, with the cursor sitting on the placeholder name so
// the first thing typed replaces it.
func TestCreateUserSeed(t *testing.T) {
	app := openUserList(t, usersApp(t))
	app.connName, app.dbName = "prod", "shop"

	seed := buildCreateUserStmt(seedEngine{app.engine}, app.connName, app.dbName)
	if !strings.Contains(seed.sql, "-- new user on prod · shop") {
		t.Errorf("seed should say where it runs:\n%s", seed.sql)
	}
	// A direct DSN has no connection name: the header must not read "on  · shop".
	bare := buildCreateUserStmt(seedEngine{app.engine}, "", "shop")
	if !strings.Contains(bare.sql, "-- new user on shop") {
		t.Errorf("unnamed connection should drop cleanly out of the header:\n%s", bare.sql)
	}
	if !strings.Contains(seed.sql, "CREATE USER 'newuser'") {
		t.Errorf("seed should carry the engine's template:\n%s", seed.sql)
	}
	if !seed.users {
		t.Error("the seed must be marked as a user-management write")
	}
	if seed.kind != selectWord {
		t.Errorf("kind = %v, want the placeholder pre-selected", seed.kind)
	}
	// line/col must actually point at the placeholder, whatever the header length.
	lines := strings.Split(seed.sql, "\n")
	if seed.line < 1 || seed.line > len(lines) {
		t.Fatalf("line %d is outside the seed", seed.line)
	}
	if got := lines[seed.line-1]; !strings.HasPrefix(got[seed.col-1:], newUserName) {
		t.Errorf("cursor lands on %q, want the placeholder name", got[seed.col-1:])
	}
}

// seedEngine gives the sqlite test engine a MySQL-shaped CREATE USER template
// (sqlite has none), so the seed can be built without a live server.
type seedEngine struct{ db.Engine }

func (seedEngine) CreateUserSQL(name, dbName string) string {
	return "CREATE USER '" + name + "'@'%' IDENTIFIED BY 'change-me';\n"
}

// TestCreateUserRefreshesList drives what happens after the editor: the write
// runs, and because it is marked as user management the USER LIST is what
// reloads — not the pane's table. The statement here is an ordinary sqlite write
// (the test engine has no accounts); it is the marker, not the SQL, that routes.
func TestCreateUserRefreshesList(t *testing.T) {
	app := openUserList(t, usersApp(t))

	m, cmd := app.Update(editorSubmitMsg{sql: "CREATE TABLE created (id INTEGER)", users: true})
	app = m.(App)
	if cmd == nil {
		t.Fatal("a submitted user write should run")
	}
	done, ok := cmd().(execDoneMsg)
	if !ok {
		t.Fatalf("want an execDoneMsg, got %T", cmd())
	}
	if !done.users {
		t.Fatal("the user-management marker must survive into the result")
	}

	m, cmd = app.Update(done)
	app = m.(App)
	if cmd == nil {
		t.Fatal("the write should be followed by a user-list reload")
	}
	app = update(t, app, cmd())
	if app.screen != screenUsers {
		t.Fatalf("screen = %d, want to land back on the user list", app.screen)
	}
	if !strings.Contains(app.status, "user list reloaded") {
		t.Errorf("status = %q, want it to report the reload", app.status)
	}
}

// TestCreateUserSafeConfirms checks a create on a safe (production) connection
// goes through the same y/n gate as every other write: nothing runs until `y`.
func TestCreateUserSafeConfirms(t *testing.T) {
	app := openUserList(t, usersApp(t))
	app.safe, app.connName = true, "prod"

	sql := "CREATE USER 'newuser'@'%' IDENTIFIED BY 'change-me';\nGRANT SELECT ON `shop`.* TO 'newuser'@'%';"
	m, cmd := app.Update(editorSubmitMsg{sql: sql, users: true})
	app = m.(App)
	if cmd != nil {
		t.Fatal("a safe connection must not run the statement before confirmation")
	}
	if !app.confirm.active {
		t.Fatal("safe mode should open the confirmation overlay")
	}
	// The whole statement is previewed, both lines of it — a create is a CREATE
	// plus a GRANT, and confirming only the half you can see is worthless.
	view := app.View()
	for _, want := range []string{"prod", "CREATE USER 'newuser'", "GRANT SELECT"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm overlay missing %q:\n%s", want, view)
		}
	}
	if _, cmd = app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd == nil {
		t.Fatal("`y` should run the held statement")
	}
}

// TestCreateUserUnsupported checks `o` on an engine with no accounts says so
// rather than opening an empty editor.
func TestCreateUserUnsupported(t *testing.T) {
	app := usersApp(t) // the wrapper adds users, but CreateUserSQL is sqlite's: ""
	app.screen = screenUsers
	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	app = m.(App)
	if cmd != nil {
		t.Fatal("nothing should open when the engine has no user statements")
	}
	if !strings.Contains(app.status, "no users") {
		t.Errorf("status = %q, want it to report there are none", app.status)
	}
}
