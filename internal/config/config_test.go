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
cmd = "kubectl port-forward svc/db 5432:5432"
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
	if conns[1].URL != "postgres://x" {
		t.Fatalf("fields wrong: %+v", conns[1])
	}
	if conns[1].Cmd != "kubectl port-forward svc/db 5432:5432" {
		t.Fatalf("cmd not parsed: %+v", conns[1])
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
