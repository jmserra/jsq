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
	"github.com/mattn/go-runewidth"
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

	conns    []config.Conn // configured connections (source of truth for findConn)
	connList sidebar       // the connection picker screen (screenPicker), over conns

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
	cell    cellView
	help    help
	confirm confirmView // safe-mode "run this mutation?" overlay
	errView errView     // failed-statement modal (full error + query, e to re-edit)

	// panes are the split views (`<space>v`), left→right; focus indexes the live
	// one. There is always at least one. Each owns its grid, what it's showing,
	// and its own jumplist — see pane.
	panes      []pane
	focus      int
	nextPaneID int  // monotonic source of stable pane ids
	leader     bool // <space> pressed; the next key is a split/focus command

	viewSeq     int        // monotonic touch counter → LRU eviction of cached snapshots
	jumps       jumpView   // the `-key jumplist picker overlay
	pendingView *viewState // set for a cross-database jump: load it once reconnected
	pendingPos  *gridPos   // restore this cursor/scroll after the next reload (a jump with no cache)
	resetGrid   bool       // reset cursor on next rows load (new table, not a re-sort)

	lastQuery map[string]string // last scratch (s) query per conn+db+table (queryKey), for the edit loop

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

	// opPane is the id of the pane begin()'s op was dispatched for. There is one
	// op slot, so the App always knows whose result is coming: a handler applies
	// the result to THIS pane, not the focused one (focus can move while a query
	// runs), and drops it if the pane has since been closed. Messages therefore
	// need no pane field of their own.
	opPane int
}

