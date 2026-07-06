// Package tui is the bubbletea front end: picker → flat table list → grid.
package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

type screen int

const (
	screenPicker screen = iota // connection picker (bare `jsq`)
	screenBrowse               // the results grid
	screenTables               // the full-screen table list
)

const leftPad = 1 // 1-char blank margin before the table list / grid

// App is the root bubbletea Model.
type App struct {
	screen screen
	picker picker

	connName string
	readOnly bool        // connection refuses mutations (read_only in config)
	pending  config.Conn // the direct/selected connection (empty URL → picker); cmd lives here

	// While a `cmd`-backed connection is being established, connCmd/connAddr drive
	// the full-screen waiting view (what we ran + the port we're waiting for); both
	// are cleared once the connect resolves. ticking guards the one spinner loop.
	connCmd  string
	connAddr string
	ticking  bool

	engine  db.Engine
	sidebar sidebar // the full-screen table list (screenTables)
	grid    grid
	cell    cellView
	help    help

	currentTable db.Table
	basePreds    []eqPred // followed-FK equality filter, AND-ed into every load until the table changes
	baseNote     string   // human form of basePreds, shown in the status line
	sortCol      string
	sortAsc      bool

	// Jumplist: visited views oldest→newest with viewIdx marking the current one.
	// Ctrl-O/Ctrl-I step it; the ` picker jumps to any entry. A new navigation
	// truncates the forward tail and appends. A view is {table, FK-filter, sort}
	// (column filters aren't captured).
	views     []viewState
	viewIdx   int      // index of the current view; -1 before the first
	jumps     jumpView // the `-key jumplist picker overlay
	resetGrid bool     // reset cursor on next rows load (new table, not a re-sort)
	adHoc     bool     // grid shows a free-form (s) query result, not a table

	lastQuery map[db.Table]string // per-table last scratch (s) query, for the edit loop

	dbName         string
	w, h           int
	status         string
	postExecStatus string // shown after the reload that follows a full-path exec
	err            error  // mid-session failure → in-app error screen
	fatalErr       error  // connect failure → carried out to main, printed to stderr

	// Header activity indicator (top-right): activity names the in-flight DB op
	// (empty → nothing shown), cancel kills it (Esc), spinner is the frame index.
	activity string
	cancel   context.CancelFunc
	spinner  int
}

// New builds the root model. If direct.URL != "", it connects to that connection
// directly; else it shows the picker. readOnly/cmd all ride along on direct (§8).
func New(conns []config.Conn, direct config.Conn) App {
	a := App{
		picker:    picker{conns: conns},
		grid:      newGrid(),
		connName:  direct.Name,
		readOnly:  direct.ReadOnly,
		pending:   direct,
		lastQuery: map[db.Table]string{},
		viewIdx:   -1,
	}
	if direct.URL != "" {
		a.screen = screenBrowse
		a.status = "connecting…"
		a.beginConnect(direct)
	}
	return a
}

// FatalErr is the connect failure that quit the app, if any — main reads it from
// the returned model and prints it to stderr.
func (a App) FatalErr() error { return a.fatalErr }

