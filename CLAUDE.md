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

README's design section names `sqlgen`/`editor`/`keymap`/`meta` units. Only
`sqlgen.go` exists; the `$EDITOR` spawn is inline in `cmd.go` and keybindings are
hardcoded in `app.go` (no `keymap`/`meta` unit). What's really here:

```
main.go                     # flag parse (-c + one positional), resolve conn, boot tea
internal/config/config.go   # load connections.toml, read-only (url, cmd, safe)
internal/db/
  db.go                     # Engine interface (Tables/Columns/PrimaryKey/ForeignKeys/…) + Open() dispatch + shared scanQuery + stdEngine base (Query/Exec/Close over *sql.DB, embedded by each engine) + openStd/namesToTables + DSN/HostPort helpers
  sqlite.go postgres.go mysql.go   # one Engine impl each
internal/tui/
  app.go        # root App Model. Screens: screenPicker (bare `jsq`, or Backspace from the table list), screenTables (full-screen table list, Backspace from the grid), screenDatabases (database list, `d` → switchDBCmd reopens the engine on another db), screenBrowse (grid). The three list screens (`connList`/`sidebar`/`dbs`) share one `sidebar` component and route keys the same way: `handlePickerKey`/`handleTablesKey`/`handleDatabasesKey` each run `sidebarFilterEdit` while `s.filtering` (text-edit + arrows), else nav mode where `/` starts the filter and a bare letter is NOT a filter (so screen commands like `d` stay live); `sidebarNav` is the shared arrow/Ctrl-NP mover. **Screen navigation is a left↔right chain: Connections → Tables → Grid** (databases hang off Tables via `d`). Backspace is the ONLY way left — there are no `t`/`T`/`c` jump-to-screen shortcuts (they were redundant with the chain). `Enter` moves right (connect / open table / open cell); `Backspace` moves left (grid→tables→picker; databases→tables; the picker is leftmost so its Backspace is a no-op). `Esc` never changes screens — it only clears an active list/column filter (and, via the global handler, kills an in-flight op or cancels a connect). **The overlay commands (`?` help, `` ` `` jumplist, `b` history) and the jumplist steps (`Ctrl-o`/`Ctrl-i`/`Tab`) are screen-independent** — they inspect session-wide state, so handleKey runs them before the screen switch, gated only on `App.typing()` (true while the grid's quick-edit/column filter or a list's `/` filter owns the keyboard, so a letter meant for a filter stays literal). Their overlays must therefore be rendered in `View()` **before** the screen switch (like confirm/errView) — drawing them in `browseView` would make them openable on a list screen but invisible. Only `cellview` stays grid-only (it shows the cell under the grid cursor). ALL key routing (hardcoded — no keymap.go), layout, View. `begin(label)`/`stop()` drive the top-right activity indicator: begin cancels any prior op, bumps a monotonic `gen` token, stores a `context.CancelFunc`, and hands the cancellable ctx to the dispatched DB cmd; a terminal msg (or Esc, or a new begin) calls stop. Each DB cmd stamps its result msg with the `gen` it was dispatched under; `Update` drops any result whose `gen` no longer matches `a.gen` (`App.stale`) — so a superseded op that finished late can neither cancel the current op nor apply its rows over it. Non-op msgs (connect/editor errors) carry `gen 0` and are never stale. A perpetual `tickCmd` (started on connectedMsg) animates the spinner and idles invisibly when `activity==""`.
  pane.go       # the `pane` struct + splits. A pane is one independently-navigable view: grid, currentTable, basePreds/baseNote, sort, adHoc/adHocQuery, its OWN jumplist (views/viewIdx), and its layout rect. App holds `panes []pane` + `focus`; `p()`/`g()` are the focused-pane accessors and `paneByID` resolves a stable id. `<space>` is a leader (App.leader): `v` = clonePane → insert right + focus it, `q` = close, `h/j/k/l` = focusDir, which moves by **rect geometry** not pane order (so `<space>s` horizontal splits only need layoutPanes to change). **clonePane/grid.clone deep-copy rows (outer), visible, filters/filtersWide, and views** — every one is mutated in place, so sharing corrupts: appendRows appends past len into a shared backing array (rows arrive from scanQuery with spare cap), and a clone starts at an identical viewIdx so a shared `views` would have the first navigation clobber the other pane's history. Inner `[]any` rows ARE shared on purpose (applyEdit writes row[col] in place; one connection, one row → an edit should show in both). NOT implemented via restore(snapshot()) — that path shares rows.
  cmd.go        # tea.Cmd constructors — the ONLY place db.Engine is called; also $EDITOR spawn (editorCmd). Each DB cmd takes a ctx (App.begin); dbErr() swallows a cancelled ctx to a nil msg. tickCmd drives the header spinner. yankCmd (y/Y) copies to the clipboard via an OSC 52 escape (go-osc52) written to stderr — no external binary, works over SSH; not a DB cmd.
  sqlgen.go     # SQL-text generation for the $EDITOR full paths (buildUpdateStmt E, buildInsertStmt o, buildDeleteStmt D, buildDuplicateStmt p; renderInsert shared by o/p) + s helpers (selectTemplate, isReadSQL)
  msg.go        # tea.Msg types (connectedMsg, rowsMsg, moreRowsMsg, editDoneMsg, editorSubmitMsg/AbortedMsg, execDoneMsg, errMsg)
  proc.go       # the connection `cmd` helper (port-forward etc.): startRun (registers in a package-level live set), waitPort, runProc.kill (deregisters + bounded group-kill), KillRunHelpers (exit backstop), tailBuffer. proc_unix.go/proc_other.go = process-group kill (unix) vs single-process fallback. The wait address comes from db.HostPort(url).
  grid.go       # fixed-width grid Model: cursor, scroll, sort marker, filter, e-edit overlay, fullEditTarget. yankCell (raw cell text)/currentRowJSON (column-ordered JSON) feed the y/Y clipboard yank.
  sidebar.go    # full-screen list Model, laid out as a column-major grid sized to the widest name (multi-column on wide screens; ↑↓/j-k = ∓1, ←→/h-l = ∓rows, g/G to the ends). Two modes: navigation (default) and a `/`-triggered filter (`filtering` flag; type to narrow live via `filterPatterns` — prefix then substring; Enter opens the match, Esc clears). Used for the table list (screenTables), the database list (`a.dbs`, screenDatabases — items are `db.Table{Name: db}`), AND the connection picker (`a.connList`, screenPicker — items are `db.Table{Name: conn}`, Enter maps back to a `config.Conn` via `findConn`). `label` is the search placeholder. (No separate picker.go — it was folded in here.)
  cellview.go   # read-only full-cell viewer (Enter); pretty-prints JSON
  histview.go   # query-history buffer overlay (b): histEntry + histView list; badge (row/affected count, `+` on a LIMIT hit) + snippet renderers
  confirm.go    # safe-mode (connection safe=true) "run this mutation?" y/n overlay
  errview.go    # failed-statement modal (errView): full engine error + the query; e/Enter reopens it in $EDITOR, y yanks the error. Armed by errMsg{seed}, rendered in View() before the screen switch (like confirm — armable from any screen)
  help.go       # read-only `?` keybinding cheat sheet (full-area overlay like cellview); helpItems is the hand-kept mirror of the hardcoded keymap
  util.go       # clamp()