// New builds the root model. If direct.URL != "", it connects to that connection
// directly; else it shows the picker. cmd rides along on direct (§8).
func New(conns []config.Conn, direct config.Conn) App {
	a := App{
		conns:     conns,
		connList:  sidebar{label: "connections"},
		connName:  direct.Name,
		safe:      direct.Safe,
		pending:   direct,
		lastQuery: map[string]string{},
		history:   map[string][]histEntry{},
		sidebar:   sidebar{label: "tables"},
		dbs:       sidebar{label: "databases"},
		tunneled:  map[string]bool{},
	}
	// One pane to start; `<space>v` adds more. New is the only constructor, so
	// panes is never empty and p()/g() can't panic on a zero App.
	a.panes = []pane{a.newPane()}
	// The picker is a sidebar over the connection names; Enter maps the selected
	// name back to its config.Conn via findConn.
	connItems := make([]db.Table, len(conns))
	for i, c := range conns {
		connItems[i] = db.Table{Name: c.Name}
	}
	a.connList.setTables(connItems)
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
// Like begin(), it stops any op already running — a connect/db switch abandons the
// current view, so its in-flight query must be cancelled (else it keeps running on
// the server, leaves a phantom spinner, and its `esc` hijacks the next keypress).
func (a *App) startConnect() int {
	a.stop()
	a.preConn = &connRestore{connName: a.connName, safe: a.safe, pending: a.pending, viewIdx: a.p().viewIdx}
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
	a.connName, a.safe, a.pending, a.p().viewIdx = a.preConn.connName, a.preConn.safe, a.preConn.pending, a.preConn.viewIdx
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
// paneID is which pane the op's result belongs to; see App.opPane. Pass
// noPane for a session-level op (a databases fetch) whose result isn't a view.
func (a *App) begin(label string, paneID int) context.Context {
	a.stop()
	a.activity = label
	a.gen++ // supersede any prior op: its late result will no longer match a.gen
	a.opPane = paneID
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	return ctx
}

// noPane marks an op whose result isn't destined for a pane.
const noPane = 0

// opTarget resolves the pane the in-flight op was dispatched for. False once
// that pane has been closed — its result is then dropped rather than applied to
// whichever pane happens to be focused now.
func (a *App) opTarget() (*pane, bool) { return a.paneByID(a.opPane) }

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
	// Copy-on-write the panes slice so &a.panes[i] (p()/g()) points into an array
	// this model owns, and a handler's mutation can't reach back into the App we
	// were called with. Not strictly required — bubbletea discards that model, so
	// a leak into it is unobservable — but it makes the p() accessor's contract
	// true rather than merely-true-in-practice, for one small alloc per message.
	//
	// Shallow by design: pane interiors (grid rows, filter maps, views) are still
	// shared with the previous model. Deep-copying those is pane.clone's job, and
	// only when splitting.
	a.panes = append([]pane(nil), a.panes...)

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
		// Clear the loading flag on whichever pane's op failed. A gen-0 errMsg
		// (an editor spawn failure) has no op, so opPane still names the last
		// one — harmless, since it isn't loading.
		if p, ok := a.opTarget(); ok {
			p.grid.loading = false
		}
		// A failed user-authored statement (a free-form s query or a quick-path
		// cell edit) carries a seed: show the full, untruncated error with the
		// statement in a modal, so it can be edited and re-run (e/Enter) rather
		// than lost to a one-line status. Armable from any screen, like confirm.
		if msg.seed != nil {
			a.errView.arm(msg.err, *msg.seed, a.w-leftPad, a.h-1)
			a.status = "query failed — e to edit, Esc to dismiss"
			return a, nil
		}
		a.connCmd, a.connAddr = "", "" // dismiss the waiting view
		a.connecting = false
		a.pendingPos = nil  // a failed load never repositions a later one
		a.pendingView = nil // and never resolves a pending cross-DB jump
		a.restoreConn()     // a failed mid-session connect rolls back its optimistic identity
		// A mid-session failure — a rejected reconnect, a failed table load — is
		// recoverable: the engine is still usable (the connect path fails via
		// connectErrMsg instead). Surface it in the status line and stay on the
		// current screen rather than trapping on a dead-end error page.
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
		// Each pane keeps its own jumplist across the switch (it spans databases) —
		// do NOT reset it. Clear only the live table state; a jump restores it via
		// pendingView below.
		//
		// Touching just the focused pane is sound because allowSessionMove refuses
		// every conn/db change while split: a connectedMsg can only land when there
		// is exactly one pane, which is therefore the focused one.
		a.p().currentTable = db.Table{}
		a.p().basePreds, a.p().baseNote = nil, ""
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
		a.stop() // must precede the pane check: this op owns the slot (cf. stale)
		p, ok := a.opTarget()
		if !ok { // its pane was closed mid-load — nothing to apply the rows to
			return a, nil
		}
		p.grid.setResult(msg.rs)
		p.grid.hasMore = msg.full
		p.grid.loading = false
		// Header marker: explicit J/K sort, else the default PK-descending order.
		sc, sa := p.sortCol, p.sortAsc
		if sc == "" && len(msg.rs.PK) > 0 {
			sc, sa = msg.rs.PK[0], false
		}
		p.grid.setSort(sc, sa)
		switch {
		case a.pendingPos != nil: // a jump reload: land where we left off
			p.grid.setPos(*a.pendingPos)
			a.pendingPos, a.resetGrid = nil, false
		case a.resetGrid:
			p.grid.reset()
			a.resetGrid = false
		}
		p.adHoc = false // a table load leaves any prior s/S result behind
		a.screen = screenBrowse
		a.layout()
		// The table itself is a header segment now (tableSegment), so a fresh load
		// just clears the previous view's transient message.
		a.status = ""
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
		p, ok := a.opTarget()
		if !ok {
			return a, nil
		}
		p.grid.appendRows(msg.rows, msg.full)
		return a, nil

	case editDoneMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.stop()
		p, ok := a.opTarget()
		if !ok {
			return a, nil
		}
		// A keyed edit must touch exactly one row; anything else is loud (§8).
		if msg.affected == 1 {
			if msg.null {
				p.grid.applyEditNull(msg.rowIdx, msg.colIdx)
				a.status = fmt.Sprintf("set %s = NULL", msg.col)
			} else {
				p.grid.applyEdit(msg.rowIdx, msg.colIdx, msg.val)
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
		} else if msg.scratch {
			a.recordQuery(msg.sql) // table-list scratch: no table to key, but still history
		}
		// A read (s) shows its rows directly; a mutation runs via Exec — and on a
		// safe connection it's held for a y/n confirmation first. A multi-statement
		// submission on a safe connection is treated as a mutation even if it leads
		// with a read verb, so a trailing write can't slip past unconfirmed.
		// The failed statement reopens exactly as submitted, keeping its s remember/
		// scratch markers so a re-run still continues the edit loop and records.
		seed := editorSeed{sql: msg.sql, remember: msg.remember, scratch: msg.scratch}
		if isReadSQL(msg.sql) && !(a.safe && isMultiStatement(msg.sql)) {
			ctx := a.begin("running query", a.p().id)
			a.status = "running query…"
			return a, runQueryCmd(ctx, a.gen, a.engine, msg.sql, seed)
		}
		sql := msg.sql
		if a.safe {
			return a.askMutation(sql, "running",
				func(ctx context.Context, gen int) tea.Cmd { return execRawCmd(ctx, gen, a.engine, sql, seed) })
		}
		ctx := a.begin("running", a.p().id)
		a.status = "running…"
		return a, execRawCmd(ctx, a.gen, a.engine, sql, seed)

	case editorAbortedMsg:
		a.stop()
		a.status = "edit cancelled"
		return a, nil

	case queryResultMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.stop()
		p, ok := a.opTarget()
		if !ok {
			return a, nil
		}
		p.grid.setResult(msg.rs)
		p.grid.setSort("", false)
		p.grid.clearFilters()
		p.grid.hasMore = false
		p.grid.loading = false
		p.grid.reset()
		p.adHoc = true
		p.adHocQuery = msg.sql
		a.screen = screenBrowse
		a.layout()
		a.recordQueryCount(msg.sql, len(msg.rs.Rows), true)
		a.status = fmt.Sprintf("query — %d row(s)", len(msg.rs.Rows))
		return a, nil

	case execDoneMsg:
		if a.stale(msg.gen) {
			return a, nil
		}
		a.recordQueryCount(msg.sql, int(msg.affected), false)
		p, ok := a.opTarget()
		// No table to reload (e.g. a write scratch from the table list before any
		// table was opened, or the pane went away) → just report and stay put.
		if !ok || p.currentTable.Name == "" {
			a.stop()
			a.status = fmt.Sprintf("ran — %d row(s) affected", msg.affected)
			return a, nil
		}
		// Reload the view the write ran against so the change is reflected; the
		// affected count survives the reload via postExecStatus. Reload p, NOT the
		// focused pane — focus can have moved while the write was in flight, and
		// reloading whatever is focused now would both miss the change and stomp an
		// unrelated view.
		a.postExecStatus = fmt.Sprintf("ran — %d row(s) affected", msg.affected)
		ctx := a.begin("reloading", p.id)
		return a, a.loadPaneCmd(ctx, p)

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
	// The two lists each own the whole body (separate full-screen pages).
	a.sidebar.w, a.sidebar.h = avail, bodyH
	a.dbs.w, a.dbs.h = avail, bodyH
	a.connList.w, a.connList.h = avail, bodyH
	a.layoutPanes(avail, bodyH)
}

// paneCols groups the panes into their columns, left→right, each listed
// top→bottom. a.panes is maintained in that order, so a column is a contiguous
// run — but this indexes by pane.col rather than assuming it, so a stale
// ordering shows up as a misplaced pane instead of silent corruption.
func (a *App) paneCols() [][]int {
	n := 0
	for i := range a.panes {
		if a.panes[i].col+1 > n {
			n = a.panes[i].col + 1
		}
	}
	cols := make([][]int, n)
	for i := range a.panes {
		c := a.panes[i].col
		cols[c] = append(cols[c], i)
	}
	return cols
}

// layoutPanes assigns each pane its rect within the body.
//
// The layout is columns of stacked panes: widths split across the columns
// (minus the dividers), heights split within each column. Remainders go to the
// leftmost/topmost so the body is filled exactly rather than leaving a ragged
// edge. A stacked pane's own header line is part of its slot, so its grid gets
// one row less.
func (a *App) layoutPanes(avail, bodyH int) {
	cols := a.paneCols()
	n := len(cols)
	inner := avail - (n - 1) // the │ dividers between columns
	if inner < n {
		inner = n // degenerate width: 1 col each and let the clamp truncate
	}
	wBase, wExtra := inner/n, inner%n
	x := 0
	for ci, idxs := range cols {
		w := wBase
		if ci < wExtra {
			w++
		}
		m := len(idxs)
		hBase, hExtra := bodyH/m, bodyH%m
		y := 0
		for k, i := range idxs {
			slot := hBase
			if k < hExtra {
				slot++
			}
			// Split panes each carry a header line naming their table; unsplit, the
			// one global status line already does that, so the grid gets the lot.
			gh := slot
			if len(a.panes) > 1 {
				gh = slot - 1
			}
			if gh < 1 {
				gh = 1
			}
			a.panes[i].x, a.panes[i].y, a.panes[i].w, a.panes[i].h = x, y, w, gh
			a.panes[i].grid.setSize(w, gh)
			y += slot
		}
		x += w + 1 // + divider
	}
}

func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
	}
	// The <space> leader is consumed here, before anything else can claim the key:
	// otherwise `<space>j` would move the grid cursor instead of the focus.
	//
	// It's armed only from a clean grid screen, but a message can land between the
	// <space> and the next key and put something else on screen — an errMsg arming
	// the failure modal, say. Re-check rather than assume: if the ground has moved,
	// drop the leader and let the key go wherever it now belongs.
	if a.leader {
		a.leader = false
		if a.screen == screenBrowse && !a.modalActive() {
			return a.handleLeaderKey(msg)
		}
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
			// The focused pane is the one that armed this: the overlay is modal and
			// captures every key (including the split leader), so focus cannot have
			// moved between ask and y.
			ctx := a.begin(label, a.p().id)
			a.status = label + "…"
			return a, run(ctx, a.gen)
		}
		a.confirm.active = false
		a.status = "cancelled"
		return a, nil
	}
	// A failed statement is shown modally with its query; the modal captures keys.
	// e/Enter reopens it in $EDITOR to fix and re-run; y yanks the error. Armable
	// from any screen (a table-list scratch can fail), so it's handled up here.
	if a.errView.active {
		switch msg.String() {
		case "e", "enter":
			a.errView.active = false
			return a, editorCmd(a.errView.seed)
		case "y":
			return a, yankCmd(a.errView.errText)
		case "esc", "q":
			a.errView.active = false
			a.status = "dismissed"
		case "j", "down":
			a.errView.scroll(1)
		case "k", "up":
			a.errView.scroll(-1)
		case "g":
			a.errView.off = 0
		case "G":
			a.errView.scroll(len(a.errView.lines))
		}
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
	if a.screen == screenBrowse && a.g().editing {
		return a.handleEditKey(msg)
	}
	// While editing a column filter, keys go to the filter input.
	if a.screen == screenBrowse && a.g().filtering >= 0 {
		return a.handleFilterKey(msg)
	}
	// Visual row-selection mode (V) is modal: only its movement/yank/exit keys
	// apply, so screen switches and other commands stay inert until it's left.
	if a.screen == screenBrowse && a.g().visualMode {
		return a.handleVisualKey(msg)
	}
	// The overlay commands below work from ANY screen (grid, table list, database
	// list, picker) — they inspect session-wide state, not the grid. They're gated
	// only on a.typing(), so a letter meant for a `/` filter stays literal; the
	// modal captures above have already had their say. Their overlays are drawn in
	// View() before the screen switch, so they render wherever they're opened.
	if !a.typing() {
		switch msg.String() {
		case " ": // arm the <space> leader (splits + focus movement)
			// Armed only here, never earlier: this sits after the modal captures, so
			// <space> can't arm behind the help sheet or the jumplist picker (typing()
			// is false while those are open). Grid-only, like the splits themselves.
			if a.screen == screenBrowse {
				a.leader = true
				return a, nil
			}
		case "?": // the help cheat sheet
			a.help.open(a.w-leftPad, a.h-1)
			return a, nil
		case "`": // the jumplist picker (inspect history, jump anywhere)
			if len(a.p().views) == 0 {
				a.status = "no navigation history yet"
				return a, nil
			}
			a.jumps.open(a.jumpEntries(), a.p().viewIdx, a.w-leftPad, a.h-1)
			return a, nil
		case "b": // the query-history buffer for this connection, most-recent first
			hist := a.history[a.connName]
			if len(hist) == 0 {
				a.status = "no query history yet"
				return a, nil
			}
			snap := make([]histEntry, len(hist))
			copy(snap, hist)
			a.histView.open(snap, a.w-leftPad, a.h-1)
			return a, nil
		case "ctrl+o": // jumplist back (vim Ctrl-O)
			return a.jumpBy(-1)
		case "ctrl+i", "tab": // jumplist forward — terminals send Ctrl-I as Tab
			return a.jumpBy(1)
		}
	}
	// Esc kills an in-flight DB op (a slow query, a big load). This takes
	// precedence over the grid's Esc (clear-filter), which only applies when
	// nothing is running.
	if msg.Type == tea.KeyEsc && a.cancel != nil {
		a.stop()
		a.g().loading = false
		a.status = "cancelled"
		return a, nil
	}
	switch a.screen {
	case screenPicker:
		return a.handlePickerKey(msg)

	case screenTables:
		return a.handleTablesKey(msg)

	case screenDatabases:
		return a.handleDatabasesKey(msg)

	case screenBrowse:
		switch msg.String() {
		case "backspace": // step left to the table list
			a.screen = screenTables
			a.layout()
			return a, nil
		case "d": // go to the database list
			return a.openDatabases()
		}
		return a.handleGridKey(msg)
	}
	return a, nil
}

