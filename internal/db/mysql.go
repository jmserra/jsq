package db

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql" // registers the "mysql" driver
)

type myEngine struct {
	stdEngine
}

func openMySQL(ctx context.Context, rawurl string) (Engine, error) {
	dsn, err := mysqlDSN(rawurl)
	if err != nil {
		return nil, err
	}
	sdb, err := openStd(ctx, "mysql", dsn, "connecting to mysql")
	if err != nil {
		return nil, err
	}
	return &myEngine{stdEngine{db: sdb}}, nil
}

// mysqlDSN converts a mysql:// URL into the go-sql-driver DSN, using the
// driver's own Config so passwords/params are escaped correctly.
func mysqlDSN(rawurl string) (string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "", err
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	host := u.Host
	if host == "" {
		host = "127.0.0.1:3306"
	} else if _, _, err := net.SplitHostPort(host); err != nil {
		// No port (SplitHostPort errors) → default it. JoinHostPort re-brackets an
		// IPv6 literal, so `[::1]` and bare hostnames both work (the old substring
		// `:` check mistook an IPv6 host for one that already had a port).
		host = net.JoinHostPort(strings.Trim(host, "[]"), "3306")
	}
	cfg.Addr = host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Passwd = pw
		}
	}
	if q := u.Query(); len(q) > 0 {
		cfg.Params = map[string]string{}
		for k := range q {
			cfg.Params[k] = q.Get(k)
		}
	}
	return cfg.FormatDSN(), nil
}

func (e *myEngine) Placeholder(int) string { return "?" }

func (e *myEngine) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func (e *myEngine) QualifiedName(t TableRef) string { return e.QuoteIdent(t.Name) }

func (e *myEngine) FilterPredicate(quotedCol string, _ int) string {
	return fmt.Sprintf("LOWER(CAST(%s AS CHAR)) LIKE LOWER(?)", quotedCol)
}

// ProcessListSQL lists the server's threads, longest-running first.
// information_schema (not SHOW FULL PROCESSLIST) so the result can be ordered;
// the table is present in every MySQL 5.6+ and MariaDB. Needs the PROCESS
// privilege to see other users' threads — without it you just see your own.
func (e *myEngine) ProcessListSQL() string {
	return `SELECT id, user, host, db, command, time, state, info
FROM information_schema.processlist
ORDER BY time DESC`
}

// Users lists the server's accounts (user + host — 'bob'@'%' and 'bob'@'localhost'
// are two accounts with their own privileges). mysql.user is the complete list,
// but reading it needs SELECT on the mysql database; without that we fall back to
// information_schema.user_privileges, which every account can read for itself —
// so an unprivileged connection sees just its own account rather than an error.
// Both halves are 5.7-era, so this works unchanged from 5.7 to 9.x.
func (e *myEngine) Users(ctx context.Context) ([]User, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT user, host FROM mysql.user ORDER BY user, host`)
	if err != nil {
		return e.usersFromPrivileges(ctx)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name, &u.Host); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *myEngine) usersFromPrivileges(ctx context.Context) ([]User, error) {
	names, err := queryStrings(ctx, e.db, `
		SELECT DISTINCT grantee FROM information_schema.user_privileges
		ORDER BY grantee`)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(names))
	for i, n := range names {
		out[i] = parseGrantee(n)
	}
	return out, nil
}

// parseGrantee splits information_schema's grantee form ('bob'@'%') back into the
// two halves. Anything that isn't in that form is taken as a bare name.
func parseGrantee(s string) User {
	i := strings.LastIndex(s, `'@'`)
	if i < 0 || !strings.HasPrefix(s, `'`) || !strings.HasSuffix(s, `'`) {
		return User{Name: s}
	}
	unq := func(x string) string { return strings.ReplaceAll(x, `''`, `'`) }
	return User{Name: unq(s[1:i]), Host: unq(s[i+3 : len(s)-1])}
}

// grantee renders the account the way information_schema stores it. The result is
// BOUND as a parameter, never interpolated — the quote doubling is only so the
// string equals what the catalog holds for a name containing a quote.
func grantee(u User) string {
	q := func(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }
	return q(u.Name) + "@" + q(u.Host)
}

