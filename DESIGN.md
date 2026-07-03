# jsq — Design

A minimal, highly opinionated, vim-style terminal UI for SQL databases
(PostgreSQL, MySQL, SQLite). A single static Go binary. (The name nods to `jq`
and `sq`.)

> **Credit.** jsq is heavily inspired by
> [lazysql](https://github.com/jorgerojas26/lazysql). It is a deliberately
> smaller, more opinionated take on the same idea — fast, keyboard-only database
> browsing — that trades lazysql's breadth for a tighter surface and hands SQL
> authoring and heavier edits off to your real `$EDITOR`. Much gratitude to
> lazysql for showing the way. See §13 for how the two relate.

---

## 1. Philosophy

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

---

## 2. Scope (locked)

| Decision            | Choice                                                        |
|---------------------|---------------------------------------------------------------|
| Engines             | PostgreSQL, MySQL, SQLite                                      |
| Read vs write       | Read + **deliberate** editing (model in §8)                   |
| SQL authoring       | External `$EDITOR` only                                        |
| Connections         | CLI arg **and** a manually-edited, app-read-only file         |

---

## 3. Tech stack

- **Language:** Go. Single static binary and a best-in-class TUI + `database/sql`
  ecosystem.
- **TUI framework:** `bubbletea` (`charmbracelet/bubbletea`) with `lipgloss`
  (styling) and `bubbles` (components). Chosen over tview because jsq's grid is
  deliberately **fixed-width** (§7), which erases tview's main advantage (its
  built-in data table) while keeping bubbletea's wins: an explicit Model/Update/
  View state machine that maps cleanly onto vim modes, and `tea.Cmd`-based async
  that eliminates the manual-redraw draw-race bug class entirely. Components
  used: `bubbles/textinput` (the `e` cell-edit overlay), `bubbles/viewport`
  (row scrolling), `bubbles/key` + `bubbles/help` (declarative bindings + the
  `?` cheat sheet). Fixed-width truncation uses `mattn/go-runewidth` /
  `lipgloss.Width` for correct wide-char handling.
- **DB access:** standard `database/sql` with:
  - `github.com/jackc/pgx/v5/stdlib` (Postgres)
  - `github.com/go-sql-driver/mysql` (MySQL)
  - `modernc.org/sqlite` (SQLite, pure-Go, no cgo)
- **Config parsing:** TOML (`github.com/BurntSushi/toml`).

---

## 4. Invocation

```
jsq <name>                   # connect to the named connection from the file
jsq                          # no argument → interactive connection picker
jsq postgres://user@host/db  # ad-hoc: connect directly to a URL, ignore the file
jsq ./local.db               # ad-hoc: a path → SQLite
jsq -c ~/other.toml <name>   # override the connections-file location
```

- **`jsq <name>`** connects straight to that connection.
- **`jsq`** (no argument) opens the **connection picker**: a `j`/`k` list of every
  connection in the file; `Enter` connects, `Esc` / `Ctrl-c` quits. It's the same
  flat-list widget as the sidebar, so it costs no new UI.
- An argument that parses as a **URL or file path** is treated as ad-hoc and
  bypasses the file entirely (engine inferred from the scheme / extension).
- An unknown `<name>` prints the available names and exits non-zero.

---

## 5. Connections file (app-read-only)

- Location: `$JSQ_CONFIG` or `~/.config/jsq/connections.toml`.
- **jsq only ever reads this file.** No add/edit/delete from the UI, ever.
- Missing file is fine — ad-hoc URL/path invocation still works.
- **One section per connection; the section header IS the connection name** — so
  `jsq local` connects the `[local]` section. No `name =` field, no `default`.
- Secrets: inline the URL, or point at an env var (`env = ...`) so the file can
  live in your dotfiles without a password in it.