// modalActive reports whether an overlay currently owns the keyboard. Keep this
// in step with the captures at the top of handleKey and the renders in View().
func (a App) modalActive() bool {
	return a.confirm.active || a.errView.active || a.help.active ||
		a.cell.active || a.jumps.active || a.histView.active
}

// typing reports whether a text input currently owns the keyboard: the grid's
// quick-edit cell or column filter, or a list screen's `/` filter. The global
// single-letter commands in handleKey are gated on it so a `b` or `?` meant for a
// filter is typed rather than swallowed as a command.
func (a App) typing() bool {
	switch a.screen {
	case screenBrowse:
		return a.g().editing || a.g().filtering >= 0
	case screenPicker:
		return a.connList.filtering
	case screenTables:
		return a.sidebar.filtering
	case screenDatabases:
		return a.dbs.filtering
	}
	return false
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
	if !a.allowSessionMove("switch to " + c.Name) { // the engine is shared by every pane
		return a, nil
	}
	a.syncCurrent()         // capture the current view before changing identity
	gen := a.startConnect() // snapshot identity (rollback on Esc) + op token + connecting
	a.connName, a.safe, a.pending = c.Name, c.Safe, c
	a.status = "connecting to " + c.Name + "…"
	if a.engine == nil { // initial connect (startup): connectCmd, quits on failure
		a.beginConnect(c)
		if a.connCmd != "" {
			// Picker mode already reserved the spinner loop (New/Init); ensureTick
			// returns nil so we don't start a second, self-perpetuating tick chain.
			return a, tea.Batch(connectCmd(gen, c), a.ensureTick())
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
	if !a.allowSessionMove("switch database") { // refuse here, not on Enter
		return a, nil
	}
	ctx := a.begin("loading databases", noPane)
	a.status = "loading databases…"
	return a, databasesCmd(ctx, a.gen, a.engine)
}

// sidebarNav applies the list-navigation keys shared by the picker, table, and
// database screens: arrows / Ctrl-N/P step within a column, ←/→ jump columns.
// Screen-specific keys and mode routing are handled by the caller.
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
	}
}

