//go:build !unix

package tui

import "os/exec"

// Non-unix fallback: no process groups, so we can only signal the direct child.
// A `run` helper that forks children may leave them running on exit.
func setpgid(c *exec.Cmd) {}

func killGroup(c *exec.Cmd) { c.Process.Kill() }
