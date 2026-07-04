// Package tui is the bubbletea front end: picker → flat table list → grid.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jmserra/jsq/internal/config"
	"github.com/jmserra/jsq/internal/db"
)

type screen int

const (
	screenPicker screen = iota
	screenBrowse
)

type focus int

const (
	focusSidebar focus = iota
	focusGrid
)

const (
	sidebarWidth = 24
	leftPad      = 1 // 1-char blank margin before the table list / grid
)

// App is the root bubbletea Model.
type App struct {
	screen screen
	picker picker

	directDSN string // from CLI arg; empty → use the picker
	connName  string
	readOnly  bool // connection refuses mutations (read_only in config)

	engine      db.Engine
	sidebar     sidebar
	grid        grid
	cell        cellView
	focus       focus
	showSidebar bool

	currentTable db.Table
	sortCol      string
	sortAsc      bool
	resetGrid    bool // reset cursor on next rows load (new table, not a re-sort)

	dbName         string
	w, h           int
	status         string
	postExecStatus string // shown after the reload that follows a full-path exec
	err            error
}

// New builds the root model. If dsn != "", it connects directly; else the picker.
// readOnly disables all mutation for a directly-connected connection (§8).
func New(conns []config.Conn, dsn, name string, readOnly bool) App {
	a := App{
		picker:      picker{conns: conns},
		grid:        newGrid(),
		directDSN:   dsn,
		connName:    name,
		readOnly:    readOnly,
		showSidebar: true,
	}
	if dsn != "" {
		a.screen = screenBrowse
		a.status = "connecting…"
	}
	return a
}

func (a App) Init() tea.Cmd {
	if a.directDSN != "" {
		return connectCmd(a.directDSN, a.connName)
	}
	return nil
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		a.layout()
		return a, nil

	case errMsg:
		a.err = msg.err
		return a, nil

	case connectedMsg:
		a.engine = msg.engine
		if msg.name != "" {
			a.connName = msg.name
		}
		a.dbName = msg.dbName
		a.sidebar.setTables(msg.tables)
		a.screen = screenBrowse
		a.showSidebar = true
		a.focus = focusSidebar
		a.layout()
		a.status = ""
		return a, nil

	case rowsMsg:
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
		a.showSidebar = false
		a.focus = focusGrid
		a.layout()
		a.status = msg.table.Name
		// A reload triggered by a full-path exec keeps its confirmation visible.
		if a.postExecStatus != "" {
			a.status = a.postExecStatus
			a.postExecStatus = ""
		}
		return a, nil

	case moreRowsMsg:
		a.grid.appendRows(msg.rows, msg.full)
		return a, nil

	case editDoneMsg:
		// A keyed edit must touch exactly one row; anything else is loud (§8).
		if msg.affected == 1 {
			a.grid.applyEdit(msg.val)
			a.status = fmt.Sprintf("set %s = '%s'", msg.col, msg.val)
		} else {
			a.status = fmt.Sprintf("⚠ %s: %d rows affected, expected 1", msg.col, msg.affected)
		}
		return a, nil

	case editorReadyMsg:
		return a, editorCmd(msg.seed)

	case editorSubmitMsg:
		a.status = "running…"
		return a, execRawCmd(a.engine, msg.sql)

	case editorAbortedMsg:
		a.status = "edit cancelled"
		return a, nil

	case execDoneMsg:
		// Reload the current view so the change is reflected; the affected count
		// survives the reload via postExecStatus.
		a.postExecStatus = fmt.Sprintf("ran — %d row(s) affected", msg.affected)
		return a, a.loadCurrentCmd()

	case tea.KeyMsg:
		return a.handleKey(msg)
	}
	return a, nil
}

func (a *App) layout() {
	if a.screen != screenBrowse {
		return
	}
	bodyH := a.h - 1 // status line
	if bodyH < 1 {
		bodyH = 1
	}
	avail := a.w - leftPad
	if avail < 1 {
		avail = 1
	}
	gridW := avail
	if a.showSidebar {
		gridW = avail - sidebarWidth
		a.sidebar.w = sidebarWidth - 1
		a.sidebar.h = bodyH
	}
	if gridW < 1 {
		gridW = 1
	}
	a.grid.setSize(gridW, bodyH)
}

func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
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
	// While editing a cell (§8 quick path), keys go to the edit overlay.
	if a.screen == screenBrowse && a.grid.editing {
		return a.handleEditKey(msg)
	}
	// While editing a column filter, keys go to the filter input.
	if a.screen == screenBrowse && a.grid.filtering >= 0 {
		return a.handleFilterKey(msg)
	}
	// While typing a table-list filter, keys go to that input.
	if a.screen == screenBrowse && a.showSidebar && a.focus == focusSidebar && a.sidebar.filtering {
		return a.handleSidebarFilterKey(msg)
	}
	switch a.screen {
	case screenPicker:
		switch msg.String() {
		case "j", "down":
			a.picker.move(1)
		case "k", "up":
			a.picker.move(-1)
		case "enter":
			if c, ok := a.picker.selected(); ok {
				a.connName = c.Name
				a.readOnly = c.ReadOnly
				a.status = "connecting to " + c.Name + "…"
				return a, connectCmd(c.DSN(), c.Name)
			}
		}
		return a, nil

	case screenBrowse:
		switch msg.String() {
		case "H":
			a.showSidebar = !a.showSidebar
			if a.showSidebar {
				a.focus = focusSidebar
			} else {
				a.focus = focusGrid
			}
			a.layout()
			return a, nil
		case "tab":
			if a.focus == focusSidebar {
				a.focus = focusGrid
			} else if a.showSidebar {
				a.focus = focusSidebar
			}
			return a, nil
		}
		if a.showSidebar && a.focus == focusSidebar {
			return a.handleSidebarKey(msg)
		}
		return a.handleGridKey(msg)
	}
	return a, nil
}