// sidebarFilterEdit handles the keys of a list's `/`-filter input, shared by the
// picker, table, and database screens: text edits (rune/Space/Backspace/Del,
// Ctrl-W word-delete), caret movement (←/→, Home/End, Ctrl-A/E), and ↑/↓ to browse
// through the matches. Enter (open the item) and Esc (cancel the filter) carry
// screen-specific meaning and stay with the caller, which checks s.filtering.
func sidebarFilterEdit(s *sidebar, msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyBackspace:
		s.filterBackspace()
	case tea.KeyDelete:
		s.filterDelete()
	case tea.KeyCtrlW:
		s.filterDeleteWord()
	case tea.KeyLeft:
		s.filterLeft()
	case tea.KeyRight:
		s.filterRight()
	case tea.KeyHome, tea.KeyCtrlA:
		s.filterHome()
	case tea.KeyEnd, tea.KeyCtrlE:
		s.filterEnd()
	case tea.KeyUp, tea.KeyCtrlP:
		s.move(-1)
	case tea.KeyDown, tea.KeyCtrlN:
		s.move(1)
	case tea.KeySpace:
		s.filterInput(" ")
	case tea.KeyRunes:
		s.filterInput(string(msg.Runes))
	}
}

// listKeys applies the navigation/filter keys shared by all three list screens
// (connection picker, table list, database list): filter-mode text editing, `/`
// to start a filter, Esc to clear it, and j/k/h/l/g/G + arrow movement. It
// returns false — leaving the key to the caller — only for Enter, Backspace, and
// unrecognized navigation-mode runes (e.g. T, s), whose meaning is screen-specific.
func listKeys(s *sidebar, msg tea.KeyMsg) bool {
	if s.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			return false // caller commits the filter and opens the item
		case tea.KeyEsc:
			s.clearFilter()
			return true
		default:
			sidebarFilterEdit(s, msg)
			return true
		}
	}
	switch msg.Type {
	case tea.KeyEnter, tea.KeyBackspace:
		return false // screen-specific: open the item / step left
	case tea.KeyEsc:
		if s.hasFilter() {
			s.clearFilter()
		}
		return true
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "/":
			s.startFilter()
		case "j":
			s.move(1)
		case "k":
			s.move(-1)
		case "h":
			s.move(-s.rows())
		case "l":
			s.move(s.rows())
		case "g":
			s.top()
		case "G":
			s.bottom()
		default:
			return false // e.g. T / s — screen-specific
		}
		return true
	}
	sidebarNav(s, msg)
	return true
}

// handlePickerKey drives the connection picker (screenPicker): shared list
// navigation, plus Enter to connect to the highlighted connection (moving right
// to its tables). The picker is the leftmost screen, so Backspace is a no-op.
func (a App) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if listKeys(&a.connList, msg) {
		return a, nil
	}
	if msg.Type == tea.KeyEnter {
		// Ignore a second Enter while a connect is already in flight (the loader
		// isn't shown yet in the no-loop initial-picker window).
		if a.connecting {
			return a, nil
		}
		a.connList.commitFilter() // an Enter from filter mode opens; leave nav mode
		if t, ok := a.connList.selected(); ok {
			if c, ok2 := a.findConn(t.Name); ok2 {
				return a.connectTo(c)
			}
		}
	}
	return a, nil
}

// handleDatabasesKey drives the full-screen database list (reached with `T`):
// navigation by default, `/` filters, Enter switches to that database, Backspace
// steps back to the table list, and Esc clears the filter.
func (a App) handleDatabasesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if listKeys(&a.dbs, msg) {
		return a, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		a.dbs.commitFilter() // an Enter from filter mode opens; leave nav mode
		if t, ok := a.dbs.selected(); ok {
			return a.switchDatabase(t.Name)
		}
	case tea.KeyBackspace:
		a.screen = screenTables // step back to the table list
		a.layout()
	}
	return a, nil
}

// switchDatabase reconnects the engine to name on the same server and reloads.
func (a App) switchDatabase(name string) (tea.Model, tea.Cmd) {
	if name == a.dbName { // already here → just go back to its tables
		a.screen = screenTables
		a.layout()
		return a, nil
	}
	if !a.allowSessionMove("switch to " + name) { // the engine is shared by every pane
		return a, nil
	}
	a.syncCurrent()         // save the current view into the session jumplist before leaving
	gen := a.startConnect() // snapshot for rollback + op token + arm the loader
	dsn := db.WithDatabase(a.pending.URL, name)
	a.pending.URL = dsn // so a later T swaps from the new database
	a.status = "connecting to " + name + "…"
	return a, openEngineCmd(gen, config.Conn{Name: a.connName}, dsn, false)
}

// handleTablesKey drives the full-screen table list: navigation by default (`/`
// filters), Enter opens the table (moving right to the grid), `d` jumps to the
// database list, Backspace steps left to the connection picker, and Esc clears the
// filter.
func (a App) handleTablesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if listKeys(&a.sidebar, msg) {
		return a, nil
	}
	// Only Enter, Backspace, and the screen-specific runes (d, s) reach here.
	switch msg.Type {
	case tea.KeyEnter:
		a.sidebar.commitFilter() // an Enter from filter mode opens; leave nav mode
		if t, ok := a.sidebar.selected(); ok {
			return a.selectTable(t)
		}
	case tea.KeyBackspace:
		// Backspace steps left to the connection picker (Connections → Tables →
		// Grid). Any committed filter is preserved. Like the database list, the
		// picker exists only to move the session, so it's refused while split
		// rather than letting Enter dead-end there.
		if len(a.conns) > 0 && a.allowSessionMove("switch connection") {
			a.screen = screenPicker
		}
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "d": // jump to the database list
			return a.openDatabases()
		case "s": // free-form scratch query for this connection/database
			return a, editorCmd(a.blankScratchSeed())
		}
	}
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
		table:     a.p().currentTable,
		basePreds: a.p().basePreds,
		baseNote:  a.p().baseNote,
		sortCol:   a.p().sortCol,
		sortAsc:   a.p().sortAsc,
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
	if a.p().currentTable.Name == "" {
		return
	}
	if a.p().viewIdx < 0 || a.p().viewIdx >= len(a.p().views) {
		return
	}
	prev := a.p().views[a.p().viewIdx]
	v := a.currentView()
	if a.p().adHoc {
		v.pos, v.snap = prev.pos, prev.snap
	} else {
		v.pos = a.g().pos()
		v.snap = a.g().snapshot()
	}
	a.viewSeq++
	v.seq = a.viewSeq
	a.p().views[a.p().viewIdx] = v
	a.evictSnaps()
}

