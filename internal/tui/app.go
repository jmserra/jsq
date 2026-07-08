// Package tui is the bubbletea front end: picker → flat table list → grid.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

type screen int

const (
	screenPicker    screen = iota // connection picker (bare `jsq`)
	screenBrowse                  // the results grid
	screenTables                  // the full-screen table list
	screenDatabases               // the full-screen database list (T)
)

const leftPad = 1 // 1-char blank margin before the table list / grid

// App is the root bubbletea Model.
type App struct {
	screen screen
	picker picker

	connName   string
	safe       bool            // connection confirms every mutation (safe in config)
	pending    config.Conn     // the direct/selected connection (empty URL → picker); cmd lives here
	tunneled   map[string]bool // connections whose `cmd` tunnel is already up (don't re-run it)
	connecting bool            // a connect is in flight (guards a double-select on the picker)

	// While a `cmd`-backed connection is being established, connCmd/connAddr drive
	// the full-screen waiting view (what we ran + the port we're waiting for); both
	// are cleared once the connect resolves. ticking guards the one spinner loop.
	connCmd  string
	connAddr string
	ticking  bool

	// preConn snapshots the identity a mid-session connect overwrites optimistically
	// (connName/safe/pending/viewIdx), so an Esc-cancel or a failed connect can roll
	// it back. It's set only while a cancellable connect is in flight and cleared
	// once one commits (connectedMsg).
	preConn *connRestore

	engine  db.Engine
	sidebar sidebar // the full-screen table list (screenTables)
	dbs     sidebar // the full-screen database list (screenDatabases; items are db.Table{Name: db})
	grid    grid
	cell    cellView
	help    help
	confirm confirmView // safe-mode "run this mutation?" overlay

	currentTable db.Table
	basePreds    []eqPred // followed-FK equality filter, AND-ed into every load until the table changes
	baseNote     string   // human form of basePreds, shown in the status line
	sortCol      string
	sortAsc      bool

	// Jumplist: one session-wide list of visited views (oldest→newest, viewIdx =
	// current). Ctrl-O/Ctrl-I step it; the ` picker jumps to any entry. It spans
	// databases — each view records its db and a jump reconnects if needed. A view
	// is {db, table, FK-filter, sort, cursor pos} plus a cached snapshot of its
	// loaded rows, so a jump lands exactly where you left and, when the snapshot
	// is still resident, restores instantly with no DB round-trip.
	views       []viewState
	viewIdx     int        // index of the current view; -1 before the first
	viewSeq     int        // monotonic touch counter → LRU eviction of cached snapshots
	jumps       jumpView   // the `-key jumplist picker overlay
	pendingView *viewState // set for a cross-database jump: load it once reconnected
	pendingPos  *gridPos   // restore this cursor/scroll after the next reload (a jump with no cache)
	resetGrid   bool       // reset cursor on next rows load (new table, not a re-sort)
	adHoc       bool       // grid shows a free-form (s) query result, not a table

	lastQuery  map[string]string // last scratch (s) query per conn+db+table (queryKey), for the edit loop
	adHocQuery string            // SQL behind the current adHoc result, so `r` can re-run it

	// Query history: per-connection list of free-form (s) queries, most-recent
	// first, deduped by SQL. `b` opens the histView buffer over it; each entry's
	// row/affected count is filled in when its result lands (recordQueryCount).
	history  map[string][]histEntry
	histView histView

	dbName         string
	w, h           int
	status         string
	postExecStatus string // shown after the reload that follows a full-path exec
	fatalErr       error  // connect failure → carried out to main, printed to stderr

	// Header activity indicator (top-right): activity names the in-flight DB op
	// (empty → nothing shown), cancel kills it (Esc), spinner is the frame index.
	activity string
	cancel   context.CancelFunc
	spinner  int

	// gen is a monotonic token bumped by begin() and stamped onto each dispatched
	// DB command's result message. A message whose gen no longer matches belongs
	// to a superseded op (a faster later op cancelled it after it had already
	// produced its result) and is ignored — so a stale result can neither cancel
	// the current op nor apply its rows over it. Non-op messages carry gen 0.
	gen int
}

// New builds the root model. If direct.URL != "", it connects to that connection
// directly; else it shows the picker. cmd rides along on direct (§8).
func New(conns []config.Conn, direct config.Conn) App {
	a := App{
		picker:    picker{conns: conns},
		grid:      newGrid(),
		connName:  direct.Name,
		safe:      direct.Safe,
		pending:   direct,
		lastQuery: map[string]string{},
		history:   map[string][]histEntry{},
		viewIdx:   -1,
		sidebar:   sidebar{label: "tables"},
		dbs:       sidebar{label: "databases"},
		tunneled:  map[string]bool{},
	}
	if direct.URL != "" {
		a.screen = screenBrowse
		a.status = "connecting…"
		a.beginConnect(direct)
	} else {
		// Picker mode: reserve the spinner loop (Init dispatches it) so the very
		// first connect — before any perpetual loop exists — animates its loader.
		a.ticking = true
	}
	return a
}

// FatalErr is the connect failure that quit the app, if any — main reads it from
// the returned model and prints it to stderr.
func (a App) FatalErr() error { return a.fatalErr }

func (a App) Init() tea.Cmd {
	if a.pending.URL == "" {
		// Picker mode: run the spinner loop for the whole session (it idles
		// invisibly) so a slow first connect from the picker shows an animated loader.
		return tickCmd()
	}
	// The initial connect carries gen 0 — it can't be cancelled in place (Esc quits,
	// there being no previous page), so its result is never dropped as stale.
	if a.connCmd != "" {
		// New already armed the waiting view + spinner loop; animate it while
		// connectCmd runs the cmd/wait/open in the background.
		return tea.Batch(connectCmd(0, a.pending), tickCmd())
	}
	return connectCmd(0, a.pending)
}

// beginConnect arms the full-screen waiting view for a `cmd`-backed connection
// (a no-op otherwise) and marks the spinner loop running; the caller dispatches
// connectCmd + tickCmd. connectedMsg/errMsg clear connCmd to dismiss the view.
func (a *App) beginConnect(c config.Conn) {
	if c.Cmd == "" {
		return
	}
	a.connCmd = c.Cmd
	a.connAddr = db.HostPort(c.URL)
	a.ticking = true
}

// connRestore is the identity a mid-session connect overwrites before it resolves;
// an Esc-cancel or a failed connect rolls it back so we don't end up showing a
// connection/database/jumplist position we never actually reached.
type connRestore struct {
	connName string
	safe     bool
	pending  config.Conn
	viewIdx  int
}

// startConnect marks a cancellable connect as in flight: it snapshots the identity
// to roll back on cancel, bumps the op token so a late result can be dropped, and
// sets connecting. Callers invoke it just before overwriting connName/pending/etc.
func (a *App) startConnect() int {
	a.preConn = &connRestore{connName: a.connName, safe: a.safe, pending: a.pending, viewIdx: a.viewIdx}
	a.gen++
	a.connecting = true
	return a.gen
}