```

**Connect flow** (one `connectCmd(config.Conn)` in cmd.go, dispatched by `Init`
and picker Enter): a single tea.Cmd starts the `cmd` helper (if any), waits for
`db.HostPort(url)` to accept a TCP connection (`waitPort`, 30s), then opens the
engine and lists tables — all off the Update loop. The port wait only runs when
`cmd` is set (the tunnel needs a moment); a plain connection skips straight to
`db.Open`. There is **no** per-process state on the model and no `runStartedMsg`
handshake: `startRun` registers every helper in a package-level `liveProcs` set
the instant it launches, and `main` `defer`s `KillRunHelpers()` to reap them on
*any* quit path. That's what makes an early Ctrl-C safe — cleanup never depends
on a bubbletea message being folded into the model first (the bug that a
model-owned `App.runProc` had). `runProc.kill` deregisters then kills the whole
**process group** (unix) so a shell that forks a port-forward takes its children
with it — and because output is pipe-captured, killing only the direct child
would otherwise hang `Wait` on the still-open pipe.

While a `cmd`-backed connect is in flight, `App.connCmd`/`connAddr` drive a
full-screen `connectingView` loader (naming the command + the port), animated by
the one spinner tick loop (`ticking`/`ensureTick` guard against a second loop);
`beginConnect` arms it, `connectedMsg`/`errMsg` clear it. The picker Enter is
guarded by `a.pending.URL != ""` so a second Enter during a slow connect can't
spawn a duplicate engine/helper. `New` takes the resolved `config.Conn`
(`pending`).

**Follow foreign keys** (`Enter` on an FK column, `App.follow`): resolution is
**in-memory** — the
FKs come with the load (`loadCmd` best-effort-fetches `Engine.ForeignKeys` into
`ResultSet.FKs`, per-engine: sqlite `PRAGMA foreign_key_list`, pg
`pg_constraint`+`generate_subscripts`, mysql `key_column_usage`; `grid` keeps
them and `grid.fkFor(col)` looks up the covering FK). `follow` builds `eqPred`s
`refCol = <row value>` from the current row (composite-key aware) and navigates
**synchronously** (no engine call in Update — the only DB work is `loadView`'s
reload). The referenced table opens as a normal editable/sortable single table
with those preds in `App.basePreds` (human form in `baseNote`, shown in status),
AND-ed into every load via `whereClause`/`loadCmd`/`loadMoreCmd` and cleared by
`selectTable`. NULL cells / non-FK columns just set a status line. FK columns are
flagged in the header with `fkMarker` (`→`). Note grid cell values are
driver-typed (sqlite ints come back `int64`, not string) — fine as bound params.

**Connections & databases**: Backspace walks left to the table list then the
picker (`connectTo` on Enter); `d` opens the database list from either the grid or
the table list. Switching connection/database reconnects
the engine via `openEngineCmd` (closes the old engine, opens a fresh one on the
target DSN, `db.WithDatabase` for a db swap). Multiple connections stay "open"
because their `cmd` tunnels persist in `liveProcs` for the whole session;
`App.tunneled[name]` records which have started theirs so a re-select doesn't
re-run the `cmd` (openEngineCmd's `startTunnel`). `connectTo` reuses the current
connection (no-op → its tables), else reconnects. Initial connect still uses
`connectCmd` (quits on failure); mid-session `openEngineCmd` uses `errMsg`.

**Jumplist**: one **session-wide** list (`App.views`, oldest→newest, `viewIdx` =
current); a `viewState` is `{conn, db, table, basePreds, baseNote, sort, pos}`
plus an optional cached `*gridSnapshot` (`snap`) of its loaded rows/grid state
and an LRU `seq`. `pos` (grid cursor + scroll) makes a jump land exactly where you
left. `syncCurrent` captures both `pos` and a fresh `snap` off the live grid
before any move (skipped while `adHoc` is on screen — the grid then holds a scratch
query, not the table, so the cached table snapshot is preserved). `loadView`
**restores from `snap` instantly** (no DB call, `r` refreshes if stale) when one is
resident; otherwise it reloads and sets `App.pendingPos` so the `rowsMsg` handler
repositions to `pos` once rows arrive (`loadViewCmd` widens the fetch window if the
saved cursor sits past the default one). Snapshots are bounded: `evictSnaps` keeps
only the `maxCachedViews` (16) most-recently-touched `snap`s, dropping older ones
to metadata (which still reload+reposition, just not instantly). Committed column
filters ride in `snap` but are lost on eviction. Snapshot slices are shared by
reference (an in-place cell edit stays reflected in the cache); `visible` and the
`filters` map are deep-copied so the live grid can't corrupt a stored snapshot.
It **spans connections and databases** — each view records its `conn`+`db`, and a
jump elsewhere reconnects first: `jumpBy`/`jumpTo` route through `goToView`, which
(on a conn or db mismatch) stashes `App.pendingView` and dispatches
`openEngineCmd`; `connectedMsg` then `loadView`s `pendingView` (restoring its
`snap` or reloading) instead of the table list. So `connectedMsg` must **not**
reset `views` — only the live table state. Same-place jumps just `loadView`.
`navigate` (selectTable/follow) `syncCurrent`s, truncates the forward tail,
appends. `Ctrl-O`=`jumpBy(-1)`; forward `jumpBy(+1)` is `Ctrl-I` (where the
terminal distinguishes it from `Tab`) or `Tab` while browsing the grid (Tab there
was a no-op; terminals send `Ctrl-I` as `Tab` on bubbletea v1). The `` ` `` picker
(`jumpView`, db-qualified labels) is the terminal-proof way to reach any view.