// GrantsSQL unions MySQL's four information_schema privilege tables into the
// shared scope/object/privilege shape, ordered widest scope first. Those four
// are complete for privileges — verified against 9.7.1, where user_privileges
// lists the DYNAMIC privileges (MANAGE_DATA_MASKING_POLICY et al.) alongside the
// static ones, so mysql.global_grants would only duplicate them.
//
// The union itself is 5.7-safe: those four views, FIELD(), IF() and CONCAT() are
// all old, and 5.7 has no dynamic privileges to miss.
//
// Role grants are the exception: they live in mysql.role_edges and appear in no
// information_schema table, which matters because a user whose privileges all
// arrive through a role otherwise reads as having none. That table is 8.0+ and
// needs SELECT on mysql.*, so it is probed by reading it rather than assumed —
// missing (5.7) OR unreadable drops the part instead of failing the whole query,
// and 5.7 has no roles for it to lose. (MariaDB keeps the same thing in
// mysql.roles_mapping with different columns; unhandled, so roles are simply not
// listed there.)
func (e *myEngine) GrantsSQL(ctx context.Context, u User) (string, []any, error) {
	parts := []string{
		`SELECT 'global' AS scope, '' AS object, privilege_type AS privilege, is_grantable AS grantable
		   FROM information_schema.user_privileges WHERE grantee = ?`,
		`SELECT 'database', table_schema, privilege_type, is_grantable
		   FROM information_schema.schema_privileges WHERE grantee = ?`,
		`SELECT 'table', CONCAT(table_schema, '.', table_name), privilege_type, is_grantable
		   FROM information_schema.table_privileges WHERE grantee = ?`,
		`SELECT 'column', CONCAT(table_schema, '.', table_name, '.', column_name), privilege_type, is_grantable
		   FROM information_schema.column_privileges WHERE grantee = ?`,
	}
	g := grantee(u)
	args := []any{g, g, g, g}

	if e.readable(ctx, "mysql.role_edges") {
		parts = append(parts, `SELECT 'role', CONCAT(FROM_USER, '@', FROM_HOST), 'MEMBER', IF(WITH_ADMIN_OPTION, 'YES', 'NO')
		   FROM mysql.role_edges WHERE TO_USER = ? AND TO_HOST = ?`)
		args = append(args, u.Name, u.Host)
	}

	sql := "SELECT scope, object, privilege, grantable FROM (\n" +
		strings.Join(parts, "\nUNION ALL\n") +
		"\n) g\nORDER BY FIELD(scope, 'global', 'role', 'database', 'table', 'column'), object, privilege"
	return sql, args, nil
}

// readable reports whether a catalog table both exists and can be selected from
// by this connection — the two ways an optional privilege source drops out.
func (e *myEngine) readable(ctx context.Context, table string) bool {
	// table is one of this file's own constants, never user input.
	rows, err := e.db.QueryContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1")
	if err != nil {
		return false
	}
	rows.Close()
	return true
}

func (e *myEngine) Databases(ctx context.Context) ([]string, error) {
	return queryStrings(ctx, e.db, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema','mysql','performance_schema','sys')
		ORDER BY schema_name`)
}

func (e *myEngine) Tables(ctx context.Context) ([]Table, error) {
	names, err := queryStrings(ctx, e.db, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	return namesToTables(names), nil
}

func (e *myEngine) Columns(ctx context.Context, t TableRef) ([]Column, error) {
	unique, err := e.uniqueColumns(ctx, t)
	if err != nil {
		return nil, err
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT column_name, data_type, column_default, extra, column_key
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ?
		ORDER BY ordinal_position`, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []Column
	for rows.Next() {
		var name, dtype, extra, key string
		var dflt sql.NullString
		if err := rows.Scan(&name, &dtype, &dflt, &extra, &key); err != nil {
			return nil, err
		}
		cols = append(cols, Column{
			Name:          name,
			Type:          dtype,
			PrimaryKey:    key == "PRI",
			Unique:        unique[name],
			AutoGenerated: strings.Contains(extra, "auto_increment"),
			HasDefault:    dflt.Valid,
		})
	}
	return cols, rows.Err()
}

// uniqueColumns returns every column covered by a unique index (PRIMARY included).
// column_key only flags the leading column of a composite unique index, so a
// UNIQUE(a,b) would leave b unflagged — statistics.non_unique=0 covers all of them,
// matching how pg/sqlite mark every covered column.
func (e *myEngine) uniqueColumns(ctx context.Context, t TableRef) (map[string]bool, error) {
	names, err := queryStrings(ctx, e.db, `
		SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND non_unique = 0`, t.Name)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// PrimaryKey reads just the PK columns from key_column_usage — deliberately not
// via Columns, which pulls column_default/extra and (in composite order) is
// heavier. PrimaryKey is on the hot path (every load and scroll fetch).
func (e *myEngine) PrimaryKey(ctx context.Context, t TableRef) ([]string, error) {
	return queryStrings(ctx, e.db, `
		SELECT column_name
		FROM information_schema.key_column_usage
		WHERE table_schema = DATABASE() AND table_name = ?
		  AND constraint_name = 'PRIMARY'
		ORDER BY ordinal_position`, t.Name)
}

// ForeignKeys reads FK constraints from key_column_usage (rows carrying a
// referenced_table_name), grouped by constraint name and ordered within a
// composite key by ordinal_position. Single-DB, so the ref table has no schema.
func (e *myEngine) ForeignKeys(ctx context.Context, t TableRef) ([]ForeignKey, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT constraint_name, column_name, referenced_table_name, referenced_column_name
		FROM information_schema.key_column_usage
		WHERE table_schema = DATABASE() AND table_name = ?
		  AND referenced_table_name IS NOT NULL
		ORDER BY constraint_name, ordinal_position`, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var acc fkAccum
	for rows.Next() {
		var name, col, refTable, refCol string
		if err := rows.Scan(&name, &col, &refTable, &refCol); err != nil {
			return nil, err
		}
		acc.add(name, TableRef{Name: refTable}, col, refCol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return acc.result(), nil
}