// maxCachedViews bounds how many jumplist entries keep their full loaded rows in
// memory; older (least-recently-visited) snapshots are dropped to their metadata
// and reload on demand. Keeps the cache "reasonable" on a long session.
const maxCachedViews = 16

// evictSnaps drops the row cache from all but the maxCachedViews most recently
// touched views (by seq). The metadata (table, preds, sort, pos) is kept, so an
// evicted view still reloads and repositions — just not instantly.
//
// The bound is GLOBAL across every pane's jumplist, not per-pane: it exists to
// cap memory (each snapshot holds up to gridLimit() rows), and per-pane budgets
// would multiply that by the pane count. Note two panes cloned from one view
// share a *gridSnapshot, so it's counted once per referencing entry and nil-ing
// one doesn't free it — that only errs toward evicting more, which is safe.
func (a *App) evictSnaps() {
	type ref struct{ pane, view int }
	cached := []ref{}
	for pi := range a.panes {
		for vi := range a.panes[pi].views {
			if a.panes[pi].views[vi].snap != nil {
				cached = append(cached, ref{pi, vi})
			}
		}
	}
	if len(cached) <= maxCachedViews {
		return
	}
	seq := func(r ref) int { return a.panes[r.pane].views[r.view].seq }
	sort.Slice(cached, func(i, j int) bool { return seq(cached[i]) < seq(cached[j]) })
	for _, r := range cached[:len(cached)-maxCachedViews] {
		a.panes[r.pane].views[r.view].snap = nil
	}
}

// navigate records v as a new jump (dropping any forward history) and loads it.
func (a App) navigate(v viewState, label string) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	// Truncate the forward tail and append; the 3-index slice forces a fresh
	// backing array so the prior (value-copied) model isn't mutated.
	a.p().views = append(a.p().views[:a.p().viewIdx+1:a.p().viewIdx+1], v)
	if len(a.p().views) > 100 { // keep the list bounded
		a.p().views = a.p().views[len(a.p().views)-100:]
	}
	a.p().viewIdx = len(a.p().views) - 1
	return a.loadView(v, label)
}

// jumpBy steps the jumplist: d=-1 is Ctrl-O (back), d=+1 is Ctrl-I (forward).
func (a App) jumpBy(d int) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	ni := a.p().viewIdx + d
	if ni < 0 || ni >= len(a.p().views) {
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
	v := a.p().views[idx]
	// A jumplist entry can live in another database (the list spans them), but
	// panes share one engine — so reconnecting for this pane would pull the
	// database out from under the others, leaving them rendering rows from a
	// closed database under a header naming the new one. Refuse instead.
	if elsewhere, where := a.sessionMove(v.conn, v.db); elsewhere {
		if !a.allowSessionMove("jump to " + where) {
			return a, nil
		}
	}
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
		a.p().viewIdx = idx
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
		a.p().viewIdx = idx
		dsn := db.WithDatabase(a.pending.URL, v.db)
		a.pending.URL = dsn
		vv := v
		a.pendingView = &vv
		a.status = "connecting to " + v.db + "…"
		return a, openEngineCmd(gen, config.Conn{Name: a.connName}, dsn, false)
	}
	a.p().viewIdx = idx
	return a.loadView(v, "loading "+v.table.Name)
}

// sessionMove reports whether targeting conn/db would move the whole session
// (reopen the engine), and names where it's going. Empty conn/db mean "wherever
// we already are".
func (a App) sessionMove(conn, dbName string) (bool, string) {
	if conn != "" && conn != a.connName {
		return true, conn
	}
	if dbName != "" && dbName != a.dbName {
		return true, dbName
	}
	return false, ""
}

// allowSessionMove gates every connection/database change on there being exactly
// one pane, reporting the refusal in the status line when there isn't. what
// completes "close the split to …".
//
// The engine is session-wide: all panes read one database. Reopening it while
// split would leave the other panes showing rows from a database that's no
// longer open. Rather than half-updating them (or silently blanking the user's
// layout), a split makes the session's conn+db fixed — collapse it first.
// Lifting this needs per-pane engines (a refcounted registry keyed conn+db).
//
// Gate at the point the user ASKS, not at the last step: the connection picker
// and the database list exist only to move the session, so opening one while
// split and only refusing on Enter walks them into a dead end.
func (a *App) allowSessionMove(what string) bool {
	if len(a.panes) == 1 {
		return true
	}
	a.status = "close the split (<space>q) to " + what
	return false
}

// findConn looks up a configured connection by name.
func (a App) findConn(name string) (config.Conn, bool) {
	for _, c := range a.conns {
		if c.Name == name {
			return c, true
		}
	}
	return config.Conn{}, false
}

// jumpEntries is the picker's label list, current view synced in first.
func (a App) jumpEntries() []string {
	a.syncCurrent()
	out := make([]string, len(a.p().views))
	for i, v := range a.p().views {
		out[i] = v.label()
	}
	return out
}

// jumpTo loads the view at index i (the picker's Enter). Current/out-of-range → no-op.
func (a App) jumpTo(i int) (tea.Model, tea.Cmd) {
	a.syncCurrent()
	if i < 0 || i >= len(a.p().views) || i == a.p().viewIdx {
		return a, nil
	}
	return a.goToView(i)
}

// loadView switches to v. If v carries a cached snapshot (a revisited view), it
// restores instantly from memory — no DB round-trip; `r` refreshes if the data
// looks stale. Otherwise it reloads the table and repositions to v.pos once the
// rows arrive. Shared by selectTable, follow, and the jumplist.
func (a App) loadView(v viewState, label string) (App, tea.Cmd) {
	a.p().currentTable = v.table
	a.p().basePreds = v.basePreds
	a.p().baseNote = v.baseNote
	a.p().sortCol, a.p().sortAsc = v.sortCol, v.sortAsc
	if v.snap != nil { // instant restore from the in-memory cache
		a.stop()
		a.g().restore(v.snap)
		a.p().adHoc = false
		a.pendingPos, a.resetGrid = nil, false
		a.screen = screenBrowse
		a.layout()
		a.status = "" // the table crumb comes from tableSegment
		return a, nil
	}
	a.resetGrid = true
	a.g().clearFilters()
	pos := v.pos
	a.pendingPos = &pos // reposition to where we left, once the rows load
	ctx := a.begin(label, a.p().id)
	a.status = label + "…"
	return a, a.loadViewCmd(ctx, pos)
}

