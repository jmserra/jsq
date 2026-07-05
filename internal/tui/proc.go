package tui

// A connection may carry a `run` command (config: run = "...") — a helper
// process, typically a port-forward, that jsq starts before connecting, keeps
// running for the whole session, and kills on exit. `wait_port` then holds
// jsq at the door until that process has actually opened its listening socket.
//
// Every started helper is registered in a package-level set the moment it
// launches, so KillRunHelpers (deferred by main) always terminates it however
// the program quits — cleanup never depends on a bubbletea message being
// delivered, which is what makes an early Ctrl-C safe.

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// waitTimeout bounds the wait_port probe: give up (and error) after this long.
const waitTimeout = 30 * time.Second

// liveProcs tracks every started run helper so KillRunHelpers can reap them all
// on exit regardless of how far the connect flow got.
var liveProcs = struct {
	mu sync.Mutex
	m  map[*runProc]struct{}
}{m: map[*runProc]struct{}{}}

// KillRunHelpers terminates every run helper still alive. main defers it so no
// port-forward outlives jsq, whatever quit us.
func KillRunHelpers() {
	liveProcs.mu.Lock()
	procs := make([]*runProc, 0, len(liveProcs.m))
	for p := range liveProcs.m {
		procs = append(procs, p)
	}
	liveProcs.mu.Unlock()
	for _, p := range procs {
		p.kill()
	}
}

// runProc is a live `run` helper process plus a bounded tail of its output (for
// diagnostics when it dies before the port opens).
type runProc struct {
	cmd     *exec.Cmd
	cmdline string
	out     *tailBuffer
	done    chan struct{} // closed once the process exits (goroutine reaped it)
}

// startRun launches cmdline via `sh -c`, detached from our stdin so it can't
// steal the TUI's keystrokes, with stdout+stderr captured to a bounded buffer.
func startRun(cmdline string) (*runProc, error) {
	c := exec.Command("sh", "-c", cmdline)
	buf := &tailBuffer{max: 8 << 10}
	c.Stdin = nil
	c.Stdout = buf
	c.Stderr = buf
	setpgid(c)
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("run %q: %w", cmdline, err)
	}
	p := &runProc{cmd: c, cmdline: cmdline, out: buf, done: make(chan struct{})}
	// Register before anything else can observe p, so the exit backstop owns it
	// from the instant it exists — no window where a Ctrl-C could miss it.
	liveProcs.mu.Lock()
	liveProcs.m[p] = struct{}{}
	liveProcs.mu.Unlock()
	go func() { c.Wait(); close(p.done) }()
	return p, nil
}

// dead reports whether the process has already exited (non-blocking).
func (p *runProc) dead() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// kill deregisters the process, then terminates its whole group and reaps it.
// Safe on a nil receiver, and a no-op once the process has exited — so the
// connect-failure path and KillRunHelpers can both call it without racing.
// Skipping an already-dead process also avoids signalling a recycled PID:
// because output is pipe-captured, done closes only when the whole group is
// gone, so there is nothing left to kill.
func (p *runProc) kill() {
	if p == nil {
		return
	}
	liveProcs.mu.Lock()
	delete(liveProcs.m, p)
	liveProcs.mu.Unlock()
	if p.cmd.Process == nil || p.dead() {
		return
	}
	killGroup(p.cmd)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

// waitPort blocks until addr accepts a TCP connection, or until timeout, probing
// once a second. If the run process dies first, that's reported instead (with its
// captured output). The caller's goroutine is abandoned at process exit, so no
// context plumbing is needed to stop it.
func waitPort(addr string, proc *runProc, timeout time.Duration) error {
	end := time.Now().Add(timeout)
	for {
		if proc != nil && proc.dead() {
			return fmt.Errorf("run %q exited before %s opened:\n%s",
				proc.cmdline, addr, strings.TrimSpace(proc.out.String()))
		}
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("%s not available after %s", addr, timeout)
		}
		time.Sleep(time.Second)
	}
}

// waitAddr normalizes a wait_port value: a bare port gets 127.0.0.1, a value
// with a colon (host:port) is used as-is.
func waitAddr(waitPort string) string {
	if waitPort == "" {
		return ""
	}
	if strings.Contains(waitPort, ":") {
		return waitPort
	}
	return "127.0.0.1:" + waitPort
}

// tailBuffer is an io.Writer that retains only the last max bytes written — a
// noisy port-forward can log for the whole session without growing memory.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
