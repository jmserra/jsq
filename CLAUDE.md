# CLAUDE.md — start-of-session notes for jsq

`README.md` is the whole human-facing story (usage + philosophy + design + status).
This file is the small set of things *I* need before editing the code: the real
file map, the invariants that must not break, and where the code diverges from
what README's design section describes. Don't duplicate README here.

## Build / test

- `make build` — `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o jsq .`.
  Keep it cgo-free (SQLite is pure-Go via `modernc.org/sqlite`) — that's what
  makes cross-compilation trivial. Never add a cgo dep.
- `make test` — `go test ./...`. SQLite + config + tui tests run offline.
- `internal/db` **Postgres/MySQL tests are live and skipped unless env vars are
  set**: `JSQ_TEST_PG=<dsn>`; `JSQ_TEST_MYSQL_DB` (+ optional `_HOST`/`_USER`/
  `_PASS`). For a local MySQL, per global config just run `mysql <dbname>`.
- `internal/tui/app_test.go` drives the whole bubbletea model **headlessly** —
  feed `tea.Msg`/`tea.KeyMsg` into `Update`, assert on `View()`. That's the
  pattern for any UI behavior test; no terminal needed. Follow it.
- The `jsq` binary in the repo root is git-ignored.

## Actual file map (flatter than README's design tree)

README's design section names `sqlgen`/`editor`/`keymap`/`meta` units. **They
don't exist yet.** What's really here:

```
main.go                     # flag parse (-c + one positional), resolve conn, boot tea
internal/config/config.go   # load read-only connections.toml; Conn.DSN() injects env password
internal/db/
  db.go                     # Engine interface + Open() dispatch + shared scanQuery + DSN helpers
  sqlite.go postgres.go mysql.go   # one Engine impl each
internal/tui/
  app.go        # root App Model: screen/focus, ALL key routing (hardcoded — no keymap.go), layout, View
  cmd.go        # tea.Cmd constructors — the ONLY place db.Engine is called; also $EDITOR spawn (editorCmd)
  sqlgen.go     # pure SQL-text generation for the $EDITOR full paths (buildUpdateStmt E, buildInsertStmt o; D/p later)
  msg.go        # tea.Msg types (connectedMsg, rowsMsg, moreRowsMsg, editDoneMsg, editorSubmitMsg/AbortedMsg, execDoneMsg, errMsg)
  grid.go       # fixed-width grid Model: cursor, scroll, sort marker, filter, e-edit overlay, fullEditTarget
  sidebar.go    # flat filterable table list Model
  picker.go     # connection picker (bare `jsq`)
  cellview.go   # read-only full-cell viewer (Enter); pretty-prints JSON
  util.go       # clamp()
```

The `$EDITOR` full path (`E`/`o`, and later `D`/`p`/`s`/`S`): the generators
return an `editorSeed` (SQL + cursor line/col + `selectKind`); `editorCmd` seeds a
temp file and spawns `$EDITOR` via `tea.ExecProcess`. `E` builds its seed inline
(`buildUpdateStmt`, in-memory grid data); `o` needs enriched column metadata so
it goes async — `prepareInsertCmd` fetches `Columns()` then `buildInsertStmt`,
returning an `editorReadyMsg` that `Update` turns into `editorCmd`. (`Columns()`
now also populates `Column.Unique` per engine — pg/sqlite via a unique-constraint
/ unique-index query, mysql via `column_key`; it's only called for insert prep,
not on every table open.) For vim-family editors
(`isVimFamily`), `positionArgs` adds `+call cursor(...)` and a `feedkeys` (not
`:normal`, which drops the selection) so the editor opens with the value
selected — `vi'` inside a string's quotes, `v$` for a NULL/number token. On exit
`editorResult` decides submit-vs-abort (mtime bump or content change → run;
cleared buffer or `:q!` → abort). Submitted SQL runs **verbatim** via
`execRawCmd`, then the view reloads. This is the deliberate exception to
invariant 6 — full-path SQL is user-authored and inlined (`sqlLiteral`), not
parameter-bound.