```toml
# ~/.config/jsq/connections.toml

[local]
url = "postgres://user@localhost:5432/appdev?sslmode=disable"

[prod]
url = "postgres://user@prod.example.com:5432/app"
env = "JSQ_PROD_PASSWORD"        # password injected from this env var at connect

[mysql-scratch]
url = "mysql://user@localhost:3306/scratch"

[notes]
url = "sqlite:///path/to/notes.db"    # or just: "./notes.db"

[prod-ro]
url = "postgres://user@prod.example.com:5432/app"
read_only = true                 # jsq refuses all mutations on this conn (§8)
```

Engine is inferred from the URL scheme (`postgres`/`postgresql`, `mysql`,
`sqlite`/`file`/bare path). The picker lists the section names in file order.

Format is **TOML** — the section-per-connection layout reads like INI but gets a
strict parser and adds no dependency beyond the one already in §3.

---

## 6. Architecture

```
jsq/
├── main.go                 # flag parse, resolve connection, bootstrap TUI
├── go.mod
└── internal/
    ├── config/             # load + validate connections.toml (read-only)
    ├── db/
    │   ├── db.go           # Engine interface (the ONE abstraction)
    │   ├── postgres.go
    │   ├── mysql.go
    │   ├── sqlite.go
    │   └── meta.go         # schema/table/column/PK introspection helpers
    ├── sqlgen/             # build UPDATE/DELETE/INSERT from a row + key
    ├── editor/             # $EDITOR via tea.ExecProcess (releases the
    │                       #   terminal, restores TUI on quit), capture buffer
    └── tui/                # bubbletea: each file is a Model with Update/View
        ├── app.go          # root Model: composes panes, owns mode + focus,
        │                   #   routes tea.Msg, runs DB work as tea.Cmd
        ├── picker.go       # connection picker (bare `jsq`): j/k list + Enter
        ├── sidebar.go      # flat table-list navigator Model (on-demand)
        ├── grid.go         # fixed-width results grid Model (§7)
        ├── overlay.go      # textinput cell-edit overlay + full-cell viewer +
        │                   #   confirm / error / help popups
        ├── msg.go          # tea.Msg types (rowsLoaded, execDone, dbErr, …)
        ├── cmd.go          # tea.Cmd constructors wrapping db.Engine calls
        └── keymap.go       # bubbles/key bindings — single source of truth
```

Async rule: **no `db.Engine` call ever runs inside `Update`.** Queries and execs
are dispatched as `tea.Cmd`s (in `cmd.go`) that return a `tea.Msg` (in `msg.go`)
when they finish; `Update` only ever mutates state from messages. This is what
structurally removes the draw-race class of bug.

### The one abstraction: `db.Engine`

Keep it deliberately small. Everything the TUI needs, nothing it doesn't.

```go
type Engine interface {
    // Introspection
    Tables(ctx) ([]Table, error)              // flat list for the connected DB;
                                              //   Table carries its schema name
    Columns(ctx, t TableRef) ([]Column, error)
    PrimaryKey(ctx, t TableRef) ([]string, error) // cols; empty => read-only view
    Databases(ctx) ([]string, error)          // deferred database picker (§12);
                                              //   unused by the v1 sidebar

    // Data
    Query(ctx, sql string, args ...any) (*ResultSet, error)  // arbitrary SELECT
    Exec(ctx, sql string, args ...any) (affected int64, err error)

    // Dialect hooks used by sqlgen
    QuoteIdent(s string) string      // "col" vs `col`
    Placeholder(i int) string        // $1 vs ?
    UsesSchemas() bool               // pg=true, mysql/sqlite=false
    // Case-insensitive, any-type LIKE predicate for a column filter (§7.1).
    // e.g. pg: LOWER("col"::text) LIKE LOWER($1)
    FilterPredicate(quotedCol string, placeholderIdx int) string
    Close() error
}
```

`ResultSet` carries column names, `[][]any` rows, and — crucially — enough
provenance (source table + PK column indices, when the query was a plain
single-table select) to decide whether the grid is **editable** (§8).

`Column` is enriched beyond name/type so sqlgen can generate correct inserts,
duplicates, and edit annotations without guessing. This metadata is fetched once
per table and reused everywhere:

```go
type Column struct {
    Name          string
    Type          string
    Nullable      bool
    PrimaryKey    bool
    AutoGenerated bool   // serial / identity / auto_increment / sqlite rowid alias
    HasDefault    bool
    Default       string
    Unique        bool   // part of some unique constraint (PK or otherwise)
}
```

Per-engine sources for the tricky fields:
- **Postgres:** `information_schema.columns` — `is_identity`, and `column_default`
  matching `nextval(...)` for `serial`.
- **MySQL:** `EXTRA = 'auto_increment'`.
- **SQLite:** `PRAGMA table_info` plus detecting the `INTEGER PRIMARY KEY` rowid
  alias.

---

## 7. UI & navigation

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

- **Single-line header** at the top: `connection > database > table`.
- **Sidebar is on-demand.** Hidden by default; the results pane is the star.
  The switch-table loop is `H` → `j`/`k` or `/` to find it → `Enter`: `H` brings
  the sidebar back **focused**, `Enter` loads the table and the sidebar
  auto-hides, focus back on the grid.
- **Flat, filterable table list — no tree.** jsq opens straight into the database
  named by the connection; the sidebar is a single flat list of that database's
  tables, and `/` filters it. No database/schema tree, no expand/collapse. Names
  are schema-qualified (`sales.orders`) only for non-default schemas; tables in
  the default schema (`public`) show bare names.
- **Switching databases is deferred (§12).** v1 stays on the one configured
  database. A later *database picker* will let you jump to another DB on the same
  host/user/pass, reusing the connection's credentials.
- **Single result pane, no tabs** (§7.2). Each query replaces what's shown;
  `Ctrl-r`/`Ctrl-o` bring back past queries.
- **Minimal borders**, and a scroll-position hint while rows remain unloaded.

### Results grid (fixed-width)

The grid is intentionally simple — this is the constraint that made bubbletea
the right framework (§3). No auto-sizing, no wrapping, no variable widths.

- **Every column has a fixed width.** Default from a per-type heuristic (e.g.
  ints narrow, `text`/`varchar` capped at N), values truncated with `…` to fit.
  Optionally widen/narrow the focused column with a key later; not v1-critical.
- **Cursor is a single cell** (`cursorRow`, `cursorCol`); the selected cell is
  highlighted. `h/j/k/l` move it.
- **Scrolling is by slice, not geometry.** `rowOff`/`colOff` track the top-left
  visible cell; after any cursor move, nudge the offsets to keep the cursor in
  view. Horizontal scroll steps whole columns (trivial because widths are fixed).
- **No pages — continuous lazy scroll.** There are no page-flip keys and no
  page-size setting. Rows are fetched in a **window sized to the visible
  viewport**, recomputed automatically on `tea.WindowSizeMsg` (terminal resize) —
  never set by hand. `j`/`k` scroll; crossing the loaded edge fetches the next
  window (keyset on the active sort key — the `ORDER BY` column from `J`/`K`, or
  the PK by default; see §7.3) and extends the buffer, so it feels like one long
  list. `g`/`G` jump to the first / last row — `G` fetches the tail window
  directly (reverse sort-key + limit) rather than loading everything. The buffer
  stays a single contiguous window that slides/extends as you scroll.
- **No total row count.** The status line shows the cursor's absolute row and a
  `(more↓)` hint while rows remain unloaded — never `n/total`. jsq never runs
  `COUNT(*)`, so opening or filtering a huge table is always cheap.
- **Truncation is rune-width aware** (`lipgloss.Width` / `go-runewidth`), the one
  non-trivial helper.
- **NULL vs empty rendering.** A SQL `NULL` renders as a **dimmed `NULL`**; an
  empty string renders as a **blank cell**; a literal string `"NULL"` renders as
  **normal (undimmed) text** — so all three are visually distinct. (The literal
  `"NULL"` case is rare, but the color makes it unambiguous.)
- **Control chars are glyphed, cells stay one line.** At render time, `\n`/`\r`/
  `\t` are replaced with visible glyphs (`↵`, `→`) so a multi-line or JSON-blob
  value can never spill across terminal rows and break alignment. The real,
  untruncated value (with actual newlines) is available via the `Enter` cell
  viewer (§7 grid) — this is purely a rendering concern.