// loadViewCmd loads the current view for a jump, widening the fetch window if the
// remembered cursor sits past the default one (LIMIT/OFFSET from the top, so the
// row is only there if the window reaches it).
func (a App) loadViewCmd(ctx context.Context, pos gridPos) tea.Cmd {
	limit := a.gridLimit()
	if need := pos.cursorR + a.g().visibleRows() + 1; need > limit {
		limit = need
	}
	return loadCmd(ctx, a.gen, a.engine, a.p().currentTable, limit, a.p().sortCol, a.p().sortAsc, a.p().basePreds, a.g().filterSpecs())
}

// follow navigates the foreign key on the cursor's column to the row it points
// at. Resolution is in-memory (the FKs were fetched at load, on the grid) — the
// only DB work is loadView's reload, so no engine call happens in Update.
func (a App) follow() (tea.Model, tea.Cmd) {
	if a.p().adHoc {
		a.status = "follow unavailable on a query result"
		return a, nil
	}
	col, ok := a.g().currentColName()
	row, ok2 := a.g().currentRowMap()
	if !ok || !ok2 {
		return a, nil
	}
	fk, found := a.g().fkFor(col)
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
		if req, ok := a.g().commitEdit(); ok {
			if a.safe {
				return a.askMutation(previewEditSQL(a.engine, req), "saving",
					func(ctx context.Context, gen int) tea.Cmd { return execEditCmd(ctx, gen, a.engine, req) })
			}
			ctx := a.begin("saving", a.p().id)
			a.status = "saving…"
			return a, execEditCmd(ctx, a.gen, a.engine, req)
		}
		a.status = ""
	case tea.KeyEsc:
		a.g().cancelEdit()
		a.status = ""
	case tea.KeyBackspace:
		a.g().editBackspace()
	case tea.KeyDelete:
		a.g().editDelete()
	case tea.KeyLeft:
		a.g().editLeft()
	case tea.KeyRight:
		a.g().editRight()
	case tea.KeyHome, tea.KeyCtrlA:
		a.g().editHome()
	case tea.KeyEnd, tea.KeyCtrlE:
		a.g().editEnd()
	case tea.KeyCtrlW:
		a.g().editDeleteWord()
	case tea.KeySpace:
		a.g().editInput(" ")
	case tea.KeyRunes:
		a.g().editInput(string(msg.Runes))
	}
	return a, nil
}

// handleFilterKey routes keys while a column filter is being typed (§7.1).
func (a App) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		a.g().commitFilter()
		ctx := a.begin("filtering", a.p().id)
		return a, a.loadCurrentCmd(ctx)
	case tea.KeyEsc:
		a.g().clearFilter()
		ctx := a.begin("loading", a.p().id)
		return a, a.loadCurrentCmd(ctx)
	case tea.KeyBackspace:
		a.g().filterBackspace()
	case tea.KeyDelete:
		a.g().filterDelete()
	case tea.KeyCtrlW:
		a.g().filterDeleteWord()
	case tea.KeyLeft:
		a.g().filterLeft()
	case tea.KeyRight:
		a.g().filterRight()
	case tea.KeyHome, tea.KeyCtrlA:
		a.g().filterHome()
	case tea.KeyEnd, tea.KeyCtrlE:
		a.g().filterEnd()
	case tea.KeyDown:
		a.g().moveRow(1)
	case tea.KeyUp:
		a.g().moveRow(-1)
	case tea.KeySpace:
		a.g().filterInput(" ")
	case tea.KeyRunes:
		a.g().filterInput(string(msg.Runes))
	}
	return a, nil
}

func (a App) handleGridKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		a.g().moveRow(1)
	case "k", "up":
		a.g().moveRow(-1)
	case "h", "left":
		a.g().moveCol(-1)
	case "l", "right":
		a.g().moveCol(1)
	case "g":
		a.g().top()
	case "G":
		a.g().bottom()
	case "0":
		a.g().firstCol()
	case "$":
		a.g().lastCol()
	case "/":
		if a.p().adHoc {
			a.status = "filter unavailable on a query result"
			return a, nil
		}
		a.g().startFilter()
	case "esc":
		if a.g().clearCurrentFilter() {
			ctx := a.begin("loading", a.p().id)
			return a, a.loadCurrentCmd(ctx)
		}
	case "e":
		if !a.g().editable() {
			a.status = "not editable — no single-table primary key"
		} else if a.g().startEdit() {
			a.status = "editing " + a.g().cols[a.g().editC].name + " — Enter saves, Esc cancels"
		}
		return a, nil
	case "E":
		if !a.g().editable() {
			a.status = "not editable — no single-table primary key"
		} else if col, val, keys, ok := a.g().fullEditTarget(); ok {
			return a, editorCmd(buildUpdateStmt(a.engine, a.g().table, col, val, keys))
		}
		return a, nil
	case "o":
		if !a.g().editable() {
			a.status = "not editable — no single-table primary key"
		} else {
			ctx := a.begin("preparing insert", a.p().id)
			a.status = "preparing insert…"
			return a, prepareInsertCmd(ctx, a.gen, a.engine, a.p().currentTable)
		}
		return a, nil
	case "D":
		if !a.g().editable() {
			a.status = "not editable — no single-table primary key"
		} else if keys, ok := a.g().rowKeys(); ok {
			return a, editorCmd(buildDeleteStmt(a.engine, a.g().table, keys))
		}
		return a, nil
	case "p":
		if !a.g().editable() {
			a.status = "not editable — no single-table primary key"
		} else if vals, ok := a.g().currentRowValues(); ok {
			ctx := a.begin("preparing duplicate", a.p().id)
			a.status = "preparing duplicate…"
			return a, prepareDuplicateCmd(ctx, a.gen, a.engine, a.p().currentTable, vals)
		}
		return a, nil
	case "y":
		if s, ok := a.g().yankCell(); ok {
			a.status = fmt.Sprintf("copied cell (%d chars)", len(s))
			return a, yankCmd(s)
		}
	case "Y":
		if s, ok := a.g().currentRowJSON(); ok {
			a.status = "copied row as JSON"
			return a, yankCmd(s)
		}
	case "V":
		if len(a.g().visible) > 0 {
			a.g().enterVisual()
			a.status = "visual row select — j/k extend, o swap end, y yank, Esc cancel"
		}
		return a, nil
	case "enter":
		// On an FK column, follow the reference — inspecting a foreign key in
		// a full-cell viewer is never what you want. Otherwise open the cell.
		if col, ok := a.g().currentColName(); ok && !a.p().adHoc {
			if _, isFK := a.g().fkFor(col); isFK {
				return a.follow()
			}
		}
		if v, col, ok := a.g().currentCell(); ok {
			a.cell.open(col, a.g().cursorR, v, a.g().w, a.g().h)
		}
		return a, nil // opening a modal must not fall through to maybeLoadMore
	case "J":
		if a.p().adHoc {
			a.status = "sort unavailable on a query result"
			return a, nil
		}
		if name, ok := a.g().currentColName(); ok {
			a.p().sortCol, a.p().sortAsc = name, true
			ctx := a.begin("sorting", a.p().id)
			return a, a.loadCurrentCmd(ctx)
		}
	case "K":
		if a.p().adHoc {
			a.status = "sort unavailable on a query result"
			return a, nil
		}
		if name, ok := a.g().currentColName(); ok {
			a.p().sortCol, a.p().sortAsc = name, false
			ctx := a.begin("sorting", a.p().id)
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

// handleVisualKey routes keys while visual row-selection mode is active. Only
// movement (extends the selection), o (swap the moving edge), y (yank the
// selection as JSON and exit), and Esc (cancel) apply; everything else is inert.
func (a App) handleVisualKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		a.g().moveRow(1)
	case "k", "up":
		a.g().moveRow(-1)
	case "g":
		a.g().top()
	case "G":
		a.g().bottom()
	case "o":
		a.g().visualSwap()
	case "y":
		s, n, ok := a.g().yankSelectionJSON()
		a.g().exitVisual()
		if ok {
			a.status = fmt.Sprintf("%d rows copied to clipboard", n)
			return a, yankCmd(s)
		}
		return a, nil
	case "esc":
		a.g().exitVisual()
		a.status = "visual select cancelled"
		return a, nil
	default:
		return a, nil
	}
	// Movement may have neared the loaded edge — extend the buffer as usual.
	cmd := a.maybeLoadMore()
	return a, cmd
}

