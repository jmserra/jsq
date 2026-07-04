# jsq

A minimal, highly opinionated, **vim-style terminal UI for SQL databases** —
PostgreSQL, MySQL, and SQLite. One static Go binary, keyboard-only, no mouse.
(The name nods to `jq` and `sq`.)

jsq is heavily inspired by [lazysql](https://github.com/jorgerojas26/lazysql).
It's a deliberately smaller, more opinionated take on the same idea: fast,
keyboard-driven database browsing, with SQL authoring and heavier edits handed
off to your real `$EDITOR`. See [Relationship to lazysql](#relationship-to-lazysql).

This README is both the user guide and the design document — practical usage
first, then the philosophy and design that shape it.

---

## Status

Browsing works end-to-end across all three engines: connect, list tables, and a
fixed-width results grid with continuous scroll, per-column sort, per-column
filter, and a full-cell viewer. **Both cell edits are in: the quick overlay
(`e`) and the `$EDITOR` full path (`E`).**

On the roadmap (parts of the design below describe the intended end state, not
what ships today):

- **Editing** — `o`/`D`/`p` (insert / delete / duplicate) generated as SQL and
  opened in `$EDITOR` (the `e` quick cell edit and `E` full-path edit already work).
- **`s` / `S`** — author free-form SQL in `$EDITOR`.
- **Query history** — `Ctrl-r` picker and `Ctrl-o` step-back.
- **`?` help overlay** — generated from the keymap.
- **Clipboard yank** (`y`/`Y`) and a **database picker**.

---

## Install

```sh
# from source
git clone https://github.com/jmserra/jsq && cd jsq
go build -o jsq .
# or
go install github.com/jmserra/jsq@latest
```

No cgo — SQLite is pure-Go (`modernc.org/sqlite`), so cross-compilation is trivial.

## Usage

```sh
jsq <name>                     # connect using a named entry in the config file
jsq                            # no args → interactive connection picker
jsq postgres://user@host/db    # ad-hoc: connect straight to a URL
jsq ./local.db                 # ad-hoc: a file path → SQLite
jsq -c ~/other.toml <name>     # use a different connections file
```

- **`jsq <name>`** connects straight to that connection.
- **`jsq`** (no argument) opens the **connection picker**: a `j`/`k` list of every
  connection in the file; `Enter` connects, `Esc` / `Ctrl-c` quits.
- An argument that parses as a **URL or file path** is treated as ad-hoc and
  bypasses the file entirely (engine inferred from the scheme / extension).
- An unknown `<name>` prints the available names and exits non-zero.

The engine is inferred from the URL scheme (`postgres`/`postgresql`, `mysql`,
`sqlite`/`file`) or a bare file path (SQLite).

## Connections file

Location: `$JSQ_CONFIG` or `~/.config/jsq/connections.toml`. **jsq only ever
reads this file** — there is no in-app connection editor, ever. One section per
connection; the section header *is* the connection name, so `jsq local` connects
the `[local]` section. A missing file is fine — ad-hoc URL/path invocation still
works.

```toml
# ~/.config/jsq/connections.toml

[local]
url = "postgres://user@localhost:5432/appdev?sslmode=disable"

[prod]
url = "postgres://user@prod.example.com:5432/app"
env = "JSQ_PROD_PASSWORD"     # password injected from this env var at connect

[work]
url = "mysql://user@localhost:3306/mydb"

[notes]
url = "sqlite:///path/to/notes.db"   # or just: "./notes.db"

[prod-ro]
url = "postgres://user@prod.example.com:5432/app"
read_only = true              # jsq refuses all mutations on this connection
```

Put the password in the URL, or point at an env var with `env = "..."` so the
file can live in your dotfiles without a secret in it. Engine is inferred from
the URL scheme (`postgres`/`postgresql`, `mysql`, `sqlite`/`file`/bare path). The
picker lists the section names in file order. Format is **TOML** — the
section-per-connection layout reads like INI but gets a strict parser.

## Keybindings

| Key | Action |
| --- | --- |
| `h` `j` `k` `l` | move cursor |
| `g` / `G` | first / last row |
| `0` / `$` | first / last column |
| `J` / `K` | sort current column ascending / descending |
| `/` | filter current column (type to preview; `↑`/`↓` browse matches) |
| `Enter` (grid) | commit filter, or — with no filter — inspect the full cell value |
| `Esc` | clear the current column's filter |
| `e` | quick-edit the current cell (single-line overlay; `Enter` runs a PK-keyed `UPDATE`, `Esc` cancels) |
| `E` | edit the current cell in `$EDITOR` — opens the generated keyed `UPDATE` with the value pre-selected (vim/nvim); `:wq` runs it, `:q!` or an empty buffer aborts |
| `H` | toggle the table sidebar (focuses it; auto-hides on select) |
| `Enter` (sidebar) | open the selected table |
| `Tab` / `Shift-Tab` | cycle focus between sidebar and grid |
| `Ctrl-c` | quit |

**Filtering** is prefix-search by default (a trailing `%` is added
automatically); type a leading `%` yourself for a contains match. Filters are
case-insensitive, work on any column type, and stack across columns. **Scrolling
is continuous** — reaching the loaded edge fetches the next window; there are no
pages. The grid opens sorted by primary key, newest first.

**Editing** with `e` is available only when the grid came from a single-table
select with a resolved primary key, and the connection isn't `read_only`. The
`UPDATE` is always keyed on the full primary key and runs immediately; the status
line reports what changed. A bare `Enter` that changed nothing (including an
untouched `NULL` cell) does nothing — so you can't blank a value by accident. For
long, multi-line, `NULL`-setting, or otherwise fiddly edits, `E` opens the
generated `UPDATE` in your `$EDITOR` instead: review it, `:wq` to run, `:q!` to
abort.

---

# Design

## Philosophy

- **Minimal surface.** Every feature must earn its place. When in doubt, leave it out.
- **Vim-only.** No mouse, no arrow-key crutches required, no discoverability UI
  beyond a `?` cheat sheet. Modal where it helps.
- **Route work to real tools.** Writing SQL happens in your real `$EDITOR`
  (nvim), not a reimplemented in-app editor. jsq generates SQL and hands it off.
- **Read-mostly, edit-deliberately.** Browsing is instant and safe. Mutations are
  explicit, show you the exact statement, and require confirmation.
- **No hidden state.** Connections live in a file *you* edit. jsq never writes it.

### Non-goals

- No connection manager UI (no add/edit/delete connections from inside the app).
- No built-in SQL editor widget, no autocomplete engine, no SQL lexer/highlighter
  of our own (that's the editor's job).
- No staged/pending-change buffer with batch commit (lazysql's biggest complexity).
- No MSSQL, no NoSQL, no cloud-specific auth flows.
- No saved-queries store, no query history persistence (v1). Reconsider later.
- No mouse support.

### Scope (locked)

| Decision            | Choice                                                        |
|---------------------|---------------------------------------------------------------|
| Engines             | PostgreSQL, MySQL, SQLite                                      |
| Read vs write       | Read + **deliberate** editing                                 |
| SQL authoring       | External `$EDITOR` only                                       |
| Connections         | CLI arg **and** a manually-edited, app-read-only file         |

## Tech stack

- **Language:** Go. Single static binary and a best-in-class TUI + `database/sql`
  ecosystem. No cgo (SQLite is pure-Go via `modernc.org/sqlite`).
- **TUI framework:** `bubbletea` with `lipgloss` (styling) and `bubbles`
  (components). Chosen over tview because jsq's grid is deliberately
  **fixed-width**, which erases tview's main advantage (its built-in data table)
  while keeping bubbletea's wins: an explicit Model/Update/View state machine that
  maps cleanly onto vim modes, and `tea.Cmd`-based async that eliminates the
  manual-redraw draw-race bug class entirely. Fixed-width truncation uses
  `mattn/go-runewidth` / `lipgloss.Width` for correct wide-char handling.
- **DB access:** standard `database/sql` with `jackc/pgx/v5/stdlib` (Postgres),
  `go-sql-driver/mysql` (MySQL), `modernc.org/sqlite` (SQLite, pure-Go, no cgo).
- **Config parsing:** TOML (`BurntSushi/toml`).

## Architecture

The core is the async rule: **no `db.Engine` call ever runs inside `Update`.**
Queries and execs are dispatched as `tea.Cmd`s that return a `tea.Msg` when they
finish; `Update` only ever mutates state from messages. This is what structurally
removes the draw-race class of bug.

```
main.go                     # flag parse, resolve connection, bootstrap the TUI
internal/config/            # load + validate connections.toml (read-only)
internal/db/                # the ONE abstraction (Engine) + one impl per engine
internal/tui/               # bubbletea: each file is a Model with Update/View
```

> The current source layout is flatter than the eventual target — SQL generation
> lives inline in `internal/tui/cmd.go` and keybindings inline in `app.go` for
> now. Dedicated `sqlgen`/`editor`/`keymap` units will appear as the `$EDITOR`
> and help-overlay features land. See `CLAUDE.md` for the exact current file map.

### The one abstraction: `db.Engine`

Deliberately small — everything the TUI needs, nothing it doesn't.

```go
type Engine interface {
    // Introspection
    Tables(ctx) ([]Table, error)
    Columns(ctx, TableRef) ([]Column, error)
    PrimaryKey(ctx, TableRef) ([]string, error)  // empty => read-only view
    Databases(ctx) ([]string, error)             // deferred database picker

    // Data
    Query(ctx, sql string, args ...any) (*ResultSet, error)
    Exec(ctx, sql string, args ...any) (affected int64, err error)

    // Dialect hooks
    QuoteIdent(s string) string       // "col" vs `col`
    QualifiedName(t TableRef) string  // schema-qualified, quoted table name
    Placeholder(i int) string         // $1 vs ?
    UsesSchemas() bool                // pg=true, mysql/sqlite=false
    FilterPredicate(quotedCol string, i int) string  // case-insensitive LIKE
    Close() error
}
```

`ResultSet` carries column names, `[][]any` rows, and — crucially — enough
provenance (source table + PK) to decide whether the grid is **editable**.
`Column` is enriched (`Nullable`, `PrimaryKey`, `AutoGenerated`, `HasDefault`,
`Default`, `Unique`) so SQL generation can produce correct inserts/duplicates
without guessing. Per-engine sources for the tricky fields: Postgres
`information_schema` (`is_identity`, `nextval(...)` default); MySQL
`EXTRA = 'auto_increment'`; SQLite `PRAGMA table_info` plus the `INTEGER PRIMARY
KEY` rowid alias.

## UI & navigation

### Layout

Two panes — a declutter-first layout with an on-demand sidebar:

```
 local > appdev > users
┌ tables (toggle)  ┬─────────── results ───────────────┐
│ users            │  id  name        email            │
│ orders           │  1   Ada         ada@x.io          │
│ line_items       │  2   Linus       linus@x.io        │
│ …                │  …                                 │
└──────────────────┴────────────────────────────────────┘
```

- **Single-line header:** `connection > database > table`.
- **Sidebar is on-demand.** Hidden by default; the results pane is the star. The
  switch-table loop is `H` → `j`/`k` or `/` to find it → `Enter`: `H` brings the
  sidebar back **focused**, `Enter` loads the table and the sidebar auto-hides.
- **Flat, filterable table list — no tree.** jsq opens straight into the database
  named by the connection; the sidebar is a single flat list of that database's
  tables, and `/` filters it. Names are schema-qualified (`sales.orders`) only for
  non-default schemas; `public` tables show bare names.
- **Single result pane, no tabs.** Each query replaces what's shown.

### Results grid (fixed-width)

The grid is intentionally simple — the constraint that made bubbletea the right
framework. No auto-sizing, no wrapping, no variable widths.

- **Every column has a fixed width.** Default from a per-type/name heuristic,
  values truncated with `…` to fit (rune-width aware via `go-runewidth`).
- **Cursor is a single cell** (`cursorR`, `cursorC`), highlighted; `h/j/k/l` move it.
- **Scrolling is by slice, not geometry.** `rowOff`/`colOff` track the top-left
  visible cell. Horizontal scroll steps whole columns (trivial with fixed widths).
- **No pages — continuous lazy scroll.** `j`/`k` scroll; crossing the loaded edge
  fetches the next window and extends the buffer, so it feels like one long list.
  `g`/`G` jump to the first / last loaded row. **No total row count** — jsq never
  runs `COUNT(*)`, so opening or filtering a huge table is always cheap.
- **NULL vs empty rendering.** SQL `NULL` → dimmed `NULL`; empty string → blank
  cell; a literal string `"NULL"` → normal text — all three visually distinct.
- **Control chars are glyphed** (`\n`→`↵`, `\t`→`→`, `\r` dropped) so a cell can
  never spill across terminal rows. The real value is available via `Enter`.
- **The header row** is fixed vertically and scrolls horizontally in lockstep with
  the body. Each header cell shows the column name, its active filter pattern, and
  a sort marker (`▲`/`▼`).

### Column filters (`/`)

`/` filters the column under the cursor — a two-phase, fzf-like flow: a live
client-side preview of the loaded rows while you type, committed to a real
server-side query on `Enter`.

- What you type is the `LIKE` pattern; a trailing `%` is implied (prefix search),
  a leading `%` you type yourself (contains). Always case-insensitive, any column
  type, bound as a parameter.
- **Live preview** narrows the currently-loaded rows in place with the same LIKE
  semantics — doubling as in-grid "find". `↓`/`↑` move through the narrowed rows
  while the input is active (arrows, since `j`/`k` are literal text).
- **`Enter` commits** the filter server-side across the whole table; **`Esc`
  clears** that column's filter. Filters **stack** (`AND`-ed). The filtered result
  is still a single-table select, so the grid **stays editable**.
- Per-engine predicate: pg `LOWER("col"::text) LIKE LOWER($n)`; mysql
  `LOWER(CAST(\`col\` AS CHAR)) LIKE LOWER(?)`; sqlite `LOWER(CAST("col" AS TEXT))
  LIKE LOWER(?)`.

### Sorting (`J` / `K`)

`J` / `K` order the whole result by the column under the cursor (ASC / DESC) —
server-side re-query, then continuous scroll from the top. Single active sort
column (a new `J`/`K` replaces it; `J` then `K` flips direction). The sort column
is the scroll key, tiebroken by PK. **Default order** is by primary key
descending (newest first). The header shows a `▲`/`▼` marker. Composes with
filters.

### Query history *(roadmap)*

One result pane, not tabs — the query, not the result, is the thing you keep.
Planned: a session-scoped in-memory ring of every statement (reads and mutations
alike); `Ctrl-r` history picker (reads re-execute, mutations open in nvim);
`Ctrl-o` steps back to the previous read. Classification errs safe — only a
leading `SELECT`/`SHOW`/`EXPLAIN`/`PRAGMA` counts as a read; everything else,
including any `WITH …`, is treated as a mutation.

### Modes

Only **Normal** and **Filter**, plus transient overlays (cell-edit input,
full-cell viewer, and eventually the history picker and help). There is no `:`
command mode, and no in-app SQL "insert mode" — editing SQL suspends jsq and drops
you into `$EDITOR`.

## Editing model

Goal: keep mutation power without lazysql's change-tracking machinery. **No
pending-changes buffer** — each mutation is a single statement, either previewed
in nvim (full path) or shown in the status line after it runs (quick path). No
batching, no staged diff, no commit step. Each statement autocommits.

### Editability rule

A grid is editable **only** when its rows came from a single-table select and jsq
resolved a primary key. Otherwise edit keys are inert (status line says why).
**Connections are editable by default** — the sole opt-out is `read_only = true`
on a connection, which disables all mutation regardless.

### Two-speed editing

- **Quick path — inline overlay** *(built)*: `e` on a cell opens a single-line
  input pre-filled with the current value. `Enter` builds the PK-keyed `UPDATE`
  and runs it immediately (low blast radius); `Esc` cancels. jsq reports the
  affected-row count. `Enter` on empty input sets `''`; a bare `Enter` on an
  untouched `NULL` is a no-op (stays NULL) so you can't blank it by accident.
  Setting a value *to* `NULL` is done via the `E` full path (write `NULL`
  unquoted). This is the only in-app text-input widget in jsq.
- **Full path — generate SQL → `$EDITOR` → `:wq`**: jsq builds the statement from
  the current row and opens it in your editor; a save (`:wq`) runs the SQL
  verbatim, an empty buffer or quit-without-save (`:q!`) aborts. The value is
  inlined at the end of the `SET` line, and — for vim/nvim — the editor opens
  with the cursor on it and the value pre-selected in Visual mode, so `c` edits it
  straight away (inside the quotes for a string, the whole token for a
  number/`NULL`). Set `NULL` by writing a bare `NULL`. The statement is always
  keyed on the full PK. `E` (edit the current cell) is **built**; `o`
  (blank `INSERT` skeleton), `D` (`DELETE … WHERE pk = …`), and `p` (duplicate:
  an `INSERT` pre-filled from the current row, same table only, auto-generated PK
  omitted / natural PK flagged as the value to change) are **roadmap** on this
  same path. Because you author the SQL, the full path runs it as written — it is
  not parameterized, unlike the quick path.

### Safety rails

- Every generated statement has a `WHERE` keyed on the full PK — never a bare
  `UPDATE`/`DELETE`.
- Show affected-row count after exec; if it's not exactly 1 for a keyed edit, warn
  loudly.
- No implicit transactions spanning multiple edits. Each statement autocommits.
- Arbitrary destructive SQL you write yourself in `$EDITOR` is your own
  responsibility — jsq runs what you `:wq`.

## Configuration

- `~/.config/jsq/connections.toml` — connections (above), app-read-only.
- Env: `$JSQ_CONFIG` (file location), `$EDITOR`, and each connection's own
  password var (the `env = ...` key). Keybindings are compiled in; a keymap
  override file is a possible later addition.

## Build & distribution

- `go build -o jsq .` → a single static binary. No cgo, so cross-compilation is
  trivial. A `Makefile` provides `build` (stripped/optimized), `run`, `test`,
  `tidy`, and `clean` targets.

## Deferred (post-v1)

Column width adjust · disk-persisted history · database picker (`Databases()` is
already on the `Engine` interface) · vertical row-detail view for wide tables ·
cross-table duplicate · filter stacking as OR / clear-all · keymap override file.

## Relationship to lazysql

jsq owes its shape to [lazysql](https://github.com/jorgerojas26/lazysql) — the
idea of a fast, keyboard-driven database TUI, and much of the interaction feel.
Where jsq differs is scope: it trades breadth for a smaller, more opinionated
surface, and pushes text-editing work out to `$EDITOR`. Concretely, jsq leaves
out (by design): the connection manager UI, the built-in SQL editor / completer /
lexer / highlighter, the staged pending-changes buffer + commit/rollback,
persistent saved queries / on-disk history, and ancillary UI (CSV export,
standalone JSON viewer, MSSQL, multi-step connection screens).

The result does less, in fewer keystrokes, with a codebase small enough to keep
entirely in your head. If you want the full-featured experience, use lazysql —
it's excellent.
