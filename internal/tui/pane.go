package tui

import "github.com/jmserra/jsq/internal/db"

// pane is one independently-navigable view of the database: a grid plus the
// metadata describing what it's showing. Panes are the unit `<space>v` splits.
//
// Deliberately small. Only state that genuinely differs between two side-by-side
// views lives here; everything else stays on App:
//
//   - The connection/database/engine are session-wide — every pane looks at the
//     same database (a conn/db switch while split is refused, see goToView).
//   - status/pendingPos/resetGrid/postExecStatus stay on App because there is one
//     op slot, so at most one of them can be pending at a time.
//   - cell/help/jumps/histView stay on App: they're modal and full-area, not
//     per-pane.
//
// The jumplist (views/viewIdx) IS per-pane — like vim, where each window has its
// own — so Ctrl-O in one pane walks only that pane's history.
type pane struct {
	id int // stable identity; survives reordering, unlike an index

	grid         grid
	currentTable db.Table
	basePreds    []eqPred // followed-FK equality filter, AND-ed into every load
	baseNote     string   // human form of basePreds, shown in the header
	sortCol      string
	sortAsc      bool
	adHoc        bool   // grid shows a free-form (s) query result, not a table
	adHocQuery   string // SQL behind the adHoc result, so `r` can re-run it

	// Per-pane jumplist: visited views oldest→newest, viewIdx = current (-1
	// before the first). See App.views' old doc comment for the semantics.
	views   []viewState
	viewIdx int
}

// newPane builds an empty pane with a fresh id and an empty jumplist.
func (a *App) newPane() pane {
	a.nextPaneID++
	return pane{id: a.nextPaneID, grid: newGrid(), viewIdx: -1}
}

// p returns a pointer to the focused pane, for mutation.
//
// Safe to call on a value receiver: Update copy-on-writes the panes slice, so
// &a.panes[i] points into an array this model owns.
//
// IMPORTANT: never hold the returned pointer across a mutation of the a.panes
// slice header. An append may reallocate, leaving the pointer aimed at the
// orphaned array where writes are silently lost. Re-take p() after any
// append/remove.
func (a *App) p() *pane { return &a.panes[a.focus] }

// g is shorthand for the focused pane's grid — by far the most-used pane field.
func (a *App) g() *grid { return &a.p().grid }

// paneByID resolves a pane by its stable id. Returns false once that pane has
// been closed, which is how a DB result for a since-closed pane gets dropped.
func (a *App) paneByID(id int) (*pane, bool) {
	for i := range a.panes {
		if a.panes[i].id == id {
			return &a.panes[i], true
		}
	}
	return nil, false
}