// maybeLoadMore triggers a continuous-scroll fetch when the cursor nears the
// loaded edge and more rows exist. It never fires while another op is in flight:
// begin() would cancel that op, so a cursor move during a sort/filter/reload
// could silently supersede it and append the next window onto the stale rows.
//
// With splits that guard is GLOBAL — there is one op slot, so a slow load in one
// pane stalls scroll-fetch in another until it lands (the cursor just stops at
// the loaded edge). Correct for a single slot; per-pane concurrency would need
// per-pane activity/cancel/gen.
func (a *App) maybeLoadMore() tea.Cmd {
	if a.activity != "" || !a.g().wantMore() {
		return nil
	}
	a.g().loading = true
	anchor, _ := a.g().lastRowMap() // keyset cursor; nil falls back to OFFSET
	ctx := a.begin("loading more", a.p().id)
	return loadMoreCmd(ctx, a.gen, a.engine, a.p().currentTable, a.p().sortCol, a.p().sortAsc,
		a.p().basePreds, a.g().filterSpecs(), anchor, len(a.g().rows), a.gridLimit())
}

// scratchSeed is the prefill for s: this table's last scratch query if one was
// run, else the SELECT template. remember ties the eventual submit back to the
// table so the loop continues.
func (a App) scratchSeed() editorSeed {
	sql := selectTemplate(a.engine, a.p().currentTable.Ref())
	if last, ok := a.lastQuery[a.queryKey(a.p().currentTable)]; ok {
		sql = last
	}
	return editorSeed{sql: sql, remember: a.p().currentTable}
}

// blankScratchSeed is the prefill for s from the table list: an empty scratch
// buffer headed by a comment naming the connection and database you're in, so a
// free-form query starts from a clean slate (not a table's SELECT template). The
// cursor lands on the empty line under the comment; scratch=true records it in the
// connection's `b` history on submit even though there's no table.
func (a App) blankScratchSeed() editorSeed {
	name := a.connName
	if name == "" {
		name = "scratch"
	}
	head := "-- " + name
	if a.dbName != "" {
		head += " · " + a.dbName
	}
	return editorSeed{sql: head + "\n\n", line: 2, col: 1, scratch: true}
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
		ctx := a.begin("running query", a.p().id)
		a.status = "running query…"
		return a, runQueryCmd(ctx, a.gen, a.engine, e.sql, a.histSeed(e.sql))
	}
	return a, editorCmd(a.histSeed(e.sql))
}

// histSeed opens a history query in $EDITOR to evolve it: seeded with the SQL
// and marked (remember) so a :wq re-records it and continues the s edit loop.
func (a App) histSeed(sql string) editorSeed {
	if !strings.HasSuffix(sql, "\n") {
		sql += "\n"
	}
	return editorSeed{sql: sql, remember: a.p().currentTable}
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
	return a.loadPaneCmd(ctx, a.p())
}

// loadPaneCmd reloads a specific pane's view. Handlers resolving a result's pane
// via opTarget must use this rather than loadCurrentCmd: focus can move while an
// op is in flight, so "current" and "the pane this op belongs to" can differ.
func (a App) loadPaneCmd(ctx context.Context, p *pane) tea.Cmd {
	return loadCmd(ctx, a.gen, a.engine, p.currentTable, a.gridLimit(), p.sortCol, p.sortAsc, p.basePreds, p.grid.filterSpecs())
}

// reloadView re-runs the current view (`r`): a table reload keeps the sort,
// followed-FK predicate, column filters, and cursor; an adHoc result re-runs its
// query. A no-op when there's nothing loaded yet.
func (a App) reloadView() (tea.Model, tea.Cmd) {
	if a.p().adHoc {
		if a.p().adHocQuery == "" {
			return a, nil
		}
		ctx := a.begin("reloading", a.p().id)
		a.status = "reloading…"
		return a, runQueryCmd(ctx, a.gen, a.engine, a.p().adHocQuery, editorSeed{sql: a.p().adHocQuery})
	}
	if a.p().currentTable.Name == "" {
		return a, nil
	}
	ctx := a.begin("reloading", a.p().id)
	a.status = "reloading…"
	return a, a.loadCurrentCmd(ctx)
}

// gridLimit is the fetch window, sized to the viewport: a few screenfuls, so a
// tall terminal loads more per fetch and a short one less. Floored so we always
// over-fetch well past the visible rows (smooth scroll, not a fetch per
// keystroke) and capped to bound a single round-trip. Used for the initial load
// and each scroll window alike.
func (a App) gridLimit() int {
	return clamp((a.h-1)*4, 100, 500)
}