- **The header row is a row of per-column filter slots** (see §7.1). It's fixed
  vertically (never scrolls with the body) and scrolls horizontally in lockstep
  with the body via the same `colOff`. A header cell shows the column name
  normally, or the active filter pattern (styled distinctly) when that column is
  filtered. The header is *not* part of the table body.

State sketch (the whole grid Model is ~this plus Update/View):

```go
type grid struct {
    cols            []Column        // name + fixed Width
    rows            [][]string      // pre-stringified cells for the loaded window
    winStart         int            // absolute row index of rows[0] (sliding window)
    cursorR, cursorC int
    rowOff, colOff   int            // top-left visible cell (within the window)
    h, w             int            // viewport size from tea.WindowSizeMsg; h also
                                    //   sets the fetch-window size (auto on resize)
    editable         bool           // §8 — false disables e/E/o/D/p
    filters         map[int]string  // colIndex → LIKE pattern; AND-ed together
    filtering        int            // colIndex being edited, or -1
    sortCol          int            // ORDER BY column (default: PK); the scroll key
    sortAsc          bool           // J = asc, K = desc
    input            textinput.Model // active header-slot editor
}
```

### 7.1 Column filters (`/`)

`/` filters the column under the cursor. It's a two-phase, fzf-like flow: a live
client-side preview of the loaded rows while you type, committed to a real
server-side query on `Enter`. Active filters live in the header cells.

- `/` puts the current column's header cell into edit mode (a `textinput`
  in-place), pre-filled with that column's existing pattern if any.
- What you type is the `LIKE` pattern **verbatim, bound as a parameter** — you
  supply the wildcards. `smith` matches only `smith`; `%smith%` matches contains.
- **Live preview while typing (client-side, no round-trip).** As you type, jsq
  narrows the *currently loaded rows* in place using the **same LIKE semantics**
  (case-insensitive, `%`/`_` wildcards, matched against the text form of the
  column). This doubles as in-grid "find" (C2): if the value is already on screen
  you see it instantly. Preview and committed result use identical matching, so
  what you see while typing is what `Enter` will give you.
- **Arrow keys navigate the preview** without leaving the filter. While the
  input is active, `↓`/`↑` move the row cursor through the live-narrowed rows
  (arrows, not `j`/`k`, because those are literal text in the input) — type to
  narrow, arrow to land on a row, no server round-trip needed.
- **`Enter` commits the filter (server-side).** It runs the real
  `FilterPredicate` across the *whole* table (not just the loaded window),
  updates the header slot, and scrolls the filtered set. **`Esc` clears** that
  column's filter (reverts the header cell to the column name); empty pattern +
  `Enter` also clears. There is no "cancel edit keeping the old value" — `Esc`
  always means "this column is now unfiltered."
- **Filters stack.** Every column with a pattern contributes one predicate,
  all `AND`-ed, into the re-query. Filter three columns → three `AND`s.
- **Always case-insensitive, works on any column type.** jsq builds each
  predicate through a dialect hook so `LIKE` applies to a lower-cased text form
  of the column regardless of its real type (§6):
  - Postgres: `LOWER("col"::text) LIKE LOWER($n)`
  - MySQL: `LOWER(CAST(`col` AS CHAR)) LIKE LOWER(?)`
  - SQLite: `LOWER(CAST("col" AS TEXT)) LIKE LOWER(?)`
- The filtered result is still a single-table select, so the grid **stays
  editable** (PK provenance preserved) and the continuous scroll simply restarts
  from the top of the filtered set.
- Header cells render the pattern in an accent style (e.g. `⌕ %smith%`) so a
  filter is visually distinct from a plain column name; long patterns truncate to
  the fixed column width like everything else.
- No clear-all in v1 — clear filters one column at a time with `Esc` on each
  (per-column `Esc` is the only path; keeps the surface small).

### 7.2 Result pane & query history