// restoreConn rolls back the identity captured by startConnect (a no-op if none),
// used when a connect is cancelled or fails.
func (a *App) restoreConn() {
	if a.preConn == nil {
		return
	}
	a.connName, a.safe, a.pending, a.viewIdx = a.preConn.connName, a.preConn.safe, a.preConn.pending, a.preConn.viewIdx
	a.preConn = nil
}

// cancelConnect aborts an in-flight connect (Esc): it invalidates the connect's
// late result (a bumped token via restore's caller), rolls back the optimistic
// identity, dismisses the loader, and drops any staged jump — the View then falls
// back to the screen we were on, i.e. the previous page.
func (a *App) cancelConnect() {
	a.gen++ // supersede the in-flight connect so its connectedMsg is dropped
	a.restoreConn()
	a.pendingView = nil
	a.connecting = false
	a.connCmd, a.connAddr = "", ""
	a.status = "cancelled"
}

// ensureTick starts the spinner loop unless one is already running — so a
// waiting-view connect and connectedMsg don't spin up two loops.
func (a *App) ensureTick() tea.Cmd {
	if a.ticking {
		return nil
	}
	a.ticking = true
	return tickCmd()
}

// isConnecting reports whether the full-screen connecting loader should own the
// screen: a cmd-backed connect (connCmd set) or a plain reconnect that has the
// spinner loop running to animate it (connecting && ticking). The ticking guard
// keeps the no-loop initial-picker window off the loader (it would sit frozen),
// while every mid-session reconnect — where a perpetual loop is already up —
// shows an animated "connecting to X…" even when the open isn't instant.
func (a App) isConnecting() bool { return a.connCmd != "" || (a.connecting && a.ticking) }

// begin marks a new in-flight DB op labelled for the header indicator, cancels
// any previous op, and returns a cancellable context for the op's command. The
// stored cancel func is what Esc calls (see stop). Callers must set a.activity
// via this before dispatching a cancellable tea.Cmd.
func (a *App) begin(label string) context.Context {
	a.stop()
	a.activity = label
	a.gen++ // supersede any prior op: its late result will no longer match a.gen
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	return ctx
}

// stale reports whether a gen-stamped result message belongs to a superseded op
// and should be ignored. Non-op messages carry gen 0 and are never stale.
func (a App) stale(gen int) bool { return gen != 0 && gen != a.gen }

// stop clears the activity indicator and cancels any in-flight op — used both to
// kill a running op (Esc) and to tidy up once one completes.
func (a *App) stop() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.activity = ""
}

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		a.layout()
		return a, nil

	case connectErrMsg:
		if a.stale(msg.gen) { // a picker connect the user cancelled before it failed
			return a, nil
		}
		// No session to fall back to — quit and let main print it to stderr.
		a.stop()
		a.fatalErr = msg.err
		return a, tea.Quit

	case errMsg:
		if a.stale(msg.gen) { // a superseded op's late failure — ignore it
			return a, nil
		}
		a.stop()
		a.connCmd, a.connAddr = "", "" // dismiss the waiting view
		a.connecting = false
		a.grid.loading = false
		a.pendingPos = nil  // a failed load never repositions a later one
		a.pendingView = nil // and never resolves a pending cross-DB jump
		a.restoreConn()     // a failed mid-session connect rolls back its optimistic identity
		// A mid-session failure — a typo'd ad-hoc query, a rejected mutation, a
		// failed reconnect — is recoverable: the engine is still usable (the connect
		// path fails via connectErrMsg instead). Surface it in the status line and
		// stay on the current screen rather than trapping on a dead-end error page.
		a.status = "error: " + strings.Join(strings.Fields(msg.err.Error()), " ")
		return a, nil

	case tickMsg:
		if a.activity != "" || a.isConnecting() {
			a.spinner++
		}
		return a, tickCmd()

	case connectedMsg:
		if a.stale(msg.gen) { // a cancelled/superseded connect landed late — drop it
			if msg.engine != nil {
				msg.engine.Close() // and don't leak the engine it opened
			}
			return a, nil
		}
		a.preConn = nil                // this connect committed; nothing to roll back
		a.connCmd, a.connAddr = "", "" // dismiss the waiting view
		a.connecting = false
		old := a.engine
		a.engine = msg.engine
		if old != nil {
			old.Close() // close the previous engine now that the new one is ready
		}
		if msg.name != "" {
			a.connName = msg.name
		}
		a.tunneled[a.connName] = true // its cmd (if any) is now running — reuse it
		a.dbName = msg.dbName
		a.sidebar.setTables(msg.tables)
		// The jumplist is session-wide (spans databases) — do NOT reset it. Clear
		// only the live table state; a jump restores it via pendingView below.
		a.currentTable = db.Table{}
		a.basePreds, a.baseNote = nil, ""
		a.layout()
		if a.pendingView != nil { // a cross-database jump: load that view, not the list
			v := *a.pendingView
			a.pendingView = nil
			return a.loadView(v, "loading "+v.table.Name)
		}
		a.screen = screenTables // otherwise land on the table list
		a.status = ""
		// Kick off the perpetual spinner tick (unless the waiting view already
		// started it); it idles invisibly and animates only during a DB op.
		return a, a.ensureTick()

	case databasesMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.stop()
		if len(msg.names) == 0 {
			a.status = "no other databases on this connection"
			return a, nil
		}
		tabs := make([]db.Table, len(msg.names))
		for i, n := range msg.names {
			tabs[i] = db.Table{Name: n}
		}
		a.dbs.setTables(tabs)
		a.screen = screenDatabases
		a.layout()
		return a, nil

	case rowsMsg:
		if a.stale(msg.gen) { // a superseded load landed late — don't apply it
			return a, nil
		}
		a.stop()
		a.grid.setResult(msg.rs)
		a.grid.hasMore = msg.full
		a.grid.loading = false
		// Header marker: explicit J/K sort, else the default PK-descending order.
		sc, sa := a.sortCol, a.sortAsc
		if sc == "" && len(msg.rs.PK) > 0 {
			sc, sa = msg.rs.PK[0], false
		}
		a.grid.setSort(sc, sa)
		switch {
		case a.pendingPos != nil: // a jump reload: land where we left off
			a.grid.setPos(*a.pendingPos)
			a.pendingPos, a.resetGrid = nil, false
		case a.resetGrid:
			a.grid.reset()
			a.resetGrid = false
		}
		a.adHoc = false // a table load leaves any prior s/S result behind
		a.screen = screenBrowse
		a.layout()
		a.status = msg.table.Name
		if a.baseNote != "" { // a followed-FK view: show the predicate
			a.status = msg.table.Name + " · " + a.baseNote
		}
		// A reload triggered by a full-path exec keeps its confirmation visible.
		if a.postExecStatus != "" {
			a.status = a.postExecStatus
			a.postExecStatus = ""
		}
		return a, nil

	case moreRowsMsg:
		if a.stale(msg.gen) { // a superseded scroll fetch — never append to a newer view
			return a, nil
		}
		a.stop()
		a.grid.appendRows(msg.rows, msg.full)
		return a, nil

	case editDoneMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.stop()
		// A keyed edit must touch exactly one row; anything else is loud (§8).
		if msg.affected == 1 {
			if msg.null {
				a.grid.applyEditNull(msg.rowIdx, msg.colIdx)
				a.status = fmt.Sprintf("set %s = NULL", msg.col)
			} else {
				a.grid.applyEdit(msg.rowIdx, msg.colIdx, msg.val)
				a.status = fmt.Sprintf("set %s = '%s'", msg.col, msg.val)
			}
		} else {
			a.status = fmt.Sprintf("⚠ %s: %d rows affected, expected 1", msg.col, msg.affected)
		}
		return a, nil

	case editorReadyMsg:
		if a.stale(msg.gen) { // a superseded insert/duplicate prep — don't open the editor
			return a, nil
		}
		a.stop() // insert/duplicate prep finished; the editor spawn isn't a DB op
		return a, editorCmd(msg.seed)

	case editorSubmitMsg:
		// Remember an s query as its table's last query, so the next s prefills
		// it (even if it errors) for a tight edit-run-edit loop.
		if msg.remember.Name != "" {
			a.lastQuery[a.queryKey(msg.remember)] = msg.sql
			a.recordQuery(msg.sql) // and into the connection-scoped `b` history
		}
		// A read (s) shows its rows directly; a mutation runs via Exec — and on a
		// safe connection it's held for a y/n confirmation first. A multi-statement
		// submission on a safe connection is treated as a mutation even if it leads
		// with a read verb, so a trailing write can't slip past unconfirmed.
		if isReadSQL(msg.sql) && !(a.safe && isMultiStatement(msg.sql)) {
			ctx := a.begin("running query")
			a.status = "running query…"
			return a, runQueryCmd(ctx, a.gen, a.engine, msg.sql)
		}
		sql := msg.sql
		if a.safe {
			return a.askMutation(sql, "running",
				func(ctx context.Context, gen int) tea.Cmd { return execRawCmd(ctx, gen, a.engine, sql) })
		}
		ctx := a.begin("running")
		a.status = "running…"
		return a, execRawCmd(ctx, a.gen, a.engine, sql)

	case editorAbortedMsg:
		a.stop()
		a.status = "edit cancelled"
		return a, nil

	case queryResultMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.stop()
		a.grid.setResult(msg.rs)
		a.grid.setSort("", false)
		a.grid.clearFilters()
		a.grid.hasMore = false
		a.grid.loading = false
		a.grid.reset()
		a.adHoc = true
		a.adHocQuery = msg.sql
		a.screen = screenBrowse
		a.layout()
		a.recordQueryCount(msg.sql, len(msg.rs.Rows), true)
		a.status = fmt.Sprintf("query — %d row(s)", len(msg.rs.Rows))
		return a, nil

	case execDoneMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		// Reload the current view so the change is reflected; the affected count
		// survives the reload via postExecStatus.
		a.recordQueryCount(msg.sql, int(msg.affected), false)
		a.postExecStatus = fmt.Sprintf("ran — %d row(s) affected", msg.affected)
		ctx := a.begin("reloading")
		return a, a.loadCurrentCmd(ctx)

	case tea.KeyMsg:
		return a.handleKey(msg)
	}
	return a, nil
}