func (a App) Init() tea.Cmd {
	if a.pending.URL == "" {
		return nil
	}
	if a.connCmd != "" {
		// New already armed the waiting view + spinner loop; animate it while
		// connectCmd runs the cmd/wait/open in the background.
		return tea.Batch(connectCmd(a.pending), tickCmd())
	}
	return connectCmd(a.pending)
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

// ensureTick starts the spinner loop unless one is already running — so a
// waiting-view connect and connectedMsg don't spin up two loops.
func (a *App) ensureTick() tea.Cmd {
	if a.ticking {
		return nil
	}
	a.ticking = true
	return tickCmd()
}

// begin marks a new in-flight DB op labelled for the header indicator, cancels
// any previous op, and returns a cancellable context for the op's command. The
// stored cancel func is what Esc calls (see stop). Callers must set a.activity
// via this before dispatching a cancellable tea.Cmd.
func (a *App) begin(label string) context.Context {
	a.stop()
	a.activity = label
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	return ctx
}

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
		// No session to fall back to — quit and let main print it to stderr.
		a.stop()
		a.fatalErr = msg.err
		return a, tea.Quit

	case errMsg:
		a.stop()
		a.connCmd, a.connAddr = "", "" // dismiss the waiting view
		a.err = msg.err
		return a, nil

	case tickMsg:
		if a.activity != "" || a.connCmd != "" {
			a.spinner++
		}
		return a, tickCmd()

	case connectedMsg:
		a.connCmd, a.connAddr = "", "" // dismiss the waiting view
		a.engine = msg.engine
		if msg.name != "" {
			a.connName = msg.name
		}
		a.dbName = msg.dbName
		a.sidebar.setTables(msg.tables)
		a.screen = screenTables // land on the table list
		a.layout()
		a.status = ""
		// Kick off the perpetual spinner tick (unless the waiting view already
		// started it); it idles invisibly and animates only during a DB op.
		return a, a.ensureTick()

	case rowsMsg:
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
		if a.resetGrid {
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
		a.stop()
		a.grid.appendRows(msg.rows, msg.full)
		return a, nil

	case editDoneMsg:
		a.stop()
		// A keyed edit must touch exactly one row; anything else is loud (§8).
		if msg.affected == 1 {
			a.grid.applyEdit(msg.val)
			a.status = fmt.Sprintf("set %s = '%s'", msg.col, msg.val)
		} else {
			a.status = fmt.Sprintf("⚠ %s: %d rows affected, expected 1", msg.col, msg.affected)
		}
		return a, nil

	case editorReadyMsg:
		a.stop() // insert/duplicate prep finished; the editor spawn isn't a DB op
		return a, editorCmd(msg.seed)

	case editorSubmitMsg:
		// Remember an s query as its table's last query, so the next s prefills
		// it (even if it errors) for a tight edit-run-edit loop.
		if msg.remember.Name != "" {
			a.lastQuery[msg.remember] = msg.sql
		}
		// A read (s) shows its rows; a mutation runs via Exec — and is refused on a
		// read-only connection (the s mutation guard; E/o/D/p are already blocked
		// at the key).
		if isReadSQL(msg.sql) {
			ctx := a.begin("running query")
			a.status = "running query…"
			return a, runQueryCmd(ctx, a.engine, msg.sql)
		}
		if a.readOnly {
			a.status = "read-only connection — statement not run"
			return a, nil
		}
		ctx := a.begin("running")
		a.status = "running…"
		return a, execRawCmd(ctx, a.engine, msg.sql)

	case editorAbortedMsg:
		a.stop()
		a.status = "edit cancelled"
		return a, nil

	case queryResultMsg:
		a.stop()
		a.grid.setResult(msg.rs)
		a.grid.setSort("", false)
		a.grid.clearFilters()
		a.grid.hasMore = false
		a.grid.loading = false
		a.grid.reset()
		a.adHoc = true
		a.screen = screenBrowse
		a.layout()
		a.status = fmt.Sprintf("query — %d row(s)", len(msg.rs.Rows))
		return a, nil

	case execDoneMsg:
		// Reload the current view so the change is reflected; the affected count
		// survives the reload via postExecStatus.
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
	// Grid and table list each own the whole body (separate full-screen pages).
	a.grid.setSize(avail, bodyH)
	a.sidebar.w, a.sidebar.h = avail, bodyH
}

func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
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
		case "enter":
			// Ignore a second Enter while a connect is already in flight — the
			// probe/open runs in the background with the picker still on screen, and
			// re-triggering would start a duplicate engine (and `run` process) whose
			// handle we'd then leak. pending.URL is empty until the first Enter.
			if a.pending.URL != "" {
				return a, nil
			}
			if c, ok := a.picker.selected(); ok {
				a.connName = c.Name
				a.readOnly = c.ReadOnly
				a.pending = c
				a.status = "connecting to " + c.Name + "…"
				a.beginConnect(c)
				if a.connCmd != "" {
					return a, tea.Batch(connectCmd(c), tickCmd())
				}
				return a, connectCmd(c)
			}
		}
		return a, nil

	case screenTables:
		return a.handleTablesKey(msg)

	case screenBrowse:
		switch msg.String() {
		case "ctrl+o": // jumplist back (vim Ctrl-O)
			return a.jumpBy(-1)
		case "ctrl+i", "tab": // jumplist forward — terminals send Ctrl-I as Tab
			return a.jumpBy(1)
		case "t": // go to the table list
			a.screen = screenTables
			a.layout()
			return a, nil
		}
		return a.handleGridKey(msg)
	}
	return a, nil
}

