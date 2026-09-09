package db

import "testing"

// TestRevokeSQL pins the statement each grants row turns back into. The rows are
// exactly what GrantsSQL emits, so this is the round trip that makes `D` on the
// grants view work: one row in, the statement that undoes it out.
func TestRevokeSQL(t *testing.T) {
	my, pg := &myEngine{}, &pgEngine{}
	bob := User{Name: "bob", Host: "%"}
	role := User{Name: "bob"} // Postgres has no host half

	cases := []struct {
		name string
		eng  Engine
		u    User
		g    Grant
		want string
	}{
		{"mysql global", my, bob,
			Grant{Scope: "global", Privilege: "SELECT"},
			"REVOKE SELECT ON *.* FROM 'bob'@'%';\n"},
		{"mysql database", my, bob,
			Grant{Scope: "database", Object: "shop", Privilege: "INSERT"},
			"REVOKE INSERT ON `shop`.* FROM 'bob'@'%';\n"},
		{"mysql table", my, bob,
			Grant{Scope: "table", Object: "shop.items", Privilege: "UPDATE"},
			"REVOKE UPDATE ON `shop`.`items` FROM 'bob'@'%';\n"},
		{"mysql column", my, bob,
			Grant{Scope: "column", Object: "shop.items.price", Privilege: "UPDATE"},
			"REVOKE UPDATE (`price`) ON `shop`.`items` FROM 'bob'@'%';\n"},
		// A role grant is a different statement shape: no ON clause at all.
		{"mysql role", my, bob,
			Grant{Scope: "role", Object: "readonly@%", Privilege: "MEMBER"},
			"REVOKE 'readonly'@'%' FROM 'bob'@'%';\n"},

		{"postgres table", pg, role,
			Grant{Scope: "table", Object: "public.items", Privilege: "SELECT"},
			`REVOKE SELECT ON "public"."items" FROM "bob";` + "\n"},
		{"postgres database", pg, role,
			Grant{Scope: "database", Object: "shop", Privilege: "CONNECT"},
			`REVOKE CONNECT ON DATABASE "shop" FROM "bob";` + "\n"},
		{"postgres schema", pg, role,
			Grant{Scope: "schema", Object: "public", Privilege: "USAGE"},
			`REVOKE USAGE ON SCHEMA "public" FROM "bob";` + "\n"},
		{"postgres role", pg, role,
			Grant{Scope: "role", Object: "readonly", Privilege: "MEMBER"},
			`REVOKE "readonly" FROM "bob";` + "\n"},
		// An attribute is a role flag, not a grant: it negates with ALTER ROLE.
		{"postgres attribute", pg, role,
			Grant{Scope: "attribute", Privilege: "SUPERUSER"},
			`ALTER ROLE "bob" NOSUPERUSER;` + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.eng.RevokeSQL(c.u, c.g); got != c.want {
				t.Errorf("RevokeSQL =\n%q\nwant\n%q", got, c.want)
			}
		})
	}

	// An unknown scope has no statement — the caller reports it rather than
	// running something invented.
	if got := my.RevokeSQL(bob, Grant{Scope: "sequence", Privilege: "USAGE"}); got != "" {
		t.Errorf("unknown scope should produce nothing, got %q", got)
	}
	if got := (&sqliteEngine{}).RevokeSQL(bob, Grant{Scope: "global", Privilege: "SELECT"}); got != "" {
		t.Errorf("sqlite has no grants, got %q", got)
	}
}

// TestGrantSQL checks the `o` template on a grants view: narrow, and aimed at
// the user whose privileges are on screen.
func TestGrantSQL(t *testing.T) {
	if got := (&myEngine{}).GrantSQL(User{Name: "bob", Host: "%"}, "shop"); got != "GRANT SELECT ON `shop`.* TO 'bob'@'%';\n" {
		t.Errorf("mysql GrantSQL = %q", got)
	}
	if got := (&pgEngine{}).GrantSQL(User{Name: "bob"}, "shop"); got != `GRANT SELECT ON ALL TABLES IN SCHEMA public TO "bob";`+"\n" {
		t.Errorf("postgres GrantSQL = %q", got)
	}
}