func (a *App) layout() {
	bodyH := a.h - 1 // status line
	if bodyH < 1 {
		bodyH = 1
	}
	avail := a.w - leftPad
	if avail < 1 {
		avail = 1
	}
	// Grid and the two lists each own the whole body (separate full-screen pages).
	a.grid.setSize(avail, bodyH)
	a.sidebar.w, a.sidebar.h = avail, bodyH
	a.dbs.w, a.dbs.h = avail, bodyH
}

func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
	}
	// While a connect is in flight the loader owns the screen. Esc cancels it and
	// returns to the previous page (or quits if this is the very first connect, with
	// nowhere to go back to); every other key but Ctrl-C is swallowed so a stray key
	// can't mutate state behind the loader.
	if a.isConnecting() {
		if msg.Type == tea.KeyEsc {
			if a.engine == nil && a.screen != screenPicker {
				return a, tea.Quit // initial direct connect: no previous page
			}
			a.cancelConnect()
			return a, nil
		}
		return a, nil
	}
	// Safe-mode confirmation captures every key: only 'y' runs the pending
	// mutation, anything else cancels it.
	if a.confirm.active {
		if msg.String() == "y" {
			run, label := a.confirm.run, a.confirm.label
			a.confirm.active = false
			ctx := a.begin(label)
			a.status = label + "…"
			return a, run(ctx, a.gen)
		}
		a.confirm.active = false
		a.status = "cancelled"
		return a, nil
	}
	// Help cheat sheet captures keys while open; any of ?/Esc/Enter/q closes it.
	if a.help.active {
		switch msg.String() {
		case "?", "esc", "enter", "q":
			a.help.active = false
		case "j", "down":
			a.help.scroll(1)
		case "k", "up":
			a.help.scroll(-1)
		case "g":
			a.help.off = 0
		case "G":
			a.help.scroll(len(helpLines))
		}
		return a, nil
	}
	// Cell viewer captures keys while open.
	if a.cell.active {
		switch msg.String() {
		case "esc", "enter", "q":
			a.cell.active = false
		case "j", "down":
			a.cell.scroll(1)
		case "k", "up":
			a.cell.scroll(-1)
		case "g":
			a.cell.off = 0
		case "G":
			a.cell.scroll(len(a.cell.lines))
		}
		return a, nil
	}
	// Jumplist picker captures keys while open; Enter jumps to the highlighted view.
	if a.jumps.active {
		switch msg.String() {
		case "esc", "q", "`":
			a.jumps.active = false
		case "j", "down":
			a.jumps.move(1)
		case "k", "up":
			a.jumps.move(-1)
		case "g":
			a.jumps.cursor = 0
		case "G":
			a.jumps.move(len(a.jumps.entries))
		case "enter":
			a.jumps.active = false
			return a.jumpTo(a.jumps.cursor)
		}
		return a, nil
	}
	// Query-history buffer captures keys while open: Enter runs a read (a write
	// opens in $EDITOR), `s` opens any entry in $EDITOR to evolve it.
	if a.histView.active {
		switch msg.String() {
		case "esc", "q", "b":
			a.histView.active = false
		case "j", "down":
			a.histView.move(1)
		case "k", "up":
			a.histView.move(-1)
		case "g":
			a.histView.top()
		case "G":
			a.histView.bottom()
		case "s":
			if e, ok := a.histView.selected(); ok {
				a.histView.active = false
				return a, editorCmd(a.histSeed(e.sql))
			}
		case "enter":
			if e, ok := a.histView.selected(); ok {
				a.histView.active = false
				return a.runHist(e)
			}
		}
		return a, nil
	}
	// While editing a cell (§8 quick path), keys go to the edit overlay.
	if a.screen == screenBrowse && a.grid.editing {
		return a.handleEditKey(msg)
	}
	// While editing a column filter, keys go to the filter input.
	if a.screen == screenBrowse && a.grid.filtering >= 0 {
		return a.handleFilterKey(msg)
	}
	// `?` opens the help cheat sheet — from grid or sidebar, but only once the
	// modal/typing captures above have had their say (so `?` typed into a filter
	// stays literal).
	if a.screen == screenBrowse && msg.String() == "?" {
		a.help.open(a.w-leftPad, a.h-1)
		return a, nil
	}
	// ` opens the jumplist picker (inspect history, jump anywhere).
	if a.screen == screenBrowse && msg.String() == "`" {
		if len(a.views) == 0 {
			a.status = "no navigation history yet"
			return a, nil
		}
		a.jumps.open(a.jumpEntries(), a.viewIdx, a.w-leftPad, a.h-1)
		return a, nil
	}
	// b opens the query-history buffer: the free-form queries run on this
	// connection, most-recent first, with their last result counts.
	if a.screen == screenBrowse && msg.String() == "b" {
		hist := a.history[a.connName]
		if len(hist) == 0 {
			a.status = "no query history yet"
			return a, nil
		}
		snap := make([]histEntry, len(hist))
		copy(snap, hist)
		a.histView.open(snap, a.w-leftPad, a.h-1)
		return a, nil
	}
	// Esc kills an in-flight DB op (a slow query, a big load). This takes
	// precedence over the grid's Esc (clear-filter), which only applies when
	// nothing is running.
	if msg.Type == tea.KeyEsc && a.cancel != nil {
		a.stop()
		a.grid.loading = false
		a.status = "cancelled"
		return a, nil
	}
	switch a.screen {
	case screenPicker:
		switch msg.String() {
		case "j", "down":
			a.picker.move(1)
		case "k", "up":
			a.picker.move(-1)
		case "esc":
			if a.engine != nil { // opened mid-session → back to where we were
				if a.currentTable.Name != "" {
					a.screen = screenBrowse // a loaded grid
				} else {
					a.screen = screenTables // nothing loaded yet → the table list
				}
				a.layout()
			}
		case "enter":
			// Ignore a second Enter while a connect is already in flight (the loader
			// isn't shown yet in the no-loop initial-picker window).
			if a.connecting {
				return a, nil
			}
			if c, ok := a.picker.selected(); ok {
				return a.connectTo(c)
			}
		}
		return a, nil

	case screenTables:
		return a.handleTablesKey(msg)

	case screenDatabases:
		return a.handleDatabasesKey(msg)

	case screenBrowse:
		switch msg.String() {
		case "ctrl+o": // jumplist back (vim Ctrl-O)
			return a.jumpBy(-1)
		case "ctrl+i", "tab": // jumplist forward — terminals send Ctrl-I as Tab
			return a.jumpBy(1)
		case "t", "backspace": // go to the table list
			a.screen = screenTables
			a.layout()
			return a, nil
		case "T": // go to the database list
			return a.openDatabases()
		case "c": // open the connection picker
			if len(a.picker.conns) == 0 {
				a.status = "no configured connections"
				return a, nil
			}
			a.syncCurrent()
			a.screen = screenPicker
			return a, nil
		}
		return a.handleGridKey(msg)
	}
	return a, nil
}