The `$EDITOR` full path (`E`/`o`/`D`/`p`/`s`): the generators
return an `editorSeed` (SQL + cursor line/col + `selectKind`); `editorCmd` seeds a
temp file and spawns `$EDITOR` via `tea.ExecProcess`. `E` and `D` build their seed
inline (`buildUpdateStmt`/`buildDeleteStmt`, from in-memory grid PK data); `o` and
`p` need enriched column metadata so they go async — `prepareInsertCmd` /
`prepareDuplicateCmd` fetch `Columns()` then `buildInsertStmt`/`buildDuplicateStmt`
(the latter seeded with the row's captured values), returning an `editorReadyMsg`
that `Update` turns into `editorCmd`. (`Columns()`
now also populates `Column.Unique` per engine via a unique-index/constraint query
(`uniqueColumns` on each engine — mysql uses `information_schema.statistics
non_unique=0`, NOT `column_key`, so every column of a composite `UNIQUE(a,b)` is
flagged, not just the leading one); it's only called for insert prep, not on every
table open.) For vim-family editors
(`isVimFamily`), `positionArgs` adds `+call cursor(...)` and a `feedkeys` (not
`:normal`, which drops the selection) so the editor opens with the value
selected — `vi'` inside a string's quotes, `v$` for a NULL/number token. On exit
`editorResult` decides submit-vs-abort (mtime bump or content change → run;
cleared buffer or `:q!` → abort). The `editorSubmitMsg` handler then classifies
the SQL (`isReadSQL`): a **read** (`s` SELECT-likes) runs via `runQueryCmd` →
`queryResultMsg`, shown in the grid with `adHoc=true` (no table provenance → not
editable, and J/K/`/` are guarded off since they'd re-query the table); a
**write** (E/o/D/p, or an `s` mutation) runs **verbatim** via `execRawCmd`, then
the view reloads. Note WITH counts as a write. This is the deliberate
exception to invariant 5 — full-path SQL is user-authored and inlined
(`sqlLiteral`), not parameter-bound. Both `runQueryCmd`/`execRawCmd` (and the
quick-path `execEditCmd`) carry an `editorSeed` down so a **failure** returns
`errMsg{seed}` via `dbErrSeed` — the handler arms the `errview.go` modal (full
untruncated error + the query) instead of the one-line status; `e`/`Enter`
reopens the seed in `$EDITOR` to fix and re-run. The free-form seed keeps its
`remember`/`scratch` markers (so a re-run still continues the `s` loop); the
quick-edit seed is the equivalent `E` full-path UPDATE (`buildUpdateStmt`). A
seedless `errMsg` (connect/reconnect/load failure) still just sets the status.

**One op slot, routed by pane**: `begin(label, paneID)` records `App.opPane`; there
is exactly one in-flight DB op, so the App always knows whose result is coming.
Each gen-stamped handler goes **stale → stop → `opTarget()` → drop**, applying the
result to *that* pane rather than the focused one (focus can move while a query
runs) and dropping it if the pane was closed. Messages carry no pane field — a
pane id would imply concurrency we don't have. Ordering matters: unlike a stale
message, a pane-gone message owns the op slot, so skipping `stop()` would strand
the spinner and leave Esc pointing at a dead op. Consequence: `maybeLoadMore`'s
`activity != ""` guard is **global**, so a slow load in one pane stalls another's
scroll-fetch until it finishes — correct for one slot, but don't read that
function's comment as single-pane.

**Safe mode** (`App.safe`, from `config.Conn.Safe`, cached like the old
`readOnly` was — set alongside `connName` on every connection switch): when the
active connection has `safe=true`, both mutation dispatch points — the quick-path
`e` (`execEditCmd`) and the full-path write (`execRawCmd`) — first route through
`App.askMutation`, which arms `confirm.go`'s `confirmView` overlay (connection +
database + the SQL) instead of running. The `confirm` guard at the top of
`handleKey` runs the held command on `y` (any other key cancels). The overlay is
rendered in **`View()` itself** (before the screen switch), not `browseView` —
it's modal and can be armed from any screen (e.g. a write scratch `s` on the table
list), so it must draw regardless of `a.screen`. The quick-path
preview is `previewEditSQL` (values inlined for display only — the statement that
runs still parameter-binds them; not an invariant-5 exception). On the quick
path, typing exactly `NULL` (uppercase) in the `e` overlay sets SQL NULL:
`commitEdit` flags `editReq.null`, `execEditCmd` binds `nil` (not the string),
and `applyEditNull` shows the faint `NULL` in-grid. The literal string `"NULL"`
needs the `E` full path. Reads are never
gated. New mutation paths must funnel through `askMutation` when `a.safe`.