func (a App) View() string {
	if a.isConnecting() {
		return a.connectingView()
	}
	// The safe-mode confirmation is modal — it captures every key — and can be armed
	// from any screen (e.g. a scratch write from the table list), so it's rendered
	// here, before the screen switch, rather than only in browseView.
	if a.confirm.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.confirm.View())
		return a.statusLine() + "\n" + body
	}
	// The failed-statement modal is likewise armable from any screen, so it's
	// rendered here before the screen switch (cf. confirm).
	if a.errView.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.errView.View())
		return a.statusLine() + "\n" + body
	}
	// The help sheet, jumplist picker, and query-history buffer are opened by
	// global keys (?, `, b) from any screen, so they too are drawn before the
	// screen switch — rendering them only in browseView would make them openable
	// on a list screen but invisible there.
	if a.help.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.help.View())
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
	switch a.screen {
	case screenPicker:
		return lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.connList.View())
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
	// (confirm/errView and the help/jumplist/history overlays are handled in
	// View() — they can be armed from any screen. Only the cell viewer is
	// grid-only: it shows the cell under the grid cursor.)
	if a.cell.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.cell.View())
		return a.statusLine() + "\n" + body
	}
	body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.panesView())
	return a.statusLine() + "\n" + body
}

// panesView renders the layout: each pane clamped to its rect, stacked within
// its column, the columns joined left→right with a divider between them.
// Unsplit it's just the grid, exactly as before.
//
// Stacked panes need no horizontal rule — each pane's own header line already
// separates it from the one above.
func (a App) panesView() string {
	if len(a.panes) == 1 {
		return a.g().View()
	}
	cols := a.paneCols()
	blocks := make([]string, 0, len(cols)*2-1)
	for ci, idxs := range cols {
		if ci > 0 {
			blocks = append(blocks, a.dividerView())
		}
		parts := make([]string, 0, len(idxs))
		for _, i := range idxs {
			parts = append(parts, a.paneView(i))
		}
		blocks = append(blocks, lipgloss.JoinVertical(lipgloss.Left, parts...))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, blocks...)
}

// paneView renders one pane: its header line, then its grid, clamped to the rect.
//
// The clamp is load-bearing, not cosmetic. grid.View's renderRow writes a whole
// cell before checking x >= g.w, so a line can overrun the pane's width; joined
// horizontally, the widest line would decide the block width and shove every
// pane to its right. Height() likewise pads a short (or empty — grid.View
// returns "" with no columns) pane, which JoinHorizontal would otherwise collapse.
//
// It must be lipgloss and not runewidth.Truncate: the grid emits ANSI styling,
// which runewidth counts as width and would slice mid-escape.
func (a App) paneView(i int) string {
	p := &a.panes[i]
	body := lipgloss.NewStyle().
		Width(p.w).MaxWidth(p.w).
		Height(p.h).MaxHeight(p.h).
		Render(p.grid.View())
	return a.paneHeader(i) + "\n" + body
}

// dividerView is the rule between two columns: full body height, since a column
// spans the body however many panes are stacked in it.
func (a App) dividerView() string {
	h := a.h - 1 // the global status line
	if h < 1 {
		h = 1
	}
	col := strings.TrimSuffix(strings.Repeat("│\n", h), "\n")
	return lipgloss.NewStyle().Foreground(dividerColor).Render(col)
}

// dividerColor is the rule between split panes — faint enough to read as chrome.
var dividerColor = lipgloss.Color("8")

// paneHeader is a split pane's own crumb line: which table it's showing and its
// own paging counter, since the one global status line can only speak for the
// focused pane. The focused pane's is highlighted — that's the only indication
// of where the keys are going.
//
// Unsplit there is no pane header at all: the global status line already names
// the table, and a second line of chrome would cost a row of data.
func (a App) paneHeader(i int) string {
	p := &a.panes[i]
	name := p.tableSegment()
	if name == "" {
		name = "(no table)"
	}
	right := ""
	if len(p.grid.visible) > 0 {
		row, loaded, more := p.grid.posSummary()
		right = fmt.Sprintf("%d/%d", row, loaded)
		if more {
			right += "↓"
		}
	}
	// Fit name + right into the pane width, truncating the name (not the counter).
	gap := p.w - runewidth.StringWidth(right)
	if gap < 0 {
		gap = 0
	}
	line := runewidth.Truncate(name, gap, "…")
	line = runewidth.FillRight(line, gap) + right
	if i == a.focus {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(line)
	}
	return lipgloss.NewStyle().Faint(true).Render(line)
}

// tableSegment is the pane's table crumb: the table it's showing, suffixed with
// the followed-FK predicate when there is one. Empty before anything is opened.
func (p *pane) tableSegment() string {
	if p.currentTable.Name == "" {
		return ""
	}
	if p.baseNote != "" { // a followed-FK view: show the predicate
		return p.currentTable.Name + " · " + p.baseNote
	}
	return p.currentTable.Name
}

// tableSegment is the header's table crumb for the focused pane.
func (a App) tableSegment() string {
	if a.p().currentTable.Name == "" {
		return ""
	}
	if a.p().baseNote != "" { // a followed-FK view: show the predicate
		return a.p().currentTable.Name + " · " + a.p().baseNote
	}
	return a.p().currentTable.Name
}

// statusLine renders "connName > dbName > table > message". The table segment is
// derived from the live view (not from a.status), so a transient message appends
// a segment rather than overwriting the table — the current table stays visible.
//
// When split, the table crumb and the paging counter move to each pane's own
// header (a single line can't speak for two panes) and this keeps only the
// session-wide "connName > dbName > message". Unsplit — the overwhelmingly common
// case — it renders exactly as it always has, so the split costs no chrome.
func (a App) statusLine() string {
	split := len(a.panes) > 1
	name := a.connName
	if name == "" {
		name = "adhoc"
	}
	rest := []string{}
	if a.dbName != "" {
		rest = append(rest, a.dbName)
	}
	if seg := a.tableSegment(); seg != "" && !split {
		rest = append(rest, seg)
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
	// Right side: the activity spinner while a DB op runs, else (in the grid, with
	// rows loaded) a compact paging hint — cursor row / loaded count, with a ↓ when
	// more rows exist below the loaded buffer.
	var right string
	switch {
	case a.activity != "":
		// spinner + label + a hint that Esc kills it.
		frame := string(spinnerFrames[a.spinner%len(spinnerFrames)])
		right = activityStyle.Render(frame + " " + a.activity + " · esc ")
	case a.screen == screenBrowse && !split && len(a.g().visible) > 0:
		row, loaded, more := a.g().posSummary()
		s := fmt.Sprintf("%d/%d", row, loaded)
		if more {
			s += "↓"
		}
		right = faint.Render(s + " ")
	}
	if right == "" {
		return left
	}
	gap := a.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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