// connectTo connects to the selected connection: reusing it if it's the current
// one, opening the engine directly if its tunnel is already up (no re-run of the
// `cmd`), or running the full connect (tunnel + wait) the first time. The initial
// connect (no engine yet) quits on failure; a mid-session switch does not.
func (a App) connectTo(c config.Conn) (tea.Model, tea.Cmd) {
	if a.engine != nil && c.Name == a.connName {
		a.screen = screenTables // already here → just show its tables
		a.layout()
		return a, nil
	}
	a.syncCurrent()         // capture the current view before changing identity
	gen := a.startConnect() // snapshot identity (rollback on Esc) + op token + connecting
	a.connName, a.safe, a.pending = c.Name, c.Safe, c
	a.status = "connecting to " + c.Name + "…"
	if a.engine == nil { // initial connect (startup): connectCmd, quits on failure
		a.beginConnect(c)
		if a.connCmd != "" {
			return a, tea.Batch(connectCmd(gen, c), tickCmd())
		}
		return a, connectCmd(gen, c)
	}
	// Mid-session switch: reuse the tunnel if it's already up.
	start := !a.tunneled[c.Name]
	if start && c.Cmd != "" {
		a.beginConnect(c) // loader while the tunnel comes up
	}
	return a, openEngineCmd(gen, c, c.URL, start)
}

// openDatabases fetches the connection's databases and shows the picker.
func (a App) openDatabases() (tea.Model, tea.Cmd) {
	ctx := a.begin("loading databases")
	a.status = "loading databases…"
	return a, databasesCmd(ctx, a.gen, a.engine)
}

// sidebarNav applies the list-navigation keys shared by the table and database
// screens: arrows / Ctrl-N/P step within a column, ←/→ jump columns, and Space
// types a filter space. Screen-specific keys (Enter, Esc, Backspace, rune input)
// are handled by the caller before delegating here.
func sidebarNav(s *sidebar, msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyUp, tea.KeyCtrlP:
		s.move(-1)
	case tea.KeyDown, tea.KeyCtrlN:
		s.move(1)
	case tea.KeyLeft:
		s.move(-s.rows())
	case tea.KeyRight:
		s.move(s.rows())
	case tea.KeySpace:
		s.filterInput(" ")
	}
}

// handleDatabasesKey drives the full-screen database list: typing filters, Enter
// switches to that database, Esc clears the filter then returns to the table list.
func (a App) handleDatabasesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if t, ok := a.dbs.selected(); ok {
			return a.switchDatabase(t.Name)
		}
		return a, nil
	case tea.KeyEsc:
		if a.dbs.hasFilter() {
			a.dbs.clearFilter()
			return a, nil
		}
		a.screen = screenTables // no filter → back to the table list
		a.layout()
		return a, nil
	case tea.KeyBackspace:
		a.dbs.filterBackspace()
		return a, nil
	case tea.KeyRunes:
		a.dbs.filterInput(string(msg.Runes))
		return a, nil
	}
	sidebarNav(&a.dbs, msg)
	return a, nil
}

