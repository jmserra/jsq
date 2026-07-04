// Package config loads the read-only connections file (see README.md).
// The file is TOML with one section per connection; the section header is the
// connection name. jsq never writes this file.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Conn is one connection entry. Name comes from the section header.
type Conn struct {
	Name     string `toml:"-"`
	URL      string `toml:"url"`
	Env      string `toml:"env"`
	ReadOnly bool   `toml:"read_only"`
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

// DSN returns the connection URL with the password injected from the env var
// (the `env = ...` key) when the URL itself carries no password.
func (c Conn) DSN() string {
	if c.Env == "" {
		return c.URL
	}
	pw := os.Getenv(c.Env)
	if pw == "" {
		return c.URL
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.User == nil {
		return c.URL
	}
	if _, has := u.User.Password(); has {
		return c.URL
	}
	u.User = url.UserPassword(u.User.Username(), pw)
	return u.String()
}
