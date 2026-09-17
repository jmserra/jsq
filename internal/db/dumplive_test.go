package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The dump halves that can only be checked against a real server: whether the
// driver's type names identify a binary column, and whether the engine can hand
// over its own DDL. Read-only — both tests pick a table and look at it.
//
// Same env vars as the other live tests: JSQ_TEST_MYSQL_DB (+ _HOST/_USER/_PASS)
// and JSQ_TEST_PG.

func liveMySQL(t *testing.T) Engine {
	t.Helper()
	dbName := os.Getenv("JSQ_TEST_MYSQL_DB")
	if dbName == "" {
		t.Skip("set JSQ_TEST_MYSQL_DB to run the live mysql test")
	}
	host := os.Getenv("JSQ_TEST_MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1:3306"
	}
	u := url.URL{Scheme: "mysql", Host: host, Path: "/" + dbName}
	if user := os.Getenv("JSQ_TEST_MYSQL_USER"); user != "" {
		u.User = url.UserPassword(user, os.Getenv("JSQ_TEST_MYSQL_PASS"))
	}
	e, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// TestMySQLLiveDump checks the export halves against a real server: SHOW CREATE
// TABLE comes back as DDL, and a streamed row renders to literals. MySQL returns
// []byte for text columns as readily as for blobs, so this is where the type
// name doing that work is actually exercised.
func TestMySQLLiveDump(t *testing.T) {
	e := liveMySQL(t)
	ctx := context.Background()

	tables, err := e.Tables(ctx)
	if err != nil {
		t.Fatalf("tables: %v", err)
	}
	if len(tables) == 0 {
		t.Skip("no tables")
	}
	ref := tables[0].Ref()

	ddl, err := e.StructureSQL(ctx, ref)
	if err != nil {
		t.Fatalf("StructureSQL(%s): %v", ref.Name, err)
	}
	if !strings.HasPrefix(ddl, "DROP TABLE IF EXISTS `") || !strings.Contains(ddl, "CREATE TABLE") {
		t.Errorf("structure of %s is not a DROP+CREATE:\n%s", ref.Name, ddl)
	}
	t.Logf("structure of %s:\n%s", ref.Name, firstLines(ddl, 6))

	var types []string
	var rendered []string
	err = e.Stream(ctx, "SELECT * FROM "+e.QualifiedName(ref)+" LIMIT 1", nil, RowStream{
		Head: func(_, ty []string) error { types = ty; return nil },
		Row: func(vals []any) error {
			for i, v := range vals {
				rendered = append(rendered, e.SQLLiteral(v, types[i]))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for i, lit := range rendered {
		// Every literal must be self-delimiting: NULL, a number, a quoted string
		// or a 0x blob. A bare unquoted word would mean a []byte fell through.
		if lit == "" {
			t.Errorf("column %d (%s) rendered empty", i, types[i])
		}
		if strings.HasPrefix(lit, "'") && !strings.HasSuffix(lit, "'") {
			t.Errorf("column %d (%s) is an unterminated string: %s", i, types[i], lit)
		}
	}
	t.Logf("first row of %s: %s", ref.Name, firstLines(strings.Join(rendered, ", "), 1))
}

// TestPostgresLiveDump is the same check for Postgres, where the structure block
// is deliberately a DELETE plus a pointer at pg_dump rather than composed DDL.
func TestPostgresLiveDump(t *testing.T) {
	dsn := os.Getenv("JSQ_TEST_PG")
	if dsn == "" {
		t.Skip("set JSQ_TEST_PG to run the live postgres test")
	}
	ctx := context.Background()
	e, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()

	tables, err := e.Tables(ctx)
	if err != nil {
		t.Fatalf("tables: %v", err)
	}
	if len(tables) == 0 {
		t.Skip("no tables")
	}
	ref := tables[0].Ref()

	ddl, err := e.StructureSQL(ctx, ref)
	if err != nil {
		t.Fatalf("StructureSQL: %v", err)
	}
	if !strings.Contains(ddl, "pg_dump") || !strings.Contains(ddl, "DELETE FROM") {
		t.Errorf("postgres structure block is not the DELETE + pg_dump note:\n%s", ddl)
	}

	var types []string
	var rendered []string
	err = e.Stream(ctx, "SELECT * FROM "+e.QualifiedName(ref)+" LIMIT 1", nil, RowStream{
		Head: func(_, ty []string) error { types = ty; return nil },
		Row: func(vals []any) error {
			for i, v := range vals {
				rendered = append(rendered, e.SQLLiteral(v, types[i]))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for i, lit := range rendered {
		if lit == "" {
			t.Errorf("column %d (%s) rendered empty", i, types[i])
		}
		// standard_conforming_strings is on in the prologue, so a backslash in a
		// value must still be a single backslash here.
		if strings.Contains(lit, `\\`) && !strings.HasPrefix(lit, `'\x`) {
			t.Errorf("column %d (%s) doubled a backslash: %s", i, types[i], lit)
		}
	}
	t.Logf("first row of %s: %s", tableLabel(ref), firstLines(strings.Join(rendered, ", "), 1))
}

// tableLabel is the test's own display form (the TUI has its own).
func tableLabel(t TableRef) string {
	if t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	out := strings.Join(lines, "\n")
	if len(out) > 600 {
		out = out[:600] + "…"
	}
	return out
}