// switchDatabase reconnects the engine to name on the same server and reloads.
func (a App) switchDatabase(name string) (tea.Model, tea.Cmd) {
	if name == a.dbName { // already here → just go back to its tables
		a.screen = screenTables
		a.layout()
		return a, nil
	}
	a.syncCurrent()         // save the current view into the session jumplist before leaving
	gen := a.startConnect() // snapshot for rollback + op token + arm the loader
	dsn := db.WithDatabase(a.pending.URL, name)
	a.pending.URL = dsn // so a later T swaps from the new database
	a.status = "connecting to " + name + "…"
	return a, openEngineCmd(gen, config.Conn{Name: a.connName}, dsn, false)
}

// handleTablesKey drives the full-screen table list: typing narrows it (no `/`),
// arrows / Ctrl-N/P move, Enter opens, Esc clears the filter or returns to the
// grid, and Backspace (with no filter) steps up to the connection picker.
func (a App) handleTablesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if t, ok := a.sidebar.selected(); ok {
			return a.selectTable(t)
		}
		return a, nil
	case tea.KeyEsc:
		if a.sidebar.hasFilter() {
			a.sidebar.clearFilter()
			return a, nil
		}
		if a.currentTable.Name != "" { // no filter → back to the grid we came from
			a.screen = screenBrowse
			a.layout()
		}
		return a, nil
	case tea.KeyBackspace:
		// Backspace edits the filter while one is active; with none it steps up to
		// the connection picker (mirroring Backspace in the grid → table list).
		if a.sidebar.hasFilter() {
			a.sidebar.filterBackspace()
		} else if len(a.picker.conns) > 0 {
			a.screen = screenPicker
		}
		return a, nil
	case tea.KeyRunes:
		// T jumps to the database list; a lowercase t still filters (matching is
		// case-insensitive, so nothing is lost by not typing a literal T).
		if string(msg.Runes) == "T" {
			return a.openDatabases()
		}
		a.sidebar.filterInput(string(msg.Runes))
		return a, nil
	}
	sidebarNav(&a.sidebar, msg)
	return a, nil
}

// viewState is a jumplist entry: enough to reload a table exactly as it was,
// plus (when resident) a cached snapshot for an instant restore. db is the
// database it lives in, so the one session-wide jumplist can span databases — a
// jump reconnects if needed. Committed column filters ride along in the cached
// snapshot but are lost once it's evicted (the reload path doesn't reapply them).
type viewState struct {
	conn      string // connection name (so the jumplist can span connections)
	db        string
	table     db.Table
	basePreds []eqPred
	baseNote  string
	sortCol   string
	sortAsc   bool

	// pos is the cursor/scroll position when the view was last left, so a jump
	// lands exactly where you were. snap is the cached loaded rows + grid state
	// for an instant, DB-free restore (nil once evicted or on a never-loaded
	// view, in which case the jump reloads and repositions to pos). seq is the
	// LRU stamp that bounds how many snapshots we keep in memory.
	pos  gridPos
	snap *gridSnapshot
	seq  int
}

func (a App) currentView() viewState {
	return viewState{
		conn:      a.connName,
		db:        a.dbName,
		table:     a.currentTable,
		basePreds: a.basePreds,
		baseNote:  a.baseNote,
		sortCol:   a.sortCol,
		sortAsc:   a.sortAsc,
	}
}

// label is the jumplist picker's one-line description of a view, qualified like
// the header: conn > db > table · note.
func (v viewState) label() string {
	var parts []string
	if v.conn != "" {
		parts = append(parts, v.conn)
	}
	if v.db != "" {
		parts = append(parts, v.db)
	}
	s := v.table.Name
	if len(parts) > 0 {
		s = strings.Join(parts, " > ") + " > " + s
	}
	if v.baseNote != "" {
		s += " · " + v.baseNote
	}
	return s
}

// syncCurrent refreshes the current entry with the live view (so a sort made
// after arriving is remembered, and its rows/cursor are cached) before any
// jumplist move. A no-op on a list screen, where there is no live table view to
// capture. While an s query is on screen (adHoc) the grid holds query rows, not
// the table's, so the table's cached snapshot/position is preserved as-is.
func (a *App) syncCurrent() {
	if a.currentTable.Name == "" {
		return
	}
	if a.viewIdx < 0 || a.viewIdx >= len(a.views) {
		return
	}
	prev := a.views[a.viewIdx]
	v := a.currentView()
	if a.adHoc {
		v.pos, v.snap = prev.pos, prev.snap
	} else {
		v.pos = a.grid.pos()
		v.snap = a.grid.snapshot()
	}
	a.viewSeq++
	v.seq = a.viewSeq
	a.views[a.viewIdx] = v
	a.evictSnaps()
}

// maxCachedViews bounds how many jumplist entries keep their full loaded rows in
// memory; older (least-recently-visited) snapshots are dropped to their metadata
// and reload on demand. Keeps the cache "reasonable" on a long session.
const maxCachedViews = 16

// evictSnaps drops the row cache from all but the maxCachedViews most recently
// touched views (by seq). The metadata (table, preds, sort, pos) is kept, so an
// evicted view still reloads and repositions — just not instantly.
func (a *App) evictSnaps() {
	cached := make([]int, 0, len(a.views))
	for i := range a.views {
		if a.views[i].snap != nil {
			cached = append(cached, i)
		}
	}
	if len(cached) <= maxCachedViews {
		return
	}
	sort.Slice(cached, func(i, j int) bool { return a.views[cached[i]].seq < a.views[cached[j]].seq })
	for _, i := range cached[:len(cached)-maxCachedViews] {
		a.views[i].snap = nil
	}
}

// navigate records v as a new jump (dropping any forward history) and loads it.
func (a App) navigate(v viewState, label string) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	// Truncate the forward tail and append; the 3-index slice forces a fresh
	// backing array so the prior (value-copied) model isn't mutated.
	a.views = append(a.views[:a.viewIdx+1:a.viewIdx+1], v)
	if len(a.views) > 100 { // keep the list bounded
		a.views = a.views[len(a.views)-100:]
	}
	a.viewIdx = len(a.views) - 1
	return a.loadView(v, label)
}

// jumpBy steps the jumplist: d=-1 is Ctrl-O (back), d=+1 is Ctrl-I (forward).
func (a App) jumpBy(d int) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	ni := a.viewIdx + d
	if ni < 0 || ni >= len(a.views) {
		where := "back"
		if d > 0 {
			where = "forward"
		}
		a.status = "no view to go " + where + " to"
		return a, nil
	}
	return a.goToView(ni)
}