There is **one result pane**, not tabs. Every query (a table open, an `s`/`S`
free-form query, an applied filter) *replaces* what's shown. No tab bar, no tab
management — the query, not the result, is the thing you keep and re-run.

- **History is a session-scoped, in-memory ring** of every statement jsq runs —
  **reads and mutations alike**. No disk persistence in v1. Re-running always
  re-executes against the live DB (fresh rows, not a cached snapshot).
- **`Ctrl-r` — history picker.** Overlay list of past statements, newest first,
  each showing its SQL truncated to width. `j`/`k` move, `Esc` closes.
  - **`Enter` is type-aware:** a read re-executes and replaces the pane. A
    **mutation** does **not** run — it opens in nvim (the §8 full path) for you to
    review and `:wq`. This keeps the invariant that mutations never fire from a
    list.
  - **Classification errs safe.** Only a statement whose leading keyword (after
    stripping comments/whitespace) is `SELECT` — or a read-only introspection verb
    like `SHOW`/`EXPLAIN`/`PRAGMA` — counts as a read. **Everything else, including
    any `WITH …` statement, is treated as a mutation** and opens in nvim rather
    than auto-executing. `WITH` is explicitly not auto-run because a data-modifying
    CTE (`WITH x AS (…) DELETE …`, valid in Postgres) also leads with `WITH`; the
    small cost is losing one-tap re-run for read-only CTEs.
  - **`E`** opens the selected entry (any type, including a read) in nvim to tweak
    before running — you usually want a *variation* of an old query, not the exact
    one.
- **`Ctrl-o` — step back.** Re-run the previous *read* query without opening the
  picker (vim-jumplist feel); repeatable to walk backwards. Skips mutations.

### 7.3 Sorting (`J` / `K`)

`J` / `K` order the whole result by the column under the cursor — `J` ascending,
`K` descending. Server-side re-query, then continuous scroll from the top of the
sorted set. (`J`/`K` sit next to the `j`/`k` you move with.)

- **Single active sort column.** A new `J`/`K` on another column replaces the
  previous sort (not multi-column in v1); `J` then `K` on the same column just
  flips direction.
- **The sort column *is* the scroll key.** Keyset scroll (§7 grid) follows the
  active `ORDER BY`, tiebroken by the primary key so it stays deterministic when
  the sort column has duplicates: `ORDER BY <col> <dir>, <pk> <dir>`.
- **Default order** (before any `J`/`K`) is by primary key / rowid — the stable
  key the scroll needs anyway.
- **Header shows direction** — a `▲` (asc) / `▼` (desc) marker on the sorted
  column's header cell, alongside any filter pattern.
- **Composes with filters** — the `ORDER BY` applies to the filtered set; sorting
  is a read, so it's available on any grid (editable or not).

### Modes (vim modal)

| Mode       | Purpose                                     | Enter / exit          |
|------------|---------------------------------------------|-----------------------|
| **Normal** | Navigate sidebar + grid                     | default; `Esc` returns|
| **Filter** | Sidebar: live list filter · Grid: column filter slot (§7.1) | `/` … `Enter` apply / `Esc` clear |

There is **no `:` command mode** — nothing minimal about a command parser for one
or two actions. There's also no in-app SQL "insert mode": editing suspends jsq
and drops you into `$EDITOR` (§8). So the only modes are Normal and Filter, plus
transient overlays (cell-edit input, full-cell viewer, history picker, help).

### Keybindings (draft — keymap.go is the single source of truth)

Global:
- `?` help — full-screen overlay listing **all** shortcuts, grouped
  (Navigation / Sort / Filter / Edit / Query / History / Global), rendered from
  `keymap.go` via `bubbles/help` so it can never drift from the real bindings;
  `?` or `Esc` closes · `Ctrl-c` quit · `Tab`/`Shift-Tab` cycle focus
- `H` toggle sidebar — shows *and focuses* it; auto-hides after you pick a table
  · `R` refresh current view
- `Ctrl-r` query-history picker · `Ctrl-o` step back to previous read (§7.2)

