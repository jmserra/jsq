package db

import (
	"context"
	"testing"
)

// checkUsersLive exercises Users + GrantsSQL against a live server: list the
// users, then run the grants read for the first one and check it comes back in
// the shared scope/object/privilege/grantable shape. Called by the MySQL and
// Postgres live tests, so the SQL each engine composes is actually run — it is
// too dialect-specific to be worth asserting on as text.
func checkUsersLive(t *testing.T, ctx context.Context, e Engine) {
	t.Helper()
	users, err := e.Users(ctx)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	t.Logf("%d users", len(users))
	for i, u := range users {
		if i >= 8 {
			t.Logf("  … (%d more)", len(users)-8)
			break
		}
		t.Logf("  %s", u.Label())
	}
	if len(users) == 0 {
		return
	}

	query, args, err := e.GrantsSQL(ctx, users[0])
	if err != nil {
		t.Fatalf("grants sql: %v", err)
	}
	if query == "" {
		t.Fatal("an engine with users must return a grants query")
	}
	rs, err := e.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("grants(%s): %v", users[0].Label(), err)
	}
	want := []string{"scope", "object", "privilege", "grantable"}
	if len(rs.Cols) != len(want) {
		t.Fatalf("grants columns = %v, want %v", rs.Cols, want)
	}
	for i, c := range rs.Cols {
		if c != want[i] {
			t.Fatalf("grants columns = %v, want %v", rs.Cols, want)
		}
	}
	t.Logf("grants for %s → %d row(s)", users[0].Label(), len(rs.Rows))
	for i, r := range rs.Rows {
		if i >= 8 {
			t.Logf("  … (%d more)", len(rs.Rows)-8)
			break
		}
		t.Logf("  %-9v %-28v %-24v %v", r[0], r[1], r[2], r[3])
	}
}