// goToView loads the jumplist entry at idx, reconnecting first if it lives on
// another connection or database. On a reconnect it snapshots the current identity
// (startConnect) so an Esc-cancel rolls back cleanly, and moves the jumplist
// pointer here (not in the caller) so a cancelled jump leaves it where it was.
// pendingView makes connectedMsg load the view rather than the table list.
func (a App) goToView(idx int) (tea.Model, tea.Cmd) {
	v := a.views[idx]
	if v.conn != "" && v.conn != a.connName { // cross-connection jump
		c, ok := a.findConn(v.conn)
		if !ok {
			a.status = "connection " + v.conn + " is not available"
			return a, nil
		}
		dsn := c.URL
		if v.db != "" {
			dsn = db.WithDatabase(c.URL, v.db)
		}
		gen := a.startConnect() // snapshots viewIdx before the move below
		a.viewIdx = idx
		a.connName, a.safe, a.pending = c.Name, c.Safe, c
		vv := v
		a.pendingView = &vv
		start := !a.tunneled[c.Name]
		a.status = "connecting to " + c.Name + "…"
		if start && c.Cmd != "" {
			a.beginConnect(c) // cmd-backed: also show the running/waiting detail
		}
		return a, openEngineCmd(gen, c, dsn, start)
	}
	if v.db != "" && v.db != a.dbName { // same connection, different database
		gen := a.startConnect()
		a.viewIdx = idx
		dsn := db.WithDatabase(a.pending.URL, v.db)
		a.pending.URL = dsn
		vv := v
		a.pendingView = &vv
		a.status = "connecting to " + v.db + "…"
		return a, openEngineCmd(gen, config.Conn{Name: a.connName}, dsn, false)
	}
	a.viewIdx = idx
	return a.loadView(v, "loading "+v.table.Name)
}

// findConn looks up a configured connection by name.
func (a App) findConn(name string) (config.Conn, bool) {
	for _, c := range a.picker.conns {
		if c.Name == name {
			return c, true
		}
	}
	return config.Conn{}, false
}

// jumpEntries is the picker's label list, current view synced in first.
func (a App) jumpEntries() []string {
	a.syncCurrent()
	out := make([]string, len(a.views))
	for i, v := range a.views {
		out[i] = v.label()
	}
	return out
}

// jumpTo loads the view at index i (the picker's Enter). Current/out-of-range → no-op.
func (a App) jumpTo(i int) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	if i < 0 || i >= len(a.views) || i == a.viewIdx {
		return a, nil
	}
	return a.goToView(i)
}

// loadView switches to v. If v carries a cached snapshot (a revisited view), it
// restores instantly from memory — no DB round-trip; `r` refreshes if the data
// looks stale. Otherwise it reloads the table and repositions to v.pos once the
// rows arrive. Shared by selectTable, follow, and the jumplist.
func (a App) loadView(v viewState, label string) (App, tea.Cmd) {
	a.currentTable = v.table
	a.basePreds = v.basePreds
	a.baseNote = v.baseNote
	a.sortCol, a.sortAsc = v.sortCol, v.sortAsc
	if v.snap != nil { // instant restore from the in-memory cache
		a.stop()
		a.grid.restore(v.snap)
		a.adHoc = false
		a.pendingPos, a.resetGrid = nil, false
		a.screen = screenBrowse
		a.layout()
		a.status = v.table.Name
		if v.baseNote != "" {
			a.status = v.table.Name + " · " + v.baseNote
		}
		return a, nil
	}
	a.resetGrid = true
	a.grid.clearFilters()
	pos := v.pos
	a.pendingPos = &pos // reposition to where we left, once the rows load
	ctx := a.begin(label)
	a.status = label + "…"
	return a, a.loadViewCmd(ctx, pos)
}

// loadViewCmd loads the current view for a jump, widening the fetch window if the
// remembered cursor sits past the default one (LIMIT/OFFSET from the top, so the
// row is only there if the window reaches it).
func (a App) loadViewCmd(ctx context.Context, pos gridPos) tea.Cmd {
	limit := a.gridLimit()
	if need := pos.cursorR + a.grid.visibleRows() + 1; need > limit {
		limit = need
	}
	return loadCmd(ctx, a.gen, a.engine, a.currentTable, limit, a.sortCol, a.sortAsc, a.basePreds, a.grid.filterSpecs())
}

// follow navigates the foreign key on the cursor's column to the row it points
// at. Resolution is in-memory (the FKs were fetched at load, on the grid) — the
// only DB work is loadView's reload, so no engine call happens in Update.
func (a App) follow() (tea.Model, tea.Cmd) {
	if a.adHoc {
		a.status = "follow unavailable on a query result"
		return a, nil
	}
	col, ok := a.grid.currentColName()
	row, ok2 := a.grid.currentRowMap()
	if !ok || !ok2 {
		return a, nil
	}
	fk, found := a.grid.fkFor(col)
	if !found {
		a.status = "no foreign key on " + col
		return a, nil
	}
	preds := make([]eqPred, 0, len(fk.Columns))
	for i, local := range fk.Columns {
		v := row[local]
		if v == nil {
			a.status = "cannot follow: " + local + " is NULL"
			return a, nil
		}
		preds = append(preds, eqPred{col: fk.RefColumns[i], val: v})
	}
	note := fmt.Sprintf("%s = %v", fk.RefColumns[0], row[fk.Columns[0]])
	return a.navigate(viewState{
		conn:      a.connName,
		db:        a.dbName,
		table:     db.Table{Schema: fk.RefTable.Schema, Name: fk.RefTable.Name},
		basePreds: preds,
		baseNote:  note,
		sortAsc:   true,
	}, "loading "+fk.RefTable.Name)
}

// selectTable loads the given table into the grid with default sort and no
// filters, and auto-hides the sidebar (handled on rowsMsg).
func (a App) selectTable(t db.Table) (tea.Model, tea.Cmd) {
	// a fresh table from the sidebar is unfiltered, default (PK desc) sort.
	return a.navigate(viewState{conn: a.connName, db: a.dbName, table: t, sortAsc: true}, "loading "+t.Name)
}

// askMutation arms the safe-mode confirmation overlay (connection safe=true) for
// a pending mutation: it shows sql on the current connection/database, and run
// dispatches the exec command only if the user confirms with 'y'.
func (a App) askMutation(sql, label string, run func(context.Context, int) tea.Cmd) (tea.Model, tea.Cmd) {
	a.confirm.ask(a.connName, a.dbName, sql, label, run, a.w-leftPad, a.h-1)
	a.status = "confirm — run this statement? (y = yes)"
	return a, nil
}

// handleEditKey routes keys while a cell is being edited (§8 quick path). Enter
// builds the keyed UPDATE and runs it immediately; Esc cancels. A bare Enter
// with no typing is a no-op (commitEdit returns ok=false), so a NULL cell can't
// be blanked by accident.
func (a App) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if req, ok := a.grid.commitEdit(); ok {
			if a.safe {
				return a.askMutation(previewEditSQL(a.engine, req), "saving",
					func(ctx context.Context, gen int) tea.Cmd { return execEditCmd(ctx, gen, a.engine, req) })
			}
			ctx := a.begin("saving")
			a.status = "saving…"
			return a, execEditCmd(ctx, a.gen, a.engine, req)
		}
		a.status = a.currentTable.Name
	case tea.KeyEsc:
		a.grid.cancelEdit()
		a.status = a.currentTable.Name
	case tea.KeyBackspace:
		a.grid.editBackspace()
	case tea.KeyDelete:
		a.grid.editDelete()
	case tea.KeyLeft:
		a.grid.editLeft()
	case tea.KeyRight:
		a.grid.editRight()
	case tea.KeyHome, tea.KeyCtrlA:
		a.grid.editHome()
	case tea.KeyEnd, tea.KeyCtrlE:
		a.grid.editEnd()
	case tea.KeyCtrlW:
		a.grid.editDeleteWord()
	case tea.KeySpace:
		a.grid.editInput(" ")
	case tea.KeyRunes:
		a.grid.editInput(string(msg.Runes))
	}
	return a, nil
}

