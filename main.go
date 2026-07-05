// Command jsq is a minimal, vim-only SQL TUI. See README.md.
package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jsq:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := config.DefaultPath()
	var arg string

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 >= len(args) {
				return fmt.Errorf("-c needs a path")
			}
			cfgPath = args[i+1]
			i++
		default:
			if arg == "" {
				arg = args[i]
			}
		}
	}

	conns, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	var direct config.Conn
	switch {
	case arg == "":
		if len(conns) == 0 {
			return fmt.Errorf("no connections in %s and no URL/path given", cfgPath)
		}
	case looksLikeDSN(arg):
		direct = config.Conn{URL: arg}
	default:
		c, ok := findConn(conns, arg)
		if !ok {
			return fmt.Errorf("unknown connection %q; available: %s", arg, names(conns))
		}
		direct = c
	}

	// Whatever quits us (Esc, error screen, Ctrl-C, panic), reap any `run` helper
	// so a port-forward never outlives jsq. The registry is populated the instant
	// a helper starts, so this doesn't depend on the connect flow having finished.
	defer tui.KillRunHelpers()

	p := tea.NewProgram(tui.New(conns, direct), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return err
	}
	// A connect failure quits the program carrying its error out here, so it
	// prints to stderr (via main) instead of dead-ending on an in-app screen.
	if a, ok := final.(tui.App); ok {
		return a.FatalErr()
	}
	return nil
}

// looksLikeDSN treats URLs and filesystem-looking paths as ad-hoc connections;
// a bare word is a connection name.
func looksLikeDSN(s string) bool {
	return strings.Contains(s, "://") || strings.ContainsAny(s, "/\\.")
}

func findConn(conns []config.Conn, name string) (config.Conn, bool) {
	for _, c := range conns {
		if c.Name == name {
			return c, true
		}
	}
	return config.Conn{}, false
}

func names(conns []config.Conn) string {
	out := make([]string, len(conns))
	for i, c := range conns {
		out[i] = c.Name
	}
	return strings.Join(out, ", ")
}
