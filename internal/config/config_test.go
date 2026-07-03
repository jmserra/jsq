package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrderAndFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	body := `
[local]
url = "sqlite:///tmp/a.db"

[prod]
url = "postgres://x"
env = "PW"
read_only = true
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	conns, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("want 2 conns, got %d", len(conns))
	}
	if conns[0].Name != "local" || conns[1].Name != "prod" {
		t.Fatalf("file order not preserved: %+v", conns)
	}
	if !conns[1].ReadOnly || conns[1].Env != "PW" || conns[1].URL != "postgres://x" {
		t.Fatalf("fields wrong: %+v", conns[1])
	}
}

func TestDSNInjectsPassword(t *testing.T) {
	t.Setenv("JSQ_TEST_PW", "s3cret")

	c := Conn{URL: "postgres://jm@host:5432/app", Env: "JSQ_TEST_PW"}
	if got, want := c.DSN(), "postgres://jm:s3cret@host:5432/app"; got != want {
		t.Fatalf("DSN = %q, want %q", got, want)
	}
	// No env var configured → URL unchanged.
	if c := (Conn{URL: "postgres://jm@host/app"}); c.DSN() != c.URL {
		t.Fatalf("no-env DSN changed: %q", c.DSN())
	}
	// URL already carries a password → unchanged.
	if c := (Conn{URL: "postgres://jm:existing@host/app", Env: "JSQ_TEST_PW"}); c.DSN() != c.URL {
		t.Fatalf("existing-password DSN changed: %q", c.DSN())
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	conns, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if conns != nil {
		t.Fatalf("want nil, got %v", conns)
	}
}