Navigation (sidebar + grid, vim-style):
- `h j k l` move · `g`/`G` top/bottom (deliberately single-key `g`, not vim's
  `gg` — jsq has no doubled-key commands)
- grid: `0` first column · `$` last column (mirror of `g`/`G` for rows;
  `colOff` scrolls to bring the target column into view)
- `/` filter — sidebar: filter the table list · grid: edit the current column's
  filter slot (§7.1)

Results grid:
- `Enter` — sidebar: load the selected table · grid: **inspect the full cell
  value** in a read-only, scrollable popup (untruncated; pretty-prints JSON when
  the value parses as JSON; `Esc` closes). This is the read path for long
  TEXT/JSON/blob cells — never the edit path (`e`/`E`).
- `y` yank current **cell** to the system clipboard as its **raw value** (no
  quoting/decoration — `ada@x.io`, or an empty string for NULL)
- `Y` yank current **row** to the system clipboard as a **JSON object** keyed by
  column name (`{"id":1,"name":"Ada","email":"ada@x.io"}`; SQL `NULL` → JSON
  `null`, numerics unquoted, everything else a JSON string)
- No page keys — the grid is a **continuous lazy scroll** (§7 grid): `j`/`k`
  scroll (auto-fetching more at the edges), `g`/`G` jump to first/last row. The
  fetch window = viewport height, updated automatically on terminal resize.
- `J` / `K` sort by the current column **ASC / DESC** (server-side re-query;
  header shows `▲`/`▼`; the sort column becomes the scroll key — §7.3)

Query (free-form SQL — moved off `e`/`E`, which now edit cells):
- `s` → open `$EDITOR` with a scratch SQL buffer; `:wq` runs it → result pane
- `S` → open `$EDITOR` pre-filled with `SELECT * FROM <current table> LIMIT 100;`

Editing (only when the grid is editable — §8):
- `e` edit current cell — **quick single-line overlay** (fast path)
- `E` edit current cell — **full nvim** buffer with the generated `UPDATE`
- `o` insert blank row · `D` delete row
- `p` duplicate the current row → generated `INSERT` opens in nvim (same table).
  No yank step; `p` acts on the row under the cursor.

---

## 8. Editing model

Goal: keep mutation power without lazysql's change-tracking machinery. The model
has **no pending-changes buffer** — each mutation is a single statement that is
either previewed in nvim (full path) or shown in the status line after it runs
(quick path). No batching, no staged diff, no commit step.

### Editability rule

A grid is editable **only** when its rows came from a single-table select and
jsq resolved a primary key (or a unique row identity). Otherwise the grid is
read-only and edit keys are inert (status line says why). A connection flagged
`read_only = true`, or connected via an ad-hoc URL you didn't mark writable,
disables all mutation regardless.

### Mechanic (decided): two-speed editing

Everything that isn't a trivial single-value tweak generates SQL and hands it to
nvim (`:wq` runs, quit-without-save aborts). Single-cell edits *also* get a fast
in-app path, because round-tripping through nvim to change one value is too heavy.

So there are exactly two speeds:

**Full path — generate SQL → nvim → `:wq`.** jsq builds the statement from the
current row and opens it in nvim. Fully vim-native, self-documenting (you always
see the exact SQL), zero in-app editing widgets beyond the one below. jsq
produces a *good starting point* and annotates the traps; the human is the
conflict resolver, and nvim is where they resolve. Used by:

- `E` on a cell → the generated `UPDATE`, for long/multi-line/JSON values you
  want a real editor for:
  ```sql
  UPDATE "users" SET "email" = 'ada@x.io'   -- edit this value
  WHERE "id" = 1;
  ```
- `o` → a blank `INSERT` skeleton: all insertable columns with NULLs / defaults.
- `D` → the `DELETE … WHERE pk = …;`, confirmed by `:wq`.
- `p` → duplicate: an `INSERT` pre-filled from the current row (below).