`s` (`App.scratchSeed`) prefills the `selectTemplate` for the current table, or
the remembered last query if one was run — `App.lastQuery` is keyed by
`App.queryKey` (conn+db+table, so a same-named table in another database/connection
doesn't inherit it), the seed's `remember` field (the table) rides through
`editorCmd`→`editorSubmitMsg`, and the submit handler stores the SQL back into
`lastQuery` (even on error) so the next `s` on that table continues the edit-run
loop. Only `s` sets `remember`; E/o/D/p leave it zero. `s` **from the table list**
(`App.blankScratchSeed`, screenTables) instead opens an empty buffer headed by a
`-- conn · db` comment (no table template): `remember` is zero (nothing to key a
last-query against) but `scratch=true` rides through so the submit still files it
in the connection's `b` history (`editorSubmitMsg`'s `else if msg.scratch`).

**Query history** (`b`, `App.history`): a per-connection, most-recent-first,
SQL-deduped list of the free-form (`s`) queries. `recordQuery` (called from the
`editorSubmitMsg` handler when `remember` is set — so E/o/D/p structured edits
never enter it) promotes a query to the front on submit; `recordQueryCount`
(from `queryResultMsg`/`execDoneMsg`, matched by `histKey` = whitespace-trimmed
SQL) fills in its last row/affected count once the run lands. `b` snapshots the
list into `histView` (an overlay like `jumpView`); Enter runs a read directly
(`runHist`, re-recording for recency) but opens a write in `$EDITOR` for review
(`histSeed`, `remember`=current table so a `:wq` re-records + continues the `s`
loop); `s` opens any entry in `$EDITOR`. History is in-memory only (no persistence).