// handleFilterKey routes keys while a column filter is being typed (§7.1).
func (a App) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		a.grid.commitFilter()
		ctx := a.begin("filtering")
		return a, a.loadCurrentCmd(ctx)
	case tea.KeyEsc:
		a.grid.clearFilter()
		ctx := a.begin("loading")
		return a, a.loadCurrentCmd(ctx)
	case tea.KeyBackspace:
		a.grid.filterBackspace()
	case tea.KeyDown:
		a.grid.moveRow(1)
	case tea.KeyUp:
		a.grid.moveRow(-1)
	case tea.KeySpace:
		a.grid.filterInput(" ")
	case tea.KeyRunes:
		a.grid.filterInput(string(msg.Runes))
	}
	return a, nil
}

func (a App) handleGridKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		a.grid.moveRow(1)
	case "k", "up":
		a.grid.moveRow(-1)
	case "h", "left":
		a.grid.moveCol(-1)
	case "l", "right":
		a.grid.moveCol(1)
	case "g":
		a.grid.top()
	case "G":
		a.grid.bottom()
	case "0":
		a.grid.firstCol()
	case "$":
		a.grid.lastCol()
	case "/":
		if a.adHoc {
			a.status = "filter unavailable on a query result"
			return a, nil
		}
		a.grid.startFilter()
	case "esc":
		if a.grid.clearCurrentFilter() {
			ctx := a.begin("loading")
			return a, a.loadCurrentCmd(ctx)
		}
	case "e":
		if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if a.grid.startEdit() {
			a.status = "editing " + a.grid.cols[a.grid.editC].name + " — Enter saves, Esc cancels"
		}
		return a, nil
	case "E":
		if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if col, val, keys, ok := a.grid.fullEditTarget(); ok {
			return a, editorCmd(buildUpdateStmt(a.engine, a.grid.table, col, val, keys))
		}
		return a, nil
	case "o":
		if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else {
			ctx := a.begin("preparing insert")
			a.status = "preparing insert…"
			return a, prepareInsertCmd(ctx, a.gen, a.engine, a.currentTable)
		}
		return a, nil
	case "D":
		if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if keys, ok := a.grid.rowKeys(); ok {
			return a, editorCmd(buildDeleteStmt(a.engine, a.grid.table, keys))
		}
		return a, nil
	case "p":
		if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if vals, ok := a.grid.currentRowValues(); ok {
			ctx := a.begin("preparing duplicate")
			a.status = "preparing duplicate…"
			return a, prepareDuplicateCmd(ctx, a.gen, a.engine, a.currentTable, vals)
		}
		return a, nil
	case "y":
		if s, ok := a.grid.yankCell(); ok {
			a.status = fmt.Sprintf("copied cell (%d chars)", len(s))
			return a, yankCmd(s)
		}
	case "Y":
		if s, ok := a.grid.currentRowJSON(); ok {
			a.status = "copied row as JSON"
			return a, yankCmd(s)
		}
	case "f":
		return a.follow()
	case "enter":
		if v, col, ok := a.grid.currentCell(); ok {
			a.cell.open(col, a.grid.cursorR, v, a.grid.w, a.grid.h)
		}
	case "J":
		if a.adHoc {
			a.status = "sort unavailable on a query result"
			return a, nil
		}
		if name, ok := a.grid.currentColName(); ok {
			a.sortCol, a.sortAsc = name, true
			ctx := a.begin("sorting")
			return a, a.loadCurrentCmd(ctx)
		}
	case "K":
		if a.adHoc {
			a.status = "sort unavailable on a query result"
			return a, nil
		}
		if name, ok := a.grid.currentColName(); ok {
			a.sortCol, a.sortAsc = name, false
			ctx := a.begin("sorting")
			return a, a.loadCurrentCmd(ctx)
		}
	case "s":
		return a, editorCmd(a.scratchSeed())
	case "r":
		return a.reloadView()
	}
	// After a movement, fetch the next window if the cursor neared the edge.
	// Evaluate the command first so its state mutation (activity/loading) lands
	// on the model we return.
	cmd := a.maybeLoadMore()
	return a, cmd
}

// maybeLoadMore triggers a continuous-scroll fetch when the cursor nears the
// loaded edge and more rows exist. It never fires while another op is in flight:
// begin() would cancel that op, so a cursor move during a sort/filter/reload
// could silently supersede it and append the next window onto the stale rows.
func (a *App) maybeLoadMore() tea.Cmd {
	if a.activity != "" || !a.grid.wantMore() {
		return nil
	}
	a.grid.loading = true
	anchor, _ := a.grid.lastRowMap() // keyset cursor; nil falls back to OFFSET
	ctx := a.begin("loading more")
	return loadMoreCmd(ctx, a.gen, a.engine, a.currentTable, a.sortCol, a.sortAsc,
		a.basePreds, a.grid.filterSpecs(), anchor, len(a.grid.rows), a.gridLimit())
}

// scratchSeed is the prefill for s: this table's last scratch query if one was
// run, else the SELECT template. remember ties the eventual submit back to the
// table so the loop continues.
func (a App) scratchSeed() editorSeed {
	sql := selectTemplate(a.engine, a.currentTable.Ref())
	if last, ok := a.lastQuery[a.queryKey(a.currentTable)]; ok {
		sql = last
	}
	return editorSeed{sql: sql, remember: a.currentTable}
}

// maxHistory bounds a connection's remembered query list.
const maxHistory = 200

// recordQuery promotes sql to the front of the current connection's query
// history (deduped by SQL), its result count pending until the run lands. Called
// for every free-form (s) query as it's submitted, so the b buffer lists it even
// if it errors.
func (a *App) recordQuery(sql string) {
	key := histKey(sql)
	if key == "" {
		return
	}
	list := a.history[a.connName]
	kept := make([]histEntry, 0, len(list)+1)
	kept = append(kept, histEntry{sql: key})
	for _, e := range list {
		if e.sql != key { // drop the prior copy; it moves to the front
			kept = append(kept, e)
		}
	}
	if len(kept) > maxHistory {
		kept = kept[:maxHistory]
	}
	a.history[a.connName] = kept
}