// handleTablesKey drives the full-screen table list: typing narrows it (no `/`),
// arrows / Ctrl-N/P move, Enter opens, Esc clears the filter or returns to the grid.
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
	case tea.KeyUp, tea.KeyCtrlP:
		a.sidebar.move(-1)
	case tea.KeyDown, tea.KeyCtrlN:
		a.sidebar.move(1)
	case tea.KeyBackspace:
		a.sidebar.filterBackspace()
	case tea.KeySpace:
		a.sidebar.filterInput(" ")
	case tea.KeyRunes:
		a.sidebar.filterInput(string(msg.Runes))
	}
	return a, nil
}

// viewState is a jumplist entry: enough to reload a table exactly as it was
// (bar column filters, which aren't captured).
type viewState struct {
	table     db.Table
	basePreds []eqPred
	baseNote  string
	sortCol   string
	sortAsc   bool
}

func (a App) currentView() viewState {
	return viewState{
		table:     a.currentTable,
		basePreds: a.basePreds,
		baseNote:  a.baseNote,
		sortCol:   a.sortCol,
		sortAsc:   a.sortAsc,
	}
}

// label is the jumplist picker's one-line description of a view.
func (v viewState) label() string {
	s := v.table.Name
	if v.baseNote != "" {
		s += " · " + v.baseNote
	}
	return s
}

// syncCurrent refreshes the current entry with the live view (so a sort made
// after arriving is remembered) before any jumplist move.
func (a *App) syncCurrent() {
	if a.viewIdx >= 0 && a.viewIdx < len(a.views) {
		a.views[a.viewIdx] = a.currentView()
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
	a.viewIdx = ni
	return a.loadView(a.views[ni], "loading "+a.views[ni].table.Name)
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
	a.viewIdx = i
	return a.loadView(a.views[i], "loading "+a.views[i].table.Name)
}

// loadView switches to v and reloads it (default cursor, cleared column filters).
// Shared by selectTable, follow, and the jumplist.
func (a App) loadView(v viewState, label string) (App, tea.Cmd) {
	a.currentTable = v.table
	a.basePreds = v.basePreds
	a.baseNote = v.baseNote
	a.sortCol, a.sortAsc = v.sortCol, v.sortAsc
	a.resetGrid = true
	a.grid.clearFilters()
	ctx := a.begin(label)
	a.status = label + "…"
	return a, a.loadCurrentCmd(ctx)
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
	return a.navigate(viewState{table: t, sortAsc: true}, "loading "+t.Name)
}

// handleEditKey routes keys while a cell is being edited (§8 quick path). Enter
// builds the keyed UPDATE and runs it immediately; Esc cancels. A bare Enter
// with no typing is a no-op (commitEdit returns ok=false), so a NULL cell can't
// be blanked by accident.
func (a App) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if req, ok := a.grid.commitEdit(); ok {
			ctx := a.begin("saving")
			a.status = "saving…"
			return a, execEditCmd(ctx, a.engine, req)
		}
		a.status = a.currentTable.Name
	case tea.KeyEsc:
		a.grid.cancelEdit()
		a.status = a.currentTable.Name
	case tea.KeyBackspace:
		a.grid.editBackspace()
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
		if a.readOnly {
			a.status = "read-only connection — editing disabled"
		} else if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if a.grid.startEdit() {
			a.status = "editing " + a.grid.cols[a.grid.editC].name + " — Enter saves, Esc cancels"
		}
		return a, nil
	case "E":
		if a.readOnly {
			a.status = "read-only connection — editing disabled"
		} else if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if col, val, keys, ok := a.grid.fullEditTarget(); ok {
			return a, editorCmd(buildUpdateStmt(a.engine, a.grid.table, col, val, keys))
		}
		return a, nil
	case "o":
		if a.readOnly {
			a.status = "read-only connection — editing disabled"
		} else if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else {
			ctx := a.begin("preparing insert")
			a.status = "preparing insert…"
			return a, prepareInsertCmd(ctx, a.engine, a.currentTable)
		}
		return a, nil
	case "D":
		if a.readOnly {
			a.status = "read-only connection — editing disabled"
		} else if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if keys, ok := a.grid.rowKeys(); ok {
			return a, editorCmd(buildDeleteStmt(a.engine, a.grid.table, keys))
		}
		return a, nil
	case "p":
		if a.readOnly {
			a.status = "read-only connection — editing disabled"
		} else if !a.grid.editable() {
			a.status = "not editable — no single-table primary key"
		} else if vals, ok := a.grid.currentRowValues(); ok {
			ctx := a.begin("preparing duplicate")
			a.status = "preparing duplicate…"
			return a, prepareDuplicateCmd(ctx, a.engine, a.currentTable, vals)
		}
		return a, nil
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
	}
	// After a movement, fetch the next window if the cursor neared the edge.
	// Evaluate the command first so its state mutation (activity/loading) lands
	// on the model we return.
	cmd := a.maybeLoadMore()
	return a, cmd
}

