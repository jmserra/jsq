package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

type pgEngine struct {
	stdEngine
}

func openPostgres(ctx context.Context, dsn string) (Engine, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	sdb, err := openStd(ctx, "pgx", stdlib.RegisterConnConfig(cfg), "connecting to postgres")
	if err != nil {
		return nil, err
	}
	return &pgEngine{stdEngine{db: sdb}}, nil
}

// pgSchema defaults an unqualified table to the public schema.
func pgSchema(t TableRef) string {
	if t.Schema != "" {
		return t.Schema
	}
	return "public"
}

func (e *pgEngine) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }

func (e *pgEngine) QuoteIdent(s string) string { return quoteIdentDouble(s) }

func (e *pgEngine) QualifiedName(t TableRef) string {
	if t.Schema != "" {
		return e.QuoteIdent(t.Schema) + "." + e.QuoteIdent(t.Name)
	}
	return e.QuoteIdent(t.Name)
}

func (e *pgEngine) FilterPredicate(quotedCol string, i int) string {
	return fmt.Sprintf("LOWER(%s::text) LIKE LOWER($%d)", quotedCol, i)
}

// ProcessListSQL lists the cluster's backends, longest-running query first
// (idle sessions have a NULL query_start, which sorts last). Our own backend is
// excluded — it is always the query you are reading. A non-superuser sees other
// users' rows but their query text is hidden unless it is pg_read_all_stats.
func (e *pgEngine) ProcessListSQL() string {
	return `SELECT pid, usename, datname, client_addr, state,
       now() - query_start AS duration, wait_event_type, wait_event, query
FROM pg_stat_activity
WHERE pid <> pg_backend_pid()
ORDER BY query_start`
}

// Users lists the cluster's roles — both login roles and the group roles that
// grant through them, since a role's privileges are only legible alongside the
// groups it belongs to. The pg_* built-in roles (pg_read_all_stats and friends)
// are filtered out as noise; every user can read pg_roles, so this needs no
// privilege of its own.
func (e *pgEngine) Users(ctx context.Context) ([]User, error) {
	names, err := queryStrings(ctx, e.db,
		`SELECT rolname FROM pg_roles WHERE rolname NOT LIKE 'pg\_%' ORDER BY rolname`)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(names))
	for i, n := range names {
		out[i] = User{Name: n}
	}
	return out, nil
}

// GrantsSQL assembles the role's privileges from the catalogs into the shared
// scope/object/privilege shape. Four sources, widest first:
//
//   - attribute: the pg_roles flags (SUPERUSER, CREATEDB, LOGIN, …). Postgres has
//     no global privilege table; these flags are its equivalent.
//   - role: pg_auth_members — the groups this role belongs to. Privileges reached
//     through them are NOT expanded here, so a role row is the pointer to follow.
//   - database/schema/table: aclexplode over the object's ACL. COALESCE to
//     acldefault matters — a NULL acl means "never granted, defaults apply", so
//     without it an object's owner would show no privileges on what they own.
//
// Read from pg_catalog rather than information_schema.table_privileges, which
// only shows grants involving a currently-enabled role — it would silently
// return nothing for the other users you are here to inspect. Grants to PUBLIC
// are left out: they belong to everyone, not to this role.
func (e *pgEngine) GrantsSQL(_ context.Context, u User) (string, []any, error) {
	return `SELECT scope, object, privilege, grantable FROM (
  SELECT 'attribute' AS scope, '' AS object, a.attr AS privilege, '' AS grantable
    FROM pg_roles r
    CROSS JOIN LATERAL (VALUES
      ('SUPERUSER', r.rolsuper), ('CREATEDB', r.rolcreatedb), ('CREATEROLE', r.rolcreaterole),
      ('LOGIN', r.rolcanlogin), ('REPLICATION', r.rolreplication), ('BYPASSRLS', r.rolbypassrls),
      ('INHERIT', r.rolinherit)) AS a(attr, held)
    WHERE r.rolname = $1 AND a.held
  UNION ALL
  SELECT 'role', g.rolname, 'MEMBER', CASE WHEN m.admin_option THEN 'YES' ELSE 'NO' END
    FROM pg_auth_members m
    JOIN pg_roles g ON g.oid = m.roleid
    JOIN pg_roles c ON c.oid = m.member
    WHERE c.rolname = $1
  UNION ALL
  SELECT 'database', d.datname, x.privilege_type, CASE WHEN x.is_grantable THEN 'YES' ELSE 'NO' END
    FROM pg_database d
    CROSS JOIN LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) x
    WHERE pg_get_userbyid(x.grantee) = $1
  UNION ALL
  SELECT 'schema', n.nspname, x.privilege_type, CASE WHEN x.is_grantable THEN 'YES' ELSE 'NO' END
    FROM pg_namespace n
    CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) x
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_'
      AND pg_get_userbyid(x.grantee) = $1
  UNION ALL
  SELECT 'table', n.nspname || '.' || c.relname, x.privilege_type,
         CASE WHEN x.is_grantable THEN 'YES' ELSE 'NO' END
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) x
    WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
      AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_'
      AND pg_get_userbyid(x.grantee) = $1
) g
ORDER BY CASE scope
           WHEN 'attribute' THEN 0 WHEN 'role' THEN 1 WHEN 'database' THEN 2
           WHEN 'schema' THEN 3 ELSE 4 END,
         object, privilege`, []any{u.Name}, nil
}

