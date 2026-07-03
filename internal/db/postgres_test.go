package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestOpenPostgresRoutes verifies a postgres DSN reaches the driver (not the
// "not implemented" stub). It connects to a refused port, so it fails fast.
func TestOpenPostgresRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Open(ctx, "postgres://jsq@127.0.0.1:1/nope")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("postgres should route to the pgx driver, got: %v", err)
	}
}
