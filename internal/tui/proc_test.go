package tui

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

func TestWaitPortReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := waitPort(ln.Addr().String(), nil, 3*time.Second); err != nil {
		t.Fatalf("open port should be ready: %v", err)
	}
}

func TestWaitPortTimeout(t *testing.T) {
	// Bind then close to get an address nothing is listening on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	err := waitPort(addr, nil, 1500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestWaitPortProcExits(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	p, err := startRun("echo boom >&2; exit 1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.kill() // deregister from the live-helper set
	err = waitPort(addr, p, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "failed before") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want crash-with-output error, got %v", err)
	}
}

// TestWaitPortDetachedCmdOK covers the `docker compose up -d` shape: the helper
// exits cleanly (0) while the service comes up, so the port wait must succeed
// rather than treating the exit as a failure.
func TestWaitPortDetachedCmdOK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	p, err := startRun("true") // exits 0 immediately, like a detached launcher
	if err != nil {
		t.Fatal(err)
	}
	defer p.kill()
	if err := waitPort(ln.Addr().String(), p, 3*time.Second); err != nil {
		t.Fatalf("clean exit + open port should succeed, got %v", err)
	}
}

func TestRunProcKill(t *testing.T) {
	p, err := startRun("sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if p.dead() {
		t.Fatal("process should still be running")
	}
	p.kill()
	if !p.dead() {
		t.Fatal("process should be dead after kill")
	}
	p.kill() // second kill is a no-op, must not hang or panic
	(*runProc)(nil).kill()
}

// TestRunFlowConnects drives the whole cmd → connect flow through one connectCmd:
// it starts the `cmd` helper and opens the engine before returning connectedMsg.
// (A SQLite URL has no host:port, so no wait happens — that path is covered by
// TestWaitPort* and db.TestHostPort.)
func TestRunFlowConnects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	e.Close()

	defer KillRunHelpers() // reap the sleep helper this test starts

	app := New(nil, config.Conn{URL: path, Name: "kube", Cmd: "sleep 30"})
	if app.connCmd == "" {
		t.Fatal("waiting view should be armed for a cmd-backed connection")
	}

	// connectCmd starts the helper and opens the engine (SQLite → no port wait).
	msg := connectCmd(app.pending)()
	if _, ok := msg.(connectedMsg); !ok {
		t.Fatalf("expected connectedMsg, got %T (%+v)", msg, msg)
	}
	app = update(t, app, msg)
	if app.engine == nil {
		t.Fatal("engine not set after connect")
	}
	if app.connCmd != "" {
		t.Fatal("waiting view should be dismissed after connect")
	}
}

// TestConnectingView checks the loader names the cmd we ran and the port we wait
// for, without executing connectCmd (so no real process is spawned).
func TestConnectingView(t *testing.T) {
	app := New(nil, config.Conn{
		Name: "kube",
		URL:  "postgres://u@localhost:5432/app",
		Cmd:  "kubectl port-forward svc/db 5432:5432",
	})
	app.w, app.h = 80, 24
	v := app.View()
	for _, want := range []string{"kube", "kubectl port-forward svc/db 5432:5432", "localhost:5432", "running", "waiting"} {
		if !strings.Contains(v, want) {
			t.Fatalf("connecting view missing %q:\n%s", want, v)
		}
	}
	// A cmd-less connection shows no loader.
	plain := New(nil, config.Conn{Name: "local", URL: "sqlite://./x.db"})
	if plain.connCmd != "" {
		t.Fatal("cmd-less connection should not arm the waiting view")
	}
}

// TestConnectErrorQuits verifies a connect failure quits the app carrying the
// error out (main prints it to stderr), rather than showing an in-app screen.
func TestConnectErrorQuits(t *testing.T) {
	app := New(nil, config.Conn{})
	m, cmd := app.Update(connectErrMsg{err: errors.New("nope")})
	app = m.(App)
	if app.FatalErr() == nil || app.FatalErr().Error() != "nope" {
		t.Fatalf("FatalErr = %v, want \"nope\"", app.FatalErr())
	}
	if cmd == nil {
		t.Fatal("connect error should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("connect error should quit the program")
	}
}

// TestConnectCmdReportsConnectErr checks a failed connect surfaces as
// connectErrMsg (here: the helper exits before its port opens).
func TestConnectCmdReportsConnectErr(t *testing.T) {
	msg := connectCmd(config.Conn{URL: "postgres://u@127.0.0.1:1/app", Cmd: "exit 1"})()
	if _, ok := msg.(connectErrMsg); !ok {
		t.Fatalf("expected connectErrMsg, got %T (%+v)", msg, msg)
	}
}

// TestKillRunHelpers verifies the exit backstop reaps a started helper — this is
// what protects against a port-forward outliving jsq on any quit path.
func TestKillRunHelpers(t *testing.T) {
	p, err := startRun("sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if p.dead() {
		t.Fatal("helper should still be running")
	}
	KillRunHelpers()
	if !p.dead() {
		t.Fatal("KillRunHelpers should have killed the helper")
	}
}

// TestPickerIgnoresDoubleEnter guards the leak where a second Enter on the picker
// (while the first connect is still running in the background) would start a
// duplicate engine and `run` process.
func TestPickerIgnoresDoubleEnter(t *testing.T) {
	conns := []config.Conn{{Name: "c", URL: "sqlite://./x.db"}}
	app := New(conns, config.Conn{}) // empty direct → picker mode

	m, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app = m.(App)
	if cmd == nil {
		t.Fatal("first Enter should dispatch a connect command")
	}
	if _, cmd2 := app.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd2 != nil {
		t.Fatal("second Enter while connecting must be a no-op")
	}
}