// DropUserSQL removes the role — but Postgres refuses while anything still
// depends on it: objects it owns, or privileges granted to it. The two
// statements that clear that are offered as comments above the DROP rather than
// run, because REASSIGN OWNED moves real tables to another owner and that is a
// decision, not a detail. Order matters if they are used: reassign first (it
// moves ownership), then DROP OWNED (which clears the remaining grants).
func (e *pgEngine) DropUserSQL(u User) string {
	role := e.QuoteIdent(u.Name)
	return "-- Postgres refuses while the role owns objects or holds grants; if so:\n" +
		"-- REASSIGN OWNED BY " + role + " TO CURRENT_USER;\n" +
		"-- DROP OWNED BY " + role + ";\n" +
		"DROP ROLE " + role + ";\n"
}

// GrantSQL templates one more grant for an existing role, on this database's
// public schema — the narrow starting point, to widen by hand.
func (e *pgEngine) GrantSQL(u User, _ string) string {
	return "GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + e.QuoteIdent(u.Name) + ";\n"
}

// RevokeSQL reverses one grants row. Three of the scopes are ordinary REVOKEs on
// an object; the other two are not REVOKEs at all, which is the point of routing
// through the engine: a role membership is REVOKE <role> FROM <role>, and an
// `attribute` row is a role flag, so taking it away is ALTER ROLE … NO<flag>.
func (e *pgEngine) RevokeSQL(u User, g Grant) string {
	role := e.QuoteIdent(u.Name)
	switch g.Scope {
	case "attribute":
		// INHERIT/LOGIN/SUPERUSER/… all negate with a NO prefix.
		return "ALTER ROLE " + role + " NO" + g.Privilege + ";\n"
	case "role":
		return "REVOKE " + e.QuoteIdent(g.Object) + " FROM " + role + ";\n"
	case "database":
		return "REVOKE " + g.Privilege + " ON DATABASE " + e.QuoteIdent(g.Object) + " FROM " + role + ";\n"
	case "schema":
		return "REVOKE " + g.Privilege + " ON SCHEMA " + e.QuoteIdent(g.Object) + " FROM " + role + ";\n"
	case "table":
		schema, tbl := cutDot(g.Object)
		return "REVOKE " + g.Privilege + " ON " + e.QuoteIdent(schema) + "." + e.QuoteIdent(tbl) +
			" FROM " + role + ";\n"
	}
	return ""
}

