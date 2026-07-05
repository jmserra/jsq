package tui

import (
	"context"
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
	if err == nil || !strings.Contains(err.Error(), "exited before") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want exited-with-output error, got %v", err)
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

// TestRunFlowConnects drives the whole run → wait_port → connect flow through one
// connectCmd: it starts the `run` helper, waits for the (already-open) port, and
// opens the engine, all before returning connectedMsg.
func TestRunFlowConnects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	e.Close()

	// A listener stands in for the port the `run` helper would open.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer KillRunHelpers() // reap the sleep helper this test starts

	app := New(nil, config.Conn{
		URL:      path,
		Name:     "kube",
		Run:      "sleep 30",
		WaitPort: ln.Addr().String(),
	})

	msg := app.Init()()
	if _, ok := msg.(connectedMsg); !ok {
		t.Fatalf("expected connectedMsg, got %T (%+v)", msg, msg)
	}
	app = update(t, app, msg)
	if app.engine == nil {
		t.Fatal("engine not set after connect")
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

func TestWaitAddr(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"5432":           "127.0.0.1:5432",
		"db.internal:80": "db.internal:80",
	}
	for in, want := range cases {
		if got := waitAddr(in); got != want {
			t.Fatalf("waitAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