func (a App) handleSidebarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		a.sidebar.move(1)
	case "k", "up":
		a.sidebar.move(-1)
	case "g":
		a.sidebar.top()
	case "G":
		a.sidebar.bottom()
	case "/":
		a.sidebar.startFilter()
	case "esc":
		a.sidebar.clearFilter()
	case "enter":
		if t, ok := a.sidebar.selected(); ok {
			return a.selectTable(t)
		}
	}
	return a, nil
}

// handleSidebarFilterKey routes keys while the table-list filter is being typed
// (§7). Purely client-side — narrowing the already-loaded table names, no
// server round-trip. Enter loads the highlighted table; Esc cancels the filter.
func (a App) handleSidebarFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		t, ok := a.sidebar.selected()
		a.sidebar.commitFilter() // keep the pattern active for when H returns here
		if ok {
			return a.selectTable(t)
		}
	case tea.KeyEsc:
		a.sidebar.clearFilter()
	case tea.KeyBackspace:
		a.sidebar.filterBackspace()
	case tea.KeyDown:
		a.sidebar.move(1)
	case tea.KeyUp:
		a.sidebar.move(-1)
	case tea.KeySpace:
		a.sidebar.filterInput(" ")
	case tea.KeyRunes:
		a.sidebar.filterInput(string(msg.Runes))
	}
	return a, nil
}

// selectTable loads the given table into the grid with default sort and no
// filters, and auto-hides the sidebar (handled on rowsMsg).
func (a App) selectTable(t db.Table) (tea.Model, tea.Cmd) {
	a.currentTable = t
	a.sortCol, a.sortAsc = "", true // reset to default (PK desc) on new table
	a.resetGrid = true              // new table → cursor to top-left
	a.grid.clearFilters()
	a.status = "loading " + t.Name + "…"
	return a, a.loadCurrentCmd()
}

// handleEditKey routes keys while a cell is being edited (§8 quick path). Enter
// builds the keyed UPDATE and runs it immediately; Esc cancels. A bare Enter
// with no typing is a no-op (commitEdit returns ok=false), so a NULL cell can't
// be blanked by accident.
func (a App) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if req, ok := a.grid.commitEdit(); ok {
			a.status = "saving…"
			return a, execEditCmd(a.engine, req)
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
		return a, a.loadCurrentCmd()
	case tea.KeyEsc:
		a.grid.clearFilter()
		return a, a.loadCurrentCmd()
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
		a.grid.startFilter()
	case "esc":
		if a.grid.clearCurrentFilter() {
			return a, a.loadCurrentCmd()
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
			a.status = "preparing insert…"
			return a, prepareInsertCmd(a.engine, a.currentTable)
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
	case "enter":
		if v, col, ok := a.grid.currentCell(); ok {
			a.cell.open(col, a.grid.cursorR, v, a.grid.w, a.grid.h)
		}
	case "J":
		if name, ok := a.grid.currentColName(); ok {
			a.sortCol, a.sortAsc = name, true
			return a, a.loadCurrentCmd()
		}
	case "K":
		if name, ok := a.grid.currentColName(); ok {
			a.sortCol, a.sortAsc = name, false
			return a, a.loadCurrentCmd()
		}
	}
	// After a movement, fetch the next window if the cursor neared the edge.
	return a, a.maybeLoadMore()
}

// maybeLoadMore triggers a continuous-scroll fetch when the cursor nears the
// loaded edge and more rows exist.
func (a *App) maybeLoadMore() tea.Cmd {
	if !a.grid.wantMore() {
		return nil
	}
	a.grid.loading = true
	return loadMoreCmd(a.engine, a.currentTable, a.sortCol, a.sortAsc,
		a.grid.filterSpecs(), len(a.grid.rows), a.gridLimit())
}

// loadCurrentCmd (re)loads the current table with the active sort and filters.
func (a App) loadCurrentCmd() tea.Cmd {
	return loadCmd(a.engine, a.currentTable, a.gridLimit(), a.sortCol, a.sortAsc, a.grid.filterSpecs())
}

func (a App) gridLimit() int {
	if n := (a.h - 2) * 4; n >= 200 {
		return n
	}
	return 200
}

func (a App) View() string {
	if a.err != nil {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).
			Render("error: "+a.err.Error()) + "\n\npress ctrl-c to quit"
	}
	switch a.screen {
	case screenPicker:
		return lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.picker.View())
	case screenBrowse:
		return a.browseView()
	}
	return ""
}

func (a App) browseView() string {
	if a.cell.active {
		body := lipgloss.NewStyle().PaddingLeft(leftPad).Render(a.cell.View())
		return a.statusLine() + "\n" + body
	}
	body := a.grid.View()
	if a.showSidebar {
		sb := lipgloss.NewStyle().Width(sidebarWidth).Render(a.sidebar.View(a.focus == focusSidebar))
		body = lipgloss.JoinHorizontal(lipgloss.Top, sb, a.grid.View())
	}
	body = lipgloss.NewStyle().PaddingLeft(leftPad).Render(body)
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
	return lipgloss.NewStyle().Faint(true).Render(" " + strings.Join(parts, " > ") + " ")
}