Note: many `.go` comments still carry `§N` section refs that pointed at the old
DESIGN.md — harmless shorthand, but they no longer resolve to a numbered doc.

## Invariants — do not break

1. **No `db.Engine` call inside `Update`.** All DB work is a `tea.Cmd` in `cmd.go`
   returning a `tea.Msg`; `Update` only mutates state from messages. This removes
   the draw-race bug class. Never call the engine from `app.go`.
2. **Every mutation is keyed on the full PK** — never a bare `UPDATE`/`DELETE`.
   After exec, warn loudly if affected != 1 (see `editDoneMsg` in `app.go`).
3. **Editability** (`grid.editable()`): single-table select + resolved PK + every
   PK column present in the result. Otherwise edit keys are inert.
4. **`read_only` gates before editability.** Any new edit key (`E`/`o`/`D`/`p`)
   must check `a.readOnly` first, like the `"e"` handler does.
5. **jsq only ever reads `connections.toml`.** No write path. Keep it so.
6. **Bind values as parameters** (`Placeholder(i)` + args) — filter patterns,
   quick-path edit values, PK predicates. Only identifiers are interpolated, via
   `QuoteIdent`. **Sole exception:** the `$EDITOR` full path runs user-authored
   SQL verbatim (values inlined by `sqlLiteral`) — that's the documented model,
   not a leak. New `$EDITOR`-authored statements follow it; anything jsq runs
   *without* the user seeing the SQL must be parameter-bound.

## Code vs. README design — real divergences (not just "not built yet")

Know these before touching scroll/paging:

- **Scroll is `LIMIT`/`OFFSET`, not keyset.** `cmd.go:loadMoreCmd` does
  `ORDER BY … LIMIT n OFFSET len(rows)`, despite the design calling for keyset on
  the sort key. Concurrent writes mid-scroll can dup/skip rows. Migrating to
  keyset is a real feature, not a cleanup.
- **Fetch window isn't viewport-sized.** `app.go:gridLimit()` = `max(200,
  (h-2)*4)` — a fixed floor of 200, used for both initial load and scroll window.
- **`G` doesn't fetch a tail window** — `grid.bottom()` just jumps the cursor to
  the end of the loaded buffer; `maybeLoadMore` may then extend it.
- **No `(more↓)` / row-position status hint** — `grid.hasMore` is tracked but the
  status line only shows `conn > db > table/message`.

## Conventions when extending

- **New keybinding** → a case in the relevant `handle*Key` in `app.go`; if it does
  DB work, return a `tea.Cmd` from `cmd.go`. Update README's keybinding table.
- **New DB work** → `tea.Cmd` in `cmd.go` + `tea.Msg` in `msg.go`, handled in
  `Update`. Build SQL with `QualifiedName`/`QuoteIdent`/`Placeholder` so it stays
  correct across all three engines.
- **Per-engine differences live behind the `Engine` interface** — add a method
  there rather than type-switching on the engine in the TUI.
- **Filter semantics** (keep grid + sidebar identical): `searchPattern` appends a
  trailing `%` (prefix search); leading `%` is user-typed. Case-insensitive, any
  type, via `FilterPredicate`. Client preview uses `likeToRegex` with the same
  semantics so preview == committed result.
- **NULL vs empty rendering**: `nil` → faint `NULL`; `""` → blank; literal
  `"NULL"` string → normal text. Driven by the `isNull` flag, not string content.

## Per-engine gotchas

- **Postgres**: schema-aware; non-`public` tables render `schema.table`.
  `AutoGenerated` = identity or `nextval(...)` default.
- **MySQL**: URL → driver DSN via `mysql.Config` in `mysqlDSN` (don't hand-format).
  Single DB (`DATABASE()`), no schema qualification. `AutoGenerated` = `extra`
  has `auto_increment`.
- **SQLite**: `PRAGMA table_info`; a lone `INTEGER PRIMARY KEY` is the rowid alias
  → `AutoGenerated`. DSN accepts `sqlite://`, `file:`, or a bare path.