**Quick path — inline overlay.** `e` on a cell opens a single-line input overlay
anchored on that cell, pre-filled with the current value. `Enter` builds the
keyed `UPDATE` and runs it immediately; `Esc` cancels. No modal, no nvim — the
speed *is* the point. It's a single PK-keyed update (low blast radius), and jsq
reports the affected-row count in the status line afterward, so you still get
confirmation of what happened even though you didn't pre-view the SQL. For
anything the overlay can't comfortably hold (newlines, big text, JSON), use `E`.

This is the only in-app text-input widget in jsq. Everything else is nvim.

**NULL vs empty in the overlay.** The overlay pre-fills with the cell's text
(blank for an empty string; a NULL cell shows a dim `NULL` ghost, input empty).
`Enter` on typed text sets that value; **`Enter` on empty input sets an empty
string `''`** — "empty is empty". A bare `Enter` on an *untouched* NULL cell is a
no-op (stays NULL), so you can't blank it by accident. **Setting a value *to*
`NULL`** is done via `E` (write `NULL` unquoted) — deliberately routed to the
explicit path since it's rare. The status line echoes what ran (`set email = ''`
vs `set email = NULL`) so the outcome is never ambiguous.

### Duplicating a row (`p`)

Duplicate is just *insert, pre-filled from an existing row* — the same `INSERT`
path as `o`, seeded with the current row instead of blanks.

- **`p`** generates an `INSERT` from the row under the cursor and opens it in
  nvim; `:wq` runs it. There is **no yank step and no row register** — `p` always
  acts on the current row, **same table only**. (Dropping the register is what
  removes the old `y`/`yy` keymap ambiguity; the yank keys `y`/`Y` stay
  clipboard-only.)

jsq only produces a *good starting point*; nvim is where you finish it. It never
has to be clever about defaults, unique collisions, or composite keys.

**Auto-generated PK** (serial / identity / `AUTO_INCREMENT` / sqlite rowid) — the
PK column is omitted so the DB assigns a fresh one:

```sql
-- jsq: duplicate of "users" (id=1). "id" omitted — auto-assigned.
INSERT INTO "users" ("name", "email", "created_at")
VALUES (
  'Ada',                     -- name
  'ada@x.io',                -- email        ⚠ UNIQUE — change before :wq
  '2024-01-01 10:00:00+00'   -- created_at
);
```

**Natural / non-generated PK** — jsq can't drop it (the insert would collide), so
it keeps it and flags it as the value you must change:

```sql
-- jsq: duplicate of "countries" (code='US'). PK is not auto-generated — set a new value.
INSERT INTO "countries" ("code", "name")
VALUES (
  'US',              -- code   ⚠ PRIMARY KEY — must be unique, change this
  'United States'    -- name
);
```

Generator rules (shared by `o` and `p`):
1. **Trailing `-- colname` comment** on every value line so a wide `VALUES(...)`
   stays readable.
2. **`⚠ UNIQUE` / `⚠ PRIMARY KEY` annotations** on columns likely to collide,
   surfaced *before* `:wq` rather than as a post-hoc DB error. Driven by the
   `Column.Unique` / `Column.PrimaryKey` metadata (§6).
3. **Auto-generated columns omitted** from the column list and `VALUES` (driven
   by `Column.AutoGenerated`), so the DB fills them. Want a copied `created_at`
   to become `now()` instead? Delete that line — the column default applies.

After the `INSERT` runs, locate the new row via `RETURNING` (pg / sqlite) or
`LastInsertId` (mysql) and highlight it on refresh, so `p :wq` visibly lands you
on the clone.

### Safety rails

- Every generated statement has a `WHERE` keyed on the full PK — never a bare
  `UPDATE`/`DELETE` without a predicate.
- Show affected-row count after exec; if it's not exactly 1 for a keyed edit,
  warn loudly.
- No implicit transactions spanning multiple edits (no batch). Each statement
  autocommits. Simpler mental model; the DB is always in a consistent state.
- Arbitrary destructive SQL you *write yourself* in `$EDITOR` is your own
  responsibility — jsq runs what you `:wq`.

---

## 9. Data flow (a browse + edit cycle)

