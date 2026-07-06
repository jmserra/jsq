// Package config loads the connections file (see README.md). jsq only ever
// reads this file. The file is TOML with one section per connection; the
// section header is the connection name.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Conn is one connection entry. Name comes from the section header.
type Conn struct {
	Name string `toml:"-"`
	URL  string `toml:"url"`

	// Safe, when true, makes jsq pop a y/n confirmation naming the connection and
	// database before it runs any mutation on this connection. Defaults to false.
	Safe bool `toml:"safe"`

	// Cmd is a shell command started before connecting and kept alive for the
	// whole session (e.g. a port-forward), then terminated on exit. When set, jsq
	// waits for the URL's host:port to accept connections before opening the DB
	// (probed once a second, up to 30s). Empty → none.
	Cmd string `toml:"cmd"`
}

// DefaultPath returns $JSQ_CONFIG or ~/.config/jsq/connections.toml.
func DefaultPath() string {
	if p := os.Getenv("JSQ_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "jsq", "connections.toml")
}

// Load reads the connections file, returning connections in file order.
// A missing file is not an error — it yields an empty slice.
func Load(path string) ([]Conn, error) {
	raw := map[string]Conn{}
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Preserve section order using the document key order.
	var order []string
	seen := map[string]bool{}
	for _, k := range md.Keys() {
		top := k[0]
		if !seen[top] {
			seen[top] = true
			order = append(order, top)
		}
	}

	conns := make([]Conn, 0, len(order))
	for _, name := range order {
		c := raw[name]
		c.Name = name
		conns = append(conns, c)
	}
	return conns, nil
}