`r` (`App.reloadView`) re-runs the current view: a table reload is just
`loadCurrentCmd` (keeps sort, `basePreds`, column filters, and cursor — `setResult`
re-clamps it); an adHoc result re-runs `App.adHocQuery`, the SQL stashed from the
last `queryResultMsg` (now carries its `sql`). No-op before anything is loaded.

**Continuous scroll (keyset)**: `loadMoreCmd` pages by a keyset cursor, not
`OFFSET`. `orderKeys(sortCol, sortAsc, pk)` defines the **total order** — the
sort column then every remaining PK column (tiebreakers, same dir), or the full
PK descending by default — and drives *both* the `ORDER BY` (`orderClauseKeys`,
used by `loadCmd` and `loadMoreCmd` alike) and the cursor. **Invariant: first
load and scroll must share that order**, so both go through `orderKeys` — don't
give one a different `ORDER BY`. `keysetWhere` builds `… AND (keyset cursor)`
from the anchor (`grid.lastRowMap()`, the last loaded row, passed down from
`maybeLoadMore`); `keysetCursor` emits the lexicographic `(k0>v0) OR (k0=v0 AND
k1>v1) OR …` expansion (`>` asc / `<` desc — handles mixed directions, unlike a
row-value tuple compare) with every value parameter-bound. It **falls back to
`LIMIT`/`OFFSET`** (the old path) unless `keysetEligible(sortCol, pk)` — i.e. all
ordering keys are **PK columns** (default PK sort, or an explicit sort on a PK
column). That gate is the NULL-safety guarantee: PK columns are NOT NULL, so the
order has no NULLs, so keyset can't skip a NULL group that an engine sorts to the
far end (they disagree on which end, and it flips with direction). A non-PK sort
therefore stays on OFFSET — correct, just not concurrency-stable. `filterPreds`
is the shared base+filter builder (threads the placeholder start index so the
cursor's params continue the numbering).

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
4. **jsq only ever reads `connections.toml`.** No write path. Keep it so.
5. **All panes share one connection+database+engine.** Every conn/db change goes
   through `allowSessionMove`, which refuses while `len(panes) > 1` — reopening
   the engine under the other panes would leave them rendering a closed database.
   This is what keeps `pendingView`/`connRestore`/`connectedMsg` provably
   single-pane (they may assume the focused pane is the only one). Lifting it
   needs per-pane engines (a refcounted registry keyed conn+db).
6. **Never hold a `*pane` across a mutation of `a.panes`.** An append may
   reallocate and the pointer then aims at the orphaned array, where writes are
   silently lost. Re-take `p()` after any append/remove (see splitVertical).
7. **Bind values as parameters** (`Placeholder(i)` + args) — filter patterns,
   quick-path edit values, PK predicates. Only identifiers are interpolated, via
   `QuoteIdent`. **Sole exception:** the `$EDITOR` full path runs user-authored
   SQL verbatim (values inlined by `sqlLiteral`) — that's the documented model,
   not a leak. New `$EDITOR`-authored statements follow it; anything jsq runs
   *without* the user seeing the SQL must be parameter-bound.

## Scroll/paging — intentional behavior, don't "fix"

- **`G` jumps to the loaded end, not a true tail-fetch.** `grid.bottom()` moves
  the cursor to the last loaded row (`maybeLoadMore` may then extend it); it does
  not fetch the table's actual last rows. Deliberate: a real tail window would
  need bidirectional scrolling (the buffer is a top-anchored prefix), and `K`
  already reaches the other extreme by flipping the sort. Leave it as is.

## Conventions when extending

- **New keybinding** → a case in the relevant `handle*Key` in `app.go`; if it does
  DB work, return a `tea.Cmd` from `cmd.go`. Update README's keybinding table.
- **New DB work** → `tea.Cmd` in `cmd.go` + `tea.Msg` in `msg.go`, handled in
  `Update`. Build SQL with `QualifiedName`/`QuoteIdent`/`Placeholder` so it stays
  correct across all three engines.
- **Per-engine differences live behind the `Engine` interface** — add a method
  there rather than type-switching on the engine in the TUI.
- **Filter semantics** (keep grid + sidebar identical): `filterPatterns(raw)` is
  the single source — an accurate prefix (`searchPattern`, trailing `%`) first,
  then a lazy substring (`%raw%`) fallback tried only when the prefix matches
  nothing AND the user typed no `%` of their own (a user-typed `%` disables the
  fallback). Case-insensitive, any type, via `FilterPredicate`. Client preview
  (`likeToRegex`) walks the same pattern list so preview == committed result. The
  grid decides prefix-vs-substring **once at commit** (`grid.needSubstring`, over
  the loaded rows) and stores it in `filtersWide[col]` so `filterSpecs` stays
  stable across keyset pagination (it rides in the snapshot next to `filters`).
- **Filter input editing** (`textField` in textfield.go): both the grid column
  filter (`grid.filter`) and the list filter (`sidebar.filter`) are `textField`s —
  a rune-indexed caret with insert/backspace/del/deleteWord/left/right/home/end,
  wired to `←`/`→`, `Home`/`End`, `Ctrl-a`/`Ctrl-e`, `Ctrl-w`, `Del` in
  `handleFilterKey` (grid) and `sidebarFilterEdit` (lists). Rendering is **not** on
  `textField` — the callers (grid `renderHeader`, sidebar `View`) and the quick-edit
  cell (`renderEditCell`) all go through the one shared `renderCaretField` (grid.go),
  so every text input draws the identical caret: a **reverse-video** cell
  (`caretStyle`) that the terminal paints as a block in its normal palette (a light
  block on a dark terminal), legible over any background — no colour-specific block,
  no thin-bar mid-string gap. All open the caret at end-of-text. (The quick-edit
  cell holds its text+caret in an embedded `textField` — the same input type the
  filters use, so the edit ops are one-line delegators — plus its own
  `editDirty`/`editOrigNull`/`editR`/`editC` state, and shares the renderer.)
- **NULL vs empty rendering**: `nil` → faint `NULL`; `""` → blank; literal
  `"NULL"` string → normal text. Driven by the `isNull` flag, not string content.

## Per-engine gotchas

- **Postgres**: schema-aware; every table renders `schema.table` (including
  `public.` — see `tableLabel`, which qualifies uniformly so the prefix-first
  list filter can't drop schema-qualified tables).
  `AutoGenerated` = identity or `nextval(...)` default.
- **MySQL**: URL → driver DSN via `mysql.Config` in `mysqlDSN` (don't hand-format).
  Single DB (`DATABASE()`), no schema qualification. `AutoGenerated` = `extra`
  has `auto_increment`.
- **SQLite**: `PRAGMA table_info`; a lone `INTEGER PRIMARY KEY` is the rowid alias
  → `AutoGenerated`. DSN accepts `sqlite://`, `file:`, or a bare path.