// recordQueryCount fills in the outcome (rows read or affected) of the most
// recent run of sql on the current connection — a no-op if it isn't a remembered
// query (E/o/D/p structured edits never enter the history).
func (a *App) recordQueryCount(sql string, count int, read bool) {
	key := histKey(sql)
	list := a.history[a.connName]
	for i := range list {
		if list[i].sql == key {
			list[i].count, list[i].read, list[i].ran = count, read, true
			return
		}
	}
}

// runHist executes a history entry on Enter: a read runs directly (and is
// bumped to most-recent); a write opens in $EDITOR for review rather than
// running unseen. On a safe connection a multi-statement "read" is treated as a
// write and opened, mirroring the s submit path.
func (a App) runHist(e histEntry) (tea.Model, tea.Cmd) {
	if isReadSQL(e.sql) && !(a.safe && isMultiStatement(e.sql)) {
		a.recordQuery(e.sql) // bump recency; the count refills on the result
		ctx := a.begin("running query")
		a.status = "running query…"
		return a, runQueryCmd(ctx, a.gen, a.engine, e.sql)
	}
	return a, editorCmd(a.histSeed(e.sql))
}

// histSeed opens a history query in $EDITOR to evolve it: seeded with the SQL
// and marked (remember) so a :wq re-records it and continues the s edit loop.
func (a App) histSeed(sql string) editorSeed {
	if !strings.HasSuffix(sql, "\n") {
		sql += "\n"
	}
	return editorSeed{sql: sql, remember: a.currentTable}
}

// queryKey scopes a remembered scratch query to the current connection and
// database as well as the table, so a same-named table in another database or
// connection doesn't inherit an unrelated last query. The editor blocks the UI
// while open, so connName/dbName are unchanged between the seed's read here and
// the submit's write, keeping the two sides consistent.
func (a App) queryKey(t db.Table) string {
	return strings.Join([]string{a.connName, a.dbName, t.Schema, t.Name}, "\x00")
}

// loadCurrentCmd (re)loads the current table with the active sort, any followed-FK
// base predicate, and the column filters.
func (a App) loadCurrentCmd(ctx context.Context) tea.Cmd {
	return loadCmd(ctx, a.gen, a.engine, a.currentTable, a.gridLimit(), a.sortCol, a.sortAsc, a.basePreds, a.grid.filterSpecs())
}

// reloadView re-runs the current view (`r`): a table reload keeps the sort,
// followed-FK predicate, column filters, and cursor; an adHoc result re-runs its
// query. A no-op when there's nothing loaded yet.
func (a App) reloadView() (tea.Model, tea.Cmd) {
	if a.adHoc {
		if a.adHocQuery == "" {
			return a, nil
		}
		ctx := a.begin("reloading")
		a.status = "reloading…"
		return a, runQueryCmd(ctx, a.gen, a.engine, a.adHocQuery)
	}
	if a.currentTable.Name == "" {
		return a, nil
	}
	ctx := a.begin("reloading")
	a.status = "reloading…"
	return a, a.loadCurrentCmd(ctx)
}

func (a App) gridLimit() int {
	if n := (a.h - 2) * 4; n >= 200 {
		return n
	}
	return 200
}

func (a App) View() string {
	if a.isConnecting() {
		return a.connectingView()
	}
	switch a.screen {
	case screenPicker:
		return lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.picker.View())
	case screenTables:
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.sidebar.View())
		return a.statusLine() + "\n" + body
	case screenDatabases:
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.dbs.View())
		return a.statusLine() + "\n" + body
	case screenBrowse:
		return a.browseView()
	}
	return ""
}

func (a App) browseView() string {
	if a.confirm.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.confirm.View())
		return a.statusLine() + "\n" + body
	}
	if a.help.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.help.View())
		return a.statusLine() + "\n" + body
	}
	if a.cell.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.cell.View())
		return a.statusLine() + "\n" + body
	}
	if a.jumps.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.jumps.View())
		return a.statusLine() + "\n" + body
	}
	if a.histView.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.histView.View())
		return a.statusLine() + "\n" + body
	}
	body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.grid.View())
	return a.statusLine() + "\n" + body
}

// statusLine renders "connName > dbName > table" (table = current table / msg).
func (a App) statusLine() string {
	name := a.connName
	if name == "" {
		name = "adhoc"
	}
	rest := []string{}
	if a.dbName != "" {
		rest = append(rest, a.dbName)
	}
	if a.status != "" {
		rest = append(rest, a.status)
	}
	faint := lipgloss.NewStyle().Faint(true)
	// A safe connection is most likely production — flag it by rendering the
	// connection name in red so it stands out in the header.
	nameStyle := faint
	if a.safe {
		nameStyle = safeConnStyle
	}
	left := faint.Render(" ") + nameStyle.Render(name)
	if len(rest) > 0 {
		left += faint.Render(" > " + strings.Join(rest, " > "))
	}
	left += faint.Render(" ")
	if a.activity == "" {
		return left
	}
	// Top-right activity indicator: spinner + label + a hint that Esc kills it.
	frame := string(spinnerFrames[a.spinner%len(spinnerFrames)])
	ind := activityStyle.Render(frame + " " + a.activity + " · esc ")
	gap := a.w - lipgloss.Width(left) - lipgloss.Width(ind)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + ind
}

var activityStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))

// safeConnStyle marks a safe (likely production) connection name in the header.
var safeConnStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))

// connectingView is the full-screen loader shown while a connect/reconnect is in
// flight — so even a plain, non-instant open shows something is happening. For a
// cmd-backed connection it also names the helper we ran and the port we're waiting
// on; a plain open shows just the spinner and the target.
func (a App) connectingView() string {
	frame := string(spinnerFrames[a.spinner%len(spinnerFrames)])
	label := lipgloss.NewStyle().Faint(true)
	head := a.status // the caller set "connecting to X…"; fall back if empty
	if head == "" {
		head = "connecting"
		if a.connName != "" {
			head += " to " + a.connName
		}
	}
	var b strings.Builder
	b.WriteString("\n " + activityStyle.Render(frame) + " " + head + "\n\n")
	if a.connCmd != "" {
		// The cmd can be long; soft-wrap it (indented under the label) so it never
		// clips off the right edge.
		b.WriteString(" " + label.Render("running") + "  " + a.wrapIndent(a.connCmd, 10) + "\n")
		if a.connAddr != "" {
			b.WriteString(" " + label.Render("waiting") + "  " + a.connAddr + " …\n")
		}
	}
	b.WriteString("\n " + label.Render("esc to cancel") + "\n")
	return b.String()
}

// wrapIndent soft-wraps s to the terminal width, indenting continuation lines by
// indent spaces so they line up under a label. A no-op until the width is known.
func (a App) wrapIndent(s string, indent int) string {
	if a.w <= 0 || indent >= a.w {
		return s
	}
	wrapped := lipgloss.NewStyle().Width(a.w - indent).Render(s)
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent))
}