// maybeLoadMore triggers a continuous-scroll fetch when the cursor nears the
// loaded edge and more rows exist.
func (a *App) maybeLoadMore() tea.Cmd {
	if !a.grid.wantMore() {
		return nil
	}
	a.grid.loading = true
	ctx := a.begin("loading more")
	return loadMoreCmd(ctx, a.engine, a.currentTable, a.sortCol, a.sortAsc,
		a.basePreds, a.grid.filterSpecs(), len(a.grid.rows), a.gridLimit())
}

// scratchSeed is the prefill for s: this table's last scratch query if one was
// run, else the SELECT template. remember ties the eventual submit back to the
// table so the loop continues.
func (a App) scratchSeed() editorSeed {
	sql := selectTemplate(a.engine, a.currentTable.Ref())
	if last, ok := a.lastQuery[a.currentTable]; ok {
		sql = last
	}
	return editorSeed{sql: sql, remember: a.currentTable}
}

// loadCurrentCmd (re)loads the current table with the active sort, any followed-FK
// base predicate, and the column filters.
func (a App) loadCurrentCmd(ctx context.Context) tea.Cmd {
	return loadCmd(ctx, a.engine, a.currentTable, a.gridLimit(), a.sortCol, a.sortAsc, a.basePreds, a.grid.filterSpecs())
}

func (a App) gridLimit() int {
	if n := (a.h - 2) * 4; n >= 200 {
		return n
	}
	return 200
}

func (a App) View() string {
	if a.err != nil {
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
		if a.w > 0 {
			style = style.Width(a.w) // soft-wrap long errors instead of clipping
		}
		return style.Render("error: "+a.err.Error()) + "\n\npress ctrl-c to quit"
	}
	if a.connCmd != "" {
		return a.connectingView()
	}
	switch a.screen {
	case screenPicker:
		return lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.picker.View())
	case screenTables:
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.sidebar.View())
		return a.statusLine() + "\n" + body
	case screenBrowse:
		return a.browseView()
	}
	return ""
}

func (a App) browseView() string {
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
	body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.grid.View())
	return a.statusLine() + "\n" + body
}

// statusLine renders "connName > dbName > table" (table = current table / msg).
func (a App) statusLine() string {
	name := a.connName
	if name == "" {
		name = "adhoc"
	}
	parts := []string{name}
	if a.dbName != "" {
		parts = append(parts, a.dbName)
	}
	if a.status != "" {
		parts = append(parts, a.status)
	}
	left := lipgloss.NewStyle().Faint(true).Render(" " + strings.Join(parts, " > ") + " ")
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

// connectingView is the full-screen loader shown while a `cmd`-backed connection
// starts its helper and waits for the port — it names what we ran and where.
func (a App) connectingView() string {
	frame := string(spinnerFrames[a.spinner%len(spinnerFrames)])
	label := lipgloss.NewStyle().Faint(true)
	head := "connecting"
	if a.connName != "" {
		head += " to " + a.connName
	}
	var b strings.Builder
	b.WriteString("\n " + activityStyle.Render(frame) + " " + head + "\n\n")
	// The cmd can be long; soft-wrap it (indented under the label) so it never
	// clips off the right edge.
	b.WriteString(" " + label.Render("running") + "  " + a.wrapIndent(a.connCmd, 10) + "\n")
	if a.connAddr != "" {
		b.WriteString(" " + label.Render("waiting") + "  " + a.connAddr + " …\n")
	}
	b.WriteString("\n " + label.Render("ctrl-c to abort") + "\n")
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
