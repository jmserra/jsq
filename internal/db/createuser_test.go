package db

import (
	"strings"
	"testing"
)

// TestCreateUserSQL pins the shape of each dialect's CREATE USER template. It is
// what the user sees in $EDITOR and runs verbatim, so the parts that matter are
// that the placeholder name appears everywhere the account is referenced (one
// edit renames it throughout) and that the starting grant is narrow.
func TestCreateUserSQL(t *testing.T) {
	cases := []struct {
		name    string
		eng     Engine
		dbName  string
		want    []string
		notWant []string
	}{
		{
			name:   "mysql",
			eng:    &myEngine{},
			dbName: "shop",
			want: []string{
				"CREATE USER 'newuser'@'%' IDENTIFIED BY 'change-me';",
				"GRANT SELECT ON `shop`.* TO 'newuser'@'%';",
			},
			notWant: []string{"ALL PRIVILEGES", "*.*"},
		},
		{
			name:   "mysql without a database",
			eng:    &myEngine{},
			dbName: "",
			want:   []string{"CREATE USER 'newuser'@'%'"},
			// No database to scope a grant to: say nothing rather than reach for *.*
			notWant: []string{"GRANT"},
		},
		{
			name:   "postgres",
			eng:    &pgEngine{},
			dbName: "shop",
			want: []string{
				`CREATE ROLE "newuser" LOGIN PASSWORD 'change-me';`,
				`GRANT CONNECT ON DATABASE "shop" TO "newuser";`,
				`GRANT USAGE ON SCHEMA public TO "newuser";`,
				`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "newuser";`,
				`-- ALTER DEFAULT PRIVILEGES`, // offered as a comment, not run
			},
			notWant: []string{"SUPERUSER", "CREATEDB"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.eng.CreateUserSQL("newuser", c.dbName)
			t.Logf("\n%s", got)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("template is missing %q:\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("template should not contain %q:\n%s", w, got)
				}
			}
		})
	}

	// SQLite has no accounts, so there is nothing to template.
	if got := (&sqliteEngine{}).CreateUserSQL("newuser", "shop"); got != "" {
		t.Errorf("sqlite template = %q, want empty", got)
	}
}

// TestDropUserSQL pins the drop statement. Postgres carries the two statements
// that unblock it as comments — a role that owns objects cannot simply be
// dropped, and REASSIGN OWNED is too consequential to run unasked.
func TestDropUserSQL(t *testing.T) {
	if got := (&myEngine{}).DropUserSQL(User{Name: "bob", Host: "%"}); got != "DROP USER 'bob'@'%';\n" {
		t.Errorf("mysql DropUserSQL = %q", got)
	}
	got := (&pgEngine{}).DropUserSQL(User{Name: "bob"})
	for _, want := range []string{
		`DROP ROLE "bob";`,
		`-- REASSIGN OWNED BY "bob" TO CURRENT_USER;`,
		`-- DROP OWNED BY "bob";`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("postgres DropUserSQL is missing %q:\n%s", want, got)
		}
	}
	// The DROP itself must not be commented out — only the two remedies are.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "DROP ROLE") {
			return
		}
	}
	t.Errorf("the DROP itself should be live SQL:\n%s", got)
}