// CreateUserSQL templates what a usable Postgres login role needs, which is more
// than one statement: the role, the right to connect to this database, the right
// to see into the schema, and then the tables themselves. The last grant covers
// only the tables that exist right now — ALTER DEFAULT PRIVILEGES is what covers
// future ones, mentioned in a comment rather than run, since it is a standing
// policy change and should be a deliberate edit.
func (e *pgEngine) CreateUserSQL(name, dbName string) string {
	q := e.QuoteIdent(name)
	sql := "CREATE ROLE " + q + " LOGIN PASSWORD 'change-me';\n"
	if dbName != "" {
		sql += "GRANT CONNECT ON DATABASE " + e.QuoteIdent(dbName) + " TO " + q + ";\n"
	}
	sql += "GRANT USAGE ON SCHEMA public TO " + q + ";\n" +
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + q + ";\n" +
		"-- existing tables only; for future ones:\n" +
		"-- ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO " + q + ";\n"
	return sql
}

func (e *pgEngine) Databases(ctx context.Context) ([]string, error) {
	// datallowconn filters out databases that reject connections (e.g. template0),
	// which would otherwise be offered in the switcher but fail on select.
	return queryStrings(ctx, e.db,
		`SELECT datname FROM pg_database WHERE datistemplate = false AND datallowconn ORDER BY datname`)
}

