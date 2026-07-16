package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jmserra/jsq/internal/db"
)

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

	// Rect within the body, assigned by layoutPanes. Focus movement (`<space>h/l`)
	// is computed from these rather than from pane order, so it keeps working when
	// `<space>s` adds a second axis.
	x, y, w, h int
}

// newPane builds an empty pane with a fresh id and an empty jumplist.
func (a *App) newPane() pane {
	a.nextPaneID++
	return pane{id: a.nextPaneID, grid: newGrid(), viewIdx: -1}
}

// clone returns an independent copy of the pane under a fresh id — what
// `<space>v` puts on the right. It duplicates exactly what the pane is showing,
// including a free-form (s) query result and committed column filters.
//
// It clones the live pane rather than going through viewState because viewState
// models a *jumplist entry*, not a view: it has no adHoc/adHocQuery (so an s
// result would silently clone as the underlying table) and no committed filters
// (those live in gridSnapshot, and are lost once it's evicted).
//
// views must be deep-copied. syncCurrent writes `views[viewIdx] = v`, and the
// clone starts at an identical viewIdx, so a shared backing array would have the
// first navigation in either pane overwrite the other's history. The *gridSnapshot
// pointers inside are shared, which is fine — nothing writes one in place except
// the intentional edit reflection.
func (a *App) clonePane(src *pane) pane {
	c := *src
	c.id = 0 // assigned below; never inherit the source's identity
	c.grid = src.grid.clone()
	c.basePreds = append([]eqPred(nil), src.basePreds...)
	c.views = append([]viewState(nil), src.views...)
	a.nextPaneID++
	c.id = a.nextPaneID
	return c
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

// handleLeaderKey runs the key that followed <space>. The leader flag is already
// cleared by the caller, so every path here is one-shot.
func (a App) handleLeaderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "v": // vertical split: a copy of this pane, to the right
		return a.splitVertical()
	case "q": // close this pane (without it, a split couldn't be undone)
		return a.closePane()
	case "h", "j", "k", "l":
		a.focusDir(msg.String())
		return a, nil
	}
	// Anything else (including <space><esc>) just cancels the leader. Esc must not
	// fall through to the op-kill: it was aimed at the leader, not a query.
	return a, nil
}

// maxPanes bounds the split count. Past this the panes are too narrow to read,
// and every pane costs a cached snapshot budget.
const maxPanes = 4

// splitVertical duplicates the focused pane to its right and focuses the copy —
// so `<space>v` then navigating leaves the original untouched behind you.
func (a App) splitVertical() (tea.Model, tea.Cmd) {
	if len(a.panes) >= maxPanes {
		a.status = fmt.Sprintf("at most %d panes", maxPanes)
		return a, nil
	}
	c := a.clonePane(a.p())
	// Insert right of the focused pane, then re-take any pane pointer: this append
	// may reallocate, and a *pane held across it would aim at the orphaned array.
	i := a.focus + 1
	a.panes = append(a.panes, pane{})
	copy(a.panes[i+1:], a.panes[i:])
	a.panes[i] = c
	a.focus = i
	a.layout() // after the append: the clone's rowOff was sized for the old height
	return a, nil
}

// closePane drops the focused pane, focusing its left neighbour. A no-op on the
// last pane — there's always at least one, so p()/g() can't fail.
func (a App) closePane() (tea.Model, tea.Cmd) {
	if len(a.panes) == 1 {
		a.status = "only pane"
		return a, nil
	}
	a.panes = append(a.panes[:a.focus], a.panes[a.focus+1:]...)
	if a.focus >= len(a.panes) {
		a.focus = len(a.panes) - 1
	}
	a.layout()
	return a, nil
}

// focusDir moves focus to the nearest pane in the given direction, by rect
// geometry rather than by pane order — so it keeps working unchanged once
// `<space>s` introduces a second axis.
func (a *App) focusDir(dir string) {
	cur := a.panes[a.focus]
	best, bestDist := -1, 0
	for i := range a.panes {
		if i == a.focus {
			continue
		}
		p := a.panes[i]
		var d int
		switch dir {
		case "h":
			if p.x >= cur.x {
				continue
			}
			d = cur.x - p.x
		case "l":
			if p.x <= cur.x {
				continue
			}
			d = p.x - cur.x
		case "k":
			if p.y >= cur.y {
				continue
			}
			d = cur.y - p.y
		case "j":
			if p.y <= cur.y {
				continue
			}
			d = p.y - cur.y
		}
		if best < 0 || d < bestDist {
			best, bestDist = i, d
		}
	}
	if best >= 0 {
		a.focus = best
	}
}
