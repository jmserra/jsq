//go:build unix

package tui

import (
	"os/exec"
	"syscall"
)

// setpgid puts the `run` helper in its own process group so that killGroup can
// take down the whole tree — sh plus whatever it forks (a port-forward and its
// children). Without this, killing only the direct child leaves grandchildren
// alive, and (since we capture output via a pipe) Wait blocks until they exit.
func setpgid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup SIGKILLs the whole process group (negative pid).
func killGroup(c *exec.Cmd) {
	syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
}