func (e *pgEngine) Tables(ctx context.Context) ([]Table, error) {
	// Ordinary + partitioned tables (relkind r/p — same set pg_catalog.pg_tables
	// exposes), minus two kinds of noise: the system/temp/toast schemas, and any
	// table owned by an extension (a pg_depend 'e' edge). The latter hides PostGIS
	// et al. — spatial_ref_sys, the tiger geocoder tables, topology — which the
	// user can't meaningfully edit and never wants cluttering the list; a real
	// user table is never extension-owned, so it always survives the filter.
	rows, err := e.db.QueryContext(ctx, `
		SELECT n.nspname, c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname !~ '^pg_'
		  AND NOT EXISTS (
			SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_class'::regclass
			  AND d.objid = c.oid
			  AND d.deptype = 'e')
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Table
	for rows.Next() {
		var s, n string
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out = append(out, Table{Schema: s, Name: n})
	}
	return out, rows.Err()
}

func (e *pgEngine) Columns(ctx context.Context, t TableRef) ([]Column, error) {
	schema := pgSchema(t)
	rows, err := e.db.QueryContext(ctx, `
		SELECT column_name, data_type, column_default, is_identity
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []Column
	idx := map[string]int{}
	for rows.Next() {
		var name, dtype, isIdentity string
		var dflt sql.NullString
		if err := rows.Scan(&name, &dtype, &dflt, &isIdentity); err != nil {
			return nil, err
		}
		auto := isIdentity == "YES" || (dflt.Valid && strings.HasPrefix(dflt.String, "nextval("))
		idx[name] = len(cols)
		cols = append(cols, Column{
			Name:          name,
			Type:          dtype,
			HasDefault:    dflt.Valid,
			AutoGenerated: auto,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pk, err := e.PrimaryKey(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, name := range pk {
		if i, ok := idx[name]; ok {
			cols[i].PrimaryKey = true
			cols[i].Unique = true
		}
	}
	uniq, err := e.uniqueColumns(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, name := range uniq {
		if i, ok := idx[name]; ok {
			cols[i].Unique = true
		}
	}
	return cols, nil
}

// uniqueColumns returns columns covered by a UNIQUE constraint (used to annotate
// generated inserts). Bare unique indexes aren't included — constraints are the
// common case and mirror the PrimaryKey query.
func (e *pgEngine) uniqueColumns(ctx context.Context, t TableRef) ([]string, error) {
	return queryStrings(ctx, e.db, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'UNIQUE'
		  AND tc.table_schema = $1 AND tc.table_name = $2`, pgSchema(t), t.Name)
}

func (e *pgEngine) PrimaryKey(ctx context.Context, t TableRef) ([]string, error) {
	return queryStrings(ctx, e.db, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = $1 AND tc.table_name = $2
		ORDER BY kcu.ordinal_position`, pgSchema(t), t.Name)
}

// ForeignKeys reads FK constraints from pg_catalog, walking each constraint's
// parallel (conkey, confkey) column arrays by subscript so composite keys keep
// their column order.
func (e *pgEngine) ForeignKeys(ctx context.Context, t TableRef) ([]ForeignKey, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT con.conname, att.attname, nsp2.nspname, cl2.relname, att2.attname
		FROM pg_constraint con
		JOIN pg_class cl       ON cl.oid = con.conrelid
		JOIN pg_namespace nsp  ON nsp.oid = cl.relnamespace
		JOIN pg_class cl2      ON cl2.oid = con.confrelid
		JOIN pg_namespace nsp2 ON nsp2.oid = cl2.relnamespace
		JOIN generate_subscripts(con.conkey, 1) AS s(i) ON true
		JOIN pg_attribute att  ON att.attrelid = con.conrelid  AND att.attnum = con.conkey[s.i]
		JOIN pg_attribute att2 ON att2.attrelid = con.confrelid AND att2.attnum = con.confkey[s.i]
		WHERE con.contype = 'f' AND nsp.nspname = $1 AND cl.relname = $2
		ORDER BY con.conname, s.i`, pgSchema(t), t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var acc fkAccum
	for rows.Next() {
		var name, col, refSchema, refTable, refCol string
		if err := rows.Scan(&name, &col, &refSchema, &refTable, &refCol); err != nil {
			return nil, err
		}
		acc.add(name, TableRef{Schema: refSchema, Name: refTable}, col, refCol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return acc.result(), nil
}

// --- SQL export (see dump.go) ---

func (e *pgEngine) SQLLiteral(v any, colType string) string {
	switch x := v.(type) {
	case []byte:
		return pgBytesLiteral(x, colType)
	case string:
		return pgBytesLiteral([]byte(x), colType)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	}
	return stdSQLLiteral(v, false)
}

// pgBytesLiteral writes text as a quoted string — with backslashes left alone,
// since the prologue sets standard_conforming_strings=on — and binary as the
// hex bytea input format. '\x' (empty) is valid, so no special case.
func pgBytesLiteral(b []byte, colType string) string {
	if !binaryBytes(b, colType) {
		return sqlQuote(string(b), false)
	}
	return `'\x` + hexBytes(b) + `'::bytea`
}

// StructureSQL empties the table rather than recreating it. Postgres DDL cannot
// be had from the server the way SHOW CREATE TABLE gives it on MySQL, and
// rebuilding one from information_schema loses indexes, sequence ownership and
// every constraint — so jsq emits no CREATE TABLE it would get wrong, and points
// at pg_dump instead. The DELETE runs under session_replication_role='replica'
// (the prologue), so it neither trips foreign keys nor fires ON DELETE CASCADE.
func (e *pgEngine) StructureSQL(_ context.Context, t TableRef) (string, error) {
	q := e.QualifiedName(t)
	return fmt.Sprintf("-- structure: jsq writes no Postgres DDL — for the schema run\n"+
		"--   pg_dump -s -t %s\n"+
		"DELETE FROM %s;\n", q, q), nil
}

// DumpPrologue disables foreign-key triggers for the load, which lets the tables
// replay in any order. session_replication_role needs superuser (or pg 15+'s
// pg_write_all_data); without it the SET fails and the load runs with the
// constraints live, which only matters if the table order is wrong.
func (e *pgEngine) DumpPrologue() string {
	return "SET client_encoding = 'UTF8';\n" +
		"SET standard_conforming_strings = on;\n" +
		"SET session_replication_role = 'replica';\n"
}

func (e *pgEngine) DumpEpilogue() string { return "SET session_replication_role = 'origin';\n" }