1. Startup: `jsq <name>` / URL / path connects directly; bare `jsq` shows the
   connection picker (§4) and connects on `Enter`. Then open `db.Engine` →
   introspect the connected database's tables.
2. Sidebar is a flat, filterable list of those tables. `Enter` on a table →
   `SELECT * FROM t` (first viewport window) → `ResultSet` with PK provenance →
   grid.
3. Scroll is continuous: rows load in viewport-sized windows via keyset (the sort
   key), fetched lazily at the edges — no discrete pages, window size tracks the
   terminal height. `/` on a column adds/updates that column's filter slot
   (§7.1); jsq rebuilds the `WHERE` as the `AND` of all active per-column
   `FilterPredicate`s, re-queries from the top, and scrolls the filtered set.
4. `s`/`S` → `$EDITOR` → on save, run the buffer as a query → replaces the result
   pane; the statement is appended to the session history ring (§7.2).
5. Mutations → sqlgen builds the statement → §8 mechanic → `Exec` → reload the
   current scroll window → show affected count. Quick path: `e` overlay →
   immediate keyed `UPDATE`. Full path: `E`/`o`/`D`/`p` → nvim → `:wq`.

---

## 10. Configuration

- `~/.config/jsq/connections.toml` — connections (§5), app-read-only.
- Env: `$JSQ_CONFIG` (file location), `$EDITOR`, and each connection's own
  password var (the `env = ...` key in §5). No `$JSQ_CONN` / default — connection
  is chosen by CLI arg or the picker (§4).
- No other config in v1. Keybindings are compiled in; a keymap override file is a
  possible later addition (§12).

---

## 11. Build & distribution

- `go build -o jsq .` → a single static binary. No cgo (SQLite is pure-Go via
  `modernc.org/sqlite`), so cross-compilation is trivial.
- A `Makefile` / `justfile` with `build`, `run`, `install` targets is enough to
  start; a release pipeline can come later if there's demand.

---

## 12. Deferred (post-v1)

Deliberately left out of v1, but noted as natural extensions:

- **Column width adjust** — a key to widen/narrow the focused column (§7 grid).
- **Disk-persisted history** — v1 history is in-memory/session-only (§7.2).
- **Database picker** — switch to another DB on the same host/user/pass without
  re-editing the connections file, reusing connection params (§7). `Databases()`
  is already in the `Engine` interface for this.
- **Vertical row-detail view** — show the current row as a `column: value` list
  for very wide tables. v1 relies on the cell viewer (`Enter`) + `0`/`$`/`h`/`l`.
- **Cross-table duplicate** — `p` duplicates the current row, same table only (§8).
- **Filter stacking as OR / clear-all** — v1 stacks filters as `AND`; per-column
  `Esc` clears (§7.1).
- **Keymap override file** — bindings are compiled in for v1 (§10).

---

## 13. Relationship to lazysql

jsq owes its shape to [lazysql](https://github.com/jorgerojas26/lazysql) — the
idea of a fast, keyboard-driven database TUI, and much of the interaction feel.
Where jsq differs is scope: it deliberately trades breadth for a smaller, more
opinionated surface, and pushes text-editing work out to `$EDITOR` instead of
reimplementing it. Concretely, jsq leaves out (by design, not omission):

- **Connection manager UI** — connections live in a hand-edited, app-read-only
  file (§5); there's no in-app add/edit/delete.
- **Built-in SQL editor / completer / lexer / syntax highlighting** — SQL is
  authored in your real `$EDITOR` (§7 `s`/`S`, §8).
- **Staged pending-changes buffer + commit/rollback** — jsq runs immediate,
  primary-key-scoped, confirmed single statements instead (§8).
- **Persistent saved queries / on-disk history** — history is an in-memory,
  session-only ring (§7.2).
- **Ancillary UI** — CSV export, standalone JSON viewer, results-table menus,
  MSSQL, and multi-step connection screens are all out of scope for v1.

The result is a tool that does less, in fewer keystrokes, with a codebase small
enough to keep entirely in your head. If you want the full-featured experience,
use lazysql — it's excellent.

